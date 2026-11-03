package generator

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/jecklgamis/resonate/internal/tmpl"
)

// GRPCCall is one unary RPC in an iteration's call sequence. Message is the
// request as protobuf JSON and may embed {{ }} expressions (see
// internal/tmpl); the response is rendered back to protobuf JSON (canonical
// lowerCamelCase field names, zero values included) for Extract and
// ExpectBody to run against.
type GRPCCall struct {
	// Method is "package.Service/Method" (a leading "/" is accepted). Whether
	// it is unary, server-streaming, client-streaming or bidirectional is read
	// from the schema; the fields below say what each kind needs.
	Method string

	// Message is the request, for unary and server-streaming methods.
	Message string

	// Messages are the requests, for client-streaming and bidirectional
	// methods: sent in order (each rendered fresh as it is sent), the whole
	// list Repeat times, then the send side is closed.
	Messages []string

	// SendInterval, if > 0, pauses between consecutive sent messages.
	SendInterval time.Duration

	// Repeat is how many times to send the Messages list (default 1).
	Repeat int

	// Receive, for server-streaming and bidirectional methods, ends the
	// stream successfully once this many responses have arrived; the server
	// closing the stream (or Duration/Timeout elapsing) first is a failure.
	Receive int

	// Duration, for streaming methods, is how long to listen. With Receive
	// unset, reaching it is the normal, successful end of the stream; with
	// Receive set it is the deadline for receiving them. With neither set the
	// stream is read until the server closes it, bounded by
	// GRPCOptions.Timeout (a stream still open at that point is a failure —
	// an endless stream needs Receive or Duration to say when to stop).
	Duration time.Duration

	// Metadata is sent with this call, on top of GRPCTarget.Metadata (same
	// key: this one wins). Values may embed {{ }} expressions.
	Metadata map[string]string

	// Extract pulls values out of the response into named vars, available to
	// later calls in the same iteration via {{.Vars.<name>}}. Rules are the
	// usual json:/yaml:/regex:/... language run against the response JSON;
	// "header:<Name>" reads response headers and trailers, and "status" is
	// the gRPC status code number (0 = OK). On a streaming call the rules run
	// against every received message, so the last one's values stick, and
	// "header:" sees response headers only (trailers arrive after the last
	// message).
	Extract map[string]string

	// ExpectCode, if non-empty, lists the gRPC status codes that count as
	// success, by name ("OK", "NOT_FOUND", ...), replacing the default of
	// OK-only. With ExpectBody set but ExpectCode empty, OK is still required.
	// On a streaming call it checks the status the server ends the stream
	// with; a stream that resonate itself ends (Receive reached, Duration
	// elapsed) has no server status to check.
	ExpectCode []string

	// ExpectBody checks the response JSON with the same rule language as
	// Extract: a non-empty value must match exactly, an empty value only
	// requires the rule to evaluate. Combines with ExpectCode (AND). On a
	// streaming call every received message must match; the first that
	// doesn't fails the call and ends the stream.
	ExpectBody map[string]string
}

// GRPCTarget describes one iteration against one server: the calls are made
// in order over a shared connection, and the iteration stops at the first
// failed call.
type GRPCTarget struct {
	// Address is the server, "host:port" (or any address grpc-go's resolver
	// accepts, e.g. "unix:///path/to.sock").
	Address string

	// Metadata is sent with every call; values may embed {{ }} expressions.
	Metadata map[string]string

	Calls []GRPCCall
}

// GRPCOptions configures the connection(s) and where the schema comes from.
//
// The schema (which messages a method takes and returns) comes from, in
// order of precedence: ProtoFiles, DescriptorSet, or — when neither is set —
// the server's own reflection service.
type GRPCOptions struct {
	// Timeout is the deadline for each call, and for the reflection lookup.
	Timeout time.Duration

	// Plaintext connects without TLS. Otherwise TLS is used, configured by
	// Insecure/CAFile/CertFile/KeyFile exactly as for HTTPOptions; setting
	// any of those alongside Plaintext is a construction error.
	Plaintext bool
	Insecure  bool
	CAFile    string
	CertFile  string
	KeyFile   string

	// Connections is how many separate gRPC connections to spread virtual
	// users over (VU i uses connection i % Connections). Default 1. Each is a
	// single HTTP/2 connection, and a server typically allows ~100 concurrent
	// streams on one, so raise this when concurrency is high or calls will
	// queue client-side and inflate latency.
	Connections int

	// ProtoFiles are .proto sources to compile; ImportPaths are the roots
	// they and their imports resolve against (default: the directories
	// containing ProtoFiles).
	ProtoFiles  []string
	ImportPaths []string

	// DescriptorSet is a serialized FileDescriptorSet, as written by
	// `protoc --descriptor_set_out=FILE --include_imports`.
	DescriptorSet string

	// Identities, if non-empty, is a pool of named values (e.g. tokens). Each
	// virtual user (vuID) gets exactly one, round-robin over the pool, sticky
	// for that VU's lifetime, exposed as {{.Identity.<key>}}.
	Identities []map[string]string

	// Feeder, if set, hands out one CSV/JSON row per iteration, exposed as
	// {{.Feeder.<column>}}.
	Feeder *Feeder
}

func DefaultGRPCOptions() GRPCOptions {
	return GRPCOptions{Timeout: 30 * time.Second, Connections: 1}
}

// grpcKind is a method's streaming shape, read from its descriptor.
type grpcKind int

const (
	kindUnary grpcKind = iota
	kindServerStream
	kindClientStream
	kindBidi
)

func (k grpcKind) String() string {
	return [...]string{"unary", "server-streaming", "client-streaming", "bidirectional-streaming"}[k]
}

// compiledSend is one request message of a stream: a template, or — when it
// has no {{ }} — a message parsed once at construction and reused read-only.
type compiledSend struct {
	field  tmpl.Field
	static *dynamicpb.Message
}

func (s compiledSend) build(in protoreflect.MessageDescriptor, data tmpl.Data) (*dynamicpb.Message, error) {
	if s.static != nil {
		return s.static, nil
	}
	body, err := s.field.Render(data)
	if err != nil {
		return nil, fmt.Errorf("rendering message: %w", err)
	}
	msg := dynamicpb.NewMessage(in)
	if err := unmarshalGRPCMessage(body, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

type compiledGRPCCall struct {
	method     string // "Method", the stream name
	path       string // "/pkg.Service/Method"
	kind       grpcKind
	in, out    protoreflect.MessageDescriptor
	message    tmpl.Field
	static     *dynamicpb.Message // non-nil when the message has no {{ }}: parsed once, reused
	metadata   map[string]tmpl.Field
	extract    map[string]string
	expectCode []codes.Code
	expectBody map[string]string

	// Streaming only.
	sendFields []tmpl.Field // compiled c.Messages, awaiting the schema
	sends      []compiledSend
	receive    int
	duration   time.Duration
	interval   time.Duration
	repeat     int
}

// GRPCGenerator makes gRPC calls — unary, server-, client- and bidirectional
// streaming. Every iteration runs its call sequence in order; a unary call is
// one Result, a streaming call one Result per received message (see
// invokeStream), and a failed call ends the iteration.
//
// Connections are dialed once and shared by all workers for the whole run
// (gRPC multiplexes calls over each). Calls use gRPC's wait-for-ready mode:
// with a server down, a call blocks until Timeout instead of failing at once,
// since an instant failure would let a worker loop with no --rate spin at
// millions of calls per second — the same hazard validateLiteralURL guards
// against for HTTP.
type GRPCGenerator struct {
	conns      []*grpc.ClientConn
	calls      []compiledGRPCCall
	timeout    time.Duration
	identities []map[string]string
	feeder     *Feeder
	seq        uint64
}

func NewGRPCGenerator(target GRPCTarget, opts GRPCOptions) (*GRPCGenerator, error) {
	if strings.TrimSpace(target.Address) == "" {
		return nil, fmt.Errorf("grpc address is required, e.g. localhost:50051")
	}
	if len(target.Calls) == 0 {
		return nil, fmt.Errorf("grpc target must have at least one call")
	}
	if opts.Connections < 0 {
		return nil, fmt.Errorf("grpc connections must be >= 0, got %d", opts.Connections)
	}
	if len(opts.ProtoFiles) > 0 && opts.DescriptorSet != "" {
		return nil, fmt.Errorf("proto_files and descriptor_set are mutually exclusive; set one (or neither, to use server reflection)")
	}
	if len(opts.ImportPaths) > 0 && len(opts.ProtoFiles) == 0 {
		return nil, fmt.Errorf("import_paths has no effect without proto_files")
	}
	if opts.Plaintext && (opts.Insecure || opts.CAFile != "" || opts.CertFile != "" || opts.KeyFile != "") {
		return nil, fmt.Errorf("plaintext disables TLS and can't be combined with insecure/ca/cert/key options")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	// Everything that can fail without touching the network goes first.
	calls := make([]compiledGRPCCall, len(target.Calls))
	var services []string
	for i, c := range target.Calls {
		cc, service, err := compileGRPCCall(target.Metadata, c)
		if err != nil {
			return nil, fmt.Errorf("call %d (%s): %w", i, c.Method, err)
		}
		calls[i] = cc
		if !slices.Contains(services, service) {
			services = append(services, service)
		}
	}

	creds := credentials.TransportCredentials(insecure.NewCredentials())
	if !opts.Plaintext {
		tlsConfig, err := newTLSConfig(opts.Insecure, opts.CAFile, opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(tlsConfig)
	}

	n := max(opts.Connections, 1)
	conns := make([]*grpc.ClientConn, 0, n)
	closeAll := func() {
		for _, c := range conns {
			c.Close()
		}
	}
	for range n {
		conn, err := grpc.NewClient(target.Address, grpc.WithTransportCredentials(creds))
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("creating grpc client for %q: %w", target.Address, err)
		}
		conns = append(conns, conn)
	}

	// Resolve the schema, then bind each call to its method's message types.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var resolver descriptorResolver
	var err error
	switch {
	case len(opts.ProtoFiles) > 0:
		resolver, err = loadProtoSources(ctx, opts.ProtoFiles, opts.ImportPaths)
	case opts.DescriptorSet != "":
		resolver, err = loadDescriptorSet(opts.DescriptorSet)
	default:
		resolver, err = loadReflectedSchema(ctx, conns[0], services)
	}
	if err != nil {
		closeAll()
		return nil, err
	}
	for i := range calls {
		if err := bindGRPCCall(&calls[i], resolver, target.Calls[i]); err != nil {
			closeAll()
			return nil, fmt.Errorf("call %d (%s): %w", i, target.Calls[i].Method, err)
		}
	}

	return &GRPCGenerator{
		conns:      conns,
		calls:      calls,
		timeout:    timeout,
		identities: opts.Identities,
		feeder:     opts.Feeder,
	}, nil
}

// compileGRPCCall does the schema-independent half of setting up a call:
// templates, expectation parsing and the merged metadata. It also returns the
// service name so the caller can batch the schema lookup.
func compileGRPCCall(targetMeta map[string]string, c GRPCCall) (compiledGRPCCall, string, error) {
	service, method, err := parseGRPCMethod(c.Method)
	if err != nil {
		return compiledGRPCCall{}, "", err
	}

	message, err := tmpl.Compile("message", c.Message)
	if err != nil {
		return compiledGRPCCall{}, "", err
	}
	sendFields := make([]tmpl.Field, len(c.Messages))
	for i, m := range c.Messages {
		f, err := tmpl.Compile(fmt.Sprintf("messages[%d]", i), m)
		if err != nil {
			return compiledGRPCCall{}, "", fmt.Errorf("messages[%d]: %w", i, err)
		}
		sendFields[i] = f
	}
	if c.Receive < 0 || c.Repeat < 0 || c.Duration < 0 || c.SendInterval < 0 {
		return compiledGRPCCall{}, "", fmt.Errorf("receive, repeat, duration and send_interval must not be negative")
	}
	repeat := c.Repeat
	if repeat == 0 {
		repeat = 1
	}

	merged := map[string]string{}
	for k, v := range targetMeta {
		merged[strings.ToLower(k)] = v
	}
	for k, v := range c.Metadata {
		merged[strings.ToLower(k)] = v
	}
	meta := make(map[string]tmpl.Field, len(merged))
	for k, v := range merged {
		if strings.TrimSpace(k) == "" {
			return compiledGRPCCall{}, "", fmt.Errorf("metadata key must not be empty")
		}
		f, err := tmpl.Compile("metadata:"+k, v)
		if err != nil {
			return compiledGRPCCall{}, "", err
		}
		meta[k] = f
	}

	var expect []codes.Code
	for _, name := range c.ExpectCode {
		code, ok := grpcCodeFromName(name)
		if !ok {
			return compiledGRPCCall{}, "", fmt.Errorf("invalid expect_code %q (expected a gRPC status name such as OK, NOT_FOUND, UNAVAILABLE)", name)
		}
		expect = append(expect, code)
	}
	for rule := range c.ExpectBody {
		if !isKnownExtractRule(rule) {
			return compiledGRPCCall{}, "", fmt.Errorf("invalid expect_body rule %q (expected \"json:<path>\", \"yaml:<path>\", \"xml:<path>\", \"regex:<pattern>\", \"css:<selector>\", \"header:<Name>\", or \"status\")", rule)
		}
	}

	return compiledGRPCCall{
		method:     method,
		path:       "/" + service + "/" + method,
		message:    message,
		metadata:   meta,
		extract:    c.Extract,
		expectCode: expect,
		expectBody: c.ExpectBody,
		sendFields: sendFields,
		receive:    c.Receive,
		duration:   c.Duration,
		interval:   c.SendInterval,
		repeat:     repeat,
	}, service, nil
}

// bindGRPCCall attaches the method's request/response types and streaming
// kind, checks the call's fields suit that kind, and pre-parses static
// messages so a malformed one fails now.
func bindGRPCCall(cc *compiledGRPCCall, r descriptorResolver, c GRPCCall) error {
	service, method, _ := parseGRPCMethod(c.Method)
	md, err := findGRPCMethod(r, service, method)
	if err != nil {
		return err
	}
	cc.in, cc.out = md.Input(), md.Output()
	switch {
	case md.IsStreamingClient() && md.IsStreamingServer():
		cc.kind = kindBidi
	case md.IsStreamingClient():
		cc.kind = kindClientStream
	case md.IsStreamingServer():
		cc.kind = kindServerStream
	}

	streamOnly := len(c.Messages) > 0 || c.Receive != 0 || c.Duration != 0 || c.SendInterval != 0 || c.Repeat != 0
	switch cc.kind {
	case kindUnary:
		if streamOnly {
			return fmt.Errorf("%s is a unary RPC: it takes one request (message); messages, receive, duration, send_interval and repeat are for streaming RPCs", c.Method)
		}
	case kindServerStream:
		if len(c.Messages) > 0 || c.SendInterval != 0 || c.Repeat != 0 {
			return fmt.Errorf("%s is a server-streaming RPC: it takes one request (message), not messages, send_interval or repeat", c.Method)
		}
	case kindClientStream:
		if c.Message != "" {
			return fmt.Errorf("%s is a client-streaming RPC: give its requests as messages, not message", c.Method)
		}
		if c.Receive != 0 {
			return fmt.Errorf("%s is a client-streaming RPC: it gets exactly one response, so receive doesn't apply", c.Method)
		}
		if len(c.Messages) == 0 {
			return fmt.Errorf("%s is a client-streaming RPC: messages needs at least one entry", c.Method)
		}
	case kindBidi:
		if c.Message != "" {
			return fmt.Errorf("%s is a bidirectional-streaming RPC: give its requests as messages, not message", c.Method)
		}
	}

	// The requests this call sends: the single message, or the messages list.
	switch cc.kind {
	case kindUnary, kindServerStream:
		send, err := bindSend(cc.in, c.Message, cc.message)
		if err != nil {
			return err
		}
		cc.static = send.static
		if cc.kind == kindServerStream {
			cc.sends = []compiledSend{send}
		}
	default:
		for i, text := range c.Messages {
			send, err := bindSend(cc.in, text, cc.sendFields[i])
			if err != nil {
				return fmt.Errorf("messages[%d]: %w", i, err)
			}
			cc.sends = append(cc.sends, send)
		}
	}
	return nil
}

// bindSend pairs a compiled message template with its parsed form when it is
// static (no {{ }}), so a malformed static message fails at construction.
func bindSend(in protoreflect.MessageDescriptor, text string, f tmpl.Field) (compiledSend, error) {
	if strings.Contains(text, "{{") {
		return compiledSend{field: f}, nil
	}
	msg := dynamicpb.NewMessage(in)
	if err := unmarshalGRPCMessage(text, msg); err != nil {
		return compiledSend{}, err
	}
	return compiledSend{field: f, static: msg}, nil
}

func parseGRPCMethod(m string) (service, method string, err error) {
	m = strings.TrimPrefix(strings.TrimSpace(m), "/")
	i := strings.LastIndex(m, "/")
	if i <= 0 || i == len(m)-1 {
		return "", "", fmt.Errorf("invalid method %q: expected \"package.Service/Method\"", m)
	}
	return m[:i], m[i+1:], nil
}

// unmarshalGRPCMessage parses protobuf JSON into msg; an empty string means
// the empty message, so a call with no arguments needs no `message`.
func unmarshalGRPCMessage(s string, msg proto.Message) error {
	if strings.TrimSpace(s) == "" {
		s = "{}"
	}
	if err := protojson.Unmarshal([]byte(s), msg); err != nil {
		return fmt.Errorf("parsing message as %s JSON: %w", msg.ProtoReflect().Descriptor().FullName(), err)
	}
	return nil
}

func (a *GRPCGenerator) Protocol() string { return "grpc" }

func (a *GRPCGenerator) Close() error {
	var first error
	for _, c := range a.conns {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	a.feeder.Close()
	return first
}

func (a *GRPCGenerator) identityFor(vuID int) map[string]string {
	if len(a.identities) == 0 {
		return nil
	}
	return a.identities[vuID%len(a.identities)]
}

func (a *GRPCGenerator) Do(ctx context.Context, vuID int) []Result {
	data := tmpl.Data{
		Seq:      atomic.AddUint64(&a.seq, 1) - 1,
		VU:       vuID,
		Identity: a.identityFor(vuID),
		Vars:     map[string]string{},
		Feeder:   a.feeder.Next(),
	}
	conn := a.conns[vuID%len(a.conns)]

	results := make([]Result, 0, len(a.calls))
	for i := range a.calls {
		var rs []Result
		if a.calls[i].kind == kindUnary {
			r := a.invoke(ctx, conn, &a.calls[i], data)
			if runEnded(ctx) && !r.Success() {
				// The run ended while this call was in flight, so the server
				// never got to answer; reporting it would count a measurement
				// artifact (and trip a success_rate assertion) as a failure.
				break
			}
			rs = []Result{r}
		} else {
			rs = a.invokeStream(ctx, conn, &a.calls[i], data)
		}
		results = append(results, rs...)
		if failed(rs) {
			break
		}
	}
	return results
}

func (a *GRPCGenerator) invoke(ctx context.Context, conn *grpc.ClientConn, c *compiledGRPCCall, data tmpl.Data) Result {
	fail := func(err error) Result {
		return Result{Timestamp: time.Now(), Error: err, Protocol: "grpc"}
	}

	req := c.static
	if req == nil {
		body, err := c.message.Render(data)
		if err != nil {
			return fail(fmt.Errorf("rendering message: %w", err))
		}
		req = dynamicpb.NewMessage(c.in)
		if err := unmarshalGRPCMessage(body, req); err != nil {
			return fail(err)
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	if len(c.metadata) > 0 {
		pairs := make([]string, 0, 2*len(c.metadata))
		for k, f := range c.metadata {
			v, err := f.Render(data)
			if err != nil {
				return fail(fmt.Errorf("rendering metadata %q: %w", k, err))
			}
			pairs = append(pairs, k, v)
		}
		callCtx = metadata.AppendToOutgoingContext(callCtx, pairs...)
	}

	resp := dynamicpb.NewMessage(c.out)
	var header, trailer metadata.MD
	start := time.Now()
	err := conn.Invoke(callCtx, c.path, req, resp,
		grpc.Header(&header), grpc.Trailer(&trailer), grpc.WaitForReady(true))
	latency := time.Since(start)

	st := status.Convert(err) // OK for a nil error
	result := Result{
		Timestamp:  start,
		Latency:    latency,
		StatusCode: httpStatusFromGRPCCode(st.Code()),
		BytesOut:   int64(proto.Size(req)),
		Protocol:   "grpc",
	}
	if err == nil {
		result.BytesIn = int64(proto.Size(resp))
	}

	if len(c.expectCode) == 0 && len(c.expectBody) == 0 {
		result.Error = err // nil on success; "rpc error: code = ... desc = ..." otherwise
	} else {
		matched := true
		var reasons []string

		wantCodes := c.expectCode
		if len(wantCodes) == 0 {
			wantCodes = []codes.Code{codes.OK}
		}
		if !slices.Contains(wantCodes, st.Code()) {
			matched = false
			reasons = append(reasons, describeCodeMismatch(st, wantCodes))
		}

		if len(c.expectBody) > 0 {
			if err != nil {
				matched = false
				reasons = append(reasons, "body: no response to check (the call failed)")
			} else {
				bodyJSON, jerr := responseJSON(resp)
				rules := make([]string, 0, len(c.expectBody))
				for rule := range c.expectBody {
					rules = append(rules, rule)
				}
				slices.Sort(rules) // deterministic order in the combined error message
				for _, rule := range rules {
					want := c.expectBody[rule]
					if jerr != nil {
						matched = false
						reasons = append(reasons, fmt.Sprintf("body %q: %v", rule, jerr))
						continue
					}
					got, rerr := extractValue(rule, bodyJSON, grpcHeaders(header, trailer), int(st.Code()))
					switch {
					case rerr != nil:
						matched = false
						reasons = append(reasons, fmt.Sprintf("body %q: %v", rule, rerr))
					case want != "" && got != want:
						matched = false
						reasons = append(reasons, fmt.Sprintf("body %q: got %q, want %q", rule, got, want))
					}
				}
			}
		}

		result.successOverride = &matched
		if !matched {
			result.Error = fmt.Errorf("check failed: %s", strings.Join(reasons, "; "))
		}
	}

	if err == nil && len(c.extract) > 0 && result.Success() {
		if bodyJSON, jerr := responseJSON(resp); jerr == nil {
			h := grpcHeaders(header, trailer)
			for name, rule := range c.extract {
				if v, xerr := extractValue(rule, bodyJSON, h, int(st.Code())); xerr == nil {
					data.Vars[name] = v
				}
			}
		}
	}
	return result
}

// responseJSON renders a response as canonical protobuf JSON, zero values
// included so a rule like json:count finds a field the server left at 0.
func responseJSON(m proto.Message) ([]byte, error) {
	return protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
}

// grpcHeaders merges response headers and trailers into an http.Header so
// the shared header:<Name> extract rule works unchanged.
func grpcHeaders(header, trailer metadata.MD) http.Header {
	h := http.Header{}
	for _, md := range []metadata.MD{header, trailer} {
		for k, vs := range md {
			for _, v := range vs {
				h.Add(k, v)
			}
		}
	}
	return h
}

// grpcCodeNames maps the canonical upper-snake status names to codes.
var grpcCodeNames = map[string]codes.Code{
	"OK":                  codes.OK,
	"CANCELLED":           codes.Canceled,
	"CANCELED":            codes.Canceled,
	"UNKNOWN":             codes.Unknown,
	"INVALID_ARGUMENT":    codes.InvalidArgument,
	"DEADLINE_EXCEEDED":   codes.DeadlineExceeded,
	"NOT_FOUND":           codes.NotFound,
	"ALREADY_EXISTS":      codes.AlreadyExists,
	"PERMISSION_DENIED":   codes.PermissionDenied,
	"RESOURCE_EXHAUSTED":  codes.ResourceExhausted,
	"FAILED_PRECONDITION": codes.FailedPrecondition,
	"ABORTED":             codes.Aborted,
	"OUT_OF_RANGE":        codes.OutOfRange,
	"UNIMPLEMENTED":       codes.Unimplemented,
	"INTERNAL":            codes.Internal,
	"UNAVAILABLE":         codes.Unavailable,
	"DATA_LOSS":           codes.DataLoss,
	"UNAUTHENTICATED":     codes.Unauthenticated,
}

func grpcCodeFromName(name string) (codes.Code, bool) {
	c, ok := grpcCodeNames[strings.ToUpper(strings.TrimSpace(name))]
	return c, ok
}

func grpcCodeName(c codes.Code) string {
	// Go through the table (preferring the spelling gRPC itself documents)
	// rather than codes.Code.String(), which is CamelCase.
	best := ""
	for name, code := range grpcCodeNames {
		if code == c && name != "CANCELED" && (best == "" || name < best) {
			best = name
		}
	}
	if best == "" {
		return fmt.Sprintf("CODE_%d", uint32(c))
	}
	return best
}

// httpStatusFromGRPCCode is gRPC's own documented status-to-HTTP mapping. The
// report's status-code breakdown and Result.Success() are HTTP-shaped, so a
// gRPC result is reported under its HTTP equivalent (OK -> 200); the precise
// gRPC code travels in the Result's Error text instead.
func httpStatusFromGRPCCode(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	default: // Unknown, Internal, DataLoss
		return http.StatusInternalServerError
	}
}

// describeCodeMismatch renders "code: got NOT_FOUND (message), want one of [OK]".
func describeCodeMismatch(st *status.Status, want []codes.Code) string {
	names := make([]string, len(want))
	for i, w := range want {
		names[i] = grpcCodeName(w)
	}
	got := grpcCodeName(st.Code())
	if st.Message() != "" {
		got += fmt.Sprintf(" (%s)", st.Message())
	}
	return fmt.Sprintf("code: got %s, want one of %v", got, names)
}

// runEnded reports whether the run's context is done — or its deadline has
// passed, which is not the same instant: gRPC compares a call's deadline
// against the clock and fails it on the spot, while the context only marks
// itself expired when its timer goroutine next runs, microseconds later. In
// that window ctx.Err() is still nil, so a call the run's end cut short would
// look like a live-run failure (a burst of ~one-per-worker DeadlineExceeded
// results at the very end of every duration-bounded run).
func runEnded(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	d, ok := ctx.Deadline()
	return ok && !time.Now().Before(d)
}

// failed reports whether any of a call's Results is a failure.
func failed(rs []Result) bool {
	for _, r := range rs {
		if !r.Success() {
			return true
		}
	}
	return false
}

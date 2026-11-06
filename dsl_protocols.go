package resonate

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// This file is the fluent builder's coverage of the non-HTTP protocols (SSE,
// gRPC) and of the HTTP transport options the other protocols' YAML has but the
// first version of the builder didn't. A Scenario is exactly one protocol —
// HTTP (Target/Step/Setup/Get/...), WS, SSE or GRPC — and mixing them fails at
// Build, like mixing Target with Step always did.

// --- Which protocol is this scenario? ---------------------------------------

// describeMode names the scenario's protocol for an error message.
func (s *Scenario) describeMode() string {
	switch {
	case s.ws != nil:
		return "a WebSocket scenario"
	case s.sse != nil:
		return "an SSE scenario"
	case s.grpc != nil:
		return "a gRPC scenario"
	}
	return "an HTTP scenario"
}

// nonHTTPStepProtocol is the protocol's short name when the scenario is one
// with no per-step HTTP requests (so no Name, ExpectStatus, ...), else "".
func (s *Scenario) nonHTTPStepProtocol() string {
	switch {
	case s.ws != nil:
		return "WebSocket"
	case s.sse != nil:
		return "SSE"
	case s.grpc != nil:
		return "gRPC"
	}
	return ""
}

// checkModes rejects a Scenario that has started more than one protocol. A
// stray Get/Post/Header on a WS/gRPC scenario also lands here, because it
// starts an implicit HTTP target.
func (s *Scenario) checkModes(isFlow bool) error {
	var used []string
	if len(s.targets) > 0 {
		used = append(used, "Target/Get/Post")
	}
	if isFlow {
		used = append(used, "Step/Setup")
	}
	if s.ws != nil {
		used = append(used, "WS")
	}
	if s.sse != nil {
		// SSE's own request is held in s.sse, not s.targets, so it never
		// shows up as a Target.
		used = append(used, "SSE")
	}
	if s.grpc != nil {
		used = append(used, "GRPC")
	}
	if len(used) > 1 {
		return fmt.Errorf("resonate: scenario mixes %s — use one protocol per Scenario", strings.Join(used, " with "))
	}
	return nil
}

// checkHTTPOnly rejects an HTTP-transport-only setting on a scenario that
// isn't HTTP (SSE is HTTP underneath, so it's allowed). It's checked here, not
// in each setter, because the setter can be called before WS()/GRPC() and
// can't know yet.
func (s *Scenario) checkHTTPOnly() error {
	if len(s.httpOnly) == 0 || (s.ws == nil && s.grpc == nil) {
		return nil
	}
	names := slices.Clone(s.httpOnly)
	slices.Sort(names)
	names = slices.Compact(names)
	return fmt.Errorf("resonate: %s doesn't apply to %s", strings.Join(names, ", "), s.describeMode())
}

func (s *Scenario) markHTTPOnly(name string) { s.httpOnly = append(s.httpOnly, name) }

// setFeeder routes a feeder to whichever protocol's options this scenario uses.
func (s *Scenario) setFeeder(f *Feeder) *Scenario {
	switch {
	case s.grpc != nil:
		s.grpcOpts.Feeder = f
	case s.ws != nil:
		s.wsOpts.Feeder = f
	default: // HTTP, flow and SSE all read it from the HTTP options
		s.httpOpts.Feeder = f
	}
	return s
}

// --- HTTP transport options --------------------------------------------------

// FollowRedirects sets whether 3xx responses are followed (the default is to
// follow them). HTTP and SSE only.
func (s *Scenario) FollowRedirects(follow bool) *Scenario {
	s.markHTTPOnly("FollowRedirects")
	s.httpOpts.FollowRedirects = follow
	return s
}

// MaxIdleConns bounds how many idle keep-alive connections the shared client
// keeps for reuse (0 keeps the default of 100). Raise it to at least your peak
// concurrency, or connections are re-dialled per request and latency inflates —
// see the README's "Connection reuse at high concurrency". HTTP and SSE only.
func (s *Scenario) MaxIdleConns(n int) *Scenario {
	s.markHTTPOnly("MaxIdleConns")
	s.httpOpts.MaxIdleConns = n
	return s
}

// MaxResponseBody caps how many response bytes are read and counted per
// request (0 = unlimited); the rest is drained so the connection stays
// reusable. HTTP and SSE only.
func (s *Scenario) MaxResponseBody(bytes int64) *Scenario {
	s.markHTTPOnly("MaxResponseBody")
	s.httpOpts.MaxResponseBody = bytes
	return s
}

// H2C speaks HTTP/2 over plaintext TCP with prior knowledge (no TLS, no
// upgrade). It can't be combined with Insecure, CAFile, ClientCert,
// DisableKeepAlive or HTTP3 — Build rejects that. HTTP and SSE only.
func (s *Scenario) H2C() *Scenario {
	s.markHTTPOnly("H2C")
	s.httpOpts.H2C = true
	return s
}

// DisableKeepAlive opens a fresh connection for every request. HTTP and SSE
// only; it can't be combined with H2C or HTTP3.
func (s *Scenario) DisableKeepAlive() *Scenario {
	s.markHTTPOnly("DisableKeepAlive")
	s.httpOpts.DisableKeepAlive = true
	return s
}

// CAFile trusts the CA bundle at path in addition to the system roots — for a
// server certified by a private CA. HTTP, SSE and gRPC; a WebSocket scenario
// has no CA option (Build reports that).
func (s *Scenario) CAFile(path string) *Scenario {
	switch {
	case s.grpc != nil:
		s.grpcOpts.CAFile = path
	case s.ws != nil:
		s.deferErr(fmt.Errorf("resonate: CAFile doesn't apply to a WebSocket scenario"))
	default:
		s.httpOpts.CAFile = path
	}
	return s
}

// ClientCert presents a client certificate (mTLS); both files are required
// together. HTTP, SSE and gRPC; not WebSocket.
func (s *Scenario) ClientCert(certFile, keyFile string) *Scenario {
	switch {
	case s.grpc != nil:
		s.grpcOpts.CertFile, s.grpcOpts.KeyFile = certFile, keyFile
	case s.ws != nil:
		s.deferErr(fmt.Errorf("resonate: ClientCert doesn't apply to a WebSocket scenario"))
	default:
		s.httpOpts.CertFile, s.httpOpts.KeyFile = certFile, keyFile
	}
	return s
}

// --- Server-Sent Events ------------------------------------------------------

func sseRef(t *SSETarget) *stepRef {
	return &stepRef{
		method: &t.Method, url: &t.URL,
		query: &t.Query, header: &t.Header, body: &t.Body, rawBody: &t.RawBody,
	}
}

// SSE switches the Scenario to build an SSEGenerator: every iteration opens a
// stream with the given method (GET, or POST for an LLM-style streaming API)
// and url, reads events, and closes it. Method/Get/Post, Header, Query, Body,
// JSONBody, FileBody and RawBody then describe that request, exactly as they do
// for an HTTP target. Events and StreamDuration say when to stop reading.
// ExpectStatus/ExpectHeader/ExpectBody, Extract, Pause and Name don't apply to
// a stream and are rejected (or, for Extract/Pause, ignored, as on any
// independent Target).
func (s *Scenario) SSE(method, url string) *Scenario {
	s.sse = &SSETarget{}
	s.cur = sseRef(s.sse)
	return s.Method(method, url)
}

// Events ends an SSE stream, successfully, after n events. A stream that ends
// or hits its limit before n events is a failure. SSE only.
func (s *Scenario) Events(n int) *Scenario {
	if s.sse == nil {
		s.deferErr(fmt.Errorf("resonate: Events only applies to an SSE scenario (use Receive for a gRPC call); this is %s", s.describeMode()))
		return s
	}
	s.sse.Events = n
	return s
}

// StreamDuration ends a stream after d: an SSE stream (SSE) or the current
// gRPC streaming call (after Call). It is the length of one stream, not of
// the load test — that's Duration.
func (s *Scenario) StreamDuration(d time.Duration) *Scenario {
	switch {
	case s.sse != nil:
		s.sse.Duration = d
	case s.grpc != nil:
		if !s.requireCall("StreamDuration") {
			return s
		}
		s.curCall.Duration = d
	default:
		s.deferErr(fmt.Errorf("resonate: StreamDuration only applies to an SSE stream or a gRPC call; this is %s", s.describeMode()))
	}
	return s
}

// --- gRPC --------------------------------------------------------------------

func (s *Scenario) requireGRPC(name string) bool {
	if s.grpc == nil {
		s.deferErr(fmt.Errorf("resonate: %s only applies to a gRPC scenario (call GRPC first); this is %s", name, s.describeMode()))
		return false
	}
	return true
}

func (s *Scenario) requireCall(name string) bool {
	if !s.requireGRPC(name) {
		return false
	}
	if s.curCall == nil {
		s.deferErr(fmt.Errorf("resonate: %s applies to a gRPC call — call Call(method) first", name))
		return false
	}
	return true
}

// GRPC switches the Scenario to build a GRPCGenerator against address
// ("host:port"). Add calls with Call; they run in order every iteration. The
// schema comes from server reflection unless ProtoFiles or DescriptorSet is
// given — and reflection is a network round trip, so Build can fail if the
// server is unreachable or has it off. Connections are TLS unless Plaintext.
func (s *Scenario) GRPC(address string) *Scenario {
	s.grpc = &GRPCTarget{Address: address}
	s.grpcOpts = DefaultGRPCOptions()
	s.curCall = nil
	return s
}

// Call appends a call to the scenario, named "package.Service/Method". The
// kind (unary, server-, client- or bidirectional-streaming) is read from the
// schema, never declared. Message/Messages/Receive/StreamDuration/Metadata/
// Extract/ExpectCode/ExpectBody then apply to this call until the next Call.
func (s *Scenario) Call(method string) *Scenario {
	if !s.requireGRPC("Call") {
		return s
	}
	s.grpc.Calls = append(s.grpc.Calls, GRPCCall{Method: method})
	s.curCall = &s.grpc.Calls[len(s.grpc.Calls)-1]
	return s
}

// grpcMessage sets the current call's request (the JSON form of the request
// message, templated). After WS, Message instead appends a frame.
func (s *Scenario) grpcMessage(body string) *Scenario {
	if !s.requireCall("Message") {
		return s
	}
	if s.curCall.Message != "" || len(s.curCall.Messages) > 0 {
		s.deferErr(fmt.Errorf("resonate: call %q already has a message — a gRPC call has one Message, or Messages for a client stream", s.curCall.Method))
		return s
	}
	s.curCall.Message = body
	return s
}

// Messages sets the requests a client- or bidirectional-streaming call sends,
// in order (JSON, templated). Use Message for a call that sends one.
func (s *Scenario) Messages(bodies ...string) *Scenario {
	if !s.requireCall("Messages") {
		return s
	}
	if s.curCall.Message != "" || len(s.curCall.Messages) > 0 {
		s.deferErr(fmt.Errorf("resonate: call %q already has a message — set Message or Messages, not both", s.curCall.Method))
		return s
	}
	s.curCall.Messages = bodies
	return s
}

// SendInterval spaces a streaming call's sends this far apart.
func (s *Scenario) SendInterval(d time.Duration) *Scenario {
	if s.requireCall("SendInterval") {
		s.curCall.SendInterval = d
	}
	return s
}

// SendRepeat sends a streaming call's Message(s) this many times over
// (Repeat is taken by the flow builder).
func (s *Scenario) SendRepeat(n int) *Scenario {
	if s.requireCall("SendRepeat") {
		s.curCall.Repeat = n
	}
	return s
}

// Receive ends a streaming call, successfully, after n response messages.
func (s *Scenario) Receive(n int) *Scenario {
	if s.requireCall("Receive") {
		s.curCall.Receive = n
	}
	return s
}

// Metadata sets gRPC metadata — the equivalent of an HTTP header. Before any
// Call it applies to every call; after one, to that call only (overriding a
// same-named scenario-level key).
func (s *Scenario) Metadata(key, value string) *Scenario {
	if !s.requireGRPC("Metadata") {
		return s
	}
	m := &s.grpc.Metadata
	if s.curCall != nil {
		m = &s.curCall.Metadata
	}
	if *m == nil {
		*m = map[string]string{}
	}
	(*m)[key] = value
	return s
}

// ExpectCode lists the gRPC status names (OK, NOT_FOUND, ...) that count as
// success for the current call, replacing the OK-only default.
func (s *Scenario) ExpectCode(codes ...string) *Scenario {
	if s.requireCall("ExpectCode") {
		s.curCall.ExpectCode = codes
	}
	return s
}

func (s *Scenario) grpcExpectBody(rule, value string) *Scenario {
	if !s.requireCall("ExpectBody") {
		return s
	}
	if s.curCall.ExpectBody == nil {
		s.curCall.ExpectBody = map[string]string{}
	}
	s.curCall.ExpectBody[rule] = value
	return s
}

func (s *Scenario) grpcExtract(name, rule string) *Scenario {
	if !s.requireCall("Extract") {
		return s
	}
	if s.curCall.Extract == nil {
		s.curCall.Extract = map[string]string{}
	}
	s.curCall.Extract[name] = rule
	return s
}

// Plaintext connects without TLS (an h2c gRPC server). It can't be combined
// with Insecure, CAFile or ClientCert — Build rejects that.
func (s *Scenario) Plaintext() *Scenario {
	if s.requireGRPC("Plaintext") {
		s.grpcOpts.Plaintext = true
	}
	return s
}

// Connections dials this many connections and spreads virtual users across
// them. One connection is one HTTP/2 connection, which servers typically cap
// at ~100 concurrent streams, so past that calls queue client-side and latency
// inflates — raise it with your concurrency. Default 1.
func (s *Scenario) Connections(n int) *Scenario {
	if s.requireGRPC("Connections") {
		s.grpcOpts.Connections = n
	}
	return s
}

// ProtoFiles takes the schema from .proto files (compiled in-process, no
// protoc) instead of server reflection.
func (s *Scenario) ProtoFiles(paths ...string) *Scenario {
	if s.requireGRPC("ProtoFiles") {
		s.grpcOpts.ProtoFiles = paths
	}
	return s
}

// ImportPaths are where ProtoFiles' imports are looked up; they default to the
// files' own directories.
func (s *Scenario) ImportPaths(paths ...string) *Scenario {
	if s.requireGRPC("ImportPaths") {
		s.grpcOpts.ImportPaths = paths
	}
	return s
}

// DescriptorSet takes the schema from a compiled FileDescriptorSet (the output
// of `protoc --descriptor_set_out`) instead of server reflection.
func (s *Scenario) DescriptorSet(path string) *Scenario {
	if s.requireGRPC("DescriptorSet") {
		s.grpcOpts.DescriptorSet = path
	}
	return s
}

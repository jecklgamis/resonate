package generator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jecklgamis/resonate/internal/tmpl"
)

// MCPDefaultProtocolVersion is the MCP protocol revision a client announces in
// its initialize request when MCPTarget.ProtocolVersion is empty.
const MCPDefaultProtocolVersion = "2025-06-18"

// Report steps for a session's setup. They are reserved: a call may not use
// either name, so setup and tool calls are never mixed in one report row.
const (
	mcpStepInitialize  = "initialize"
	mcpStepInitialized = "initialized"
)

// mcpMaxErrText bounds the server-supplied text copied into an error message,
// so the report's Errors table (keyed on err.Error()) stays small and stable.
const mcpMaxErrText = 120

// mcpMaxMessage caps one JSON-RPC reply that is read into memory when no
// MaxResponseBody is configured, so a misbehaving server cannot exhaust
// resonate's memory.
const mcpMaxMessage = 16 << 20

// MCPCall is one JSON-RPC request a session makes each iteration. Exactly one
// of Tool, Method or List is set.
//
//   - Tool calls a tool (method "tools/call"); Arguments are its arguments.
//   - List lists "tools", "resources" or "prompts" (method "<kind>/list").
//   - Method sends any other JSON-RPC method, with Params.
//
// Arguments and Params are either a mapping (rendered to JSON; every string
// value may embed {{ }} expressions and renders as a JSON string) or a string
// holding a raw JSON document, itself a template — the form to use when a
// templated value must be a JSON number or boolean:
//
//	arguments: '{"a": {{.Seq}}, "b": 2}'
type MCPCall struct {
	// Name labels this call in the report's per-step breakdown and in
	// step-scoped assertions. Optional: it defaults to the tool name, the
	// method, or "<kind>/list". Calls sharing a name are reported together.
	Name string

	Tool      string
	Arguments any

	List string

	Method string
	Params any

	// Extract pulls values out of the JSON-RPC result into {{.Vars.<name>}}
	// for later calls in the same iteration. Rules are json:, yaml:, xml: or
	// regex:, evaluated against the result object.
	Extract map[string]string

	// ExpectResult checks the JSON-RPC result with the same rule language as
	// Extract; an empty expected value only requires the rule to evaluate.
	ExpectResult map[string]string

	// ExpectToolError makes a tool-level error (result.isError: true) the
	// expected outcome of a Tool call: it passes, and a call that succeeds
	// fails. Without it a tool error is a failure.
	ExpectToolError bool
}

// MCPTarget describes an MCP server reached over the streamable HTTP
// transport and the calls each iteration makes against it.
//
// Every virtual user owns one MCP session: its first iteration sends
// "initialize" (reported as step "initialize"), keeps the Mcp-Session-Id the
// server returns, and sends "notifications/initialized" (step "initialized");
// later iterations reuse the session. A server that answers a session with
// HTTP 404 has expired it, and the virtual user opens a new one on its next
// iteration. Sessions are not closed explicitly: the engine has no "virtual
// user departed" hook.
//
// Each iteration then makes Calls in order, stopping at the first failure.
// URL, Header and every string in Arguments/Params may embed {{ }}
// expressions (see internal/tmpl), re-rendered on every request.
type MCPTarget struct {
	URL    string
	Header map[string]string
	Calls  []MCPCall

	// ProtocolVersion is announced in initialize and, once the server has
	// answered, the version it chose is sent as the MCP-Protocol-Version
	// header on every later request. Default MCPDefaultProtocolVersion.
	ProtocolVersion string

	// ClientName and ClientVersion identify resonate in initialize's
	// clientInfo. Defaults "resonate" and "1.0".
	ClientName    string
	ClientVersion string
}

// MCPOptions configures the transport and per-VU data shared by every
// session. HTTP carries the usual transport settings (Timeout, Insecure,
// certs/CA, H2C, HTTP3, MaxResponseBody, Feeder). Timeout bounds one whole
// JSON-RPC exchange, including reading an event-stream reply.
type MCPOptions struct {
	HTTP HTTPOptions

	// Identities, if non-empty, is a pool of named values (e.g. tokens).
	// Each virtual user (vuID) gets exactly one, round-robin over the pool,
	// sticky for that VU's lifetime, exposed as {{.Identity.<key>}}.
	Identities []map[string]string
}

func DefaultMCPOptions() MCPOptions {
	return MCPOptions{HTTP: DefaultHTTPOptions()}
}

// jsonRender renders a compiled JSON template to a JSON document.
type jsonRender func(tmpl.Data) (json.RawMessage, error)

type compiledMCPCall struct {
	name            string
	method          string
	params          jsonRender // nil: send no params
	isTool          bool
	extract         map[string]string
	expectResult    map[string]string
	expectToolError bool
}

// MCPGenerator drives MCP sessions: one JSON-RPC session per virtual user,
// reused across iterations.
//
// One iteration reports one Result per JSON-RPC request it made: the session
// setup (steps "initialize" and "initialized") the first time a virtual user
// runs, then one Result per call, with Latency = time until the JSON-RPC
// response arrived (for an event-stream reply, until the response event — not
// until the stream closed). StatusCode is the HTTP status. A call fails on a
// transport error, a non-200 status, a JSON-RPC error, a tool error
// (result.isError) or a failed expect_result check; each has its own stable
// error text so the report's Errors table separates the causes.
type MCPGenerator struct {
	fields     httpFields
	client     *http.Client
	timeout    time.Duration
	maxBody    int64
	identities []map[string]string
	feeder     *Feeder
	seq        uint64

	protocolVersion string
	clientName      string
	clientVersion   string
	calls           []compiledMCPCall

	states sync.Map // int(vuID) -> *mcpVU
}

// mcpVU is one virtual user's session.
type mcpVU struct {
	mu        sync.Mutex
	ready     bool
	sessionID string
	version   string // negotiated protocol version
	nextID    int64
}

func (v *mcpVU) reset() {
	v.ready = false
	v.sessionID = ""
	v.version = ""
}

func NewMCPGenerator(target MCPTarget, opts MCPOptions) (*MCPGenerator, error) {
	if len(target.Calls) == 0 {
		return nil, fmt.Errorf("mcp needs at least one call")
	}

	calls := make([]compiledMCPCall, 0, len(target.Calls))
	for i, c := range target.Calls {
		cc, err := compileMCPCall(c)
		if err != nil {
			return nil, fmt.Errorf("call %d: %w", i, err)
		}
		calls = append(calls, cc)
	}

	// The request template (URL, headers) is an HTTP POST whose body is set
	// per call; compiling it through compileHTTPFields reuses the literal-URL
	// validation and header templating every HTTP-based protocol shares.
	header := map[string]string{}
	for k, v := range target.Header {
		header[k] = v
	}
	if !hasHeader(header, "Content-Type") {
		header["Content-Type"] = "application/json"
	}
	if !hasHeader(header, "Accept") {
		header["Accept"] = "application/json, text/event-stream"
	}
	f, err := compileHTTPFields("POST", target.URL, nil, header, "", nil, opts.HTTP.BaseURL, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if opts.HTTP.HTTP3 {
		if err := requireHTTPSForHTTP3(f); err != nil {
			return nil, err
		}
	}

	client, err := newHTTPClient(opts.HTTP)
	if err != nil {
		return nil, err
	}
	// http.Client.Timeout would cut an event-stream reply short; each
	// exchange is bounded by a context deadline instead.
	client.Timeout = 0

	timeout := opts.HTTP.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	g := &MCPGenerator{
		fields:          f,
		client:          client,
		timeout:         timeout,
		maxBody:         opts.HTTP.MaxResponseBody,
		identities:      opts.Identities,
		feeder:          opts.HTTP.Feeder,
		protocolVersion: target.ProtocolVersion,
		clientName:      target.ClientName,
		clientVersion:   target.ClientVersion,
		calls:           calls,
	}
	if g.protocolVersion == "" {
		g.protocolVersion = MCPDefaultProtocolVersion
	}
	if g.clientName == "" {
		g.clientName = "resonate"
	}
	if g.clientVersion == "" {
		g.clientVersion = "1.0"
	}
	return g, nil
}

func hasHeader(h map[string]string, name string) bool {
	for k := range h {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func compileMCPCall(c MCPCall) (compiledMCPCall, error) {
	set := 0
	for _, s := range []string{c.Tool, c.List, c.Method} {
		if s != "" {
			set++
		}
	}
	switch {
	case set == 0:
		return compiledMCPCall{}, fmt.Errorf("set one of tool, list or method")
	case set > 1:
		return compiledMCPCall{}, fmt.Errorf("tool, list and method are mutually exclusive; set one")
	}
	if c.Tool == "" && c.Arguments != nil {
		return compiledMCPCall{}, fmt.Errorf("arguments only apply to a tool call (use params with method)")
	}
	if c.Method == "" && c.Params != nil {
		return compiledMCPCall{}, fmt.Errorf("params only apply to a method call (use arguments with tool)")
	}
	if c.Tool == "" && c.ExpectToolError {
		return compiledMCPCall{}, fmt.Errorf("expect_tool_error only applies to a tool call")
	}

	cc := compiledMCPCall{
		name:            c.Name,
		extract:         c.Extract,
		expectResult:    c.ExpectResult,
		expectToolError: c.ExpectToolError,
	}
	var err error
	switch {
	case c.Tool != "":
		cc.method, cc.isTool = "tools/call", true
		if cc.name == "" {
			cc.name = c.Tool
		}
		cc.params, err = compileToolParams(c.Tool, c.Arguments)
	case c.List != "":
		if !slices.Contains([]string{"tools", "resources", "prompts"}, c.List) {
			return compiledMCPCall{}, fmt.Errorf("invalid list %q (expected \"tools\", \"resources\" or \"prompts\")", c.List)
		}
		cc.method = c.List + "/list"
		if cc.name == "" {
			cc.name = cc.method
		}
	default:
		cc.method = c.Method
		if cc.name == "" {
			cc.name = c.Method
		}
		if c.Params != nil {
			cc.params, err = compileJSONDoc("params", c.Params)
		}
	}
	if err != nil {
		return compiledMCPCall{}, err
	}

	if cc.name == mcpStepInitialize || cc.name == mcpStepInitialized {
		return compiledMCPCall{}, fmt.Errorf("the step name %q is reserved for session setup; give the call another name", cc.name)
	}
	for _, rule := range c.Extract {
		if err := checkMCPRule(rule); err != nil {
			return compiledMCPCall{}, fmt.Errorf("extract: %w", err)
		}
	}
	for rule := range c.ExpectResult {
		if err := checkMCPRule(rule); err != nil {
			return compiledMCPCall{}, fmt.Errorf("expect_result: %w", err)
		}
	}
	return cc, nil
}

// checkMCPRule validates an extract/expect_result rule. A JSON-RPC result has
// no headers or status, so header: and status rules can never work.
func checkMCPRule(rule string) error {
	if !isKnownExtractRule(rule) {
		return fmt.Errorf("invalid rule %q (expected \"json:<path>\", \"yaml:<path>\", \"xml:<path>\" or \"regex:<pattern>\")", rule)
	}
	if rule == "status" || strings.HasPrefix(rule, "header:") || strings.HasPrefix(rule, "css:") {
		return fmt.Errorf("rule %q doesn't apply to a JSON-RPC result (use json:, yaml:, xml: or regex:)", rule)
	}
	return nil
}

// compileToolParams builds the tools/call params {name, arguments}.
func compileToolParams(tool string, args any) (jsonRender, error) {
	if args == nil {
		doc, err := json.Marshal(map[string]any{"name": tool})
		if err != nil {
			return nil, err
		}
		return func(tmpl.Data) (json.RawMessage, error) { return doc, nil }, nil
	}
	argsRender, err := compileJSONDoc("arguments", args)
	if err != nil {
		return nil, err
	}
	name, _ := json.Marshal(tool)
	return func(d tmpl.Data) (json.RawMessage, error) {
		a, err := argsRender(d)
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		b.WriteString(`{"name":`)
		b.Write(name)
		b.WriteString(`,"arguments":`)
		b.Write(a)
		b.WriteByte('}')
		return b.Bytes(), nil
	}, nil
}

// compileJSONDoc compiles an arguments/params value: a string is a raw JSON
// template; anything else (a YAML mapping, list or scalar) is rendered to JSON
// with every string leaf treated as a template.
func compileJSONDoc(name string, v any) (jsonRender, error) {
	if s, ok := v.(string); ok {
		f, err := tmpl.Compile(name, s)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(s, "{{") && !json.Valid([]byte(s)) {
			return nil, fmt.Errorf("%s is not valid JSON: %q", name, truncateText(s, mcpMaxErrText))
		}
		return func(d tmpl.Data) (json.RawMessage, error) {
			out, err := f.Render(d)
			if err != nil {
				return nil, fmt.Errorf("rendering %s: %w", name, err)
			}
			if !json.Valid([]byte(out)) {
				return nil, fmt.Errorf("%s is not valid JSON after templating: %q", name, truncateText(out, mcpMaxErrText))
			}
			return json.RawMessage(out), nil
		}, nil
	}
	node, err := compileJSONNode(name, v)
	if err != nil {
		return nil, err
	}
	return func(d tmpl.Data) (json.RawMessage, error) {
		val, err := node(d)
		if err != nil {
			return nil, fmt.Errorf("rendering %s: %w", name, err)
		}
		return json.Marshal(val)
	}, nil
}

// compileJSONNode compiles a decoded YAML value into a function that renders
// the equivalent JSON-marshalable value, templating every string leaf.
func compileJSONNode(name string, v any) (func(tmpl.Data) (any, error), error) {
	switch x := v.(type) {
	case string:
		f, err := tmpl.Compile(name, x)
		if err != nil {
			return nil, err
		}
		return func(d tmpl.Data) (any, error) { return f.Render(d) }, nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		kids := make([]func(tmpl.Data) (any, error), len(keys))
		for i, k := range keys {
			n, err := compileJSONNode(name+"."+k, x[k])
			if err != nil {
				return nil, err
			}
			kids[i] = n
		}
		return func(d tmpl.Data) (any, error) {
			out := make(map[string]any, len(keys))
			for i, k := range keys {
				val, err := kids[i](d)
				if err != nil {
					return nil, err
				}
				out[k] = val
			}
			return out, nil
		}, nil
	case map[any]any:
		conv := make(map[string]any, len(x))
		for k, val := range x {
			conv[fmt.Sprint(k)] = val
		}
		return compileJSONNode(name, conv)
	case []any:
		kids := make([]func(tmpl.Data) (any, error), len(x))
		for i, e := range x {
			n, err := compileJSONNode(name+"["+strconv.Itoa(i)+"]", e)
			if err != nil {
				return nil, err
			}
			kids[i] = n
		}
		return func(d tmpl.Data) (any, error) {
			out := make([]any, len(kids))
			for i, k := range kids {
				val, err := k(d)
				if err != nil {
					return nil, err
				}
				out[i] = val
			}
			return out, nil
		}, nil
	default: // numbers, booleans, null
		return func(tmpl.Data) (any, error) { return x, nil }, nil
	}
}

// truncateText collapses whitespace and cuts s to at most n runes (never in the
// middle of a multi-byte character), for server-supplied text that ends up in
// an error message.
func truncateText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func (a *MCPGenerator) Protocol() string { return "mcp" }

// StepNames lists the report labels this generator's results carry, so
// step-scoped assertions can be validated before the run.
func (a *MCPGenerator) StepNames() []string {
	names := []string{mcpStepInitialize, mcpStepInitialized}
	for _, c := range a.calls {
		if !slices.Contains(names, c.name) {
			names = append(names, c.name)
		}
	}
	return names
}

func (a *MCPGenerator) Close() error {
	closeClient(a.client)
	a.feeder.Close()
	return nil
}

func (a *MCPGenerator) identityFor(vuID int) map[string]string {
	if len(a.identities) == 0 {
		return nil
	}
	return a.identities[vuID%len(a.identities)]
}

func (a *MCPGenerator) Do(ctx context.Context, vuID int) []Result {
	v, _ := a.states.LoadOrStore(vuID, &mcpVU{})
	vu := v.(*mcpVU)
	vu.mu.Lock()
	defer vu.mu.Unlock()

	vars := map[string]string{}
	data := tmpl.Data{
		Seq:      atomic.AddUint64(&a.seq, 1) - 1,
		VU:       vuID,
		Identity: a.identityFor(vuID),
		Vars:     vars,
		Feeder:   a.feeder.Next(),
	}

	var results []Result
	if !vu.ready {
		if !a.openSession(ctx, vu, data, &results) {
			return results
		}
	}
	vars["session_id"] = vu.sessionID

	for _, call := range a.calls {
		if ctx.Err() != nil {
			return results
		}
		params, err := renderParams(call, data)
		if err != nil {
			results = append(results, Result{Timestamp: time.Now(), Error: err, Protocol: "mcp", Step: call.name})
			return results
		}
		res, out, dropped := a.exchange(ctx, vu, data, call.name, call.method, params, false)
		if dropped {
			return results
		}
		a.evaluate(call, out, &res, vars)
		results = append(results, res)
		if !res.Success() {
			return results
		}
	}
	return results
}

func renderParams(call compiledMCPCall, data tmpl.Data) (json.RawMessage, error) {
	if call.params == nil {
		return nil, nil
	}
	return call.params(data)
}

// openSession runs initialize + notifications/initialized for vu, appending
// their Results. It reports whether the session is ready.
func (a *MCPGenerator) openSession(ctx context.Context, vu *mcpVU, data tmpl.Data, results *[]Result) bool {
	vu.reset()
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": a.protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": a.clientName, "version": a.clientVersion},
	})
	res, out, dropped := a.exchange(ctx, vu, data, mcpStepInitialize, "initialize", params, false)
	if dropped {
		return false
	}
	if res.Error == nil && out.rpcErr == nil {
		var init struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(out.result, &init)
		vu.version = init.ProtocolVersion
		if vu.version == "" {
			vu.version = a.protocolVersion
		}
		vu.sessionID = out.header.Get("Mcp-Session-Id")
	}
	*results = append(*results, res)
	if !res.Success() {
		return false
	}

	res, _, dropped = a.exchange(ctx, vu, data, mcpStepInitialized, "notifications/initialized", nil, true)
	if dropped {
		return false
	}
	*results = append(*results, res)
	if !res.Success() {
		vu.reset()
		return false
	}
	vu.ready = true
	return true
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpOutcome is what an exchange learned beyond the Result.
type mcpOutcome struct {
	result json.RawMessage
	rpcErr *mcpRPCError
	header http.Header
}

type mcpMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *mcpRPCError    `json:"error"`
	Method string          `json:"method"`
}

// exchange sends one JSON-RPC request (or, for a notification, a message with
// no id) and reads the reply. dropped reports that the run itself ended
// mid-exchange, so nothing should be reported.
func (a *MCPGenerator) exchange(ctx context.Context, vu *mcpVU, data tmpl.Data, step, method string, params json.RawMessage, notification bool) (res Result, out mcpOutcome, dropped bool) {
	res = Result{Protocol: "mcp", Step: step}

	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int64          `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	var id int64
	if !notification {
		vu.nextID++
		id = vu.nextID
		msg.ID = &id
	}
	body, err := json.Marshal(msg)
	if err != nil {
		res.Timestamp, res.Error = time.Now(), err
		return res, out, false
	}

	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	f := a.fields
	f.rawBody = body
	req, bodyLen, err := buildRequest(callCtx, f, data)
	if err != nil {
		res.Timestamp, res.Error = time.Now(), err
		return res, out, false
	}
	if vu.sessionID != "" && method != "initialize" {
		req.Header.Set("Mcp-Session-Id", vu.sessionID)
	}
	if vu.version != "" && method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", vu.version)
	}
	res.BytesOut = int64(bodyLen)

	start := time.Now()
	res.Timestamp = start
	resp, err := a.client.Do(req)
	if err != nil {
		if runEnded(ctx) {
			return res, out, true
		}
		res.Latency = time.Since(start)
		res.Error = a.transportErr(callCtx, err)
		return res, out, false
	}
	defer resp.Body.Close()
	res.StatusCode = resp.StatusCode
	out.header = resp.Header

	reader, _, decErr := decodeResponseBody(resp)
	if decErr != nil {
		reader = resp.Body
	}
	limit := a.maxBody
	if limit <= 0 {
		limit = mcpMaxMessage
	}
	counter := &countingReader{r: io.LimitReader(reader, limit)}

	finish := func() {
		res.Latency = time.Since(start)
		res.BytesIn = counter.n
	}

	switch {
	case notification:
		_, _ = io.Copy(io.Discard, counter)
		finish()
		if resp.StatusCode != 200 && resp.StatusCode != 202 && resp.StatusCode != 204 {
			res.Error = fmt.Errorf("mcp: %s: unexpected HTTP status %d", method, resp.StatusCode)
		}
		return res, out, false

	case resp.StatusCode != 200:
		// An error status may still carry a JSON-RPC error body that explains it.
		b, _ := io.ReadAll(io.LimitReader(counter, 64<<10))
		finish()
		res.Error = httpStatusErr(resp.StatusCode, b)
		if resp.StatusCode == 404 && vu.sessionID != "" {
			// The server no longer knows this session; open a new one next time.
			vu.reset()
		}
		return res, out, false
	}

	if decErr != nil {
		_, _ = io.Copy(io.Discard, counter)
		finish()
		res.Error = decErr
		return res, out, false
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	var m *mcpMessage
	switch mediaType {
	case "text/event-stream":
		m, err = readMCPEventStream(counter, id)
		finish()
		if err == nil {
			// Let the server finish the stream so the connection can be
			// reused, but never wait on one that stays open.
			drainBriefly(counter, cancel)
		}
	case "application/json":
		var b []byte
		b, err = io.ReadAll(counter)
		finish()
		if err == nil {
			m, err = findMCPMessage(b, id)
		}
	default:
		_, _ = io.Copy(io.Discard, counter)
		finish()
		err = fmt.Errorf("mcp: unexpected content-type %q (want application/json or text/event-stream)", resp.Header.Get("Content-Type"))
	}
	if err != nil {
		if runEnded(ctx) {
			return res, out, true
		}
		res.Error = a.transportErr(callCtx, err)
		return res, out, false
	}

	if m.Error != nil {
		out.rpcErr = m.Error
		res.Error = fmt.Errorf("mcp: json-rpc error %d: %s", m.Error.Code, truncateText(m.Error.Message, mcpMaxErrText))
		return res, out, false
	}
	out.result = m.Result
	if len(out.result) == 0 {
		out.result = json.RawMessage("null")
	}
	return res, out, false
}

// transportErr turns a failed exchange into a stable error: a deadline we set
// reads as a timeout rather than Go's address-bearing url.Error text.
func (a *MCPGenerator) transportErr(callCtx context.Context, err error) error {
	if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("mcp: timed out after %s", a.timeout)
	}
	return err
}

func httpStatusErr(status int, body []byte) error {
	if m, err := findMCPMessage(body, -1); err == nil && m != nil && m.Error != nil {
		return fmt.Errorf("mcp: HTTP %d: json-rpc error %d: %s", status, m.Error.Code, truncateText(m.Error.Message, mcpMaxErrText))
	}
	return fmt.Errorf("mcp: unexpected HTTP status %d", status)
}

// findMCPMessage finds the JSON-RPC response for id in body, a single
// message or a batch. An id of -1 accepts any response.
func findMCPMessage(body []byte, id int64) (*mcpMessage, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("mcp: empty reply")
	}
	var msgs []mcpMessage
	if body[0] == '[' {
		if err := json.Unmarshal(body, &msgs); err != nil {
			return nil, fmt.Errorf("mcp: reply is not valid JSON-RPC: %w", err)
		}
	} else {
		var m mcpMessage
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("mcp: reply is not valid JSON-RPC: %w", err)
		}
		msgs = []mcpMessage{m}
	}
	for i := range msgs {
		if msgs[i].Method == "" && idMatches(msgs[i].ID, id) {
			return &msgs[i], nil
		}
	}
	return nil, fmt.Errorf("mcp: no JSON-RPC response for request %d in the reply", id)
}

func idMatches(raw json.RawMessage, id int64) bool {
	if len(raw) == 0 {
		return false
	}
	if id < 0 {
		return true
	}
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n == id
}

// readMCPEventStream reads a text/event-stream reply until the JSON-RPC
// response for id arrives. Other messages on the stream (progress and logging
// notifications, server-initiated requests) are skipped.
func readMCPEventStream(r io.Reader, id int64) (*mcpMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), sseMaxLine)
	var data bytes.Buffer
	dispatch := func() *mcpMessage {
		if data.Len() == 0 {
			return nil
		}
		payload := bytes.Clone(data.Bytes())
		data.Reset()
		m, err := findMCPMessage(payload, id)
		if err != nil {
			return nil
		}
		return m
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m := dispatch(); m != nil {
				return m, nil
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if m := dispatch(); m != nil { // a final event with no trailing blank line
		return m, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("mcp: stream ended without a response to request %d", id)
}

// drainBriefly reads what is left of a reply so its connection can be reused,
// cancelling (and so closing the connection) if the server keeps the stream
// open instead of finishing it.
func drainBriefly(r io.Reader, cancel context.CancelFunc) {
	t := time.AfterFunc(100*time.Millisecond, cancel)
	defer t.Stop()
	_, _ = io.Copy(io.Discard, r)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// evaluate applies a call's tool-error, expect_result and extract settings to
// a successful JSON-RPC exchange, failing res when a check does not hold.
func (a *MCPGenerator) evaluate(call compiledMCPCall, out mcpOutcome, res *Result, vars map[string]string) {
	if res.Error != nil || out.result == nil {
		return
	}

	var reasons []string
	if call.isTool {
		var tr struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(out.result, &tr)
		switch {
		case tr.IsError && !call.expectToolError:
			text := ""
			if len(tr.Content) > 0 {
				text = truncateText(tr.Content[0].Text, mcpMaxErrText)
			}
			res.Error = fmt.Errorf("mcp: tool error: %s", text)
			return
		case !tr.IsError && call.expectToolError:
			reasons = append(reasons, "expected a tool error (isError: true) but the call succeeded")
		}
	}

	rules := make([]string, 0, len(call.expectResult))
	for rule := range call.expectResult {
		rules = append(rules, rule)
	}
	slices.Sort(rules)
	for _, rule := range rules {
		want := call.expectResult[rule]
		got, err := extractValue(rule, out.result, nil, 0)
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("result %q: %v", rule, err))
		case want != "" && got != want:
			reasons = append(reasons, fmt.Sprintf("result %q: got %q, want %q", rule, got, want))
		}
	}
	if len(reasons) > 0 {
		res.Error = fmt.Errorf("check failed: %s", strings.Join(reasons, "; "))
		return
	}

	for name, rule := range call.extract {
		if v, err := extractValue(rule, out.result, nil, 0); err == nil {
			vars[name] = v
		}
	}
}

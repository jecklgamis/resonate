package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// --- a fake MCP server ----------------------------------------------------------

type mcpSeen struct {
	method  string
	id      string
	session string
	version string
	accept  string
	ctype   string
	auth    string
	params  map[string]any
	body    string
}

// fakeMCP is a minimal streamable-HTTP MCP server that records every request.
// Its knobs switch the reply format and inject the failures the generator must
// classify.
type fakeMCP struct {
	t *testing.T

	// reply format
	sse        bool // reply as text/event-stream, preceded by a notification
	batch      bool // reply as a JSON array holding an unrelated message first
	holdStream bool // leave an event stream open after the response

	// stateless: never issue a session id (and accept requests without one)
	stateless bool

	// status forces the HTTP status of a method's reply (with an error body).
	status map[string]int

	// rpcError makes a method answer with a JSON-RPC error.
	rpcError map[string]int

	// ctype overrides the Content-Type of a plain JSON reply.
	ctype string

	// slow delays every tools/call by this long.
	slow time.Duration

	mu       sync.Mutex
	seen     []mcpSeen
	sessions map[string]bool
	nextID   int
	expired  map[string]bool
}

func newFakeMCP(t *testing.T, configure ...func(*fakeMCP)) (*fakeMCP, *httptest.Server) {
	t.Helper()
	f := &fakeMCP{t: t, sessions: map[string]bool{}, expired: map[string]bool{}, status: map[string]int{}, rpcError: map[string]int{}}
	for _, c := range configure {
		c(f)
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

// expire makes the server forget a session, as a restart or reaper would.
func (f *fakeMCP) expire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
}

func (f *fakeMCP) expireAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = map[string]bool{}
}

func (f *fakeMCP) calls(method string) []mcpSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mcpSeen
	for _, s := range f.seen {
		if s.method == method {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.seen = append(f.seen, mcpSeen{
		method: req.Method, id: string(req.ID), session: r.Header.Get("Mcp-Session-Id"),
		version: r.Header.Get("MCP-Protocol-Version"), accept: r.Header.Get("Accept"),
		ctype: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization"),
		params: req.Params, body: string(body),
	})
	f.mu.Unlock()

	if code, ok := f.status[req.Method]; ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"forced status"}}`)
		return
	}

	if req.Method == "initialize" {
		if !f.stateless {
			f.mu.Lock()
			f.nextID++
			id := fmt.Sprintf("sess-%d", f.nextID)
			f.sessions[id] = true
			f.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", id)
		}
		f.reply(w, req.ID, map[string]any{
			"protocolVersion": "2025-03-26", // deliberately not the default: the client must adopt it
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		}, nil, nil)
		return
	}
	if !f.stateless {
		f.mu.Lock()
		ok := f.sessions[r.Header.Get("Mcp-Session-Id")]
		f.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"unknown session"}}`)
			return
		}
	}
	if len(req.ID) == 0 { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if code, ok := f.rpcError[req.Method]; ok {
		f.reply(w, req.ID, nil, &mcpRPCError{Code: code, Message: "forced rpc error"}, nil)
		return
	}

	switch req.Method {
	case "tools/call":
		if f.slow > 0 {
			time.Sleep(f.slow)
		}
		name, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		f.reply(w, req.ID, f.tool(name, args), nil, []any{map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"data": "working"}}})
	case "tools/list":
		f.reply(w, req.ID, map[string]any{"tools": []any{map[string]any{"name": "add"}}}, nil, nil)
	default:
		f.reply(w, req.ID, map[string]any{"echoed": req.Method}, nil, nil)
	}
}

func (f *fakeMCP) tool(name string, args map[string]any) map[string]any {
	text := func(s string, isErr bool) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isErr}
	}
	switch name {
	case "add":
		a, _ := args["a"].(float64)
		b, _ := args["b"].(float64)
		r := text(fmt.Sprint(a+b), false)
		r["structuredContent"] = map[string]any{"result": a + b}
		return r
	case "echo":
		s, _ := args["message"].(string)
		return text(s, false)
	case "fail":
		return text("this tool always fails", true)
	}
	return text("unknown tool "+name, true)
}

// reply writes a JSON-RPC response in whichever format the fake is set to.
// before are extra messages that precede the response in an event stream.
func (f *fakeMCP) reply(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *mcpRPCError, before []any) {
	resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	switch {
	case f.sse:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, m := range append(before, resp) {
			b, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		if f.holdStream {
			time.Sleep(2 * time.Second) // a server that never closes the stream
		}
	case f.batch:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(append([]any{map[string]any{"jsonrpc": "2.0", "method": "notifications/progress"}}, resp))
	default:
		ct := "application/json"
		if f.ctype != "" {
			ct = f.ctype
		}
		w.Header().Set("Content-Type", ct)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// --- helpers ----------------------------------------------------------------------

func mcpGen(t *testing.T, url string, calls []MCPCall, tweak ...func(*MCPTarget, *MCPOptions)) *MCPGenerator {
	t.Helper()
	target := MCPTarget{URL: url, Calls: calls}
	opts := DefaultMCPOptions()
	opts.HTTP.Timeout = 5 * time.Second
	for _, tw := range tweak {
		tw(&target, &opts)
	}
	g, err := NewMCPGenerator(target, opts)
	if err != nil {
		t.Fatalf("NewMCPGenerator: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func steps(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Step
	}
	return out
}

func errText(r Result) string {
	if r.Error == nil {
		return ""
	}
	return r.Error.Error()
}

var addCall = MCPCall{Tool: "add", Arguments: map[string]any{"a": 2, "b": 3}}

// --- tests ----------------------------------------------------------------------------

func TestMCPSessionLifecycleAndHeaders(t *testing.T) {
	for _, mode := range []struct {
		name string
		cfg  func(*fakeMCP)
	}{
		{"json", func(*fakeMCP) {}},
		{"event stream", func(f *fakeMCP) { f.sse = true }},
		{"batch", func(f *fakeMCP) { f.batch = true }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			f, srv := newFakeMCP(t, mode.cfg)
			g := mcpGen(t, srv.URL, []MCPCall{addCall}, func(tg *MCPTarget, _ *MCPOptions) {
				tg.Header = map[string]string{"Authorization": "Bearer {{.Identity.token}}"}
				tg.ClientName, tg.ClientVersion = "perf", "9"
			}, func(_ *MCPTarget, o *MCPOptions) { o.Identities = []map[string]string{{"token": "abc"}} })

			rs := g.Do(context.Background(), 0)
			if got, want := strings.Join(steps(rs), ","), "initialize,initialized,add"; got != want {
				t.Fatalf("steps = %s, want %s", got, want)
			}
			for _, r := range rs {
				if !r.Success() || r.Protocol != "mcp" {
					t.Errorf("%s: success=%v protocol=%q err=%v", r.Step, r.Success(), r.Protocol, r.Error)
				}
			}
			if rs[1].StatusCode != 202 || rs[2].StatusCode != 200 {
				t.Errorf("status codes initialized=%d add=%d, want 202 and 200", rs[1].StatusCode, rs[2].StatusCode)
			}
			if rs[2].BytesOut == 0 || rs[2].BytesIn == 0 || rs[2].Latency <= 0 {
				t.Errorf("add result has no size/latency: %+v", rs[2])
			}

			init := f.calls("initialize")[0]
			if init.session != "" || init.version != "" {
				t.Errorf("initialize must not carry a session or version header: %+v", init)
			}
			if init.accept != "application/json, text/event-stream" || init.ctype != "application/json" {
				t.Errorf("accept=%q content-type=%q", init.accept, init.ctype)
			}
			if init.auth != "Bearer abc" {
				t.Errorf("templated header = %q, want Bearer abc", init.auth)
			}
			ci, _ := init.params["clientInfo"].(map[string]any)
			if init.params["protocolVersion"] != MCPDefaultProtocolVersion || ci["name"] != "perf" || ci["version"] != "9" {
				t.Errorf("initialize params = %v", init.params)
			}

			note := f.calls("notifications/initialized")[0]
			if note.id != "" || note.session != "sess-1" || note.version != "2025-03-26" {
				t.Errorf("initialized notification = %+v (no id, the session, and the version the server chose)", note)
			}
			call := f.calls("tools/call")[0]
			if call.session != "sess-1" || call.version != "2025-03-26" || call.auth != "Bearer abc" {
				t.Errorf("tools/call headers = %+v", call)
			}
			if call.params["name"] != "add" {
				t.Errorf("tools/call params = %v", call.params)
			}
		})
	}
}

func TestMCPSessionIsReusedPerVirtualUser(t *testing.T) {
	f, srv := newFakeMCP(t)
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	ctx := context.Background()

	first := g.Do(ctx, 0)
	second := g.Do(ctx, 0)
	if len(first) != 3 || len(second) != 1 || second[0].Step != "add" {
		t.Fatalf("iteration 1 steps %v, iteration 2 steps %v; setup must only run once per VU", steps(first), steps(second))
	}
	g.Do(ctx, 1) // another VU opens its own session
	if n := len(f.calls("initialize")); n != 2 {
		t.Errorf("initialize sent %d times, want 2 (one per VU)", n)
	}
	sessions := map[string]bool{}
	for _, c := range f.calls("tools/call") {
		sessions[c.session] = true
	}
	if len(sessions) != 2 {
		t.Errorf("tool calls used sessions %v, want two distinct", sessions)
	}
}

func TestMCPStatelessServerWorksWithoutASessionID(t *testing.T) {
	f, srv := newFakeMCP(t, func(f *fakeMCP) { f.stateless = true })
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	rs := g.Do(context.Background(), 0)
	if len(rs) != 3 || failedAny(rs) {
		t.Fatalf("results %v, errors %v", steps(rs), errs(rs))
	}
	if s := f.calls("tools/call")[0].session; s != "" {
		t.Errorf("sent session %q to a server that issued none", s)
	}
}

func failedAny(rs []Result) bool {
	for _, r := range rs {
		if !r.Success() {
			return true
		}
	}
	return false
}

func errs(rs []Result) []string {
	var out []string
	for _, r := range rs {
		if r.Error != nil {
			out = append(out, r.Step+": "+r.Error.Error())
		}
	}
	return out
}

func TestMCPExpiredSessionIsReopened(t *testing.T) {
	f, srv := newFakeMCP(t)
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	ctx := context.Background()
	g.Do(ctx, 0)
	f.expireAll()

	rs := g.Do(ctx, 0)
	if len(rs) != 1 || rs[0].StatusCode != 404 || rs[0].Success() {
		t.Fatalf("a call on an expired session should fail once with 404: %+v", rs)
	}
	if !strings.Contains(errText(rs[0]), "HTTP 404") || !strings.Contains(errText(rs[0]), "unknown session") {
		t.Errorf("error = %q", errText(rs[0]))
	}

	rs = g.Do(ctx, 0)
	if got, want := strings.Join(steps(rs), ","), "initialize,initialized,add"; got != want || failedAny(rs) {
		t.Fatalf("next iteration should open a new session: steps %s errors %v", got, errs(rs))
	}
	if n := len(f.calls("initialize")); n != 2 {
		t.Errorf("initialize sent %d times, want 2", n)
	}
}

func TestMCPSetupFailureIsRetriedNextIteration(t *testing.T) {
	f, srv := newFakeMCP(t, func(f *fakeMCP) { f.status["initialize"] = 500 })
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	ctx := context.Background()

	rs := g.Do(ctx, 0)
	if len(rs) != 1 || rs[0].Step != "initialize" || rs[0].Success() {
		t.Fatalf("a failed initialize must stop the iteration: %v %v", steps(rs), errs(rs))
	}
	if !strings.Contains(errText(rs[0]), "HTTP 500") {
		t.Errorf("error = %q", errText(rs[0]))
	}
	g.Do(ctx, 0)
	if n := len(f.calls("initialize")); n != 2 {
		t.Errorf("initialize attempted %d times, want 2 (a failed setup is retried, not cached)", n)
	}
	if n := len(f.calls("tools/call")); n != 0 {
		t.Errorf("%d tool calls sent without a session", n)
	}

	// A failing initialized notification leaves the session unusable too.
	f2, srv2 := newFakeMCP(t, func(f *fakeMCP) { f.status["notifications/initialized"] = 500 })
	g2 := mcpGen(t, srv2.URL, []MCPCall{addCall})
	rs = g2.Do(ctx, 0)
	if got := strings.Join(steps(rs), ","); got != "initialize,initialized" || rs[1].Success() {
		t.Fatalf("steps %s errors %v", got, errs(rs))
	}
	g2.Do(ctx, 0)
	if n := len(f2.calls("initialize")); n != 2 {
		t.Errorf("initialize attempted %d times, want 2", n)
	}
}

func TestMCPFailureClassification(t *testing.T) {
	cases := []struct {
		name    string
		cfg     func(*fakeMCP)
		call    MCPCall
		wantErr string
		status  int
	}{
		{"tool error", nil, MCPCall{Tool: "fail"}, "mcp: tool error: this tool always fails", 200},
		{"json-rpc error", func(f *fakeMCP) { f.rpcError["tools/call"] = -32602 }, addCall, "mcp: json-rpc error -32602: forced rpc error", 200},
		{"http error with json-rpc body", func(f *fakeMCP) { f.status["tools/call"] = 503 }, addCall, "mcp: HTTP 503: json-rpc error -32000: forced status", 503},
		{"result check", nil, MCPCall{Tool: "add", Arguments: map[string]any{"a": 1, "b": 1}, ExpectResult: map[string]string{"json:structuredContent.result": "3"}}, `check failed: result "json:structuredContent.result": got "2", want "3"`, 200},
		{"unevaluable rule", nil, MCPCall{Tool: "add", Arguments: map[string]any{"a": 1, "b": 1}, ExpectResult: map[string]string{"json:structuredContent.nope": ""}}, "check failed: result", 200},
		{"expected tool error did not happen", nil, MCPCall{Tool: "add", Arguments: map[string]any{"a": 1, "b": 1}, ExpectToolError: true}, "expected a tool error", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg []func(*fakeMCP)
			if tc.cfg != nil {
				cfg = append(cfg, tc.cfg)
			}
			_, srv := newFakeMCP(t, cfg...)
			g := mcpGen(t, srv.URL, []MCPCall{tc.call})
			rs := g.Do(context.Background(), 0)
			last := rs[len(rs)-1]
			if last.Success() {
				t.Fatalf("expected a failure, got %+v", last)
			}
			if !strings.Contains(errText(last), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", errText(last), tc.wantErr)
			}
			if last.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", last.StatusCode, tc.status)
			}
		})
	}
}

func TestMCPExpectToolErrorPassesAnExpectedFailure(t *testing.T) {
	_, srv := newFakeMCP(t)
	g := mcpGen(t, srv.URL, []MCPCall{{Tool: "fail", ExpectToolError: true}})
	rs := g.Do(context.Background(), 0)
	if failedAny(rs) {
		t.Fatalf("an expected tool error should pass: %v", errs(rs))
	}
}

func TestMCPFailureStopsTheRestOfTheIteration(t *testing.T) {
	_, srv := newFakeMCP(t)
	g := mcpGen(t, srv.URL, []MCPCall{{Tool: "fail"}, addCall})
	rs := g.Do(context.Background(), 0)
	if got := strings.Join(steps(rs), ","); got != "initialize,initialized,fail" {
		t.Errorf("steps = %s; the call after a failure must not run", got)
	}
}

func TestMCPCallsRenderTemplatesAndChainExtractedValues(t *testing.T) {
	f, srv := newFakeMCP(t)
	calls := []MCPCall{
		{Tool: "add", Arguments: map[string]any{"a": 4, "b": 5}, Extract: map[string]string{"sum": "json:structuredContent.result"}},
		// raw JSON form: a templated value that must be a number, not a string
		{Tool: "add", Name: "second", Arguments: `{"a": {{.Vars.sum}}, "b": {{.Seq}}}`},
		// mapping form: string leaves are templated and stay strings
		{Tool: "echo", Arguments: map[string]any{"message": "vu={{.VU}} who={{.Identity.name}} sid={{.Vars.session_id}}", "nested": map[string]any{"list": []any{"{{.Vars.sum}}", 7, true}}}},
	}
	g := mcpGen(t, srv.URL, calls, func(_ *MCPTarget, o *MCPOptions) {
		o.Identities = []map[string]string{{"name": "ann"}, {"name": "bob"}}
	})
	rs := g.Do(context.Background(), 3) // vu 3 -> identity index 1 (bob)
	if failedAny(rs) {
		t.Fatalf("errors: %v", errs(rs))
	}
	got := f.calls("tools/call")
	if a := got[1].params["arguments"].(map[string]any); a["a"] != float64(9) || a["b"] != float64(0) {
		t.Errorf("second call arguments = %v, want a=9 (extracted) b=0 (seq) as numbers", a)
	}
	args := got[2].params["arguments"].(map[string]any)
	if args["message"] != "vu=3 who=bob sid=sess-1" {
		t.Errorf("message = %q", args["message"])
	}
	list := args["nested"].(map[string]any)["list"].([]any)
	if list[0] != "9" || list[1] != float64(7) || list[2] != true {
		t.Errorf("nested list = %v; string leaves render as strings, other scalars stay as they are", list)
	}
	if rs[len(rs)-2].Step != "second" {
		t.Errorf("the Name override was not used: %v", steps(rs))
	}
}

func TestMCPListMethodAndParams(t *testing.T) {
	f, srv := newFakeMCP(t)
	g := mcpGen(t, srv.URL, []MCPCall{
		{List: "tools"},
		{Method: "resources/read", Params: map[string]any{"uri": "file:///{{.VU}}"}, ExpectResult: map[string]string{"json:echoed": "resources/read"}},
		{Method: "ping"},
	})
	rs := g.Do(context.Background(), 2)
	if failedAny(rs) {
		t.Fatalf("errors: %v", errs(rs))
	}
	if got, want := strings.Join(steps(rs), ","), "initialize,initialized,tools/list,resources/read,ping"; got != want {
		t.Errorf("steps = %s, want %s (default names are the JSON-RPC methods)", got, want)
	}
	if p := f.calls("resources/read")[0].params; p["uri"] != "file:///2" {
		t.Errorf("params = %v", p)
	}
	if body := f.calls("ping")[0].body; strings.Contains(body, "params") {
		t.Errorf("a call with no params must not send any: %s", body)
	}
}

func TestMCPEventStreamReplies(t *testing.T) {
	t.Run("notifications before the response are skipped", func(t *testing.T) {
		_, srv := newFakeMCP(t, func(f *fakeMCP) { f.sse = true })
		g := mcpGen(t, srv.URL, []MCPCall{{Tool: "echo", Arguments: map[string]any{"message": "hi"}, ExpectResult: map[string]string{"json:content[0].text": "hi"}}})
		if rs := g.Do(context.Background(), 0); failedAny(rs) {
			t.Fatalf("errors: %v", errs(rs))
		}
	})
	t.Run("a stream the server leaves open does not stall the client", func(t *testing.T) {
		_, srv := newFakeMCP(t, func(f *fakeMCP) { f.sse, f.holdStream = true, true })
		g := mcpGen(t, srv.URL, []MCPCall{addCall})
		start := time.Now()
		rs := g.Do(context.Background(), 0)
		if failedAny(rs) {
			t.Fatalf("errors: %v", errs(rs))
		}
		// Three exchanges; each would take 2s if the client waited for the stream to close.
		if d := time.Since(start); d > 1500*time.Millisecond {
			t.Errorf("took %s: the client waited on open streams", d)
		}
		for _, r := range rs {
			if r.Step == "add" && r.Latency > 500*time.Millisecond {
				t.Errorf("latency %s includes the wait for the stream to close", r.Latency)
			}
		}
	})
}

func TestMCPStreamWithoutAResponseIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"initialize"`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/x\"}\n\n") // never answers
			return
		}
	}))
	defer srv.Close()
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	rs := g.Do(context.Background(), 0)
	if len(rs) != 1 || rs[0].Success() || !strings.Contains(errText(rs[0]), "stream ended without a response") {
		t.Fatalf("results %v errors %v", steps(rs), errs(rs))
	}
}

func TestMCPUnexpectedReplies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctype   string
		body    string
		wantErr string
	}{
		{"wrong content type", "text/html", "<html>", `unexpected content-type "text/html"`},
		{"not json-rpc", "application/json", "nope", "not valid JSON-RPC"},
		{"empty", "application/json", "", "empty reply"},
		{"answers a different id", "application/json", `{"jsonrpc":"2.0","id":999,"result":{}}`, "no JSON-RPC response for request 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			g := mcpGen(t, srv.URL, []MCPCall{addCall})
			rs := g.Do(context.Background(), 0)
			if rs[0].Success() || !strings.Contains(errText(rs[0]), tc.wantErr) {
				t.Errorf("error = %q, want %q", errText(rs[0]), tc.wantErr)
			}
		})
	}
}

func TestMCPTimeoutIsAStableError(t *testing.T) {
	_, srv := newFakeMCP(t, func(f *fakeMCP) { f.slow = 2 * time.Second })
	g := mcpGen(t, srv.URL, []MCPCall{addCall}, func(_ *MCPTarget, o *MCPOptions) { o.HTTP.Timeout = 150 * time.Millisecond })
	rs := g.Do(context.Background(), 0)
	last := rs[len(rs)-1]
	if last.Success() || errText(last) != "mcp: timed out after 150ms" {
		t.Errorf("error = %q; a deadline we set must read as a stable timeout, not Go's url.Error text", errText(last))
	}
}

func TestMCPConnectionRefusedIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	g := mcpGen(t, url, []MCPCall{addCall})
	rs := g.Do(context.Background(), 0)
	if len(rs) != 1 || rs[0].Success() {
		t.Fatalf("results %v errors %v", steps(rs), errs(rs))
	}
}

func TestMCPCallCutOffByTheRunsEndIsDropped(t *testing.T) {
	_, srv := newFakeMCP(t, func(f *fakeMCP) { f.slow = 2 * time.Second })
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rs := g.Do(ctx, 0)
	for _, r := range rs {
		if r.Step == "add" {
			t.Errorf("a call cut off by the end of the run must not be reported: %+v", r)
		}
	}
}

func TestMCPMaxResponseBodyBoundsAReply(t *testing.T) {
	_, srv := newFakeMCP(t)
	g := mcpGen(t, srv.URL, []MCPCall{addCall}, func(_ *MCPTarget, o *MCPOptions) { o.HTTP.MaxResponseBody = 20 })
	rs := g.Do(context.Background(), 0)
	if rs[0].Success() {
		t.Fatalf("a reply larger than max_response_body must not be silently accepted: %+v", rs[0])
	}
	if rs[0].BytesIn > 20 {
		t.Errorf("read %d bytes with a 20 byte cap", rs[0].BytesIn)
	}
}

func TestMCPConstructionValidation(t *testing.T) {
	ok := MCPCall{Tool: "add"}
	for _, tc := range []struct {
		name    string
		target  MCPTarget
		wantErr string
	}{
		{"no calls", MCPTarget{URL: "http://x/mcp"}, "at least one call"},
		{"empty call", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{}}}, "set one of tool, list or method"},
		{"tool and list", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", List: "tools"}}}, "mutually exclusive"},
		{"tool and method", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", Method: "m"}}}, "mutually exclusive"},
		{"arguments on a list", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{List: "tools", Arguments: map[string]any{}}}}, "arguments only apply to a tool call"},
		{"params on a tool", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", Params: map[string]any{}}}}, "params only apply to a method call"},
		{"expect_tool_error on a list", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{List: "tools", ExpectToolError: true}}}, "expect_tool_error only applies to a tool call"},
		{"bad list kind", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{List: "nope"}}}, `invalid list "nope"`},
		{"reserved name initialize", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", Name: "initialize"}}}, "reserved"},
		{"reserved tool name initialized", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "initialized"}}}, "reserved"},
		{"bad extract rule", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", Extract: map[string]string{"x": "nope:y"}}}}, "extract: invalid rule"},
		{"header rule", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", ExpectResult: map[string]string{"header:X": ""}}}}, "doesn't apply to a JSON-RPC result"},
		{"status rule", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", ExpectResult: map[string]string{"status": ""}}}}, "doesn't apply to a JSON-RPC result"},
		{"css rule", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", ExpectResult: map[string]string{"css:p": ""}}}}, "doesn't apply to a JSON-RPC result"},
		{"arguments not JSON", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", Arguments: "{not json"}}}, "not valid JSON"},
		{"bad template", MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a", Arguments: map[string]any{"k": "{{.Vars"}}}}, "template"},
		{"no scheme", MCPTarget{URL: "localhost/mcp", Calls: []MCPCall{ok}}, ""},
		{"empty url", MCPTarget{Calls: []MCPCall{ok}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewMCPGenerator(tc.target, DefaultMCPOptions())
			if err == nil {
				t.Fatal("expected a construction error")
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	t.Run("HTTP/3 needs https", func(t *testing.T) {
		opts := DefaultMCPOptions()
		opts.HTTP.HTTP3 = true
		if _, err := NewMCPGenerator(MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{ok}}, opts); err == nil {
			t.Error("a plain http:// URL must be rejected with HTTP/3, before any request")
		}
	})
	t.Run("valid shapes are accepted", func(t *testing.T) {
		for _, c := range []MCPCall{
			{Tool: "a"},
			{Tool: "a", Arguments: `{"k": {{.Seq}}}`},
			{Tool: "a", Arguments: map[string]any{"k": []any{1, "x", nil, map[any]any{"deep": "{{.VU}}"}}}},
			{List: "resources"},
			{Method: "custom/thing", Params: `[1,2]`},
		} {
			if _, err := NewMCPGenerator(MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{c}}, DefaultMCPOptions()); err != nil {
				t.Errorf("%+v: %v", c, err)
			}
		}
	})
}

func TestMCPStepNamesAndProtocol(t *testing.T) {
	g, err := NewMCPGenerator(MCPTarget{URL: "http://x/mcp", Calls: []MCPCall{{Tool: "a"}, {Tool: "a"}, {List: "tools"}, {Method: "ping", Name: "hb"}}}, DefaultMCPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if got, want := strings.Join(g.StepNames(), ","), "initialize,initialized,a,tools/list,hb"; got != want {
		t.Errorf("StepNames = %s, want %s", got, want)
	}
	if g.Protocol() != "mcp" {
		t.Errorf("Protocol = %q", g.Protocol())
	}
}

// Many virtual users at once, each with its own session: run with -race.
func TestMCPConcurrentVirtualUsers(t *testing.T) {
	f, srv := newFakeMCP(t, func(f *fakeMCP) { f.sse = true })
	g := mcpGen(t, srv.URL, []MCPCall{addCall, {Tool: "echo", Arguments: map[string]any{"message": "{{.VU}}"}}})
	const vus, iters = 8, 6
	var failures atomic.Int32
	var wg sync.WaitGroup
	for vu := 0; vu < vus; vu++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if failedAny(g.Do(context.Background(), vu)) {
					failures.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Errorf("%d failed iterations", failures.Load())
	}
	if n := len(f.calls("initialize")); n != vus {
		t.Errorf("%d sessions opened for %d VUs", n, vus)
	}
	if n := len(f.calls("tools/call")); n != vus*iters*2 {
		t.Errorf("%d tool calls, want %d", n, vus*iters*2)
	}
}

func TestMCPRequestIDsIncreasePerSession(t *testing.T) {
	f, srv := newFakeMCP(t)
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	ctx := context.Background()
	g.Do(ctx, 0)
	g.Do(ctx, 0)
	var ids []string
	for _, m := range []string{"initialize", "tools/call"} {
		for _, c := range f.calls(m) {
			ids = append(ids, c.id)
		}
	}
	// initialize is 1; the two tool calls are 2 and 3 (the notification takes no id).
	if got := strings.Join(ids, ","); got != "1,2,3" {
		t.Errorf("ids = %v; initialize must be 1 and the tool calls 2 then 3, the notification none", ids)
	}
}

func TestMCPServerTextInErrorsIsBoundedAndValidUTF8(t *testing.T) {
	// The 17-byte collapsed prefix is odd, so a cut at byte 120 lands inside a
	// two-byte character; a rune-safe cut never does.
	long := strings.Repeat("é", 300)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ ID json.RawMessage }
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "error": map[string]any{"code": -1, "message": "line one\n\n  line   two" + long}})
	}))
	defer srv.Close()
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	msg := errText(g.Do(context.Background(), 0)[0])
	if !utf8.ValidString(msg) {
		t.Fatalf("the error message is not valid UTF-8: %q", msg)
	}
	if !strings.Contains(msg, "line one line two") || strings.Contains(msg, "\n") {
		t.Errorf("whitespace should be collapsed: %q", msg)
	}
	if n := len([]rune(msg)); n > 150 {
		t.Errorf("the message is %d runes; server text must be bounded", n)
	}
	if !strings.HasSuffix(msg, "...") {
		t.Errorf("a truncated message should say so: %q", msg)
	}
}

// The report's Errors table is shared, so a transport failure must never carry
// the request URL, whose query string may hold a token.
func TestMCPTransportErrorsDoNotLeakTheURL(t *testing.T) {
	const secret = "SUPERSECRET123"
	check := func(t *testing.T, name, url, want string) {
		t.Helper()
		g := mcpGen(t, url+"?token="+secret, []MCPCall{addCall})
		rs := g.Do(context.Background(), 0)
		msg := errText(rs[len(rs)-1])
		if msg == "" {
			t.Fatalf("%s: expected a failure", name)
		}
		if strings.Contains(msg, secret) || strings.Contains(msg, "token=") || strings.Contains(msg, "http://") {
			t.Errorf("%s: the error leaks the request URL: %q", name, msg)
		}
		if msg != want {
			t.Errorf("%s: error = %q, want %q", name, msg, want)
		}
	}

	t.Run("connection refused", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		check(t, "refused", url, "mcp: connection refused")
	})
	t.Run("server hangs up", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close() // no reply at all
		}))
		defer srv.Close()
		check(t, "hang up", srv.URL, "mcp: connection closed by the server")
	})
	t.Run("an error with no fixed text still drops the URL", func(t *testing.T) {
		// An untrusted certificate reaches the generic path, not one of the
		// fixed messages above.
		srv := httptest.NewTLSServer(http.NotFoundHandler())
		defer srv.Close()
		g := mcpGen(t, srv.URL+"?token="+secret, []MCPCall{addCall})
		msg := errText(g.Do(context.Background(), 0)[0])
		if !strings.HasPrefix(msg, "mcp: ") || !strings.Contains(msg, "certificate") {
			t.Fatalf("expected a certificate error, got %q", msg)
		}
		if strings.Contains(msg, secret) || strings.Contains(msg, "https://") {
			t.Errorf("the error leaks the request URL: %q", msg)
		}
	})
	t.Run("the timeout text carries no URL either", func(t *testing.T) {
		_, srv := newFakeMCP(t, func(f *fakeMCP) { f.slow = 2 * time.Second })
		g := mcpGen(t, srv.URL+"?token="+secret, []MCPCall{addCall}, func(_ *MCPTarget, o *MCPOptions) { o.HTTP.Timeout = 100 * time.Millisecond })
		rs := g.Do(context.Background(), 0)
		if msg := errText(rs[len(rs)-1]); msg != "mcp: timed out after 100ms" {
			t.Errorf("error = %q", msg)
		}
	})
}

func TestMCPOversizedReplyNamesTheCap(t *testing.T) {
	_, srv := newFakeMCP(t)
	echo := MCPCall{Tool: "echo", Arguments: map[string]any{"message": strings.Repeat("x", 4000)}}
	g := mcpGen(t, srv.URL, []MCPCall{echo}, func(_ *MCPTarget, o *MCPOptions) { o.HTTP.MaxResponseBody = 1024 })
	rs := g.Do(context.Background(), 0)
	if msg := errText(rs[len(rs)-1]); msg != "mcp: response exceeds max_response_body" {
		t.Errorf("error = %q, want the cap named", msg)
	}
	// A reply that fits exactly under a generous cap still works.
	g2 := mcpGen(t, srv.URL, []MCPCall{echo}, func(_ *MCPTarget, o *MCPOptions) { o.HTTP.MaxResponseBody = 1 << 20 })
	rs = g2.Do(context.Background(), 0)
	if msg := errText(rs[len(rs)-1]); msg != "" {
		t.Errorf("a reply under the cap failed: %q", msg)
	}
}

func TestMCPAcceptsJSONMediaTypeVariants(t *testing.T) {
	for _, ct := range []string{"application/json-rpc", "application/vnd.api+json", "application/json; charset=utf-8"} {
		_, srv := newFakeMCP(t, func(f *fakeMCP) { f.ctype = ct })
		g := mcpGen(t, srv.URL, []MCPCall{addCall})
		rs := g.Do(context.Background(), 0)
		if msg := errText(rs[len(rs)-1]); msg != "" {
			t.Errorf("content-type %q: %s", ct, msg)
		}
	}
}

func TestMCPUnrenderableArgumentsDoNotFreeSpin(t *testing.T) {
	_, srv := newFakeMCP(t)
	bad := MCPCall{Tool: "add", Arguments: `{"a": {{.Vars.missing}}, "b": 1}`}
	g := mcpGen(t, srv.URL, []MCPCall{bad})
	start := time.Now()
	rs := g.Do(context.Background(), 0)
	if errText(rs[len(rs)-1]) == "" {
		t.Fatal("expected the call to fail")
	}
	if time.Since(start) < mcpRenderFailPause {
		t.Errorf("a failure with no network I/O returned in %s; workers would free-spin", time.Since(start))
	}
	// The pause must not outlive the run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	g.Do(ctx, 1)
	if time.Since(start) > mcpRenderFailPause/2 {
		t.Errorf("the pause ignored a cancelled context")
	}
}

// Errors the generator words itself already start with "mcp: "; passing them
// through the transport error path must not stack a second prefix.
func TestMCPOwnErrorsAreNotPrefixedTwice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "this is not json")
	}))
	defer srv.Close()
	g := mcpGen(t, srv.URL, []MCPCall{addCall})
	msg := errText(g.Do(context.Background(), 0)[0])
	if !strings.HasPrefix(msg, "mcp: reply is not valid JSON-RPC") || strings.Contains(msg, "mcp: mcp:") {
		t.Errorf("error = %q", msg)
	}
}

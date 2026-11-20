package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jecklgamis/resonate/internal/cli"
	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/generator"
)

func startServer(t *testing.T, opts options) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(newServer(opts))
	t.Cleanup(ts.Close)
	return ts
}

// lockedBuf is a log sink that is safe to read while the server still writes.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

// rpc posts one JSON-RPC message and returns the response, decoded from either
// reply format.
type rpcReply struct {
	status  int
	header  http.Header
	body    string
	message map[string]any   // the JSON-RPC response
	events  []map[string]any // every message on an event stream, response last
}

func rpc(t *testing.T, srv *httptest.Server, session, method string, id any, params any) rpcReply {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := rpcReply{status: resp.StatusCode, header: resp.Header, body: string(body)}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, block := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
			var m map[string]any
			for _, line := range strings.Split(block, "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					if err := json.Unmarshal([]byte(data), &m); err != nil {
						t.Fatalf("event isn't JSON: %q", data)
					}
				}
			}
			out.events = append(out.events, m)
		}
		if len(out.events) > 0 {
			out.message = out.events[len(out.events)-1]
		}
	} else if len(body) > 0 {
		_ = json.Unmarshal(body, &out.message)
	}
	return out
}

func initSession(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	r := rpc(t, srv, "", "initialize", 1, map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	id := r.header.Get("Mcp-Session-Id")
	if r.status != 200 || id == "" {
		t.Fatalf("initialize: status %d, session %q, body %s", r.status, id, r.body)
	}
	return id
}

func result(t *testing.T, r rpcReply) map[string]any {
	t.Helper()
	res, ok := r.message["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %s", r.body)
	}
	return res
}

func rpcErrorCode(r rpcReply) float64 {
	e, _ := r.message["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

// --- wire format -----------------------------------------------------------------------

func TestInitializeOpensASessionAndNegotiatesTheVersion(t *testing.T) {
	srv := startServer(t, options{})
	r := rpc(t, srv, "", "initialize", 7, map[string]any{"protocolVersion": "2025-03-26"})
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d content-type %q", r.status, r.header.Get("Content-Type"))
	}
	res := result(t, r)
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want the client's", res["protocolVersion"])
	}
	if r.message["id"] != float64(7) {
		t.Errorf("response id = %v, want the request's", r.message["id"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	for _, k := range []string{"tools", "resources", "prompts"} {
		if _, ok := caps[k]; !ok {
			t.Errorf("capabilities lack %q: %v", k, caps)
		}
	}
	if len(r.header.Get("Mcp-Session-Id")) < 16 {
		t.Errorf("session id = %q", r.header.Get("Mcp-Session-Id"))
	}
	if other := initSession(t, srv); other == r.header.Get("Mcp-Session-Id") {
		t.Error("two initialize calls returned the same session id")
	}
	if v := result(t, rpc(t, srv, "", "initialize", 1, nil))["protocolVersion"]; v != protocolVersion {
		t.Errorf("default version = %v, want %s", v, protocolVersion)
	}
}

func TestRequestsNeedALiveSession(t *testing.T) {
	srv := startServer(t, options{})
	if r := rpc(t, srv, "", "tools/list", 1, nil); r.status != 400 || rpcErrorCode(r) != -32600 || !strings.Contains(r.body, "Mcp-Session-Id") {
		t.Errorf("no session: status %d body %s", r.status, r.body)
	}
	if r := rpc(t, srv, "nope", "tools/list", 1, nil); r.status != 404 || rpcErrorCode(r) != -32600 {
		t.Errorf("unknown session: status %d body %s", r.status, r.body)
	}
	id := initSession(t, srv)
	if r := rpc(t, srv, id, "tools/list", 1, nil); r.status != 200 {
		t.Errorf("live session: status %d", r.status)
	}
}

func TestNotificationsAreAcceptedWithoutABody(t *testing.T) {
	srv := startServer(t, options{})
	id := initSession(t, srv)
	r := rpc(t, srv, id, "notifications/initialized", nil, nil)
	if r.status != http.StatusAccepted || r.body != "" {
		t.Errorf("status %d body %q, want 202 and no body", r.status, r.body)
	}
}

func TestDeleteEndsASession(t *testing.T) {
	srv := startServer(t, options{})
	id := initSession(t, srv)
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/mcp", nil)
	req.Header.Set("Mcp-Session-Id", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE status = %d", resp.StatusCode)
	}
	if r := rpc(t, srv, id, "ping", 2, nil); r.status != 404 {
		t.Errorf("a call on a deleted session: status %d, want 404", r.status)
	}
}

func TestToolsAndTheirAnswers(t *testing.T) {
	srv := startServer(t, options{})
	id := initSession(t, srv)
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		return result(t, rpc(t, srv, id, "tools/call", 2, map[string]any{"name": name, "arguments": args}))
	}
	structured := func(res map[string]any) map[string]any {
		s, _ := res["structuredContent"].(map[string]any)
		return s
	}
	text := func(res map[string]any) string {
		c, _ := res["content"].([]any)
		first, _ := c[0].(map[string]any)
		s, _ := first["text"].(string)
		return s
	}

	if res := call("add", map[string]any{"a": 2, "b": 3}); res["isError"] != false || structured(res)["result"] != float64(5) || text(res) != "5" {
		t.Errorf("add = %v", res)
	}
	if res := call("multiply", map[string]any{"a": 4, "b": 2.5}); structured(res)["result"] != float64(10) {
		t.Errorf("multiply = %v", res)
	}
	if res := call("echo", map[string]any{"message": "hello"}); text(res) != "hello" || res["isError"] != false {
		t.Errorf("echo = %v", res)
	}
	q := structured(call("solve_quadratic", map[string]any{"a": 1, "b": -3, "c": 2}))
	if q["real_roots"] != float64(2) || q["discriminant"] != float64(1) {
		t.Errorf("solve_quadratic = %v", q)
	}
	if roots, _ := q["roots"].([]any); len(roots) != 2 || roots[0] != float64(2) || roots[1] != float64(1) {
		t.Errorf("roots = %v, want [2 1]", q["roots"])
	}
	if q := structured(call("solve_quadratic", map[string]any{"a": 1, "b": 0, "c": 1})); q["real_roots"] != float64(0) {
		t.Errorf("no real roots: %v", q)
	}
	if q := structured(call("solve_quadratic", map[string]any{"a": 1, "b": 2, "c": 1})); q["real_roots"] != float64(1) {
		t.Errorf("one root: %v", q)
	}
	start := time.Now()
	call("sleep", map[string]any{"ms": 60})
	if time.Since(start) < 50*time.Millisecond {
		t.Errorf("sleep answered after %s, want >= 60ms", time.Since(start))
	}

	// A tool that ran and failed is a normal result with isError: true.
	for name, args := range map[string]map[string]any{
		"fail":            nil,
		"add":             {"a": "one", "b": "two"},
		"echo":            {"message": 5},
		"solve_quadratic": {"a": 0, "b": 1, "c": 1},
		"sleep":           {"ms": -1},
	} {
		r := rpc(t, srv, id, "tools/call", 3, map[string]any{"name": name, "arguments": args})
		if r.status != 200 || result(t, r)["isError"] != true {
			t.Errorf("%s(%v): status %d body %s, want a 200 result with isError", name, args, r.status, r.body)
		}
	}
	// A protocol problem is a JSON-RPC error.
	if r := rpc(t, srv, id, "tools/call", 4, map[string]any{"name": "nope"}); rpcErrorCode(r) != -32602 || !strings.Contains(r.body, "unknown tool: nope") {
		t.Errorf("unknown tool: %s", r.body)
	}
	if r := rpc(t, srv, id, "tools/call", 5, map[string]any{}); rpcErrorCode(r) != -32602 {
		t.Errorf("no tool name: %s", r.body)
	}
}

func TestListingsResourcesAndPrompts(t *testing.T) {
	srv := startServer(t, options{})
	id := initSession(t, srv)
	tools, _ := result(t, rpc(t, srv, id, "tools/list", 1, nil))["tools"].([]any)
	var names []string
	for _, tl := range tools {
		m := tl.(map[string]any)
		names = append(names, m["name"].(string))
		if _, ok := m["inputSchema"].(map[string]any); !ok {
			t.Errorf("tool %v has no inputSchema", m["name"])
		}
	}
	if got, want := strings.Join(names, ","), "add,multiply,echo,solve_quadratic,sleep,fail"; got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}
	if rs, _ := result(t, rpc(t, srv, id, "resources/list", 2, nil))["resources"].([]any); len(rs) != 1 {
		t.Errorf("resources = %v", rs)
	}
	if ps, _ := result(t, rpc(t, srv, id, "prompts/list", 3, nil))["prompts"].([]any); len(ps) != 1 {
		t.Errorf("prompts = %v", ps)
	}
	read := result(t, rpc(t, srv, id, "resources/read", 4, map[string]any{"uri": "file:///demo.txt"}))
	contents, _ := read["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["text"] != "hello from the example MCP server" {
		t.Errorf("resources/read = %v", read)
	}
	if r := rpc(t, srv, id, "resources/read", 5, map[string]any{"uri": "file:///other"}); rpcErrorCode(r) != -32002 {
		t.Errorf("a missing resource: %s", r.body)
	}
	if r := rpc(t, srv, id, "ping", 6, nil); r.status != 200 || len(result(t, r)) != 0 {
		t.Errorf("ping: %s", r.body)
	}
	if r := rpc(t, srv, id, "no/such", 7, nil); rpcErrorCode(r) != -32601 {
		t.Errorf("unknown method: %s", r.body)
	}
}

func TestMalformedAndOtherRequests(t *testing.T) {
	srv := startServer(t, options{})
	for name, body := range map[string]string{
		"not json":      "nope",
		"wrong version": `{"jsonrpc":"1.0","method":"ping","id":1}`,
		"no method":     `{"jsonrpc":"2.0","id":1}`,
	} {
		resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 || !strings.Contains(string(b), "-32700") {
			t.Errorf("%s: status %d body %s, want 400 with a parse error", name, resp.StatusCode, b)
		}
	}
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST, DELETE" {
		t.Errorf("GET /mcp: status %d Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" {
		t.Errorf("/healthz = %q", b)
	}
}

func TestEventStreamReplies(t *testing.T) {
	srv := startServer(t, options{sse: true})
	id := initSession(t, srv)

	r := rpc(t, srv, id, "tools/call", 9, map[string]any{"name": "add", "arguments": map[string]any{"a": 1, "b": 1}})
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type = %q", r.header.Get("Content-Type"))
	}
	if len(r.events) != 2 || r.events[0]["method"] != "notifications/message" || r.events[0]["id"] != nil {
		t.Errorf("a tool call should send a log notification before the response: %v", r.events)
	}
	if r.message["id"] != float64(9) || result(t, r)["isError"] != false {
		t.Errorf("response = %s", r.body)
	}
	if !strings.Contains(r.body, "event: message\ndata: ") {
		t.Errorf("events should be framed as SSE messages: %q", r.body)
	}
	// Anything else is a single event.
	if r := rpc(t, srv, id, "tools/list", 10, nil); len(r.events) != 1 {
		t.Errorf("tools/list sent %d events, want 1", len(r.events))
	}
	// initialize too, with the session id still in a header.
	r = rpc(t, srv, "", "initialize", 1, nil)
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") || r.header.Get("Mcp-Session-Id") == "" || len(r.events) != 1 {
		t.Errorf("initialize over a stream: %q %q %d events", r.header.Get("Content-Type"), r.header.Get("Mcp-Session-Id"), len(r.events))
	}
	// Errors that precede a session stay plain JSON.
	if r := rpc(t, srv, "", "ping", 3, nil); r.status != 400 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Errorf("session error: status %d content-type %q", r.status, r.header.Get("Content-Type"))
	}
}

func TestSessionsExpireAfterTheTTL(t *testing.T) {
	srv := startServer(t, options{sessionTTL: 80 * time.Millisecond})
	id := initSession(t, srv)
	// Activity keeps a session alive...
	for i := 0; i < 4; i++ {
		time.Sleep(30 * time.Millisecond)
		if r := rpc(t, srv, id, "ping", i, nil); r.status != 200 {
			t.Fatalf("ping %d on a busy session: status %d", i, r.status)
		}
	}
	// ...idleness does not.
	time.Sleep(150 * time.Millisecond)
	if r := rpc(t, srv, id, "ping", 9, nil); r.status != 404 {
		t.Errorf("an idle session should have expired: status %d", r.status)
	}
	if r := rpc(t, srv, id, "ping", 10, nil); r.status != 404 {
		t.Errorf("an expired session must stay gone: status %d", r.status)
	}
}

// --- through resonate ----------------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && filepath.Base(dir) != "dsl-examples" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}

type stepSummary struct {
	Name        string  `json:"name"`
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

type summary struct {
	Requests    int           `json:"requests"`
	SuccessRate float64       `json:"success_rate"`
	Steps       []stepSummary `json:"steps"`
}

// runScenario runs a scenario through the real `resonate run`, unchanged apart
// from pointing its host at srv (the path it names is kept, so it still hits the
// endpoint it was written for) and shortening the load to a few iterations on
// one worker (so exactly one session is opened).
func runScenario(t *testing.T, sc config.Scenario, srv *httptest.Server, iterations uint64) summary {
	t.Helper()
	t.Chdir(repoRoot(t))
	u, err := url.Parse(sc.MCP.URL)
	if err != nil {
		t.Fatal(err)
	}
	sc.MCP.URL = srv.URL + u.Path
	sc.Load = config.LoadConfig{Requests: iterations, Workers: 1}
	b, err := yaml.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := cli.NewRootCommand("test")
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"run", path, "--json", "--quiet", "--html-report", "", "--json-report", "", "--results-file", ""})

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := cmd.Execute()
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("run failed: %v", runErr)
	}
	var s summary
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, out)
	}
	return s
}

func (s summary) step(name string) stepSummary {
	for _, st := range s.Steps {
		if st.Name == name {
			return st
		}
	}
	return stepSummary{}
}

// Both reply formats, because a real server uses either and resonate must read both.
var replyModes = []struct {
	name string
	opts options
}{
	{"json replies", options{}},
	{"event-stream replies", options{sse: true}},
}

func TestShippedExamplesRunAgainstTheExampleServer(t *testing.T) {
	const iterations = 4
	for _, mode := range replyModes {
		srv := startServer(t, mode.opts)
		for _, tc := range []struct {
			file  string
			calls int
			steps []string
		}{
			{"mcp-basic.yaml", 3, []string{"add", "solve_quadratic", "echo"}},
			{"mcp-explore.yaml", 5, []string{"tools/list", "resources/list", "prompts/list", "read-demo", "ping"}},
			{"mcp-chaining.yaml", 3, []string{"add", "multiply", "echo"}},
			{"mcp-errors.yaml", 3, []string{"add", "failing-tool", "bad-arguments"}},
		} {
			t.Run(mode.name+"/"+tc.file, func(t *testing.T) {
				sc, err := config.Load(filepath.Join(repoRoot(t), "examples", tc.file))
				if err != nil {
					t.Fatal(err)
				}
				s := runScenario(t, *sc, srv, iterations)
				if s.SuccessRate != 1 {
					t.Errorf("success rate = %v, want 1", s.SuccessRate)
				}
				// One session (initialize + initialized), then every call each iteration.
				if want := 2 + iterations*tc.calls; s.Requests != want {
					t.Errorf("requests = %d, want %d (2 setup + %d iterations of %d calls)", s.Requests, want, iterations, tc.calls)
				}
				for _, name := range append([]string{"initialize", "initialized"}, tc.steps...) {
					if st := s.step(name); st.Requests == 0 || st.SuccessRate != 1 {
						t.Errorf("step %q = %+v, want it present and passing", name, st)
					}
				}
			})
		}
	}
}

// docBlock is one complete `protocol: mcp` scenario found in a docs page. A block
// preceded by an invisible `<!-- docs-test: parse-only -->` comment is only
// strict-parsed, not run (it is an illustration that needs another server).
type docBlock struct {
	src       string
	parseOnly bool
}

var mcpDocFence = regexp.MustCompile("(?s)(<!-- docs-test: parse-only -->\n)?```yaml\n(.*?)```")

func mcpDocBlocks(md string) []docBlock {
	var out []docBlock
	for _, m := range mcpDocFence.FindAllStringSubmatch(md, -1) {
		if strings.HasPrefix(strings.TrimSpace(m[2]), "protocol: mcp") {
			out = append(out, docBlock{src: m[2], parseOnly: m[1] != ""})
		}
	}
	return out
}

// The MCP scenarios in the docs target this server's endpoint, so they run here
// against the endpoint they name (only the host is rewritten), in both reply modes.
func TestDocumentedMCPScenariosRunAgainstTheExampleServer(t *testing.T) {
	total, ran := 0, 0
	for _, page := range []string{"mcp.md", "scenarios.md", "usage.md"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", page))
		if err != nil {
			t.Fatal(err)
		}
		for i, blk := range mcpDocBlocks(string(b)) {
			total++
			name := fmt.Sprintf("%s scenario %d", page, i+1)
			dec := yaml.NewDecoder(strings.NewReader(blk.src))
			dec.KnownFields(true)
			var sc config.Scenario
			if err := dec.Decode(&sc); err != nil {
				t.Errorf("%s doesn't parse strictly: %v\n%s", name, err, blk.src)
				continue
			}
			if blk.parseOnly {
				continue
			}
			for _, mode := range replyModes {
				ran++
				s := runScenario(t, sc, startServer(t, mode.opts), 3)
				if s.SuccessRate != 1 || s.Requests == 0 {
					t.Errorf("%s against the example server (%s): %+v\n%s", name, mode.name, s, blk.src)
				}
			}
		}
	}
	if total < 5 || ran < 8 {
		t.Fatalf("found %d MCP scenarios in the docs and ran %d; expected at least 5 and 8", total, ran)
	}
}

// The example server reaps idle sessions; resonate must recover from the 404 by
// opening a new one, which is the behaviour the guide documents.
func TestResonateReopensASessionTheServerExpired(t *testing.T) {
	srv := startServer(t, options{sessionTTL: 60 * time.Millisecond})
	opts := generator.DefaultMCPOptions()
	opts.HTTP.Timeout = 5 * time.Second
	g, err := generator.NewMCPGenerator(generator.MCPTarget{URL: srv.URL + "/mcp", Calls: []generator.MCPCall{
		{Tool: "add", Arguments: map[string]any{"a": 1, "b": 2}, ExpectResult: map[string]string{"json:structuredContent.result": "3"}},
	}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx := context.Background()

	first := g.Do(ctx, 0)
	if len(first) != 3 || !allOK(first) {
		t.Fatalf("first iteration: %d results, ok=%v", len(first), allOK(first))
	}
	time.Sleep(150 * time.Millisecond) // idle past the TTL
	expired := g.Do(ctx, 0)
	if len(expired) != 1 || expired[0].StatusCode != 404 || expired[0].Success() {
		t.Fatalf("a call on an expired session should fail with 404: %+v", expired)
	}
	again := g.Do(ctx, 0)
	if len(again) != 3 || !allOK(again) || again[0].Step != "initialize" {
		t.Fatalf("the next iteration should open a new session: %d results, ok=%v", len(again), allOK(again))
	}
}

func allOK(rs []generator.Result) bool {
	for _, r := range rs {
		if !r.Success() {
			return false
		}
	}
	return true
}

// --- logging ---------------------------------------------------------------------------------

func TestAccessLogShowsEachRequestWhenCalledAndWhenDone(t *testing.T) {
	var buf lockedBuf
	srv := startServer(t, options{logger: log.New(&buf, "", 0), logRequests: true})
	id := initSession(t, srv)
	rpc(t, srv, id, "tools/call", 2, map[string]any{"name": "add", "arguments": map[string]any{"a": 1, "b": 2}})
	rpc(t, srv, "", "tools/list", 3, nil) // no session: a 400

	out := buf.String()
	for _, want := range []string{"call POST   /mcp", "done POST   initialize", "done POST   tools/call", "done POST   tools/list", " 400 "} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "call POST") != 3 || strings.Count(out, "done POST") != 3 {
		t.Errorf("each request should log one call and one done line:\n%s", out)
	}
}

func TestAccessLogNeverShowsCredentials(t *testing.T) {
	var buf lockedBuf
	srv := startServer(t, options{logger: log.New(&buf, "", 0), logRequests: true})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Authorization", "Bearer super-secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	out := buf.String()
	if strings.Contains(out, "super-secret-token") {
		t.Errorf("the log leaked a credential:\n%s", out)
	}
	if !strings.Contains(out, "auth=Bearer") {
		t.Errorf("the log should say credentials were sent, without echoing them:\n%s", out)
	}
}

func TestRequestLoggingCanBeTurnedOffLeavingASummary(t *testing.T) {
	var buf lockedBuf
	ts := httptest.NewServer(newServer(options{logger: log.New(&buf, "", 0), logRequests: false}))
	defer ts.Close()
	id := initSession(t, ts)
	rpc(t, ts, id, "ping", 2, nil)
	rpc(t, ts, id, "ping", 3, nil)
	rpc(t, ts, "", "ping", 4, nil) // a 400, not a failure of the server

	if buf.String() != "" {
		t.Errorf("per-request logging is off, but the log has:\n%s", buf.String())
	}
	srv := newServer(options{})
	srv.access.interval = map[string]int{"initialize": 1, "ping": 2}
	srv.access.total, srv.access.totalEr = 3, 1
	srv.access.intervalEr = 1
	line, ok := srv.access.summary(10 * time.Second)
	if !ok || !strings.Contains(line, "initialize=1 ping=2") || !strings.Contains(line, "3 requests") || !strings.Contains(line, "(1 failed)") {
		t.Errorf("summary = %q (ok=%v)", line, ok)
	}
	if _, ok := srv.access.summary(10 * time.Second); ok {
		t.Error("a summary of an empty window should report nothing")
	}
	if n, failed := srv.access.totals(); n != 3 || failed != 1 {
		t.Errorf("totals = %d, %d", n, failed)
	}
}

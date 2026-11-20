package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jecklgamis/resonate/internal/config"
)

// mcpTestServer is a minimal stateful MCP server: initialize hands out a
// session, every later request must carry it, and a few tools answer.
type mcpTestServer struct {
	*httptest.Server
	mu       sync.Mutex
	sessions map[string]bool
	methods  map[string]int // JSON-RPC method -> count
	auths    map[string]int // Authorization header value -> count
}

func newMCPTestServer(t *testing.T) *mcpTestServer {
	t.Helper()
	s := &mcpTestServer{sessions: map[string]bool{}, methods: map[string]int{}, auths: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.methods[req.Method]++
		s.auths[r.Header.Get("Authorization")]++
		s.mu.Unlock()

		reply := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		}
		if req.Method == "initialize" {
			s.mu.Lock()
			id := fmt.Sprintf("s%d", len(s.sessions)+1)
			s.sessions[id] = true
			s.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", id)
			reply(map[string]any{"protocolVersion": "2025-06-18"})
			return
		}
		s.mu.Lock()
		ok := s.sessions[r.Header.Get("Mcp-Session-Id")]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "tools/call":
			a, _ := req.Params.Arguments["a"].(float64)
			b, _ := req.Params.Arguments["b"].(float64)
			switch req.Params.Name {
			case "add":
				reply(map[string]any{"isError": false, "structuredContent": map[string]any{"result": a + b}, "content": []any{map[string]any{"type": "text", "text": "ok"}}})
			default:
				reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "boom"}}})
			}
		default:
			reply(map[string]any{"tools": []any{}})
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *mcpTestServer) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.methods[method]
}

type mcpStepJSON struct {
	Name        string  `json:"name"`
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

type mcpSummary struct {
	Requests    int           `json:"requests"`
	SuccessRate float64       `json:"success_rate"`
	Steps       []mcpStepJSON `json:"steps"`
}

func (m mcpSummary) step(name string) (mcpStepJSON, bool) {
	for _, s := range m.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return mcpStepJSON{}, false
}

func mcpRunJSON(t *testing.T, path string, extra ...string) mcpSummary {
	t.Helper()
	cmd := newRunCommand()
	cmd.SetArgs(append([]string{path, "--json", "--quiet"}, extra...))
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	var s mcpSummary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, out)
	}
	return s
}

func TestRunCommandMCPScenarioEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := newMCPTestServer(t)

	path := writeTempScenario(t, `
protocol: mcp

load:
  requests: 6
  workers: 2

mcp:
  url: `+srv.URL+`/mcp
  headers:
    Authorization: Bearer {{.Identity.token}}
  identities:
    - token: t1
    - token: t2
  calls:
    - tool: add
      arguments: {a: 2, b: 3}
      expect_result:
        json:structuredContent.result: "5"
    - list: tools
`)
	s := mcpRunJSON(t, path, "--html-report", "", "--json-report", "", "--results-file", "")
	if s.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", s.SuccessRate)
	}
	// 6 iterations of 2 calls, plus 2 workers' session setup (initialize + initialized).
	if s.Requests != 6*2+2*2 {
		t.Errorf("Requests = %d, want 16 (12 calls + 2 sessions * 2 setup requests)", s.Requests)
	}
	for name, want := range map[string]int{"initialize": 2, "initialized": 2, "add": 6, "tools/list": 6} {
		if got, ok := s.step(name); !ok || got.Requests != want || got.SuccessRate != 1 {
			t.Errorf("step %q = %+v (found %v), want %d requests at 100%%", name, got, ok, want)
		}
	}
	if srv.count("initialize") != 2 {
		t.Errorf("the server saw %d sessions, want 2 (one per worker)", srv.count("initialize"))
	}
	if srv.auths["Bearer t1"] == 0 || srv.auths["Bearer t2"] == 0 {
		t.Errorf("each worker should send its own identity: %v", srv.auths)
	}
}

func TestRunCommandMCPFailuresAreReportedPerStep(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := newMCPTestServer(t)
	path := writeTempScenario(t, `
protocol: mcp
load: {requests: 4, workers: 1}
mcp:
  url: `+srv.URL+`/mcp
  calls:
    - {tool: add, arguments: {a: 1, b: 1}}
    - {tool: explode}
    - {tool: add, name: never-runs}
`)
	s := mcpRunJSON(t, path, "--html-report", "", "--json-report", "", "--results-file", "")
	if boom, ok := s.step("explode"); !ok || boom.SuccessRate != 0 || boom.Requests != 4 {
		t.Errorf("explode step = %+v, want 4 failed requests", boom)
	}
	if _, ok := s.step("never-runs"); ok {
		t.Error("a call after a failed one must not run")
	}
	if add, _ := s.step("add"); add.SuccessRate != 1 {
		t.Errorf("add step = %+v", add)
	}
}

func TestRunCommandMCPExpectedToolErrorPasses(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := newMCPTestServer(t)
	path := writeTempScenario(t, `
protocol: mcp
load: {requests: 3, workers: 1}
mcp:
  url: `+srv.URL+`/mcp
  calls:
    - {tool: explode, expect_tool_error: true}
`)
	if s := mcpRunJSON(t, path, "--html-report", "", "--json-report", "", "--results-file", ""); s.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1: an expected tool error is a pass", s.SuccessRate)
	}
}

func TestRunCommandMCPConfigErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"missing url":       {"mcp:\n  calls: [{list: tools}]\n", "mcp.url is required"},
		"no calls":          {"mcp:\n  url: http://example.test/mcp\n", "mcp.calls needs at least one call"},
		"bad timeout":       {"mcp:\n  url: http://example.test/mcp\n  timeout: soon\n  calls: [{list: tools}]\n", "invalid mcp.timeout"},
		"relative url":      {"mcp:\n  url: /mcp\n  calls: [{list: tools}]\n", "absolute URL"},
		"tls conflict":      {"mcp:\n  url: http://example.test/mcp\n  h2c: true\n  insecure: true\n  calls: [{list: tools}]\n", "h2c"},
		"http3 needs tls":   {"mcp:\n  url: http://example.test/mcp\n  http3: true\n  calls: [{list: tools}]\n", "https"},
		"empty call":        {"mcp:\n  url: http://example.test/mcp\n  calls: [{name: x}]\n", "set one of tool, list or method"},
		"tool and list":     {"mcp:\n  url: http://example.test/mcp\n  calls: [{tool: a, list: tools}]\n", "mutually exclusive"},
		"bad list":          {"mcp:\n  url: http://example.test/mcp\n  calls: [{list: nope}]\n", "invalid list"},
		"reserved name":     {"mcp:\n  url: http://example.test/mcp\n  calls: [{tool: a, name: initialize}]\n", "reserved"},
		"bad rule":          {"mcp:\n  url: http://example.test/mcp\n  calls: [{tool: a, expect_result: {\"bogus:x\": \"1\"}}]\n", "invalid rule"},
		"arguments on list": {"mcp:\n  url: http://example.test/mcp\n  calls: [{list: tools, arguments: {a: 1}}]\n", "arguments only apply to a tool call"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := writeTempScenario(t, "protocol: mcp\nload:\n  requests: 1\n  workers: 1\n"+tc.yaml)
			cmd := newRunCommand()
			cmd.SetArgs([]string{path, "--dry-run", "--quiet"})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRunCommandUnknownProtocolListsMCP(t *testing.T) {
	t.Chdir(t.TempDir())
	path := writeTempScenario(t, "protocol: carrier-pigeon\nload:\n  requests: 1\n")
	cmd := newRunCommand()
	cmd.SetArgs([]string{path})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `"mcp"`) {
		t.Fatalf("want the unsupported-protocol error to list mcp, got %v", err)
	}
}

// Step-scoped assertions are validated before the run: they may name the
// session-setup steps and the calls, and nothing else.
func TestRunCommandMCPStepAssertionsAreValidated(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := newMCPTestServer(t)
	scenario := func(step string) string {
		return `
protocol: mcp
load: {requests: 2, workers: 1}
mcp:
  url: ` + srv.URL + `/mcp
  calls:
    - {tool: add, arguments: {a: 1, b: 1}}
assertions:
  - {metric: success_rate, step: ` + step + `, min: "1"}
`
	}
	for _, ok := range []string{"add", "initialize", "initialized"} {
		path := writeTempScenario(t, scenario(ok))
		if s := mcpRunJSON(t, path, "--html-report", "", "--json-report", "", "--results-file", ""); s.SuccessRate != 1 {
			t.Errorf("step %q: SuccessRate = %v", ok, s.SuccessRate)
		}
	}
	cmd := newRunCommand()
	cmd.SetArgs([]string{writeTempScenario(t, scenario("subtract")), "--quiet"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "subtract") {
		t.Fatalf("an assertion naming an unknown step must be rejected up front, got %v", err)
	}
	if srv.count("initialize") > 3 {
		t.Errorf("the rejected scenario must not have run (server saw %d initialize requests)", srv.count("initialize"))
	}
}

func TestRunCommandMCPDryRunSendsOneIteration(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := newMCPTestServer(t)
	path := writeTempScenario(t, `
protocol: mcp
load: {duration: 1h, workers: 1}
mcp:
  url: `+srv.URL+`/mcp
  calls:
    - {tool: add, arguments: {a: 1, b: 2}}
`)
	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--dry-run"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if srv.count("initialize") != 1 || srv.count("tools/call") != 1 {
		t.Errorf("dry run sent initialize=%d tools/call=%d, want one iteration", srv.count("initialize"), srv.count("tools/call"))
	}
	if strings.Count(out, "mcp") < 3 {
		t.Errorf("dry run should print one line per request (initialize, initialized, call):\n%s", out)
	}
}

func TestScenarioSettingsMCP(t *testing.T) {
	m := scenarioSettings("x.yaml", &config.Scenario{Protocol: "mcp", MCP: config.MCPConfig{
		URL:   "http://h/mcp?token=SECRET",
		Calls: []config.MCPCallConfig{{Tool: "a"}, {List: "tools"}},
	}})
	if got := settingValue(m, "Shape"); got != "one session per virtual user, 2 calls per iteration" {
		t.Errorf("Shape = %q", got)
	}
	if strings.Contains(settingValue(m, "Target"), "SECRET") {
		t.Errorf("the settings table must not show the URL's query string: %q", settingValue(m, "Target"))
	}
}

// A file with scenarios: can run an MCP workload next to another protocol.
func TestRunCommandMCPInAMultiScenarioFile(t *testing.T) {
	t.Chdir(t.TempDir())
	mcp := newMCPTestServer(t)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer web.Close()
	path := writeTempScenario(t, `
scenarios:
  agent:
    protocol: mcp
    load: {requests: 3, workers: 1}
    mcp:
      url: `+mcp.URL+`/mcp
      calls: [{tool: add, arguments: {a: 1, b: 1}}]
  site:
    load: {requests: 3, workers: 1}
    http:
      targets: [{url: `+web.URL+`/}]
`)
	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--json", "--quiet", "--html-report", "", "--json-report", "", "--results-file", ""})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	var s struct {
		SuccessRate float64 `json:"success_rate"`
		Scenarios   []struct {
			Name     string `json:"name"`
			Requests int    `json:"requests"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, out)
	}
	if s.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v", s.SuccessRate)
	}
	got := map[string]int{}
	for _, sc := range s.Scenarios {
		got[sc.Name] = sc.Requests
	}
	if got["agent"] != 3+2 || got["site"] != 3 {
		t.Errorf("per-scenario requests = %v, want agent 5 (3 calls + 2 setup) and site 3", got)
	}
}

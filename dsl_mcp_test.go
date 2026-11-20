package resonate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jecklgamis/resonate"
)

// ---- MCP: the fluent builder, and the facade's generator ----

type mcpRequest struct {
	Method, Session, Version, Auth string
	Params                         map[string]any
}

// mcpServer is a small stateful MCP server answering with plain JSON.
func mcpServer(t *testing.T) (*httptest.Server, func() []mcpRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []mcpRequest
	sessions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		seen = append(seen, mcpRequest{req.Method, r.Header.Get("Mcp-Session-Id"), r.Header.Get("MCP-Protocol-Version"), r.Header.Get("Authorization"), req.Params})
		mu.Unlock()

		reply := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		}
		switch {
		case req.Method == "initialize":
			mu.Lock()
			sessions++
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", sessions))
			mu.Unlock()
			reply(map[string]any{"protocolVersion": "2025-06-18"})
		case len(req.ID) == 0:
			w.WriteHeader(http.StatusAccepted)
		case req.Method == "tools/call":
			name, _ := req.Params["name"].(string)
			args, _ := req.Params["arguments"].(map[string]any)
			switch name {
			case "add":
				a, _ := args["a"].(float64)
				b, _ := args["b"].(float64)
				reply(map[string]any{"isError": false, "structuredContent": map[string]any{"result": a + b}})
			case "echo":
				reply(map[string]any{"isError": false, "structuredContent": map[string]any{"message": args["message"]}})
			default:
				reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no such tool"}}})
			}
		default:
			reply(map[string]any{"ok": req.Method})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []mcpRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]mcpRequest(nil), seen...)
	}
}

func mcpCalls(seen []mcpRequest, method string) []mcpRequest {
	var out []mcpRequest
	for _, r := range seen {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func TestMCPGeneratorRunEndToEnd(t *testing.T) {
	srv, seen := mcpServer(t)
	gen, err := resonate.NewMCPGenerator(
		resonate.MCPTarget{URL: srv.URL, Calls: []resonate.MCPCall{{Tool: "add", Arguments: map[string]any{"a": 2, "b": 3}}}},
		resonate.DefaultMCPOptions(),
	)
	if err != nil {
		t.Fatalf("NewMCPGenerator error: %v", err)
	}
	defer gen.Close()
	if gen.Protocol() != "mcp" {
		t.Errorf("Protocol = %q", gen.Protocol())
	}

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 2, Workers: 1})
	// One session (initialize + initialized), then 2 iterations of 1 call.
	if summary.Requests != 4 || summary.SuccessRate != 1 {
		t.Errorf("summary = %d requests, %v success; want 4 and 1", summary.Requests, summary.SuccessRate)
	}
	if n := len(mcpCalls(seen(), "initialize")); n != 1 {
		t.Errorf("initialize sent %d times, want once per virtual user", n)
	}
	if resonate.MCPDefaultProtocolVersion == "" {
		t.Error("MCPDefaultProtocolVersion is empty")
	}
}

func TestScenarioMCPRunsEndToEnd(t *testing.T) {
	srv, seen := mcpServer(t)
	summary, err := resonate.NewScenario().
		MCP(srv.URL).
		Header("Authorization", "Bearer secret").
		Tool("add", map[string]any{"a": 2, "b": 3}).ExpectResult("json:structuredContent.result", "5").
		MCPList("tools").
		Requests(3).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 3 iterations * 2 calls, plus the session's two setup requests.
	if summary.Requests != 8 || summary.SuccessRate != 1 {
		t.Errorf("summary = %d requests, %v success; want 8 and 1", summary.Requests, summary.SuccessRate)
	}
	got := seen()
	for _, r := range got {
		if r.Auth != "Bearer secret" {
			t.Errorf("%s was sent without the header: %+v", r.Method, r)
		}
	}
	if call := mcpCalls(got, "tools/call")[0]; call.Session != "session-1" || call.Version != "2025-06-18" {
		t.Errorf("tools/call = %+v", call)
	}
}

func TestScenarioMCPCallSettingsReachTheGenerator(t *testing.T) {
	srv, seen := mcpServer(t)
	summary, err := resonate.NewScenario().
		MCP(srv.URL).
		ProtocolVersion("2025-03-26").
		ClientInfo("perf-suite", "7").
		Tool("add", map[string]any{"a": 1, "b": 2}).Name("sum").Extract("total", "json:structuredContent.result").
		Tool("echo", `{"message": "total={{.Vars.total}} seq={{.Seq}}"}`).Name("say").ExpectResult("json:structuredContent.message", "total=3 seq=0").
		Tool("nope", nil).ExpectToolError().
		MCPMethod("resources/read", map[string]any{"uri": "file:///{{.VU}}"}).
		Requests(1).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.SuccessRate != 1 {
		t.Fatalf("success rate = %v", summary.SuccessRate)
	}
	var names []string
	for _, s := range summary.Steps {
		names = append(names, s.Name)
	}
	if got, want := strings.Join(names, ","), "initialize,initialized,sum,say,nope,resources/read"; got != want {
		t.Errorf("steps = %s, want %s", got, want)
	}
	init := mcpCalls(seen(), "initialize")[0]
	info, _ := init.Params["clientInfo"].(map[string]any)
	if init.Params["protocolVersion"] != "2025-03-26" || info["name"] != "perf-suite" || info["version"] != "7" {
		t.Errorf("initialize params = %v", init.Params)
	}
	if p := mcpCalls(seen(), "resources/read")[0].Params; p["uri"] != "file:///0" {
		t.Errorf("method params = %v", p)
	}
}

func TestScenarioMCPPerStepAssertions(t *testing.T) {
	srv, _ := mcpServer(t)
	build := func(step string, min float64) (resonate.Summary, error) {
		return resonate.NewScenario().
			MCP(srv.URL).
			Tool("add", map[string]any{"a": 1, "b": 1}).
			Tool("nope", nil).Name("failing").
			Requests(2).Workers(1).
			Assert(resonate.Assertion{Metric: "success_rate", Step: step, Min: f64(min)}).
			Run(context.Background())
	}
	if _, err := build("add", 1); err != nil {
		t.Errorf("the add step succeeded, its assertion must hold: %v", err)
	}
	if _, err := build("initialize", 1); err != nil {
		t.Errorf("the session-setup steps can be asserted on too: %v", err)
	}
	if _, err := build("failing", 1); err == nil || !strings.Contains(err.Error(), "success_rate{step=failing}") {
		t.Errorf("err = %v, want the failing step named", err)
	}
	if _, err := build("addd", 1); err == nil || !strings.Contains(err.Error(), `unknown step "addd"`) {
		t.Errorf("err = %v, want a typo'd step rejected before the run", err)
	}
}

func TestScenarioMCPMisuse(t *testing.T) {
	m := func() *resonate.Scenario { return resonate.NewScenario().MCP("http://localhost:1/mcp") }
	cases := map[string]struct {
		s    *resonate.Scenario
		want string
	}{
		"Tool outside MCP":             {resonate.NewScenario().Get("http://x/").Tool("a", nil), "only applies to an MCP scenario"},
		"MCPList outside MCP":          {resonate.NewScenario().Get("http://x/").MCPList("tools"), "only applies to an MCP scenario"},
		"MCPMethod outside MCP":        {resonate.NewScenario().Get("http://x/").MCPMethod("ping", nil), "only applies to an MCP scenario"},
		"ProtocolVersion outside MCP":  {resonate.NewScenario().Get("http://x/").ProtocolVersion("x"), "only applies to an MCP scenario"},
		"ClientInfo outside MCP":       {resonate.NewScenario().Get("http://x/").ClientInfo("a", "b"), "only applies to an MCP scenario"},
		"ExpectResult before a call":   {m().ExpectResult("json:a", ""), "call Tool, MCPList or MCPMethod first"},
		"ExpectToolError before call":  {m().ExpectToolError(), "call Tool, MCPList or MCPMethod first"},
		"Extract before a call":        {m().Extract("a", "json:a"), "call Tool, MCPList or MCPMethod first"},
		"Name before a call":           {m().Name("x"), "call Tool, MCPList or MCPMethod first"},
		"ExpectBody":                   {m().Tool("a", nil).ExpectBody("json:a", ""), "use ExpectResult"},
		"ExpectResult outside MCP":     {resonate.NewScenario().Get("http://x/").ExpectResult("json:a", ""), "only applies to an MCP scenario"},
		"Get after MCP":                {m().Get("http://x/"), "mixes"},
		"MCP then WS":                  {m().WS("ws://x/"), "mixes"},
		"MCP then GRPC":                {m().GRPC("x:1"), "mixes"},
		"SSE then MCP":                 {resonate.NewScenario().SSE("GET", "http://x/").MCP("http://x/mcp"), "mixes"},
		"no calls":                     {m(), "at least one call"},
		"both a tool and a list":       {m().Tool("a", nil).MCPList("tools").ExpectToolError(), "expect_tool_error only applies to a tool call"},
		"bad list kind":                {m().MCPList("nope"), "invalid list"},
		"reserved step name":           {m().Tool("a", nil).Name("initialize"), "reserved"},
		"arguments that are not JSON":  {m().Tool("a", "{oops"), "not valid JSON"},
		"unsupported expect_result":    {m().Tool("a", nil).ExpectResult("header:X", ""), "doesn't apply to a JSON-RPC result"},
		"HTTP3 with a plain http URL":  {m().Tool("a", nil).HTTP3(), "https"},
		"H2C with Insecure (TLS-only)": {m().Tool("a", nil).H2C().Insecure(), "h2c"},
	}
	for name, c := range cases {
		if _, err := c.s.Build(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestScenarioMCPIdentitiesFeederAndTimeoutReachTheGenerator(t *testing.T) {
	srv, seen := mcpServer(t)
	_, err := resonate.NewScenario().
		MCP(srv.URL).
		Header("Authorization", "Bearer {{.Identity.token}}").
		Identity(map[string]string{"token": "t0"}).
		Identity(map[string]string{"token": "t1"}).
		FeederRows([]map[string]string{{"who": "alice"}, {"who": "bob"}}, "sequential").
		Tool("echo", map[string]any{"message": "{{.Feeder.who}}"}).
		Timeout(2 * time.Second).
		Requests(2).Workers(2).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auths, who := map[string]bool{}, map[any]bool{}
	for _, r := range seen() {
		auths[r.Auth] = true
		if r.Method == "tools/call" {
			who[r.Params["arguments"].(map[string]any)["message"]] = true
		}
	}
	if !auths["Bearer t0"] || !auths["Bearer t1"] {
		t.Errorf("identities: server saw %v, want one per virtual user", auths)
	}
	if !who["alice"] || !who["bob"] {
		t.Errorf("feeder: tool calls carried %v, want both rows", who)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(time.Second) }))
	defer slow.Close()
	summary, _ := resonate.NewScenario().
		MCP(slow.URL).Tool("add", nil).Timeout(100 * time.Millisecond).
		Requests(1).Workers(1).
		Run(context.Background())
	if summary.SuccessRate != 0 {
		t.Errorf("a 100ms timeout against a 1s server should fail the run, success = %v", summary.SuccessRate)
	}
}

func TestScenarioMCPOverTLSNeedsInsecureOrACA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": map[string]any{"isError": false}})
	}))
	defer srv.Close()

	run := func(s *resonate.Scenario) resonate.Summary {
		sum, _ := s.Tool("add", nil).Requests(1).Workers(1).Run(context.Background())
		return sum
	}
	if sum := run(resonate.NewScenario().MCP(srv.URL).Insecure()); sum.SuccessRate != 1 {
		t.Errorf("with Insecure the run should succeed, success = %v", sum.SuccessRate)
	}
	if sum := run(resonate.NewScenario().MCP(srv.URL)); sum.SuccessRate != 0 {
		t.Errorf("an untrusted certificate must fail the run, success = %v", sum.SuccessRate)
	}
}

//go:build !windows

// Package pythonmcpserver holds a server built on the official MCP Python SDK
// and the opt-in test that points resonate at it.
//
// examples/testmcpserver is written by the same people as resonate's MCP
// support, so the two can share a wrong assumption and still pass each other's
// tests. This server is somebody else's implementation of the protocol: the
// official SDK decides how replies are framed, whether a session exists, what a
// bad argument returns. Running the shipped scenarios against it, in every
// combination of reply format and statefulness the SDK offers, is the check
// that resonate works against a server it did not write.
//
// It needs `uv` and a network on first use (uv installs the SDK into a
// throwaway environment), so it is skipped unless asked for:
//
//	make test-mcp-interop        # sets RESONATE_MCP_INTEROP=1
package pythonmcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jecklgamis/resonate/internal/cli"
	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/generator"
)

// variant is one way the SDK server can be run.
type variant struct {
	name         string
	jsonResponse bool // reply with application/json instead of an event stream
	stateless    bool // issue no Mcp-Session-Id
}

var variants = []variant{
	{"event-stream replies, stateful", false, false},
	{"json replies, stateful", true, false},
	{"event-stream replies, stateless", false, true},
	{"json replies, stateless", true, true},
}

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

// requireInterop skips the test unless the interop run was asked for and uv is
// installed, and returns uv's path. Every test calls it first, so a plain
// `go test ./...` reports a skip, never a pass.
func requireInterop(t *testing.T) string {
	t.Helper()
	if os.Getenv("RESONATE_MCP_INTEROP") == "" {
		t.Skip("set RESONATE_MCP_INTEROP=1 (or run `make test-mcp-interop`) to run this against the official Python SDK")
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("uv is not installed; it runs the Python server")
	}
	return uv
}

// startSDKServer runs server.py with the given extra flags and returns its
// base URL once it answers. The process group is killed on cleanup, because
// `uv run` starts the interpreter as a child.
func startSDKServer(t *testing.T, args ...string) string {
	t.Helper()
	uv := requireInterop(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	var out bytes.Buffer
	cmd := exec.Command(uv, append([]string{"run", filepath.Join(repoRoot(t), "examples", "pythonmcpserver", "server.py"), "--port", fmt.Sprint(port)}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(90 * time.Second) // the first run installs the SDK
	for time.Now().Before(deadline) {
		resp, err := http.Post(base+"/mcp", "application/json", strings.NewReader("{}"))
		if err == nil {
			resp.Body.Close()
			return base
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the Python server did not come up on %s:\n%s", base, out.String())
	return ""
}

func (v variant) args() []string {
	var a []string
	if v.jsonResponse {
		a = append(a, "--json-response")
	}
	if v.stateless {
		a = append(a, "--stateless")
	}
	return a
}

// rawInitialize posts an initialize request and returns the response, so the
// tests can assert what the SDK really does, rather than trusting the flags.
func rawInitialize(t *testing.T, base string) (status int, contentType, sessionID string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"interop","version":"1"}}}`
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Mcp-Session-Id")
}

type summary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
	Steps       []struct {
		Name        string  `json:"name"`
		Requests    int     `json:"requests"`
		SuccessRate float64 `json:"success_rate"`
	} `json:"steps"`
}

// runScenario runs a scenario through the real `resonate run`, pointing its URL
// at base (the path it names is kept) on one worker, so exactly one session is
// opened and the request count is exact.
func runScenario(t *testing.T, sc config.Scenario, base string, iterations uint64) summary {
	t.Helper()
	t.Chdir(repoRoot(t))
	u, err := url.Parse(sc.MCP.URL)
	if err != nil {
		t.Fatal(err)
	}
	sc.MCP.URL = base + u.Path
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

// The shipped example scenarios, unchanged, against every variant of the SDK
// server. They were written against examples/testmcpserver; passing here is the
// evidence that they, and resonate, do not depend on that server's quirks.
func TestShippedExamplesRunAgainstTheOfficialPythonSDK(t *testing.T) {
	requireInterop(t)
	const iterations = 4
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			base := startSDKServer(t, v.args()...)

			// Make sure the variant is the variant, or this proves nothing.
			status, ctype, session := rawInitialize(t, base)
			if status != 200 {
				t.Fatalf("initialize status = %d", status)
			}
			if got := strings.HasPrefix(ctype, "application/json"); got != v.jsonResponse {
				t.Fatalf("content-type %q does not match json-response=%v", ctype, v.jsonResponse)
			}
			if got := strings.HasPrefix(ctype, "text/event-stream"); got == v.jsonResponse {
				t.Fatalf("content-type %q does not match json-response=%v", ctype, v.jsonResponse)
			}
			if (session != "") == v.stateless {
				t.Fatalf("session id %q does not match stateless=%v", session, v.stateless)
			}

			for _, tc := range []struct {
				file  string
				calls int
			}{
				{"mcp-basic.yaml", 3},
				{"mcp-explore.yaml", 5},
				{"mcp-chaining.yaml", 3},
				{"mcp-errors.yaml", 3},
			} {
				t.Run(tc.file, func(t *testing.T) {
					sc, err := config.Load(filepath.Join(repoRoot(t), "examples", tc.file))
					if err != nil {
						t.Fatal(err)
					}
					s := runScenario(t, *sc, base, iterations)
					if s.SuccessRate != 1 {
						t.Errorf("success rate = %v, want 1 (%+v)", s.SuccessRate, s.Steps)
					}
					// One session (initialize + initialized, sent even to a stateless
					// server), then every call each iteration.
					if want := 2 + iterations*tc.calls; s.Requests != want {
						t.Errorf("requests = %d, want %d", s.Requests, want)
					}
				})
			}
		})
	}
}

// What the SDK does when things go wrong is the part most likely to differ from
// what resonate's own example server does, so it is pinned down here.
func TestTheSDKsErrorBehaviourIsHandledAsDocumented(t *testing.T) {
	base := startSDKServer(t, "--json-response")
	opts := generator.DefaultMCPOptions()
	opts.HTTP.Timeout = 10 * time.Second
	run := func(call generator.MCPCall) []generator.Result {
		t.Helper()
		g, err := generator.NewMCPGenerator(generator.MCPTarget{URL: base + "/mcp", Calls: []generator.MCPCall{call}}, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		return g.Do(context.Background(), 0)
	}
	last := func(rs []generator.Result) generator.Result { return rs[len(rs)-1] }
	text := func(r generator.Result) string {
		if r.Error == nil {
			return ""
		}
		return r.Error.Error()
	}

	// This SDK reports an unknown tool as a *tool* error (isError: true), not as
	// a JSON-RPC error, so it fails by default and passes with expect_tool_error.
	if r := last(run(generator.MCPCall{Tool: "no_such_tool"})); r.Success() || !strings.Contains(text(r), "mcp: tool error: Unknown tool") {
		t.Errorf("unknown tool: %+v, want a tool error", r)
	}
	if r := last(run(generator.MCPCall{Tool: "no_such_tool", ExpectToolError: true})); !r.Success() {
		t.Errorf("unknown tool with expect_tool_error should pass: %v", r.Error)
	}
	// Arguments that fail the SDK's schema validation are a tool error too, with
	// a multi-line message that must come out collapsed onto one line.
	r := last(run(generator.MCPCall{Tool: "add", Arguments: map[string]any{"a": "x", "b": "y"}}))
	if r.Success() || !strings.Contains(text(r), "mcp: tool error:") || strings.ContainsAny(text(r), "\n\r") {
		t.Errorf("bad argument types: %q", text(r))
	}
	// A tool that raises.
	if r := last(run(generator.MCPCall{Tool: "fail"})); r.Success() || !strings.Contains(text(r), "mcp: tool error:") {
		t.Errorf("fail tool: %+v", r)
	}
}

// A server that forgets idle sessions answers 404; resonate must report it once
// and open a new session on the next iteration.
func TestResonateReopensASessionTheSDKExpired(t *testing.T) {
	base := startSDKServer(t, "--json-response", "--session-idle-timeout", "1")
	opts := generator.DefaultMCPOptions()
	opts.HTTP.Timeout = 10 * time.Second
	g, err := generator.NewMCPGenerator(generator.MCPTarget{URL: base + "/mcp", Calls: []generator.MCPCall{
		{Tool: "add", Arguments: map[string]any{"a": 1, "b": 2}, ExpectResult: map[string]string{"json:structuredContent.result": "3"}},
	}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx := context.Background()

	if rs := g.Do(ctx, 0); len(rs) != 3 || !allOK(rs) {
		t.Fatalf("first iteration: %d results, ok=%v", len(rs), allOK(rs))
	}
	// The SDK reaps idle sessions on a timer, so wait well past the timeout.
	time.Sleep(4 * time.Second)
	expired := g.Do(ctx, 0)
	if len(expired) != 1 || expired[0].StatusCode != 404 || expired[0].Success() {
		t.Fatalf("a call on an expired session should fail with 404: %+v", expired)
	}
	if msg := expired[0].Error.Error(); !strings.Contains(msg, "HTTP 404") || !strings.Contains(msg, "Session not found") {
		t.Errorf("error = %q", msg)
	}
	if rs := g.Do(ctx, 0); len(rs) != 3 || !allOK(rs) || rs[0].Step != "initialize" {
		t.Fatalf("the next iteration should open a new session: %d results, ok=%v", len(rs), allOK(rs))
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

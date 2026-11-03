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

var defaults = options{eventInterval: 2 * time.Millisecond, defaultEvents: 12}

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

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
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

type summary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

// runScenario runs a scenario through the real `resonate run`, unchanged apart
// from pointing its host at srv (the path it names is kept, so it still hits the
// endpoint it was written for) and shortening the load to a few iterations.
func runScenario(t *testing.T, sc config.Scenario, srv *httptest.Server, iterations uint64) summary {
	t.Helper()
	t.Chdir(repoRoot(t))
	u, err := url.Parse(sc.SSE.URL)
	if err != nil {
		t.Fatal(err)
	}
	sc.SSE.URL = srv.URL + u.Path
	sc.Load = config.LoadConfig{Requests: iterations, Workers: 2}
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

func TestShippedExamplesRunAgainstTheExampleServer(t *testing.T) {
	srv := startServer(t, defaults)
	const iterations = 4
	for _, tc := range []struct {
		file    string
		perIter int
		why     string
	}{
		// events: 10 -> a connect plus 10 events (the server would send 12).
		{"sse-basic.yaml", 1 + 10, "connect + 10 events"},
		// "hello from vu N in iteration M" is 7 words, then [DONE].
		{"sse-post.yaml", 1 + 7 + 1, "connect + 7 tokens + [DONE]"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			sc, err := config.Load(filepath.Join(repoRoot(t), "examples", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			s := runScenario(t, *sc, srv, iterations)
			if s.SuccessRate != 1 {
				t.Errorf("success rate = %v, want 1", s.SuccessRate)
			}
			if want := iterations * tc.perIter; s.Requests != want {
				t.Errorf("requests = %d, want %d (%s per iteration)", s.Requests, want, tc.why)
			}
		})
	}
}

// docBlock is one complete `protocol: sse` scenario found in a docs page. A block
// preceded by an invisible `<!-- docs-test: parse-only -->` comment is only
// strict-parsed, not run (it is a long-running illustration).
type docBlock struct {
	src       string
	parseOnly bool
}

var sseDocFence = regexp.MustCompile("(?s)(<!-- docs-test: parse-only -->\n)?```yaml\n(.*?)```")

func sseDocBlocks(md string) []docBlock {
	var out []docBlock
	for _, m := range sseDocFence.FindAllStringSubmatch(md, -1) {
		if strings.HasPrefix(strings.TrimSpace(m[2]), "protocol: sse") {
			out = append(out, docBlock{src: m[2], parseOnly: m[1] != ""})
		}
	}
	return out
}

// The SSE scenarios in the docs target this server's endpoints, so they run here
// against the endpoints they name (only the host is rewritten).
func TestDocumentedSSEScenariosRunAgainstTheExampleServer(t *testing.T) {
	srv := startServer(t, defaults)
	total, ran := 0, 0
	for _, page := range []string{"sse.md", "scenarios.md"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", page))
		if err != nil {
			t.Fatal(err)
		}
		for i, blk := range sseDocBlocks(string(b)) {
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
			ran++
			s := runScenario(t, sc, srv, 3)
			if s.SuccessRate != 1 || s.Requests == 0 {
				t.Errorf("%s against the example server: %+v\n%s", name, s, blk.src)
			}
		}
	}
	if total < 6 || ran < 5 {
		t.Fatalf("found %d SSE scenarios in the docs and ran %d; expected at least 6 and 5", total, ran)
	}
}

func TestEventsEndpointWireFormat(t *testing.T) {
	srv := startServer(t, defaults)
	resp, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body) // the server closes the stream after 12 events
	text := string(body)

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := strings.Count(text, "data: {\"n\":"); got != 12 {
		t.Errorf("got %d events, want 12:\n%s", got, text)
	}
	if got := strings.Count(text, ": keep-alive"); got != 3 { // before events 0, 4 and 8
		t.Errorf("got %d keep-alive comments, want 3", got)
	}
	for _, want := range []string{"id: 0\nevent: tick\ndata: {\"n\":0}\n\n", "id: 11\nevent: tick\ndata: {\"n\":11}\n\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}

	// ?n and ?interval override the defaults.
	resp2, err := http.Get(srv.URL + "/events?n=3&interval=1ms")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	if got := strings.Count(string(b2), "data: {\"n\":"); got != 3 {
		t.Errorf("?n=3 gave %d events", got)
	}
}

func TestStreamEndpointIsAnLLMStyleStream(t *testing.T) {
	srv := startServer(t, defaults)
	resp, err := http.Post(srv.URL+"/v1/stream", "application/json", strings.NewReader(`{"prompt":"alpha beta gamma"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	want := "data: {\"token\":\"alpha\"}\n\ndata: {\"token\":\"beta\"}\n\ndata: {\"token\":\"gamma\"}\n\ndata: [DONE]\n\n"
	if string(b) != want {
		t.Errorf("stream = %q, want %q", b, want)
	}

	for name, body := range map[string]string{"empty prompt": `{"prompt":"  "}`, "not json": `nope`, "no prompt": `{}`} {
		r, err := http.Post(srv.URL+"/v1/stream", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		eb, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 400 || !strings.Contains(r.Header.Get("Content-Type"), "application/json") || !strings.Contains(string(eb), "prompt") {
			t.Errorf("%s: got %d %q %q, want a 400 JSON error mentioning the prompt", name, r.StatusCode, r.Header.Get("Content-Type"), eb)
		}
	}

	if r, _ := http.Get(srv.URL + "/v1/stream"); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/stream = %d, want 405", r.StatusCode)
	}
}

func TestOtherEndpoints(t *testing.T) {
	srv := startServer(t, defaults)
	get := func(path string) (*http.Response, string) {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}

	if r, b := get("/healthz"); r.StatusCode != 200 || b != "ok\n" {
		t.Errorf("/healthz = %d %q", r.StatusCode, b)
	}
	if r, _ := get("/json"); r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("/json = %d %q, want a 200 that is not an event stream", r.StatusCode, r.Header.Get("Content-Type"))
	}
	for path, want := range map[string]int{"/error": 503, "/error?code=404": 404, "/error?code=200": 503, "/error?code=junk": 503} {
		if r, _ := get(path); r.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, r.StatusCode, want)
		}
	}
	if r, _ := get("/nope"); r.StatusCode != 404 {
		t.Errorf("unknown path = %d, want 404", r.StatusCode)
	}

	// /endless really doesn't end: read a few events, then leave.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/endless", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body) // returns when our 150ms context expires, not when the server stops
	if got := strings.Count(string(b), "data: "); got < 5 {
		t.Errorf("/endless sent only %d events in 150ms", got)
	}
}

// What each endpoint looks like through resonate: the behaviours the docs
// describe (the timeout rule, the content-type check, status failures).
func TestEndpointsThroughResonate(t *testing.T) {
	srv := startServer(t, defaults)
	run := func(target generator.SSETarget, timeout time.Duration) []generator.Result {
		opts := generator.DefaultSSEOptions()
		opts.HTTP.Timeout = timeout
		g, err := generator.NewSSEGenerator(target, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		return g.Do(context.Background(), 0)
	}
	lastErr := func(rs []generator.Result) string {
		if len(rs) == 0 || rs[len(rs)-1].Success() {
			t.Fatalf("want the last result to be a failure, got %+v", rs)
		}
		return rs[len(rs)-1].Error.Error()
	}

	// Comments aren't events: 12 events (not 15 blocks), plus the connect.
	rs := run(generator.SSETarget{URL: srv.URL + "/events"}, 3*time.Second)
	if len(rs) != 13 {
		t.Errorf("/events through resonate: %d results, want 1 connect + 12 events", len(rs))
	}

	if msg := lastErr(run(generator.SSETarget{URL: srv.URL + "/endless"}, 150*time.Millisecond)); !strings.Contains(msg, "set sse duration or events") {
		t.Errorf("/endless: %s", msg)
	}
	if msg := lastErr(run(generator.SSETarget{URL: srv.URL + "/json", Events: 1}, time.Second)); !strings.Contains(msg, "content-type") {
		t.Errorf("/json: %s", msg)
	}
	rs = run(generator.SSETarget{URL: srv.URL + "/error", Events: 1}, time.Second)
	if len(rs) != 1 || rs[0].Success() || rs[0].StatusCode != 503 {
		t.Errorf("/error: want one failed 503 result, got %+v", rs)
	}
	rs = run(generator.SSETarget{URL: srv.URL + "/v1/stream", Method: "POST", Body: `{}`}, time.Second)
	if len(rs) != 1 || rs[0].Success() || rs[0].StatusCode != 400 {
		t.Errorf("/v1/stream with no prompt: want one failed 400 result, got %+v", rs)
	}
}

func TestAccessLogShowsEachRequestWhenCalledAndWhenDone(t *testing.T) {
	sink := &lockedBuf{}
	opts := defaults
	opts.logger = log.New(sink, "", 0)
	opts.logRequests = true
	srv := startServer(t, opts)

	req, _ := http.NewRequest("GET", srv.URL+"/events?n=5", nil)
	req.Header.Set("Authorization", "Bearer s3cret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	http.Get(srv.URL + "/error")
	waitFor(t, "both requests to finish", func() bool { return strings.Count(sink.String(), "done ") == 2 })

	out := sink.String()
	call, done := strings.Index(out, "call GET   /events?n=5"), strings.Index(out, "done GET   /events?n=5")
	if call < 0 || done < 0 || call > done {
		t.Errorf("want a call line before a done line for /events:\n%s", out)
	}
	for _, want := range []string{
		"auth=Bearer", "events=5", "200", "from=127.0.0.1:",
		"events: stream opened, 5 events", "events: sent all 5 events",
		"done GET   /error", "503",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cret-token") {
		t.Errorf("the log must not echo credentials:\n%s", out)
	}
}

func TestClientLeavingAnEndlessStreamIsLogged(t *testing.T) {
	sink := &lockedBuf{}
	opts := defaults
	opts.logger = log.New(sink, "", 0)
	opts.logRequests = true
	srv := startServer(t, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/endless", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	waitFor(t, "the server to notice the client left", func() bool { return strings.Contains(sink.String(), "endless: client left after") })
}

func TestRequestLoggingCanBeTurnedOffLeavingASummary(t *testing.T) {
	sink := &lockedBuf{}
	opts := defaults
	opts.logger = log.New(sink, "", 0)
	opts.logRequests = false
	srv := newServer(opts)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, path := range []string{"/events?n=2", "/events?n=2", "/error", "/error?code=404"} {
		r, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	waitFor(t, "all requests to be counted", func() bool { n, _ := srv.access.totals(); return n == 4 })

	if sink.String() != "" {
		t.Errorf("no per-request lines expected with logging off, got:\n%s", sink.String())
	}
	line, ok := srv.access.summary(10 * time.Second)
	// Two 503s are server failures; the 404 is the client's doing and doesn't count.
	if !ok || !strings.Contains(line, "/events=2") || !strings.Contains(line, "/error=2") || !strings.Contains(line, "(1 failed)") {
		t.Fatalf("summary = %q, %v", line, ok)
	}
	if _, ok := srv.access.summary(10 * time.Second); ok {
		t.Fatal("an idle window must produce no summary line")
	}
}

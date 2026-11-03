package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jecklgamis/resonate/internal/config"
)

// sseTestServer streams `events` events from any path, then closes the stream,
// recording each request's method, Authorization header and body.
type sseTestServer struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []sseSeen
	count int
}

type sseSeen struct{ method, auth, body string }

func newSSETestServer(t *testing.T, events int) *sseTestServer {
	t.Helper()
	s := &sseTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, sseSeen{r.Method, r.Header.Get("Authorization"), string(b)})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < events; i++ {
			fmt.Fprintf(w, "data: event %d\n\n", i)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sseTestServer) requests() []sseSeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sseSeen(nil), s.seen...)
}

type sseSummary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

// sseRunJSON runs `resonate run <path> --json` in-process and decodes the summary.
func sseRunJSON(t *testing.T, path string) sseSummary {
	t.Helper()
	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--json"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	var s sseSummary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	return s
}

func TestRunCommandSSEScenarioEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := newSSETestServer(t, 3)

	path := writeTempScenario(t, `
protocol: sse

load:
  requests: 2
  workers: 1

sse:
  url: `+srv.URL+`
  events: 3
`)
	s := sseRunJSON(t, path)
	// 2 iterations * (1 connect + 3 events).
	if s.Requests != 8 {
		t.Errorf("Requests = %d, want 8 (2 iterations * (connect + 3 events))", s.Requests)
	}
	if s.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", s.SuccessRate)
	}
}

func TestRunCommandSSEPostBodyAndDuration(t *testing.T) {
	t.Chdir(t.TempDir())

	var mu sync.Mutex
	var method, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		method, body = r.Method, string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stream stays open; the duration ends it
	}))
	defer srv.Close()

	path := writeTempScenario(t, `
protocol: sse

load:
  requests: 1
  workers: 1

sse:
  url: `+srv.URL+`
  method: POST
  body: '{"q":"vu {{.VU}}"}'
  duration: 100ms
`)
	s := sseRunJSON(t, path)
	if s.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1 (duration elapsing is a normal end)", s.SuccessRate)
	}
	mu.Lock()
	defer mu.Unlock()
	if method != "POST" || body != `{"q":"vu 0"}` {
		t.Errorf("server saw %s %q", method, body)
	}
}

func TestRunCommandSSEConfigErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"missing url":     {"sse:\n  events: 1\n", "sse.url is required"},
		"bad duration":    {"sse:\n  url: http://example.test\n  duration: soon\n", "invalid sse.duration"},
		"bad timeout":     {"sse:\n  url: http://example.test\n  timeout: soon\n", "invalid sse.timeout"},
		"negative events": {"sse:\n  url: http://example.test\n  events: -1\n", "events must be >= 0"},
		"relative url":    {"sse:\n  url: /events\n", "absolute URL"},
		"bad raw body":    {"sse:\n  url: http://example.test\n  raw_body_file: /nonexistent\n", "raw_body_file"},
		"tls conflict":    {"sse:\n  url: http://example.test\n  h2c: true\n  insecure: true\n", "h2c"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := writeTempScenario(t, "protocol: sse\nload:\n  requests: 1\n  workers: 1\n"+tc.yaml)
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

func TestRunCommandUnknownProtocolListsSSE(t *testing.T) {
	t.Chdir(t.TempDir())
	path := writeTempScenario(t, "protocol: carrier-pigeon\nload:\n  requests: 1\n")
	cmd := newRunCommand()
	cmd.SetArgs([]string{path})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `"sse"`) {
		t.Fatalf("want the unsupported-protocol error to list sse, got %v", err)
	}
}

// runAgainst rewrites a scenario's target to srv, shrinks its load to a few
// iterations, and runs it through the real `resonate run`.
func runSSEScenarioAgainst(t *testing.T, sc config.Scenario, srv *sseTestServer, iterations uint64) sseSummary {
	t.Helper()
	t.Chdir(t.TempDir())
	sc.SSE.URL = srv.URL + "/events"
	sc.Load = config.LoadConfig{Requests: iterations, Workers: 2}
	b, err := yaml.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return sseRunJSON(t, path)
}

// examples/sse-basic.yaml must keep working: it runs, unchanged apart from the
// target URL and a shortened load, and its identities and headers reach the server.
func TestShippedSSEExampleRuns(t *testing.T) {
	sc, err := config.Load("../../examples/sse-basic.yaml")
	if err != nil {
		t.Fatal(err)
	}
	srv := newSSETestServer(t, 10) // the example reads 10 events
	s := runSSEScenarioAgainst(t, *sc, srv, 4)
	if want := 4 * (1 + 10); s.Requests != want || s.SuccessRate != 1 {
		t.Fatalf("summary = %+v, want %d requests all succeeding", s, want)
	}
	for _, r := range srv.requests() {
		if r.auth != "Bearer abc123" && r.auth != "Bearer def456" {
			t.Errorf("Authorization = %q, want a Bearer token from the example's identities", r.auth)
		}
	}
}

// The SSE scenarios in the docs are copied and run by people, so they get the
// same treatment as the gRPC ones: a misspelled key (which resonate itself
// would silently ignore) fails strictly, and every one has to run.
func TestDocumentedSSEScenariosAreValidAndRun(t *testing.T) {
	b, err := os.ReadFile("../../docs/scenarios.md")
	if err != nil {
		t.Fatal(err)
	}
	fence := regexp.MustCompile("(?s)```yaml\n(.*?)```")
	n := 0
	for _, m := range fence.FindAllStringSubmatch(string(b), -1) {
		if !strings.HasPrefix(strings.TrimSpace(m[1]), "protocol: sse") {
			continue
		}
		n++
		dec := yaml.NewDecoder(strings.NewReader(m[1]))
		dec.KnownFields(true)
		var sc config.Scenario
		if err := dec.Decode(&sc); err != nil {
			t.Errorf("SSE scenario %d in docs/scenarios.md doesn't parse strictly: %v\n%s", n, err, m[1])
			continue
		}
		s := runSSEScenarioAgainst(t, sc, newSSETestServer(t, 12), 3)
		if s.SuccessRate != 1 || s.Requests == 0 {
			t.Errorf("SSE scenario %d in docs/scenarios.md: %+v, want every request to succeed\n%s", n, s, m[1])
		}
	}
	if n < 2 {
		t.Fatalf("found %d SSE scenarios in docs/scenarios.md; expected at least 2", n)
	}
}

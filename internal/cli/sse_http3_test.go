package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/quic-go/quic-go/http3"
)

func sseEventServer(t *testing.T, events int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < events; i++ {
			io.WriteString(w, fmt.Sprintf("data: event %d\n\n", i))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// h3Server serves handler over HTTP/3 using httptest's built-in localhost
// certificate (clients need --insecure / insecure: true), returning its
// https://host:port base URL.
func h3Server(t *testing.T, handler http.Handler) string {
	t.Helper()

	tls := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tls.Close)
	cfg := tls.TLS.Clone()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http3.Server{Handler: handler, TLSConfig: http3.ConfigureTLSConfig(cfg)}
	go srv.Serve(pc)
	t.Cleanup(func() {
		srv.Close()
		pc.Close()
	})
	return "https://" + pc.LocalAddr().String()
}

type runSummary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

func runJSON(t *testing.T, args ...string) runSummary {
	t.Helper()
	cmd := newRunCommand()
	cmd.SetArgs(append(args, "--json"))
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	var s runSummary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	return s
}

func TestRunCommandSSEScenarioEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := sseEventServer(t, 3)

	path := writeTempScenario(t, `
protocol: sse

load:
  requests: 2
  workers: 1

sse:
  url: `+srv.URL+`
  events: 3
`)
	s := runJSON(t, path)
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
	s := runJSON(t, path)
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
		"http3 needs tls": {"sse:\n  url: http://example.test\n  http3: true\n", "https"},
		"http3 plus h2c":  {"sse:\n  url: https://example.test\n  http3: true\n  h2c: true\n", "can't be combined"},
		"negative events": {"sse:\n  url: http://example.test\n  events: -1\n", "events must be >= 0"},
		"bad raw body":    {"sse:\n  url: http://example.test\n  body: a\n  raw_body_file: /nonexistent\n", "raw_body_file"},
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

func TestRunCommandHTTP3Scenario(t *testing.T) {
	t.Chdir(t.TempDir())

	var mu sync.Mutex
	var protos []string
	base := h3Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.Proto)
		mu.Unlock()
	}))

	path := writeTempScenario(t, `
load:
  requests: 3
  workers: 1

http:
  http3: true
  insecure: true
  timeout: 5s
  targets:
    - url: `+base+`/ping
`)
	s := runJSON(t, path)
	if s.Requests != 3 || s.SuccessRate != 1 {
		t.Fatalf("summary = %+v, want 3 successful requests", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(protos) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(protos))
	}
	for _, p := range protos {
		if p != "HTTP/3.0" {
			t.Fatalf("server saw %q, want HTTP/3.0", p)
		}
	}
}

func TestRunCommandHTTP3RejectsPlainHTTPURL(t *testing.T) {
	t.Chdir(t.TempDir())
	path := writeTempScenario(t, `
load:
  requests: 1
  workers: 1
http:
  http3: true
  targets:
    - url: http://example.test/ping
`)
	cmd := newRunCommand()
	cmd.SetArgs([]string{path})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want an https-required error, got %v", err)
	}
}

func TestHitCommandHTTP3Flag(t *testing.T) {
	t.Chdir(t.TempDir())

	var mu sync.Mutex
	var protos []string
	base := h3Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.Proto)
		mu.Unlock()
	}))

	cmd := newHitCommand()
	cmd.SetArgs([]string{base, "--http3", "--insecure", "--requests", "4", "--workers", "1", "--json", "--quiet"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	var s runSummary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	if s.Requests != 4 || s.SuccessRate != 1 {
		t.Fatalf("summary = %+v, want 4 successful requests", s)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range protos {
		if p != "HTTP/3.0" {
			t.Fatalf("server saw %q, want HTTP/3.0", p)
		}
	}
}

func TestHitCommandHTTP3Validation(t *testing.T) {
	cases := map[string][]string{
		"plain http url":    {"http://example.test/", "--http3"},
		"with h2c":          {"https://example.test/", "--http3", "--h2c"},
		"with no-keepalive": {"https://example.test/", "--http3", "--no-keepalive"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			cmd := newHitCommand()
			cmd.SetArgs(args)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

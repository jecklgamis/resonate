package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/quic-go/quic-go/http3"
)

// h3Server serves handler over HTTP/3 using httptest's built-in localhost
// certificate (clients need --insecure / insecure: true), returning its
// https://host:port base URL.
func h3Server(t *testing.T, handler http.Handler) string {
	t.Helper()
	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsSrv.Close)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http3.Server{Handler: handler, TLSConfig: http3.ConfigureTLSConfig(tlsSrv.TLS.Clone())}
	go srv.Serve(pc)
	t.Cleanup(func() {
		srv.Close()
		pc.Close()
	})
	return "https://" + pc.LocalAddr().String()
}

// h3Seen records the protocol version each request arrived over.
type h3Seen struct {
	mu     sync.Mutex
	protos []string
}

func (s *h3Seen) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.protos = append(s.protos, r.Proto)
	s.mu.Unlock()
}

func (s *h3Seen) mustAllBeHTTP3(t *testing.T, want int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.protos) != want {
		t.Fatalf("server saw %d requests, want %d", len(s.protos), want)
	}
	for _, p := range s.protos {
		if p != "HTTP/3.0" {
			t.Fatalf("server saw %q, want HTTP/3.0", p)
		}
	}
}

type h3Summary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

func h3RunJSON(t *testing.T, args ...string) h3Summary {
	t.Helper()
	var cmd interface {
		SetArgs([]string)
		Execute() error
	}
	if args[0] == "hit" {
		c := newHitCommand()
		cmd = c
		args = args[1:]
	} else {
		cmd = newRunCommand()
	}
	cmd.SetArgs(append(args, "--json", "--quiet"))
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	var s h3Summary
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	return s
}

// --- CLI: resonate hit --http3 ---

func TestHitCommandHTTP3Flag(t *testing.T) {
	t.Chdir(t.TempDir())
	seen := &h3Seen{}
	base := h3Server(t, seen)

	s := h3RunJSON(t, "hit", base+"/ping", "--http3", "--insecure", "--requests", "4", "--workers", "1",
		"--html-report", "", "--json-report", "", "--results-file", "")
	if s.Requests != 4 || s.SuccessRate != 1 {
		t.Fatalf("summary = %+v, want 4 successful requests", s)
	}
	seen.mustAllBeHTTP3(t, 4)
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
				t.Fatal("expected a validation error before any request is sent")
			}
		})
	}
}

// --- scenario files: http.http3 and sse.http3 ---

func TestRunCommandHTTP3Scenario(t *testing.T) {
	t.Chdir(t.TempDir())
	seen := &h3Seen{}
	base := h3Server(t, seen)

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
	s := h3RunJSON(t, path, "--html-report", "", "--json-report", "", "--results-file", "")
	if s.Requests != 3 || s.SuccessRate != 1 {
		t.Fatalf("summary = %+v, want 3 successful requests", s)
	}
	seen.mustAllBeHTTP3(t, 3)
}

func TestRunCommandHTTP3FlowScenarioWithBaseURL(t *testing.T) {
	t.Chdir(t.TempDir())
	seen := &h3Seen{}
	base := h3Server(t, seen)

	path := writeTempScenario(t, `
load:
  requests: 2
  workers: 1

http:
  http3: true
  insecure: true
  base_url: `+base+`
  flow:
    - url: /login
    - url: /me
`)
	s := h3RunJSON(t, path, "--html-report", "", "--json-report", "", "--results-file", "")
	if s.Requests != 4 || s.SuccessRate != 1 { // 2 iterations * 2 steps
		t.Fatalf("summary = %+v, want 4 successful requests", s)
	}
	seen.mustAllBeHTTP3(t, 4)
}

func TestRunCommandSSEOverHTTP3(t *testing.T) {
	t.Chdir(t.TempDir())
	var mu sync.Mutex
	var protos []string
	base := h3Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.Proto)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			w.Write([]byte("data: tick\n\n"))
			w.(http.Flusher).Flush()
		}
	}))

	path := writeTempScenario(t, `
protocol: sse

load:
  requests: 2
  workers: 1

sse:
  url: `+base+`/events
  events: 3
  http3: true
  insecure: true
`)
	s := h3RunJSON(t, path, "--html-report", "", "--json-report", "", "--results-file", "")
	if s.Requests != 8 || s.SuccessRate != 1 { // 2 * (connect + 3 events)
		t.Fatalf("summary = %+v, want 8 successful requests", s)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range protos {
		if p != "HTTP/3.0" {
			t.Fatalf("server saw %q, want HTTP/3.0", p)
		}
	}
}

func TestRunCommandHTTP3ConfigErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"http: plain http url":          {"http:\n  http3: true\n  targets:\n    - url: http://example.test/ping\n", "https"},
		"http: relative url, http base": {"http:\n  http3: true\n  base_url: http://example.test\n  targets:\n    - url: /ping\n", "https"},
		"http: with h2c":                {"http:\n  http3: true\n  h2c: true\n  targets:\n    - url: https://example.test/\n", "can't be combined"},
		"http: with keepalive off":      {"http:\n  http3: true\n  disable_keepalive: true\n  targets:\n    - url: https://example.test/\n", "keepalive"},
		"http: nested flow step":        {"http:\n  http3: true\n  flow:\n    - repeat: 2\n      steps:\n        - url: http://example.test/\n", "https"},
		"sse: plain http url":           {"protocol: sse\nsse:\n  http3: true\n  url: http://example.test/stream\n", "https"},
		"sse: with h2c":                 {"protocol: sse\nsse:\n  http3: true\n  h2c: true\n  url: https://example.test/stream\n", "can't be combined"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := writeTempScenario(t, "load:\n  requests: 1\n  workers: 1\n"+tc.yaml)
			cmd := newRunCommand()
			cmd.SetArgs([]string{path, "--quiet", "--dry-run"})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q before any request is sent, got %v", tc.want, err)
			}
		})
	}
}

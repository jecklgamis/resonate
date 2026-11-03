package resonate_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/jecklgamis/resonate"
)

// startH3 serves handler over HTTP/3 (QUIC, on UDP) with httptest's built-in
// localhost certificate, so clients must skip verification (Insecure). It
// returns the https://host:port base URL.
func startH3(t *testing.T, handler http.Handler) string {
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

// protoRecorder records the HTTP protocol version the server saw on each request.
type protoRecorder struct {
	mu     sync.Mutex
	protos []string
}

func (p *protoRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.protos = append(p.protos, r.Proto)
	p.mu.Unlock()
}

// allHTTP3 asserts the server saw exactly n requests, every one over HTTP/3 —
// a client that silently fell back to TCP would otherwise pass these tests.
func (p *protoRecorder) allHTTP3(t *testing.T, n int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.protos) != n {
		t.Fatalf("server saw %d requests, want %d", len(p.protos), n)
	}
	for _, proto := range p.protos {
		if proto != "HTTP/3.0" {
			t.Fatalf("server saw %q, want HTTP/3.0", proto)
		}
	}
}

// --- library, struct API: HTTPOptions.HTTP3 ---

func TestHTTP3StructAPIRunsOverQUIC(t *testing.T) {
	rec := &protoRecorder{}
	base := startH3(t, rec)

	opts := resonate.DefaultHTTPOptions()
	opts.HTTP3 = true
	opts.Insecure = true
	opts.Timeout = 5 * time.Second
	gen, err := resonate.NewHTTPGenerator([]resonate.HTTPTarget{{URL: base + "/ping"}}, opts)
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 6, Workers: 2})
	if summary.Requests != 6 || summary.SuccessRate != 1 {
		t.Fatalf("summary = %d requests, %v success rate; want 6 and 1", summary.Requests, summary.SuccessRate)
	}
	rec.allHTTP3(t, 6)
}

func TestHTTP3StructAPIRejectsMisuseUpFront(t *testing.T) {
	for name, tc := range map[string]struct {
		opts   func(*resonate.HTTPOptions)
		target resonate.HTTPTarget
		want   string
	}{
		"plain http url":          {func(o *resonate.HTTPOptions) {}, resonate.HTTPTarget{URL: "http://localhost:8080"}, "https"},
		"relative url, http base": {func(o *resonate.HTTPOptions) { o.BaseURL = "http://localhost:8080" }, resonate.HTTPTarget{URL: "/x"}, "https"},
		"with h2c":                {func(o *resonate.HTTPOptions) { o.H2C = true }, resonate.HTTPTarget{URL: "https://localhost:8443"}, "h2c"},
		"with disable keepalive":  {func(o *resonate.HTTPOptions) { o.DisableKeepAlive = true }, resonate.HTTPTarget{URL: "https://localhost:8443"}, "keepalive"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := resonate.DefaultHTTPOptions()
			opts.HTTP3 = true
			tc.opts(&opts)
			if _, err := resonate.NewHTTPGenerator([]resonate.HTTPTarget{tc.target}, opts); err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestHTTP3ForSSEViaTheStructAPI(t *testing.T) {
	base := startH3(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			w.Write([]byte("data: " + r.Proto + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))

	opts := resonate.DefaultSSEOptions()
	opts.HTTP.HTTP3 = true
	opts.HTTP.Insecure = true
	gen, err := resonate.NewSSEGenerator(resonate.SSETarget{URL: base + "/events", Events: 3}, opts)
	if err != nil {
		t.Fatalf("NewSSEGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 2, Workers: 1})
	if summary.Requests != 8 || summary.SuccessRate != 1 { // 2 * (connect + 3 events)
		t.Fatalf("summary = %d requests, %v success rate; want 8 and 1", summary.Requests, summary.SuccessRate)
	}
}

// --- library, fluent builder: Scenario.HTTP3() ---

func TestScenarioDSLHTTP3RunsOverQUIC(t *testing.T) {
	rec := &protoRecorder{}
	base := startH3(t, rec)

	summary, err := resonate.NewScenario().
		Get(base + "/ping").
		HTTP3().
		Insecure().
		Requests(5).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.Requests != 5 || summary.SuccessRate != 1 {
		t.Fatalf("summary = %d requests, %v success rate; want 5 and 1", summary.Requests, summary.SuccessRate)
	}
	rec.allHTTP3(t, 5)
}

func TestScenarioDSLHTTP3WithFlowAndBaseURL(t *testing.T) {
	rec := &protoRecorder{}
	base := startH3(t, rec)

	_, err := resonate.NewScenario().
		BaseURL(base).
		HTTP3().
		Insecure().
		Step("GET", "/login").
		Step("GET", "/me").
		Requests(2).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	rec.allHTTP3(t, 4) // 2 iterations * 2 steps
}

func TestScenarioDSLHTTP3RejectsMisuseAtBuild(t *testing.T) {
	for name, build := range map[string]func() (resonate.Generator, error){
		"plain http url": func() (resonate.Generator, error) {
			return resonate.NewScenario().Get("http://localhost:8080/x").HTTP3().Build()
		},
		"http base url": func() (resonate.Generator, error) {
			return resonate.NewScenario().BaseURL("http://localhost:8080").Get("/x").HTTP3().Build()
		},
		"HTTP3 then a websocket": func() (resonate.Generator, error) {
			return resonate.NewScenario().HTTP3().WS("wss://localhost/socket").Message("hi").Build()
		},
		"a websocket then HTTP3": func() (resonate.Generator, error) {
			return resonate.NewScenario().WS("wss://localhost/socket").Message("hi").HTTP3().Build()
		},
	} {
		t.Run(name, func(t *testing.T) {
			g, err := build()
			if err == nil {
				g.Close()
				t.Fatal("expected Build to fail")
			}
			if !strings.Contains(err.Error(), "https") && !strings.Contains(err.Error(), "WebSocket") {
				t.Fatalf("error should say why: %v", err)
			}
		})
	}
}

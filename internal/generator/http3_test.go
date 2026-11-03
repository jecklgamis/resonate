package generator

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// startH3Server serves handler over HTTP/3 (QUIC) on a random UDP port with
// ca's server cert, returning an "https://127.0.0.1:port" base URL and a
// CA bundle file path that trusts it.
func startH3Server(t *testing.T, ca *testCA, handler http.Handler) (baseURL, caFile string) {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http3.Server{
		Handler:   handler,
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{ca.serverCert}}),
	}
	go srv.Serve(pc)
	t.Cleanup(func() {
		srv.Close()
		pc.Close()
	})

	caFile = writeTempFile(t, t.TempDir(), "ca.pem", ca.caPEM)
	return "https://" + pc.LocalAddr().String(), caFile
}

func TestHTTPGeneratorHTTP3SpeaksQUIC(t *testing.T) {
	ca := newTestCA(t)

	var mu sync.Mutex
	var protos []string
	base, caFile := startH3Server(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protos = append(protos, r.Proto)
		mu.Unlock()
		w.Write([]byte("hello over h3"))
	}))

	g, err := NewHTTPGenerator([]HTTPTarget{{URL: base + "/x"}}, HTTPOptions{Timeout: 5 * time.Second, HTTP3: true, CAFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	for i := 0; i < 3; i++ {
		res := g.Do(context.Background(), 0)
		if len(res) != 1 || !res[0].Success() {
			t.Fatalf("iteration %d: want one successful result, got %+v", i, res)
		}
		if res[0].BytesIn != int64(len("hello over h3")) {
			t.Fatalf("iteration %d: BytesIn = %d", i, res[0].BytesIn)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(protos) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(protos))
	}
	for _, p := range protos {
		if p != "HTTP/3.0" {
			t.Fatalf("server saw proto %q, want HTTP/3.0", p)
		}
	}
}

func TestHTTPGeneratorHTTP3InsecureSkipsVerification(t *testing.T) {
	ca := newTestCA(t)
	base, _ := startH3Server(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	// Without trust the handshake must fail...
	g, err := NewHTTPGenerator([]HTTPTarget{{URL: base}}, HTTPOptions{Timeout: 3 * time.Second, HTTP3: true})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if res := g.Do(context.Background(), 0); res[0].Success() {
		t.Fatal("expected an untrusted-certificate failure without Insecure/CAFile")
	}

	// ...and succeed once verification is skipped.
	g2, err := NewHTTPGenerator([]HTTPTarget{{URL: base}}, HTTPOptions{Timeout: 3 * time.Second, HTTP3: true, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	if res := g2.Do(context.Background(), 0); !res[0].Success() {
		t.Fatalf("expected success with Insecure, got %+v", res[0])
	}
}

func TestFlowGeneratorHTTP3(t *testing.T) {
	ca := newTestCA(t)
	base, caFile := startH3Server(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.Write([]byte(`{"token":"abc"}`))
			return
		}
		w.Header().Set("X-Proto", r.Proto)
		w.Header().Set("X-Auth", r.Header.Get("Authorization"))
	}))

	g, err := NewFlowGenerator([]FlowStep{
		{Method: "POST", URL: base + "/login", Extract: map[string]string{"tok": "json:token"}},
		{URL: base + "/me", Header: map[string]string{"Authorization": "Bearer {{.Vars.tok}}"}, ExpectHeaders: map[string]string{"X-Proto": "HTTP/3.0", "X-Auth": "Bearer abc"}},
	}, nil, FlowOptions{HTTP: HTTPOptions{Timeout: 5 * time.Second, HTTP3: true, CAFile: caFile}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(res), res)
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("step %d failed: %+v", i, r)
		}
	}
}

func TestHTTP3RejectsNonHTTPSURLAtConstruction(t *testing.T) {
	opts := HTTPOptions{HTTP3: true}

	if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://example.test/x"}}, opts); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("HTTPGenerator: want an https-required error, got %v", err)
	}

	// A relative URL resolved through BaseURL counts, too.
	withBase := opts
	withBase.BaseURL = "http://example.test"
	if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "/x"}}, withBase); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("HTTPGenerator via BaseURL: want an https-required error, got %v", err)
	}

	// Including inside a nested control-flow block, and in setup.
	flow := []FlowStep{{Repeat: 2, Steps: []FlowStep{{URL: "http://example.test/x"}}}}
	if _, err := NewFlowGenerator(flow, nil, FlowOptions{HTTP: opts}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("FlowGenerator nested step: want an https-required error, got %v", err)
	}
	if _, err := NewFlowGenerator([]FlowStep{{URL: "https://example.test/ok"}}, []FlowStep{{URL: "http://example.test/login"}}, FlowOptions{HTTP: opts}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("FlowGenerator setup step: want an https-required error, got %v", err)
	}

	// A templated URL can't be checked until render time, so it's allowed.
	if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "{{.Feeder.url}}"}}, opts); err != nil {
		t.Fatalf("templated URL should pass construction, got %v", err)
	}

	// The same URL is fine without HTTP3.
	if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://example.test/x"}}, HTTPOptions{}); err != nil {
		t.Fatalf("plain http URL without HTTP3: %v", err)
	}
}

func TestHTTP3RejectsConflictingOptions(t *testing.T) {
	for name, opts := range map[string]HTTPOptions{
		"h2c":        {HTTP3: true, H2C: true},
		"keep-alive": {HTTP3: true, DisableKeepAlive: true},
	} {
		if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "https://example.test"}}, opts); err == nil {
			t.Errorf("%s: expected a conflicting-options error", name)
		}
	}
}

func TestHTTP3GeneratorCloseReleasesTransport(t *testing.T) {
	ca := newTestCA(t)
	base, caFile := startH3Server(t, ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	g, err := NewHTTPGenerator([]HTTPTarget{{URL: base}}, HTTPOptions{Timeout: 3 * time.Second, HTTP3: true, CAFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	if res := g.Do(context.Background(), 0); !res[0].Success() {
		t.Fatalf("unexpected failure: %+v", res[0])
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	// After Close the QUIC transport is shut down, so a further request fails
	// instead of silently reusing a live connection.
	if res := g.Do(context.Background(), 0); res[0].Success() {
		t.Fatal("expected requests to fail after Close")
	}
}

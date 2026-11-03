package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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

var defaults = options{eventInterval: 2 * time.Millisecond, events: 12}

// start runs the server on a random local port; the same number is used for UDP
// (HTTP/3) and, with withTCP, TCP (HTTP/1.1 and 2). It returns the server, its
// https://127.0.0.1:port base URL and the certificate (PEM).
func start(t *testing.T, withTCP bool, opts options) (*servers, string, []byte) {
	t.Helper()
	cert, certPEM, err := selfSignedCert()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := listen("127.0.0.1:0", withTCP, cert, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.close)
	return srv, "https://" + srv.udpAddr, certPEM
}

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

// protoOf asks /proto which protocol version the request arrived over, using
// resonate's own generator so it is the client under test. http3 selects it.
func protoOf(t *testing.T, base string, http3 bool) string {
	t.Helper()
	opts := generator.DefaultHTTPOptions()
	opts.HTTP3, opts.Insecure, opts.Timeout = http3, true, 3*time.Second
	g, err := generator.NewHTTPGenerator([]generator.HTTPTarget{{URL: base + "/proto", ExpectBody: map[string]string{"json:proto": ""}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)
	if len(res) != 1 || !res[0].Success() {
		return fmt.Sprintf("FAILED: %+v", res)
	}
	// The generator checks the body; fetch it again with a plain client over the
	// same transport kind to read the value.
	return getProto(t, base, http3)
}

// getProto reads /proto's "proto" field with a bare http client (TCP) or, for
// HTTP/3, by asking resonate to assert each candidate value.
func getProto(t *testing.T, base string, http3 bool) string {
	t.Helper()
	for _, want := range []string{"HTTP/3.0", "HTTP/2.0", "HTTP/1.1"} {
		opts := generator.DefaultHTTPOptions()
		opts.HTTP3, opts.Insecure, opts.Timeout = http3, true, 3*time.Second
		g, err := generator.NewHTTPGenerator([]generator.HTTPTarget{{URL: base + "/proto", ExpectBody: map[string]string{"json:proto": want}}}, opts)
		if err != nil {
			t.Fatal(err)
		}
		res := g.Do(context.Background(), 0)
		g.Close()
		if len(res) == 1 && res[0].Success() {
			return want
		}
	}
	return "unknown"
}

func TestSameURLReportsADifferentProtocolDependingOnHTTP3(t *testing.T) {
	_, base, _ := start(t, true, defaults)
	if got := protoOf(t, base, false); got != "HTTP/1.1" {
		t.Errorf("without http3 the server saw %q, want HTTP/1.1", got)
	}
	if got := protoOf(t, base, true); got != "HTTP/3.0" {
		t.Errorf("with http3 the server saw %q, want HTTP/3.0", got)
	}
}

func TestH3OnlyServerNeedsAnHTTP3Client(t *testing.T) {
	srv, base, _ := start(t, false, defaults)
	if srv.tcpAddr != "" {
		t.Fatal("-h3-only should leave no TCP listener")
	}
	if got := protoOf(t, base, true); got != "HTTP/3.0" {
		t.Errorf("an HTTP/3 client got %q", got)
	}
	if got := protoOf(t, base, false); !strings.HasPrefix(got, "FAILED") {
		t.Errorf("a client that doesn't ask for HTTP/3 should fail to connect to an h3-only server, got %q", got)
	}
}

func TestCertificateWorksAsCAFileAndUntrustedFailsWithout(t *testing.T) {
	_, base, certPEM := start(t, false, defaults)
	caFile := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(caFile, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	check := func(mutate func(*generator.HTTPOptions)) []generator.Result {
		opts := generator.DefaultHTTPOptions()
		opts.HTTP3, opts.Timeout = true, 3*time.Second
		mutate(&opts)
		g, err := generator.NewHTTPGenerator([]generator.HTTPTarget{{URL: base + "/healthz"}}, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		return g.Do(context.Background(), 0)
	}
	if res := check(func(o *generator.HTTPOptions) { o.CAFile = caFile }); !res[0].Success() {
		t.Errorf("trusting the saved certificate via ca_file should work: %+v", res[0])
	}
	if res := check(func(o *generator.HTTPOptions) {}); res[0].Success() {
		t.Error("a self-signed certificate must be refused without insecure or ca_file")
	}
}

func TestAltSvcIsAdvertisedOverTCPOnly(t *testing.T) {
	srv, base, _ := start(t, true, defaults)
	tcp := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 3 * time.Second}
	resp, err := tcp.Get("https://" + srv.tcpAddr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, port, _ := net.SplitHostPort(srv.udpAddr)
	if got, want := resp.Header.Get("Alt-Svc"), fmt.Sprintf(`h3=":%s"; ma=3600`, port); got != want {
		t.Errorf("Alt-Svc over TCP = %q, want %q", got, want)
	}
	_ = base
}

func TestEndpoints(t *testing.T) {
	_, base, _ := start(t, false, defaults)
	run := func(target generator.HTTPTarget) generator.Result {
		opts := generator.DefaultHTTPOptions()
		opts.HTTP3, opts.Insecure, opts.Timeout = true, true, 3*time.Second
		g, err := generator.NewHTTPGenerator([]generator.HTTPTarget{target}, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		return g.Do(context.Background(), 0)[0]
	}
	if r := run(generator.HTTPTarget{URL: base + "/healthz"}); !r.Success() || r.BytesIn != 3 {
		t.Errorf("/healthz: %+v", r)
	}
	if r := run(generator.HTTPTarget{URL: base + "/proto", ExpectBody: map[string]string{"json:proto": "HTTP/3.0", "json:tls": "TLS 1.3", "json:alpn": "h3"}}); !r.Success() {
		t.Errorf("/proto should report HTTP/3.0 over TLS 1.3 with ALPN h3: %+v", r)
	}
	if r := run(generator.HTTPTarget{Method: "POST", URL: base + "/echo", Header: map[string]string{"Content-Type": "application/json"}, Body: `{"hello":"quic"}`, ExpectBody: map[string]string{"json:hello": "quic"}}); !r.Success() {
		t.Errorf("/echo: %+v", r)
	}
	if r := run(generator.HTTPTarget{URL: base + "/nope"}); r.Success() || r.StatusCode != 404 {
		t.Errorf("unknown path should be a 404: %+v", r)
	}
}

func TestEventsEndpointStreamsOverHTTP3(t *testing.T) {
	_, base, _ := start(t, false, defaults)
	opts := generator.DefaultSSEOptions()
	opts.HTTP.HTTP3, opts.HTTP.Insecure, opts.HTTP.Timeout = true, true, 3*time.Second
	g, err := generator.NewSSEGenerator(generator.SSETarget{URL: base + "/events", Events: 5}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)
	if len(res) != 6 { // connect + 5 events
		t.Fatalf("want connect + 5 events, got %d: %+v", len(res), res)
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d failed: %+v", i, r)
		}
	}
	// Each event carries the protocol it was sent over: {"n":0,"proto":"HTTP/3.0"} is 31 bytes
	// of data plus framing, so a TCP fallback couldn't produce this size.
	if res[1].BytesIn < int64(len(`data: {"n":0,"proto":"HTTP/3.0"}`)) {
		t.Errorf("first event is %d bytes; did the stream really carry proto HTTP/3.0?", res[1].BytesIn)
	}
}

func TestAccessLogShowsTheProtocolOfEachRequest(t *testing.T) {
	sink := &lockedBuf{}
	opts := defaults
	opts.logger = log.New(sink, "", 0)
	opts.logRequests = true
	srv, base, _ := start(t, true, opts)

	protoOf(t, base, true)
	protoOf(t, base, false)
	waitFor(t, "both protocols to be counted", func() bool { return srv.access.protoCount("HTTP/3.0") > 0 && srv.access.protoCount("HTTP/1.1") > 0 })

	out := sink.String()
	for _, want := range []string{"call HTTP/3.0", "done HTTP/3.0", "call HTTP/1.1", "done HTTP/1.1", "/proto", "200"} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
}

func TestRequestLoggingCanBeTurnedOffLeavingAProtocolSummary(t *testing.T) {
	sink := &lockedBuf{}
	opts := defaults
	opts.logger = log.New(sink, "", 0)
	opts.logRequests = false
	srv, base, _ := start(t, true, opts)

	getProto(t, base, true)
	waitFor(t, "a request to be counted", func() bool { n, _ := srv.access.totals(); return n > 0 })
	if sink.String() != "" {
		t.Errorf("no per-request lines expected with logging off, got:\n%s", sink.String())
	}
	line, ok := srv.access.summary(10 * time.Second)
	if !ok || !strings.Contains(line, "/proto=") || !strings.Contains(line, "HTTP/3.0=") {
		t.Fatalf("summary = %q, %v; want it to break requests down by protocol", line, ok)
	}
	if _, ok := srv.access.summary(10 * time.Second); ok {
		t.Fatal("an idle window must produce no summary line")
	}
}

// --- the shipped examples and the docs, run through the real CLI ---

type summary struct {
	Requests    int     `json:"requests"`
	SuccessRate float64 `json:"success_rate"`
}

// rehost points every URL in a scenario at base, keeping the paths, so each
// scenario still hits the endpoint it was written for.
func rehost(t *testing.T, sc *config.Scenario, base string) {
	t.Helper()
	b, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	swap := func(raw string) string {
		if raw == "" || strings.Contains(raw, "{{") || strings.HasPrefix(raw, "/") {
			return raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		u.Scheme, u.Host = b.Scheme, b.Host
		return u.String()
	}
	var steps func([]config.HTTPTargetConfig)
	steps = func(list []config.HTTPTargetConfig) {
		for i := range list {
			list[i].URL = swap(list[i].URL)
			steps(list[i].Steps)
		}
	}
	steps(sc.HTTP.Targets)
	steps(sc.HTTP.Flow)
	steps(sc.HTTP.Setup)
	sc.HTTP.BaseURL = swap(sc.HTTP.BaseURL)
	sc.SSE.URL = swap(sc.SSE.URL)
}

func runScenario(t *testing.T, sc config.Scenario, base string, iterations uint64) summary {
	t.Helper()
	t.Chdir(repoRoot(t))
	rehost(t, &sc, base)
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

func TestShippedExamplesRunOverHTTP3(t *testing.T) {
	const iterations = 4
	for _, tc := range []struct {
		file    string
		perIter int
	}{
		{"http-http3.yaml", 1},
		{"sse-http3.yaml", 1 + 10}, // a connect plus the 10 events it asks for
	} {
		t.Run(tc.file, func(t *testing.T) {
			srv, base, _ := start(t, true, defaults) // TCP is up too: only HTTP/3 should be used
			sc, err := config.Load(filepath.Join(repoRoot(t), "examples", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			s := runScenario(t, *sc, base, iterations)
			if want := iterations * tc.perIter; s.Requests != want || s.SuccessRate != 1 {
				t.Errorf("summary = %+v, want %d requests, all successful", s, want)
			}
			if got := srv.access.protoCount("HTTP/3.0"); got != iterations {
				t.Errorf("server saw %d HTTP/3.0 requests, want %d", got, iterations)
			}
			if got := srv.access.protoCount("HTTP/1.1") + srv.access.protoCount("HTTP/2.0"); got != 0 {
				t.Errorf("%d requests fell back to TCP; HTTP/3 must not fall back", got)
			}
		})
	}
}

// docBlock is a complete scenario in a docs page. A block preceded by an invisible
// `<!-- docs-test: parse-only -->` comment is strict-parsed but not run.
type docBlock struct {
	src       string
	parseOnly bool
}

var docFence = regexp.MustCompile("(?s)(<!-- docs-test: parse-only -->\\n)?```yaml\\n(.*?)```")

func docBlocks(md string) []docBlock {
	var out []docBlock
	for _, m := range docFence.FindAllStringSubmatch(md, -1) {
		body := strings.TrimSpace(m[2])
		if strings.HasPrefix(body, "protocol: http") || strings.HasPrefix(body, "protocol: sse") {
			out = append(out, docBlock{src: m[2], parseOnly: m[1] != ""})
		}
	}
	return out
}

// The scenarios in the HTTP/3 guide are copied and run by people: each is
// strict-parsed (a misspelled key, which resonate itself would ignore, fails) and
// run over HTTP/3 against this server, and none may fall back to TCP.
func TestDocumentedHTTP3ScenariosRunAgainstTheExampleServer(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "http3.md"))
	if err != nil {
		t.Fatal(err)
	}
	total, ran := 0, 0
	for i, blk := range docBlocks(string(b)) {
		total++
		name := fmt.Sprintf("http3.md scenario %d", i+1)
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
		srv, base, _ := start(t, true, defaults)
		s := runScenario(t, sc, base, 3)
		if s.SuccessRate != 1 || s.Requests == 0 {
			t.Errorf("%s: %+v, want every request to succeed\n%s", name, s, blk.src)
		}
		if srv.access.protoCount("HTTP/3.0") == 0 || srv.access.protoCount("HTTP/1.1")+srv.access.protoCount("HTTP/2.0") != 0 {
			t.Errorf("%s: must run entirely over HTTP/3 (h3=%d, tcp=%d)\n%s", name, srv.access.protoCount("HTTP/3.0"), srv.access.protoCount("HTTP/1.1")+srv.access.protoCount("HTTP/2.0"), blk.src)
		}
	}
	if total < 3 || ran < 3 {
		t.Fatalf("found %d scenarios in docs/http3.md and ran %d; expected at least 3 of each", total, ran)
	}
}

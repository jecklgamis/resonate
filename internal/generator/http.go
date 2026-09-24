package generator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"

	"github.com/jecklgamis/resonate/internal/tmpl"
)

// HTTPTarget describes a single HTTP request template. Any field may embed
// {{ }} expressions (see internal/tmpl) that are re-rendered for every
// request — e.g. url: "/users/{{randInt 1 1000}}".
type HTTPTarget struct {
	Method string
	URL    string
	Query  map[string]string
	Header map[string]string
	Body   string

	// RawBody, if non-nil, is sent as the request body exactly as given,
	// bypassing templating entirely — for a large or binary payload where
	// per-request re-rendering would be wasteful, or where the raw bytes
	// might otherwise be misread as containing "{{ }}" template syntax.
	// Mutually exclusive with Body (a construction error to set both). A
	// non-nil empty slice is a deliberate empty body, distinct from RawBody
	// being unset (nil).
	RawBody []byte

	// ExpectStatus, if non-empty, overrides the default success criterion
	// (2xx/3xx) for this target: a response whose status isn't in this list
	// is reported as a failed Result (with an explanatory Error), even if it
	// would otherwise be a 2xx/3xx success — e.g. expecting a 404 as the
	// "correct" response for a not-found check.
	ExpectStatus []int

	// ExpectHeaders, if non-empty, checks response headers (case-insensitive
	// name, like HTTP itself): a non-empty value requires an exact match, an
	// empty value only requires the header to be present at all. Combines
	// with ExpectStatus (AND) when both are set; either one alone still
	// overrides the default 2xx/3xx-only success criterion.
	ExpectHeaders map[string]string

	// ExpectBody, if non-empty, checks the response body via the same rule
	// language as Extract ("json:<path>", "xml:<path>", "header:<Name>",
	// "status") — keyed by rule, valued by the expected result: a non-empty
	// value requires an exact match, an empty value only requires the rule
	// to evaluate without error (e.g. the JSON/XML path exists). Combines
	// with ExpectStatus/ExpectHeaders (AND) when set alongside them.
	ExpectBody map[string]string
}

// HTTPOptions configures the transport shared by all requests.
type HTTPOptions struct {
	Timeout         time.Duration
	Insecure        bool // skip TLS certificate verification
	FollowRedirects bool
	MaxIdleConns    int

	// MaxResponseBody caps how many response bytes doHTTP reads before
	// discarding the rest (BytesIn reflects what was actually read, not the
	// target's full response size). 0 = unlimited, reading the whole body
	// like before — a target streaming an unbounded or huge response can
	// otherwise exhaust resonate's own memory under load.
	MaxResponseBody int64

	// CertFile/KeyFile, if both set, authenticate resonate to the target via
	// a client certificate (mTLS). Setting only one is a construction error.
	CertFile string
	KeyFile  string

	// CAFile, if set, is a PEM bundle of additional CAs to trust — a safer
	// alternative to Insecure for targets signed by a private/internal CA.
	CAFile string

	// H2C forces HTTP/2 over plaintext (prior-knowledge h2c), for targets
	// that speak HTTP/2 without TLS. Go's default Transport only negotiates
	// HTTP/2 via TLS ALPN, so a plain http:// target otherwise always falls
	// back to HTTP/1.1 even against an h2c-capable server. Mutually
	// exclusive with CertFile/KeyFile/CAFile/Insecure, which are all
	// TLS-only concerns.
	H2C bool

	// DisableKeepAlive forces a fresh TCP (+TLS) connection per request
	// instead of the default pooled/reused connections — useful for
	// simulating "cold" clients or stress-testing a target's connection
	// accept/handshake path specifically, which persistent connections
	// otherwise mask. Mutually exclusive with H2C: HTTP/2 fundamentally
	// multiplexes over one persistent connection and http2.Transport has no
	// equivalent knob, so combining the two would otherwise silently no-op
	// rather than actually disabling connection reuse.
	DisableKeepAlive bool

	// Feeder, if set, hands out one row of CSV/JSON-loaded data per
	// iteration, exposed to templates as {{.Feeder.<column>}}. Shared by
	// HTTPGenerator and (via FlowOptions.HTTP) FlowGenerator.
	Feeder *Feeder

	// BaseURL, if set, is prepended to any target/step URL that starts
	// with "/", so a scenario hitting one host doesn't
	// need to repeat "http://host:port" on every target/step. A URL that
	// doesn't start with "/" (already absolute, or a template expression
	// like "{{...}}") is left untouched. Resolution happens once, at
	// construction, before the usual literal-URL validation — so a
	// relative URL with no BaseURL set still fails fast the same way an
	// absolute URL with no scheme/host would.
	BaseURL string
}

func DefaultHTTPOptions() HTTPOptions {
	return HTTPOptions{
		Timeout:         30 * time.Second,
		FollowRedirects: true,
	}
}

func newHTTPClient(opts HTTPOptions) (*http.Client, error) {
	if opts.H2C {
		if opts.CertFile != "" || opts.KeyFile != "" || opts.CAFile != "" || opts.Insecure || opts.DisableKeepAlive {
			return nil, fmt.Errorf("h2c is plaintext HTTP/2 and can't be combined with insecure/cert/key/ca/disable-keepalive options")
		}
		// h2c requires a client that speaks HTTP/2 without ever attempting
		// TLS; http2.Transport with AllowHTTP does that, but only if
		// DialTLSContext is overridden to dial plain TCP instead of the
		// zero-value's default (which would try TLS anyway).
		transport := &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		}
		return &http.Client{Transport: transport, Timeout: opts.Timeout}, nil
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: opts.Insecure}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in ca file %q", opts.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	if opts.CertFile != "" || opts.KeyFile != "" {
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, fmt.Errorf("both a cert and a key are required for client certificate authentication")
		}
		cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	transport := &http.Transport{
		MaxIdleConnsPerHost: 100,
		TLSClientConfig:     tlsConfig,
		DisableKeepAlives:   opts.DisableKeepAlive,
	}
	if opts.MaxIdleConns > 0 {
		transport.MaxIdleConnsPerHost = opts.MaxIdleConns
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   opts.Timeout,
	}
	if !opts.FollowRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client, nil
}

// httpFields holds one request template's fields pre-parsed into tmpl.Field
// once at construction time, so per-request rendering is cheap.
type httpFields struct {
	method        string
	url           tmpl.Field
	query         map[string]tmpl.Field
	header        map[string]tmpl.Field
	body          tmpl.Field
	rawBody       []byte // non-nil overrides body entirely, sent unrendered
	expectStatus  []int
	expectHeaders map[string]string
	expectBody    map[string]string
}

func compileHTTPFields(method, rawURL string, query, header map[string]string, body string, rawBody []byte, baseURL string, expectStatus []int, expectHeaders, expectBody map[string]string) (httpFields, error) {
	if method == "" {
		method = "GET"
	}

	if body != "" && rawBody != nil {
		return httpFields{}, fmt.Errorf("body and raw body are mutually exclusive; set one or the other")
	}

	rawURL = resolveBaseURL(baseURL, rawURL)
	if err := validateLiteralURL(rawURL); err != nil {
		return httpFields{}, err
	}
	urlField, err := tmpl.Compile("url", rawURL)
	if err != nil {
		return httpFields{}, err
	}

	q := make(map[string]tmpl.Field, len(query))
	for k, v := range query {
		f, err := tmpl.Compile("query:"+k, v)
		if err != nil {
			return httpFields{}, err
		}
		q[k] = f
	}

	h := make(map[string]tmpl.Field, len(header))
	for k, v := range header {
		f, err := tmpl.Compile("header:"+k, v)
		if err != nil {
			return httpFields{}, err
		}
		h[k] = f
	}

	bodyField, err := tmpl.Compile("body", body)
	if err != nil {
		return httpFields{}, err
	}

	for _, code := range expectStatus {
		if code < 100 || code > 599 {
			return httpFields{}, fmt.Errorf("invalid expect_status %d: must be a valid HTTP status code (100-599)", code)
		}
	}
	for name := range expectHeaders {
		if strings.TrimSpace(name) == "" {
			return httpFields{}, fmt.Errorf("invalid expect_headers: header name must not be empty")
		}
	}
	for rule := range expectBody {
		if !isKnownExtractRule(rule) {
			return httpFields{}, fmt.Errorf("invalid expect_body rule %q (expected \"json:<path>\", \"yaml:<path>\", \"xml:<path>\", \"regex:<pattern>\", \"css:<selector>\", \"header:<Name>\", or \"status\")", rule)
		}
	}

	return httpFields{method: method, url: urlField, query: q, header: h, body: bodyField, rawBody: rawBody, expectStatus: expectStatus, expectHeaders: expectHeaders, expectBody: expectBody}, nil
}

// resolveBaseURL prepends base to url if url starts with "/".
// Anything else (already absolute, or a "{{...}}" template
// expression, which never starts with "/") passes through unchanged. An
// empty base is a no-op, so a "/"-prefixed url with no BaseURL configured
// falls straight through to validateLiteralURL and fails fast there, the
// same as any other malformed URL.
func resolveBaseURL(base, rawURL string) string {
	if base == "" || !strings.HasPrefix(rawURL, "/") {
		return rawURL
	}
	return strings.TrimRight(base, "/") + rawURL
}

// validateLiteralURL fails fast on an obviously-broken URL at construction
// time, before any run starts. A URL containing {{ }} can't be validated
// until it's rendered per-request — those are left for doHTTP to catch. This
// matters beyond user-friendliness: a URL that fails to parse resolves
// locally with no network I/O, so with no --rate set a bad literal URL would
// otherwise let the worker loop free-spin at millions of iterations/sec
// instead of failing immediately.
func validateLiteralURL(rawURL string) error {
	if strings.Contains(rawURL, "{{") {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid url %q: must be an absolute URL with a scheme and host, e.g. http://host/path", rawURL)
	}
	return nil
}

// doHTTP renders f against data, sends the request over client, and returns
// the Result. If extract is non-empty, the response body is buffered (rather
// than streamed to io.Discard) and each extraction rule is evaluated against
// it; successfully extracted values are returned in the second value.
// maxBody caps how many response bytes are read/counted (0 = unlimited) —
// the remainder, if any, is still drained to io.Discard so the connection
// can be reused, it just isn't counted into BytesIn or buffered for extract.
func doHTTP(ctx context.Context, client *http.Client, f httpFields, data tmpl.Data, extract map[string]string, maxBody int64) (Result, map[string]string) {
	rawURL, err := f.url.Render(data)
	if err != nil {
		return Result{Timestamp: time.Now(), Error: fmt.Errorf("rendering url: %w", err), Protocol: "http"}, nil
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return Result{Timestamp: time.Now(), Error: fmt.Errorf("parsing url %q: %w", rawURL, err), Protocol: "http"}, nil
	}
	if len(f.query) > 0 {
		q := parsedURL.Query()
		for k, qf := range f.query {
			v, err := qf.Render(data)
			if err != nil {
				return Result{Timestamp: time.Now(), Error: fmt.Errorf("rendering query %q: %w", k, err), Protocol: "http"}, nil
			}
			q.Set(k, v)
		}
		parsedURL.RawQuery = q.Encode()
	}

	var bodyReader io.Reader
	var bodyLen int
	if f.rawBody != nil {
		bodyLen = len(f.rawBody)
		if bodyLen > 0 {
			bodyReader = bytes.NewReader(f.rawBody)
		}
	} else {
		bodyStr, err := f.body.Render(data)
		if err != nil {
			return Result{Timestamp: time.Now(), Error: fmt.Errorf("rendering body: %w", err), Protocol: "http"}, nil
		}
		bodyLen = len(bodyStr)
		if bodyStr != "" {
			bodyReader = strings.NewReader(bodyStr)
		}
	}

	req, err := http.NewRequestWithContext(ctx, f.method, parsedURL.String(), bodyReader)
	if err != nil {
		return Result{Timestamp: time.Now(), Error: err, Protocol: "http"}, nil
	}
	for k, hf := range f.header {
		v, err := hf.Render(data)
		if err != nil {
			return Result{Timestamp: time.Now(), Error: fmt.Errorf("rendering header %q: %w", k, err), Protocol: "http"}, nil
		}
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{
			Timestamp: start,
			Latency:   time.Since(start),
			Error:     err,
			Protocol:  "http",
			BytesOut:  int64(bodyLen),
		}, nil
	}
	defer resp.Body.Close()

	var bodyBytes []byte
	var n int64
	body := io.Reader(resp.Body)
	if maxBody > 0 {
		body = io.LimitReader(resp.Body, maxBody)
	}
	if len(extract) > 0 || len(f.expectBody) > 0 {
		bodyBytes, _ = io.ReadAll(body)
		n = int64(len(bodyBytes))
	} else {
		n, _ = io.Copy(io.Discard, body)
	}
	if maxBody > 0 && n >= maxBody {
		// Drain and discard whatever's left so the connection can still be
		// reused, without counting it into BytesIn or extract's/expect_body's
		// buffer.
		io.Copy(io.Discard, resp.Body)
	}
	latency := time.Since(start)

	result := Result{
		Timestamp:  start,
		Latency:    latency,
		StatusCode: resp.StatusCode,
		BytesIn:    n,
		BytesOut:   int64(bodyLen),
		Protocol:   "http",
	}
	if len(f.expectStatus) > 0 || len(f.expectHeaders) > 0 || len(f.expectBody) > 0 {
		matched := true
		var reasons []string

		if len(f.expectStatus) > 0 && !slices.Contains(f.expectStatus, resp.StatusCode) {
			matched = false
			reasons = append(reasons, fmt.Sprintf("status: got %d, want one of %v", resp.StatusCode, f.expectStatus))
		}

		headerNames := make([]string, 0, len(f.expectHeaders))
		for name := range f.expectHeaders {
			headerNames = append(headerNames, name)
		}
		slices.Sort(headerNames) // deterministic order in the combined error message
		for _, name := range headerNames {
			want := f.expectHeaders[name]
			got := resp.Header.Get(name)
			switch {
			case want == "" && len(resp.Header.Values(name)) == 0:
				matched = false
				reasons = append(reasons, fmt.Sprintf("header %q: missing", name))
			case want != "" && got != want:
				matched = false
				reasons = append(reasons, fmt.Sprintf("header %q: got %q, want %q", name, got, want))
			}
		}

		bodyRules := make([]string, 0, len(f.expectBody))
		for rule := range f.expectBody {
			bodyRules = append(bodyRules, rule)
		}
		slices.Sort(bodyRules)
		for _, rule := range bodyRules {
			want := f.expectBody[rule]
			got, err := extractValue(rule, bodyBytes, resp.Header, resp.StatusCode)
			switch {
			case err != nil:
				matched = false
				reasons = append(reasons, fmt.Sprintf("body %q: %v", rule, err))
			case want != "" && got != want:
				matched = false
				reasons = append(reasons, fmt.Sprintf("body %q: got %q, want %q", rule, got, want))
			}
		}

		result.successOverride = &matched
		if !matched {
			result.Error = fmt.Errorf("check failed: %s", strings.Join(reasons, "; "))
		}
	}

	var extracted map[string]string
	if len(extract) > 0 {
		extracted = make(map[string]string, len(extract))
		for name, rule := range extract {
			v, err := extractValue(rule, bodyBytes, resp.Header, resp.StatusCode)
			if err == nil {
				extracted[name] = v
			}
		}
	}

	return result, extracted
}

// HTTPGenerator issues independent HTTP requests, cycling through a set of
// targets round-robin. No state carries between requests; for multi-step
// flows with response chaining or per-worker identities, use FlowGenerator.
type HTTPGenerator struct {
	targets []httpFields
	client  *http.Client
	maxBody int64
	next    uint64
	seq     uint64
	feeder  *Feeder
}

func NewHTTPGenerator(targets []HTTPTarget, opts HTTPOptions) (*HTTPGenerator, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one target is required")
	}

	compiled := make([]httpFields, len(targets))
	for i, t := range targets {
		f, err := compileHTTPFields(t.Method, t.URL, t.Query, t.Header, t.Body, t.RawBody, opts.BaseURL, t.ExpectStatus, t.ExpectHeaders, t.ExpectBody)
		if err != nil {
			return nil, err
		}
		compiled[i] = f
	}

	client, err := newHTTPClient(opts)
	if err != nil {
		return nil, err
	}
	return &HTTPGenerator{targets: compiled, client: client, maxBody: opts.MaxResponseBody, feeder: opts.Feeder}, nil
}

func (a *HTTPGenerator) Protocol() string { return "http" }

func (a *HTTPGenerator) Close() error {
	a.client.CloseIdleConnections()
	a.feeder.Close()
	return nil
}

func (a *HTTPGenerator) Do(ctx context.Context, vuID int) []Result {
	idx := atomic.AddUint64(&a.next, 1) - 1
	tgt := a.targets[idx%uint64(len(a.targets))]
	data := tmpl.Data{Seq: atomic.AddUint64(&a.seq, 1) - 1, VU: vuID, Feeder: a.feeder.Next()}

	result, _ := doHTTP(ctx, a.client, tgt, data, nil, a.maxBody)
	return []Result{result}
}

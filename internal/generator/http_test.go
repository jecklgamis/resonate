package generator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordedRequest captures what the server actually received, so tests can
// assert on rendered templates rather than just HTTP status codes.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   string
}

func recordingServer(t *testing.T, status int, respBody string) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var recorded []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorded = append(recorded, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   string(body),
		})
		w.WriteHeader(status)
		w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, &recorded
}

func TestHTTPGeneratorSendsRenderedRequest(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{{
		Method: "POST",
		URL:    srv.URL + "/users/{{.Seq}}",
		Query:  map[string]string{"tag": "v{{.Seq}}"},
		Header: map[string]string{"X-Request-Id": "req-{{.Seq}}"},
		Body:   `{"n":{{.Seq}}}`,
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", results[0].StatusCode)
	}
	if !results[0].Success() {
		t.Errorf("expected Success() true for a 200 with no error")
	}

	if len(*recorded) != 1 {
		t.Fatalf("server recorded %d requests, want 1", len(*recorded))
	}
	req := (*recorded)[0]
	if req.Method != "POST" {
		t.Errorf("Method = %q, want POST", req.Method)
	}
	if req.Path != "/users/0" {
		t.Errorf("Path = %q, want /users/0 (rendered .Seq)", req.Path)
	}
	if req.Query != "tag=v0" {
		t.Errorf("Query = %q, want tag=v0", req.Query)
	}
	if req.Header.Get("X-Request-Id") != "req-0" {
		t.Errorf("X-Request-Id = %q, want req-0", req.Header.Get("X-Request-Id"))
	}
	if req.Body != `{"n":0}` {
		t.Errorf("Body = %q, want {\"n\":0}", req.Body)
	}
}

func TestHTTPGeneratorSeqIncrementsAcrossCalls(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL + "/ping/{{.Seq}}"}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	for i := 0; i < 3; i++ {
		a.Do(context.Background(), 0)
	}

	if len(*recorded) != 3 {
		t.Fatalf("got %d requests, want 3", len(*recorded))
	}
	for i, req := range *recorded {
		want := "/ping/" + string(rune('0'+i))
		if req.Path != want {
			t.Errorf("request %d path = %q, want %q", i, req.Path, want)
		}
	}
}

func TestHTTPGeneratorRoundRobinsTargets(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{
		{URL: srv.URL + "/a"},
		{URL: srv.URL + "/b"},
	}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	for i := 0; i < 4; i++ {
		a.Do(context.Background(), 0)
	}

	want := []string{"/a", "/b", "/a", "/b"}
	if len(*recorded) != len(want) {
		t.Fatalf("got %d requests, want %d", len(*recorded), len(want))
	}
	for i, req := range *recorded {
		if req.Path != want[i] {
			t.Errorf("request %d path = %q, want %q", i, req.Path, want[i])
		}
	}
}

func TestHTTPGeneratorDefaultsMethodToGET(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL + "/"}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	if (*recorded)[0].Method != "GET" {
		t.Errorf("Method = %q, want GET (default)", (*recorded)[0].Method)
	}
}

func TestHTTPGeneratorNoTargetsErrors(t *testing.T) {
	_, err := NewHTTPGenerator(nil, DefaultHTTPOptions())
	if err == nil {
		t.Fatal("expected an error constructing a generator with no targets")
	}
}

func TestHTTPGeneratorBadURLTemplateErrorsAtConstruction(t *testing.T) {
	_, err := NewHTTPGenerator([]HTTPTarget{{URL: "{{.Seq"}}, DefaultHTTPOptions())
	if err == nil {
		t.Fatal("expected a compile error for an unterminated template action")
	}
}

func TestHTTPGeneratorInvalidLiteralURLErrorsAtConstruction(t *testing.T) {
	cases := []string{"", "not-a-valid-url", "://missing-scheme", "http://"}
	for _, u := range cases {
		if _, err := NewHTTPGenerator([]HTTPTarget{{URL: u}}, DefaultHTTPOptions()); err == nil {
			t.Errorf("NewHTTPGenerator(%q): expected a construction-time error for an invalid literal URL", u)
		}
	}
}

func TestHTTPGeneratorTemplatedURLSkipsConstructionValidation(t *testing.T) {
	// A templated URL's final value isn't known until render time, so
	// construction must not reject it even though it isn't parseable as-is.
	_, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://example.test/{{randInt 1 10}}"}}, DefaultHTTPOptions())
	if err != nil {
		t.Errorf("NewHTTPGenerator with a templated URL should not fail at construction, got: %v", err)
	}
}

func TestHTTPGeneratorValidLiteralURLsConstructOK(t *testing.T) {
	cases := []string{"http://example.test", "https://example.test/path?query=1", "http://localhost:8080/health"}
	for _, u := range cases {
		if _, err := NewHTTPGenerator([]HTTPTarget{{URL: u}}, DefaultHTTPOptions()); err != nil {
			t.Errorf("NewHTTPGenerator(%q): unexpected error: %v", u, err)
		}
	}
}

func TestHTTPGeneratorBaseURLPrependedToRelativePath(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: "/health"}}, HTTPOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Fatalf("got %+v, want success", results[0])
	}
	if (*recorded)[0].Path != "/health" {
		t.Errorf("server received path %q, want /health", (*recorded)[0].Path)
	}
}

func TestHTTPGeneratorBaseURLLeavesAbsoluteURLUnchanged(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, "ok")

	// An absolute URL should win even when BaseURL is set to something
	// else entirely — BaseURL only applies to "/"-prefixed relative URLs.
	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL + "/health"}}, HTTPOptions{BaseURL: "http://unused.invalid"})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Fatalf("got %+v, want success (absolute URL should ignore BaseURL)", results[0])
	}
}

func TestHTTPGeneratorBaseURLTrimsTrailingSlash(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: "/health"}}, HTTPOptions{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	if (*recorded)[0].Path != "/health" {
		t.Errorf("server received path %q, want /health (no double slash)", (*recorded)[0].Path)
	}
}

func TestHTTPGeneratorRelativeURLWithNoBaseURLErrorsAtConstruction(t *testing.T) {
	_, err := NewHTTPGenerator([]HTTPTarget{{URL: "/health"}}, DefaultHTTPOptions())
	if err == nil {
		t.Fatal("expected a construction-time error for a relative URL with no BaseURL set")
	}
}

func TestHTTPGeneratorBaseURLDoesNotApplyToTemplatedURL(t *testing.T) {
	// A URL starting with "{{" never starts with "/", so BaseURL must not
	// touch it -- the whole point of a template expression is that its
	// final value isn't known until render time.
	_, err := NewHTTPGenerator([]HTTPTarget{{URL: `{{env "FULL_URL"}}`}}, HTTPOptions{BaseURL: "http://unused.invalid"})
	if err != nil {
		t.Errorf("NewHTTPGenerator with a templated URL should not fail at construction, got: %v", err)
	}
}

func TestHTTPGeneratorConnectionErrorProducesErrorResult(t *testing.T) {
	a, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://127.0.0.1:1"}}, HTTPOptions{})
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Error == nil {
		t.Error("expected a connection error for an unreachable port")
	}
	if results[0].Success() {
		t.Error("a result with an error must not report Success()")
	}
}

func TestHTTPGeneratorNonSuccessStatusIsNotSuccess(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError, "boom")

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("a 500 response must not report Success()")
	}
	if results[0].StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", results[0].StatusCode)
	}
}

func TestHTTPGeneratorExpectStatusOverridesDefaultRange(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusNotFound, "not found")

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectStatus: []int{404}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("a 404 explicitly in ExpectStatus should report Success(), got Error: %v", results[0].Error)
	}
	if results[0].StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", results[0].StatusCode)
	}
}

func TestHTTPGeneratorExpectStatusRejectsUnlistedCode(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectStatus: []int{201, 202}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("a 200 not in ExpectStatus must not report Success()")
	}
	if results[0].StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200 (the real status, even though the check failed)", results[0].StatusCode)
	}
	if results[0].Error == nil {
		t.Fatal("expected a non-nil Error explaining the failed status check")
	}
}

func TestNewHTTPGeneratorRejectsInvalidExpectStatus(t *testing.T) {
	for _, code := range []int{99, 600, -1} {
		if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://example.test", ExpectStatus: []int{code}}}, DefaultHTTPOptions()); err == nil {
			t.Errorf("expected a construction-time error for expect_status %d", code)
		}
	}
}

func TestHTTPGeneratorExpectHeadersExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectHeaders: map[string]string{"Content-Type": "application/json"}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("matching header should report Success(), got Error: %v", results[0].Error)
	}
}

func TestHTTPGeneratorExpectHeadersMismatchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectHeaders: map[string]string{"Content-Type": "application/json"}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("a mismatched header value must not report Success()")
	}
	if results[0].Error == nil || !strings.Contains(results[0].Error.Error(), "Content-Type") {
		t.Errorf("Error = %v, want it to name the failed header", results[0].Error)
	}
}

func TestHTTPGeneratorExpectHeadersEmptyValueChecksPresenceOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "anything-goes-here")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectHeaders: map[string]string{"X-Request-Id": ""}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("a present header with an empty expected value should report Success(), got Error: %v", results[0].Error)
	}
}

func TestHTTPGeneratorExpectHeadersMissingHeaderFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectHeaders: map[string]string{"X-Request-Id": ""}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("a missing required header must not report Success()")
	}
}

func TestHTTPGeneratorExpectHeadersNameIsCaseInsensitive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json") // lowercase on the wire
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectHeaders: map[string]string{"Content-Type": "application/json"}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("header name matching should be case-insensitive, got Error: %v", results[0].Error)
	}
}

func TestHTTPGeneratorExpectHeadersCombinesWithExpectStatusAsAND(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain") // wrong header, right status
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{
		URL:           srv.URL,
		ExpectStatus:  []int{200},
		ExpectHeaders: map[string]string{"Content-Type": "application/json"},
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("a passing status check with a failing header check must not report Success() overall (AND semantics)")
	}
}

func TestNewHTTPGeneratorRejectsEmptyExpectHeaderName(t *testing.T) {
	if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://example.test", ExpectHeaders: map[string]string{" ": "x"}}}, DefaultHTTPOptions()); err == nil {
		t.Error("expected a construction-time error for a blank expect_headers name")
	}
}

func TestHTTPGeneratorExpectBodyJSONExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","id":42}`))
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectBody: map[string]string{"json:status": "ok"}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("matching JSONPath check should report Success(), got Error: %v", results[0].Error)
	}
}

func TestHTTPGeneratorExpectBodyJSONMismatchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error"}`))
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectBody: map[string]string{"json:status": "ok"}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("a mismatched JSONPath value must not report Success()")
	}
	if results[0].Error == nil || !strings.Contains(results[0].Error.Error(), "json:status") {
		t.Errorf("Error = %v, want it to name the failed rule", results[0].Error)
	}
}

func TestHTTPGeneratorExpectBodyJSONEmptyValueChecksExistenceOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"anything"}`))
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectBody: map[string]string{"json:id": ""}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("an existing path with an empty expected value should report Success(), got Error: %v", results[0].Error)
	}
}

func TestHTTPGeneratorExpectBodyJSONMissingPathFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectBody: map[string]string{"json:nope": ""}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("a missing JSONPath match must not report Success()")
	}
}

func TestHTTPGeneratorExpectBodyXMLExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<response><status>ok</status></response>`))
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectBody: map[string]string{"xml://status": "ok"}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("matching XPath check should report Success(), got Error: %v", results[0].Error)
	}
}

func TestHTTPGeneratorExpectBodyYAMLExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write([]byte("status: ok\nid: 42\n"))
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, ExpectBody: map[string]string{"yaml:status": "ok"}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if !results[0].Success() {
		t.Errorf("matching YAMLPath check should report Success(), got Error: %v", results[0].Error)
	}
}

func TestHTTPGeneratorExpectBodyCombinesWithExpectStatusAndHeadersAsAND(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"error"}`)) // wrong body value, right status and header
	}))
	t.Cleanup(srv.Close)

	a, err := NewHTTPGenerator([]HTTPTarget{{
		URL:           srv.URL,
		ExpectStatus:  []int{200},
		ExpectHeaders: map[string]string{"Content-Type": "application/json"},
		ExpectBody:    map[string]string{"json:status": "ok"},
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Success() {
		t.Error("passing status/header checks with a failing body check must not report Success() overall (AND semantics)")
	}
}

func TestNewHTTPGeneratorRejectsUnknownExpectBodyRule(t *testing.T) {
	if _, err := NewHTTPGenerator([]HTTPTarget{{URL: "http://example.test", ExpectBody: map[string]string{"bogus:rule": "x"}}}, DefaultHTTPOptions()); err == nil {
		t.Error("expected a construction-time error for an unrecognized expect_body rule prefix")
	}
}

func TestHTTPGeneratorBytesInOut(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, "0123456789")

	a, err := NewHTTPGenerator([]HTTPTarget{{Method: "POST", URL: srv.URL, Body: "abcde"}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].BytesOut != 5 {
		t.Errorf("BytesOut = %d, want 5", results[0].BytesOut)
	}
	if results[0].BytesIn != 10 {
		t.Errorf("BytesIn = %d, want 10", results[0].BytesIn)
	}
}

func TestHTTPGeneratorRawBodySentUnrendered(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	// A literal "{{" in the raw body must be sent as-is, not treated as a
	// template expression (and must not error trying to render it).
	raw := []byte(`payload with literal {{ braces }} and {{.Seq}}`)
	a, err := NewHTTPGenerator([]HTTPTarget{{Method: "POST", URL: srv.URL, RawBody: raw}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 || !results[0].Success() {
		t.Fatalf("got %+v, want a successful result", results)
	}
	if results[0].BytesOut != int64(len(raw)) {
		t.Errorf("BytesOut = %d, want %d", results[0].BytesOut, len(raw))
	}
	if (*recorded)[0].Body != string(raw) {
		t.Errorf("server received body %q, want %q (unrendered)", (*recorded)[0].Body, string(raw))
	}
}

func TestHTTPGeneratorRawBodyAndBodyMutuallyExclusive(t *testing.T) {
	_, err := NewHTTPGenerator([]HTTPTarget{{
		Method:  "POST",
		URL:     "http://example.test",
		Body:    "abc",
		RawBody: []byte("xyz"),
	}}, DefaultHTTPOptions())
	if err == nil {
		t.Fatal("expected an error combining Body and RawBody on the same target")
	}
}

func TestHTTPGeneratorRawBodyEmptySliceSendsNoBody(t *testing.T) {
	srv, recorded := recordingServer(t, http.StatusOK, "ok")

	a, err := NewHTTPGenerator([]HTTPTarget{{Method: "POST", URL: srv.URL, RawBody: []byte{}}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].BytesOut != 0 {
		t.Errorf("BytesOut = %d, want 0", results[0].BytesOut)
	}
	if (*recorded)[0].Body != "" {
		t.Errorf("server received body %q, want empty", (*recorded)[0].Body)
	}
}

func TestHTTPGeneratorHeaderTemplateError(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusOK, "ok")
	// Header values that fail to render (e.g. calling a function with wrong
	// arg count) should surface as an error Result, not panic.
	a, err := NewHTTPGenerator([]HTTPTarget{{
		URL:    srv.URL,
		Header: map[string]string{"X-Bad": "{{randInt 1}}"}, // randInt needs 2 args
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if results[0].Error == nil {
		t.Error("expected a render error for a header template with wrong function arity")
	}
}

func TestHTTPGeneratorFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	opts := DefaultHTTPOptions()
	opts.FollowRedirects = true
	a, err := NewHTTPGenerator([]HTTPTarget{{URL: redirector.URL}}, opts)
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()
	results := a.Do(context.Background(), 0)
	if results[0].StatusCode != http.StatusOK {
		t.Errorf("with FollowRedirects=true, StatusCode = %d, want 200 (final destination)", results[0].StatusCode)
	}

	opts.FollowRedirects = false
	a2, err := NewHTTPGenerator([]HTTPTarget{{URL: redirector.URL}}, opts)
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a2.Close()
	results2 := a2.Do(context.Background(), 0)
	if results2[0].StatusCode != http.StatusFound {
		t.Errorf("with FollowRedirects=false, StatusCode = %d, want 302 (the redirect itself)", results2[0].StatusCode)
	}
}

func TestHTTPGeneratorLiteralJSONBodyUnaffectedByTemplating(t *testing.T) {
	// Sanity check that a body with no {{ }} round-trips byte-for-byte.
	srv, recorded := recordingServer(t, http.StatusOK, "ok")
	body := `{"static":true,"list":[1,2,3]}`

	a, err := NewHTTPGenerator([]HTTPTarget{{Method: "POST", URL: srv.URL, Body: body}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer a.Close()
	a.Do(context.Background(), 0)

	var got map[string]any
	if err := json.Unmarshal([]byte((*recorded)[0].Body), &got); err != nil {
		t.Fatalf("server-received body isn't valid JSON: %v", err)
	}
	if (*recorded)[0].Body != body {
		t.Errorf("Body = %q, want unchanged %q", (*recorded)[0].Body, body)
	}
}

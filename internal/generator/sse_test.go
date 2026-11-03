package generator

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventStream writes the SSE response preamble and returns a function that
// writes raw text and flushes it to the client immediately.
func eventStream(w http.ResponseWriter) func(string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	flusher.Flush()
	return func(s string) {
		io.WriteString(w, s)
		flusher.Flush()
	}
}

// infiniteEvents emits an event every 5ms until the client goes away.
func infiniteEvents(w http.ResponseWriter, r *http.Request) {
	send := eventStream(w)
	for i := 0; ; i++ {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Millisecond):
			send(fmt.Sprintf("data: tick %d\n\n", i))
		}
	}
}

func sseOpts() SSEOptions {
	o := DefaultSSEOptions()
	o.HTTP.Timeout = 3 * time.Second
	return o
}

func countEvents(results []Result) int {
	return len(results) - 1 // the first Result is the connect
}

func TestSSEGeneratorStopsAfterEvents(t *testing.T) {
	var mu sync.Mutex
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		accept = r.Header.Get("Accept")
		mu.Unlock()
		infiniteEvents(w, r)
	}))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Events: 5}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	if len(res) != 6 {
		t.Fatalf("want connect + 5 event results, got %d: %+v", len(res), res)
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d failed: %+v", i, r)
		}
		if r.Protocol != "sse" {
			t.Fatalf("result %d protocol = %q", i, r.Protocol)
		}
	}
	if res[0].StatusCode != 200 {
		t.Fatalf("connect status = %d", res[0].StatusCode)
	}
	// "data: tick 0\n\n" is 14 bytes on the wire.
	if res[1].BytesIn != int64(len("data: tick 0\n\n")) {
		t.Fatalf("first event BytesIn = %d, want %d", res[1].BytesIn, len("data: tick 0\n\n"))
	}
	mu.Lock()
	defer mu.Unlock()
	if accept != "text/event-stream" {
		t.Fatalf("server saw Accept %q, want text/event-stream", accept)
	}
}

func TestSSEGeneratorEventParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := eventStream(w)
		send(": keep-alive comment\n\n")               // comment-only block: not an event
		send("retry: 1000\n\n")                        // no data field: not an event
		send("event: greeting\ndata: hi\n\n")          // one event
		send("data: line one\ndata: line two\r\n\r\n") // multi-line data + CRLF: still one event
		send("data\n\n")                               // "data" with no colon: an event with empty data
		send(": trailing comment\n")
	}))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	if got := countEvents(res); got != 3 {
		t.Fatalf("want 3 events, got %d: %+v", got, res)
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d failed: %+v", i, r)
		}
	}

	// Every byte on the wire is attributed to some Result.
	var total int64
	for _, r := range res {
		total += r.BytesIn
	}
	want := int64(len(": keep-alive comment\n\n") + len("retry: 1000\n\n") + len("event: greeting\ndata: hi\n\n") +
		len("data: line one\ndata: line two\r\n\r\n") + len("data\n\n") + len(": trailing comment\n"))
	if total != want {
		t.Fatalf("total BytesIn = %d, want %d", total, want)
	}
}

func TestSSEGeneratorDurationEndsNormally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(infiniteEvents))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Duration: 150 * time.Millisecond}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	start := time.Now()
	res := g.Do(context.Background(), 0)
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("stream lasted %s, want ~150ms", elapsed)
	}
	if countEvents(res) < 3 {
		t.Fatalf("expected several events in 150ms, got %d", countEvents(res))
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d should succeed (duration elapsing is a normal end): %+v", i, r)
		}
	}
}

func TestSSEGeneratorDurationLongerThanClientTimeout(t *testing.T) {
	// Timeout bounds connecting, not the stream: a listen window longer than
	// Timeout must not be cut short by http.Client.Timeout.
	srv := httptest.NewServer(http.HandlerFunc(infiniteEvents))
	defer srv.Close()

	opts := sseOpts()
	opts.HTTP.Timeout = 100 * time.Millisecond
	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Duration: 400 * time.Millisecond}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	start := time.Now()
	res := g.Do(context.Background(), 0)
	if time.Since(start) < 350*time.Millisecond {
		t.Fatalf("stream cut short after %s", time.Since(start))
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d failed: %+v", i, r)
		}
	}
}

func TestSSEGeneratorServerClosesBeforeEnoughEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := eventStream(w)
		send("data: 1\n\n")
		send("data: 2\n\n")
	}))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Events: 5}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	last := res[len(res)-1]
	if last.Success() || last.Error == nil || !strings.Contains(last.Error.Error(), "stream ended after 2 of 5 events") {
		t.Fatalf("want a final 'stream ended after 2 of 5 events' failure, got %+v", last)
	}
	if countEvents(res) != 3 { // 2 events + the failure Result
		t.Fatalf("results = %d, want connect + 2 events + failure", len(res))
	}
}

func TestSSEGeneratorEventsDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		send := eventStream(w)
		send("data: only one\n\n")
		<-r.Context().Done() // then stall
	}))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Events: 3, Duration: 150 * time.Millisecond}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	last := res[len(res)-1]
	if last.Success() || last.Error == nil || !strings.Contains(last.Error.Error(), "with 1 of 3 events") {
		t.Fatalf("want a 'with 1 of 3 events' timeout failure, got %+v", last)
	}
}

func TestSSEGeneratorEndlessStreamWithNoBoundFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(infiniteEvents))
	defer srv.Close()

	opts := sseOpts()
	opts.HTTP.Timeout = 150 * time.Millisecond
	g, err := NewSSEGenerator(SSETarget{URL: srv.URL}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	last := res[len(res)-1]
	if last.Success() || last.Error == nil || !strings.Contains(last.Error.Error(), "set sse duration or events") {
		t.Fatalf("want an unbounded-stream failure pointing at duration/events, got %+v", last)
	}
}

func TestSSEGeneratorRejectsWrongContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"not":"a stream"}`)
	}))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Events: 1}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() || res[0].Error == nil || !strings.Contains(res[0].Error.Error(), "content-type") {
		t.Fatalf("want a single failed connect result mentioning content-type, got %+v", res)
	}
}

func TestSSEGeneratorFailsOnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Events: 1}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() || res[0].StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want one failed 503 result, got %+v", res)
	}
}

func TestSSEGeneratorConnectErrorAndTimeout(t *testing.T) {
	// Connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: url, Events: 1}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if res := g.Do(context.Background(), 0); len(res) != 1 || res[0].Success() || res[0].Error == nil {
		t.Fatalf("want a failed connect result, got %+v", res)
	}

	// Server accepts but never sends headers.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()

	opts := sseOpts()
	opts.HTTP.Timeout = 100 * time.Millisecond
	g2, err := NewSSEGenerator(SSETarget{URL: hang.URL, Events: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	res := g2.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() || res[0].Error == nil || !strings.Contains(res[0].Error.Error(), "connect timed out") {
		t.Fatalf("want a 'connect timed out' failure, got %+v", res)
	}
}

func TestSSEGeneratorRunEndingMidStreamIsNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(infiniteEvents))
	defer srv.Close()

	g, err := NewSSEGenerator(SSETarget{URL: srv.URL, Events: 1000}, sseOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := g.Do(ctx, 0)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Do didn't return promptly after ctx ended: %s", time.Since(start))
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d: a run ending mid-stream shouldn't add a failure: %+v", i, r)
		}
	}
}

func TestSSEGeneratorPostBodyHeadersTemplatesAndIdentity(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotBody, gotAuth, gotCT, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotBody, gotAuth, gotCT, gotQuery = r.Method, string(b), r.Header.Get("Authorization"), r.Header.Get("Accept"), r.URL.RawQuery
		mu.Unlock()
		send := eventStream(w)
		send("data: done\n\n")
	}))
	defer srv.Close()

	opts := sseOpts()
	opts.Identities = []map[string]string{{"token": "t0"}, {"token": "t1"}}
	g, err := NewSSEGenerator(SSETarget{
		Method: "POST",
		URL:    srv.URL + "/chat",
		Query:  map[string]string{"vu": "{{.VU}}"},
		Header: map[string]string{"Authorization": "Bearer {{.Identity.token}}", "Accept": "text/event-stream, application/json"},
		Body:   `{"prompt":"hello from vu {{.VU}}"}`,
		Events: 1,
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 1)
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d failed: %+v", i, r)
		}
	}
	if res[0].BytesOut != int64(len(`{"prompt":"hello from vu 1"}`)) {
		t.Fatalf("connect BytesOut = %d", res[0].BytesOut)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != "POST" || gotBody != `{"prompt":"hello from vu 1"}` || gotAuth != "Bearer t1" || gotQuery != "vu=1" {
		t.Fatalf("server saw method=%q body=%q auth=%q query=%q", gotMethod, gotBody, gotAuth, gotQuery)
	}
	if gotCT != "text/event-stream, application/json" {
		t.Fatalf("a user-supplied Accept must not be overwritten, got %q", gotCT)
	}
}

func TestSSEGeneratorFeederAndBaseURL(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		eventStream(w)("data: x\n\n")
	}))
	defer srv.Close()

	feeder, err := NewFeeder(writeTempDataFile(t, "rooms.csv", "room\nalpha\nbeta\n"), "sequential")
	if err != nil {
		t.Fatal(err)
	}
	opts := sseOpts()
	opts.HTTP.BaseURL = srv.URL
	opts.HTTP.Feeder = feeder
	g, err := NewSSEGenerator(SSETarget{URL: "/rooms/{{.Feeder.room}}/stream", Events: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	g.Do(context.Background(), 0)
	g.Do(context.Background(), 0)

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/rooms/alpha/stream" || paths[1] != "/rooms/beta/stream" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestSSEGeneratorMaxResponseBodyCapsStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(infiniteEvents))
	defer srv.Close()

	opts := sseOpts()
	opts.HTTP.MaxResponseBody = int64(len("data: tick 0\n\n") * 2) // exactly two events
	g, err := NewSSEGenerator(SSETarget{URL: srv.URL}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	if countEvents(res) != 2 {
		t.Fatalf("want 2 events before the cap, got %d: %+v", countEvents(res), res)
	}
	for i, r := range res {
		if !r.Success() {
			t.Fatalf("result %d failed: %+v", i, r)
		}
	}
}

func TestNewSSEGeneratorValidation(t *testing.T) {
	cases := map[string]SSETarget{
		"negative events":   {URL: "http://example.test", Events: -1},
		"negative duration": {URL: "http://example.test", Duration: -time.Second},
		"relative url":      {URL: "/stream"},
		"missing scheme":    {URL: "example.test/stream"},
		"body and raw body": {URL: "http://example.test", Body: "a", RawBody: []byte("b")},
	}
	for name, target := range cases {
		if _, err := NewSSEGenerator(target, sseOpts()); err == nil {
			t.Errorf("%s: expected a construction error", name)
		}
	}
}

func TestSSEGeneratorRunEndingWhileConnectingIsNotAFailure(t *testing.T) {
	// A server that never answers: the connect is still pending when the run ends.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()

	g, err := NewSSEGenerator(SSETarget{URL: hang.URL, Events: 1}, sseOpts()) // 3s connect timeout
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := g.Do(ctx, 0)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Do didn't return promptly once the run ended: %s", time.Since(start))
	}
	if len(res) != 0 {
		t.Fatalf("a connect cut off by the run ending must not be reported, got %+v", res)
	}

	// While the run is live, the same hang is a real connect timeout.
	opts := sseOpts()
	opts.HTTP.Timeout = 100 * time.Millisecond
	g2, err := NewSSEGenerator(SSETarget{URL: hang.URL, Events: 1}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	res = g2.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() || !strings.Contains(res[0].Error.Error(), "connect timed out") {
		t.Fatalf("want a 'connect timed out' failure on a live run, got %+v", res)
	}
}

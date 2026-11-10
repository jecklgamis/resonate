package generator

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jecklgamis/resonate/internal/tmpl"
)

// sseMaxLine caps a single SSE line (one "data: ..." field). A longer line is
// reported as a stream error instead of buffering without bound.
const sseMaxLine = 4 << 20

// SSETarget describes one Server-Sent Events stream: send a request (GET by
// default; POST with a body is common for streaming APIs), read the
// text/event-stream response for Events events and/or Duration, then close.
// URL, Query, Header and Body may embed {{ }} expressions (see
// internal/tmpl), re-rendered on every iteration.
type SSETarget struct {
	Method  string
	URL     string
	Query   map[string]string
	Header  map[string]string
	Body    string
	RawBody []byte // sent as-is, bypassing templating; mutually exclusive with Body

	// Compress compresses the request body ("gzip", "deflate", "br" or
	// "zstd") — see HTTPTarget.Compress.
	Compress string

	// Events, if > 0, ends the stream successfully once this many events have
	// arrived. The stream closing — or Duration / the transport Timeout
	// elapsing — before then is a failure.
	Events int

	// Duration, if > 0, is how long to listen. With Events == 0, reaching it
	// is the normal, successful end of the stream; with Events > 0 it is a
	// deadline for receiving them. With neither Events nor Duration set the
	// stream is read until the server closes it, bounded by
	// SSEOptions.HTTP.Timeout (hitting that bound is a failure — a stream
	// that never ends needs Events or Duration to say when to stop).
	Duration time.Duration
}

// SSEOptions configures the transport and per-VU data shared by every stream.
// HTTP carries the usual transport settings (Timeout, Insecure, certs/CA,
// H2C, HTTP3, MaxResponseBody, BaseURL, Feeder) — Timeout bounds connecting
// and receiving response headers, not the stream itself (see SSETarget).
// MaxResponseBody caps the bytes read per stream; the stream then looks like
// the server closed it.
type SSEOptions struct {
	HTTP HTTPOptions

	// Identities, if non-empty, is a pool of named values (e.g. tokens).
	// Each virtual user (vuID) gets exactly one, round-robin over the pool,
	// sticky for that VU's lifetime, exposed as {{.Identity.<key>}}.
	Identities []map[string]string
}

func DefaultSSEOptions() SSEOptions {
	return SSEOptions{HTTP: DefaultHTTPOptions()}
}

// SSEGenerator opens a fresh stream every iteration, reads it per its
// SSETarget, and closes it — like WSGenerator, a stream isn't pooled across
// iterations (a held-open response has no safe "VU departed" hook to close
// it from).
//
// One iteration reports one Result for the connect (Latency = time to
// response headers; fails on a non-2xx/3xx status or a Content-Type other
// than text/event-stream) plus one Result per event (Latency = gap since the
// previous event, or for the first event since the headers arrived; BytesIn =
// the event's bytes on the wire, including any comment/keep-alive lines
// before it). Lines starting with ":" (comments) are never counted as events.
// A stream that ends unsuccessfully adds a final failed Result carrying the
// reason.
type SSEGenerator struct {
	fields     httpFields
	client     *http.Client
	timeout    time.Duration
	events     int
	duration   time.Duration
	maxBody    int64
	identities []map[string]string
	feeder     *Feeder
	seq        uint64
}

func NewSSEGenerator(target SSETarget, opts SSEOptions) (*SSEGenerator, error) {
	if target.Events < 0 {
		return nil, fmt.Errorf("sse events must be >= 0, got %d", target.Events)
	}
	if target.Duration < 0 {
		return nil, fmt.Errorf("sse duration must be >= 0, got %s", target.Duration)
	}

	f, err := compileHTTPFields(target.Method, target.URL, target.Query, target.Header, target.Body, target.RawBody, opts.HTTP.BaseURL, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if f, err = f.withCompress(target.Compress); err != nil {
		return nil, err
	}

	if opts.HTTP.HTTP3 {
		if err := requireHTTPSForHTTP3(f); err != nil {
			return nil, err
		}
	}

	client, err := newHTTPClient(opts.HTTP)
	if err != nil {
		return nil, err
	}
	// http.Client.Timeout covers reading the response body, which would cut
	// every long-lived stream short; connect and stream bounds are enforced
	// per Do() instead.
	client.Timeout = 0

	timeout := opts.HTTP.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &SSEGenerator{
		fields:     f,
		client:     client,
		timeout:    timeout,
		events:     target.Events,
		duration:   target.Duration,
		maxBody:    opts.HTTP.MaxResponseBody,
		identities: opts.Identities,
		feeder:     opts.HTTP.Feeder,
	}, nil
}

func (a *SSEGenerator) Protocol() string { return "sse" }

func (a *SSEGenerator) Close() error {
	closeClient(a.client)
	a.feeder.Close()
	return nil
}

func (a *SSEGenerator) identityFor(vuID int) map[string]string {
	if len(a.identities) == 0 {
		return nil
	}
	return a.identities[vuID%len(a.identities)]
}

func (a *SSEGenerator) Do(ctx context.Context, vuID int) []Result {
	data := tmpl.Data{Seq: atomic.AddUint64(&a.seq, 1) - 1, VU: vuID, Identity: a.identityFor(vuID), Feeder: a.feeder.Next()}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, bodyLen, err := buildRequest(streamCtx, a.fields, data)
	if err != nil {
		return []Result{{Timestamp: time.Now(), Error: err, Protocol: "sse"}}
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/event-stream")
	}
	if req.Header.Get("Cache-Control") == "" {
		req.Header.Set("Cache-Control", "no-cache")
	}

	start := time.Now()
	connectTimer := time.AfterFunc(a.timeout, cancel)
	resp, err := a.client.Do(req)
	connectInTime := connectTimer.Stop()
	if err == nil && !connectInTime {
		// The timer fired just as the response arrived; the context is
		// already cancelled, so the stream couldn't be read anyway.
		resp.Body.Close()
		err = context.DeadlineExceeded
	}
	if err != nil {
		if runEnded(ctx) {
			// The run itself ended while we were still connecting; the server
			// never got to answer, so this isn't a failure to report.
			return nil
		}
		if !connectInTime {
			err = fmt.Errorf("connect timed out after %s", a.timeout)
		}
		return []Result{{Timestamp: start, Latency: time.Since(start), Error: err, Protocol: "sse", BytesOut: int64(bodyLen)}}
	}
	defer resp.Body.Close()

	headersAt := time.Now()
	connect := Result{Timestamp: start, Latency: headersAt.Sub(start), StatusCode: resp.StatusCode, Protocol: "sse", BytesOut: int64(bodyLen)}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		// Not a stream. Drain a little so a keep-alive connection can be reused.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return []Result{connect}
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		connect.Error = fmt.Errorf("unexpected content-type %q (want text/event-stream)", resp.Header.Get("Content-Type"))
		return []Result{connect}
	}

	limit, capped := a.duration, false
	if limit == 0 {
		limit, capped = a.timeout, true
	}
	var limitHit atomic.Bool
	limitTimer := time.AfterFunc(limit, func() {
		limitHit.Store(true)
		cancel()
	})
	defer limitTimer.Stop()

	var body io.Reader = resp.Body
	if a.maxBody > 0 {
		body = io.LimitReader(body, a.maxBody)
	}

	// consumed counts bytes of lines the scanner has actually handed back (not
	// bytes merely read ahead into its buffer), so each event's BytesIn is
	// exact.
	var consumed int64
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), sseMaxLine)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		consumed += int64(advance)
		return advance, token, err
	})

	results := []Result{connect}
	var (
		n            int
		hasData      bool
		lastMark     = headersAt
		lastConsumed int64
		reached      bool
	)
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0: // blank line ends the event being assembled
			if !hasData {
				continue
			}
			hasData = false
			now := time.Now()
			n++
			results = append(results, Result{
				Timestamp:  lastMark,
				Latency:    now.Sub(lastMark),
				StatusCode: 200, // synthetic, like ws: Success() expects 2xx/3xx
				BytesIn:    consumed - lastConsumed,
				Protocol:   "sse",
			})
			lastMark, lastConsumed = now, consumed
		case line[0] == ':': // comment / keep-alive
		default:
			// A line without a colon is a field name with an empty value.
			if name, _, _ := bytes.Cut(line, []byte{':'}); string(name) == "data" {
				hasData = true
			}
		}
		if a.events > 0 && n >= a.events {
			reached = true
			break
		}
	}
	scanErr := sc.Err()

	// Bytes consumed after the last dispatched event (trailing comments, a
	// partial event) still crossed the wire; attribute them to the last Result.
	if rest := consumed - lastConsumed; rest > 0 {
		results[len(results)-1].BytesIn += rest
	}

	fail := func(err error) []Result {
		return append(results, Result{Timestamp: lastMark, Latency: time.Since(lastMark), Error: err, Protocol: "sse"})
	}
	switch {
	case reached:
		return results
	case runEnded(ctx):
		// The run itself ended mid-stream; that's not the server's fault.
		return results
	case limitHit.Load():
		switch {
		case a.events > 0:
			return fail(fmt.Errorf("timed out after %s with %d of %d events", limit, n, a.events))
		case capped:
			return fail(fmt.Errorf("stream still open after the %s timeout; set sse duration or events to end it sooner", limit))
		}
		return results // Duration elapsed: the normal end of a listen window
	case scanErr != nil:
		return fail(scanErr)
	case a.events > 0: // EOF before enough events
		return fail(fmt.Errorf("stream ended after %d of %d events", n, a.events))
	}
	return results // server closed the stream
}

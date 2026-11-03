// Command testsseserver is a small Server-Sent Events server for trying
// resonate's SSE examples without any service of your own. It uses only the
// standard library and serves what examples/sse-basic.yaml, examples/sse-post.yaml
// and the SSE scenarios in the docs call:
//
//	GET  /events      a finite stream of events, then the server closes it
//	POST /v1/stream   an LLM-style streaming API: a token per event, then [DONE]
//	GET  /endless     a stream that never ends (to see resonate's timeout rule)
//	GET  /json        a 200 that is not an event stream (to see the content-type check)
//	GET  /error       an error status, 503 by default (?code=NNN)
//	GET  /healthz     plain "ok"
//
//	go run ./examples/testsseserver                  # :8765
//	go run ./examples/testsseserver -addr :9090 -events 30
//
// It logs what it does: each request as it arrives and as it finishes (status,
// duration, events sent), and what the streams did. That is a lot of output
// under load, so for a performance run use -log-requests=false, which leaves a
// one-line summary every -stats-interval instead.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// options configures the server.
type options struct {
	// eventInterval is the gap between events on every stream.
	eventInterval time.Duration

	// defaultEvents is how many events GET /events sends before closing
	// (override per request with ?n=).
	defaultEvents int

	// logger receives the server's log lines; nil logs nothing.
	logger *log.Logger

	// logRequests logs every request as it arrives and finishes, plus what the
	// streams did. Off, only the periodic summary line is printed, which is the
	// setting for load tests.
	logRequests bool
}

func (o options) logf(format string, args ...any) {
	if o.logRequests && o.logger != nil {
		o.logger.Printf(format, args...)
	}
}

// exampleServer is the HTTP handler plus the counters behind its summary line.
type exampleServer struct {
	http.Handler
	access *accessLog
}

func main() {
	addr := flag.String("addr", ":8765", "address to listen on")
	interval := flag.Duration("event-interval", 20*time.Millisecond, "gap between events on a stream")
	events := flag.Int("events", 12, "events GET /events sends before closing it")
	logRequests := flag.Bool("log-requests", true, "log every request and what the streams did (turn off for load tests)")
	statsEvery := flag.Duration("stats-interval", 10*time.Second, "print a one-line request summary this often when there was traffic (0 disables)")
	flag.Parse()

	srv := newServer(options{
		eventInterval: *interval,
		defaultEvents: *events,
		logger:        log.Default(),
		logRequests:   *logRequests,
	})
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	// No WriteTimeout: a stream is a response that stays open on purpose.
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}

	if *statsEvery > 0 {
		go func() {
			for range time.Tick(*statsEvery) {
				if line, ok := srv.access.summary(*statsEvery); ok {
					log.Print(line)
				}
			}
		}()
	}

	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		sig := <-stop
		n, failed := srv.access.totals()
		log.Printf("received %v: shutting down after %d requests (%d failed)", sig, n, failed)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if httpSrv.Shutdown(ctx) != nil { // streams may never end on their own
			httpSrv.Close()
		}
	}()

	log.Printf("example SSE server listening on %s", lis.Addr())
	log.Printf("config: event-interval=%s events=%d log-requests=%v stats-interval=%s", *interval, *events, *logRequests, *statsEvery)
	log.Println("endpoints: GET /events, POST /v1/stream, GET /endless, GET /json, GET /error, GET /healthz")
	if !*logRequests {
		log.Println("per-request logging is off; a summary line is printed every", *statsEvery, "while there is traffic")
	}
	if err := httpSrv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// newServer builds the HTTP handler.
func newServer(opts options) *exampleServer {
	if opts.eventInterval <= 0 {
		opts.eventInterval = 20 * time.Millisecond
	}
	if opts.defaultEvents <= 0 {
		opts.defaultEvents = 12
	}
	var logf func(string, ...any)
	if opts.logger != nil {
		logf = opts.logger.Printf
	}
	access := newAccessLog(logf, opts.logRequests)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) { handleEvents(opts, w, r) })
	mux.HandleFunc("POST /v1/stream", func(w http.ResponseWriter, r *http.Request) { handleStream(opts, w, r) })
	mux.HandleFunc("GET /endless", func(w http.ResponseWriter, r *http.Request) { handleEndless(opts, w, r) })
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"not":"an event stream"}`+"\n")
	})
	mux.HandleFunc("GET /error", func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusServiceUnavailable
		if c, err := strconv.Atoi(r.URL.Query().Get("code")); err == nil && c >= 400 && c <= 599 {
			code = c
		}
		http.Error(w, http.StatusText(code), code)
	})

	return &exampleServer{Handler: access.wrap(mux), access: access}
}

// beginStream sends the headers of an event stream.
func beginStream(w http.ResponseWriter) http.Flusher {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	if f != nil {
		f.Flush() // headers out now, so the client's connect completes before the first event
	}
	return f
}

// sendEvent writes one event ("id" and "event" omitted when empty) and flushes.
func sendEvent(w http.ResponseWriter, f http.Flusher, id, event, data string) error {
	var b strings.Builder
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", id)
	}
	if event != "" {
		fmt.Fprintf(&b, "event: %s\n", event)
	}
	fmt.Fprintf(&b, "data: %s\n\n", data)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	countEvent(w)
	if f != nil {
		f.Flush()
	}
	return nil
}

// sendComment writes a comment line, the usual keep-alive; it is not an event.
func sendComment(w http.ResponseWriter, f http.Flusher, text string) error {
	if _, err := fmt.Fprintf(w, ": %s\n\n", text); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// handleEvents sends a finite stream, then closes it. Every fourth event is
// preceded by a keep-alive comment, to show comments aren't counted as events.
func handleEvents(opts options, w http.ResponseWriter, r *http.Request) {
	n, interval := opts.defaultEvents, opts.eventInterval
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 10000 {
		n = v
	}
	if v, err := time.ParseDuration(r.URL.Query().Get("interval")); err == nil && v > 0 && v <= 10*time.Second {
		interval = v
	}

	f := beginStream(w)
	opts.logf("events: stream opened, %d events %s apart", n, interval)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for sent := 0; sent < n; {
		select {
		case <-r.Context().Done():
			opts.logf("events: client went away after %d of %d events", sent, n)
			return
		case <-tick.C:
			if sent%4 == 0 {
				if sendComment(w, f, "keep-alive") != nil {
					return
				}
			}
			if sendEvent(w, f, strconv.Itoa(sent), "tick", fmt.Sprintf(`{"n":%d}`, sent)) != nil {
				return
			}
			sent++
		}
	}
	opts.logf("events: sent all %d events, closing the stream", n)
}

// handleStream is an LLM-style streaming API: it reads {"prompt": "..."} and
// streams the prompt back a word at a time, as "tokens", then [DONE].
func handleStream(opts options, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"send a JSON body with a non-empty \"prompt\""}`+"\n")
		opts.logf("stream: rejected, no usable prompt")
		return
	}

	tokens := strings.Fields(req.Prompt)
	f := beginStream(w)
	opts.logf("stream: prompt %q, streaming %d tokens", req.Prompt, len(tokens))
	tick := time.NewTicker(opts.eventInterval)
	defer tick.Stop()
	for _, tok := range tokens {
		select {
		case <-r.Context().Done():
			opts.logf("stream: client went away mid-response")
			return
		case <-tick.C:
			data, _ := json.Marshal(map[string]string{"token": tok})
			if sendEvent(w, f, "", "", string(data)) != nil {
				return
			}
		}
	}
	sendEvent(w, f, "", "", "[DONE]")
	opts.logf("stream: done")
}

// handleEndless never finishes: an event every interval until the client leaves.
func handleEndless(opts options, w http.ResponseWriter, r *http.Request) {
	f := beginStream(w)
	opts.logf("endless: stream opened; it will not end until the client leaves")
	tick := time.NewTicker(opts.eventInterval)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-r.Context().Done():
			opts.logf("endless: client left after %d events", i)
			return
		case <-tick.C:
			if sendEvent(w, f, strconv.Itoa(i), "", fmt.Sprintf(`{"n":%d}`, i)) != nil {
				return
			}
		}
	}
}

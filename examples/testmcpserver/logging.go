package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// accessLog is HTTP middleware that logs like an access log: a "call" line when
// a request arrives and a "done" line when it finishes, with status, duration,
// the JSON-RPC method and bytes written. It also counts requests per JSON-RPC
// method so summary() can print one line per interval, which is what you want
// under load.
type accessLog struct {
	logf        func(format string, args ...any) // nil = log nothing
	logRequests bool

	mu         sync.Mutex
	interval   map[string]int // finished requests per method since the last summary
	intervalEr int
	total      int
	totalEr    int
}

func newAccessLog(logf func(string, ...any), logRequests bool) *accessLog {
	return &accessLog{logf: logf, logRequests: logRequests, interval: map[string]int{}}
}

// loggingWriter records what a handler wrote. It keeps http.Flusher, which the
// event-stream replies depend on.
type loggingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	method string // the JSON-RPC method the handler served, for the log
}

func (w *loggingWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *loggingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// noteMethod records the JSON-RPC method a handler is serving, for the "done"
// line and the summary.
func noteMethod(w http.ResponseWriter, method string) {
	if lw, ok := w.(*loggingWriter); ok {
		lw.method = method
	}
}

func (a *accessLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &loggingWriter{ResponseWriter: rw, status: http.StatusOK}
		start := time.Now()
		if a.logRequests && a.logf != nil {
			a.logf("call %-6s %-8s from=%s%s", r.Method, r.URL.Path, r.RemoteAddr, authNote(r))
		}

		next.ServeHTTP(w, r)

		label := w.method
		if label == "" {
			label = r.Method + " " + r.URL.Path
		}
		// A 5xx is the server failing; a 4xx is the client sending something
		// wrong (a missing session, a bad body), which the example server uses
		// on purpose.
		failed := w.status >= 500
		a.mu.Lock()
		a.interval[label]++
		a.total++
		if failed {
			a.intervalEr++
			a.totalEr++
		}
		a.mu.Unlock()

		if a.logRequests && a.logf != nil {
			a.logf("done %-6s %-24s %d %9s bytes=%d from=%s",
				r.Method, label, w.status, time.Since(start).Round(time.Microsecond), w.bytes, r.RemoteAddr)
		}
	})
}

// authNote says whether the request carried credentials, without echoing them.
func authNote(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, _, _ := strings.Cut(h, " ")
		return " auth=" + scheme
	}
	return ""
}

// summary returns a one-line account of the requests finished since the last
// call (and resets that window), or ok=false if there were none.
func (a *accessLog) summary(window time.Duration) (line string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.interval) == 0 {
		return "", false
	}
	labels := make([]string, 0, len(a.interval))
	n := 0
	for l, c := range a.interval {
		labels = append(labels, l)
		n += c
	}
	slices.Sort(labels)
	parts := make([]string, len(labels))
	for i, l := range labels {
		parts[i] = fmt.Sprintf("%s=%d", l, a.interval[l])
	}
	line = fmt.Sprintf("stats %d requests in the last %s (%d failed): %s  | total %d (%d failed)",
		n, window, a.intervalEr, strings.Join(parts, " "), a.total, a.totalEr)
	a.interval, a.intervalEr = map[string]int{}, 0
	return line, true
}

// totals reports the cumulative request and failure counts.
func (a *accessLog) totals() (requests, failed int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total, a.totalEr
}

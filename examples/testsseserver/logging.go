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
// a request arrives (so a long-lived stream shows up the moment it opens, not
// only when it ends) and a "done" line when it finishes, with status, duration,
// events sent and bytes written. It also counts requests so summary() can
// print one line per interval, which is what you want under load.
type accessLog struct {
	logf        func(format string, args ...any) // nil = log nothing
	logRequests bool

	mu         sync.Mutex
	interval   map[string]int // finished requests per path since the last summary
	intervalEr int
	total      int
	totalEr    int
}

func newAccessLog(logf func(string, ...any), logRequests bool) *accessLog {
	return &accessLog{logf: logf, logRequests: logRequests, interval: map[string]int{}}
}

// loggingWriter records what a handler wrote. It must keep http.Flusher, which
// every SSE handler depends on.
type loggingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	events int
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

func (a *accessLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &loggingWriter{ResponseWriter: rw, status: http.StatusOK}
		start := time.Now()
		if a.logRequests && a.logf != nil {
			a.logf("call %-5s %-22s from=%s%s", r.Method, r.URL.RequestURI(), r.RemoteAddr, authNote(r))
		}

		next.ServeHTTP(w, r)

		// A 5xx is the server failing; a 4xx is the client asking for something
		// wrong, which the example server uses on purpose (e.g. a bad body).
		failed := w.status >= 500
		a.mu.Lock()
		a.interval[r.URL.Path]++
		a.total++
		if failed {
			a.intervalEr++
			a.totalEr++
		}
		a.mu.Unlock()

		if a.logRequests && a.logf != nil {
			a.logf("done %-5s %-22s %d %9s events=%d bytes=%d from=%s",
				r.Method, r.URL.RequestURI(), w.status, time.Since(start).Round(time.Microsecond), w.events, w.bytes, r.RemoteAddr)
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

// countEvent notes that a handler sent one event, for the "done" line.
func countEvent(w http.ResponseWriter) {
	if lw, ok := w.(*loggingWriter); ok {
		lw.events++
	}
}

// summary returns a one-line account of the requests finished since the last
// call (and resets that window), or ok=false if there were none.
func (a *accessLog) summary(window time.Duration) (line string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.interval) == 0 {
		return "", false
	}
	paths := make([]string, 0, len(a.interval))
	n := 0
	for p, c := range a.interval {
		paths = append(paths, p)
		n += c
	}
	slices.Sort(paths)
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = fmt.Sprintf("%s=%d", p, a.interval[p])
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

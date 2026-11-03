package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// accessLog is HTTP middleware that logs like an access log: a "call" line when a
// request arrives and a "done" line when it finishes (protocol version, status,
// duration, bytes), and counts requests, per path and per protocol version, so
// summary() can print one line per interval, which is what you want under load.
type accessLog struct {
	logf        func(format string, args ...any) // nil = log nothing
	logRequests bool

	mu         sync.Mutex
	interval   map[string]int // finished requests per path since the last summary
	protos     map[string]int // finished requests per protocol version, cumulative
	intervalEr int
	total      int
	totalEr    int
}

func newAccessLog(logf func(string, ...any), logRequests bool) *accessLog {
	return &accessLog{logf: logf, logRequests: logRequests, interval: map[string]int{}, protos: map[string]int{}}
}

type loggingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *loggingWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

func (w *loggingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Flush keeps http.Flusher, which the SSE endpoint depends on.
func (w *loggingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *accessLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &loggingWriter{ResponseWriter: rw, status: http.StatusOK}
		start := time.Now()
		if a.logRequests && a.logf != nil {
			a.logf("call %-8s %-5s %-20s from=%s", r.Proto, r.Method, r.URL.RequestURI(), r.RemoteAddr)
		}

		next.ServeHTTP(w, r)

		failed := w.status >= 500
		a.mu.Lock()
		a.interval[r.URL.Path]++
		a.protos[r.Proto]++
		a.total++
		if failed {
			a.intervalEr++
			a.totalEr++
		}
		a.mu.Unlock()

		if a.logRequests && a.logf != nil {
			a.logf("done %-8s %-5s %-20s %d %9s bytes=%d from=%s",
				r.Proto, r.Method, r.URL.RequestURI(), w.status, time.Since(start).Round(time.Microsecond), w.bytes, r.RemoteAddr)
		}
	})
}

// protoCount is how many finished requests arrived over the given protocol
// version ("HTTP/3.0", "HTTP/1.1", "HTTP/2.0").
func (a *accessLog) protoCount(proto string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.protos[proto]
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
	protos := make([]string, 0, len(a.protos))
	for p, c := range a.protos {
		protos = append(protos, fmt.Sprintf("%s=%d", p, c))
	}
	slices.Sort(protos)
	line = fmt.Sprintf("stats %d requests in the last %s (%d failed): %s  | total %d (%d failed) by protocol: %s",
		n, window, a.intervalEr, strings.Join(parts, " "), a.total, a.totalEr, strings.Join(protos, " "))
	a.interval, a.intervalEr = map[string]int{}, 0
	return line, true
}

func (a *accessLog) totals() (requests, failed int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total, a.totalEr
}

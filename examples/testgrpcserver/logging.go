package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// rpcStats is a gRPC stats.Handler: it sees every RPC and connection the
// server handles — streaming ones included, which a unary interceptor would
// miss — without any help from the handlers, so no method can go unlogged.
// Like an access log it writes a "call" line when a method is called and a
// "done" line when it finishes (status, duration, messages each way); the
// first matters for streams, which can stay open long after they arrive. It
// also logs each client connection (when logRequests is set), and counts RPCs
// so summary() can print one line per interval instead.
type rpcStats struct {
	logf        func(format string, args ...any) // nil = log nothing
	logRequests bool

	mu         sync.Mutex
	interval   map[string]int // finished RPCs per method since the last summary
	intervalEr int
	total      int
	totalEr    int
}

func newRPCStats(logf func(string, ...any), logRequests bool) *rpcStats {
	return &rpcStats{logf: logf, logRequests: logRequests, interval: map[string]int{}}
}

type (
	rpcStateKey  struct{}
	connAddrKey  struct{}
	rpcStateInfo struct {
		method  string
		in, out int // messages the server received / sent
	}
)

func (r *rpcStats) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, rpcStateKey{}, &rpcStateInfo{method: strings.TrimPrefix(info.FullMethodName, "/")})
}

func (r *rpcStats) HandleRPC(ctx context.Context, s stats.RPCStats) {
	st, _ := ctx.Value(rpcStateKey{}).(*rpcStateInfo)
	if st == nil {
		return
	}
	switch e := s.(type) {
	case *stats.Begin:
		if r.logRequests && r.logf != nil {
			r.logf("call %-38s %-16s from=%s", st.method, callKind(e), peerAddr(ctx))
		}
	case *stats.InPayload:
		st.in++
	case *stats.OutPayload:
		st.out++
	case *stats.End:
		code := status.Code(e.Error)
		// A client hanging up on a stream is how a bounded stream normally
		// ends (resonate cancels it after `receive`), not a server error.
		failed := code != codes.OK && code != codes.Canceled

		r.mu.Lock()
		r.interval[st.method]++
		r.total++
		if failed {
			r.intervalEr++
			r.totalEr++
		}
		r.mu.Unlock()

		if r.logRequests && r.logf != nil {
			line := fmt.Sprintf("done %-38s %-18s %9s  in=%d out=%d  from=%s",
				st.method, codeName(code), e.EndTime.Sub(e.BeginTime).Round(time.Microsecond), st.in, st.out, peerAddr(ctx))
			if e.Error != nil && code != codes.Canceled {
				line += fmt.Sprintf("  err=%q", status.Convert(e.Error).Message())
			}
			r.logf("%s", line)
		}
	}
}

// callKind names an RPC's streaming shape from its Begin event.
func callKind(b *stats.Begin) string {
	switch {
	case b.IsClientStream && b.IsServerStream:
		return "bidi-streaming"
	case b.IsClientStream:
		return "client-streaming"
	case b.IsServerStream:
		return "server-streaming"
	}
	return "unary"
}

// peerAddr is the calling client's address, as the server sees it.
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		return p.Addr.String()
	}
	return "?"
}

func (r *rpcStats) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return context.WithValue(ctx, connAddrKey{}, info.RemoteAddr.String())
}

func (r *rpcStats) HandleConn(ctx context.Context, s stats.ConnStats) {
	if !r.logRequests || r.logf == nil {
		return
	}
	addr, _ := ctx.Value(connAddrKey{}).(string)
	switch s.(type) {
	case *stats.ConnBegin:
		r.logf("conn connected    %s", addr)
	case *stats.ConnEnd:
		r.logf("conn disconnected %s", addr)
	}
}

// summary returns a one-line account of the RPCs finished since the last call
// (and resets that window), or ok=false if there were none — so an idle server
// stays quiet.
func (r *rpcStats) summary(window time.Duration) (line string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.interval) == 0 {
		return "", false
	}
	methods := make([]string, 0, len(r.interval))
	n := 0
	for m, c := range r.interval {
		methods = append(methods, m)
		n += c
	}
	slices.Sort(methods)
	parts := make([]string, len(methods))
	for i, m := range methods {
		parts[i] = fmt.Sprintf("%s=%d", m[strings.LastIndex(m, ".")+1:], r.interval[m])
	}
	line = fmt.Sprintf("stats %d RPCs in the last %s (%d failed): %s  | total %d (%d failed)",
		n, window, r.intervalEr, strings.Join(parts, " "), r.total, r.totalEr)
	r.interval, r.intervalEr = map[string]int{}, 0
	return line, true
}

// totals reports the cumulative RPC and failure counts.
func (r *rpcStats) totals() (rpcs, failed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total, r.totalEr
}

// codeNames are gRPC's canonical upper-snake status names, the spelling
// resonate's expect_code takes (codes.Code.String() is CamelCase, and
// converting that mechanically mangles "OK").
var codeNames = map[codes.Code]string{
	codes.OK:                 "OK",
	codes.Canceled:           "CANCELLED",
	codes.Unknown:            "UNKNOWN",
	codes.InvalidArgument:    "INVALID_ARGUMENT",
	codes.DeadlineExceeded:   "DEADLINE_EXCEEDED",
	codes.NotFound:           "NOT_FOUND",
	codes.AlreadyExists:      "ALREADY_EXISTS",
	codes.PermissionDenied:   "PERMISSION_DENIED",
	codes.ResourceExhausted:  "RESOURCE_EXHAUSTED",
	codes.FailedPrecondition: "FAILED_PRECONDITION",
	codes.Aborted:            "ABORTED",
	codes.OutOfRange:         "OUT_OF_RANGE",
	codes.Unimplemented:      "UNIMPLEMENTED",
	codes.Internal:           "INTERNAL",
	codes.Unavailable:        "UNAVAILABLE",
	codes.DataLoss:           "DATA_LOSS",
	codes.Unauthenticated:    "UNAUTHENTICATED",
}

func codeName(c codes.Code) string {
	if n, ok := codeNames[c]; ok {
		return n
	}
	return fmt.Sprintf("CODE_%d", uint32(c))
}

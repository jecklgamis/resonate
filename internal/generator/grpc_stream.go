package generator

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/jecklgamis/resonate/internal/tmpl"
)

// invokeStream runs one server-, client- or bidirectional-streaming call:
// it opens the stream, sends c.sends (c.repeat times, c.interval apart) from
// a goroutine, receives responses on this one, and decides how it ended.
//
// Results — a stream is not one request, so it is reported the way a
// WebSocket connection's messages are:
//   - one Result per received message, Latency being the gap since the
//     previous message (the first: since the stream was opened, which for a
//     client-streaming call spans all the sends — the upload's round trip);
//   - one failed Result, last, if the stream failed in a way no message
//     already shows (bad status, timeout, ended early, ...);
//   - one success Result if the stream ended cleanly having delivered no
//     messages at all, so it is still counted;
//   - the bytes sent are added to the first Result.
//
// The stream ends when: Receive messages have arrived (success); Duration
// elapses (success, unless Receive was also set); the server closes it; a
// received message fails ExpectBody; or the run itself ends (not a failure).
// With neither Receive nor Duration the deadline is the generator's Timeout,
// and hitting it is a failure — otherwise a never-ending stream would pin
// its worker for the whole run.
func (a *GRPCGenerator) invokeStream(ctx context.Context, conn *grpc.ClientConn, c *compiledGRPCCall, data tmpl.Data) []Result {
	limit, capped := c.duration, false
	if limit <= 0 {
		limit, capped = a.timeout, true
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var limitHit atomic.Bool
	limitTimer := time.AfterFunc(limit, func() {
		limitHit.Store(true)
		cancel()
	})
	defer limitTimer.Stop()

	if len(c.metadata) > 0 {
		pairs := make([]string, 0, 2*len(c.metadata))
		for k, f := range c.metadata {
			v, err := f.Render(data)
			if err != nil {
				return []Result{{Timestamp: time.Now(), Error: fmt.Errorf("rendering metadata %q: %w", k, err), Protocol: "grpc"}}
			}
			pairs = append(pairs, k, v)
		}
		streamCtx = metadata.AppendToOutgoingContext(streamCtx, pairs...)
	}

	start := time.Now()
	stream, err := conn.NewStream(streamCtx, &grpc.StreamDesc{
		StreamName:    c.method,
		ClientStreams: c.kind == kindClientStream || c.kind == kindBidi,
		ServerStreams: c.kind == kindServerStream || c.kind == kindBidi,
	}, c.path, grpc.WaitForReady(true))
	if err != nil {
		if limitHit.Load() && ctx.Err() == nil {
			err = fmt.Errorf("could not open the stream within %s", limit)
		}
		return []Result{{Timestamp: start, Latency: time.Since(start), StatusCode: httpStatusFromGRPCCode(status.Code(err)), Error: err, Protocol: "grpc"}}
	}

	// The sender renders templates concurrently with the receive loop writing
	// extracted values into data.Vars, so it works from a snapshot of the vars
	// as they were when the stream opened rather than sharing the map.
	sendData := data
	sendData.Vars = maps.Clone(data.Vars)

	var sentBytes atomic.Int64
	var sendErr atomic.Pointer[error]
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		for r := 0; r < c.repeat; r++ {
			for i := range c.sends {
				if (r > 0 || i > 0) && c.interval > 0 {
					pause := time.NewTimer(c.interval)
					select {
					case <-pause.C:
					case <-streamCtx.Done():
						pause.Stop()
						return
					}
				}
				msg, err := c.sends[i].build(c.in, sendData)
				if err != nil {
					sendErr.Store(&err)
					cancel()
					return
				}
				if err := stream.SendMsg(msg); err != nil {
					return // the stream's real status surfaces through RecvMsg
				}
				sentBytes.Add(int64(proto.Size(msg)))
			}
		}
		stream.CloseSend()
	}()
	defer func() {
		cancel() // unblocks a sender stuck on flow control or an interval
		<-senderDone
	}()

	var (
		results    []Result
		mark       = start // the last milestone: stream open, then each message
		n          int
		recvErr    error
		eof        bool
		reached    bool
		bodyFailed bool
	)
	needBody := len(c.extract) > 0 || len(c.expectBody) > 0
	for {
		msg := dynamicpb.NewMessage(c.out)
		err := stream.RecvMsg(msg)
		if err == io.EOF {
			eof = true
			break
		}
		if err != nil {
			recvErr = err
			break
		}

		now := time.Now()
		n++
		r := Result{
			Timestamp:  mark,
			Latency:    now.Sub(mark),
			StatusCode: http.StatusOK, // synthetic, as for ws: the stream's own status comes at the end
			BytesIn:    int64(proto.Size(msg)),
			Protocol:   "grpc",
		}
		mark = now

		if needBody {
			var hdr http.Header
			if md, err := stream.Header(); err == nil {
				hdr = grpcHeaders(md, nil)
			}
			if reasons := checkStreamMessage(c, msg, hdr, data); len(reasons) > 0 {
				no := false
				r.successOverride = &no
				r.Error = fmt.Errorf("check failed: %s", strings.Join(reasons, "; "))
				results = append(results, r)
				bodyFailed = true
				break
			}
		}
		results = append(results, r)
		if c.receive > 0 && n >= c.receive {
			reached = true
			break
		}
	}

	sent := sentBytes.Load()
	finish := func() []Result {
		if len(results) > 0 {
			results[0].BytesOut += sent
		}
		return results
	}
	// failure appends the stream's terminal failed Result. statusCode is 0 for
	// failures that are resonate's own verdict rather than a server status,
	// like a transport error on an HTTP request.
	failure := func(err error, statusCode int) []Result {
		results = append(results, Result{Timestamp: mark, Latency: time.Since(mark), StatusCode: statusCode, Error: err, Protocol: "grpc"})
		return finish()
	}

	switch {
	case sendErr.Load() != nil:
		return failure(*sendErr.Load(), 0)
	case bodyFailed:
		return finish()
	case reached:
		return finish()
	case runEnded(ctx):
		// The run itself ended mid-stream; that's not the server's fault.
		return finish()
	case limitHit.Load():
		switch {
		case c.receive > 0:
			return failure(fmt.Errorf("timed out after %s with %d of %d messages", limit, n, c.receive), 0)
		case capped:
			return failure(fmt.Errorf("stream still open after the %s timeout; set receive or duration to end it sooner", limit), 0)
		}
		// Duration elapsed: the normal end of a listen window.
	case recvErr != nil:
		st := status.Convert(recvErr)
		if !slices.Contains(c.expectCode, st.Code()) {
			err := recvErr
			if len(c.expectCode) > 0 {
				err = fmt.Errorf("check failed: %s", describeCodeMismatch(st, c.expectCode))
			}
			return failure(err, httpStatusFromGRPCCode(st.Code()))
		}
	case eof:
		if len(c.expectCode) > 0 && !slices.Contains(c.expectCode, codes.OK) {
			return failure(fmt.Errorf("check failed: %s", describeCodeMismatch(status.New(codes.OK, ""), c.expectCode)), http.StatusOK)
		}
		if c.receive > 0 && n < c.receive {
			return failure(fmt.Errorf("stream ended after %d of %d messages", n, c.receive), 0)
		}
	}

	if n == 0 { // a clean end with nothing received still counts as a call
		results = append(results, Result{Timestamp: start, Latency: time.Since(start), StatusCode: http.StatusOK, Protocol: "grpc"})
	}
	return finish()
}

// checkStreamMessage applies ExpectBody to one received message and, if it
// passes, Extract. It returns the reasons the message failed, if any.
func checkStreamMessage(c *compiledGRPCCall, msg proto.Message, hdr http.Header, data tmpl.Data) []string {
	bodyJSON, err := responseJSON(msg)
	if err != nil {
		return []string{fmt.Sprintf("body: %v", err)}
	}
	if hdr == nil {
		hdr = http.Header{}
	}

	rules := make([]string, 0, len(c.expectBody))
	for rule := range c.expectBody {
		rules = append(rules, rule)
	}
	slices.Sort(rules) // deterministic order in the combined error message
	var reasons []string
	for _, rule := range rules {
		want := c.expectBody[rule]
		got, err := extractValue(rule, bodyJSON, hdr, 0)
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("body %q: %v", rule, err))
		case want != "" && got != want:
			reasons = append(reasons, fmt.Sprintf("body %q: got %q, want %q", rule, got, want))
		}
	}
	if len(reasons) > 0 {
		return reasons
	}

	for name, rule := range c.extract {
		if v, err := extractValue(rule, bodyJSON, hdr, 0); err == nil {
			data.Vars[name] = v
		}
	}
	return nil
}

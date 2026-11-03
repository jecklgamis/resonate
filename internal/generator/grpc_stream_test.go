package generator

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const streamProto = `syntax = "proto3";
package test.stream.v1;
message Item { string text = 1; int32 n = 2; }
message Summary { int32 count = 1; string joined = 2; }
service Stream {
  rpc Unary(Item) returns (Item);
  rpc Slow(Item) returns (Item);                  // answers after 5s unless cancelled first
  rpc Count(Item) returns (stream Item);          // emits n items, then ends
  rpc Hold(Item) returns (stream Item);           // emits an item every 5ms, forever
  rpc Fail(Item) returns (stream Item);           // one item, then NOT_FOUND
  rpc Collect(stream Item) returns (Summary);     // counts and joins what it is sent
  rpc Chat(stream Item) returns (stream Item);    // echoes each item as "echo:<text>"
}
`

// streamServer hosts test.stream.v1.Stream — every streaming shape — with no
// generated code: handlers speak dynamicpb messages built from the schema
// compiled out of streamProto, which is also what the clients are pointed at.
type streamServer struct {
	addr      string
	protoPath string

	mu sync.Mutex
	md []metadata.MD
}

func startStreamServer(t *testing.T) *streamServer {
	t.Helper()
	dir := t.TempDir()
	protoPath := filepath.Join(dir, "stream.proto")
	if err := os.WriteFile(protoPath, []byte(streamProto), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := loadProtoSources(context.Background(), []string{protoPath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgDesc := func(name string) protoreflect.MessageDescriptor {
		d, err := resolver.FindDescriptorByName(protoreflect.FullName("test.stream.v1." + name))
		if err != nil {
			t.Fatal(err)
		}
		return d.(protoreflect.MessageDescriptor)
	}
	item, summary := msgDesc("Item"), msgDesc("Summary")
	textF, nF := item.Fields().ByName("text"), item.Fields().ByName("n")
	countF, joinedF := summary.Fields().ByName("count"), summary.Fields().ByName("joined")

	newItem := func(text string, n int32) *dynamicpb.Message {
		m := dynamicpb.NewMessage(item)
		m.Set(textF, protoreflect.ValueOfString(text))
		m.Set(nF, protoreflect.ValueOfInt32(n))
		return m
	}

	ss := &streamServer{protoPath: protoPath}
	record := func(ctx context.Context) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ss.mu.Lock()
			ss.md = append(ss.md, md)
			ss.mu.Unlock()
		}
	}
	readItem := func(stream grpc.ServerStream) (*dynamicpb.Message, error) {
		m := dynamicpb.NewMessage(item)
		return m, stream.RecvMsg(m)
	}

	desc := &grpc.ServiceDesc{
		ServiceName: "test.stream.v1.Stream",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Unary",
				Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					m := dynamicpb.NewMessage(item)
					if err := dec(m); err != nil {
						return nil, err
					}
					return m, nil
				},
			},
			{
				MethodName: "Slow",
				Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					m := dynamicpb.NewMessage(item)
					if err := dec(m); err != nil {
						return nil, err
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(5 * time.Second):
						return m, nil
					}
				},
			},
		},
		Streams: []grpc.StreamDesc{
			{StreamName: "Count", ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				record(stream.Context())
				req, err := readItem(stream)
				if err != nil {
					return err
				}
				total := req.Get(nF).Int()
				for i := int64(0); i < total; i++ {
					if err := stream.SendMsg(newItem("item-"+string(rune('0'+i%10)), int32(i))); err != nil {
						return err
					}
				}
				return nil
			}},
			{StreamName: "Hold", ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				if _, err := readItem(stream); err != nil {
					return err
				}
				tick := time.NewTicker(5 * time.Millisecond)
				defer tick.Stop()
				for i := int32(0); ; i++ {
					select {
					case <-stream.Context().Done():
						return nil
					case <-tick.C:
						if err := stream.SendMsg(newItem("tick", i)); err != nil {
							return err
						}
					}
				}
			}},
			{StreamName: "Fail", ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				if _, err := readItem(stream); err != nil {
					return err
				}
				stream.SendMsg(newItem("one", 1))
				return status.Error(codes.NotFound, "gone")
			}},
			{StreamName: "Collect", ClientStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				record(stream.Context())
				var texts []string
				for {
					m, err := readItem(stream)
					if err == io.EOF {
						break
					}
					if err != nil {
						return err
					}
					texts = append(texts, m.Get(textF).String())
				}
				out := dynamicpb.NewMessage(summary)
				out.Set(countF, protoreflect.ValueOfInt32(int32(len(texts))))
				out.Set(joinedF, protoreflect.ValueOfString(strings.Join(texts, ",")))
				return stream.SendMsg(out)
			}},
			{StreamName: "Chat", ClientStreams: true, ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				for {
					m, err := readItem(stream)
					if err == io.EOF {
						return nil
					}
					if err != nil {
						return err
					}
					if err := stream.SendMsg(newItem("echo:"+m.Get(textF).String(), int32(m.Get(nF).Int()))); err != nil {
						return err
					}
				}
			}},
		},
	}

	srv := grpc.NewServer()
	srv.RegisterService(desc, struct{}{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	ss.addr = lis.Addr().String()
	return ss
}

func newStreamGen(t *testing.T, ss *streamServer, mutate func(*GRPCOptions), calls ...GRPCCall) *GRPCGenerator {
	t.Helper()
	opts := plainOpts()
	opts.ProtoFiles = []string{ss.protoPath}
	if mutate != nil {
		mutate(&opts)
	}
	g, err := NewGRPCGenerator(GRPCTarget{Address: ss.addr, Calls: calls}, opts)
	if err != nil {
		t.Fatalf("NewGRPCGenerator: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func allOK(t *testing.T, rs []Result) {
	t.Helper()
	for i, r := range rs {
		if !r.Success() {
			t.Fatalf("result %d failed: %+v", i, r)
		}
	}
}

func lastErr(t *testing.T, rs []Result) string {
	t.Helper()
	if len(rs) == 0 || rs[len(rs)-1].Success() || rs[len(rs)-1].Error == nil {
		t.Fatalf("want the last result to be a failure, got %+v", rs)
	}
	return rs[len(rs)-1].Error.Error()
}

func TestGRPCServerStreamingReadsUntilServerCloses(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Count", Message: `{"n":3}`})

	res := g.Do(context.Background(), 0)
	if len(res) != 3 {
		t.Fatalf("want one result per message (3), got %d: %+v", len(res), res)
	}
	allOK(t, res)
	for i, r := range res {
		if r.Protocol != "grpc" || r.StatusCode != 200 || r.BytesIn == 0 {
			t.Fatalf("result %d malformed: %+v", i, r)
		}
	}
	// {"n":3} is 2 bytes on the wire, attributed to the first result only.
	if res[0].BytesOut != 2 || res[1].BytesOut != 0 || res[2].BytesOut != 0 {
		t.Fatalf("BytesOut = %d/%d/%d, want 2/0/0", res[0].BytesOut, res[1].BytesOut, res[2].BytesOut)
	}
}

func TestGRPCServerStreamingReceiveEndsEarly(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Hold", Message: `{}`, Receive: 3})

	start := time.Now()
	res := g.Do(context.Background(), 0)
	if len(res) != 3 {
		t.Fatalf("want exactly 3 messages, got %d: %+v", len(res), res)
	}
	allOK(t, res)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("an endless stream with receive: 3 took %s", time.Since(start))
	}
}

func TestGRPCServerStreamingEndedBeforeReceiveIsAFailure(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Count", Message: `{"n":2}`, Receive: 5})

	res := g.Do(context.Background(), 0)
	if len(res) != 3 { // 2 messages + the failure
		t.Fatalf("want 2 messages + 1 failure, got %d: %+v", len(res), res)
	}
	if msg := lastErr(t, res); !strings.Contains(msg, "stream ended after 2 of 5 messages") {
		t.Fatalf("unexpected failure: %s", msg)
	}
}

func TestGRPCStreamingDurationEndsNormally(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Hold", Message: `{}`, Duration: 150 * time.Millisecond})

	start := time.Now()
	res := g.Do(context.Background(), 0)
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("listened for %s, want ~150ms", elapsed)
	}
	if len(res) < 3 {
		t.Fatalf("expected several messages in 150ms, got %d", len(res))
	}
	allOK(t, res) // a listen window elapsing is not a failure
}

func TestGRPCStreamingEndlessWithNoBoundFails(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, func(o *GRPCOptions) { o.Timeout = 150 * time.Millisecond },
		GRPCCall{Method: "test.stream.v1.Stream/Hold", Message: `{}`})

	if msg := lastErr(t, g.Do(context.Background(), 0)); !strings.Contains(msg, "set receive or duration") {
		t.Fatalf("want an unbounded-stream failure pointing at receive/duration, got %s", msg)
	}
}

func TestGRPCStreamingReceiveDeadline(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Hold", Message: `{}`, Receive: 100000, Duration: 100 * time.Millisecond})

	if msg := lastErr(t, g.Do(context.Background(), 0)); !strings.Contains(msg, "timed out after 100ms with") || !strings.Contains(msg, "of 100000 messages") {
		t.Fatalf("unexpected failure: %s", msg)
	}
}

func TestGRPCStreamingServerErrorStatus(t *testing.T) {
	ss := startStreamServer(t)

	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Fail", Message: `{}`})
	res := g.Do(context.Background(), 0)
	if len(res) != 2 || !res[0].Success() || res[1].Success() {
		t.Fatalf("want one good message then a failure, got %+v", res)
	}
	if res[1].StatusCode != 404 || !strings.Contains(res[1].Error.Error(), "NotFound") {
		t.Fatalf("failure should carry NOT_FOUND (404), got %+v", res[1])
	}

	// An expected terminal status is a success.
	g2 := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Fail", Message: `{}`, ExpectCode: []string{"NOT_FOUND"}})
	res = g2.Do(context.Background(), 0)
	if len(res) != 1 {
		t.Fatalf("want just the one message, got %+v", res)
	}
	allOK(t, res)

	// And a stream that ends OK when something else was expected is not.
	g3 := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Count", Message: `{"n":1}`, ExpectCode: []string{"UNAVAILABLE"}})
	if msg := lastErr(t, g3.Do(context.Background(), 0)); !strings.Contains(msg, "got OK") || !strings.Contains(msg, "UNAVAILABLE") {
		t.Fatalf("unexpected failure: %s", msg)
	}
}

func TestGRPCStreamingExpectBodyAppliesToEveryMessage(t *testing.T) {
	ss := startStreamServer(t)

	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Count", Message: `{"n":3}`, ExpectBody: map[string]string{"json:text": ""}})
	res := g.Do(context.Background(), 0)
	if len(res) != 3 {
		t.Fatalf("want 3 passing messages, got %+v", res)
	}
	allOK(t, res)

	// item-0 matches, item-1 doesn't: the stream stops at the first mismatch.
	g2 := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Count", Message: `{"n":3}`, ExpectBody: map[string]string{"json:text": "item-0"}})
	res = g2.Do(context.Background(), 0)
	if len(res) != 2 || !res[0].Success() || res[1].Success() {
		t.Fatalf("want message 1 ok, message 2 failed, then stop; got %+v", res)
	}
	if !strings.Contains(res[1].Error.Error(), `got "item-1", want "item-0"`) {
		t.Fatalf("unexpected failure: %v", res[1].Error)
	}
}

func TestGRPCStreamingExtractKeepsLastValueAndChains(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil,
		GRPCCall{Method: "test.stream.v1.Stream/Count", Message: `{"n":3}`, Extract: map[string]string{"last": "json:text"}},
		GRPCCall{Method: "test.stream.v1.Stream/Unary", Message: `{"text":"{{.Vars.last}}"}`, ExpectBody: map[string]string{"json:text": "item-2"}})

	res := g.Do(context.Background(), 0)
	if len(res) != 4 { // 3 stream messages + 1 unary
		t.Fatalf("want 4 results, got %d: %+v", len(res), res)
	}
	allOK(t, res)
}

func TestGRPCClientStreaming(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{
		Method:     "test.stream.v1.Stream/Collect",
		Messages:   []string{`{"text":"a"}`, `{"text":"vu{{.VU}}"}`},
		Repeat:     2,
		ExpectBody: map[string]string{"json:count": "4", "json:joined": "a,vu7,a,vu7"},
	})

	res := g.Do(context.Background(), 7)
	if len(res) != 1 {
		t.Fatalf("want the single response as one result, got %d: %+v", len(res), res)
	}
	allOK(t, res)
	// Item{text:"a"} is 3 bytes, Item{text:"vu7"} is 5: (3+5) * 2.
	if res[0].BytesOut != 16 {
		t.Fatalf("BytesOut = %d, want 16", res[0].BytesOut)
	}
}

func TestGRPCBidirectionalStreaming(t *testing.T) {
	ss := startStreamServer(t)
	msgs := []string{`{"text":"a"}`, `{"text":"b"}`, `{"text":"c"}`}

	t.Run("until the server closes", func(t *testing.T) {
		g := newStreamGen(t, ss, nil, GRPCCall{
			Method: "test.stream.v1.Stream/Chat", Messages: msgs,
			ExpectBody: map[string]string{"regex:echo:(.)": ""},
		})
		res := g.Do(context.Background(), 0)
		if len(res) != 3 {
			t.Fatalf("want 3 echoes, got %d: %+v", len(res), res)
		}
		allOK(t, res)
		if res[0].BytesOut != 9 { // three 3-byte Items, on the first result
			t.Fatalf("BytesOut = %d, want 9", res[0].BytesOut)
		}
	})

	t.Run("receive ends it early", func(t *testing.T) {
		g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Chat", Messages: msgs, Receive: 2})
		res := g.Do(context.Background(), 0)
		if len(res) != 2 {
			t.Fatalf("want exactly 2 messages, got %d: %+v", len(res), res)
		}
		allOK(t, res)
	})

	t.Run("send_interval paces the sends", func(t *testing.T) {
		g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Chat", Messages: msgs, SendInterval: 40 * time.Millisecond})
		start := time.Now()
		res := g.Do(context.Background(), 0)
		if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
			t.Fatalf("3 sends 40ms apart finished in %s", elapsed)
		}
		if len(res) != 3 {
			t.Fatalf("want 3 echoes, got %+v", res)
		}
		allOK(t, res)
	})
}

func TestGRPCStreamingSendRenderErrorFailsTheCall(t *testing.T) {
	ss := startStreamServer(t)
	// Renders to a number where the schema wants a string.
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Collect", Messages: []string{`{"text": {{.VU}} }`}})

	res := g.Do(context.Background(), 0)
	if msg := lastErr(t, res); !strings.Contains(msg, "parsing message") {
		t.Fatalf("unexpected failure: %s", msg)
	}
}

func TestGRPCStreamingMetadataAndFailureStopsIteration(t *testing.T) {
	ss := startStreamServer(t)
	opts := func(o *GRPCOptions) { o.Identities = []map[string]string{{"token": "tok"}} }

	g, err := NewGRPCGenerator(GRPCTarget{
		Address:  ss.addr,
		Metadata: map[string]string{"authorization": "Bearer {{.Identity.token}}"},
		Calls: []GRPCCall{
			{Method: "test.stream.v1.Stream/Count", Message: `{"n":1}`, ExpectBody: map[string]string{"json:text": "nope"}},
			{Method: "test.stream.v1.Stream/Count", Message: `{"n":1}`},
		},
	}, func() GRPCOptions { o := plainOpts(); o.ProtoFiles = []string{ss.protoPath}; opts(&o); return o }())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() {
		t.Fatalf("a failed stream should end the iteration, got %+v", res)
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.md) != 1 || ss.md[0].Get("authorization")[0] != "Bearer tok" {
		t.Fatalf("server saw metadata %v (and the second call must never run)", ss.md)
	}
}

func TestGRPCStreamingRunEndingMidStreamIsNotAFailure(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Hold", Message: `{}`, Receive: 100000})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := g.Do(ctx, 0)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Do didn't return promptly once the run ended: %s", time.Since(start))
	}
	allOK(t, res)
}

func TestGRPCStreamingKindValidation(t *testing.T) {
	ss := startStreamServer(t)
	const svc = "test.stream.v1.Stream/"
	cases := map[string]struct {
		call GRPCCall
		want string
	}{
		"unary with receive":            {GRPCCall{Method: svc + "Unary", Receive: 1}, "unary RPC"},
		"unary with messages":           {GRPCCall{Method: svc + "Unary", Messages: []string{"{}"}}, "unary RPC"},
		"unary with duration":           {GRPCCall{Method: svc + "Unary", Duration: time.Second}, "unary RPC"},
		"server stream with messages":   {GRPCCall{Method: svc + "Count", Messages: []string{"{}"}}, "server-streaming"},
		"server stream with repeat":     {GRPCCall{Method: svc + "Count", Repeat: 2}, "server-streaming"},
		"client stream with message":    {GRPCCall{Method: svc + "Collect", Message: "{}", Messages: []string{"{}"}}, "as messages"},
		"client stream with receive":    {GRPCCall{Method: svc + "Collect", Messages: []string{"{}"}, Receive: 1}, "receive doesn't apply"},
		"client stream without send":    {GRPCCall{Method: svc + "Collect"}, "at least one entry"},
		"bidi with message":             {GRPCCall{Method: svc + "Chat", Message: "{}"}, "as messages"},
		"negative receive":              {GRPCCall{Method: svc + "Count", Receive: -1}, "negative"},
		"negative interval":             {GRPCCall{Method: svc + "Chat", SendInterval: -time.Second}, "negative"},
		"malformed static stream entry": {GRPCCall{Method: svc + "Chat", Messages: []string{`{"bogus":1}`}}, "messages[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := plainOpts()
			opts.ProtoFiles = []string{ss.protoPath}
			g, err := NewGRPCGenerator(GRPCTarget{Address: ss.addr, Calls: []GRPCCall{tc.call}}, opts)
			if err == nil {
				g.Close()
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q doesn't contain %q", err, tc.want)
			}
		})
	}
}

func TestGRPCStreamingConcurrentUseIsSafeAndDoesNotLeak(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil,
		GRPCCall{Method: "test.stream.v1.Stream/Count", Message: `{"n":2}`, Extract: map[string]string{"t": "json:text"}},
		GRPCCall{Method: "test.stream.v1.Stream/Chat", Messages: []string{`{"text":"{{.Vars.t}}-{{.VU}}"}`, `{"text":"static"}`}, Repeat: 2})

	// Settle gRPC's own background goroutines before counting.
	g.Do(context.Background(), 0)
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for vu := 0; vu < 8; vu++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				for _, r := range g.Do(context.Background(), vu) {
					if !r.Success() {
						t.Errorf("vu %d: %+v", vu, r)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Fatalf("goroutines grew from %d to %d after 80 streams: the sender is leaking", before, after)
	}
}

func TestGRPCBidiExtractWhileSendingDoesNotRace(t *testing.T) {
	ss := startStreamServer(t)
	// The receive loop writes {{.Vars.last}} (extract) while the sender is
	// still rendering messages that read it: only safe because the sender
	// works from a snapshot of the vars.
	g := newStreamGen(t, ss, nil, GRPCCall{
		Method:       "test.stream.v1.Stream/Chat",
		Messages:     []string{`{"text":"{{.Vars.last}}"}`},
		Repeat:       30,
		SendInterval: time.Millisecond,
		Extract:      map[string]string{"last": "json:text"},
	})
	allOK(t, g.Do(context.Background(), 0))
}

func TestGRPCUnaryCallInterruptedByRunEndingIsNotAFailure(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil,
		GRPCCall{Method: "test.stream.v1.Stream/Unary", Message: `{"text":"fast"}`},
		GRPCCall{Method: "test.stream.v1.Stream/Slow", Message: `{"text":"slow"}`})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := g.Do(ctx, 0)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Do didn't return promptly once the run ended: %s", time.Since(start))
	}
	// The completed fast call is reported; the one the run's end cut short is not.
	if len(res) != 1 {
		t.Fatalf("want only the completed call's result, got %d: %+v", len(res), res)
	}
	allOK(t, res)

	// A genuine failure on a live run is still reported.
	g2 := newStreamGen(t, ss, func(o *GRPCOptions) { o.Timeout = 100 * time.Millisecond },
		GRPCCall{Method: "test.stream.v1.Stream/Slow", Message: `{"text":"slow"}`})
	res = g2.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() {
		t.Fatalf("a call exceeding its own timeout on a live run must fail, got %+v", res)
	}
}

// lateCtx reports a deadline in the past while its Err() is still nil: the
// window between a context's deadline passing and its timer marking it done.
type lateCtx struct {
	context.Context
	deadline time.Time
}

func (c lateCtx) Deadline() (time.Time, bool) { return c.deadline, true }

func TestRunEndedSeesADeadlinePassedBeforeTheContextIsMarkedDone(t *testing.T) {
	past := lateCtx{context.Background(), time.Now().Add(-time.Millisecond)}
	if past.Err() != nil {
		t.Fatal("test premise: Err() must still be nil")
	}
	if !runEnded(past) {
		t.Fatal("a passed deadline must count as the run having ended, even before ctx.Err() is set")
	}
	if runEnded(lateCtx{context.Background(), time.Now().Add(time.Hour)}) {
		t.Fatal("a future deadline must not count as ended")
	}
	if runEnded(context.Background()) {
		t.Fatal("a context with no deadline must not count as ended")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !runEnded(cancelled) {
		t.Fatal("a cancelled context must count as ended")
	}
}

func TestGRPCUnaryCallFailingAtTheDeadlineInstantIsNotReported(t *testing.T) {
	ss := startStreamServer(t)
	g := newStreamGen(t, ss, nil, GRPCCall{Method: "test.stream.v1.Stream/Slow", Message: `{"text":"x"}`})

	// The deadline has passed but Err() is nil, so gRPC fails the call on its
	// own clock check — the exact shape of the end-of-run burst.
	for _, r := range g.Do(lateCtx{context.Background(), time.Now().Add(-time.Millisecond)}, 0) {
		if !r.Success() {
			t.Fatalf("a call cut off by the run's deadline was reported as a failure: %+v", r)
		}
	}
}

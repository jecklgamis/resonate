// Command testgrpcserver is a small gRPC server for trying resonate's gRPC
// examples without any service of your own. It implements the two services
// the example scenarios call:
//
//	acme.user.v1.UserService  (unary)       examples/grpc-basic.yaml, grpc-proto-files.yaml
//	acme.feed.v1.Feed         (streaming)   examples/grpc-streaming.yaml
//
// from the .proto files in examples/proto, with no generated code: messages
// are dynamic, built from the schemas compiled at startup. State is in memory
// and grows with load; it is a demo target, not a model of a real service.
//
//	go run ./examples/testgrpcserver                    # :50051, reflection on
//	go run ./examples/testgrpcserver -reflection=false  # for the proto_files example
//
// It logs what it does: startup, each client connection, each finished RPC
// (method, status, duration, messages in/out, client), and a few lines about
// what the calls did (users created, streams opened and closed). That is a
// lot of output under load, so for a performance run use -log-requests=false,
// which leaves a one-line summary every -stats-interval instead.
//
// See the usage comments at the top of each example scenario.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	v1reflection "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflection "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	exampleprotos "github.com/jecklgamis/resonate/examples/proto"
)

// options configures the server.
type options struct {
	// reflection serves gRPC server reflection, which is how
	// examples/grpc-basic.yaml learns the schema. Turn it off to try the
	// proto_files / descriptor_set examples, which exist for servers without it.
	reflection bool

	// eventInterval is the gap between Feed/Subscribe events.
	eventInterval time.Duration

	// logger receives the server's log lines; nil logs nothing.
	logger *log.Logger

	// logRequests logs every connection and finished RPC, plus what the
	// handlers did. Off, only the periodic summary line (see rpcStats) is
	// printed — the setting for load tests, where per-request logging would
	// dominate the server's cost.
	logRequests bool
}

// logf logs a request-level line, if request logging is on.
func (o options) logf(format string, args ...any) {
	if o.logRequests && o.logger != nil {
		o.logger.Printf(format, args...)
	}
}

// exampleServer is the gRPC server plus the counters behind its summary line.
type exampleServer struct {
	*grpc.Server
	rpcs *rpcStats
}

func main() {
	addr := flag.String("addr", ":50051", "address to listen on")
	reflect := flag.Bool("reflection", true, "serve gRPC server reflection (turn off to try the proto_files example)")
	interval := flag.Duration("event-interval", 20*time.Millisecond, "gap between Feed/Subscribe events")
	logRequests := flag.Bool("log-requests", true, "log every connection and finished RPC (turn off for load tests)")
	statsEvery := flag.Duration("stats-interval", 10*time.Second, "print a one-line RPC summary this often when there was activity (0 disables)")
	flag.Parse()

	srv, err := newServer(options{
		reflection:    *reflect,
		eventInterval: *interval,
		logger:        log.Default(),
		logRequests:   *logRequests,
	})
	if err != nil {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}

	if *statsEvery > 0 {
		go func() {
			for range time.Tick(*statsEvery) {
				if line, ok := srv.rpcs.summary(*statsEvery); ok {
					log.Print(line)
				}
			}
		}()
	}

	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		sig := <-stop
		rpcs, failed := srv.rpcs.totals()
		log.Printf("received %v: shutting down after %d RPCs (%d failed)", sig, rpcs, failed)
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second): // streams may never end on their own
			srv.Stop()
		}
	}()

	log.Printf("example gRPC server listening on %s", lis.Addr())
	log.Printf("config: reflection=%v event-interval=%s log-requests=%v stats-interval=%s", *reflect, *interval, *logRequests, *statsEvery)
	log.Println("services: acme.user.v1.UserService (CreateUser, GetUser), acme.feed.v1.Feed (Subscribe, Upload, Chat)")
	if !*logRequests {
		log.Println("per-request logging is off; a summary line is printed every", *statsEvery, "while there is traffic")
	}
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

// compileSchemas compiles the embedded example .proto files.
func compileSchemas(ctx context.Context) (linker.Files, error) {
	srcs := map[string]string{}
	var names []string
	entries, err := fs.ReadDir(exampleprotos.FS, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".proto") {
			continue
		}
		b, err := fs.ReadFile(exampleprotos.FS, e.Name())
		if err != nil {
			return nil, err
		}
		srcs[e.Name()] = string(b)
		names = append(names, e.Name())
	}
	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: protocompile.SourceAccessorFromMap(srcs),
		}),
	}
	return compiler.Compile(ctx, names...)
}

// newServer builds the gRPC server with both services registered.
func newServer(opts options) (*exampleServer, error) {
	files, err := compileSchemas(context.Background())
	if err != nil {
		return nil, fmt.Errorf("compiling example protos: %w", err)
	}
	resolver := files.AsResolver()

	var lookupErr error
	msg := func(name string) protoreflect.MessageDescriptor {
		d, err := resolver.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			lookupErr = err
			return nil
		}
		return d.(protoreflect.MessageDescriptor)
	}
	userD, createReqD, getReqD := msg("acme.user.v1.User"), msg("acme.user.v1.CreateUserRequest"), msg("acme.user.v1.GetUserRequest")
	subReqD, eventD := msg("acme.feed.v1.SubscribeRequest"), msg("acme.feed.v1.Event")
	itemD, summaryD, chatD := msg("acme.feed.v1.UploadItem"), msg("acme.feed.v1.UploadSummary"), msg("acme.feed.v1.ChatMessage")
	if lookupErr != nil {
		return nil, lookupErr
	}

	var logf func(string, ...any)
	if opts.logger != nil {
		logf = opts.logger.Printf
	}
	rpcs := newRPCStats(logf, opts.logRequests)
	srv := grpc.NewServer(grpc.StatsHandler(rpcs))
	srv.RegisterService(userService(opts, userD, createReqD, getReqD), struct{}{})
	srv.RegisterService(feedService(opts, subReqD, eventD, itemD, summaryD, chatD), struct{}{})

	if opts.reflection {
		// The default reflection answers from the global proto registry, which
		// doesn't know these dynamically compiled schemas; hand it ours.
		ro := reflection.ServerOptions{Services: srv, DescriptorResolver: resolver}
		v1reflection.RegisterServerReflectionServer(srv, reflection.NewServerV1(ro))
		v1alphareflection.RegisterServerReflectionServer(srv, reflection.NewServer(ro))
	}
	return &exampleServer{Server: srv, rpcs: rpcs}, nil
}

// userService is acme.user.v1.UserService: an in-memory user store.
func userService(opts options, userD, createReqD, getReqD protoreflect.MessageDescriptor) *grpc.ServiceDesc {
	var (
		mu    sync.RWMutex
		users = map[string]*dynamicpb.Message{}
		next  atomic.Int64
	)
	uID, uName, uAge, uCreated := userD.Fields().ByName("id"), userD.Fields().ByName("name"), userD.Fields().ByName("age"), userD.Fields().ByName("created_at")
	cName, cAge := createReqD.Fields().ByName("name"), createReqD.Fields().ByName("age")
	gID := getReqD.Fields().ByName("id")
	tsD := uCreated.Message()

	return &grpc.ServiceDesc{
		ServiceName: "acme.user.v1.UserService",
		HandlerType: (*any)(nil),
		Metadata:    "user.proto",
		Methods: []grpc.MethodDesc{
			{
				MethodName: "CreateUser",
				Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					req := dynamicpb.NewMessage(createReqD)
					if err := dec(req); err != nil {
						return nil, err
					}
					name, age := req.Get(cName).String(), int32(req.Get(cAge).Int())
					if strings.TrimSpace(name) == "" {
						opts.logf("user rejected: name is required")
						return nil, status.Error(codes.InvalidArgument, "name is required")
					}
					if age < 0 {
						opts.logf("user rejected: negative age %d", age)
						return nil, status.Error(codes.InvalidArgument, "age must not be negative")
					}

					now := time.Now()
					ts := dynamicpb.NewMessage(tsD)
					ts.Set(tsD.Fields().ByName("seconds"), protoreflect.ValueOfInt64(now.Unix()))
					ts.Set(tsD.Fields().ByName("nanos"), protoreflect.ValueOfInt32(int32(now.Nanosecond())))

					id := fmt.Sprintf("user-%d", next.Add(1))
					u := dynamicpb.NewMessage(userD)
					u.Set(uID, protoreflect.ValueOfString(id))
					u.Set(uName, protoreflect.ValueOfString(name))
					u.Set(uAge, protoreflect.ValueOfInt32(age))
					u.Set(uCreated, protoreflect.ValueOfMessage(ts))

					mu.Lock()
					users[id] = u
					total := len(users)
					mu.Unlock()
					opts.logf("user created %s name=%q age=%d (%d users stored)", id, name, age, total)
					return u, nil
				},
			},
			{
				MethodName: "GetUser",
				Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					req := dynamicpb.NewMessage(getReqD)
					if err := dec(req); err != nil {
						return nil, err
					}
					id := req.Get(gID).String()
					mu.RLock()
					u, ok := users[id]
					mu.RUnlock()
					if !ok {
						opts.logf("user lookup missed id=%q", id)
						return nil, status.Errorf(codes.NotFound, "user %q not found", id)
					}
					return u, nil
				},
			},
		},
	}
}

// feedService is acme.feed.v1.Feed: one method for each streaming shape.
func feedService(opts options, subReqD, eventD, itemD, summaryD, chatD protoreflect.MessageDescriptor) *grpc.ServiceDesc {
	interval := opts.eventInterval
	topicF := subReqD.Fields().ByName("topic")
	evTopic, evID, evPayload := eventD.Fields().ByName("topic"), eventD.Fields().ByName("id"), eventD.Fields().ByName("payload")
	itemPayload := itemD.Fields().ByName("payload")
	sumAccepted, sumBytes := summaryD.Fields().ByName("accepted"), summaryD.Fields().ByName("total_bytes")
	chatText := chatD.Fields().ByName("text")

	chat := func(text string) *dynamicpb.Message {
		m := dynamicpb.NewMessage(chatD)
		m.Set(chatText, protoreflect.ValueOfString(text))
		return m
	}

	return &grpc.ServiceDesc{
		ServiceName: "acme.feed.v1.Feed",
		HandlerType: (*any)(nil),
		Metadata:    "feed.proto",
		Streams: []grpc.StreamDesc{
			{
				// Server streaming: an event every `interval`, forever.
				StreamName: "Subscribe", ServerStreams: true,
				Handler: func(_ any, stream grpc.ServerStream) error {
					req := dynamicpb.NewMessage(subReqD)
					if err := stream.RecvMsg(req); err != nil {
						return err
					}
					topic := req.Get(topicF).String()
					if topic == "" {
						opts.logf("subscribe rejected: topic is required")
						return status.Error(codes.InvalidArgument, "topic is required")
					}
					opts.logf("subscribe topic=%q opened (an event every %s until the client stops)", topic, interval)
					tick := time.NewTicker(interval)
					defer tick.Stop()
					for id := int64(1); ; id++ {
						select {
						case <-stream.Context().Done():
							opts.logf("subscribe topic=%q closed after %d events", topic, id-1)
							return nil
						case <-tick.C:
							ev := dynamicpb.NewMessage(eventD)
							ev.Set(evTopic, protoreflect.ValueOfString(topic))
							ev.Set(evID, protoreflect.ValueOfInt64(id))
							ev.Set(evPayload, protoreflect.ValueOfString(fmt.Sprintf("%s event %d", topic, id)))
							if err := stream.SendMsg(ev); err != nil {
								return err
							}
						}
					}
				},
			},
			{
				// Client streaming: count what arrives, answer once at the end.
				StreamName: "Upload", ClientStreams: true,
				Handler: func(_ any, stream grpc.ServerStream) error {
					var accepted int32
					var total int64
					for {
						item := dynamicpb.NewMessage(itemD)
						err := stream.RecvMsg(item)
						if err == io.EOF {
							break
						}
						if err != nil {
							return err
						}
						accepted++
						total += int64(len(item.Get(itemPayload).String()))
					}
					opts.logf("upload received %d items, %d payload bytes", accepted, total)
					out := dynamicpb.NewMessage(summaryD)
					out.Set(sumAccepted, protoreflect.ValueOfInt32(accepted))
					out.Set(sumBytes, protoreflect.ValueOfInt64(total))
					return stream.SendMsg(out)
				},
			},
			{
				// Bidirectional: echo each message; when the client is done
				// sending, say goodbye (so n messages in give n+1 out).
				StreamName: "Chat", ClientStreams: true, ServerStreams: true,
				Handler: func(_ any, stream grpc.ServerStream) error {
					var n int
					for {
						in := dynamicpb.NewMessage(chatD)
						err := stream.RecvMsg(in)
						if err == io.EOF {
							opts.logf("chat client finished sending; saying goodbye after %d messages", n)
							return stream.SendMsg(chat(fmt.Sprintf("bye: received %d messages", n)))
						}
						if err != nil {
							return err
						}
						n++
						if err := stream.SendMsg(chat("echo: " + in.Get(chatText).String())); err != nil {
							return err
						}
					}
				},
			},
		},
	}
}

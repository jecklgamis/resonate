package generator

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"
	v1alphagrpc "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

const healthCheck = "grpc.health.v1.Health/Check"

// grpcTestServer is an in-process gRPC server running grpc-go's standard
// health service — a real unary RPC ("up" is SERVING, "down" NOT_SERVING,
// anything else NOT_FOUND) with a streaming sibling (Watch) and no codegen
// needed — recording what each call carried.
type grpcTestServer struct {
	addr string
	srv  *grpc.Server

	mu       sync.Mutex
	md       []metadata.MD
	services []string // the "service" field of each Check request
	peers    map[string]bool
}

type grpcServerConfig struct {
	reflection string // "v1+v1alpha" (default), "v1alpha", or "none"
	creds      credentials.TransportCredentials
}

func startGRPCServer(t *testing.T, cfg grpcServerConfig) *grpcTestServer {
	t.Helper()
	ts := &grpcTestServer{peers: map[string]bool{}}

	opts := []grpc.ServerOption{grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		ts.mu.Lock()
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ts.md = append(ts.md, md)
		}
		if p, ok := peer.FromContext(ctx); ok {
			ts.peers[p.Addr.String()] = true
		}
		if r, ok := req.(*grpc_health_v1.HealthCheckRequest); ok {
			ts.services = append(ts.services, r.GetService())
		}
		ts.mu.Unlock()
		return h(ctx, req)
	})}
	if cfg.creds != nil {
		opts = append(opts, grpc.Creds(cfg.creds))
	}
	srv := grpc.NewServer(opts...)

	hs := health.NewServer()
	hs.SetServingStatus("up", grpc_health_v1.HealthCheckResponse_SERVING)
	hs.SetServingStatus("down", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, hs)

	switch cfg.reflection {
	case "none":
	case "v1alpha":
		v1alphagrpc.RegisterServerReflectionServer(srv, reflection.NewServer(reflection.ServerOptions{Services: srv}))
	default:
		reflection.Register(srv)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	ts.addr, ts.srv = lis.Addr().String(), srv
	return ts
}

func (s *grpcTestServer) seenServices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.services...)
}

func plainOpts() GRPCOptions {
	o := DefaultGRPCOptions()
	o.Plaintext = true
	o.Timeout = 3 * time.Second
	return o
}

func newGRPCGen(t *testing.T, ts *grpcTestServer, opts GRPCOptions, calls ...GRPCCall) *GRPCGenerator {
	t.Helper()
	g, err := NewGRPCGenerator(GRPCTarget{Address: ts.addr, Calls: calls}, opts)
	if err != nil {
		t.Fatalf("NewGRPCGenerator: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func TestGRPCGeneratorUnaryCallViaReflection(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	g := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, Message: `{"service":"up"}`})

	res := g.Do(context.Background(), 0)
	if len(res) != 1 || !res[0].Success() {
		t.Fatalf("want one successful result, got %+v", res)
	}
	r := res[0]
	if r.Protocol != "grpc" || r.StatusCode != 200 || r.Latency <= 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	// HealthCheckRequest{service:"up"} is 4 bytes on the wire; the
	// SERVING response is 2 (tag + enum value).
	if r.BytesOut != 4 || r.BytesIn != 2 {
		t.Fatalf("BytesOut=%d BytesIn=%d, want 4 and 2", r.BytesOut, r.BytesIn)
	}
}

func TestGRPCGeneratorFailedStatusIsAFailure(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	g := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, Message: `{"service":"nope"}`})

	res := g.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() {
		t.Fatalf("want one failed result, got %+v", res)
	}
	if res[0].StatusCode != 404 {
		t.Fatalf("NOT_FOUND should report as HTTP 404, got %d", res[0].StatusCode)
	}
	if res[0].Error == nil || !strings.Contains(res[0].Error.Error(), "NotFound") {
		t.Fatalf("Error should carry the real gRPC code, got %v", res[0].Error)
	}
}

func TestGRPCGeneratorExpectCode(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})

	// An expected NOT_FOUND is a success...
	g := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, Message: `{"service":"nope"}`, ExpectCode: []string{"not_found"}})
	if res := g.Do(context.Background(), 0); !res[0].Success() || res[0].StatusCode != 404 {
		t.Fatalf("expected NOT_FOUND should succeed (as 404), got %+v", res[0])
	}

	// ...and an unexpected OK is a failure that names both sides.
	g2 := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, Message: `{"service":"up"}`, ExpectCode: []string{"NOT_FOUND", "UNAVAILABLE"}})
	res := g2.Do(context.Background(), 0)
	if res[0].Success() || res[0].Error == nil || !strings.Contains(res[0].Error.Error(), "got OK") || !strings.Contains(res[0].Error.Error(), "NOT_FOUND") {
		t.Fatalf("want a code-mismatch failure, got %+v", res[0])
	}

	// A failing code is also reported (with its message) when only expect_body is set.
	g3 := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, Message: `{"service":"nope"}`, ExpectBody: map[string]string{"json:status": ""}})
	res = g3.Do(context.Background(), 0)
	if res[0].Success() || !strings.Contains(res[0].Error.Error(), "NOT_FOUND") {
		t.Fatalf("expect_body alone should still require OK, got %+v", res[0])
	}
}

func TestGRPCGeneratorExpectBody(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})

	g := newGRPCGen(t, ts, plainOpts(),
		GRPCCall{Method: healthCheck, Message: `{"service":"up"}`, ExpectBody: map[string]string{"json:status": "SERVING"}})
	if res := g.Do(context.Background(), 0); !res[0].Success() {
		t.Fatalf("matching body should succeed, got %+v", res[0])
	}

	g2 := newGRPCGen(t, ts, plainOpts(),
		GRPCCall{Method: healthCheck, Message: `{"service":"down"}`, ExpectBody: map[string]string{"json:status": "SERVING"}})
	res := g2.Do(context.Background(), 0)
	if res[0].Success() || !strings.Contains(res[0].Error.Error(), `got "NOT_SERVING", want "SERVING"`) {
		t.Fatalf("mismatching body should fail with both values, got %+v", res[0])
	}
}

func TestGRPCGeneratorChainsExtractedValues(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	g := newGRPCGen(t, ts, plainOpts(),
		GRPCCall{Method: healthCheck, Message: `{"service":"up"}`, Extract: map[string]string{"st": "json:status"}},
		// "SERVING" is not a registered service, so the second call is NOT_FOUND
		// — expected here; what matters is the extracted value reached it.
		GRPCCall{Method: "/" + healthCheck, Message: `{"service":"{{.Vars.st}}"}`, ExpectCode: []string{"NOT_FOUND"}})

	res := g.Do(context.Background(), 0)
	if len(res) != 2 || !res[0].Success() || !res[1].Success() {
		t.Fatalf("want two successful results, got %+v", res)
	}
	if got := ts.seenServices(); len(got) != 2 || got[0] != "up" || got[1] != "SERVING" {
		t.Fatalf("server saw services %v, want [up SERVING]", got)
	}
}

func TestGRPCGeneratorStopsAtFirstFailedCall(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	g := newGRPCGen(t, ts, plainOpts(),
		GRPCCall{Method: healthCheck, Message: `{"service":"nope"}`},
		GRPCCall{Method: healthCheck, Message: `{"service":"up"}`})

	if res := g.Do(context.Background(), 0); len(res) != 1 || res[0].Success() {
		t.Fatalf("want the iteration to stop after the failed first call, got %+v", res)
	}
	if got := ts.seenServices(); len(got) != 1 {
		t.Fatalf("server saw %v; the second call should never have been sent", got)
	}
}

func TestGRPCGeneratorMetadataIdentitiesAndFeeder(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	feeder, err := NewFeeder(writeTempDataFile(t, "svc.csv", "svc\nup\ndown\n"), "sequential")
	if err != nil {
		t.Fatal(err)
	}
	opts := plainOpts()
	opts.Identities = []map[string]string{{"token": "t0"}, {"token": "t1"}}
	opts.Feeder = feeder

	g, err := NewGRPCGenerator(GRPCTarget{
		Address:  ts.addr,
		Metadata: map[string]string{"Authorization": "Bearer {{.Identity.token}}", "X-Env": "test"},
		Calls: []GRPCCall{{
			Method:     healthCheck,
			Message:    `{"service":"{{.Feeder.svc}}"}`,
			Metadata:   map[string]string{"x-env": "override", "x-vu": "{{.VU}}"},
			ExpectCode: []string{"OK"},
		}},
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	g.Do(context.Background(), 1) // VU 1 -> identity t1, first feeder row
	g.Do(context.Background(), 0) // VU 0 -> identity t0, second feeder row

	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.md) != 2 {
		t.Fatalf("server saw %d calls, want 2", len(ts.md))
	}
	first := ts.md[0]
	if first.Get("authorization")[0] != "Bearer t1" || first.Get("x-vu")[0] != "1" {
		t.Fatalf("first call metadata = %v", first)
	}
	if first.Get("x-env")[0] != "override" {
		t.Fatalf("call-level metadata should win over target-level, got %v", first.Get("x-env"))
	}
	if ts.md[1].Get("authorization")[0] != "Bearer t0" {
		t.Fatalf("second call metadata = %v", ts.md[1])
	}
	if got := ts.services; len(got) != 2 || got[0] != "up" || got[1] != "down" {
		t.Fatalf("feeder rows reached the server as %v, want [up down]", got)
	}
}

func healthProtoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `syntax = "proto3";
package grpc.health.v1;
message HealthCheckRequest { string service = 1; }
message HealthCheckResponse {
  enum ServingStatus { UNKNOWN = 0; SERVING = 1; NOT_SERVING = 2; SERVICE_UNKNOWN = 3; }
  ServingStatus status = 1;
}
service Health {
  rpc Check(HealthCheckRequest) returns (HealthCheckResponse);
  rpc Watch(HealthCheckRequest) returns (stream HealthCheckResponse);
}
`
	if err := os.WriteFile(filepath.Join(dir, "health.proto"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGRPCGeneratorSchemaFromProtoFiles(t *testing.T) {
	// No reflection on the server: the schema must come from the .proto source.
	ts := startGRPCServer(t, grpcServerConfig{reflection: "none"})
	dir := healthProtoDir(t)
	call := GRPCCall{Method: healthCheck, Message: `{"service":"up"}`, ExpectBody: map[string]string{"json:status": "SERVING"}}

	for name, mutate := range map[string]func(*GRPCOptions){
		"default import path": func(o *GRPCOptions) { o.ProtoFiles = []string{filepath.Join(dir, "health.proto")} },
		"explicit import path": func(o *GRPCOptions) {
			o.ProtoFiles = []string{filepath.Join(dir, "health.proto")}
			o.ImportPaths = []string{dir}
		},
		"relative to import": func(o *GRPCOptions) { o.ProtoFiles = []string{"health.proto"}; o.ImportPaths = []string{dir} },
	} {
		t.Run(name, func(t *testing.T) {
			opts := plainOpts()
			mutate(&opts)
			g := newGRPCGen(t, ts, opts, call)
			if res := g.Do(context.Background(), 0); !res[0].Success() {
				t.Fatalf("call failed: %+v", res[0])
			}
		})
	}
}

func TestGRPCGeneratorSchemaFromDescriptorSet(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{reflection: "none"})

	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(grpc_health_v1.File_grpc_health_v1_health_proto),
	}}
	b, err := proto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	path := writeTempFile(t, t.TempDir(), "health.pb", b)

	opts := plainOpts()
	opts.DescriptorSet = path
	g := newGRPCGen(t, ts, opts, GRPCCall{Method: healthCheck, Message: `{"service":"up"}`})
	if res := g.Do(context.Background(), 0); !res[0].Success() {
		t.Fatalf("call failed: %+v", res[0])
	}
}

func TestGRPCGeneratorReflectionFallsBackToV1Alpha(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{reflection: "v1alpha"})
	g := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, Message: `{"service":"up"}`})
	if res := g.Do(context.Background(), 0); !res[0].Success() {
		t.Fatalf("call failed: %+v", res[0])
	}
}

func TestGRPCGeneratorWithoutAnySchemaSourceFailsClearly(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{reflection: "none"})
	_, err := NewGRPCGenerator(GRPCTarget{Address: ts.addr, Calls: []GRPCCall{{Method: healthCheck}}}, plainOpts())
	if err == nil || !strings.Contains(err.Error(), "doesn't offer reflection") || !strings.Contains(err.Error(), "proto_files") {
		t.Fatalf("want an error pointing at reflection and proto_files, got %v", err)
	}
}

func TestGRPCGeneratorUnreachableServerDoesNotBlameReflection(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	addr := ts.addr
	ts.srv.Stop()

	opts := plainOpts()
	opts.Timeout = 300 * time.Millisecond
	_, err := NewGRPCGenerator(GRPCTarget{Address: addr, Calls: []GRPCCall{{Method: healthCheck}}}, opts)
	if err == nil {
		t.Fatal("expected a construction error for an unreachable server")
	}
	if strings.Contains(err.Error(), "enable it on the server") {
		t.Fatalf("a connectivity failure shouldn't tell the user to enable reflection: %v", err)
	}
}

func TestGRPCGeneratorConstructionValidation(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	call := GRPCCall{Method: healthCheck, Message: `{"service":"up"}`}

	cases := map[string]struct {
		target GRPCTarget
		mutate func(*GRPCOptions)
		want   string
	}{
		"no address":          {GRPCTarget{Calls: []GRPCCall{call}}, nil, "address is required"},
		"no calls":            {GRPCTarget{Address: ts.addr}, nil, "at least one call"},
		"bad method format":   {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{{Method: "justaname"}}}, nil, `expected "package.Service/Method"`},
		"unknown service":     {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{{Method: "no.Such/Thing"}}}, nil, "no.Such"},
		"unknown method":      {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{{Method: "grpc.health.v1.Health/Nope"}}}, nil, "has no method"},
		"bad static message":  {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{{Method: healthCheck, Message: `{"nonsense":1}`}}}, nil, "parsing message"},
		"bad expect_code":     {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{{Method: healthCheck, ExpectCode: []string{"MAYBE"}}}}, nil, "invalid expect_code"},
		"bad expect_body":     {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{{Method: healthCheck, ExpectBody: map[string]string{"bogus": "x"}}}}, nil, "invalid expect_body"},
		"negative conns":      {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, func(o *GRPCOptions) { o.Connections = -1 }, "connections"},
		"proto and set":       {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, func(o *GRPCOptions) { o.ProtoFiles = []string{"a.proto"}; o.DescriptorSet = "a.pb" }, "mutually exclusive"},
		"import paths alone":  {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, func(o *GRPCOptions) { o.ImportPaths = []string{"x"} }, "import_paths"},
		"plaintext with ca":   {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, func(o *GRPCOptions) { o.CAFile = "ca.pem" }, "plaintext"},
		"plaintext+insecure":  {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, func(o *GRPCOptions) { o.Insecure = true }, "plaintext"},
		"missing descriptors": {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, func(o *GRPCOptions) { o.DescriptorSet = "/nonexistent.pb" }, "descriptor_set"},
		"missing proto":       {GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, func(o *GRPCOptions) { o.ProtoFiles = []string{"/nonexistent/x.proto"} }, "proto_files"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts := plainOpts()
			if tc.mutate != nil {
				tc.mutate(&opts)
			}
			g, err := NewGRPCGenerator(tc.target, opts)
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

func TestGRPCGeneratorBadTemplatedMessageFailsPerCall(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	// Valid to construct (a template can't be checked until rendered), but
	// renders to a number where the schema wants a string.
	g := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, Message: `{"service": {{.VU}} }`})

	res := g.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Success() || res[0].Error == nil || !strings.Contains(res[0].Error.Error(), "parsing message") {
		t.Fatalf("want a per-call parse failure, got %+v", res)
	}
	if got := ts.seenServices(); len(got) != 0 {
		t.Fatalf("an unparseable message must never reach the server, saw %v", got)
	}
}

func TestGRPCGeneratorEmptyMessageIsTheEmptyRequest(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	// An empty HealthCheckRequest asks for the server's overall status.
	g := newGRPCGen(t, ts, plainOpts(), GRPCCall{Method: healthCheck, ExpectBody: map[string]string{"json:status": "SERVING"}})
	if res := g.Do(context.Background(), 0); !res[0].Success() {
		t.Fatalf("call failed: %+v", res[0])
	}
}

func TestGRPCGeneratorSpreadsVirtualUsersOverConnections(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	opts := plainOpts()
	opts.Connections = 3
	g := newGRPCGen(t, ts, opts, GRPCCall{Method: healthCheck, Message: `{"service":"up"}`})

	for vu := 0; vu < 7; vu++ {
		if res := g.Do(context.Background(), vu); !res[0].Success() {
			t.Fatalf("vu %d failed: %+v", vu, res[0])
		}
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.peers) != 3 {
		t.Fatalf("server saw %d distinct client connections, want 3: %v", len(ts.peers), ts.peers)
	}
}

func TestGRPCGeneratorConcurrentUseIsSafe(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	g := newGRPCGen(t, ts, plainOpts(),
		GRPCCall{Method: healthCheck, Message: `{"service":"up"}`}, // static, shared message
		GRPCCall{Method: healthCheck, Message: `{"service":"{{.VU}}"}`, ExpectCode: []string{"NOT_FOUND"}})

	var wg sync.WaitGroup
	for vu := 0; vu < 8; vu++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
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
}

func TestGRPCGeneratorDownServerWaitsForTimeoutInsteadOfSpinning(t *testing.T) {
	ts := startGRPCServer(t, grpcServerConfig{})
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(grpc_health_v1.File_grpc_health_v1_health_proto),
	}}
	b, _ := proto.Marshal(set)
	descPath := writeTempFile(t, t.TempDir(), "health.pb", b)

	opts := plainOpts()
	opts.DescriptorSet = descPath // no network needed to construct
	opts.Timeout = 300 * time.Millisecond
	g := newGRPCGen(t, ts, opts, GRPCCall{Method: healthCheck, Message: `{"service":"up"}`})
	ts.srv.Stop()

	// With fail-fast RPCs the first call or two may fail instantly, but once
	// the channel is in backoff every later call must still take ~Timeout.
	var slowest time.Duration
	for i := 0; i < 3; i++ {
		start := time.Now()
		res := g.Do(context.Background(), 0)
		if res[0].Success() {
			t.Fatal("call to a stopped server succeeded")
		}
		slowest = max(slowest, time.Since(start))
		if i > 0 && time.Since(start) < 200*time.Millisecond {
			t.Fatalf("call %d returned after %s; a down server must block until Timeout, not fail instantly", i, time.Since(start))
		}
	}
	if slowest > 3*time.Second {
		t.Fatalf("calls took %s, far past the 300ms timeout", slowest)
	}
}

func TestGRPCGeneratorTLS(t *testing.T) {
	ca := newTestCA(t)
	caFile := writeTempFile(t, t.TempDir(), "ca.pem", ca.caPEM)
	serverCreds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{ca.serverCert}})
	call := GRPCCall{Method: healthCheck, Message: `{"service":"up"}`}

	tlsOpts := func() GRPCOptions {
		o := DefaultGRPCOptions()
		o.Timeout = 2 * time.Second
		return o
	}

	t.Run("trusted via ca_file", func(t *testing.T) {
		ts := startGRPCServer(t, grpcServerConfig{creds: serverCreds})
		o := tlsOpts()
		o.CAFile = caFile
		g := newGRPCGen(t, ts, o, call)
		if res := g.Do(context.Background(), 0); !res[0].Success() {
			t.Fatalf("call failed: %+v", res[0])
		}
	})

	t.Run("insecure skips verification", func(t *testing.T) {
		ts := startGRPCServer(t, grpcServerConfig{creds: serverCreds})
		o := tlsOpts()
		o.Insecure = true
		g := newGRPCGen(t, ts, o, call)
		if res := g.Do(context.Background(), 0); !res[0].Success() {
			t.Fatalf("call failed: %+v", res[0])
		}
	})

	t.Run("untrusted server cert fails construction", func(t *testing.T) {
		ts := startGRPCServer(t, grpcServerConfig{creds: serverCreds})
		o := tlsOpts()
		o.Timeout = 500 * time.Millisecond
		if g, err := NewGRPCGenerator(GRPCTarget{Address: ts.addr, Calls: []GRPCCall{call}}, o); err == nil {
			g.Close()
			t.Fatal("expected the schema lookup to fail on an untrusted certificate")
		}
	})

	t.Run("mutual TLS", func(t *testing.T) {
		mtls := credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{ca.serverCert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    ca.caPool,
		})
		ts := startGRPCServer(t, grpcServerConfig{creds: mtls})
		dir := t.TempDir()
		o := tlsOpts()
		o.CAFile = caFile
		o.CertFile = writeTempFile(t, dir, "client.pem", ca.clientCertPEM)
		o.KeyFile = writeTempFile(t, dir, "client.key", ca.clientKeyPEM)
		g := newGRPCGen(t, ts, o, call)
		if res := g.Do(context.Background(), 0); !res[0].Success() {
			t.Fatalf("call failed: %+v", res[0])
		}
	})
}

func TestGRPCStatusMapping(t *testing.T) {
	for name, want := range map[string]int{
		"OK": 200, "NOT_FOUND": 404, "UNAVAILABLE": 503, "INVALID_ARGUMENT": 400,
		"UNAUTHENTICATED": 401, "PERMISSION_DENIED": 403, "DEADLINE_EXCEEDED": 504,
		"RESOURCE_EXHAUSTED": 429, "INTERNAL": 500, "UNIMPLEMENTED": 501, "ALREADY_EXISTS": 409,
	} {
		code, ok := grpcCodeFromName(name)
		if !ok {
			t.Fatalf("%s not recognised", name)
		}
		if got := httpStatusFromGRPCCode(code); got != want {
			t.Errorf("%s -> %d, want %d", name, got, want)
		}
		if got := grpcCodeName(code); got != name {
			t.Errorf("grpcCodeName(%s) = %q", name, got)
		}
	}
	if c, ok := grpcCodeFromName(" canceled "); !ok || grpcCodeName(c) != "CANCELLED" {
		t.Errorf("both spellings of CANCELLED should be accepted and print canonically")
	}
}

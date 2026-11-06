package resonate_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/reflection"

	"github.com/jecklgamis/resonate"
)

// These tests cover the fluent builder for every protocol and every option the
// generators have, so a protocol or option reachable from YAML or the struct
// API but not from Scenario shows up here as a missing test.

// ---- SSE ----

type sseRequest struct {
	Method, Path, Query, Body, Auth string
}

func sseServer(t *testing.T, events int) (*httptest.Server, func() []sseRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []sseRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, sseRequest{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < events; i++ {
			fmt.Fprint(w, "data: tick\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []sseRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]sseRequest(nil), seen...)
	}
}

func TestScenarioSSERunsEndToEnd(t *testing.T) {
	srv, seen := sseServer(t, 3)
	summary, err := resonate.NewScenario().
		SSE("GET", srv.URL+"/events").
		Events(3).
		Requests(2).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 2 iterations * (1 connect + 3 events).
	if summary.Requests != 8 || summary.SuccessRate != 1 {
		t.Errorf("summary = %d requests, %v success; want 8 and 1", summary.Requests, summary.SuccessRate)
	}
	if got := seen(); len(got) != 2 || got[0].Method != "GET" || got[0].Path != "/events" {
		t.Errorf("server saw %+v", got)
	}
}

func TestScenarioSSEBuildsTheRequestLikeAnHTTPTarget(t *testing.T) {
	srv, seen := sseServer(t, 1)
	_, err := resonate.NewScenario().
		SSE("POST", srv.URL+"/v1/stream").
		Header("Authorization", "Bearer abc").
		Query("model", "x").
		JSONBody(`{"prompt":"hi {{.VU}}"}`).
		Events(1).
		Requests(1).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := seen()
	if len(got) != 1 {
		t.Fatalf("server saw %d requests", len(got))
	}
	r := got[0]
	if r.Method != "POST" || r.Path != "/v1/stream" || r.Query != "model=x" || r.Auth != "Bearer abc" || r.Body != `{"prompt":"hi 0"}` {
		t.Errorf("request = %+v", r)
	}
}

func TestScenarioSSEStreamDurationAndFeeder(t *testing.T) {
	srv, seen := sseServer(t, 2)
	_, err := resonate.NewScenario().
		SSE("GET", srv.URL+"/s").
		Query("user", "{{.Feeder.id}}").
		FeederRows([]map[string]string{{"id": "a"}, {"id": "b"}}, "sequential").
		StreamDuration(200 * time.Millisecond).
		Requests(2).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := seen()
	if len(got) != 2 || got[0].Query != "user=a" || got[1].Query != "user=b" {
		t.Errorf("the feeder didn't reach the SSE request: %+v", got)
	}
}

func TestScenarioSSERejectsWhatDoesNotApplyToAStream(t *testing.T) {
	cases := map[string]func(*resonate.Scenario) *resonate.Scenario{
		"ExpectStatus": func(s *resonate.Scenario) *resonate.Scenario { return s.ExpectStatus(200) },
		"ExpectHeader": func(s *resonate.Scenario) *resonate.Scenario { return s.ExpectHeader("X", "y") },
		"ExpectBody":   func(s *resonate.Scenario) *resonate.Scenario { return s.ExpectBody("json:a", "b") },
		"Name":         func(s *resonate.Scenario) *resonate.Scenario { return s.Name("x") },
	}
	for name, apply := range cases {
		_, err := apply(resonate.NewScenario().SSE("GET", "http://localhost:1/")).Build()
		if err == nil || !strings.Contains(err.Error(), "SSE") {
			t.Errorf("%s on an SSE scenario: err = %v, want a rejection that says why (and no panic)", name, err)
		}
	}
}

// ---- gRPC ----

// healthServer hosts grpc.health.v1.Health with reflection on, recording the
// metadata of every unary call.
func healthServer(t *testing.T) (addr string, metadataSeen func() []metadata.MD) {
	t.Helper()
	var mu sync.Mutex
	var seen []metadata.MD
	srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		mu.Lock()
		seen = append(seen, md)
		mu.Unlock()
		return h(ctx, req)
	}))
	hs := health.NewServer()
	hs.SetServingStatus("up", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, hs)
	reflection.Register(srv)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), func() []metadata.MD {
		mu.Lock()
		defer mu.Unlock()
		return append([]metadata.MD(nil), seen...)
	}
}

func TestScenarioGRPCUnaryRunsEndToEnd(t *testing.T) {
	addr, _ := healthServer(t)
	summary, err := resonate.NewScenario().
		GRPC(addr).Plaintext().
		Call("grpc.health.v1.Health/Check").
		Message(`{"service":"up"}`).
		ExpectBody("json:status", "SERVING").
		Requests(6).Workers(2).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Requests != 6 || summary.SuccessRate != 1 {
		t.Errorf("summary = %d requests, %v success; want 6 and 1", summary.Requests, summary.SuccessRate)
	}
}

func TestScenarioGRPCExpectBodyMismatchFails(t *testing.T) {
	addr, _ := healthServer(t)
	summary, _ := resonate.NewScenario().
		GRPC(addr).Plaintext().
		Call("grpc.health.v1.Health/Check").Message(`{"service":"up"}`).
		ExpectBody("json:status", "NOT_SERVING").
		Requests(3).Workers(1).
		Run(context.Background())
	if summary.SuccessRate != 0 {
		t.Errorf("success rate = %v; the body check on the call never took effect", summary.SuccessRate)
	}
}

func TestScenarioGRPCExpectCodeAndMetadataScoping(t *testing.T) {
	addr, mdSeen := healthServer(t)
	summary, err := resonate.NewScenario().
		GRPC(addr).Plaintext().
		Metadata("x-tenant", "acme"). // before any Call: every call
		Call("grpc.health.v1.Health/Check").
		Message(`{"service":"missing"}`).
		Metadata("x-call", "one"). // after Call: this call only
		ExpectCode("NOT_FOUND").
		Requests(2).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("success rate = %v; NOT_FOUND was declared the expected code", summary.SuccessRate)
	}
	md := mdSeen()
	if len(md) != 2 || md[0].Get("x-tenant")[0] != "acme" || md[0].Get("x-call")[0] != "one" {
		t.Errorf("metadata the server saw = %v", md)
	}
}

func TestScenarioGRPCStreamingWithReceive(t *testing.T) {
	addr, _ := healthServer(t)
	summary, err := resonate.NewScenario().
		GRPC(addr).Plaintext().
		Call("grpc.health.v1.Health/Watch").
		Message(`{"service":"up"}`).
		Receive(1).
		Requests(4).Workers(2).
		Run(context.Background())
	if err != nil || summary.Requests != 4 || summary.SuccessRate != 1 {
		t.Errorf("err=%v summary=%d requests, %v success; want 4 and 1", err, summary.Requests, summary.SuccessRate)
	}
}

func TestScenarioGRPCChainsCallsWithExtractAndFeeder(t *testing.T) {
	addr, mdSeen := healthServer(t)
	_, err := resonate.NewScenario().
		GRPC(addr).Plaintext().
		Call("grpc.health.v1.Health/Check").
		Message(`{"service":"{{.Feeder.svc}}"}`).
		Extract("status", "json:status").
		Call("grpc.health.v1.Health/Check").
		Message(`{"service":"up"}`).
		Metadata("x-prev", "{{.Vars.status}}").
		FeederRows([]map[string]string{{"svc": "up"}}, "sequential").
		Requests(1).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	md := mdSeen()
	if len(md) != 2 || md[1].Get("x-prev")[0] != "SERVING" {
		t.Errorf("the first call's extracted value didn't reach the second: %v", md)
	}
}

func TestScenarioGRPCSchemaAndConnectionOptionsReachTheGenerator(t *testing.T) {
	// Each is wrong on purpose: the error is the proof the option arrived.
	cases := map[string]struct {
		build func() *resonate.Scenario
		want  string
	}{
		"ProtoFiles": {func() *resonate.Scenario {
			return resonate.NewScenario().GRPC("localhost:1").Plaintext().ProtoFiles("/no/such.proto").
				Call("a.B/C").Message(`{}`)
		}, "no/such.proto"},
		"DescriptorSet": {func() *resonate.Scenario {
			return resonate.NewScenario().GRPC("localhost:1").Plaintext().DescriptorSet("/no/such.desc").
				Call("a.B/C").Message(`{}`)
		}, "no/such.desc"},
		"CAFile": {func() *resonate.Scenario {
			return resonate.NewScenario().GRPC("localhost:1").CAFile("/no/such/ca.pem").
				Call("a.B/C").Message(`{}`)
		}, "ca.pem"},
		"ClientCert": {func() *resonate.Scenario {
			return resonate.NewScenario().GRPC("localhost:1").ClientCert("/no/cert.pem", "/no/key.pem").
				Call("a.B/C").Message(`{}`)
		}, "cert.pem"},
		"Plaintext with Insecure": {func() *resonate.Scenario {
			return resonate.NewScenario().GRPC("localhost:1").Plaintext().Insecure().
				Call("a.B/C").Message(`{}`)
		}, "laintext"},
	}
	for name, c := range cases {
		_, err := c.build().Build()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestScenarioGRPCConnectionsAndTimeoutBuild(t *testing.T) {
	addr, _ := healthServer(t)
	_, err := resonate.NewScenario().
		GRPC(addr).Plaintext().Connections(3).Timeout(5 * time.Second).
		Call("grpc.health.v1.Health/Check").Message(`{"service":"up"}`).
		Requests(3).Workers(3).
		Run(context.Background())
	if err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestScenarioGRPCMisuse(t *testing.T) {
	g := func() *resonate.Scenario { return resonate.NewScenario().GRPC("localhost:1").Plaintext() }
	cases := map[string]struct {
		s    *resonate.Scenario
		want string
	}{
		"Call outside gRPC":          {resonate.NewScenario().Get("http://x/").Call("a.B/C"), "only applies to a gRPC scenario"},
		"Message before Call":        {g().Message(`{}`), "call Call(method) first"},
		"Receive before Call":        {g().Receive(1), "call Call(method) first"},
		"Message twice":              {g().Call("a.B/C").Message(`{}`).Message(`{}`), "already has a message"},
		"Messages after Message":     {g().Call("a.B/C").Message(`{}`).Messages(`{}`, `{}`), "already has a message"},
		"Header":                     {g().Header("a", "b"), "use Metadata"},
		"StreamDuration before Call": {g().StreamDuration(time.Second), "call Call(method) first"},
		"Name":                       {g().Name("x"), "no per-step breakdown"},
		"Metadata outside gRPC":      {resonate.NewScenario().Get("http://x/").Metadata("a", "b"), "only applies to a gRPC scenario"},
		"ExpectStatus":               {g().Call("a.B/C").ExpectStatus(200), "mixes"},
		"Get after GRPC":             {g().Get("http://x/"), "mixes"},
	}
	for name, c := range cases {
		if _, err := c.s.Build(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// ---- HTTP transport options ----

func TestScenarioFollowRedirectsFalseSeesTheRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	for follow, wantCode := range map[bool]int{true: 200, false: 302} {
		summary, err := resonate.NewScenario().Get(srv.URL + "/start").FollowRedirects(follow).
			Requests(2).Workers(1).Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if summary.StatusCodes[wantCode] != 2 {
			t.Errorf("FollowRedirects(%v): status codes = %v, want 2x%d", follow, summary.StatusCodes, wantCode)
		}
	}
}

func TestScenarioMaxResponseBodyCapsBytesIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	t.Cleanup(srv.Close)
	summary, err := resonate.NewScenario().Get(srv.URL).MaxResponseBody(100).
		Requests(3).Workers(1).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.BytesIn != 300 {
		t.Errorf("BytesIn = %d, want 3 x 100: the cap never reached the client", summary.BytesIn)
	}
}

func TestScenarioDisableKeepAliveOpensAConnectionPerRequest(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	if _, err := resonate.NewScenario().Get(srv.URL).DisableKeepAlive().
		Requests(5).Workers(1).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := conns.Load(); got != 5 {
		t.Errorf("%d connections for 5 requests with keep-alive disabled", got)
	}

	conns.Store(0)
	if _, err := resonate.NewScenario().Get(srv.URL).MaxIdleConns(10).
		Requests(5).Workers(1).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("%d connections for 5 sequential requests with keep-alive on, want 1", got)
	}
}

func TestScenarioH2CSpeaksHTTP2OverPlaintext(t *testing.T) {
	var protos sync.Map
	srv := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protos.Store(r.Proto, true)
	}), &http2.Server{}))
	t.Cleanup(srv.Close)

	if _, err := resonate.NewScenario().Get(srv.URL).H2C().Requests(3).Workers(1).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := protos.Load("HTTP/2.0"); !ok {
		t.Error("the server never saw HTTP/2: H2C() didn't reach the client")
	}
}

func TestScenarioTransportOptionsApplyToSSEToo(t *testing.T) {
	srv, _ := sseServer(t, 1)
	_, err := resonate.NewScenario().SSE("GET", srv.URL).
		MaxIdleConns(50).FollowRedirects(true).MaxResponseBody(1 << 20).
		Events(1).Requests(1).Workers(1).Run(context.Background())
	if err != nil {
		t.Errorf("SSE is HTTP underneath, so the HTTP transport options must be accepted: %v", err)
	}
}

func TestScenarioTLSOptionsReachTheClient(t *testing.T) {
	for name, s := range map[string]*resonate.Scenario{
		"CAFile":     resonate.NewScenario().Get("https://localhost:1/").CAFile("/no/ca.pem"),
		"ClientCert": resonate.NewScenario().Get("https://localhost:1/").ClientCert("/no/c.pem", "/no/k.pem"),
	} {
		if _, err := s.Build(); err == nil {
			t.Errorf("%s: a missing file must fail the build, or the option was dropped", name)
		}
	}
}

func TestScenarioH2CConflictsAreRejected(t *testing.T) {
	for name, s := range map[string]*resonate.Scenario{
		"H2C+Insecure":         resonate.NewScenario().Get("http://localhost:1/").H2C().Insecure(),
		"H2C+DisableKeepAlive": resonate.NewScenario().Get("http://localhost:1/").H2C().DisableKeepAlive(),
	} {
		if _, err := s.Build(); err == nil {
			t.Errorf("%s: want a construction error", name)
		}
	}
}

func TestScenarioHTTPOnlyOptionsAreRejectedOnWSAndGRPCInEitherOrder(t *testing.T) {
	cases := map[string]func() *resonate.Scenario{
		"H2C after WS":                 func() *resonate.Scenario { return resonate.NewScenario().WS("ws://x/").H2C() },
		"H2C before WS":                func() *resonate.Scenario { return resonate.NewScenario().H2C().WS("ws://x/") },
		"MaxIdleConns after GRPC":      func() *resonate.Scenario { return resonate.NewScenario().GRPC("x:1").MaxIdleConns(5) },
		"FollowRedirects before GRPC":  func() *resonate.Scenario { return resonate.NewScenario().FollowRedirects(false).GRPC("x:1") },
		"MaxResponseBody after WS":     func() *resonate.Scenario { return resonate.NewScenario().WS("ws://x/").MaxResponseBody(1) },
		"DisableKeepAlive before GRPC": func() *resonate.Scenario { return resonate.NewScenario().DisableKeepAlive().GRPC("x:1") },
		"HTTP3 before GRPC":            func() *resonate.Scenario { return resonate.NewScenario().HTTP3().GRPC("x:1") },
		"CAFile after WS":              func() *resonate.Scenario { return resonate.NewScenario().WS("ws://x/").CAFile("a") },
		"ClientCert after WS":          func() *resonate.Scenario { return resonate.NewScenario().WS("ws://x/").ClientCert("a", "b") },
	}
	for name, mk := range cases {
		if _, err := mk().Build(); err == nil || !strings.Contains(err.Error(), "doesn't apply") {
			t.Errorf("%s: err = %v, want a \"doesn't apply\" rejection", name, err)
		}
	}
}

// ---- one protocol per scenario; silent no-ops are now errors ----

func TestScenarioMixingProtocolsIsRejected(t *testing.T) {
	for name, s := range map[string]*resonate.Scenario{
		"SSE then Target": resonate.NewScenario().SSE("GET", "http://x/").Target("GET", "http://y/"),
		"Get then SSE":    resonate.NewScenario().Get("http://y/").SSE("GET", "http://x/"),
		"WS and SSE":      resonate.NewScenario().WS("ws://x/").SSE("GET", "http://x/"),
		"SSE and GRPC":    resonate.NewScenario().SSE("GET", "http://x/").GRPC("x:1"),
		"GRPC and Step":   resonate.NewScenario().GRPC("x:1").Step("GET", "http://x/"),
		"WS and GRPC":     resonate.NewScenario().WS("ws://x/").GRPC("x:1"),
	} {
		if _, err := s.Build(); err == nil || !strings.Contains(err.Error(), "mixes") {
			t.Errorf("%s: err = %v, want a mixes-protocols error", name, err)
		}
	}
}

func TestScenarioMethodAfterSSERetargetsTheStreamRequest(t *testing.T) {
	srv, seen := sseServer(t, 1)
	_, err := resonate.NewScenario().
		SSE("GET", "http://ignored.invalid/").
		Post(srv.URL + "/real"). // describes the SSE request, not a second HTTP target
		Events(1).Requests(1).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 1 || got[0].Method != "POST" || got[0].Path != "/real" {
		t.Errorf("server saw %+v", got)
	}
}

func TestScenarioWebSocketOnlySettersNoLongerSilentlyDoNothing(t *testing.T) {
	for name, s := range map[string]*resonate.Scenario{
		"Message on HTTP":       resonate.NewScenario().Get("http://x/").Message("hi"),
		"Wait on HTTP":          resonate.NewScenario().Get("http://x/").Wait(),
		"Binary on HTTP":        resonate.NewScenario().Get("http://x/").Binary(),
		"Wait before Message":   resonate.NewScenario().WS("ws://x/").Wait(),
		"Binary before Message": resonate.NewScenario().WS("ws://x/").Binary(),
		"Events on HTTP":        resonate.NewScenario().Get("http://x/").Events(3),
		"Events on gRPC":        resonate.NewScenario().GRPC("x:1").Events(3),
	} {
		if _, err := s.Build(); err == nil {
			t.Errorf("%s: a setter that can't apply must fail the build instead of being ignored", name)
		}
	}
}

func TestScenarioPerStepAssertionsAreRejectedForProtocolsWithoutSteps(t *testing.T) {
	srv, _ := sseServer(t, 1)
	_, err := resonate.NewScenario().SSE("GET", srv.URL).Events(1).Requests(1).Workers(1).
		Assert(resonate.Assertion{Metric: "success_rate", Step: "x", Min: f64(1)}).
		Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "only apply to http") {
		t.Errorf("err = %v", err)
	}
}

func TestScenarioAbortWorksForGRPC(t *testing.T) {
	addr, _ := healthServer(t)
	start := time.Now()
	summary, err := resonate.NewScenario().
		GRPC(addr).Plaintext().
		Call("grpc.health.v1.Health/Check").Message(`{"service":"missing"}`). // NOT_FOUND: every call fails
		Duration(time.Minute).Workers(2).
		Assert(resonate.Assertion{Metric: "success_rate", Min: f64(0.9), Abort: true}).
		Run(context.Background())
	if time.Since(start) > 20*time.Second {
		t.Fatalf("a failing 1m gRPC run took %s", time.Since(start))
	}
	if err == nil || summary.Aborted == "" {
		t.Errorf("err=%v aborted=%q; abort must work for every protocol, not only HTTP", err, summary.Aborted)
	}
}

func TestScenarioIdentitiesReachSSEAndGRPC(t *testing.T) {
	srv, seen := sseServer(t, 1)
	_, err := resonate.NewScenario().
		SSE("GET", srv.URL).
		Header("Authorization", "Bearer {{.Identity.token}}").
		Identity(map[string]string{"token": "t0"}).
		Identity(map[string]string{"token": "t1"}).
		Events(1).Requests(2).Workers(2).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auths := map[string]bool{}
	for _, r := range seen() {
		auths[r.Auth] = true
	}
	if !auths["Bearer t0"] || !auths["Bearer t1"] {
		t.Errorf("SSE identities: server saw %v, want one request per identity", auths)
	}

	addr, mdSeen := healthServer(t)
	_, err = resonate.NewScenario().
		GRPC(addr).Plaintext().
		Identity(map[string]string{"tok": "g0"}).
		Call("grpc.health.v1.Health/Check").Message(`{"service":"up"}`).
		Metadata("x-token", "{{.Identity.tok}}").
		Requests(1).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if md := mdSeen(); len(md) != 1 || md[0].Get("x-token")[0] != "g0" {
		t.Errorf("gRPC identity: metadata = %v", md)
	}
}

func TestScenarioFeederRowReachesAGRPCCall(t *testing.T) {
	addr, mdSeen := healthServer(t)
	_, err := resonate.NewScenario().
		GRPC(addr).Plaintext().
		FeederRows([]map[string]string{{"who": "alice"}, {"who": "bob"}}, "sequential").
		Call("grpc.health.v1.Health/Check").Message(`{"service":"up"}`).
		Metadata("x-who", "{{.Feeder.who}}").
		Requests(2).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	md := mdSeen()
	if len(md) != 2 || md[0].Get("x-who")[0] != "alice" || md[1].Get("x-who")[0] != "bob" {
		t.Errorf("the feeder row didn't reach the gRPC call: %v", md)
	}
}

func TestScenarioGRPCMetadataIsScopedToItsCall(t *testing.T) {
	addr, mdSeen := healthServer(t)
	_, err := resonate.NewScenario().
		GRPC(addr).Plaintext().
		Metadata("x-all", "everywhere").
		Call("grpc.health.v1.Health/Check").Message(`{"service":"up"}`).
		Metadata("x-first", "only-here").
		Call("grpc.health.v1.Health/Check").Message(`{"service":"up"}`).
		Requests(1).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	md := mdSeen()
	if len(md) != 2 {
		t.Fatalf("server saw %d calls", len(md))
	}
	for i, m := range md {
		if m.Get("x-all")[0] != "everywhere" {
			t.Errorf("call %d lacks the scenario-level metadata: %v", i, m)
		}
	}
	if len(md[0].Get("x-first")) != 1 || len(md[1].Get("x-first")) != 0 {
		t.Errorf("x-first must be on the first call only: first=%v second=%v", md[0].Get("x-first"), md[1].Get("x-first"))
	}
}

func TestScenarioSSEStreamDurationEndsAnEndlessStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for r.Context().Err() == nil {
			fmt.Fprint(w, "data: tick\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)

	// Without a duration (or Events) an endless stream runs into the timeout
	// cap and is a failure, so success proves StreamDuration reached the target.
	start := time.Now()
	summary, err := resonate.NewScenario().
		SSE("GET", srv.URL).StreamDuration(200 * time.Millisecond).Timeout(3 * time.Second).
		Requests(1).Workers(1).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.SuccessRate != 1 || time.Since(start) > 2500*time.Millisecond {
		t.Errorf("success %v after %s; StreamDuration should end the stream cleanly after ~200ms", summary.SuccessRate, time.Since(start))
	}
}

func TestScenarioGRPCTimeoutBoundsAHangingStream(t *testing.T) {
	addr, _ := healthServer(t)
	// Health/Watch never ends and there's no Receive/StreamDuration, so the
	// call can only end at the timeout (as a failure).
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	summary, _ := resonate.NewScenario().
		GRPC(addr).Plaintext().Timeout(300 * time.Millisecond).
		Call("grpc.health.v1.Health/Watch").Message(`{"service":"up"}`).
		Requests(1).Workers(1).Run(ctx)
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s; the 300ms Timeout never reached the gRPC options (the default is 30s)", time.Since(start))
	}
	// One message arrives at once (a success), then the stream hits the cap: the
	// trailing failure is what shows the timeout applied.
	if summary.Requests < 2 || summary.SuccessRate >= 1 {
		t.Errorf("summary = %d requests, %v success; want the capped stream to end in a failure", summary.Requests, summary.SuccessRate)
	}
}

func TestScenarioFeederRowReachesAWebSocketConnection(t *testing.T) {
	srv, getHeader := wsEchoServer(t)
	_, err := resonate.NewScenario().
		WS(wsURL(srv.URL)).
		Header("X-User", "{{.Feeder.id}}").
		FeederRows([]map[string]string{{"id": "feeder-row-7"}}, "sequential").
		Message("ping").
		Requests(1).Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := getHeader().Get("X-User"); got != "feeder-row-7" {
		t.Errorf("X-User = %q; the feeder never reached the WS connection", got)
	}
}

func TestScenarioSSEEventsEndsTheStreamEarly(t *testing.T) {
	srv, _ := sseServer(t, 5) // the server would send 5
	summary, err := resonate.NewScenario().
		SSE("GET", srv.URL).Events(2).
		Requests(1).Workers(1).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 1 connect + 2 events: reading stops at Events, not at the server closing.
	if summary.Requests != 3 || summary.SuccessRate != 1 {
		t.Errorf("summary = %d requests, %v success; want 3 and 1", summary.Requests, summary.SuccessRate)
	}
}

func TestScenarioGRPCStreamingFieldsReachTheCall(t *testing.T) {
	addr, _ := healthServer(t)
	unary := func() *resonate.Scenario {
		return resonate.NewScenario().GRPC(addr).Plaintext().
			Call("grpc.health.v1.Health/Check").Message(`{"service":"up"}`)
	}
	// Check is unary, so the generator rejects any streaming-only field on it:
	// the rejection is the proof each setter's value arrived at the call.
	for name, s := range map[string]*resonate.Scenario{
		"Receive":        unary().Receive(2),
		"StreamDuration": unary().StreamDuration(time.Second),
		"SendInterval":   unary().SendInterval(time.Second),
		"SendRepeat":     unary().SendRepeat(3),
	} {
		if _, err := s.Build(); err == nil || !strings.Contains(err.Error(), "unary RPC") {
			t.Errorf("%s: err = %v, want the generator's streaming-field rejection", name, err)
		}
	}
	_, err := resonate.NewScenario().GRPC(addr).Plaintext().
		Call("grpc.health.v1.Health/Check").Messages(`{"service":"up"}`, `{"service":"up"}`).Build()
	if err == nil || !strings.Contains(err.Error(), "unary RPC") {
		t.Errorf("Messages: err = %v, want the generator's rejection", err)
	}
}

func TestScenarioGRPCConnectionsSpreadsUsersAcrossConnections(t *testing.T) {
	var mu sync.Mutex
	peers := map[string]bool{}
	srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if p, ok := peer.FromContext(ctx); ok {
			mu.Lock()
			peers[p.Addr.String()] = true
			mu.Unlock()
		}
		return h(ctx, req)
	}))
	hs := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, hs)
	reflection.Register(srv)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	_, err = resonate.NewScenario().
		GRPC(lis.Addr().String()).Plaintext().Connections(3).
		Call("grpc.health.v1.Health/Check").Message(`{"service":""}`).
		Requests(9).Workers(3).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 3 {
		t.Errorf("the server saw %d client connections, want 3 (Connections(3) with 3 users)", len(peers))
	}

	if _, err := resonate.NewScenario().GRPC("localhost:1").Plaintext().Connections(-1).
		Call("a.B/C").Message(`{}`).Build(); err == nil || !strings.Contains(err.Error(), "connections") {
		t.Errorf("a negative Connections must be rejected, got %v", err)
	}
}

func TestScenarioGRPCImportPathsResolveProtoImports(t *testing.T) {
	mainDir, depDir := t.TempDir(), t.TempDir()
	write := func(dir, name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(depDir, "dep.proto", `syntax = "proto3"; package dep; message Req { string id = 1; }`)
	mainProto := write(mainDir, "main.proto", `syntax = "proto3"; package svc;
import "dep.proto";
message Resp { string x = 1; }
service Svc { rpc Do(dep.Req) returns (Resp); }`)

	build := func(importPaths ...string) error {
		s := resonate.NewScenario().GRPC("localhost:1").Plaintext().ProtoFiles(mainProto)
		if len(importPaths) > 0 {
			s = s.ImportPaths(importPaths...)
		}
		_, err := s.Call("svc.Svc/Do").Message(`{"id":"1"}`).Build()
		return err
	}
	if err := build(); err == nil || !strings.Contains(err.Error(), "dep.proto") {
		t.Fatalf("without ImportPaths the import can't resolve; err = %v", err)
	}
	if err := build(mainDir, depDir); err != nil {
		t.Errorf("with ImportPaths covering the import: %v", err)
	}
}

package resonate_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jecklgamis/resonate"
)

func TestScenarioDSLEndToEnd(t *testing.T) {
	var gotContentType, gotRequestID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotRequestID = r.Header.Get("X-Request-Id")
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Post(srv.URL+"/orders").
		Header("X-Request-Id", "{{uuid}}").
		JSONBody(`{"id": {{.Seq}}}`).
		ExpectStatus(http.StatusCreated).
		Requests(3).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}

	if summary.Requests != 3 {
		t.Errorf("Requests = %d, want 3", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotRequestID == "" {
		t.Error("X-Request-Id was not sent")
	}
}

func TestScenarioDSLStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Stages(
			resonate.Stage{Duration: 200 * time.Millisecond, Workers: 5, Rate: 0},
			resonate.Stage{Duration: 200 * time.Millisecond, Workers: 5, Rate: 0},
		).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}

	if summary.Requests == 0 {
		t.Error("Requests = 0, want a staged run to have sent at least one request")
	}
	if summary.SuccessRate < 0.9 {
		t.Errorf("SuccessRate = %v, want close to 1 (a request or two may be cut off right at the schedule boundary)", summary.SuccessRate)
	}
}

func TestScenarioDSLMultipleTargets(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Target("GET", srv.URL+"/a").
		Target("GET", srv.URL+"/b").
		Requests(4).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}

	if summary.Requests != 4 {
		t.Errorf("Requests = %d, want 4", summary.Requests)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/a"] != 2 || hits["/b"] != 2 {
		t.Errorf("hits = %v, want /a:2 /b:2 (round-robin)", hits)
	}
}

func TestScenarioDSLFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"abc123"}`))
		case "/orders":
			if r.Header.Get("Authorization") != "Bearer abc123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Step("GET", srv.URL+"/login").
		Extract("token", "json:token").
		Step("POST", srv.URL+"/orders").
		Header("Authorization", "Bearer {{.Vars.token}}").
		ExpectStatus(http.StatusCreated).
		Requests(3).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}

	if summary.Requests != 6 {
		t.Errorf("Requests = %d, want 6 (3 iterations * 2 steps)", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
}

func TestScenarioDSLSetupAndIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"tok-` + r.Header.Get("X-User") + `"}`))
		case "/me":
			w.Header().Set("X-Got-Token", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Identity(map[string]string{"user": "alice"}).
		Setup("GET", srv.URL+"/login").
		Header("X-User", "{{.Identity.user}}").
		Extract("token", "json:token").
		Step("GET", srv.URL+"/me").
		Header("Authorization", "Bearer {{.Vars.token}}").
		ExpectHeader("X-Got-Token", "Bearer tok-alice").
		Requests(2).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}

	if summary.Requests != 2 {
		t.Errorf("Requests = %d, want 2", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
}

func TestScenarioDSLPauseSleepsBeforeStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	summary, err := resonate.NewScenario().
		Step("GET", srv.URL).
		Step("GET", srv.URL).
		Pause(100 * time.Millisecond).
		Requests(1).
		Workers(1).
		Run(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.Requests != 2 {
		t.Errorf("Requests = %d, want 2", summary.Requests)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 100ms", elapsed)
	}
}

func TestScenarioDSLRepeatRunsStepsNTimes(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Repeat(3).
		Step("GET", srv.URL).
		End().
		Requests(1).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.Requests != 3 {
		t.Errorf("Requests = %d, want 3", summary.Requests)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hits = %d, want 3", got)
	}
}

func TestScenarioDSLIfRunsOnlyWhenTrue(t *testing.T) {
	var adminHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"role":"admin"}`))
		case "/admin":
			atomic.AddInt32(&adminHits, 1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Step("GET", srv.URL+"/login").
		Extract("role", "json:role").
		If(`{{eq .Vars.role "admin"}}`).
		Step("GET", srv.URL+"/admin").
		End().
		Requests(1).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.Requests != 2 {
		t.Errorf("Requests = %d, want 2 (login + admin)", summary.Requests)
	}
	if atomic.LoadInt32(&adminHits) != 1 {
		t.Error("admin step should have run because role == admin")
	}
}

func TestScenarioDSLFileBodyIsTemplated(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "order.json.tmpl")
	if err := os.WriteFile(path, []byte(`{"id": {{.Seq}}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	summary, err := resonate.NewScenario().
		Post(srv.URL).
		FileBody(path).
		Requests(1).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	if gotBody != `{"id": 0}` {
		t.Errorf("server received body %q, want %q (rendered .Seq)", gotBody, `{"id": 0}`)
	}
}

func TestScenarioDSLFileBodyMissingFileDefersError(t *testing.T) {
	_, err := resonate.NewScenario().
		Post("http://example.test").
		FileBody("/nonexistent/path/does-not-exist.json").
		Build()
	if err == nil {
		t.Fatal("expected an error from Build() for a missing FileBody file")
	}
}

func TestScenarioDSLRawFileBodyMissingFileDefersError(t *testing.T) {
	_, err := resonate.NewScenario().
		Post("http://example.test").
		RawFileBody("/nonexistent/path/does-not-exist.bin").
		Build()
	if err == nil {
		t.Fatal("expected an error from Build() for a missing RawFileBody file")
	}
}

func TestScenarioDSLRawFileBodySentUnrendered(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "payload.bin")
	raw := []byte(`literal {{ not a template }} bytes`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	summary, err := resonate.NewScenario().
		Post(srv.URL).
		RawFileBody(path).
		Requests(1).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	if gotBody != string(raw) {
		t.Errorf("server received body %q, want %q (unrendered)", gotBody, string(raw))
	}
}

func TestScenarioDSLRawBodyAndBodyMutuallyExclusive(t *testing.T) {
	_, err := resonate.NewScenario().
		Post("http://example.test").
		Body("abc").
		RawBody([]byte("xyz")).
		Build()
	if err == nil {
		t.Fatal("expected an error combining Body and RawBody")
	}
}

func TestScenarioDSLBaseURLPrependedToRelativePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		BaseURL(srv.URL).
		Get("/health").
		Requests(1).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	if gotPath != "/health" {
		t.Errorf("server received path %q, want /health", gotPath)
	}
}

func TestScenarioDSLMixingTargetAndStepErrors(t *testing.T) {
	_, err := resonate.NewScenario().
		Get("http://localhost:8080/a").
		Step("GET", "http://localhost:8080/b").
		Build()
	if err == nil {
		t.Fatal("Build() error = nil, want an error for mixing Target and Step")
	}
}

func TestScenarioDSLIterations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Workers(2).
		Iterations(3).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}

	// No Duration/Requests/Stages set, so Iterations alone is the stop
	// condition: exactly Workers*Iterations requests, self-terminating.
	if summary.Requests != 6 {
		t.Errorf("Requests = %d, want 6 (2 workers * 3 iterations)", summary.Requests)
	}
}

func TestScenarioDSLFeeder(t *testing.T) {
	var gotEmail string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotEmail = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, "users.csv")
	if err := os.WriteFile(path, []byte("email\nalice@example.com\n"), 0o644); err != nil {
		t.Fatalf("writing feeder file: %v", err)
	}

	summary, err := resonate.NewScenario().
		Post(srv.URL).
		Body("{{.Feeder.email}}").
		Feeder(path, "sequential").
		Requests(1).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	if gotEmail != "alice@example.com" {
		t.Errorf("request body = %q, want alice@example.com", gotEmail)
	}
}

func TestScenarioDSLFeederLoadErrorDeferredToBuild(t *testing.T) {
	_, err := resonate.NewScenario().
		Get("http://localhost:8080/a").
		Feeder("/does/not/exist.csv", "sequential").
		Build()
	if err == nil {
		t.Fatal("Build() error = nil, want an error for a missing feeder file")
	}
}

func TestScenarioDSLAssertPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Requests(5).
		Workers(1).
		Assert(resonate.Assertion{Metric: "success_rate", Min: floatPtr(0.99)}).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v, want nil (assertion should pass)", err)
	}
	if summary.Requests != 5 {
		t.Errorf("Requests = %d, want 5", summary.Requests)
	}
}

func TestScenarioDSLAssertFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Requests(5).
		Workers(1).
		Assert(resonate.Assertion{Metric: "success_rate", Min: floatPtr(0.99)}).
		Run(context.Background())
	if err == nil {
		t.Fatal("Scenario.Run error = nil, want an error (assertion should fail)")
	}
	// The Summary is still returned (not zeroed) even though the assertion failed.
	if summary.Requests != 5 {
		t.Errorf("Requests = %d, want 5 (Summary should still be populated on assertion failure)", summary.Requests)
	}
}

func TestScenarioDSLAssertUnknownMetricErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, err := resonate.NewScenario().
		Get(srv.URL).
		Requests(1).
		Workers(1).
		Assert(resonate.Assertion{Metric: "not_a_real_metric", Min: floatPtr(1)}).
		Run(context.Background())
	if err == nil {
		t.Fatal("Scenario.Run error = nil, want an error for an unknown assertion metric")
	}
}

func floatPtr(f float64) *float64 { return &f }

// wsEchoServer accepts a WebSocket connection, records the handshake
// headers, and echoes every text message back wrapped as {"echo": "<msg>"}
// — mirrors internal/generator's wsEchoServer, duplicated here since that
// one is unexported and this test lives in package resonate_test.
func wsEchoServer(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var gotHeader http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeader = r.Header.Clone()
		mu.Unlock()

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()
		for {
			typ, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			resp, _ := json.Marshal(map[string]string{"echo": string(msg)})
			if err := conn.Write(ctx, typ, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return srv, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return gotHeader
	}
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestScenarioDSLWebSocket(t *testing.T) {
	srv, getHeader := wsEchoServer(t)

	summary, err := resonate.NewScenario().
		WS(wsURL(srv.URL)).
		Header("Authorization", "Bearer {{.Identity.token}}").
		Identity(map[string]string{"token": "abc123"}).
		Message(`{"type":"subscribe"}`).
		Wait().
		Extract("echoed", "json:echo").
		Message(`{"type":"ping","prev":"{{.Vars.echoed}}"}`).
		Wait().
		ExpectBody("json:echo", `{"type":"ping","prev":"{"type":"subscribe"}"}`).
		Requests(1).
		Workers(1).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Scenario.Run error: %v", err)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	// One connect result + one per message.
	if summary.Requests != 3 {
		t.Errorf("Requests = %d, want 3 (1 connect + 2 messages)", summary.Requests)
	}
	if got := getHeader().Get("Authorization"); got != "Bearer abc123" {
		t.Errorf("Authorization header = %q, want %q (from Identity)", got, "Bearer abc123")
	}
}

func TestScenarioDSLWebSocketExpectBodyWithoutWaitErrors(t *testing.T) {
	_, err := resonate.NewScenario().
		WS("ws://localhost:8080/socket").
		Message(`{"type":"ping"}`).
		ExpectBody("json:status", "ok").
		Build()
	if err == nil {
		t.Fatal("Build() error = nil, want an error for ExpectBody without Wait")
	}
}

func TestScenarioDSLWebSocketMixedWithTargetErrors(t *testing.T) {
	_, err := resonate.NewScenario().
		Get("http://localhost:8080/a").
		WS("ws://localhost:8080/socket").
		Message("ping").
		Wait().
		Build()
	if err == nil {
		t.Fatal("Build() error = nil, want an error for mixing WS and Target")
	}
}

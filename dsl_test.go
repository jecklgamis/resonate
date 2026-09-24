package resonate_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

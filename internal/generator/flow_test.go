package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// jsonHandler routes by path and returns JSON bodies, so tests can exercise
// extract rules (json:<path>) the way a real API would.
func jsonHandler(t *testing.T, routes map[string]func(r *http.Request) (int, map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, body := route(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFlowGeneratorSetupTokenUsedInFlowStep(t *testing.T) {
	var loginCalls, orderCalls int32
	srv := jsonHandler(t, map[string]func(r *http.Request) (int, map[string]any){
		"/login": func(r *http.Request) (int, map[string]any) {
			atomic.AddInt32(&loginCalls, 1)
			return http.StatusOK, map[string]any{"token": "tok-abc"}
		},
		"/orders": func(r *http.Request) (int, map[string]any) {
			atomic.AddInt32(&orderCalls, 1)
			if r.Header.Get("Authorization") != "Bearer tok-abc" {
				t.Errorf("orders request missing expected Authorization header, got %q", r.Header.Get("Authorization"))
			}
			return http.StatusOK, map[string]any{"id": "order-42"}
		},
	})

	a, err := NewFlowGenerator(
		[]FlowStep{
			{
				Method:  "POST",
				URL:     srv.URL + "/orders",
				Header:  map[string]string{"Authorization": "Bearer {{.Vars.token}}"},
				Extract: map[string]string{"order_id": "json:id"},
			},
		},
		[]FlowStep{
			{
				Method:  "POST",
				URL:     srv.URL + "/login",
				Extract: map[string]string{"token": "json:token"},
			},
		},
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !results[0].Success() {
		t.Errorf("flow step should have succeeded, got status %d err %v", results[0].StatusCode, results[0].Error)
	}
	if got := atomic.LoadInt32(&loginCalls); got != 1 {
		t.Errorf("loginCalls = %d, want 1 (setup ran once)", got)
	}
	if got := atomic.LoadInt32(&orderCalls); got != 1 {
		t.Errorf("orderCalls = %d, want 1", got)
	}
}

func TestFlowGeneratorStepTwoReceivesOrderID(t *testing.T) {
	var step2Path string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orders":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "order-99"})
		default:
			mu.Lock()
			step2Path = r.URL.Path
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "POST", URL: srv.URL + "/orders", Extract: map[string]string{"order_id": "json:id"}},
			{Method: "GET", URL: srv.URL + "/orders/{{.Vars.order_id}}"},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	mu.Lock()
	got := step2Path
	mu.Unlock()
	if got != "/orders/order-99" {
		t.Errorf("step 2 hit path %q, want /orders/order-99 (chained from step 1's extracted id)", got)
	}
}

func TestFlowGeneratorStopsAfterFailedStep(t *testing.T) {
	var step2Hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		step2Hit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL + "/fail"},
			{Method: "GET", URL: srv.URL + "/never-reached"},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (flow should stop after the failed first step)", len(results))
	}
	if step2Hit {
		t.Error("step 2 should never have been reached after step 1 returned a 500")
	}
}

func TestFlowGeneratorExpectStatusFailureStopsFlow(t *testing.T) {
	var step2Hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/step1" {
			w.WriteHeader(http.StatusOK) // a real 200, but not in this step's ExpectStatus
			return
		}
		step2Hit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL + "/step1", ExpectStatus: []int{201}},
			{Method: "GET", URL: srv.URL + "/step2"},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (flow should stop after step 1 fails its status check)", len(results))
	}
	if results[0].Success() {
		t.Error("step 1's 200 response should not report Success() since only 201 was expected")
	}
	if step2Hit {
		t.Error("step 2 should never have been reached after step 1 failed its status check")
	}
}

func TestFlowGeneratorExpectHeadersFailureStopsFlow(t *testing.T) {
	var step2Hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/step1" {
			w.Header().Set("Content-Type", "text/plain") // step 1 wants application/json
			w.WriteHeader(http.StatusOK)
			return
		}
		step2Hit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL + "/step1", ExpectHeaders: map[string]string{"Content-Type": "application/json"}},
			{Method: "GET", URL: srv.URL + "/step2"},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (flow should stop after step 1 fails its header check)", len(results))
	}
	if results[0].Success() {
		t.Error("step 1's mismatched Content-Type should not report Success()")
	}
	if step2Hit {
		t.Error("step 2 should never have been reached after step 1 failed its header check")
	}
}

func TestFlowGeneratorExpectBodyFailureStopsFlow(t *testing.T) {
	var step2Hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/step1" {
			w.Write([]byte(`{"status":"error"}`)) // step 1 wants "ok"
			return
		}
		step2Hit = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL + "/step1", ExpectBody: map[string]string{"json:status": "ok"}},
			{Method: "GET", URL: srv.URL + "/step2"},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (flow should stop after step 1 fails its body check)", len(results))
	}
	if results[0].Success() {
		t.Error("step 1's mismatched json:status should not report Success()")
	}
	if step2Hit {
		t.Error("step 2 should never have been reached after step 1 failed its body check")
	}
}

func TestFlowGeneratorIdentityAssignmentRoundRobin(t *testing.T) {
	var mu sync.Mutex
	seenUsernames := map[int]string{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	identities := []map[string]string{
		{"username": "alice"},
		{"username": "bob"},
		{"username": "carol"},
	}

	a, err := NewFlowGenerator(
		[]FlowStep{{Method: "GET", URL: srv.URL + "/ping", Header: map[string]string{"X-User": "{{.Identity.username}}"}}},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions(), Identities: identities},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	for vuID := 0; vuID < 5; vuID++ {
		mu.Lock()
		seenUsernames[vuID] = identities[vuID%len(identities)]["username"]
		mu.Unlock()
		a.Do(context.Background(), vuID)
	}

	want := map[int]string{0: "alice", 1: "bob", 2: "carol", 3: "alice", 4: "bob"}
	for vuID, wantName := range want {
		if got := seenUsernames[vuID]; got != wantName {
			t.Errorf("vuID %d expected identity %q, computed %q", vuID, wantName, got)
		}
	}
}

func TestFlowGeneratorSetupRunsOncePerVUID(t *testing.T) {
	var loginCount int32
	srv := jsonHandler(t, map[string]func(r *http.Request) (int, map[string]any){
		"/login": func(r *http.Request) (int, map[string]any) {
			n := atomic.AddInt32(&loginCount, 1)
			return http.StatusOK, map[string]any{"token": fmt.Sprintf("tok-%d", n)}
		},
		"/ping": func(r *http.Request) (int, map[string]any) {
			return http.StatusOK, map[string]any{}
		},
	})

	a, err := NewFlowGenerator(
		[]FlowStep{{Method: "GET", URL: srv.URL + "/ping", Header: map[string]string{"Authorization": "Bearer {{.Vars.token}}"}}},
		[]FlowStep{{Method: "POST", URL: srv.URL + "/login", Extract: map[string]string{"token": "json:token"}}},
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	// Same vuID called 3 times: setup (login) should run exactly once.
	for i := 0; i < 3; i++ {
		a.Do(context.Background(), 0)
	}
	if got := atomic.LoadInt32(&loginCount); got != 1 {
		t.Errorf("loginCount for a single vuID called 3 times = %d, want 1 (setup runs once per VU)", got)
	}

	// A different vuID should trigger its own independent setup/login.
	a.Do(context.Background(), 1)
	if got := atomic.LoadInt32(&loginCount); got != 2 {
		t.Errorf("loginCount after a second distinct vuID = %d, want 2", got)
	}
}

func TestFlowGeneratorSetupResultsNotInReturnedResults(t *testing.T) {
	srv := jsonHandler(t, map[string]func(r *http.Request) (int, map[string]any){
		"/login": func(r *http.Request) (int, map[string]any) { return http.StatusOK, map[string]any{"token": "t"} },
		"/ping":  func(r *http.Request) (int, map[string]any) { return http.StatusOK, map[string]any{} },
	})

	a, err := NewFlowGenerator(
		[]FlowStep{{Method: "GET", URL: srv.URL + "/ping"}},
		[]FlowStep{{Method: "POST", URL: srv.URL + "/login", Extract: map[string]string{"token": "json:token"}}},
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Errorf("got %d results, want exactly 1 — setup's login call must not be reported as a flow result", len(results))
	}
}

func TestFlowGeneratorPauseSleepsBetweenSteps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL},
			{Method: "GET", URL: srv.URL, Pause: 100 * time.Millisecond},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	start := time.Now()
	results := a.Do(context.Background(), 0)
	elapsed := time.Since(start)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 100ms (pause before step 2)", elapsed)
	}
}

func TestFlowGeneratorPauseAbortsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL, Pause: time.Hour},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	results := a.Do(ctx, 0)
	elapsed := time.Since(start)

	if len(results) != 0 {
		t.Errorf("got %d results, want 0 (should abort during the pause, before sending)", len(results))
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %v, want well under an hour-long pause", elapsed)
	}
}

func TestFlowGeneratorRepeatRunsNestedStepsNTimes(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Repeat: 3, Steps: []FlowStep{{Method: "GET", URL: srv.URL}}},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hits = %d, want 3", got)
	}
}

func TestFlowGeneratorRepeatStopsOnNestedFailure(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Repeat: 5, Steps: []FlowStep{{Method: "GET", URL: srv.URL}}},
			{Method: "GET", URL: srv.URL + "/never-reached"},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	// 2 requests inside the repeat block (1 success, 1 failure), then the
	// whole flow stops — the step after the repeat block is never reached.
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}

func TestFlowGeneratorDuringRunsForApproximatelyItsDuration(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{During: 150 * time.Millisecond, Steps: []FlowStep{{Method: "GET", URL: srv.URL}}},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	start := time.Now()
	results := a.Do(context.Background(), 0)
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 150ms", elapsed)
	}
	if len(results) < 2 {
		t.Errorf("got %d results, want at least a couple of iterations within 150ms", len(results))
	}
}

func TestFlowGeneratorIfRunsOnlyWhenConditionTrue(t *testing.T) {
	var loginCalls, adminCalls int32
	srv := jsonHandler(t, map[string]func(r *http.Request) (int, map[string]any){
		"/login": func(r *http.Request) (int, map[string]any) {
			atomic.AddInt32(&loginCalls, 1)
			return http.StatusOK, map[string]any{"role": "admin"}
		},
		"/admin": func(r *http.Request) (int, map[string]any) {
			atomic.AddInt32(&adminCalls, 1)
			return http.StatusOK, map[string]any{}
		},
	})

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL + "/login", Extract: map[string]string{"role": "json:role"}},
			{If: `{{eq .Vars.role "admin"}}`, Steps: []FlowStep{
				{Method: "GET", URL: srv.URL + "/admin"},
			}},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (login + admin)", len(results))
	}
	if atomic.LoadInt32(&adminCalls) != 1 {
		t.Error("admin step should have run because role == admin")
	}
}

func TestFlowGeneratorIfSkipsWhenConditionFalse(t *testing.T) {
	var adminCalls int32
	srv := jsonHandler(t, map[string]func(r *http.Request) (int, map[string]any){
		"/login": func(r *http.Request) (int, map[string]any) {
			return http.StatusOK, map[string]any{"role": "user"}
		},
		"/admin": func(r *http.Request) (int, map[string]any) {
			atomic.AddInt32(&adminCalls, 1)
			return http.StatusOK, map[string]any{}
		},
	})

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: srv.URL + "/login", Extract: map[string]string{"role": "json:role"}},
			{If: `{{eq .Vars.role "admin"}}`, Steps: []FlowStep{
				{Method: "GET", URL: srv.URL + "/admin"},
			}},
		},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (login only, admin skipped)", len(results))
	}
	if atomic.LoadInt32(&adminCalls) != 0 {
		t.Error("admin step should not have run because role != admin")
	}
}

func TestFlowGeneratorRepeatAndMethodMutuallyExclusive(t *testing.T) {
	_, err := NewFlowGenerator(
		[]FlowStep{{Repeat: 3, Method: "GET", URL: "http://example.com", Steps: []FlowStep{{Method: "GET", URL: "http://example.com"}}}},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err == nil {
		t.Fatal("expected an error combining repeat with method/url on the same step")
	}
}

func TestFlowGeneratorRepeatAndDuringMutuallyExclusive(t *testing.T) {
	_, err := NewFlowGenerator(
		[]FlowStep{{Repeat: 3, During: time.Second, Steps: []FlowStep{{Method: "GET", URL: "http://example.com"}}}},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err == nil {
		t.Fatal("expected an error combining repeat with during on the same step")
	}
}

func TestFlowGeneratorRepeatRequiresNestedSteps(t *testing.T) {
	_, err := NewFlowGenerator(
		[]FlowStep{{Repeat: 3}},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()},
	)
	if err == nil {
		t.Fatal("expected an error for a repeat step with no nested steps")
	}
}

func TestFlowGeneratorBaseURLPrependedToRelativeSteps(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Method: "GET", URL: "/login"},
			{Method: "GET", URL: "/orders"},
		},
		nil,
		FlowOptions{HTTP: HTTPOptions{BaseURL: srv.URL}},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	if len(gotPaths) != 2 || gotPaths[0] != "/login" || gotPaths[1] != "/orders" {
		t.Errorf("gotPaths = %v, want [/login /orders]", gotPaths)
	}
}

func TestFlowGeneratorBaseURLAppliesInsideNestedControlFlow(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/poll" {
			atomic.AddInt32(&hits, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{
			{Repeat: 3, Steps: []FlowStep{{Method: "GET", URL: "/poll"}}},
		},
		nil,
		FlowOptions{HTTP: HTTPOptions{BaseURL: srv.URL}},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("hits = %d, want 3 (BaseURL must resolve inside a repeat block too)", hits)
	}
}

func TestFlowGeneratorNoFlowStepsErrors(t *testing.T) {
	_, err := NewFlowGenerator(nil, nil, FlowOptions{HTTP: DefaultHTTPOptions()})
	if err == nil {
		t.Fatal("expected an error constructing a FlowGenerator with no flow steps")
	}
}

func TestFlowGeneratorNoIdentitiesRendersEmpty(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-User")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a, err := NewFlowGenerator(
		[]FlowStep{{Method: "GET", URL: srv.URL, Header: map[string]string{"X-User": "{{.Identity.username}}"}}},
		nil,
		FlowOptions{HTTP: DefaultHTTPOptions()}, // no Identities
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	if gotHeader != "" {
		t.Errorf("X-User header = %q, want empty string with no identities pool configured", gotHeader)
	}
}

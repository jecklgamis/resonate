package resonate_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/jecklgamis/resonate"
)

// This file exercises resonate's public facade the way an external
// consumer would: only the resonate package is imported, never
// resonate/internal/*. If a change to the internal packages ever breaks
// this file, it's a sign the public API surface moved underneath it.

func TestHTTPGeneratorRunEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewHTTPGenerator(
		[]resonate.HTTPTarget{{URL: srv.URL}},
		resonate.DefaultHTTPOptions(),
	)
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{
		Requests: 10,
		Workers:  2,
	})

	if summary.Requests != 10 {
		t.Errorf("Requests = %d, want 10", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1 (every request returned 200)", summary.SuccessRate)
	}
}

func TestFlowGeneratorChainsExtractedValues(t *testing.T) {
	var gotOrderPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orders" {
			w.Write([]byte(`{"id":"order-42"}`))
			return
		}
		gotOrderPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewFlowGenerator(
		[]resonate.FlowStep{
			{Method: "POST", URL: srv.URL + "/orders", Extract: map[string]string{"orderID": "json:id"}},
			{Method: "GET", URL: srv.URL + "/orders/{{.Vars.orderID}}"},
		},
		nil,
		resonate.FlowOptions{HTTP: resonate.DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 2, Workers: 1})
	if summary.SuccessRate != 1 {
		t.Fatalf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	if gotOrderPath != "/orders/order-42" {
		t.Errorf("second step's path = %q, want /orders/order-42 (chained from the first step's response)", gotOrderPath)
	}
}

func TestAssertionEvaluate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewHTTPGenerator([]resonate.HTTPTarget{{URL: srv.URL}}, resonate.DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 5, Workers: 1})

	min := 0.99
	failures, err := summary.Evaluate([]resonate.Assertion{{Metric: "success_rate", Min: &min}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1 (every request 500s)", len(failures))
	}
}

func TestValidateRejectsNegativeRate(t *testing.T) {
	if err := resonate.Validate(resonate.Options{Rate: -1}); err == nil {
		t.Fatal("expected an error for a negative Rate")
	}
}

func TestFeederDrivesRequestData(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/users.csv"
	if err := writeFile(path, "email\nalice@example.com\n"); err != nil {
		t.Fatalf("writing feeder file: %v", err)
	}

	feeder, err := resonate.NewFeeder(path, "sequential")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	row := feeder.Next()
	if row["email"] != "alice@example.com" {
		t.Errorf("Feeder.Next()[\"email\"] = %q, want alice@example.com", row["email"])
	}
}

func TestKafkaFeederDrivesRequestData(t *testing.T) {
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cl, err := kgo.NewClient(kgo.SeedBrokers(c.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.ProduceSync(context.Background(), &kgo.Record{Topic: "users", Value: []byte(`{"email":"alice@example.com"}`)}).FirstErr(); err != nil {
		t.Fatal(err)
	}
	cl.Close()

	feeder, err := resonate.NewKafkaFeeder(resonate.KafkaFeederOptions{Brokers: c.ListenAddrs(), Topic: "users"})
	if err != nil {
		t.Fatalf("NewKafkaFeeder error: %v", err)
	}
	defer feeder.Close()
	for i := 0; i < 3; i++ { // one message, so it wraps
		if row := feeder.Next(); row["email"] != "alice@example.com" || row["_offset"] != "0" {
			t.Errorf("Next() #%d = %v", i, row)
		}
	}
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o644)
}

func TestWriteHTMLFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewHTTPGenerator([]resonate.HTTPTarget{{URL: srv.URL}}, resonate.DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 2, Workers: 1})

	path := t.TempDir() + "/report.html"
	if err := resonate.WriteHTMLFile(path, resonate.HTMLReport{Title: "test report", Summary: summary}); err != nil {
		t.Fatalf("WriteHTMLFile error: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written report: %v", err)
	}
	if !strings.HasPrefix(string(contents), "<!doctype html>") {
		t.Error("written file does not start with a doctype")
	}
	if !strings.Contains(string(contents), "test report") {
		t.Error("written file does not contain the given Title")
	}
}

func TestWriteHTMLFileInvalidPathErrors(t *testing.T) {
	if err := resonate.WriteHTMLFile("/nonexistent/dir/report.html", resonate.HTMLReport{}); err == nil {
		t.Fatal("expected an error for an unwritable path")
	}
}

func TestGRPCGeneratorRunEndToEnd(t *testing.T) {
	srv := grpc.NewServer()
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

	opts := resonate.DefaultGRPCOptions()
	opts.Plaintext = true
	gen, err := resonate.NewGRPCGenerator(resonate.GRPCTarget{
		Address: lis.Addr().String(),
		Calls: []resonate.GRPCCall{{
			Method:     "grpc.health.v1.Health/Check",
			Message:    `{"service":"up"}`,
			ExpectBody: map[string]string{"json:status": "SERVING"},
		}},
	}, opts)
	if err != nil {
		t.Fatalf("NewGRPCGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 10, Workers: 2})
	if summary.Requests != 10 || summary.SuccessRate != 1 {
		t.Errorf("summary = %d requests, %v success rate; want 10 and 1", summary.Requests, summary.SuccessRate)
	}
}

func TestGRPCStreamingRunEndToEnd(t *testing.T) {
	srv := grpc.NewServer()
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

	opts := resonate.DefaultGRPCOptions()
	opts.Plaintext = true
	gen, err := resonate.NewGRPCGenerator(resonate.GRPCTarget{
		Address: lis.Addr().String(),
		Calls: []resonate.GRPCCall{{
			// Health/Watch is server-streaming: one result per received message.
			Method:  "grpc.health.v1.Health/Watch",
			Message: `{"service":"up"}`,
			Receive: 1,
		}},
	}, opts)
	if err != nil {
		t.Fatalf("NewGRPCGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 5, Workers: 2})
	if summary.Requests != 5 || summary.SuccessRate != 1 {
		t.Errorf("summary = %d requests, %v success rate; want 5 and 1", summary.Requests, summary.SuccessRate)
	}
}

func TestSSEGeneratorRunEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			w.Write([]byte("data: tick\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewSSEGenerator(
		resonate.SSETarget{URL: srv.URL, Events: 3},
		resonate.DefaultSSEOptions(),
	)
	if err != nil {
		t.Fatalf("NewSSEGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 2, Workers: 1})

	// 2 iterations * (1 connect + 3 events).
	if summary.Requests != 8 {
		t.Errorf("Requests = %d, want 8", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
}

func TestArrayFeederViaFacade(t *testing.T) {
	f, err := resonate.NewArrayFeeder([]map[string]string{{"email": "a@example.com"}}, "sequential")
	if err != nil {
		t.Fatalf("NewArrayFeeder error: %v", err)
	}
	if got := f.Next()["email"]; got != "a@example.com" {
		t.Errorf("Next()[email] = %q", got)
	}
	if _, err := resonate.NewScenario().Get("http://localhost:1").FeederRows(nil, "").Build(); err == nil {
		t.Error("FeederRows(nil) should fail at Build")
	}
}

func f64(v float64) *float64 { return &v }

func statusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestScenarioNamesStepsAndAssertsPerStep(t *testing.T) {
	srv := statusServer(t, http.StatusOK)
	summary, err := resonate.NewScenario().
		Step("GET", srv.URL+"/a").Name("first").
		Step("GET", srv.URL+"/b").Name("second").
		Requests(6).Workers(2).
		Assert(resonate.Assertion{Metric: "success_rate", Step: "second", Min: f64(1)}).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if len(summary.Steps) != 2 || summary.Steps[0].Name != "first" || summary.Steps[1].Name != "second" {
		t.Fatalf("Steps = %+v, want first then second", summary.Steps)
	}
	var _ resonate.StepSummary = summary.Steps[0]
	if summary.Steps[0].Requests != 6 {
		t.Errorf("first step requests = %d, want 6", summary.Steps[0].Requests)
	}
}

func TestScenarioStepAssertionFailureNamesTheStep(t *testing.T) {
	srv := statusServer(t, http.StatusOK)
	_, err := resonate.NewScenario().
		Get(srv.URL).Name("home").
		Requests(3).Workers(1).
		Assert(resonate.Assertion{Metric: "requests", Step: "home", Min: f64(100)}).
		Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requests{step=home}") {
		t.Errorf("err = %v, want the failure to name the step", err)
	}
}

func TestScenarioRunRejectsAssertionOnUnknownStep(t *testing.T) {
	srv := statusServer(t, http.StatusOK)
	_, err := resonate.NewScenario().
		Get(srv.URL).Name("home").
		Requests(3).Workers(1).
		Assert(resonate.Assertion{Metric: "success_rate", Step: "hmoe", Min: f64(1)}).
		Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `unknown step "hmoe"`) {
		t.Errorf("err = %v, want a typo'd step rejected before the run", err)
	}
}

func TestScenarioNameOnWebSocketIsABuildError(t *testing.T) {
	if _, err := resonate.NewScenario().WS("ws://localhost:1/").Name("x").Build(); err == nil {
		t.Error("Name on a WebSocket scenario should fail at Build")
	}
}

func TestScenarioAbortAssertionStopsTheRun(t *testing.T) {
	srv := statusServer(t, http.StatusInternalServerError)
	start := time.Now()
	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Duration(time.Minute).Workers(2).
		Assert(resonate.Assertion{Metric: "success_rate", Min: f64(0.9), Abort: true}).
		Run(context.Background())
	if time.Since(start) > 20*time.Second {
		t.Fatalf("a failing 1m run took %s; AbortAfter is 0 for the library, so it should stop on the first check", time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("err = %v, want an aborted error", err)
	}
	if summary.Aborted == "" || summary.Requests == 0 {
		t.Errorf("summary = aborted %q, %d requests; want what ran before the stop", summary.Aborted, summary.Requests)
	}
}

func TestScenarioRejectsAbortOnACount(t *testing.T) {
	srv := statusServer(t, http.StatusOK)
	_, err := resonate.NewScenario().Get(srv.URL).Requests(1).
		Assert(resonate.Assertion{Metric: "requests", Min: f64(1), Abort: true}).
		Run(context.Background())
	if err == nil {
		t.Error("an abort assertion on a count can never be judged mid-run and must be rejected")
	}
}

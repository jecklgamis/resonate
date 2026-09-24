package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/generator"
)

func TestLoadOptionsFromFlat(t *testing.T) {
	cfg := config.LoadConfig{Duration: "30s", Rate: 50, Workers: 20, Iterations: 5}
	opts, err := loadOptionsFrom(cfg)
	if err != nil {
		t.Fatalf("loadOptionsFrom error: %v", err)
	}
	if opts.Duration != 30*time.Second {
		t.Errorf("Duration = %v, want 30s", opts.Duration)
	}
	if opts.Rate != 50 || opts.Workers != 20 || opts.Iterations != 5 {
		t.Errorf("opts = %+v, unexpected", opts)
	}
	if len(opts.Stages) != 0 {
		t.Errorf("Stages = %v, want empty for a flat config", opts.Stages)
	}
}

func TestLoadOptionsFromRequiresAStopCondition(t *testing.T) {
	_, err := loadOptionsFrom(config.LoadConfig{})
	if err == nil {
		t.Fatal("expected an error when no duration/requests/iterations/stages is set")
	}
}

func TestLoadOptionsFromIterationsAloneIsSufficient(t *testing.T) {
	_, err := loadOptionsFrom(config.LoadConfig{Iterations: 10, Workers: 5})
	if err != nil {
		t.Errorf("loadOptionsFrom error: %v, want no error (iterations alone is a valid stop condition)", err)
	}
}

func TestLoadOptionsFromInvalidDuration(t *testing.T) {
	_, err := loadOptionsFrom(config.LoadConfig{Duration: "not-a-duration"})
	if err == nil {
		t.Fatal("expected an error for an unparseable load.duration")
	}
}

func TestLoadOptionsFromStages(t *testing.T) {
	cfg := config.LoadConfig{
		Requests: 1000,
		Stages: []config.StageConfig{
			{Duration: "30s", Workers: 50, Rate: 50},
			{Duration: "15s", Workers: 0, Rate: 0},
		},
	}
	opts, err := loadOptionsFrom(cfg)
	if err != nil {
		t.Fatalf("loadOptionsFrom error: %v", err)
	}
	if len(opts.Stages) != 2 {
		t.Fatalf("Stages len = %d, want 2", len(opts.Stages))
	}
	if opts.Stages[0].Duration != 30*time.Second || opts.Stages[0].Workers != 50 || opts.Stages[0].Rate != 50 {
		t.Errorf("Stages[0] = %+v, unexpected", opts.Stages[0])
	}
	if opts.Requests != 1000 {
		t.Errorf("Requests = %d, want 1000 (still applies alongside Stages)", opts.Requests)
	}
	// Flat fields should be ignored/zero once Stages is set.
	if opts.Duration != 0 || opts.Rate != 0 || opts.Workers != 0 {
		t.Errorf("flat fields leaked through with Stages set: %+v", opts)
	}
}

func TestLoadOptionsFromInvalidStageDuration(t *testing.T) {
	cfg := config.LoadConfig{Stages: []config.StageConfig{{Duration: "nope"}}}
	_, err := loadOptionsFrom(cfg)
	if err == nil {
		t.Fatal("expected an error for an unparseable stage duration")
	}
}

func TestAssertionsFromValid(t *testing.T) {
	cfgs := []config.AssertionConfig{
		{Metric: "success_rate", Min: "0.95"},
		{Metric: "latency_p95", Max: "500ms"},
		{Metric: "rate", Min: "10", Max: "1000"},
	}
	got, err := assertionsFrom(cfgs)
	if err != nil {
		t.Fatalf("assertionsFrom error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d assertions, want 3", len(got))
	}
	if got[0].Metric != "success_rate" || got[0].Min == nil || *got[0].Min != 0.95 {
		t.Errorf("assertion 0 = %+v, unexpected", got[0])
	}
	if got[1].Metric != "latency_p95" || got[1].Max == nil || *got[1].Max != 0.5 {
		t.Errorf("assertion 1 = %+v, want Max=0.5 (500ms)", got[1])
	}
	if got[2].Min == nil || got[2].Max == nil {
		t.Errorf("assertion 2 = %+v, want both Min and Max set", got[2])
	}
}

func TestAssertionsFromEmpty(t *testing.T) {
	got, err := assertionsFrom(nil)
	if err != nil {
		t.Fatalf("assertionsFrom error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestAssertionsFromMissingMetric(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Min: "0.95"}})
	if err == nil {
		t.Fatal("expected an error for a missing metric name")
	}
}

func TestAssertionsFromMissingBounds(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Metric: "success_rate"}})
	if err == nil {
		t.Fatal("expected an error when neither min nor max is set")
	}
}

func TestAssertionsFromUnknownMetric(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Metric: "bogus", Min: "1"}})
	if err == nil {
		t.Fatal("expected an error for an unknown metric name")
	}
}

func TestAssertionsFromInvalidThreshold(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Metric: "rate", Min: "not-a-number"}})
	if err == nil {
		t.Fatal("expected an error for an unparseable threshold")
	}
}

func TestAssertionsFromGTAndLT(t *testing.T) {
	got, err := assertionsFrom([]config.AssertionConfig{
		{Metric: "rate", GT: "10"},
		{Metric: "latency_p95", LT: "500ms"},
	})
	if err != nil {
		t.Fatalf("assertionsFrom error: %v", err)
	}
	if got[0].GT == nil || *got[0].GT != 10 {
		t.Errorf("assertion 0 = %+v, want GT=10", got[0])
	}
	if got[1].LT == nil || *got[1].LT != 0.5 {
		t.Errorf("assertion 1 = %+v, want LT=0.5 (500ms)", got[1])
	}
}

func TestAssertionsFromIs(t *testing.T) {
	got, err := assertionsFrom([]config.AssertionConfig{{Metric: "success_rate", Is: "1"}})
	if err != nil {
		t.Fatalf("assertionsFrom error: %v", err)
	}
	if got[0].Is == nil || *got[0].Is != 1 {
		t.Errorf("assertion 0 = %+v, want Is=1", got[0])
	}
}

func TestAssertionsFromIn(t *testing.T) {
	got, err := assertionsFrom([]config.AssertionConfig{{Metric: "rate", In: []string{"50", "75", "100"}}})
	if err != nil {
		t.Fatalf("assertionsFrom error: %v", err)
	}
	if len(got[0].In) != 3 || got[0].In[1] != 75 {
		t.Errorf("assertion 0.In = %v, want [50 75 100]", got[0].In)
	}
}

func TestAssertionsFromInInvalidEntry(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Metric: "rate", In: []string{"50", "not-a-number"}}})
	if err == nil {
		t.Fatal("expected an error for an unparseable in[] entry")
	}
}

func TestAssertionsFromAroundRequiresBothFields(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Metric: "rate", Around: "50"}})
	if err == nil {
		t.Fatal("expected an error for around without around_margin")
	}
	_, err = assertionsFrom([]config.AssertionConfig{{Metric: "rate", AroundMargin: "5"}})
	if err == nil {
		t.Fatal("expected an error for around_margin without around")
	}
}

func TestAssertionsFromAroundValid(t *testing.T) {
	got, err := assertionsFrom([]config.AssertionConfig{
		{Metric: "latency_p95", Around: "500ms", AroundMargin: "50ms", AroundExclusive: true},
	})
	if err != nil {
		t.Fatalf("assertionsFrom error: %v", err)
	}
	if got[0].Around == nil || *got[0].Around != 0.5 || got[0].AroundMargin == nil || *got[0].AroundMargin != 0.05 {
		t.Errorf("assertion 0 = %+v, want Around=0.5 AroundMargin=0.05", got[0])
	}
	if !got[0].AroundExclusive {
		t.Error("AroundExclusive = false, want true")
	}
}

func TestAssertionsFromDeviatesAroundRequiresBothFields(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Metric: "rate", DeviatesAround: "100"}})
	if err == nil {
		t.Fatal("expected an error for deviates_around without deviates_percent")
	}
}

func TestAssertionsFromDeviatesAroundValid(t *testing.T) {
	got, err := assertionsFrom([]config.AssertionConfig{
		{Metric: "rate", DeviatesAround: "100", DeviatesPercent: "10"},
	})
	if err != nil {
		t.Fatalf("assertionsFrom error: %v", err)
	}
	if got[0].DeviatesAround == nil || *got[0].DeviatesAround != 100 {
		t.Errorf("assertion 0 = %+v, want DeviatesAround=100", got[0])
	}
	if got[0].DeviatesPercent == nil || *got[0].DeviatesPercent != 10 {
		t.Errorf("assertion 0 = %+v, want DeviatesPercent=10", got[0])
	}
}

func TestAssertionsFromDeviatesPercentInvalidNumber(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{
		{Metric: "rate", DeviatesAround: "100", DeviatesPercent: "not-a-number"},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid deviates_percent")
	}
}

func TestAssertionsFromNoConditionErrors(t *testing.T) {
	_, err := assertionsFrom([]config.AssertionConfig{{Metric: "rate", AroundExclusive: true}})
	if err == nil {
		t.Fatal("expected an error when no condition field is actually set")
	}
}

func TestRunCommandAssertionsPassExitsZero(t *testing.T) {
	t.Chdir(t.TempDir()) // runAndReport now writes report.html/report.json by default; keep the repo clean
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 5
  workers: 2
http:
  targets:
    - url: `+srv.URL+`
assertions:
  - metric: success_rate
    min: "0.95"
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	err := (error)(nil)
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Errorf("Execute error: %v, want nil (all assertions should pass)", err)
	}
}

func TestRunCommandAssertionsFailExitsNonZero(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 5
  workers: 2
http:
  targets:
    - url: `+srv.URL+`
assertions:
  - metric: latency_p95
    max: 1ns
`)

	cmd := newRunCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmd.Execute()
		})
	})
	if err == nil {
		t.Fatal("expected a non-nil error when an assertion fails (for a non-zero process exit)")
	}
	if !strings.Contains(stderr, "latency_p95") {
		t.Errorf("stderr = %q, want it to name the failed metric", stderr)
	}
}

func TestRunCommandGTInAroundAssertionsPass(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 5
  workers: 2
http:
  targets:
    - url: `+srv.URL+`
assertions:
  - metric: success_rate
    gt: "0.5"

  - metric: success
    in: ["4", "5", "6"]

  - metric: requests
    around: "5"
    around_margin: "1"
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Errorf("Execute error: %v, want nil (gt/in/around assertions should all pass)", err)
	}
}

func TestRunCommandDeviatesAroundAssertionFails(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 5
  workers: 2
http:
  targets:
    - url: `+srv.URL+`
assertions:
  - metric: requests
    deviates_around: "1000"
    deviates_percent: "1"
`)

	cmd := newRunCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmd.Execute()
		})
	})
	if err == nil {
		t.Fatal("expected a non-nil error (5 requests is nowhere near 1000 ± 1%)")
	}
	if !strings.Contains(stderr, "requests") {
		t.Errorf("stderr = %q, want it to name the failed metric", stderr)
	}
}

func TestRunCommandBaseURLPrependedToRelativeTargets(t *testing.T) {
	t.Chdir(t.TempDir())
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 1
  workers: 1
http:
  base_url: `+srv.URL+`
  targets:
    - url: /health
    - url: /status
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(gotPaths) != 1 {
		t.Fatalf("got %d requests, want 1", len(gotPaths))
	}
	if gotPaths[0] != "/health" && gotPaths[0] != "/status" {
		t.Errorf("server received path %q, want /health or /status", gotPaths[0])
	}
}

func TestBuildHTTPGeneratorRelativeTargetWithNoBaseURLErrors(t *testing.T) {
	cfg := config.HTTPConfig{Targets: []config.HTTPTargetConfig{{URL: "/health"}}}
	_, err := buildHTTPGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error for a relative target URL with no http.base_url set")
	}
}

func TestRunCommandExpectStatusMakesAssertionPass(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 3
  workers: 1
http:
  targets:
    - url: `+srv.URL+`
      expect_status: [404]
assertions:
  - metric: success_rate
    min: "1.0"
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Errorf("Execute error: %v, want nil (every 404 is explicitly expected, so success_rate should be 1.0)", err)
	}
}

func TestRunCommandExpectHeadersMakesAssertionFail(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 3
  workers: 1
http:
  targets:
    - url: `+srv.URL+`
      expect_headers:
        Content-Type: application/json
assertions:
  - metric: success_rate
    min: "0.5"
`)

	cmd := newRunCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmd.Execute()
		})
	})
	if err == nil {
		t.Fatal("expected a non-nil error (every response fails the Content-Type check, so success_rate is 0)")
	}
	if !strings.Contains(stderr, "success_rate") {
		t.Errorf("stderr = %q, want it to name the failed metric", stderr)
	}
}

func TestRunCommandExpectBodyJSONPathMakesAssertionPass(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","items":[{"id":"a"},{"id":"b"}]}`))
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 3
  workers: 1
http:
  targets:
    - url: `+srv.URL+`
      expect_body:
        "json:status": "ok"
        "json:items[1].id": "b"
assertions:
  - metric: success_rate
    min: "1.0"
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Errorf("Execute error: %v, want nil (both JSONPath checks match)", err)
	}
}

func TestRunCommandExpectBodyXPathMakesAssertionFail(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<response><status>error</status></response>`))
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 3
  workers: 1
http:
  targets:
    - url: `+srv.URL+`
      expect_body:
        "xml://status": "ok"
assertions:
  - metric: success_rate
    min: "0.5"
`)

	cmd := newRunCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmd.Execute()
		})
	})
	if err == nil {
		t.Fatal("expected a non-nil error (every response has status=error, so the xml check always fails)")
	}
	if !strings.Contains(stderr, "success_rate") {
		t.Errorf("stderr = %q, want it to name the failed metric", stderr)
	}
}

func TestRunCommandFlowRepeatAndIfControlFlow(t *testing.T) {
	t.Chdir(t.TempDir())
	var pingHits, adminHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			w.Write([]byte(`{"role":"admin"}`))
		case "/ping":
			atomic.AddInt32(&pingHits, 1)
			w.WriteHeader(http.StatusOK)
		case "/admin":
			atomic.AddInt32(&adminHits, 1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 1
  workers: 1
http:
  flow:
    - method: GET
      url: `+srv.URL+`/login
      extract:
        role: "json:role"
    - repeat: 3
      steps:
        - method: GET
          url: `+srv.URL+`/ping
    - if: '{{eq .Vars.role "admin"}}'
      steps:
        - method: GET
          url: `+srv.URL+`/admin
assertions:
  - metric: success_rate
    min: "1.0"
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := atomic.LoadInt32(&pingHits); got != 3 {
		t.Errorf("pingHits = %d, want 3 (repeat: 3)", got)
	}
	if got := atomic.LoadInt32(&adminHits); got != 1 {
		t.Errorf("adminHits = %d, want 1 (if role == admin)", got)
	}
}

func TestRunCommandFlowPauseAndPauseMaxSleepBeforeStep(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 1
  workers: 1
http:
  flow:
    - method: GET
      url: `+srv.URL+`
    - method: GET
      url: `+srv.URL+`
      pause: 100ms
      pause_max: 200ms
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	start := time.Now()
	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 100ms (pause/pause_max before step 2)", elapsed)
	}
}

func TestRunCommandFlowDuringRunsForApproximatelyItsDuration(t *testing.T) {
	t.Chdir(t.TempDir())
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 1
  workers: 1
http:
  flow:
    - during: 150ms
      steps:
        - method: GET
          url: `+srv.URL+`
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	start := time.Now()
	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 150ms (during: 150ms)", elapsed)
	}
	if atomic.LoadInt32(&hits) < 2 {
		t.Errorf("hits = %d, want at least a couple of iterations within 150ms", hits)
	}
}

func TestRunCommandFeederDrivesRequestEmails(t *testing.T) {
	t.Chdir(t.TempDir())

	feederDir := t.TempDir()
	feederPath := filepath.Join(feederDir, "users.csv")
	if err := os.WriteFile(feederPath, []byte("email\nalice@example.com\nbob@example.com\n"), 0o644); err != nil {
		t.Fatalf("writing feeder csv: %v", err)
	}

	var gotEmails []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEmails = append(gotEmails, r.URL.Query().Get("email"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 4
  workers: 1
http:
  feeder:
    file: `+feederPath+`
  targets:
    - url: `+srv.URL+`?email={{.Feeder.email}}
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	if len(gotEmails) != 4 {
		t.Fatalf("got %d requests, want 4", len(gotEmails))
	}
	want := []string{"alice@example.com", "bob@example.com", "alice@example.com", "bob@example.com"}
	for i, w := range want {
		if gotEmails[i] != w {
			t.Errorf("request %d email = %q, want %q (sequential feeder should cycle)", i, gotEmails[i], w)
		}
	}
}

func TestRunCommandStreamFeederDrivesRequestEmails(t *testing.T) {
	t.Chdir(t.TempDir())

	feederDir := t.TempDir()
	feederPath := filepath.Join(feederDir, "users.csv")
	if err := os.WriteFile(feederPath, []byte("email\nalice@example.com\nbob@example.com\n"), 0o644); err != nil {
		t.Fatalf("writing feeder csv: %v", err)
	}

	var gotEmails []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEmails = append(gotEmails, r.URL.Query().Get("email"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 4
  workers: 1
http:
  feeder:
    file: `+feederPath+`
    mode: stream
  targets:
    - url: `+srv.URL+`?email={{.Feeder.email}}
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	if len(gotEmails) != 4 {
		t.Fatalf("got %d requests, want 4", len(gotEmails))
	}
	want := []string{"alice@example.com", "bob@example.com", "alice@example.com", "bob@example.com"}
	for i, w := range want {
		if gotEmails[i] != w {
			t.Errorf("request %d email = %q, want %q (stream feeder should cycle)", i, gotEmails[i], w)
		}
	}
}

func TestBuildGeneratorUnsupportedProtocol(t *testing.T) {
	_, err := buildGenerator(&config.Scenario{Protocol: "unsupported"})
	if err == nil {
		t.Fatal("expected an error for an unimplemented protocol")
	}
}

func TestBuildGeneratorWSProtocol(t *testing.T) {
	scenario := &config.Scenario{
		Protocol: "ws",
		WS: config.WSConfig{
			URL:      "ws://example.test",
			Messages: []config.WSMessageConfig{{Body: "hi"}},
		},
	}
	a, err := buildGenerator(scenario)
	if err != nil {
		t.Fatalf("buildGenerator error: %v", err)
	}
	if _, ok := a.(*generator.WSGenerator); !ok {
		t.Errorf("got %T, want *generator.WSGenerator for protocol: ws", a)
	}
}

func TestBuildWSGeneratorRequiresURL(t *testing.T) {
	_, err := buildWSGenerator(config.WSConfig{Messages: []config.WSMessageConfig{{Body: "hi"}}})
	if err == nil {
		t.Fatal("expected an error for a missing ws.url")
	}
}

func TestBuildWSGeneratorRequiresMessages(t *testing.T) {
	_, err := buildWSGenerator(config.WSConfig{URL: "ws://example.test"})
	if err == nil {
		t.Fatal("expected an error for empty ws.messages")
	}
}

func TestBuildWSGeneratorExtractWithoutWaitErrors(t *testing.T) {
	cfg := config.WSConfig{
		URL: "ws://example.test",
		Messages: []config.WSMessageConfig{
			{Body: "hi", Wait: false, Extract: map[string]string{"x": "json:x"}},
		},
	}
	_, err := buildWSGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error for extract set without wait: true")
	}
}

func TestBuildWSGeneratorExpectBodyWithoutWaitErrors(t *testing.T) {
	cfg := config.WSConfig{
		URL: "ws://example.test",
		Messages: []config.WSMessageConfig{
			{Body: "hi", Wait: false, ExpectBody: map[string]string{"json:x": "y"}},
		},
	}
	_, err := buildWSGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error for expect_body set without wait: true")
	}
}

func TestBuildWSGeneratorInvalidTimeout(t *testing.T) {
	cfg := config.WSConfig{
		URL:      "ws://example.test",
		Messages: []config.WSMessageConfig{{Body: "hi"}},
		Timeout:  "not-a-duration",
	}
	_, err := buildWSGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error for an unparseable ws.timeout")
	}
}

func TestBuildWSGeneratorValid(t *testing.T) {
	cfg := config.WSConfig{
		URL: "ws://example.test",
		Messages: []config.WSMessageConfig{
			{Body: "hi", Wait: true, Extract: map[string]string{"x": "json:x"}},
		},
		Identities: []map[string]string{{"token": "abc"}},
		Timeout:    "5s",
	}
	if _, err := buildWSGenerator(cfg); err != nil {
		t.Errorf("buildWSGenerator error: %v", err)
	}
}

func TestBuildHTTPGeneratorTargetsAndFlowMutuallyExclusive(t *testing.T) {
	cfg := config.HTTPConfig{
		Targets: []config.HTTPTargetConfig{{URL: "http://x"}},
		Flow:    []config.HTTPTargetConfig{{URL: "http://y"}},
	}
	_, err := buildHTTPGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error when both targets and flow are set")
	}
}

func TestBuildHTTPGeneratorSetupWithoutFlowErrors(t *testing.T) {
	cfg := config.HTTPConfig{
		Targets: []config.HTTPTargetConfig{{URL: "http://example.test"}},
		Setup:   []config.HTTPTargetConfig{{URL: "http://example.test/login"}},
	}
	_, err := buildHTTPGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error: http.setup has no effect without http.flow")
	}
}

func TestBuildHTTPGeneratorIdentitiesWithoutFlowErrors(t *testing.T) {
	cfg := config.HTTPConfig{
		Targets:    []config.HTTPTargetConfig{{URL: "http://example.test"}},
		Identities: []map[string]string{{"username": "alice"}},
	}
	_, err := buildHTTPGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error: http.identities has no effect without http.flow")
	}
}

func TestBuildHTTPGeneratorRequiresTargetsOrFlow(t *testing.T) {
	_, err := buildHTTPGenerator(config.HTTPConfig{})
	if err == nil {
		t.Fatal("expected an error when neither targets nor flow is set")
	}
}

func TestBuildHTTPGeneratorTargets(t *testing.T) {
	cfg := config.HTTPConfig{
		Targets: []config.HTTPTargetConfig{{Method: "GET", URL: "http://example.test"}},
	}
	a, err := buildHTTPGenerator(cfg)
	if err != nil {
		t.Fatalf("buildHTTPGenerator error: %v", err)
	}
	if _, ok := a.(*generator.HTTPGenerator); !ok {
		t.Errorf("got %T, want *generator.HTTPGenerator for http.targets", a)
	}
}

func TestBuildHTTPGeneratorFlow(t *testing.T) {
	cfg := config.HTTPConfig{
		Flow: []config.HTTPTargetConfig{{Method: "GET", URL: "http://example.test"}},
	}
	a, err := buildHTTPGenerator(cfg)
	if err != nil {
		t.Fatalf("buildHTTPGenerator error: %v", err)
	}
	if _, ok := a.(*generator.FlowGenerator); !ok {
		t.Errorf("got %T, want *generator.FlowGenerator for http.flow", a)
	}
}

func TestBuildHTTPGeneratorPropagatesTargetBodyFileError(t *testing.T) {
	cfg := config.HTTPConfig{
		Targets: []config.HTTPTargetConfig{{URL: "http://x", BodyFile: "/nonexistent/path.json"}},
	}
	_, err := buildHTTPGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error for a missing body_file")
	}
}

func TestBuildHTTPGeneratorPropagatesTargetRawBodyFileError(t *testing.T) {
	cfg := config.HTTPConfig{
		Targets: []config.HTTPTargetConfig{{URL: "http://x", RawBodyFile: "/nonexistent/path.bin"}},
	}
	_, err := buildHTTPGenerator(cfg)
	if err == nil {
		t.Fatal("expected an error for a missing raw_body_file")
	}
}

func TestRunCommandFlowRawBodyFileSentUnrendered(t *testing.T) {
	t.Chdir(t.TempDir())
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rawFile := filepath.Join(t.TempDir(), "payload.bin")
	raw := []byte(`literal {{ not a template }} bytes`)
	if err := os.WriteFile(rawFile, raw, 0o644); err != nil {
		t.Fatalf("writing temp raw body file: %v", err)
	}

	path := writeTempScenario(t, `
protocol: http
load:
  requests: 1
  workers: 1
http:
  targets:
    - method: POST
      url: `+srv.URL+`
      raw_body_file: "`+rawFile+`"
`)

	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet"})

	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotBody != string(raw) {
		t.Errorf("server received body %q, want %q (unrendered)", gotBody, string(raw))
	}
}

func TestHTTPOptionsFromDefaults(t *testing.T) {
	opts, err := httpOptionsFrom(config.HTTPConfig{})
	if err != nil {
		t.Fatalf("httpOptionsFrom error: %v", err)
	}
	if !opts.FollowRedirects {
		t.Error("FollowRedirects should default to true (NoFollowRedirects unset)")
	}
	if opts.Timeout != generator.DefaultHTTPOptions().Timeout {
		t.Errorf("Timeout = %v, want the package default", opts.Timeout)
	}
}

func TestHTTPOptionsFromOverrides(t *testing.T) {
	cfg := config.HTTPConfig{Timeout: "5s", Insecure: true, NoFollowRedirects: true}
	opts, err := httpOptionsFrom(cfg)
	if err != nil {
		t.Fatalf("httpOptionsFrom error: %v", err)
	}
	if opts.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", opts.Timeout)
	}
	if !opts.Insecure {
		t.Error("Insecure = false, want true")
	}
	if opts.FollowRedirects {
		t.Error("FollowRedirects = true, want false (NoFollowRedirects set)")
	}
}

func TestHTTPOptionsFromInvalidTimeout(t *testing.T) {
	_, err := httpOptionsFrom(config.HTTPConfig{Timeout: "not-a-duration"})
	if err == nil {
		t.Fatal("expected an error for an unparseable http.timeout")
	}
}

func TestHTTPOptionsFromLoadsFeeder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.csv")
	if err := os.WriteFile(path, []byte("email\nalice@example.com\n"), 0o644); err != nil {
		t.Fatalf("writing temp csv: %v", err)
	}
	opts, err := httpOptionsFrom(config.HTTPConfig{Feeder: config.FeederConfig{File: path}})
	if err != nil {
		t.Fatalf("httpOptionsFrom error: %v", err)
	}
	if opts.Feeder == nil {
		t.Fatal("Feeder is nil, want it loaded from http.feeder.file")
	}
	if got := opts.Feeder.Next()["email"]; got != "alice@example.com" {
		t.Errorf("Feeder.Next()[\"email\"] = %q, want alice@example.com", got)
	}
}

func TestHTTPOptionsFromInvalidFeederErrors(t *testing.T) {
	_, err := httpOptionsFrom(config.HTTPConfig{Feeder: config.FeederConfig{File: "/nonexistent/users.csv"}})
	if err == nil {
		t.Fatal("expected an error for a missing http.feeder.file")
	}
}

func TestResolveStepsPropagatesExtract(t *testing.T) {
	steps, err := resolveSteps([]config.HTTPTargetConfig{
		{Method: "POST", URL: "http://x/login", Extract: map[string]string{"token": "json:token"}},
	})
	if err != nil {
		t.Fatalf("resolveSteps error: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(steps))
	}
	if steps[0].Extract["token"] != "json:token" {
		t.Errorf("Extract = %+v, want token=json:token", steps[0].Extract)
	}
}

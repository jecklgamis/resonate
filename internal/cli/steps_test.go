package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/report"
)

// stepServer answers /fast immediately and /slow after 30ms, counting hits.
func stepServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/slow" {
			time.Sleep(30 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runScenario runs a scenario through the real run command with its report
// files redirected into a temp dir, returning the error and the JSON report
// path.
func runScenario(t *testing.T, yaml string) (error, string) {
	t.Helper()
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "report.json")
	path := writeTempScenario(t, yaml)
	cmd := newRunCommand()
	cmd.SetArgs([]string{path, "--quiet", "--html-report", "", "--results-file", "", "--json-report", jsonPath})
	var err error
	captureStdout(t, func() { captureStderr(t, func() { err = cmd.Execute() }) })
	return err, jsonPath
}

func TestRunScenarioBreaksDownStepsAndAssertsPerStep(t *testing.T) {
	var hits atomic.Int64
	srv := stepServer(t, &hits)
	err, jsonPath := runScenario(t, `
protocol: http
load:
  requests: 20
  workers: 2
http:
  flow:
    - name: fast-step
      url: `+srv.URL+`/fast
    - name: slow-step
      url: `+srv.URL+`/slow
assertions:
  - metric: latency_p50
    step: fast-step
    max: 20ms
`)
	if err != nil {
		t.Fatalf("the fast step is well under 20ms, but: %v", err)
	}

	f, _ := os.Open(jsonPath)
	defer f.Close()
	s, rerr := report.ReadSummary(f)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(s.Steps) != 2 || s.Steps[0].Name != "fast-step" || s.Steps[1].Name != "slow-step" {
		t.Fatalf("steps = %+v", s.Steps)
	}
	if s.Steps[0].Requests != 20 || s.Steps[1].Requests != 20 {
		t.Errorf("per-step requests = %d, %d; want 20 each", s.Steps[0].Requests, s.Steps[1].Requests)
	}
	if s.Steps[1].Latencies.P50 < 25*time.Millisecond || s.Steps[0].Latencies.P50 > 20*time.Millisecond {
		t.Errorf("the steps' latencies are blended: fast p50 %s, slow p50 %s", s.Steps[0].Latencies.P50, s.Steps[1].Latencies.P50)
	}
}

func TestRunScenarioFailsOnAStepAssertion(t *testing.T) {
	var hits atomic.Int64
	srv := stepServer(t, &hits)
	err, _ := runScenario(t, `
protocol: http
load:
  requests: 10
  workers: 2
http:
  flow:
    - name: fast-step
      url: `+srv.URL+`/fast
    - name: slow-step
      url: `+srv.URL+`/slow
assertions:
  - metric: latency_p50
    step: slow-step
    max: 5ms
`)
	if err == nil || !strings.Contains(err.Error(), "assertion") {
		t.Errorf("err = %v, want the slow step's assertion to fail the run", err)
	}
}

func TestRunScenarioRejectsAssertionOnUnknownStepBeforeRunning(t *testing.T) {
	var hits atomic.Int64
	srv := stepServer(t, &hits)
	err, _ := runScenario(t, `
protocol: http
load:
  requests: 10
  workers: 2
http:
  targets:
    - name: home
      url: `+srv.URL+`/fast
assertions:
  - metric: latency_p95
    step: hmoe
    max: 1s
`)
	if err == nil || !strings.Contains(err.Error(), `unknown step "hmoe"`) || !strings.Contains(err.Error(), "home") {
		t.Fatalf("err = %v, want the typo named along with the real steps", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests were sent before the typo was caught", n)
	}
}

func TestRunScenarioUnnamedStepsUseMethodAndURL(t *testing.T) {
	var hits atomic.Int64
	srv := stepServer(t, &hits)
	err, jsonPath := runScenario(t, `
protocol: http
load:
  requests: 4
  workers: 1
http:
  targets:
    - url: `+srv.URL+`/fast?token=secret
assertions:
  - metric: success_rate
    step: "GET `+srv.URL+`/fast"
    min: 1
`)
	if err != nil {
		t.Fatalf("the default step name should be METHOD url without the query: %v", err)
	}
	data, _ := os.ReadFile(jsonPath)
	if strings.Contains(string(data), "secret") {
		t.Error("the query string (a possible secret) leaked into the JSON report via a step name")
	}
}

func TestRunScenarioRejectsNameOnControlBlock(t *testing.T) {
	err, _ := runScenario(t, `
protocol: http
load:
  requests: 1
http:
  flow:
    - name: loop
      repeat: 2
      steps:
        - url: http://localhost:1/x
`)
	if err == nil || !strings.Contains(err.Error(), "can't have a name") {
		t.Errorf("err = %v", err)
	}
}

func TestRunScenarioAbortStopsEarlyAndFailsTheRun(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	err, jsonPath := runScenario(t, `
protocol: http
load:
  duration: 1m
  rate: 200
  workers: 4
http:
  targets:
    - url: `+srv.URL+`/
assertions:
  - metric: success_rate
    min: 0.9
    abort: true
    abort_after: 0s
`)
	if time.Since(start) > 15*time.Second {
		t.Fatalf("a 1m run that is failing from the first request took %s", time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "run aborted") {
		t.Fatalf("err = %v, want the run to fail as aborted", err)
	}
	data, _ := os.ReadFile(jsonPath)
	if !strings.Contains(string(data), `"aborted"`) {
		t.Error("the JSON report doesn't record that the run was aborted")
	}
}

func TestRunScenarioAbortLeavesAHealthyRunAlone(t *testing.T) {
	var hits atomic.Int64
	srv := stepServer(t, &hits)
	err, _ := runScenario(t, `
protocol: http
load:
  requests: 50
  workers: 2
http:
  targets:
    - url: `+srv.URL+`/fast
assertions:
  - metric: success_rate
    min: 0.9
    abort: true
    abort_after: 0s
`)
	if err != nil {
		t.Errorf("a healthy run was failed: %v", err)
	}
	if n := hits.Load(); n != 50 {
		t.Errorf("%d of 50 requests ran", n)
	}
}

func TestAssertionsFromAbortSettings(t *testing.T) {
	cfgs := func(c string) error {
		_, err := parseAssertionsForTest(t, "protocol: http\nassertions:\n"+c)
		return err
	}
	if err := cfgs("  - {metric: rate, min: '1', abort_after: 10s}\n"); err == nil || !strings.Contains(err.Error(), "abort_after only applies with abort") {
		t.Errorf("abort_after without abort: err = %v", err)
	}
	if err := cfgs("  - {metric: rate, min: '1', abort: true, abort_after: soon}\n"); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Errorf("bad abort_after: err = %v", err)
	}
	if err := cfgs("  - {metric: rate, min: '1', abort: true, abort_after: -1s}\n"); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("negative abort_after: err = %v", err)
	}
}

func TestAbortAfterDefaultsToFiveSeconds(t *testing.T) {
	as, err := parseAssertionsForTest(t, "protocol: http\nassertions:\n  - {metric: rate, min: '1', abort: true}\n  - {metric: rate, min: '1'}\n")
	if err != nil {
		t.Fatal(err)
	}
	if as[0].AbortAfter != 5*time.Second {
		t.Errorf("AbortAfter = %s, want the 5s warm-up default so the first noisy requests can't abort a run", as[0].AbortAfter)
	}
	if as[1].Abort || as[1].AbortAfter != 0 {
		t.Errorf("a plain assertion picked up abort settings: %+v", as[1])
	}
}

func TestRunScenarioRejectsAbortOnACount(t *testing.T) {
	err, _ := runScenario(t, `
protocol: http
load:
  requests: 1
http:
  targets:
    - url: http://localhost:1/
assertions:
  - metric: requests
    min: 100
    abort: true
`)
	if err == nil || !strings.Contains(err.Error(), "can only be judged when the run ends") {
		t.Errorf("err = %v", err)
	}
}

// parseAssertionsForTest loads a scenario and converts just its assertions, the
// step the run command does before building anything.
func parseAssertionsForTest(t *testing.T, yaml string) ([]report.Assertion, error) {
	t.Helper()
	sc, err := config.Load(writeTempScenario(t, yaml))
	if err != nil {
		t.Fatalf("loading scenario: %v", err)
	}
	return assertionsFrom(sc.Assertions)
}

// ---- compare command ----

func writeReport(t *testing.T, s report.Summary) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "r.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := s.PrintJSON(f); err != nil {
		t.Fatal(err)
	}
	return path
}

func summaryFor(p95 time.Duration, rate float64) report.Summary {
	return report.Summary{
		Requests: 1000, Success: 1000, SuccessRate: 1, Rate: rate, Duration: 10 * time.Second,
		Latencies: report.LatencyStats{P50: p95 / 2, P95: p95, P99: p95 * 2, Max: p95 * 3},
		Steps: []report.StepSummary{{Name: "login", Requests: 10, SuccessRate: 1,
			Latencies: report.LatencyStats{P95: p95 / 4}}},
	}
}

func runCompare(args ...string) (stdout, stderr string, err error) {
	cmd := newCompareCommand()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestCompareCommandPrintsTableAndPassesWithoutRules(t *testing.T) {
	base := writeReport(t, summaryFor(100*time.Millisecond, 100))
	cur := writeReport(t, summaryFor(300*time.Millisecond, 50))
	out, _, err := runCompare(base, cur)
	if err != nil {
		t.Fatalf("no --fail-on means a report only: %v", err)
	}
	for _, want := range []string{"latency_p95", "+200.0%", "login:latency_p95", "rate", "-50.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCompareCommandFailsOnARegression(t *testing.T) {
	base := writeReport(t, summaryFor(100*time.Millisecond, 100))
	cur := writeReport(t, summaryFor(150*time.Millisecond, 100))
	out, _, err := runCompare(base, cur, "--fail-on", "latency_p95>10%", "--fail-on", "rate<-5%")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 regression rule(s) matched") {
		t.Fatalf("err = %v, want exactly the p95 rule to match", err)
	}
	if !strings.Contains(out, "FAIL  latency_p95>10%") || !strings.Contains(out, "ok    rate<-5%") {
		t.Errorf("verdicts missing:\n%s", out)
	}
}

func TestCompareCommandPassesWithinBudget(t *testing.T) {
	base := writeReport(t, summaryFor(100*time.Millisecond, 100))
	cur := writeReport(t, summaryFor(105*time.Millisecond, 99))
	if _, _, err := runCompare(base, cur, "--fail-on", "latency_p95>10%", "--fail-on", "rate<-5%"); err != nil {
		t.Errorf("a 5%% p95 and 1%% rate change are inside the budget: %v", err)
	}
}

func TestCompareCommandGatesOnAStep(t *testing.T) {
	base := writeReport(t, summaryFor(100*time.Millisecond, 100))
	cur := writeReport(t, summaryFor(150*time.Millisecond, 100))
	if _, _, err := runCompare(base, cur, "--fail-on", "login:latency_p95>10%"); err == nil {
		t.Error("the login step's p95 grew 50%; the rule should match")
	}
}

func TestCompareCommandRejectsBadInput(t *testing.T) {
	good := writeReport(t, summaryFor(time.Second, 1))
	junk := filepath.Join(t.TempDir(), "junk.json")
	os.WriteFile(junk, []byte(`{"hello":"world"}`), 0o644)

	cases := map[string][]string{
		"bad rule":       {good, good, "--fail-on", "latency_p95"},
		"unknown metric": {good, good, "--fail-on", "bogus>10%"},
		"not a report":   {good, junk},
		"missing file":   {good, filepath.Join(t.TempDir(), "nope.json")},
		"one argument":   {good},
		"missing step":   {good, good, "--fail-on", "checkout:latency_p95>10%"},
	}
	for name, args := range cases {
		if _, _, err := runCompare(args...); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestCompareCommandWarnsAboutAnAbortedRun(t *testing.T) {
	base := summaryFor(100*time.Millisecond, 100)
	cur := summaryFor(100*time.Millisecond, 100)
	cur.Aborted = "success_rate: got 0.10, want >= 0.90"
	_, stderr, err := runCompare(writeReport(t, base), writeReport(t, cur))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "current run was aborted") {
		t.Errorf("no warning that the current run was cut short:\n%s", stderr)
	}
}

func TestCompareIsRegisteredOnTheRootCommand(t *testing.T) {
	cmd, _, err := NewRootCommand("dev").Find([]string{"compare"})
	if err != nil || cmd.Name() != "compare" {
		t.Errorf("compare not registered: %v", err)
	}
}

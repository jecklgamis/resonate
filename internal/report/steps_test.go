package report

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
)

func stepResult(step string, status int, lat time.Duration) generator.Result {
	return generator.Result{Timestamp: time.Now(), Latency: lat, StatusCode: status, Step: step}
}

// twoStepSummary: "login" fast and always ok, "checkout" slow with 1 in 4 failing.
func twoStepSummary() Summary {
	a := NewAggregator()
	for i := 0; i < 100; i++ {
		a.Add(stepResult("login", 200, 10*time.Millisecond))
	}
	for i := 0; i < 100; i++ {
		status := 200
		if i%4 == 0 {
			status = 500
		}
		a.Add(stepResult("checkout", status, 200*time.Millisecond))
	}
	return a.Summary(10 * time.Second)
}

func TestAggregatorBreaksDownByStepInFirstSeenOrder(t *testing.T) {
	s := twoStepSummary()
	if len(s.Steps) != 2 || s.Steps[0].Name != "login" || s.Steps[1].Name != "checkout" {
		t.Fatalf("steps = %+v, want login then checkout", s.Steps)
	}
	login, checkout := s.Steps[0], s.Steps[1]
	if login.Requests != 100 || login.SuccessRate != 1 {
		t.Errorf("login = %d requests, %.2f success; want 100, 1", login.Requests, login.SuccessRate)
	}
	if checkout.Requests != 100 || checkout.SuccessRate != 0.75 {
		t.Errorf("checkout = %d requests, %.2f success; want 100, 0.75", checkout.Requests, checkout.SuccessRate)
	}
	if login.Latencies.P95 > 20*time.Millisecond || checkout.Latencies.P95 < 150*time.Millisecond {
		t.Errorf("per-step latencies are blended: login p95 %s, checkout p95 %s", login.Latencies.P95, checkout.Latencies.P95)
	}
	if checkout.StatusCodes[500] != 25 || checkout.StatusCodes[200] != 75 {
		t.Errorf("checkout status codes = %v", checkout.StatusCodes)
	}
	if s.Requests != 200 {
		t.Errorf("run total = %d, want 200 (steps must not change the totals)", s.Requests)
	}
	if got := login.Rate; got < 9.9 || got > 10.1 {
		t.Errorf("login rate = %v, want ~10/s over the 10s run", got)
	}
}

func TestAggregatorCountsStepTransportErrorsAsFailuresWithoutLatency(t *testing.T) {
	a := NewAggregator()
	a.Add(stepResult("api", 200, 5*time.Millisecond))
	a.Add(generator.Result{Timestamp: time.Now(), Error: errors.New("connection refused"), Step: "api"})
	st := a.Summary(time.Second).Steps[0]
	if st.Requests != 2 || st.Success != 1 || st.SuccessRate != 0.5 {
		t.Errorf("step = %+v, want 2 requests, 1 success", st)
	}
	if st.Errors["connection refused"] != 1 {
		t.Errorf("errors = %v", st.Errors)
	}
}

func TestResultsWithoutAStepProduceNoBreakdown(t *testing.T) {
	a := NewAggregator()
	a.Add(generator.Result{Timestamp: time.Now(), Latency: time.Millisecond, StatusCode: 200})
	if s := a.Summary(time.Second); len(s.Steps) != 0 {
		t.Errorf("Steps = %+v for results with no Step (WebSocket/gRPC/SSE)", s.Steps)
	}
}

func TestSnapshotSkipsTheExpensiveSeries(t *testing.T) {
	a := NewAggregator()
	for i := 0; i < 50; i++ {
		a.Add(stepResult("x", 200, time.Millisecond))
	}
	snap := a.Snapshot(5 * time.Second)
	if snap.Requests != 50 || len(snap.Steps) != 1 || snap.Latencies.P95 == 0 {
		t.Errorf("snapshot lost the headline numbers: %+v", snap)
	}
	if snap.TimeSeries != nil || snap.LatencyHistogram != nil {
		t.Error("Snapshot built the time series/histogram; it runs every second during a run and must stay cheap")
	}
	if full := a.Summary(5 * time.Second); len(full.TimeSeries) == 0 {
		t.Error("Summary must still include the time series")
	}
}

func TestEvaluateStepScopedAssertion(t *testing.T) {
	s := twoStepSummary()

	// The same metric passes for one step and fails for the other: the whole
	// run's blended p95 (~200ms) would have hidden which one is slow.
	fails, err := s.Evaluate([]Assertion{
		{Metric: "latency_p95", Step: "login", Max: f64(0.050)},
		{Metric: "latency_p95", Step: "checkout", Max: f64(0.050)},
		{Metric: "success_rate", Step: "checkout", Min: f64(0.99)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 2 {
		t.Fatalf("failures = %v, want checkout's two", fails)
	}
	for _, f := range fails {
		if f.Step != "checkout" {
			t.Errorf("failure attributed to %q, want checkout", f.Step)
		}
	}
	if got := fails[0].String(); !strings.Contains(got, "latency_p95{step=checkout}") {
		t.Errorf("failure text %q doesn't name the step", got)
	}
}

func TestEvaluateStepAssertionAcceptsArbitraryPercentile(t *testing.T) {
	s := twoStepSummary()
	fails, err := s.Evaluate([]Assertion{{Metric: "latency_p99.9", Step: "checkout", Max: f64(0.050)}})
	if err != nil || len(fails) != 1 {
		t.Errorf("fails=%v err=%v; a step has its own histogram for arbitrary percentiles", fails, err)
	}
}

func TestEvaluateStepWithNoRequestsFailsInsteadOfPassing(t *testing.T) {
	s := twoStepSummary()
	fails, err := s.Evaluate([]Assertion{{Metric: "latency_p95", Step: "nope", Max: f64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 1 || !strings.Contains(fails[0].String(), `step "nope" recorded no requests`) {
		t.Errorf("failures = %v; an assertion on a step that never ran must not pass vacuously", fails)
	}
	if !strings.Contains(fails[0].String(), "login") {
		t.Errorf("failure %q should list the steps that did run", fails[0])
	}
}

func TestEvaluateStepAssertionUnknownMetricIsAnError(t *testing.T) {
	_, err := twoStepSummary().Evaluate([]Assertion{{Metric: "bogus", Step: "nope", Max: f64(1)}})
	if err == nil {
		t.Error("an unknown metric must be an error even when the step is also missing")
	}
}

func TestEvaluatePartialSkipsStepsNotYetSeen(t *testing.T) {
	s := twoStepSummary()
	rules := []Assertion{{Metric: "success_rate", Step: "later", Min: f64(0.99)}}
	if fails, _ := s.EvaluatePartial(rules); len(fails) != 0 {
		t.Errorf("EvaluatePartial failed a step that has not started yet: %v", fails)
	}
	if fails, _ := s.Evaluate(rules); len(fails) != 1 {
		t.Errorf("Evaluate must still fail it once the run is over, got %v", fails)
	}
}

func TestValidateSteps(t *testing.T) {
	as := []Assertion{{Metric: "rate", Step: "login", Min: f64(1)}}
	if err := ValidateSteps(as, []string{"login", "home"}, true); err != nil {
		t.Errorf("known step rejected: %v", err)
	}
	err := ValidateSteps(as, []string{"logn"}, true)
	if err == nil || !strings.Contains(err.Error(), `unknown step "login"`) || !strings.Contains(err.Error(), "logn") {
		t.Errorf("err = %v, want unknown step naming the real ones", err)
	}
	if err := ValidateSteps(as, nil, false); err == nil || !strings.Contains(err.Error(), "only apply to http") {
		t.Errorf("err = %v, want per-step assertions rejected for a protocol with no steps", err)
	}
	if err := ValidateSteps([]Assertion{{Metric: "rate", Min: f64(1)}}, nil, false); err != nil {
		t.Errorf("a run-level assertion must never need steps: %v", err)
	}
}

func TestPrintShowsStepTableOnlyWithMoreThanOneStep(t *testing.T) {
	var two bytes.Buffer
	twoStepSummary().Print(&two)
	if !strings.Contains(two.String(), "Steps:") || !strings.Contains(two.String(), "checkout") {
		t.Errorf("two-step report has no step table:\n%s", two.String())
	}

	a := NewAggregator()
	a.Add(stepResult("only", 200, time.Millisecond))
	var one bytes.Buffer
	a.Summary(time.Second).Print(&one)
	if strings.Contains(one.String(), "Steps:") {
		t.Errorf("a single step just repeats the totals and should not get a table:\n%s", one.String())
	}
}

func TestHTMLReportShowsStepsAndLabelsStepAssertions(t *testing.T) {
	s := twoStepSummary()
	as := []Assertion{{Metric: "latency_p95", Step: "checkout", Max: f64(0.050)}}
	fails, _ := s.Evaluate(as)
	var buf bytes.Buffer
	if err := WriteHTML(&buf, HTMLReport{Title: "t", Summary: s, Assertions: as, Failures: fails}); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"<h2>Steps</h2>", "checkout", "latency_p95{step=checkout}", "FAIL"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML report missing %q", want)
		}
	}
}

func TestHTMLReportShowsAbortBanner(t *testing.T) {
	s := twoStepSummary()
	s.Aborted = "success_rate: got 0.00, want >= 0.90"
	var buf bytes.Buffer
	if err := WriteHTML(&buf, HTMLReport{Title: "t", Summary: s}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "ABORTED") || !strings.Contains(buf.String(), "want &gt;= 0.90") {
		t.Error("an aborted run must say so in its HTML report")
	}
}

func TestPrintMentionsAbort(t *testing.T) {
	s := twoStepSummary()
	s.Aborted = "success_rate: got 0.10, want >= 0.90"
	var buf bytes.Buffer
	s.Print(&buf)
	if !strings.Contains(buf.String(), "ABORTED: success_rate") {
		t.Errorf("text report doesn't say the run was aborted:\n%s", buf.String())
	}
}

func TestSetStepOrderListsStepsInConfiguredOrder(t *testing.T) {
	a := NewAggregator()
	a.SetStepOrder([]string{"first", "second", "never-ran", "third"})
	// Results arrive in a different order than the scenario lists them.
	a.Add(stepResult("third", 200, time.Millisecond))
	a.Add(stepResult("first", 200, time.Millisecond))
	a.Add(stepResult("second", 200, time.Millisecond))
	a.Add(stepResult("surprise", 200, time.Millisecond))

	var got []string
	for _, st := range a.Summary(time.Second).Steps {
		got = append(got, st.Name)
	}
	want := []string{"first", "second", "third", "surprise"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v (configured order; a step that never ran is omitted; unexpected ones last)", got, want)
	}
}

package report

import (
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
)

func f64(v float64) *float64 { return &v }

func TestEvaluateAllPass(t *testing.T) {
	s := Summary{SuccessRate: 0.99, Rate: 55, Latencies: LatencyStats{P95: 200 * time.Millisecond}}
	failures, err := s.Evaluate([]Assertion{
		{Metric: "success_rate", Min: f64(0.95)},
		{Metric: "rate", Min: f64(50)},
		{Metric: "latency_p95", Max: f64(0.5)},
	})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none", failures)
	}
}

func TestEvaluateMinFailure(t *testing.T) {
	s := Summary{SuccessRate: 0.80}
	failures, err := s.Evaluate([]Assertion{{Metric: "success_rate", Min: f64(0.95)}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failures))
	}
	if failures[0].Metric != "success_rate" {
		t.Errorf("Metric = %q, want success_rate", failures[0].Metric)
	}
}

func TestEvaluateMaxFailure(t *testing.T) {
	s := Summary{Latencies: LatencyStats{P99: time.Second}}
	failures, err := s.Evaluate([]Assertion{{Metric: "latency_p99", Max: f64(0.5)}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failures))
	}
}

func TestEvaluateRangeBothBoundsChecked(t *testing.T) {
	s := Summary{Rate: 5}
	failures, err := s.Evaluate([]Assertion{{Metric: "rate", Min: f64(10), Max: f64(100)}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1 (below Min)", len(failures))
	}
}

func TestEvaluateGT(t *testing.T) {
	pass := Summary{Rate: 51}
	if failures, _ := pass.Evaluate([]Assertion{{Metric: "rate", GT: f64(50)}}); len(failures) != 0 {
		t.Errorf("51 > 50: failures = %+v, want none", failures)
	}
	failAtBoundary := Summary{Rate: 50}
	failures, _ := failAtBoundary.Evaluate([]Assertion{{Metric: "rate", GT: f64(50)}})
	if len(failures) != 1 {
		t.Errorf("50 > 50 is false (gt is strict): failures = %+v, want 1", failures)
	}
}

func TestEvaluateLT(t *testing.T) {
	pass := Summary{Rate: 49}
	if failures, _ := pass.Evaluate([]Assertion{{Metric: "rate", LT: f64(50)}}); len(failures) != 0 {
		t.Errorf("49 < 50: failures = %+v, want none", failures)
	}
	failAtBoundary := Summary{Rate: 50}
	failures, _ := failAtBoundary.Evaluate([]Assertion{{Metric: "rate", LT: f64(50)}})
	if len(failures) != 1 {
		t.Errorf("50 < 50 is false (lt is strict): failures = %+v, want 1", failures)
	}
}

func TestEvaluateIs(t *testing.T) {
	exact := Summary{Rate: 50}
	if failures, _ := exact.Evaluate([]Assertion{{Metric: "rate", Is: f64(50)}}); len(failures) != 0 {
		t.Errorf("50 == 50: failures = %+v, want none", failures)
	}
	off := Summary{Rate: 50.0001}
	if failures, _ := off.Evaluate([]Assertion{{Metric: "rate", Is: f64(50)}}); len(failures) != 1 {
		t.Error("50.0001 != 50, want a failure")
	}
}

func TestEvaluateIn(t *testing.T) {
	member := Summary{Rate: 100}
	if failures, _ := member.Evaluate([]Assertion{{Metric: "rate", In: []float64{50, 75, 100}}}); len(failures) != 0 {
		t.Errorf("100 in [50,75,100]: failures = %+v, want none", failures)
	}
	notMember := Summary{Rate: 60}
	if failures, _ := notMember.Evaluate([]Assertion{{Metric: "rate", In: []float64{50, 75, 100}}}); len(failures) != 1 {
		t.Error("60 not in [50,75,100], want a failure")
	}
}

func TestEvaluateAroundInclusive(t *testing.T) {
	s := Summary{Rate: 55}
	// Inclusive: exactly at the boundary (50±5 => 45..55) passes.
	if failures, _ := s.Evaluate([]Assertion{{Metric: "rate", Around: f64(50), AroundMargin: f64(5)}}); len(failures) != 0 {
		t.Errorf("55 in [45,55] inclusive: failures = %+v, want none", failures)
	}
}

func TestEvaluateAroundExclusive(t *testing.T) {
	s := Summary{Rate: 55}
	failures, _ := s.Evaluate([]Assertion{{Metric: "rate", Around: f64(50), AroundMargin: f64(5), AroundExclusive: true}})
	if len(failures) != 1 {
		t.Error("55 at the exclusive boundary of [45,55], want a failure")
	}
}

func TestEvaluateAroundRequiresBothFields(t *testing.T) {
	// Around alone, with no AroundMargin, has no effect (nothing to
	// bound against) — same "both or neither" contract as
	// DeviatesAround/DeviatesPercent.
	s := Summary{Rate: 1000000}
	failures, err := s.Evaluate([]Assertion{{Metric: "rate", Around: f64(50)}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none (Around without AroundMargin is inert)", failures)
	}
}

func TestEvaluateDeviatesAroundInclusive(t *testing.T) {
	// target 100, 10% => [90,110]; 110 is exactly at the inclusive bound.
	s := Summary{Rate: 110}
	failures, _ := s.Evaluate([]Assertion{{Metric: "rate", DeviatesAround: f64(100), DeviatesPercent: f64(10)}})
	if len(failures) != 0 {
		t.Errorf("110 within 10%% of 100 inclusive: failures = %+v, want none", failures)
	}
}

func TestEvaluateDeviatesAroundExclusive(t *testing.T) {
	s := Summary{Rate: 110}
	failures, _ := s.Evaluate([]Assertion{{Metric: "rate", DeviatesAround: f64(100), DeviatesPercent: f64(10), DeviatesExclusive: true}})
	if len(failures) != 1 {
		t.Error("110 at the exclusive 10% boundary of 100, want a failure")
	}
}

func TestEvaluateDeviatesAroundOutOfRange(t *testing.T) {
	s := Summary{Rate: 150}
	failures, _ := s.Evaluate([]Assertion{{Metric: "rate", DeviatesAround: f64(100), DeviatesPercent: f64(10)}})
	if len(failures) != 1 {
		t.Error("150 is well outside 10% of 100, want a failure")
	}
}

func TestEvaluateErrorRateDerivedFromSuccessRate(t *testing.T) {
	s := Summary{SuccessRate: 0.90}
	failures, err := s.Evaluate([]Assertion{{Metric: "error_rate", Max: f64(0.05)}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1 (error_rate 0.10 > max 0.05)", len(failures))
	}
}

func TestEvaluateUnknownMetricErrors(t *testing.T) {
	_, err := (Summary{}).Evaluate([]Assertion{{Metric: "bogus", Min: f64(1)}})
	if err == nil {
		t.Fatal("expected an error for an unknown metric")
	}
}

func TestEvaluateEmptyAssertionsNoFailures(t *testing.T) {
	failures, err := (Summary{}).Evaluate(nil)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none", failures)
	}
}

func TestFailureStringLatencyFormatsAsHumanDuration(t *testing.T) {
	a := Assertion{Metric: "latency_p95", Max: f64(0.001)}
	f := Failure{Metric: a.Metric, Actual: 0.001552875, Reasons: a.describe()}
	got := f.String()
	if !strings.Contains(got, "1.552875ms") || !strings.Contains(got, "1ms") {
		t.Errorf("String() = %q, want human-readable durations, not raw float seconds", got)
	}
}

func TestFailureStringNonLatencyFormatsAsNumber(t *testing.T) {
	a := Assertion{Metric: "success_rate", Min: f64(0.95)}
	f := Failure{Metric: a.Metric, Actual: 0.8, Reasons: a.describe()}
	got := f.String()
	if !strings.Contains(got, "0.8") || !strings.Contains(got, "0.95") {
		t.Errorf("String() = %q, want plain numbers for a non-latency metric", got)
	}
}

func TestFailureStringRangeMentionsBothBounds(t *testing.T) {
	a := Assertion{Metric: "rate", Min: f64(10), Max: f64(100)}
	f := Failure{Metric: a.Metric, Actual: 5, Reasons: a.describe()}
	got := f.String()
	if !strings.Contains(got, ">= 10") || !strings.Contains(got, "<= 100") {
		t.Errorf("String() = %q, want it to mention both bounds when Min and Max are both set", got)
	}
}

func TestIsLatencyMetric(t *testing.T) {
	for _, m := range []string{"latency_min", "latency_mean", "latency_stddev", "latency_p50", "latency_p90", "latency_p95", "latency_p99", "latency_max"} {
		if !IsLatencyMetric(m) {
			t.Errorf("IsLatencyMetric(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"rate", "success_rate", "error_rate", "requests", "success"} {
		if IsLatencyMetric(m) {
			t.Errorf("IsLatencyMetric(%q) = true, want false", m)
		}
	}
}

func TestIsLatencyMetricArbitraryPercentile(t *testing.T) {
	for _, m := range []string{"latency_p99.9", "latency_p0", "latency_p100", "latency_p33.33"} {
		if !IsLatencyMetric(m) {
			t.Errorf("IsLatencyMetric(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"latency_p101", "latency_p-1", "latency_pabc", "latency_p"} {
		if IsLatencyMetric(m) {
			t.Errorf("IsLatencyMetric(%q) = true, want false (out of range or malformed)", m)
		}
	}
}

func TestIsKnownAssertionMetric(t *testing.T) {
	for _, m := range append(append([]string{}, AssertionMetrics...), "latency_p99.9", "latency_p33.33") {
		if !IsKnownAssertionMetric(m) {
			t.Errorf("IsKnownAssertionMetric(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"bogus", "latency_p101", "latency_pxyz"} {
		if IsKnownAssertionMetric(m) {
			t.Errorf("IsKnownAssertionMetric(%q) = true, want false", m)
		}
	}
}

func TestEvaluateArbitraryPercentileUsesAggregatedLatencies(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	for i := 1; i <= 100; i++ {
		agg.Add(generator.Result{Timestamp: now, Latency: time.Duration(i) * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)

	failures, err := s.Evaluate([]Assertion{{Metric: "latency_p99.9", Max: f64(1.0)}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %+v, want none (p99.9 of 1-100ms is well under 1s)", failures)
	}

	failures, err = s.Evaluate([]Assertion{{Metric: "latency_p99.9", Max: f64(0.05)}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 1 {
		t.Errorf("failures = %+v, want 1 (p99.9 of 1-100ms exceeds 50ms)", failures)
	}
}

func TestEvaluateArbitraryPercentileOnHandBuiltSummaryIsZero(t *testing.T) {
	// A hand-built Summary (no sortedLatencies) can't compute an arbitrary
	// percentile beyond the four fixed ones — documented limitation.
	s := Summary{}
	v, err := s.metricValue("latency_p42")
	if err != nil {
		t.Fatalf("metricValue error: %v", err)
	}
	if v != 0 {
		t.Errorf("got %v, want 0 for a hand-built Summary with no sortedLatencies", v)
	}
}

func TestParseThresholdDuration(t *testing.T) {
	v, err := ParseThreshold("latency_p95", "500ms")
	if err != nil {
		t.Fatalf("ParseThreshold error: %v", err)
	}
	if v != 0.5 {
		t.Errorf("got %v, want 0.5 (seconds)", v)
	}
}

func TestParseThresholdInvalidDuration(t *testing.T) {
	_, err := ParseThreshold("latency_p95", "not-a-duration")
	if err == nil {
		t.Fatal("expected an error for an invalid duration threshold")
	}
}

func TestParseThresholdNumber(t *testing.T) {
	v, err := ParseThreshold("success_rate", "0.95")
	if err != nil {
		t.Fatalf("ParseThreshold error: %v", err)
	}
	if v != 0.95 {
		t.Errorf("got %v, want 0.95", v)
	}
}

func TestParseThresholdInvalidNumber(t *testing.T) {
	_, err := ParseThreshold("rate", "not-a-number")
	if err == nil {
		t.Fatal("expected an error for an invalid numeric threshold")
	}
}

func TestMetricValueCoversAllDocumentedMetrics(t *testing.T) {
	s := Summary{
		Requests:    100,
		Success:     95,
		SuccessRate: 0.95,
		Rate:        50,
		Latencies: LatencyStats{
			Min: time.Millisecond, Mean: 2 * time.Millisecond, P50: 3 * time.Millisecond,
			P90: 4 * time.Millisecond, P95: 5 * time.Millisecond, P99: 6 * time.Millisecond, Max: 7 * time.Millisecond,
		},
	}
	for _, m := range AssertionMetrics {
		if _, err := s.metricValue(m); err != nil {
			t.Errorf("metricValue(%q) unexpectedly errored: %v", m, err)
		}
	}
}

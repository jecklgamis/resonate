package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/engine"
	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

func TestParseHeaders(t *testing.T) {
	got, err := parseHeaders([]string{"Content-Type: application/json", "X-Token:  abc123  "})
	if err != nil {
		t.Fatalf("parseHeaders error: %v", err)
	}
	if got["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got["Content-Type"])
	}
	if got["X-Token"] != "abc123" {
		t.Errorf("X-Token = %q, want abc123 (surrounding whitespace trimmed)", got["X-Token"])
	}
}

func TestParseHeadersInvalid(t *testing.T) {
	_, err := parseHeaders([]string{"no-colon-here"})
	if err == nil {
		t.Fatal("expected an error for a header with no colon")
	}
}

func TestParseHeadersEmpty(t *testing.T) {
	got, err := parseHeaders(nil)
	if err != nil {
		t.Fatalf("parseHeaders error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty map", got)
	}
}

func TestParseHeadersValueMayContainColon(t *testing.T) {
	// Templated values like "{{now "15:04:05"}}" contain colons; only the
	// first colon should split key from value.
	got, err := parseHeaders([]string{"X-Time: 10:20:30"})
	if err != nil {
		t.Fatalf("parseHeaders error: %v", err)
	}
	if got["X-Time"] != "10:20:30" {
		t.Errorf("X-Time = %q, want 10:20:30 (only first colon splits)", got["X-Time"])
	}
}

func TestParseQuery(t *testing.T) {
	got, err := parseQuery([]string{"seq={{.Seq}}", "page=2"})
	if err != nil {
		t.Fatalf("parseQuery error: %v", err)
	}
	if got["seq"] != "{{.Seq}}" {
		t.Errorf("seq = %q, want {{.Seq}}", got["seq"])
	}
	if got["page"] != "2" {
		t.Errorf("page = %q, want 2", got["page"])
	}
}

func TestParseQueryInvalid(t *testing.T) {
	_, err := parseQuery([]string{"no-equals-sign"})
	if err == nil {
		t.Fatal("expected an error for a query param with no '='")
	}
}

func TestParseQueryValueMayContainEquals(t *testing.T) {
	got, err := parseQuery([]string{"filter=a=b"})
	if err != nil {
		t.Fatalf("parseQuery error: %v", err)
	}
	if got["filter"] != "a=b" {
		t.Errorf("filter = %q, want a=b (only first '=' splits)", got["filter"])
	}
}

func TestParseAssertionsMin(t *testing.T) {
	got, err := parseAssertions([]string{"success_rate>=0.95"})
	if err != nil {
		t.Fatalf("parseAssertions error: %v", err)
	}
	if len(got) != 1 || got[0].Metric != "success_rate" || got[0].Min == nil || *got[0].Min != 0.95 {
		t.Errorf("got %+v, want success_rate Min=0.95", got)
	}
}

func TestParseAssertionsMax(t *testing.T) {
	got, err := parseAssertions([]string{"latency_p95<=500ms"})
	if err != nil {
		t.Fatalf("parseAssertions error: %v", err)
	}
	if len(got) != 1 || got[0].Metric != "latency_p95" || got[0].Max == nil || *got[0].Max != 0.5 {
		t.Errorf("got %+v, want latency_p95 Max=0.5 (500ms)", got)
	}
}

func TestParseAssertionsGTAndLT(t *testing.T) {
	got, err := parseAssertions([]string{"rate>10", "rate<1000"})
	if err != nil {
		t.Fatalf("parseAssertions error: %v", err)
	}
	if got[0].GT == nil || *got[0].GT != 10 {
		t.Errorf("assertion 0 = %+v, want GT=10", got[0])
	}
	if got[1].LT == nil || *got[1].LT != 1000 {
		t.Errorf("assertion 1 = %+v, want LT=1000", got[1])
	}
}

func TestParseAssertionsIs(t *testing.T) {
	got, err := parseAssertions([]string{"success_rate==1"})
	if err != nil {
		t.Fatalf("parseAssertions error: %v", err)
	}
	if got[0].Is == nil || *got[0].Is != 1 {
		t.Errorf("got %+v, want Is=1", got[0])
	}
}

func TestParseAssertionsOperatorPrecedenceGEBeforeG(t *testing.T) {
	// ">=" must not be mistaken for ">" followed by a value of "=0.95".
	got, err := parseAssertions([]string{"success_rate>=0.95"})
	if err != nil {
		t.Fatalf("parseAssertions error: %v", err)
	}
	if got[0].GT != nil {
		t.Errorf("got GT=%v set, want only Min set for \">=\"", got[0].GT)
	}
	if got[0].Min == nil || *got[0].Min != 0.95 {
		t.Errorf("got %+v, want Min=0.95", got[0])
	}
}

func TestParseAssertionsArbitraryPercentile(t *testing.T) {
	got, err := parseAssertions([]string{"latency_p99.9<=1s"})
	if err != nil {
		t.Fatalf("parseAssertions error: %v", err)
	}
	if got[0].Metric != "latency_p99.9" || got[0].Max == nil || *got[0].Max != 1 {
		t.Errorf("got %+v, want latency_p99.9 Max=1 (1s)", got[0])
	}
}

func TestParseAssertionsUnknownMetricErrors(t *testing.T) {
	_, err := parseAssertions([]string{"bogus_metric>=1"})
	if err == nil {
		t.Fatal("expected an error for an unknown metric")
	}
}

func TestParseAssertionsNoOperatorErrors(t *testing.T) {
	_, err := parseAssertions([]string{"success_rate 0.95"})
	if err == nil {
		t.Fatal("expected an error for an expression with no recognized operator")
	}
}

func TestParseAssertionsInvalidThresholdErrors(t *testing.T) {
	_, err := parseAssertions([]string{"rate>=not-a-number"})
	if err == nil {
		t.Fatal("expected an error for an unparseable threshold")
	}
}

func TestParseAssertionsEmptyMetricErrors(t *testing.T) {
	_, err := parseAssertions([]string{">=0.95"})
	if err == nil {
		t.Fatal("expected an error for an expression with no metric name")
	}
}

func TestParseAssertionsEmpty(t *testing.T) {
	got, err := parseAssertions(nil)
	if err != nil {
		t.Fatalf("parseAssertions error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestConcurrencyWarningNoRateNoWarning(t *testing.T) {
	opts := engine.Options{Workers: 5}
	summary := report.Summary{Rate: 1, Latencies: report.LatencyStats{Mean: time.Second}}
	if got := concurrencyWarning(opts, summary); got != "" {
		t.Errorf("got warning %q, want none (no target rate set)", got)
	}
}

func TestConcurrencyWarningStagedNoWarning(t *testing.T) {
	opts := engine.Options{Rate: 50, Stages: []engine.Stage{{Duration: time.Second, Workers: 10, Rate: 50}}}
	summary := report.Summary{Rate: 5, Latencies: report.LatencyStats{Mean: time.Second}}
	if got := concurrencyWarning(opts, summary); got != "" {
		t.Errorf("got warning %q, want none (staged runs aren't checked)", got)
	}
}

func TestConcurrencyWarningSustainableNoWarning(t *testing.T) {
	// 20 workers at 100ms mean latency can sustain 200/s, well above the
	// 50/s target -- no warning expected.
	opts := engine.Options{Rate: 50, Workers: 20}
	summary := report.Summary{Rate: 49, Latencies: report.LatencyStats{Mean: 100 * time.Millisecond}}
	if got := concurrencyWarning(opts, summary); got != "" {
		t.Errorf("got warning %q, want none (workers are sufficient)", got)
	}
}

func TestConcurrencyWarningStarvedWarns(t *testing.T) {
	// 5 workers at 500ms mean latency can only sustain 10/s, well below the
	// 50/s target -- expect a warning naming the shortfall.
	opts := engine.Options{Rate: 50, Workers: 5}
	summary := report.Summary{Rate: 9.7, Latencies: report.LatencyStats{Mean: 500 * time.Millisecond}}
	got := concurrencyWarning(opts, summary)
	if got == "" {
		t.Fatal("expected a warning for a concurrency-starved run")
	}
	if !strings.Contains(got, "--workers") {
		t.Errorf("warning %q should suggest a --workers value", got)
	}
}

func TestConcurrencyWarningZeroLatencyNoWarning(t *testing.T) {
	// Guards the Mean<=0 short-circuit (e.g. a run with only errors, no
	// successful latency samples) from dividing by zero.
	opts := engine.Options{Rate: 50, Workers: 5}
	summary := report.Summary{Rate: 0, Latencies: report.LatencyStats{}}
	if got := concurrencyWarning(opts, summary); got != "" {
		t.Errorf("got warning %q, want none when there's no latency data to reason about", got)
	}
}

func TestProgressPrinterOnResultCounting(t *testing.T) {
	p := newProgressPrinter()
	p.onResult(generator.Result{StatusCode: 200})
	p.onResult(generator.Result{StatusCode: 500})
	p.onResult(generator.Result{Error: assertErr})

	if p.sent.Load() != 3 {
		t.Errorf("sent = %d, want 3", p.sent.Load())
	}
	if p.ok.Load() != 1 {
		t.Errorf("ok = %d, want 1 (only the 200)", p.ok.Load())
	}
	if p.errs.Load() != 2 {
		t.Errorf("errs = %d, want 2 (the 500 and the error result)", p.errs.Load())
	}
}

func TestProgressPrinterLineFormat(t *testing.T) {
	p := newProgressPrinter()
	p.onResult(generator.Result{StatusCode: 200})
	line := p.line()
	if !strings.Contains(line, "elapsed") || !strings.Contains(line, "1 sent") || !strings.Contains(line, "1 ok") {
		t.Errorf("line() = %q, missing expected fields", line)
	}
}

var assertErr = &testError{"boom"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// fakeGenerator is a minimal in-memory Generator for exercising dryRun without
// any real network dependency.
type fakeGenerator struct {
	results []generator.Result
	closed  bool
}

func (f *fakeGenerator) Do(ctx context.Context, vuID int) []generator.Result { return f.results }
func (f *fakeGenerator) Protocol() string                                    { return "fake" }
func (f *fakeGenerator) Close() error                                        { f.closed = true; return nil }

func TestDryRunTextOutput(t *testing.T) {
	a := &fakeGenerator{results: []generator.Result{
		{Protocol: "http", StatusCode: 200, Latency: 5 * time.Millisecond, BytesIn: 10},
	}}
	out := captureStdout(t, func() {
		if err := dryRun(context.Background(), a, false); err != nil {
			t.Fatalf("dryRun error: %v", err)
		}
	})
	if !strings.Contains(out, "status=200") || !strings.Contains(out, "ok") {
		t.Errorf("dryRun text output = %q, missing expected fields", out)
	}
}

func TestDryRunJSONOutput(t *testing.T) {
	a := &fakeGenerator{results: []generator.Result{{Protocol: "http", StatusCode: 200}}}
	out := captureStdout(t, func() {
		if err := dryRun(context.Background(), a, true); err != nil {
			t.Fatalf("dryRun error: %v", err)
		}
	})
	if !strings.Contains(out, `"StatusCode": 200`) {
		t.Errorf("dryRun JSON output = %q, missing StatusCode field", out)
	}
}

func TestDryRunReportsError(t *testing.T) {
	a := &fakeGenerator{results: []generator.Result{{Protocol: "http", Error: assertErr}}}
	out := captureStdout(t, func() {
		if err := dryRun(context.Background(), a, false); err != nil {
			t.Fatalf("dryRun error: %v", err)
		}
	})
	if !strings.Contains(out, "ERROR: boom") {
		t.Errorf("dryRun output = %q, want it to surface the error", out)
	}
}

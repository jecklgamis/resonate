package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
)

func TestAggregatorSuccessAndErrorCounts(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()

	agg.Add(generator.Result{Timestamp: now, Latency: 10 * time.Millisecond, StatusCode: 200, BytesIn: 100})
	agg.Add(generator.Result{Timestamp: now, Latency: 20 * time.Millisecond, StatusCode: 201, BytesIn: 50})
	agg.Add(generator.Result{Timestamp: now, Latency: 30 * time.Millisecond, StatusCode: 500, BytesIn: 10})
	agg.Add(generator.Result{Timestamp: now, Error: errors.New("connection refused")})

	s := agg.Summary(time.Second)

	if s.Requests != 4 {
		t.Errorf("Requests = %d, want 4", s.Requests)
	}
	if s.Success != 2 {
		t.Errorf("Success = %d, want 2 (only 2xx/3xx with no error)", s.Success)
	}
	if s.SuccessRate != 0.5 {
		t.Errorf("SuccessRate = %v, want 0.5", s.SuccessRate)
	}
	if s.BytesIn != 160 {
		t.Errorf("BytesIn = %d, want 160", s.BytesIn)
	}
	if s.StatusCodes[200] != 1 || s.StatusCodes[201] != 1 || s.StatusCodes[500] != 1 {
		t.Errorf("StatusCodes = %v, want one each of 200/201/500", s.StatusCodes)
	}
	if s.Errors["connection refused"] != 1 {
		t.Errorf("Errors = %v, want one \"connection refused\"", s.Errors)
	}
}

func TestAggregatorRate(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	for i := 0; i < 10; i++ {
		agg.Add(generator.Result{Timestamp: now, Latency: time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(2 * time.Second)
	if s.Rate != 5 {
		t.Errorf("Rate = %v, want 5 (10 requests / 2s)", s.Rate)
	}
}

func TestAggregatorEmpty(t *testing.T) {
	agg := NewAggregator()
	s := agg.Summary(time.Second)
	if s.Requests != 0 {
		t.Errorf("Requests = %d, want 0", s.Requests)
	}
	if s.SuccessRate != 0 {
		t.Errorf("SuccessRate = %v, want 0 for an empty run", s.SuccessRate)
	}
	if s.Latencies.Mean != 0 {
		t.Errorf("Latencies.Mean = %v, want 0 for an empty run", s.Latencies.Mean)
	}
}

func TestLatencyStatsPercentiles(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	// 100 latencies: 1ms, 2ms, ..., 100ms.
	for i := 1; i <= 100; i++ {
		agg.Add(generator.Result{Timestamp: now, Latency: time.Duration(i) * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)

	if s.Latencies.Min != time.Millisecond {
		t.Errorf("Min = %v, want 1ms", s.Latencies.Min)
	}
	if s.Latencies.Max != 100*time.Millisecond {
		t.Errorf("Max = %v, want 100ms", s.Latencies.Max)
	}
	// Percentiles come from an HDR histogram (3 significant figures), so
	// compare within 1ms rather than exactly.
	near := func(got, want time.Duration) bool {
		d := got - want
		return d >= -time.Millisecond && d <= time.Millisecond
	}
	if !near(s.Latencies.P50, 50*time.Millisecond) {
		t.Errorf("P50 = %v, want ~50ms", s.Latencies.P50)
	}
	if !near(s.Latencies.P99, 99*time.Millisecond) {
		t.Errorf("P99 = %v, want ~99ms", s.Latencies.P99)
	}
}

func TestLatencyStatsStdDev(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	// All identical latencies: stddev should be exactly 0.
	for i := 0; i < 10; i++ {
		agg.Add(generator.Result{Timestamp: now, Latency: 50 * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)
	if s.Latencies.StdDev != 0 {
		t.Errorf("StdDev = %v, want 0 for identical latencies", s.Latencies.StdDev)
	}
}

func TestLatencyStatsStdDevNonZero(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	// [10ms, 20ms, 30ms]: mean = 20ms, population stddev = sqrt(((10)^2+0^2+10^2)/3)ms ~= 8.16ms.
	for _, ms := range []int{10, 20, 30} {
		agg.Add(generator.Result{Timestamp: now, Latency: time.Duration(ms) * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)
	want := 8163 * time.Microsecond // ~8.163ms
	diff := s.Latencies.StdDev - want
	if diff < -100*time.Microsecond || diff > 100*time.Microsecond {
		t.Errorf("StdDev = %v, want ~%v", s.Latencies.StdDev, want)
	}
}

func TestSummaryErrorsExcludedFromLatencies(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	agg.Add(generator.Result{Timestamp: now, Error: errors.New("boom")})
	s := agg.Summary(time.Second)
	if s.Latencies.Min != 0 || s.Latencies.Max != 0 {
		t.Errorf("expected zero LatencyStats when only errors were recorded, got %+v", s.Latencies)
	}
}

func TestPrintAndPrintJSONDoNotError(t *testing.T) {
	agg := NewAggregator()
	agg.Add(generator.Result{Timestamp: time.Now(), Latency: time.Millisecond, StatusCode: 200, BytesIn: 10})
	s := agg.Summary(time.Second)

	var textBuf bytes.Buffer
	s.Print(&textBuf)
	if textBuf.Len() == 0 {
		t.Error("Print produced no output")
	}

	var jsonBuf bytes.Buffer
	if err := s.PrintJSON(&jsonBuf); err != nil {
		t.Fatalf("PrintJSON error: %v", err)
	}
	if jsonBuf.Len() == 0 {
		t.Error("PrintJSON produced no output")
	}
}

func TestLatencyStatsIncludesP75BetweenP50AndP90(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	for i := 1; i <= 100; i++ { // 1ms ... 100ms
		agg.Add(generator.Result{Timestamp: now, Latency: time.Duration(i) * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)

	if d := s.Latencies.P75 - 75*time.Millisecond; d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("P75 = %v, want ~75ms", s.Latencies.P75)
	}
	if !(s.Latencies.P50 <= s.Latencies.P75 && s.Latencies.P75 <= s.Latencies.P90) {
		t.Errorf("percentiles out of order: p50=%v p75=%v p90=%v", s.Latencies.P50, s.Latencies.P75, s.Latencies.P90)
	}
	// The fixed field and the arbitrary-percentile path must agree exactly.
	if got := percentile(s.hist, 0.75); got != s.Latencies.P75 {
		t.Errorf("percentile(hist, 0.75) = %v, but P75 = %v", got, s.Latencies.P75)
	}
}

func TestSummaryJSONAndTextReportP75(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	for i := 1; i <= 100; i++ {
		agg.Add(generator.Result{Timestamp: now, Latency: time.Duration(i) * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"p75":`) {
		t.Errorf("JSON has no p75 field: %s", b)
	}

	var out bytes.Buffer
	s.Print(&out)
	if !strings.Contains(out.String(), "[min, mean, p50, p75, p90, p95, p99, max]") {
		t.Errorf("text summary header should list p75 between p50 and p90:\n%s", out.String())
	}
	line := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "Latencies") {
			line = l
		}
	}
	values := strings.Split(strings.TrimSpace(line[strings.Index(line, "]")+1:]), ", ")
	if len(values) != 8 { // min, mean, p50, p75, p90, p95, p99, max
		t.Errorf("Latencies line should carry 8 values after the header, got %d: %q", len(values), line)
	}
}

package report

import (
	"bytes"
	"errors"
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
	// p50 of a 0-indexed sorted slice of 100 via idx=int(0.5*100)=50 -> sorted[50] = 51ms (1-indexed values).
	if s.Latencies.P50 != 51*time.Millisecond {
		t.Errorf("P50 = %v, want 51ms", s.Latencies.P50)
	}
	if s.Latencies.P99 != 100*time.Millisecond {
		t.Errorf("P99 = %v, want 100ms (last bucket)", s.Latencies.P99)
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

package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
)

func TestBucketSeriesCountsRequestsAndSuccess(t *testing.T) {
	agg := NewAggregator()
	start := time.Now()
	agg.Add(generator.Result{Timestamp: start, Latency: time.Millisecond, StatusCode: 200})
	agg.Add(generator.Result{Timestamp: start.Add(500 * time.Millisecond), Latency: time.Millisecond, StatusCode: 500})
	agg.Add(generator.Result{Timestamp: start.Add(2 * time.Second), Latency: time.Millisecond, StatusCode: 200})

	s := agg.Summary(3 * time.Second)
	if len(s.TimeSeries) == 0 {
		t.Fatal("expected a non-empty TimeSeries")
	}
	total, success := 0, 0
	for _, b := range s.TimeSeries {
		total += b.Requests
		success += b.Success
	}
	if total != 3 {
		t.Errorf("total bucketed requests = %d, want 3", total)
	}
	if success != 2 {
		t.Errorf("total bucketed success = %d, want 2 (500 doesn't count)", success)
	}
}

func TestBucketSeriesEmptyWhenNoPoints(t *testing.T) {
	agg := NewAggregator()
	s := agg.Summary(time.Second)
	if s.TimeSeries != nil {
		t.Errorf("TimeSeries = %+v, want nil for an empty run", s.TimeSeries)
	}
}

func TestLatencyHistogramCoversAllSamples(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	for i := 1; i <= 50; i++ {
		agg.Add(generator.Result{Timestamp: now, Latency: time.Duration(i) * time.Millisecond, StatusCode: 200})
	}
	s := agg.Summary(time.Second)

	if len(s.LatencyHistogram) == 0 {
		t.Fatal("expected a non-empty LatencyHistogram")
	}
	total := 0
	for _, b := range s.LatencyHistogram {
		total += b.Count
	}
	if total != 50 {
		t.Errorf("histogram total = %d, want 50", total)
	}
	if s.LatencyHistogram[len(s.LatencyHistogram)-1].Le != 50*time.Millisecond {
		t.Errorf("last bin Le = %v, want 50ms (the max)", s.LatencyHistogram[len(s.LatencyHistogram)-1].Le)
	}
}

func TestLatencyHistogramSingleValue(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	agg.Add(generator.Result{Timestamp: now, Latency: 5 * time.Millisecond, StatusCode: 200})
	agg.Add(generator.Result{Timestamp: now, Latency: 5 * time.Millisecond, StatusCode: 200})
	s := agg.Summary(time.Second)

	if len(s.LatencyHistogram) != 1 {
		t.Fatalf("got %d bins, want 1 when every latency is identical", len(s.LatencyHistogram))
	}
	if s.LatencyHistogram[0].Count != 2 {
		t.Errorf("bin count = %d, want 2", s.LatencyHistogram[0].Count)
	}
}

func TestWriteHTMLProducesSelfContainedPage(t *testing.T) {
	agg := NewAggregator()
	now := time.Now()
	agg.Add(generator.Result{Timestamp: now, Latency: 10 * time.Millisecond, StatusCode: 200})
	agg.Add(generator.Result{Timestamp: now.Add(time.Second), Latency: 20 * time.Millisecond, StatusCode: 500})
	s := agg.Summary(2 * time.Second)

	min := 0.0
	max := 0.6
	var buf bytes.Buffer
	err := WriteHTML(&buf, HTMLReport{
		Title:      "https://example.com",
		Summary:    s,
		Assertions: []Assertion{{Metric: "success_rate", Min: &min, Max: &max}},
		Failures:   nil,
	})
	if err != nil {
		t.Fatalf("WriteHTML error: %v", err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Error("output does not start with a doctype")
	}
	for _, want := range []string{"<style>", "<svg", "https://example.com", "success_rate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	for _, forbidden := range []string{"cdn.", "<script src", "<link rel=\"stylesheet\""} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output unexpectedly references an external resource: %q", forbidden)
		}
	}
}

func TestWriteHTMLHandlesEmptySummary(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, HTMLReport{Title: "empty", Summary: Summary{}}); err != nil {
		t.Fatalf("WriteHTML error on empty summary: %v", err)
	}
	if !strings.Contains(buf.String(), "No data.") {
		t.Error("expected charts to render a 'No data.' placeholder for an empty summary")
	}
}

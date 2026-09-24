package resonate

import (
	"io"
	"os"

	"github.com/jecklgamis/resonate/internal/report"
)

// Summary is a point-in-time snapshot of aggregated results, returned by
// Run and by Aggregator.Summary.
type Summary = report.Summary

// LatencyStats holds latency percentiles for a Summary or TimeBucket.
type LatencyStats = report.LatencyStats

// TimeBucket aggregates results seen within one fixed-width time window
// (Summary.TimeSeries), used to draw a requests-over-time chart.
type TimeBucket = report.TimeBucket

// HistogramBin counts results whose latency fell in a response-time
// distribution bucket (Summary.LatencyHistogram).
type HistogramBin = report.HistogramBin

// Aggregator collects Results from a run and computes a Summary. Run uses
// one internally; use this directly only if you're driving a Generator
// yourself instead of going through Run.
type Aggregator = report.Aggregator

// NewAggregator returns an empty Aggregator.
func NewAggregator() *Aggregator { return report.NewAggregator() }

// Assertion checks one metric of a completed Summary against an optional
// lower and/or upper bound. See AssertionMetrics for the supported Metric
// names.
type Assertion = report.Assertion

// Failure describes one Assertion that didn't hold.
type Failure = report.Failure

// AssertionMetrics lists every metric name Assertion/Evaluate accepts.
var AssertionMetrics = report.AssertionMetrics

// IsLatencyMetric reports whether metric expects a duration-shaped
// threshold ("500ms") rather than a plain number.
func IsLatencyMetric(metric string) bool { return report.IsLatencyMetric(metric) }

// ParseThreshold parses a threshold string appropriately for metric: a
// duration ("500ms") for latency metrics, seconds otherwise.
func ParseThreshold(metric, value string) (float64, error) {
	return report.ParseThreshold(metric, value)
}

// HTMLReport is everything WriteHTML needs beyond the Summary itself.
type HTMLReport = report.HTMLReport

// WriteHTML renders a single self-contained HTML file (inline CSS, inline
// SVG charts, no external assets) summarizing r: KPI cards,
// a requests-over-time chart, a response-time distribution histogram, and
// status code/error breakdowns.
func WriteHTML(w io.Writer, r HTMLReport) error { return report.WriteHTML(w, r) }

// WriteHTMLFile is a convenience wrapper around WriteHTML that creates (or
// truncates) path, writes the report, and closes the file — collapsing
// the usual os.Create/defer Close()/WriteHTML boilerplate into one call.
func WriteHTMLFile(path string, r HTMLReport) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return report.WriteHTML(f, r)
}

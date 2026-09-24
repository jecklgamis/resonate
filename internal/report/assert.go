package report

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Assertion checks one metric of a completed Summary against one or more
// conditions (gte/lte/gt/lt/between/around/deviatesAround/is/in). At
// least one condition field should be set; several may be, combined with
// AND. Values are in the metric's natural unit — a ratio (0-1) for
// *_rate, requests/sec for rate, seconds for latency_* — callers building
// an Assertion from user input (e.g. config.AssertionConfig) are
// responsible for converting durations like "500ms" to seconds first.
type Assertion struct {
	Metric string

	// Min/Max require actual >= Min / actual <= Max; both set together
	// is an inclusive between(min, max).
	Min *float64
	Max *float64

	// GT/LT require strict inequality — unlike Min/Max, the boundary
	// value itself does not satisfy the condition.
	GT *float64
	LT *float64

	// Is requires actual to equal this value exactly.
	Is *float64

	// In requires actual to be one of these values.
	In []float64

	// Around and AroundMargin together require actual to fall within
	// [Around-AroundMargin, Around+AroundMargin] (margin is an absolute
	// amount in the metric's unit). Both must be set to take effect.
	// AroundExclusive excludes the two boundary values.
	Around          *float64
	AroundMargin    *float64
	AroundExclusive bool

	// DeviatesAround and DeviatesPercent together require actual to fall
	// within DeviatesAround ± that percentage of DeviatesAround (a
	// relative, not absolute, margin). DeviatesPercent is a percentage
	// (10 means ±10%), not a fraction. Both must be set to take effect.
	// DeviatesExclusive excludes the two boundary values.
	DeviatesAround    *float64
	DeviatesPercent   *float64
	DeviatesExclusive bool
}

// describe returns one human-readable description per condition set on a
// (regardless of whether it currently holds) — used both to explain a
// Failure and, for a still-passing assertion, to show what was checked in
// the HTML report.
func (a Assertion) describe() []string {
	fv := func(v float64) string { return formatMetricValue(a.Metric, v) }
	var d []string
	if a.Min != nil {
		d = append(d, fmt.Sprintf("want >= %s", fv(*a.Min)))
	}
	if a.Max != nil {
		d = append(d, fmt.Sprintf("want <= %s", fv(*a.Max)))
	}
	if a.GT != nil {
		d = append(d, fmt.Sprintf("want > %s", fv(*a.GT)))
	}
	if a.LT != nil {
		d = append(d, fmt.Sprintf("want < %s", fv(*a.LT)))
	}
	if a.Is != nil {
		d = append(d, fmt.Sprintf("want == %s", fv(*a.Is)))
	}
	if len(a.In) > 0 {
		vals := make([]string, len(a.In))
		for i, v := range a.In {
			vals[i] = fv(v)
		}
		d = append(d, fmt.Sprintf("want one of [%s]", strings.Join(vals, ", ")))
	}
	if a.Around != nil && a.AroundMargin != nil {
		bound := "inclusive"
		if a.AroundExclusive {
			bound = "exclusive"
		}
		d = append(d, fmt.Sprintf("want %s ± %s (%s)", fv(*a.Around), fv(*a.AroundMargin), bound))
	}
	if a.DeviatesAround != nil && a.DeviatesPercent != nil {
		bound := "inclusive"
		if a.DeviatesExclusive {
			bound = "exclusive"
		}
		d = append(d, fmt.Sprintf("want %s ± %g%% (%s)", fv(*a.DeviatesAround), *a.DeviatesPercent, bound))
	}
	return d
}

// violated reports whether actual fails at least one of a's set
// conditions.
func (a Assertion) violated(actual float64) bool {
	if a.Min != nil && actual < *a.Min {
		return true
	}
	if a.Max != nil && actual > *a.Max {
		return true
	}
	if a.GT != nil && actual <= *a.GT {
		return true
	}
	if a.LT != nil && actual >= *a.LT {
		return true
	}
	if a.Is != nil && actual != *a.Is {
		return true
	}
	if len(a.In) > 0 && !containsFloat(a.In, actual) {
		return true
	}
	if a.Around != nil && a.AroundMargin != nil {
		lo, hi := *a.Around-*a.AroundMargin, *a.Around+*a.AroundMargin
		if a.AroundExclusive {
			if actual <= lo || actual >= hi {
				return true
			}
		} else if actual < lo || actual > hi {
			return true
		}
	}
	if a.DeviatesAround != nil && a.DeviatesPercent != nil {
		margin := *a.DeviatesAround * (*a.DeviatesPercent / 100)
		lo, hi := *a.DeviatesAround-margin, *a.DeviatesAround+margin
		if a.DeviatesExclusive {
			if actual <= lo || actual >= hi {
				return true
			}
		} else if actual < lo || actual > hi {
			return true
		}
	}
	return false
}

func containsFloat(vs []float64, v float64) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// Failure describes one assertion that didn't hold.
type Failure struct {
	Metric  string
	Actual  float64
	Reasons []string // one entry per condition set on the assertion (see Assertion.describe)
}

func (f Failure) String() string {
	return fmt.Sprintf("%s: got %s, %s", f.Metric, formatMetricValue(f.Metric, f.Actual), strings.Join(f.Reasons, "; "))
}

// formatMetricValue renders v as a duration ("500ms") for a latency
// metric, or as a plain number otherwise.
func formatMetricValue(metric string, v float64) string {
	if IsLatencyMetric(metric) {
		return time.Duration(v * float64(time.Second)).String()
	}
	return fmt.Sprintf("%.2f", v)
}

// latencyPercentileMetric parses a "latency_p<N>" metric name (N a
// percentage, 0-100, e.g. "latency_p95", "latency_p99.9") — an
// arbitrary-percentile response time assertion, not just the fixed
// min/mean/p50/p90/p95/p99/max/stddev names. Returns the percentage and
// true if metric has that shape, false otherwise (including out-of-range).
func latencyPercentileMetric(metric string) (pct float64, ok bool) {
	const prefix = "latency_p"
	rest, found := strings.CutPrefix(metric, prefix)
	if !found || rest == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil || v < 0 || v > 100 {
		return 0, false
	}
	return v, true
}

// metricValue returns the metric's value in the units documented on
// Assertion, or an error if the name isn't recognized.
func (s Summary) metricValue(metric string) (float64, error) {
	switch metric {
	case "requests":
		return float64(s.Requests), nil
	case "success":
		return float64(s.Success), nil
	case "success_rate":
		return s.SuccessRate, nil
	case "error_rate":
		return 1 - s.SuccessRate, nil
	case "rate":
		return s.Rate, nil
	case "latency_min":
		return s.Latencies.Min.Seconds(), nil
	case "latency_mean":
		return s.Latencies.Mean.Seconds(), nil
	case "latency_stddev":
		return s.Latencies.StdDev.Seconds(), nil
	case "latency_p50":
		return s.Latencies.P50.Seconds(), nil
	case "latency_p90":
		return s.Latencies.P90.Seconds(), nil
	case "latency_p95":
		return s.Latencies.P95.Seconds(), nil
	case "latency_p99":
		return s.Latencies.P99.Seconds(), nil
	case "latency_max":
		return s.Latencies.Max.Seconds(), nil
	}
	// Any other "latency_p<N>" is an arbitrary percentile computed directly
	// from sortedLatencies — only populated by Aggregator.Summary, so a
	// hand-built Summary literal (e.g. in a test) can use the four fixed
	// percentiles above but not an arbitrary one.
	if pct, ok := latencyPercentileMetric(metric); ok {
		return percentile(s.sortedLatencies, pct/100).Seconds(), nil
	}
	return 0, fmt.Errorf("unknown assertion metric %q (see README's \"Assertions\" section for the supported list)", metric)
}

// Evaluate checks every assertion against s and returns one Failure per
// assertion that didn't hold, in the order given. A nil/empty result means
// everything passed (or there was nothing to check).
func (s Summary) Evaluate(assertions []Assertion) ([]Failure, error) {
	var failures []Failure
	for _, a := range assertions {
		actual, err := s.metricValue(a.Metric)
		if err != nil {
			return nil, err
		}
		if a.violated(actual) {
			failures = append(failures, Failure{Metric: a.Metric, Actual: actual, Reasons: a.describe()})
		}
	}
	return failures, nil
}

// AssertionMetrics lists every fixed metric name Evaluate accepts, for use
// in error messages and validation — response-time assertions also accept
// any "latency_p<N>" percentile (0-100, e.g. "latency_p99.9"), which isn't
// enumerable, so use IsKnownAssertionMetric (not a plain membership check
// against this slice) to validate a metric name from user input.
var AssertionMetrics = []string{
	"requests", "success", "success_rate", "error_rate", "rate",
	"latency_min", "latency_mean", "latency_stddev", "latency_p50", "latency_p90", "latency_p95", "latency_p99", "latency_max",
}

// IsKnownAssertionMetric reports whether metric is a name Evaluate accepts
// — either one of the fixed AssertionMetrics names, or a "latency_p<N>"
// percentile metric for any N in [0, 100] (see latencyPercentileMetric).
func IsKnownAssertionMetric(metric string) bool {
	for _, m := range AssertionMetrics {
		if m == metric {
			return true
		}
	}
	_, ok := latencyPercentileMetric(metric)
	return ok
}

// IsLatencyMetric reports whether metric expects a duration-shaped
// threshold ("500ms") rather than a plain number.
func IsLatencyMetric(metric string) bool {
	switch metric {
	case "latency_min", "latency_mean", "latency_stddev", "latency_max":
		return true
	}
	_, ok := latencyPercentileMetric(metric)
	return ok
}

// ParseThreshold parses a threshold string appropriately for metric: a
// duration ("500ms") for latency metrics, seconds; a plain float otherwise.
func ParseThreshold(metric, value string) (float64, error) {
	if IsLatencyMetric(metric) {
		d, err := time.ParseDuration(value)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", value, err)
		}
		return d.Seconds(), nil
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", value)
	}
	return f, nil
}

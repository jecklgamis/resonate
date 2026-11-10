// Package report aggregates generator.Result values into a run summary.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/jecklgamis/resonate/internal/generator"
)

// point is one result's contribution to the time series/histogram built in
// Summary — kept separate from the exported types so callers who only want
// the aggregate numbers (the common case) don't pay for it.
type point struct {
	t       time.Time
	latency time.Duration
	success bool
}

// Latency histogram bounds, in nanoseconds: 1ns to 1h at 3 significant
// figures (≤0.1% error), ~0.5MB regardless of request count. Latencies above
// the ceiling are clamped to it rather than dropped.
const (
	histMaxLatency = int64(time.Hour)
	histSigFigs    = 3
)

func newLatencyHist() *hdrhistogram.Histogram {
	return hdrhistogram.New(1, histMaxLatency, histSigFigs)
}

// Aggregator collects results from a run. It is not safe for concurrent use;
// results should be funneled through a single goroutine (see engine.Run).
type Aggregator struct {
	latencies *hdrhistogram.Histogram
	minLat    time.Duration // exact; the histogram only keeps 3 significant figures
	maxLat    time.Duration
	points    []point
	codes     map[int]int
	errors    map[string]int
	success   int
	bytesIn   int64
	bytesOut  int64
	first     time.Time
	last      time.Time

	// steps is the per-step breakdown, keyed by Result.Step; stepOrder is the
	// order the report lists them in: the configured order when SetStepOrder
	// was given one, with any other step after it in first-seen order.
	steps     map[string]*stepAgg
	stepOrder []string
}

func NewAggregator() *Aggregator {
	return &Aggregator{
		latencies: newLatencyHist(),
		codes:     make(map[int]int),
		errors:    make(map[string]int),
		steps:     make(map[string]*stepAgg),
	}
}

// SetStepOrder fixes the order steps are listed in to the scenario's own,
// instead of the order their first results happened to arrive in — which for
// round-robin targets run concurrently is arbitrary. Steps that never make a
// request are still left out. Call it before Add.
func (a *Aggregator) SetStepOrder(names []string) {
	a.stepOrder = slices.Clone(names)
}

func (a *Aggregator) Add(r generator.Result) {
	if a.first.IsZero() || r.Timestamp.Before(a.first) {
		a.first = r.Timestamp
	}
	end := r.Timestamp.Add(r.Latency)
	if end.After(a.last) {
		a.last = end
	}

	a.bytesIn += r.BytesIn
	a.bytesOut += r.BytesOut
	a.points = append(a.points, point{t: r.Timestamp, latency: r.Latency, success: r.Success()})

	if r.Step != "" {
		st, ok := a.steps[r.Step]
		if !ok {
			st = newStepAgg()
			a.steps[r.Step] = st
			if !slices.Contains(a.stepOrder, r.Step) {
				a.stepOrder = append(a.stepOrder, r.Step)
			}
		}
		st.add(r)
	}

	if r.Error != nil {
		a.errors[r.Error.Error()]++
		return
	}

	lat := r.Latency
	if lat < 0 {
		lat = 0
	}
	if a.latencies.TotalCount() == 0 || lat < a.minLat {
		a.minLat = lat
	}
	if lat > a.maxLat {
		a.maxLat = lat
	}
	_ = a.latencies.RecordValue(min(int64(lat), histMaxLatency)) // in range by construction
	a.codes[r.StatusCode]++
	if r.Success() {
		a.success++
	}
}

// SchemaVersion is the version of the report.json / --json layout, written to
// every Summary as "schema_version". It only changes for a breaking change to
// a documented field (a rename, removal or change of unit); adding a field
// does not bump it. See docs/reports.md#stability.
const SchemaVersion = 1

// Summary is a point-in-time snapshot of aggregated results.
type Summary struct {
	// SchemaVersion identifies the JSON layout; see the SchemaVersion constant.
	SchemaVersion int `json:"schema_version"`

	Requests    int            `json:"requests"`
	Success     int            `json:"success"`
	SuccessRate float64        `json:"success_rate"`
	Duration    time.Duration  `json:"duration"`
	Rate        float64        `json:"requests_per_sec"`
	BytesIn     int64          `json:"bytes_in"`
	BytesOut    int64          `json:"bytes_out"`
	Latencies   LatencyStats   `json:"latencies"`
	StatusCodes map[int]int    `json:"status_codes,omitempty"`
	Errors      map[string]int `json:"errors,omitempty"`

	// Steps breaks the run down per named target/flow step, in the scenario's
	// order (see Aggregator.SetStepOrder). Empty for protocols that don't name
	// their requests.
	Steps []StepSummary `json:"steps,omitempty"`

	// Aborted, if set, is why the run was stopped early: an assertion marked
	// abort that failed while the run was still going (see Assertion.Abort).
	// The numbers cover only what ran before the stop.
	Aborted string `json:"aborted,omitempty"`

	// TimeSeries and LatencyHistogram are extra breakdowns kept mainly for
	// WriteHTML's charts; they're populated whenever there's at least one
	// result, so a --json consumer that doesn't want them can just ignore
	// the fields.
	TimeSeries       []TimeBucket   `json:"time_series,omitempty"`
	LatencyHistogram []HistogramBin `json:"latency_histogram,omitempty"`

	// Saturated and PeakConcurrency are only set by an open-model run
	// (engine.Options.MaxWorkers > 0). Saturated means at least one
	// iteration had to wait for an in-flight slot to free up because all
	// MaxWorkers were busy when its rate token was minted — i.e. Rate
	// couldn't always be sustained at that concurrency ceiling.
	// PeakConcurrency is the highest number of iterations in flight at once.
	Saturated       bool `json:"rate_saturated,omitempty"`
	PeakConcurrency int  `json:"peak_concurrency,omitempty"`

	// hist backs Evaluate's arbitrary-percentile assertions
	// ("latency_p97.5", not just the fixed P50/P75/P90/P95/P99 above) — kept
	// unexported (so it never appears in --json output).
	hist *hdrhistogram.Histogram
}

type LatencyStats struct {
	Min    time.Duration `json:"min"`
	Mean   time.Duration `json:"mean"`
	StdDev time.Duration `json:"stddev"`
	P50    time.Duration `json:"p50"`
	P75    time.Duration `json:"p75"`
	P90    time.Duration `json:"p90"`
	P95    time.Duration `json:"p95"`
	P99    time.Duration `json:"p99"`
	Max    time.Duration `json:"max"`
}

// TimeBucket aggregates results seen within one fixed-width time window,
// used to draw a requests-over-time chart (see WriteHTML).
type TimeBucket struct {
	Start    time.Time     `json:"start"`
	Requests int           `json:"requests"`
	Success  int           `json:"success"`
	Mean     time.Duration `json:"mean_latency"`
	// P50/P95/P99 are this bucket's own latency percentiles (nearest rank), so
	// the report can show latency degrading over the run. Zero for a bucket
	// with no requests.
	P50 time.Duration `json:"p50_latency"`
	P95 time.Duration `json:"p95_latency"`
	P99 time.Duration `json:"p99_latency"`
}

// HistogramBin counts results whose latency fell in (prev bin's Le, Le].
type HistogramBin struct {
	Le    time.Duration `json:"le"`
	Count int           `json:"count"`
}

// Summary computes the final report. wallClock is the actual measured
// duration of the run (preferred over first/last timestamp deltas when the
// caller has it, since it isn't skewed by in-flight requests at cutoff).
func (a *Aggregator) Summary(wallClock time.Duration) Summary {
	summary := a.Snapshot(wallClock)
	dur := summary.Duration
	summary.TimeSeries = bucketSeries(a.points, a.first, dur)
	summary.LatencyHistogram = latencyHistogram(a.latencies, a.minLat, a.maxLat)
	return summary
}

// Snapshot is Summary without the time series and histogram bins — the
// headline numbers, per-step breakdown and percentiles, which is all an
// assertion needs. It is cheap enough to call every second during a run
// (Summary's time series sorts every result's latency), which is how
// abort-on-threshold checks a run while it is still going. Like every
// Aggregator method it must not race with Add.
func (a *Aggregator) Snapshot(wallClock time.Duration) Summary {
	errCount := 0
	for _, c := range a.errors {
		errCount += c
	}
	requests := int(a.latencies.TotalCount()) + errCount

	dur := wallClock
	if dur <= 0 && !a.first.IsZero() {
		dur = a.last.Sub(a.first)
	}

	summary := Summary{
		SchemaVersion: SchemaVersion,
		Requests:      requests,
		Success:       a.success,
		Duration:      dur,
		BytesIn:       a.bytesIn,
		BytesOut:      a.bytesOut,
		StatusCodes:   a.codes,
		Errors:        a.errors,
	}
	if requests > 0 {
		summary.SuccessRate = float64(a.success) / float64(requests)
	}
	if dur > 0 {
		summary.Rate = float64(requests) / dur.Seconds()
	}
	if a.latencies.TotalCount() > 0 {
		summary.hist = a.latencies
		summary.Latencies = latencyStats(a.latencies, a.minLat, a.maxLat)
	}
	for _, name := range a.stepOrder {
		if st, ok := a.steps[name]; ok { // a configured step may not have made a request yet
			summary.Steps = append(summary.Steps, st.summary(name, dur))
		}
	}
	return summary
}

// bucketSeries groups points into fixed-width time windows starting at
// start, capped at ~maxBuckets windows total so an hours-long run still
// produces a chart-sized series instead of one bar per second.
func bucketSeries(points []point, start time.Time, dur time.Duration) []TimeBucket {
	if len(points) == 0 || dur <= 0 {
		return nil
	}
	const maxBuckets = 120
	width := time.Second
	if n := dur / width; n > maxBuckets {
		width = dur / maxBuckets
	}
	count := int(dur/width) + 1

	buckets := make([]TimeBucket, count)
	sums := make([]time.Duration, count)
	lats := make([][]time.Duration, count)
	for i := range buckets {
		buckets[i].Start = start.Add(time.Duration(i) * width)
	}
	for _, p := range points {
		idx := int(p.t.Sub(start) / width)
		if idx < 0 {
			idx = 0
		}
		if idx >= count {
			idx = count - 1
		}
		buckets[idx].Requests++
		sums[idx] += p.latency
		lats[idx] = append(lats[idx], p.latency)
		if p.success {
			buckets[idx].Success++
		}
	}
	for i := range buckets {
		if buckets[i].Requests > 0 {
			buckets[i].Mean = sums[i] / time.Duration(buckets[i].Requests)
			slices.Sort(lats[i])
			buckets[i].P50 = nearestRank(lats[i], 0.50)
			buckets[i].P95 = nearestRank(lats[i], 0.95)
			buckets[i].P99 = nearestRank(lats[i], 0.99)
		}
	}
	return buckets
}

// nearestRank returns the p-th percentile (p in (0, 1]) of an ascending,
// non-empty slice by the nearest-rank method.
func nearestRank(sorted []time.Duration, p float64) time.Duration {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// latencyHistogram buckets latencies into a fixed number of log-spaced bins
// spanning [min, max], for the response-time distribution chart. Log spacing
// (each bin's upper edge a constant factor above the last) keeps a long tail
// from squashing the bulk of the data into the first bar, which equal-width
// bins did. Each HDR bar is assigned to the first bin whose upper edge is at
// or above its upper bound.
func latencyHistogram(h *hdrhistogram.Histogram, min, max time.Duration) []HistogramBin {
	total := int(h.TotalCount())
	if total == 0 {
		return nil
	}

	const numBins = 20
	if min == max {
		return []HistogramBin{{Le: max, Count: total}}
	}
	// Geometric edges need a positive start; a sub-microsecond minimum is
	// below what the report can show anyway.
	lo := min
	if lo < time.Microsecond {
		lo = time.Microsecond
	}
	if lo >= max {
		return []HistogramBin{{Le: max, Count: total}}
	}
	ratio := float64(max) / float64(lo)

	bins := make([]HistogramBin, numBins)
	for i := range bins {
		bins[i].Le = time.Duration(float64(lo) * math.Pow(ratio, float64(i+1)/numBins))
	}
	bins[numBins-1].Le = max // last bin always closes exactly on max, regardless of rounding

	for _, bar := range h.Distribution() {
		if bar.Count == 0 {
			continue
		}
		v := time.Duration(bar.To)
		if v > max {
			v = max
		}
		idx := sort.Search(numBins, func(i int) bool { return bins[i].Le >= v })
		if idx >= numBins {
			idx = numBins - 1
		}
		bins[idx].Count += int(bar.Count)
	}
	return bins
}

// latencyStats computes stats from a non-empty histogram (the empty case is
// handled by the caller). min/max are tracked exactly by the Aggregator;
// the rest carry the histogram's ≤0.1% quantization error.
func latencyStats(h *hdrhistogram.Histogram, min, max time.Duration) LatencyStats {
	return LatencyStats{
		Min:    min,
		Mean:   time.Duration(h.Mean()),
		StdDev: time.Duration(h.StdDev()),
		P50:    percentile(h, 0.50),
		P75:    percentile(h, 0.75),
		P90:    percentile(h, 0.90),
		P95:    percentile(h, 0.95),
		P99:    percentile(h, 0.99),
		Max:    max,
	}
}

// percentile returns the p-th percentile (p in [0, 1]) of h — shared by
// latencyStats' fixed P50/P75/P90/P95/P99 and Evaluate's arbitrary-percentile
// assertions, so both use identical math. A nil histogram yields 0.
func percentile(h *hdrhistogram.Histogram, p float64) time.Duration {
	if h == nil || h.TotalCount() == 0 {
		return 0
	}
	return time.Duration(h.ValueAtQuantile(p * 100))
}

// Print writes a human-readable report.
func (s Summary) Print(w io.Writer) {
	fmt.Fprintf(w, "Requests      [total, rate]            %d, %.2f/sec\n", s.Requests, s.Rate)
	fmt.Fprintf(w, "Duration      [total]                  %s\n", s.Duration.Round(time.Millisecond))
	fmt.Fprintf(w, "Success       [ratio]                  %.2f%%\n", s.SuccessRate*100)
	fmt.Fprintf(w, "Latencies     [min, mean, p50, p75, p90, p95, p99, max]  %s, %s, %s, %s, %s, %s, %s, %s\n",
		s.Latencies.Min.Round(time.Microsecond),
		s.Latencies.Mean.Round(time.Microsecond),
		s.Latencies.P50.Round(time.Microsecond),
		s.Latencies.P75.Round(time.Microsecond),
		s.Latencies.P90.Round(time.Microsecond),
		s.Latencies.P95.Round(time.Microsecond),
		s.Latencies.P99.Round(time.Microsecond),
		s.Latencies.Max.Round(time.Microsecond),
	)
	fmt.Fprintf(w, "Bytes         [in, out]                %d, %d\n", s.BytesIn, s.BytesOut)
	s.printSteps(w)
	if len(s.StatusCodes) > 0 {
		fmt.Fprintf(w, "Status Codes  [code:count]             ")
		codes := make([]int, 0, len(s.StatusCodes))
		for c := range s.StatusCodes {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for i, c := range codes {
			if i > 0 {
				fmt.Fprint(w, ", ")
			}
			fmt.Fprintf(w, "%d:%d", c, s.StatusCodes[c])
		}
		fmt.Fprintln(w)
	}
	if len(s.Errors) > 0 {
		fmt.Fprintln(w, "Errors:")
		for msg, count := range s.Errors {
			fmt.Fprintf(w, "  [%d]  %s\n", count, msg)
		}
	}
	if s.Aborted != "" {
		fmt.Fprintf(w, "ABORTED: %s\n", s.Aborted)
	}
	if s.Saturated {
		fmt.Fprintf(w, "warning: target rate could not always be sustained — peak concurrency hit %d in-flight iterations; raise --max-workers to open more headroom\n", s.PeakConcurrency)
	}
}

// PrintJSON writes the summary as JSON.
func (s Summary) PrintJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

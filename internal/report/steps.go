package report

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/jecklgamis/resonate/internal/generator"
)

// stepAgg accumulates one step's results — the same bookkeeping Aggregator
// does for the whole run, so a step's numbers mean exactly what the run's
// do (a transport error counts as a request and a failure but records no
// latency).
type stepAgg struct {
	latencies *hdrhistogram.Histogram
	minLat    time.Duration
	maxLat    time.Duration
	codes     map[int]int
	errors    map[string]int
	success   int
}

func newStepAgg() *stepAgg {
	return &stepAgg{
		latencies: newLatencyHist(),
		codes:     make(map[int]int),
		errors:    make(map[string]int),
	}
}

func (s *stepAgg) add(r generator.Result) {
	if r.Error != nil {
		s.errors[r.Error.Error()]++
		return
	}
	lat := max(r.Latency, 0)
	if s.latencies.TotalCount() == 0 || lat < s.minLat {
		s.minLat = lat
	}
	if lat > s.maxLat {
		s.maxLat = lat
	}
	_ = s.latencies.RecordValue(min(int64(lat), histMaxLatency)) // in range by construction
	s.codes[r.StatusCode]++
	if r.Success() {
		s.success++
	}
}

func (s *stepAgg) summary(name string, wallClock time.Duration) StepSummary {
	errCount := 0
	for _, c := range s.errors {
		errCount += c
	}
	requests := int(s.latencies.TotalCount()) + errCount
	out := StepSummary{
		Name:        name,
		Requests:    requests,
		Success:     s.success,
		StatusCodes: s.codes,
		Errors:      s.errors,
	}
	if requests > 0 {
		out.SuccessRate = float64(s.success) / float64(requests)
	}
	if wallClock > 0 {
		out.Rate = float64(requests) / wallClock.Seconds()
	}
	if s.latencies.TotalCount() > 0 {
		out.hist = s.latencies
		out.Latencies = latencyStats(s.latencies, s.minLat, s.maxLat)
	}
	return out
}

// StepSummary is one named step's share of a run: the same headline numbers
// as Summary, restricted to the requests that step made. A step is a
// configured target or flow step (its Name, else "METHOD url").
type StepSummary struct {
	Name        string         `json:"name"`
	Requests    int            `json:"requests"`
	Success     int            `json:"success"`
	SuccessRate float64        `json:"success_rate"`
	Rate        float64        `json:"requests_per_sec"`
	Latencies   LatencyStats   `json:"latencies"`
	StatusCodes map[int]int    `json:"status_codes,omitempty"`
	Errors      map[string]int `json:"errors,omitempty"`

	// hist backs arbitrary-percentile step assertions, like Summary.hist.
	hist *hdrhistogram.Histogram
}

// step returns the named step's summary.
func (s Summary) step(name string) (StepSummary, bool) {
	for _, st := range s.Steps {
		if st.Name == name {
			return st, true
		}
	}
	return StepSummary{}, false
}

// stepNames lists the steps in the summary, for error messages.
func (s Summary) stepNames() []string {
	names := make([]string, len(s.Steps))
	for i, st := range s.Steps {
		names[i] = st.Name
	}
	return names
}

// printSteps writes the per-step table. It stays out of the way when there is
// only one step, since that would just repeat the run totals.
func (s Summary) printSteps(w io.Writer) {
	if len(s.Steps) < 2 {
		return
	}
	width := len("Step")
	for _, st := range s.Steps {
		width = max(width, len(st.Name))
	}
	fmt.Fprintln(w, "Steps:")
	fmt.Fprintf(w, "  %-*s  %9s  %8s  %10s  %10s  %10s\n", width, "Step", "Requests", "Success", "p50", "p95", "p99")
	for _, st := range s.Steps {
		fmt.Fprintf(w, "  %-*s  %9d  %7.2f%%  %10s  %10s  %10s\n", width, st.Name, st.Requests, st.SuccessRate*100,
			st.Latencies.P50.Round(time.Microsecond),
			st.Latencies.P95.Round(time.Microsecond),
			st.Latencies.P99.Round(time.Microsecond))
	}
}

// sortedStepNames returns names in a stable order (used by compare).
func sortedStepNames(steps []StepSummary) []string {
	names := make([]string, len(steps))
	for i, st := range steps {
		names[i] = st.Name
	}
	sort.Strings(names)
	return names
}

// ValidateSteps rejects an assertion scoped to a step that doesn't exist, so a
// typo fails before the run instead of as "recorded no requests" after it.
// known is the scenario's step names; supported is false for protocols that
// don't name their requests, where no step assertion can ever match.
func ValidateSteps(assertions []Assertion, known []string, supported bool) error {
	for i, a := range assertions {
		if a.Step == "" {
			continue
		}
		if !supported {
			return fmt.Errorf("assertions[%d] (%s): step %q: per-step assertions only apply to http scenarios", i, a.Metric, a.Step)
		}
		if !slices.Contains(known, a.Step) {
			return fmt.Errorf("assertions[%d] (%s): unknown step %q (steps: %s)", i, a.Metric, a.Step, stepList(known))
		}
	}
	return nil
}

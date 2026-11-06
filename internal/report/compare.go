package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// ReadSummary decodes a Summary written by PrintJSON (the report.json a run
// leaves behind, or --json output). The decoded Summary has every fixed
// metric but not the histogram, so arbitrary-percentile assertions
// (latency_p99.9) can't be evaluated against it — Compare only uses the fixed
// ones.
func ReadSummary(r io.Reader) (Summary, error) {
	var s Summary
	dec := json.NewDecoder(r)
	if err := dec.Decode(&s); err != nil {
		return Summary{}, fmt.Errorf("not a resonate JSON report: %w", err)
	}
	if s.Requests == 0 && s.Duration == 0 && len(s.Steps) == 0 {
		return Summary{}, fmt.Errorf("not a resonate JSON report: no requests or duration recorded")
	}
	return s, nil
}

// compareMetrics are the metrics a comparison table shows, in order.
var compareMetrics = []string{
	"requests", "rate", "success_rate", "error_rate",
	"latency_mean", "latency_p50", "latency_p75", "latency_p90", "latency_p95", "latency_p99", "latency_max",
}

// Delta is one metric measured in two runs.
type Delta struct {
	// Name is the metric, or "step:metric" for a step's.
	Name     string
	Baseline float64
	Current  float64
}

// Abs is the change in the metric's own unit (seconds for latency).
func (d Delta) Abs() float64 { return d.Current - d.Baseline }

// Rel is the change as a percentage of the baseline. ok is false when the
// baseline is zero and the value moved, where a percentage means nothing.
func (d Delta) Rel() (pct float64, ok bool) {
	if d.Baseline == 0 {
		return 0, d.Current == 0
	}
	return (d.Current - d.Baseline) / math.Abs(d.Baseline) * 100, true
}

// metricName is Name without any "step:" prefix.
func (d Delta) metricName() string {
	if i := strings.LastIndexByte(d.Name, ':'); i >= 0 {
		return d.Name[i+1:]
	}
	return d.Name
}

// Format renders v the way the metric is usually read.
func (d Delta) Format(v float64) string { return formatCompareValue(d.metricName(), v) }

func formatCompareValue(metric string, v float64) string {
	switch {
	case IsLatencyMetric(metric):
		return time.Duration(v * float64(time.Second)).Round(time.Microsecond).String()
	case metric == "success_rate" || metric == "error_rate":
		return fmt.Sprintf("%.2f%%", v*100)
	case metric == "requests":
		return strconv.FormatInt(int64(v), 10)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// Compare measures the run-level metrics and every step present in both runs.
// A step present in only one run is left out of the table (a gate naming it
// fails loudly instead; see Gate.Check).
func Compare(base, cur Summary) []Delta {
	var out []Delta
	add := func(prefix string, b, c metricSet) {
		for _, m := range compareMetrics {
			bv, _ := b.value(m)
			cv, _ := c.value(m)
			out = append(out, Delta{Name: prefix + m, Baseline: bv, Current: cv})
		}
	}
	add("", base.metrics(), cur.metrics())
	for _, name := range sortedStepNames(base.Steps) {
		b, _ := base.step(name)
		c, ok := cur.step(name)
		if ok {
			add(name+":", b.metrics(), c.metrics())
		}
	}
	return out
}

// lookup finds the metric name ("p95" style names are not accepted; use the
// assertion metric names) in either run.
func lookup(base, cur Summary, name string) (Delta, error) {
	step, metric := "", name
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		step, metric = name[:i], name[i+1:]
	}
	value := func(s Summary) (float64, error) {
		if step == "" {
			return s.metrics().value(metric)
		}
		st, ok := s.step(step)
		if !ok {
			return 0, fmt.Errorf("step %q is not in both runs", step)
		}
		return st.metrics().value(metric)
	}
	b, err := value(base)
	if err != nil {
		return Delta{}, fmt.Errorf("baseline: %w", err)
	}
	c, err := value(cur)
	if err != nil {
		return Delta{}, fmt.Errorf("current: %w", err)
	}
	return Delta{Name: name, Baseline: b, Current: c}, nil
}

// Gate is a regression rule over two runs: fail when a metric's change meets
// a condition. Written "<metric><op><threshold>", e.g.
//
//	latency_p95>10%       p95 grew by more than 10% of the baseline
//	rate<-5%              throughput dropped by more than 5%
//	error_rate>0.01       error rate rose by more than one point (absolute)
//	login:latency_p99>20ms  a step's p99 grew by more than 20ms
//
// A threshold ending in % is relative to the baseline; otherwise it is an
// absolute change in the metric's unit (a duration for latency metrics, a
// plain number otherwise — a ratio 0-1 for the *_rate metrics). The change is
// current minus baseline, so for latency and error rates a positive
// threshold with > guards against getting worse, and for rate and
// success_rate a negative one with < does.
type Gate struct {
	Expr      string
	Name      string // metric, or "step:metric"
	Op        string // ">", ">=", "<", "<="
	Threshold float64
	Relative  bool
}

// gateOps is checked longest-first so ">=" is never read as ">" then "=…".
var gateOps = []string{">=", "<=", ">", "<"}

// ParseGate parses one --fail-on expression.
func ParseGate(expr string) (Gate, error) {
	expr = strings.TrimSpace(expr)
	for _, op := range gateOps {
		name, val, found := strings.Cut(expr, op)
		if !found {
			continue
		}
		name, val = strings.TrimSpace(name), strings.TrimSpace(val)
		if name == "" || val == "" {
			break
		}
		g := Gate{Expr: expr, Name: name, Op: op}
		metric := name
		if i := strings.LastIndexByte(name, ':'); i >= 0 {
			metric = name[i+1:]
		}
		if !isFixedMetric(metric) {
			return Gate{}, fmt.Errorf("%q: can't compare %q (use one of %s)", expr, metric, strings.Join(AssertionMetrics, ", "))
		}
		if rest, ok := strings.CutSuffix(val, "%"); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return Gate{}, fmt.Errorf("%q: invalid percentage %q", expr, val)
			}
			g.Threshold, g.Relative = f, true
			return g, nil
		}
		f, err := ParseThreshold(metric, val)
		if err != nil {
			return Gate{}, fmt.Errorf("%q: %w", expr, err)
		}
		g.Threshold = f
		return g, nil
	}
	return Gate{}, fmt.Errorf("%q: expected <metric><op><threshold> with op one of >, >=, <, <= (e.g. latency_p95>10%%)", expr)
}

func isFixedMetric(m string) bool {
	for _, f := range AssertionMetrics {
		if f == m {
			return true
		}
	}
	return false
}

// GateResult is one gate's verdict on a pair of runs.
type GateResult struct {
	Gate   Gate
	Delta  Delta
	Failed bool
	// Note says why a relative gate couldn't be judged (zero baseline).
	Note string
}

// Check applies the gate to a baseline/current pair. A metric or step missing
// from either run is an error rather than a pass: a gate that silently stops
// checking anything (a renamed step) is worse than one that fails.
func (g Gate) Check(base, cur Summary) (GateResult, error) {
	d, err := lookup(base, cur, g.Name)
	if err != nil {
		return GateResult{}, fmt.Errorf("%s: %w", g.Expr, err)
	}
	res := GateResult{Gate: g, Delta: d}
	change := d.Abs()
	if g.Relative {
		pct, ok := d.Rel()
		if !ok {
			// A percentage of zero is undefined, and quietly passing would let a
			// metric go from nothing to something unnoticed.
			res.Failed = true
			res.Note = "baseline is zero, so a relative change is undefined; use an absolute threshold"
			return res, nil
		}
		change = pct
	}
	// Latencies are float seconds, so a change that is exactly at the
	// threshold (110ms vs 100ms is +10%) comes out as 9.999999999999998 and
	// lands on the wrong side of >= or <=. Rounding to a nanosecond (the
	// finest latency there is) / a billionth of a percent keeps the boundary
	// meaning what it says.
	change = math.Round(change*1e9) / 1e9
	switch g.Op {
	case ">":
		res.Failed = change > g.Threshold
	case ">=":
		res.Failed = change >= g.Threshold
	case "<":
		res.Failed = change < g.Threshold
	case "<=":
		res.Failed = change <= g.Threshold
	}
	return res, nil
}

// WriteComparison prints the comparison table.
func WriteComparison(w io.Writer, deltas []Delta) {
	width := len("Metric")
	for _, d := range deltas {
		width = max(width, len(d.Name))
	}
	fmt.Fprintf(w, "%-*s  %14s  %14s  %10s\n", width, "Metric", "Baseline", "Current", "Change")
	for _, d := range deltas {
		change := "n/a"
		if pct, ok := d.Rel(); ok {
			change = fmt.Sprintf("%+.1f%%", pct)
		}
		fmt.Fprintf(w, "%-*s  %14s  %14s  %10s\n", width, d.Name, d.Format(d.Baseline), d.Format(d.Current), change)
	}
}

// String describes the verdict on one line.
func (r GateResult) String() string {
	verdict := "ok  "
	if r.Failed {
		verdict = "FAIL"
	}
	change := fmt.Sprintf("%s -> %s", r.Delta.Format(r.Delta.Baseline), r.Delta.Format(r.Delta.Current))
	if pct, ok := r.Delta.Rel(); ok {
		change += fmt.Sprintf(" (%+.1f%%)", pct)
	}
	s := fmt.Sprintf("%s  %s: %s", verdict, r.Gate.Expr, change)
	if r.Note != "" {
		s += " — " + r.Note
	}
	return s
}

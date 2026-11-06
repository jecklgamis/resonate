package report

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func summaryWith(p95 time.Duration, successRate, rate float64, steps ...StepSummary) Summary {
	return Summary{
		Requests:    1000,
		SuccessRate: successRate,
		Rate:        rate,
		Duration:    10 * time.Second,
		Latencies:   LatencyStats{P50: p95 / 2, P95: p95, P99: p95 * 2, Max: p95 * 3},
		Steps:       steps,
	}
}

func TestReadSummaryRoundTripsPrintJSON(t *testing.T) {
	orig := twoStepSummary()
	var buf bytes.Buffer
	if err := orig.PrintJSON(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSummary(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != orig.Requests || got.Latencies.P95 != orig.Latencies.P95 || len(got.Steps) != 2 {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.Steps[1].Name != "checkout" || got.Steps[1].Latencies.P95 != orig.Steps[1].Latencies.P95 || got.Steps[1].SuccessRate != 0.75 {
		t.Errorf("step lost in round trip: %+v", got.Steps[1])
	}
}

func TestReadSummaryRejectsOtherJSON(t *testing.T) {
	for _, in := range []string{`{"hello":"world"}`, `[]`, `not json`, ``} {
		if _, err := ReadSummary(strings.NewReader(in)); err == nil {
			t.Errorf("ReadSummary(%q) accepted something that isn't a report", in)
		}
	}
}

func TestParseGate(t *testing.T) {
	cases := []struct {
		expr string
		want Gate
	}{
		{"latency_p95>10%", Gate{Name: "latency_p95", Op: ">", Threshold: 10, Relative: true}},
		{"rate<-5%", Gate{Name: "rate", Op: "<", Threshold: -5, Relative: true}},
		{"error_rate>=0.01", Gate{Name: "error_rate", Op: ">=", Threshold: 0.01}},
		{"latency_p99 <= +20ms", Gate{Name: "latency_p99", Op: "<=", Threshold: 0.020}},
		{"login:latency_p95>50ms", Gate{Name: "login:latency_p95", Op: ">", Threshold: 0.050}},
	}
	for _, c := range cases {
		got, err := ParseGate(c.expr)
		if err != nil {
			t.Errorf("ParseGate(%q): %v", c.expr, err)
			continue
		}
		c.want.Expr = strings.TrimSpace(c.expr)
		if got != c.want {
			t.Errorf("ParseGate(%q) = %+v, want %+v", c.expr, got, c.want)
		}
	}
}

func TestParseGateRejectsMistakes(t *testing.T) {
	for _, expr := range []string{
		"", "latency_p95", "latency_p95>", ">10%", "latency_p95>fast", "latency_p95>10", // a latency threshold needs a unit
		"latency_p99.9>10%", // not in the JSON report: no histogram
		"bogus>10%", "login:bogus>10%", "rate>x%",
	} {
		if _, err := ParseGate(expr); err == nil {
			t.Errorf("ParseGate(%q) accepted a bad rule", expr)
		}
	}
}

func TestGateCheck(t *testing.T) {
	base := summaryWith(100*time.Millisecond, 1.0, 100)
	cases := []struct {
		name string
		cur  Summary
		expr string
		fail bool
	}{
		{"p95 +5% under a 10% budget", summaryWith(105*time.Millisecond, 1, 100), "latency_p95>10%", false},
		{"p95 +20% over a 10% budget", summaryWith(120*time.Millisecond, 1, 100), "latency_p95>10%", true},
		{"p95 exactly at the limit is not over it", summaryWith(110*time.Millisecond, 1, 100), "latency_p95>10%", false},
		{"p95 faster never trips a > rule", summaryWith(50*time.Millisecond, 1, 100), "latency_p95>10%", false},
		{"throughput -10% past a -5% floor", summaryWith(100*time.Millisecond, 1, 90), "rate<-5%", true},
		{"throughput up is fine", summaryWith(100*time.Millisecond, 1, 130), "rate<-5%", false},
		{"absolute: error rate +2 points", summaryWith(100*time.Millisecond, 0.98, 100), "error_rate>0.01", true},
		{"absolute: error rate +0.5 points", summaryWith(100*time.Millisecond, 0.995, 100), "error_rate>0.01", false},
		{"absolute latency: +30ms over 20ms", summaryWith(130*time.Millisecond, 1, 100), "latency_p95>20ms", true},
		{">= includes the boundary", summaryWith(110*time.Millisecond, 1, 100), "latency_p95>=10%", true},
	}
	for _, c := range cases {
		g, err := ParseGate(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		res, err := g.Check(base, c.cur)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if res.Failed != c.fail {
			t.Errorf("%s: %q failed=%v, want %v (%s)", c.name, c.expr, res.Failed, c.fail, res)
		}
	}
}

func TestGateRelativeOnZeroBaselineFailsLoudly(t *testing.T) {
	base := summaryWith(100*time.Millisecond, 1.0, 100) // error_rate 0
	cur := summaryWith(100*time.Millisecond, 0.9, 100)  // error_rate 0.1
	g, _ := ParseGate("error_rate>50%")
	res, err := g.Check(base, cur)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed || !strings.Contains(res.Note, "absolute") {
		t.Errorf("res = %+v; going from no errors to some must not slip past a relative rule", res)
	}
	same, _ := g.Check(base, base)
	if same.Failed {
		t.Error("0 -> 0 is no change and must pass")
	}
}

func TestGateOnStep(t *testing.T) {
	step := func(p95 time.Duration) StepSummary {
		return StepSummary{Name: "login", Requests: 10, SuccessRate: 1, Latencies: LatencyStats{P95: p95}}
	}
	base := summaryWith(100*time.Millisecond, 1, 100, step(50*time.Millisecond))
	// The run-level p95 is unchanged; only the step regressed.
	cur := summaryWith(100*time.Millisecond, 1, 100, step(80*time.Millisecond))

	g, _ := ParseGate("login:latency_p95>20%")
	res, err := g.Check(base, cur)
	if err != nil || !res.Failed {
		t.Errorf("res=%+v err=%v; the step regression must trip its own rule", res, err)
	}
	run, _ := ParseGate("latency_p95>20%")
	if res, _ := run.Check(base, cur); res.Failed {
		t.Error("the run-level rule must not see the step's regression")
	}
}

func TestGateOnMissingStepOrRunIsAnError(t *testing.T) {
	with := summaryWith(time.Second, 1, 1, StepSummary{Name: "login", Requests: 1})
	without := summaryWith(time.Second, 1, 1)
	g, _ := ParseGate("login:latency_p95>10%")
	for _, pair := range [][2]Summary{{with, without}, {without, with}} {
		if _, err := g.Check(pair[0], pair[1]); err == nil || !strings.Contains(err.Error(), "not in both runs") {
			t.Errorf("err = %v; a rule whose step vanished must error, not silently pass", err)
		}
	}
}

func TestCompareListsRunAndSharedSteps(t *testing.T) {
	st := func(name string) StepSummary { return StepSummary{Name: name, Requests: 5, SuccessRate: 1} }
	base := summaryWith(time.Second, 1, 10, st("login"), st("old"))
	cur := summaryWith(2*time.Second, 1, 10, st("login"), st("new"))
	deltas := Compare(base, cur)

	names := map[string]Delta{}
	for _, d := range deltas {
		names[d.Name] = d
	}
	if d := names["latency_p95"]; d.Baseline != 1 || d.Current != 2 {
		t.Errorf("latency_p95 = %+v", d)
	}
	if _, ok := names["login:latency_p95"]; !ok {
		t.Error("a step in both runs is missing from the comparison")
	}
	for _, gone := range []string{"old:rate", "new:rate"} {
		if _, ok := names[gone]; ok {
			t.Errorf("%s is in only one run and should be left out", gone)
		}
	}

	var buf bytes.Buffer
	WriteComparison(&buf, deltas)
	out := buf.String()
	if !strings.Contains(out, "latency_p95") || !strings.Contains(out, "+100.0%") {
		t.Errorf("table doesn't show the +100%% p95 change:\n%s", out)
	}
}

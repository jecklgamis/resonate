package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

// scripted returns whatever fn says for each call, so a test controls
// exactly which steps succeed or fail.
type scripted struct {
	fn func(ctx context.Context, vuID int) []generator.Result
}

func (s scripted) Do(ctx context.Context, vuID int) []generator.Result { return s.fn(ctx, vuID) }
func (scripted) Protocol() string                                      { return "http" }
func (scripted) Close() error                                          { return nil }

func res(step string, status int) generator.Result {
	return generator.Result{Timestamp: time.Now(), Latency: time.Millisecond, StatusCode: status, Step: step}
}

func alwaysStatus(step string, status int) scripted {
	return scripted{fn: func(ctx context.Context, _ int) []generator.Result {
		time.Sleep(time.Millisecond)
		return []generator.Result{res(step, status)}
	}}
}

func fastAbortChecks(t *testing.T) {
	t.Helper()
	old := abortCheckInterval
	abortCheckInterval = 10 * time.Millisecond
	t.Cleanup(func() { abortCheckInterval = old })
}

func f(v float64) *float64 { return &v }

func successRateAtLeast(v float64, after time.Duration) report.Assertion {
	return report.Assertion{Metric: "success_rate", Min: f(v), Abort: true, AbortAfter: after}
}

// runWithin fails the test if Run doesn't return by the deadline — the whole
// point of abort is that a run which should have stopped doesn't keep going.
func runWithin(t *testing.T, limit time.Duration, g generator.Generator, opts Options) (report.Summary, time.Duration) {
	t.Helper()
	type out struct {
		s report.Summary
		d time.Duration
	}
	done := make(chan out, 1)
	go func() {
		start := time.Now()
		s := Run(context.Background(), g, opts)
		done <- out{s, time.Since(start)}
	}()
	select {
	case o := <-done:
		return o.s, o.d
	case <-time.After(limit):
		t.Fatalf("Run still going after %s; it should have been aborted", limit)
		return report.Summary{}, 0
	}
}

func TestAbortStopsRunWhenAssertionFails(t *testing.T) {
	fastAbortChecks(t)
	opts := Options{Duration: time.Minute, Workers: 2, AbortOn: []report.Assertion{successRateAtLeast(0.9, 0)}}
	s, took := runWithin(t, 10*time.Second, alwaysStatus("api", 500), opts)

	if !strings.Contains(s.Aborted, "success_rate") || !strings.Contains(s.Aborted, ">= 0.90") {
		t.Errorf("Aborted = %q, want the failed assertion", s.Aborted)
	}
	if took > 5*time.Second {
		t.Errorf("took %s of a 1m run; abort should be near-immediate", took)
	}
	if s.Requests == 0 {
		t.Error("the aborted run's summary has no requests; it should cover what ran")
	}
}

func TestAbortLeavesAPassingRunAlone(t *testing.T) {
	fastAbortChecks(t)
	opts := Options{Duration: 300 * time.Millisecond, Workers: 2, AbortOn: []report.Assertion{successRateAtLeast(0.9, 0)}}
	s, took := runWithin(t, 10*time.Second, alwaysStatus("api", 200), opts)
	if s.Aborted != "" {
		t.Errorf("Aborted = %q on a healthy run", s.Aborted)
	}
	if took < 250*time.Millisecond {
		t.Errorf("a passing run was cut short: %s of 300ms", took)
	}
}

func TestAbortWaitsForAbortAfter(t *testing.T) {
	fastAbortChecks(t)
	opts := Options{Duration: time.Minute, Workers: 2, AbortOn: []report.Assertion{successRateAtLeast(0.9, 400*time.Millisecond)}}
	s, took := runWithin(t, 10*time.Second, alwaysStatus("api", 500), opts)
	if s.Aborted == "" {
		t.Fatal("never aborted")
	}
	if took < 400*time.Millisecond {
		t.Errorf("aborted after %s, before the %s grace period", took, 400*time.Millisecond)
	}
}

func TestAbortDiscardsResultsThatCompleteAfterTheStop(t *testing.T) {
	fastAbortChecks(t)
	// VU 0 fails fast and trips the abort; VU 1 is stuck in flight until the
	// run's context is cancelled, then reports "context canceled" — an artifact
	// of stopping, which must not end up in the report. It reports 50 of them
	// because the workers' own send races ctx.Done() (a ready select picks at
	// random), so one result would leak only about half the time and the test
	// could pass without the filter.
	g := scripted{fn: func(ctx context.Context, vuID int) []generator.Result {
		if vuID == 1 {
			<-ctx.Done()
			late := make([]generator.Result, 50)
			for i := range late {
				late[i] = generator.Result{Timestamp: time.Now(), Error: errors.New("context canceled"), Step: "api"}
			}
			return late
		}
		time.Sleep(time.Millisecond)
		return []generator.Result{res("api", 500)}
	}}
	opts := Options{Duration: time.Minute, Workers: 2, AbortOn: []report.Assertion{successRateAtLeast(0.9, 0)}}
	s, _ := runWithin(t, 10*time.Second, g, opts)
	if s.Aborted == "" {
		t.Fatal("never aborted")
	}
	if n := s.Errors["context canceled"]; n != 0 {
		t.Errorf("%d \"context canceled\" results leaked into an aborted run's report", n)
	}
}

func TestAbortOnStepOnlyWatchesThatStep(t *testing.T) {
	fastAbortChecks(t)
	g := scripted{fn: func(ctx context.Context, _ int) []generator.Result {
		time.Sleep(time.Millisecond)
		return []generator.Result{res("good", 200), res("bad", 500)}
	}}
	rule := func(step string) []report.Assertion {
		return []report.Assertion{{Metric: "success_rate", Step: step, Min: f(0.9), Abort: true}}
	}

	s, _ := runWithin(t, 10*time.Second, g, Options{Duration: time.Minute, Workers: 2, AbortOn: rule("bad")})
	if !strings.Contains(s.Aborted, "{step=bad}") {
		t.Errorf("Aborted = %q, want the bad step named", s.Aborted)
	}

	// The whole run is 50% successful, but the watched step is perfect.
	s, took := runWithin(t, 10*time.Second, g, Options{Duration: 300 * time.Millisecond, Workers: 2, AbortOn: rule("good")})
	if s.Aborted != "" || took < 250*time.Millisecond {
		t.Errorf("aborted=%q after %s on a healthy step", s.Aborted, took)
	}
}

func TestAbortIgnoresAStepThatHasNotStartedYet(t *testing.T) {
	fastAbortChecks(t)
	opts := Options{
		Duration: 300 * time.Millisecond, Workers: 1,
		AbortOn: []report.Assertion{{Metric: "success_rate", Step: "later", Min: f(0.9), Abort: true}},
	}
	s, _ := runWithin(t, 10*time.Second, alwaysStatus("api", 200), opts)
	if s.Aborted != "" {
		t.Errorf("Aborted = %q; a step with no requests yet says nothing about it", s.Aborted)
	}
}

func TestAbortWorksInEveryExecutionModel(t *testing.T) {
	fastAbortChecks(t)
	rules := []report.Assertion{successRateAtLeast(0.9, 0)}
	models := map[string]Options{
		"flat": {Duration: time.Minute, Workers: 2, AbortOn: rules},
		"open": {Duration: time.Minute, Rate: 200, Workers: 2, MaxWorkers: 20, AbortOn: rules},
		// A lone stage would ramp from 0 workers, so nothing would run for a while;
		// jump straight to 2 and hold.
		"staged": {Stages: []Stage{{Workers: 2}, {Duration: time.Minute, Workers: 2}}, AbortOn: rules},
	}
	for name, opts := range models {
		t.Run(name, func(t *testing.T) {
			s, took := runWithin(t, 10*time.Second, alwaysStatus("api", 500), opts)
			if s.Aborted == "" || took > 5*time.Second {
				t.Errorf("aborted=%q after %s", s.Aborted, took)
			}
		})
	}
}

func TestNoAbortRulesMeansNoAbortField(t *testing.T) {
	s, _ := runWithin(t, 10*time.Second, alwaysStatus("api", 500), Options{Duration: 200 * time.Millisecond, Workers: 1})
	if s.Aborted != "" {
		t.Errorf("Aborted = %q with no abort rules", s.Aborted)
	}
}

func TestValidateAbort(t *testing.T) {
	ok := []report.Assertion{{Metric: "latency_p99.9", Max: f(1), Abort: true}, {Metric: "rate", Min: f(1), Abort: true}}
	if err := ValidateAbort(ok); err != nil {
		t.Errorf("valid rules rejected: %v", err)
	}
	for _, bad := range []report.Assertion{
		{Metric: "requests", Min: f(1), Abort: true},
		{Metric: "success", Min: f(1), Abort: true},
		{Metric: "bogus", Min: f(1), Abort: true},
		{Metric: "rate", Min: f(1), Abort: true, AbortAfter: -time.Second},
	} {
		if err := ValidateAbort([]report.Assertion{bad}); err == nil {
			t.Errorf("ValidateAbort accepted %+v", bad)
		}
		if err := Validate(Options{Duration: time.Second, AbortOn: []report.Assertion{bad}}); err == nil {
			t.Errorf("Validate accepted abort rule %+v", bad)
		}
	}
}

// namedGen is a generator that advertises its steps, as the HTTP ones do.
type namedGen struct {
	scripted
	names []string
}

func (n namedGen) StepNames() []string { return n.names }

func TestRunListsStepsInGeneratorOrderNotArrivalOrder(t *testing.T) {
	// The generator reports "b" before "a", but the scenario lists a, b.
	g := namedGen{
		scripted: scripted{fn: func(ctx context.Context, _ int) []generator.Result {
			time.Sleep(time.Millisecond)
			return []generator.Result{res("b", 200), res("a", 200)}
		}},
		names: []string{"a", "b"},
	}
	s, _ := runWithin(t, 10*time.Second, g, Options{Requests: 5, Workers: 1})
	if len(s.Steps) != 2 || s.Steps[0].Name != "a" || s.Steps[1].Name != "b" {
		t.Errorf("steps = %+v, want a then b", s.Steps)
	}
}

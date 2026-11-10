package engine

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

// stamped records when its first iteration ran, and answers every call with a
// result for one named step.
type stamped struct {
	step    string
	delay   time.Duration
	started atomic.Int64 // unix nanos of the first Do; 0 until then
	calls   atomic.Int64
}

func (s *stamped) Do(ctx context.Context, _ int) []generator.Result {
	s.started.CompareAndSwap(0, time.Now().UnixNano())
	s.calls.Add(1)
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return []generator.Result{res(s.step, 200)}
}
func (*stamped) Protocol() string { return "http" }
func (*stamped) Close() error     { return nil }

func (s *stamped) firstCall() time.Time { return time.Unix(0, s.started.Load()) }

// stepped is a stamped generator that also lists its steps up front, like the
// HTTP and flow generators do.
type stepped struct {
	*stamped
	names []string
}

func (s stepped) StepNames() []string { return s.names }

func TestRunMultiRunsScenariosConcurrentlyEachWithItsOwnProfile(t *testing.T) {
	// Closed model, 2 workers, versus open model, up to 8 in flight. If they
	// ran one after the other the whole run would take 2×400ms.
	a := &fakeGenerator{delay: 20 * time.Millisecond}
	b := &fakeGenerator{delay: 20 * time.Millisecond}
	specs := []Spec{
		{Name: "closed", Generator: a, Options: Options{Duration: 400 * time.Millisecond, Workers: 2}},
		{Name: "open", Generator: b, Options: Options{Duration: 400 * time.Millisecond, Rate: 200, Workers: 2, MaxWorkers: 8}},
	}
	start := time.Now()
	s, err := RunMulti(context.Background(), specs, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 700*time.Millisecond {
		t.Errorf("took %s: the scenarios ran one after the other, not together", took)
	}

	if len(s.Scenarios) != 2 || s.Scenarios[0].Name != "closed" || s.Scenarios[1].Name != "open" {
		t.Fatalf("scenarios = %+v, want closed then open (spec order)", s.Scenarios)
	}
	closed, open := s.Scenarios[0], s.Scenarios[1]
	if closed.Requests == 0 || open.Requests == 0 {
		t.Fatalf("requests: closed=%d open=%d", closed.Requests, open.Requests)
	}
	if got, want := s.Requests, closed.Requests+open.Requests; got != want {
		t.Errorf("combined requests = %d, want the scenarios' sum %d", got, want)
	}
	// Each scenario kept its own concurrency model.
	if a.peakConcurrency() > 2 {
		t.Errorf("closed scenario peaked at %d, but has only 2 workers", a.peakConcurrency())
	}
	if b.peakConcurrency() <= 2 {
		t.Errorf("open scenario peaked at %d; its open model should grow past 2 workers", b.peakConcurrency())
	}
}

func TestRunMultiStartAfterDelaysOnlyThatScenario(t *testing.T) {
	early, late := &stamped{step: "e"}, &stamped{step: "l"}
	start := time.Now()
	s, err := RunMulti(context.Background(), []Spec{
		{Name: "early", Generator: early, Options: Options{Duration: 100 * time.Millisecond, Workers: 1}},
		{Name: "late", Generator: late, Options: Options{Duration: 100 * time.Millisecond, Workers: 1}, StartAfter: 300 * time.Millisecond},
	}, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if d := early.firstCall().Sub(start); d > 150*time.Millisecond {
		t.Errorf("early scenario started %s in, want immediately", d)
	}
	if d := late.firstCall().Sub(start); d < 300*time.Millisecond {
		t.Errorf("late scenario started %s in, want at least its 300ms start_after", d)
	}
	// start_after is not part of the scenario's own duration…
	if d := s.Scenarios[1].Duration; d > 250*time.Millisecond {
		t.Errorf("late scenario's duration = %s, should be ~100ms (the wait is not counted)", d)
	}
	// …but it is part of the run's.
	if s.Duration < 400*time.Millisecond {
		t.Errorf("combined duration = %s, want ≥ 400ms (300ms wait + 100ms run)", s.Duration)
	}
	if s.Scenarios[1].StartAfter != 300*time.Millisecond || s.Scenarios[0].StartAfter != 0 {
		t.Errorf("StartAfter = %s / %s", s.Scenarios[0].StartAfter, s.Scenarios[1].StartAfter)
	}
}

func TestRunMultiCombinesStepsInScenarioOrderAndMergesSharedLabels(t *testing.T) {
	a := stepped{&stamped{step: "login"}, []string{"login", "shared"}}
	b := stepped{&stamped{step: "shared"}, []string{"shared", "pay"}}
	s, err := RunMulti(context.Background(), []Spec{
		{Name: "a", Generator: a, Options: Options{Requests: 10, Workers: 1}},
		{Name: "b", Generator: b, Options: Options{Requests: 10, Workers: 1}},
	}, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// "pay" never ran in b (its generator only emits "shared"), so it is left
	// out; "shared" is listed once, in first-seen order of the union.
	var names []string
	for _, st := range s.Steps {
		names = append(names, st.Name)
	}
	if got := strings.Join(names, ","); got != "login,shared" {
		t.Errorf("combined steps = %s, want login,shared", got)
	}
	for _, st := range s.Steps {
		if st.Name == "shared" && st.Requests != 10 {
			t.Errorf("shared step requests = %d, want 10 (only b ran it)", st.Requests)
		}
	}
	// Each scenario's own summary only has its own steps.
	if len(s.Scenarios[0].Steps) != 1 || s.Scenarios[0].Steps[0].Name != "login" {
		t.Errorf("scenario a steps = %+v", s.Scenarios[0].Steps)
	}
	if len(s.Scenarios[1].Steps) != 1 || s.Scenarios[1].Steps[0].Name != "shared" {
		t.Errorf("scenario b steps = %+v", s.Scenarios[1].Steps)
	}
}

// OnResult is shared by scenarios that run concurrently; RunMulti must hand it
// results one at a time. Under -race this fails if it does not (the counters
// below are deliberately unsynchronised).
func TestRunMultiSerialisesOnResult(t *testing.T) {
	var perScenario = map[string]int{}
	total := 0
	mk := func(name string) Spec {
		return Spec{Name: name, Generator: &stamped{step: name}, Options: Options{Duration: 200 * time.Millisecond, Workers: 4}}
	}
	s, err := RunMulti(context.Background(), []Spec{mk("a"), mk("b"), mk("c")}, MultiOptions{
		OnResult: func(scenario string, _ generator.Result) {
			perScenario[scenario]++
			total++
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != s.Requests {
		t.Errorf("OnResult saw %d results, summary has %d requests", total, s.Requests)
	}
	for _, sc := range s.Scenarios {
		if perScenario[sc.Name] != sc.Requests {
			t.Errorf("scenario %s: OnResult saw %d, summary has %d", sc.Name, perScenario[sc.Name], sc.Requests)
		}
	}
}

func TestRunMultiKeepsASpecsOwnOnResult(t *testing.T) {
	var own atomic.Int64
	g := &stamped{step: "x"}
	_, err := RunMulti(context.Background(), []Spec{
		{Name: "a", Generator: g, Options: Options{Requests: 20, Workers: 2, OnResult: func(generator.Result) { own.Add(1) }}},
	}, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if own.Load() != 20 {
		t.Errorf("the spec's own OnResult saw %d results, want 20", own.Load())
	}
}

func TestRunMultiAbortInOneScenarioStopsAllOfThem(t *testing.T) {
	fastAbortChecks(t)
	healthy := &stamped{step: "ok"}
	specs := []Spec{
		{Name: "healthy", Generator: healthy, Options: Options{Duration: time.Minute, Workers: 2}},
		{Name: "broken", Generator: alwaysStatus("api", 500), Options: Options{
			Duration: time.Minute, Workers: 2, AbortOn: []report.Assertion{successRateAtLeast(0.9, 0)},
		}},
	}
	type out struct {
		s   report.Summary
		err error
	}
	done := make(chan out, 1)
	go func() {
		s, err := RunMulti(context.Background(), specs, MultiOptions{})
		done <- out{s, err}
	}()
	var o out
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunMulti still running after 10s: the abort did not stop the healthy scenario")
	}
	if o.err != nil {
		t.Fatal(o.err)
	}
	if !strings.Contains(o.s.Aborted, `scenario "broken"`) || !strings.Contains(o.s.Aborted, "success_rate") {
		t.Errorf("Aborted = %q, want it to name the scenario and the assertion", o.s.Aborted)
	}
	if o.s.Scenarios[1].Aborted == "" {
		t.Error("the broken scenario's own summary should carry the abort")
	}
	if o.s.Scenarios[0].Requests == 0 {
		t.Error("the healthy scenario ran before the abort; its results should still be reported")
	}
}

func TestRunMultiContextCancelBeforeStartAfterRunsNothing(t *testing.T) {
	g := &stamped{step: "x"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	s, err := RunMulti(ctx, []Spec{
		{Name: "later", Generator: g, Options: Options{Duration: time.Minute, Workers: 1}, StartAfter: time.Hour},
	}, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s: cancelling should not wait out start_after", took)
	}
	if g.calls.Load() != 0 || s.Requests != 0 {
		t.Errorf("a scenario cancelled before it began made %d calls / %d requests", g.calls.Load(), s.Requests)
	}
	if len(s.Scenarios) != 1 || s.Scenarios[0].Name != "later" {
		t.Errorf("scenarios = %+v; a scenario that never ran should still be listed", s.Scenarios)
	}
}

func TestRunMultiFlagsSaturationOnTheScenarioAndTheRun(t *testing.T) {
	slow := &fakeGenerator{delay: 30 * time.Millisecond}
	fine := &stamped{step: "x"}
	s, err := RunMulti(context.Background(), []Spec{
		{Name: "fine", Generator: fine, Options: Options{Duration: 300 * time.Millisecond, Workers: 2}},
		{Name: "starved", Generator: slow, Options: Options{Duration: 300 * time.Millisecond, Rate: 1000, Workers: 1, MaxWorkers: 1}},
	}, MultiOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Scenarios[0].Saturated {
		t.Error("the closed scenario should not be saturated")
	}
	if !s.Scenarios[1].Saturated || s.Scenarios[1].PeakConcurrency != 1 {
		t.Errorf("starved scenario: saturated=%v peak=%d", s.Scenarios[1].Saturated, s.Scenarios[1].PeakConcurrency)
	}
	if !s.Saturated {
		t.Error("the combined summary should flag that some scenario was saturated")
	}
}

func TestValidateMulti(t *testing.T) {
	ok := Options{Duration: time.Second, Workers: 1}
	g := &stamped{}
	tests := []struct {
		name  string
		specs []Spec
		want  string
	}{
		{"none", nil, "no scenarios"},
		{"nameless", []Spec{{Generator: g, Options: ok}}, "name is required"},
		{"duplicate", []Spec{{Name: "a", Generator: g, Options: ok}, {Name: "a", Generator: g, Options: ok}}, `"a": duplicate name`},
		{"no generator", []Spec{{Name: "a", Options: ok}}, "no generator"},
		{"negative start_after", []Spec{{Name: "a", Generator: g, Options: ok, StartAfter: -time.Second}}, "start_after must not be negative"},
		{"bad options name the scenario", []Spec{{Name: "a", Generator: g, Options: ok}, {Name: "b", Generator: g, Options: Options{Duration: time.Second, Workers: -1}}}, `scenario "b"`},
		{"bad abort rule", []Spec{{Name: "a", Generator: g, Options: Options{Duration: time.Second, Workers: 1, AbortOn: []report.Assertion{{Metric: "requests", Min: f(1), Abort: true}}}}}, `scenario "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMulti(tt.specs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ValidateMulti = %v, want an error containing %q", err, tt.want)
			}
			// RunMulti must refuse the same input, without running anything.
			if _, err := RunMulti(context.Background(), tt.specs, MultiOptions{}); err == nil {
				t.Error("RunMulti accepted what ValidateMulti rejects")
			}
		})
	}
	if err := ValidateMulti([]Spec{{Name: "a", Generator: g, Options: ok}, {Name: "b", Generator: g, Options: ok, StartAfter: time.Second}}); err != nil {
		t.Errorf("valid specs rejected: %v", err)
	}
}

// Closing is the caller's job and must stay possible after RunMulti returns:
// RunMulti may not close what it was given.
func TestRunMultiDoesNotCloseGenerators(t *testing.T) {
	g := &fakeGenerator{}
	if _, err := RunMulti(context.Background(), []Spec{{Name: "a", Generator: g, Options: Options{Requests: 3, Workers: 1}}}, MultiOptions{}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	mu.Lock()
	closed := g.closed
	mu.Unlock()
	if closed {
		t.Error("RunMulti closed a generator it does not own")
	}
}

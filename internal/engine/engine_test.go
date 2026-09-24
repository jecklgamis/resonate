package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

// fakeGenerator records every vuID it's called with and returns one
// zero-latency successful result per Do call, so tests can run fast and
// deterministically without any network I/O.
type fakeGenerator struct {
	mu       sync.Mutex
	vuIDs    []int
	delay    time.Duration
	closed   bool
	inFlight int
	peak     int
}

func (f *fakeGenerator) Do(ctx context.Context, vuID int) []generator.Result {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	f.mu.Lock()
	f.vuIDs = append(f.vuIDs, vuID)
	f.inFlight--
	f.mu.Unlock()
	return []generator.Result{{Timestamp: time.Now(), StatusCode: 200}}
}

// peakConcurrency returns the highest number of concurrent Do calls
// observed, for tests asserting a model's concurrency cap was respected.
func (f *fakeGenerator) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func (f *fakeGenerator) Protocol() string { return "fake" }

func (f *fakeGenerator) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeGenerator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.vuIDs)
}

func (f *fakeGenerator) distinctVUIDs() map[int]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := make(map[int]bool, len(f.vuIDs))
	for _, id := range f.vuIDs {
		set[id] = true
	}
	return set
}

func TestResolveWorkers(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want int
	}{
		{"explicit workers wins", Options{Workers: 3, Rate: 100}, 3},
		{"rate-aware default, above floor", Options{Rate: 50}, 50},
		{"rate-aware default, below floor", Options{Rate: 2}, 10},
		{"rate-aware default, rounds up", Options{Rate: 10.1}, 11},
		{"no workers, no rate", Options{}, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveWorkers(c.opts)
			if got != c.want {
				t.Errorf("ResolveWorkers(%+v) = %d, want %d", c.opts, got, c.want)
			}
		})
	}
}

func TestValidateRejectsNegativeRate(t *testing.T) {
	err := Validate(Options{Rate: -1})
	if err == nil {
		t.Fatal("expected an error for a negative Rate")
	}
}

func TestValidateRejectsNegativeWorkers(t *testing.T) {
	err := Validate(Options{Workers: -1})
	if err == nil {
		t.Fatal("expected an error for negative Workers")
	}
}

func TestValidateRejectsNegativeStageFields(t *testing.T) {
	cases := []Options{
		{Stages: []Stage{{Rate: -1}}},
		{Stages: []Stage{{Workers: -1}}},
		{Stages: []Stage{{Duration: -time.Second}}},
	}
	for _, opts := range cases {
		if err := Validate(opts); err == nil {
			t.Errorf("Validate(%+v): expected an error", opts)
		}
	}
}

func TestValidateAcceptsSaneOptions(t *testing.T) {
	cases := []Options{
		{},
		{Rate: 50, Workers: 10},
		{Stages: []Stage{{Duration: time.Second, Workers: 10, Rate: 10}}},
	}
	for _, opts := range cases {
		if err := Validate(opts); err != nil {
			t.Errorf("Validate(%+v): unexpected error: %v", opts, err)
		}
	}
}

func TestRunFlatRequestsCap(t *testing.T) {
	a := &fakeGenerator{}
	Run(context.Background(), a, Options{Workers: 4, Requests: 37})
	if got := a.callCount(); got != 37 {
		t.Errorf("callCount = %d, want exactly 37 (the Requests cap)", got)
	}
}

func TestRunFlatIterationsOnlyIsExactAndSelfTerminating(t *testing.T) {
	a := &fakeGenerator{}
	start := time.Now()
	Run(context.Background(), a, Options{Workers: 5, Iterations: 4})
	elapsed := time.Since(start)

	if got := a.callCount(); got != 20 {
		t.Errorf("callCount = %d, want exactly 20 (5 workers * 4 iterations, no replenishment)", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("run took %v, expected it to self-terminate quickly with no duration/rate set", elapsed)
	}
}

func TestRunFlatIterationsWithReplenishmentExceedsWorkerCount(t *testing.T) {
	a := &fakeGenerator{}
	// 2 workers, iterations=1 (each VU departs immediately), capped at 20
	// total requests: with replenishment, more than 2 distinct vuIDs should
	// appear even though only 2 slots ever run concurrently.
	Run(context.Background(), a, Options{Workers: 2, Iterations: 1, Requests: 20})

	if got := a.callCount(); got != 20 {
		t.Fatalf("callCount = %d, want 20", got)
	}
	distinct := a.distinctVUIDs()
	if len(distinct) <= 2 {
		t.Errorf("distinct vuIDs = %v, want more than 2 (workers) since VUs should be replenished", distinct)
	}
}

func TestRunClosesGenerator(t *testing.T) {
	// Run itself doesn't call Close (callers are responsible, per
	// cli.runAndReport's defer a.Close()) — verify that assumption holds so
	// a future refactor doesn't silently start double-closing generators.
	a := &fakeGenerator{}
	Run(context.Background(), a, Options{Workers: 1, Requests: 1})
	if a.closed {
		t.Error("Run must not close the Generator itself; that's the caller's responsibility")
	}
}

func TestRunFlatClosedModelCapsConcurrencyAtWorkers(t *testing.T) {
	// Closed model (constant-vus): Workers is a hard concurrency cap even
	// with a target Rate high enough that, in open model, it would grow
	// past it (see TestRunOpenModelSustainsRateDespiteSlowWorkers).
	a := &fakeGenerator{delay: 50 * time.Millisecond}
	Run(context.Background(), a, Options{Workers: 3, Rate: 1000, Duration: 300 * time.Millisecond})
	if peak := a.peakConcurrency(); peak > 3 {
		t.Errorf("peak concurrency = %d, want <= 3 (Workers); closed model must not exceed its concurrency cap", peak)
	}
}

func TestRunFlatDurationStops(t *testing.T) {
	a := &fakeGenerator{delay: 5 * time.Millisecond}
	start := time.Now()
	summary := Run(context.Background(), a, Options{Workers: 2, Duration: 100 * time.Millisecond})
	elapsed := time.Since(start)

	if elapsed < 90*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("run took %v, want roughly the 100ms Duration", elapsed)
	}
	if summary.Requests == 0 {
		t.Error("expected at least some requests to have completed")
	}
}

func TestScheduleAtRampsLinearly(t *testing.T) {
	stages := []Stage{
		{Duration: 4 * time.Second, Workers: 20, Rate: 40},
	}
	cases := []struct {
		elapsed     time.Duration
		wantWorkers int
		wantRate    float64
	}{
		{0, 0, 0},
		{1 * time.Second, 5, 10},
		{2 * time.Second, 10, 20},
		{4 * time.Second, 20, 40}, // exactly at the boundary falls through to "past all stages"
	}
	for _, c := range cases {
		w, r := scheduleAt(stages, c.elapsed)
		if w != c.wantWorkers || r != c.wantRate {
			t.Errorf("scheduleAt(%v) = (%d, %v), want (%d, %v)", c.elapsed, w, r, c.wantWorkers, c.wantRate)
		}
	}
}

func TestScheduleAtHoldsSteadyAcrossEqualStage(t *testing.T) {
	stages := []Stage{
		{Duration: 2 * time.Second, Workers: 10, Rate: 10},
		{Duration: 2 * time.Second, Workers: 10, Rate: 10}, // same values = hold steady, no ramp
	}
	w, r := scheduleAt(stages, 3*time.Second)
	if w != 10 || r != 10 {
		t.Errorf("scheduleAt mid-hold-stage = (%d, %v), want (10, 10)", w, r)
	}
}

func TestScheduleAtRampsDownToZero(t *testing.T) {
	stages := []Stage{
		{Duration: 2 * time.Second, Workers: 10, Rate: 10},
		{Duration: 2 * time.Second, Workers: 0, Rate: 0},
	}
	w, r := scheduleAt(stages, 3*time.Second) // 1s into the 2s ramp-down
	if w != 5 {
		t.Errorf("workers mid ramp-down = %d, want 5", w)
	}
	if r != 5 {
		t.Errorf("rate mid ramp-down = %v, want 5", r)
	}
}

func TestScheduleAtInstantaneousJump(t *testing.T) {
	stages := []Stage{
		{Duration: 0, Workers: 15, Rate: 30}, // instant jump, no ramp
		{Duration: 1 * time.Second, Workers: 15, Rate: 30},
	}
	w, r := scheduleAt(stages, 0)
	if w != 15 || r != 30 {
		t.Errorf("scheduleAt at t=0 with a zero-duration first stage = (%d, %v), want (15, 30)", w, r)
	}
}

func TestScheduleAtPastAllStagesHoldsLastValue(t *testing.T) {
	stages := []Stage{
		{Duration: 1 * time.Second, Workers: 8, Rate: 16},
	}
	w, r := scheduleAt(stages, 10*time.Second)
	if w != 8 || r != 16 {
		t.Errorf("scheduleAt past the end = (%d, %v), want the last stage's values (8, 16)", w, r)
	}
}

func TestTotalDuration(t *testing.T) {
	stages := []Stage{
		{Duration: 3 * time.Second},
		{Duration: 5 * time.Second},
	}
	if got := totalDuration(stages); got != 8*time.Second {
		t.Errorf("totalDuration = %v, want 8s", got)
	}
}

func TestMaxWorkers(t *testing.T) {
	cases := []struct {
		stages []Stage
		want   int
	}{
		{[]Stage{{Workers: 5}, {Workers: 20}, {Workers: 3}}, 20},
		{[]Stage{{Workers: 0}}, 1}, // floor of 1 so at least one goroutine spawns
	}
	for _, c := range cases {
		if got := maxWorkers(c.stages); got != c.want {
			t.Errorf("maxWorkers(%+v) = %d, want %d", c.stages, got, c.want)
		}
	}
}

func TestRunStagedRespectsRequestsCap(t *testing.T) {
	a := &fakeGenerator{}
	Run(context.Background(), a, Options{
		Stages: []Stage{
			{Duration: 0, Workers: 10, Rate: 1000}, // instant jump, no ramp delay
			{Duration: 2 * time.Second, Workers: 10, Rate: 1000},
		},
		Requests: 25,
	})
	if got := a.callCount(); got != 25 {
		t.Errorf("callCount = %d, want exactly 25 (the Requests cap, reached well before the 2s stage ends)", got)
	}
}

func TestValidateOpenModel(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"negative max workers", Options{MaxWorkers: -1}, true},
		{"max workers without rate", Options{MaxWorkers: 5}, true},
		{"max workers below workers", Options{MaxWorkers: 2, Workers: 5, Rate: 10}, true},
		{"valid open model", Options{MaxWorkers: 20, Workers: 5, Rate: 10}, false},
		{"max workers equal to workers is valid (no growth headroom, but not an error)", Options{MaxWorkers: 5, Workers: 5, Rate: 10}, false},
		{"max workers with stages is valid (raises the ramping cap)", Options{MaxWorkers: 5, Stages: []Stage{{Duration: time.Second, Rate: 10}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.opts)
			if c.wantErr && err == nil {
				t.Error("expected an error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRunOpenModelSustainsRateDespiteSlowWorkers(t *testing.T) {
	// 20 in-flight iterations needed to sustain ~100/s at 200ms latency
	// (rate * latency = concurrency needed). Workers is deliberately capped
	// far below that (2) to prove open model grows past it via MaxWorkers.
	a := &fakeGenerator{delay: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	summary := Run(ctx, a, Options{Rate: 100, Workers: 2, MaxWorkers: 50})

	// Loose bound: a closed model capped at 2 workers could only ever
	// achieve ~10/s (2 / 200ms) in this window: 5 iterations across 500ms.
	// Open model growing concurrency should comfortably exceed that.
	if got := a.callCount(); got <= 10 {
		t.Errorf("callCount = %d, want well above the ~5 a closed 2-worker model would manage, proving concurrency grew past Workers", got)
	}
	if summary.PeakConcurrency <= 2 {
		t.Errorf("PeakConcurrency = %d, want > 2 (Workers), proving open model exceeded the closed-model cap", summary.PeakConcurrency)
	}
}

func TestRunOpenModelReportsSaturationWhenMaxWorkersTooSmall(t *testing.T) {
	// Rate*latency = 100/s * 200ms = 20 concurrent iterations needed;
	// MaxWorkers only allows 3, so the run can't keep up and must flag it.
	a := &fakeGenerator{delay: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	summary := Run(ctx, a, Options{Rate: 100, MaxWorkers: 3})

	if !summary.Saturated {
		t.Error("expected Saturated=true: MaxWorkers=3 can't sustain 100/s at 200ms latency")
	}
	if summary.PeakConcurrency > 3 {
		t.Errorf("PeakConcurrency = %d, want <= 3 (the MaxWorkers ceiling)", summary.PeakConcurrency)
	}
}

func TestRunOpenModelRespectsRequestsCap(t *testing.T) {
	a := &fakeGenerator{}
	Run(context.Background(), a, Options{Rate: 1000, MaxWorkers: 10, Requests: 37})
	if got := a.callCount(); got != 37 {
		t.Errorf("callCount = %d, want exactly 37 (the Requests cap)", got)
	}
}

func TestRunOpenModelIterationsReplenish(t *testing.T) {
	a := &fakeGenerator{}
	Run(context.Background(), a, Options{Rate: 1000, MaxWorkers: 3, Iterations: 1, Requests: 30})
	if got := a.callCount(); got != 30 {
		t.Fatalf("callCount = %d, want 30", got)
	}
	distinct := a.distinctVUIDs()
	if len(distinct) <= 3 {
		t.Errorf("distinct vuIDs = %v, want more than 3 (MaxWorkers) since VUs should be replenished", distinct)
	}
}

func TestRunStagedRampingVUsWithNoRateDoesNotHang(t *testing.T) {
	// Rate==0 on every stage used to hang forever: workers always waited on
	// the tokens channel, which nothing ever fed when Rate stayed 0. This is
	// the closed-model ramping-vus case — concurrency ramps via Workers,
	// iterations run back-to-back with no rate pacing at all.
	a := &fakeGenerator{}
	done := make(chan report.Summary, 1)
	go func() {
		done <- Run(context.Background(), a, Options{
			Stages: []Stage{
				{Duration: 100 * time.Millisecond, Workers: 5, Rate: 0},
				{Duration: 100 * time.Millisecond, Workers: 5, Rate: 0},
			},
		})
	}()

	select {
	case summary := <-done:
		if summary.Requests == 0 {
			t.Error("expected at least some iterations to have run")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runStaged hung with Rate==0 on every stage (ramping-vus)")
	}
}

func TestRunStagedRampingVUsCapsConcurrencyAtRampingWorkers(t *testing.T) {
	// Closed model / ramping-vus (Rate==0 throughout): concurrency must
	// never exceed the ramping Workers value, at any point in the ramp.
	a := &fakeGenerator{delay: 30 * time.Millisecond}
	Run(context.Background(), a, Options{
		Stages: []Stage{
			{Duration: 100 * time.Millisecond, Workers: 4, Rate: 0},
			{Duration: 100 * time.Millisecond, Workers: 4, Rate: 0},
		},
	})
	if peak := a.peakConcurrency(); peak > 4 {
		t.Errorf("peak concurrency = %d, want <= 4 (the ramping Workers cap)", peak)
	}
}

func TestRunStagedOpenModelExceedsRampingWorkersCap(t *testing.T) {
	// Without MaxWorkers, concurrency is capped at the stage's ramping
	// Workers value (2 here) even though the target rate needs far more to
	// keep up with 200ms latency — so achieved throughput undershoots and
	// Saturated should be set. With MaxWorkers raising the ceiling,
	// concurrency should grow well past 2 to actually sustain the rate.
	bounded := &fakeGenerator{delay: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	boundedSummary := Run(ctx, bounded, Options{
		Stages: []Stage{{Duration: 0, Workers: 2, Rate: 100}, {Duration: time.Second, Workers: 2, Rate: 100}},
	})
	if !boundedSummary.Saturated {
		t.Error("expected Saturated=true: 2 ramping workers can't sustain 100/s at 200ms latency without MaxWorkers")
	}
	if boundedSummary.PeakConcurrency > 2 {
		t.Errorf("PeakConcurrency = %d, want <= 2 (the ramping Workers cap, MaxWorkers unset)", boundedSummary.PeakConcurrency)
	}

	open := &fakeGenerator{delay: 200 * time.Millisecond}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel2()
	openSummary := Run(ctx2, open, Options{
		Stages:     []Stage{{Duration: 0, Workers: 2, Rate: 100}, {Duration: time.Second, Workers: 2, Rate: 100}},
		MaxWorkers: 50,
	})
	if openSummary.PeakConcurrency <= 2 {
		t.Errorf("PeakConcurrency = %d, want > 2, proving MaxWorkers let concurrency exceed the stage's ramping Workers value", openSummary.PeakConcurrency)
	}
	if open.callCount() <= bounded.callCount() {
		t.Errorf("open-model callCount = %d, want more than the Workers-bounded run's %d", open.callCount(), bounded.callCount())
	}
}

func TestRunStagedIterationsReplenish(t *testing.T) {
	a := &fakeGenerator{}
	Run(context.Background(), a, Options{
		Stages: []Stage{
			{Duration: 0, Workers: 3, Rate: 1000}, // instant jump, no ramp delay
			{Duration: 1 * time.Second, Workers: 3, Rate: 1000},
		},
		Iterations: 1,
		Requests:   30,
	})
	if got := a.callCount(); got != 30 {
		t.Fatalf("callCount = %d, want 30", got)
	}
	distinct := a.distinctVUIDs()
	if len(distinct) <= 3 {
		t.Errorf("distinct vuIDs = %v, want more than 3 (workers) since VUs should be replenished", distinct)
	}
}

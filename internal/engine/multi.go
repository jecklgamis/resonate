package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

// Spec is one scenario of a multi-scenario run: a generator driven by its own
// load profile, optionally starting some time after the run begins.
type Spec struct {
	// Name labels the scenario in the report and in scenario-scoped
	// assertions. Must be non-empty and unique within a run.
	Name string

	Generator generator.Generator
	Options   Options

	// StartAfter delays this scenario's start relative to the run's start (0:
	// start immediately). The delay is not part of the scenario's own
	// Duration.
	StartAfter time.Duration
}

// MultiOptions is what RunMulti takes beyond the scenarios themselves.
type MultiOptions struct {
	// OnResult, if set, is called for every result of every scenario, with
	// the scenario's name. Unlike Options.OnResult it is shared by scenarios
	// that run concurrently, so RunMulti serializes the calls: the callback
	// never runs on two goroutines at once and needs no locking of its own.
	OnResult func(scenario string, r generator.Result)
}

// ValidateMulti checks specs the way RunMulti will, without running anything,
// so a caller can reject a bad scenario set before opening any connection.
func ValidateMulti(specs []Spec) error {
	if len(specs) == 0 {
		return fmt.Errorf("no scenarios to run")
	}
	seen := make(map[string]bool, len(specs))
	for i, sp := range specs {
		if sp.Name == "" {
			return fmt.Errorf("scenario %d: name is required", i)
		}
		if seen[sp.Name] {
			return fmt.Errorf("scenario %q: duplicate name", sp.Name)
		}
		seen[sp.Name] = true
		if sp.Generator == nil {
			return fmt.Errorf("scenario %q: no generator", sp.Name)
		}
		if sp.StartAfter < 0 {
			return fmt.Errorf("scenario %q: start_after must not be negative", sp.Name)
		}
		if err := Validate(sp.Options); err != nil {
			return fmt.Errorf("scenario %q: %w", sp.Name, err)
		}
		if err := ValidateAbort(sp.Options.AbortOn); err != nil {
			return fmt.Errorf("scenario %q: %w", sp.Name, err)
		}
	}
	return nil
}

// StepLookup answers report.ValidateScopedSteps for specs: the step names of
// one scenario, or, for an assertion with no scenario, of every scenario
// together. A generator that names no steps (WebSocket, gRPC, SSE)
// contributes none, and a scope supports step assertions only if at least one
// of its generators names steps.
func StepLookup(specs []Spec) func(scenario string) (known []string, supported bool) {
	return func(scenario string) ([]string, bool) {
		var known []string
		supported := false
		for _, sp := range specs {
			if scenario != "" && sp.Name != scenario {
				continue
			}
			if sn, ok := sp.Generator.(interface{ StepNames() []string }); ok {
				supported = true
				known = append(known, sn.StepNames()...)
			}
		}
		return known, supported
	}
}

// RunMulti runs every spec concurrently, each with its own load profile
// (closed or open model, flat or staged — whatever its Options say), and
// returns one Summary for the whole run. That Summary's headline numbers,
// percentiles and per-step breakdown combine every scenario's requests (steps
// that share a label merge), and its Scenarios field holds each scenario's own
// Summary in spec order.
//
// An assertion in a scenario's Options.AbortOn that trips stops that scenario
// and cancels every other one too (the combined Summary's Aborted names the
// scenario); ctx being cancelled stops them all. Scenarios cut off before
// their StartAfter simply report no requests.
//
// The caller closes the generators, once RunMulti has returned.
func RunMulti(ctx context.Context, specs []Spec, mo MultiOptions) (report.Summary, error) {
	if err := ValidateMulti(specs); err != nil {
		return report.Summary{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	combined := report.NewAggregator()
	var order []string
	for _, sp := range specs {
		if sn, ok := sp.Generator.(interface{ StepNames() []string }); ok {
			for _, n := range sn.StepNames() {
				if !slices.Contains(order, n) {
					order = append(order, n)
				}
			}
		}
	}
	combined.SetStepOrder(order)

	// Each scenario's engine.Run feeds results from its own goroutine; this
	// lock is what makes the shared aggregator, and the caller's OnResult,
	// safe to use from all of them.
	var mu sync.Mutex
	start := time.Now()

	summaries := make([]report.Summary, len(specs))
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			summaries[i] = runSpec(ctx, cancel, sp, &mu, combined, mo)
		}()
	}
	wg.Wait()

	out := combined.Summary(time.Since(start))
	for i, sp := range specs {
		out.Scenarios = append(out.Scenarios, report.ScenarioSummary{Name: sp.Name, StartAfter: sp.StartAfter, Summary: summaries[i]})
		if summaries[i].Saturated {
			out.Saturated = true
		}
		if out.Aborted == "" && summaries[i].Aborted != "" {
			out.Aborted = summaries[i].Aborted
		}
	}
	return out, nil
}

// runSpec waits out the scenario's StartAfter and runs it, folding its
// results into the shared aggregator. A scenario whose abort assertion trips
// cancels the whole run via cancelAll.
func runSpec(ctx context.Context, cancelAll context.CancelFunc, sp Spec, mu *sync.Mutex, combined *report.Aggregator, mo MultiOptions) report.Summary {
	if sp.StartAfter > 0 {
		t := time.NewTimer(sp.StartAfter)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return report.NewAggregator().Summary(0)
		}
	}
	if ctx.Err() != nil {
		return report.NewAggregator().Summary(0)
	}

	opts := sp.Options
	own := opts.OnResult
	opts.OnResult = func(r generator.Result) {
		mu.Lock()
		defer mu.Unlock()
		combined.Add(r)
		if own != nil {
			own(r)
		}
		if mo.OnResult != nil {
			mo.OnResult(sp.Name, r)
		}
	}

	summary := Run(ctx, sp.Generator, opts)
	if summary.Aborted != "" {
		summary.Aborted = fmt.Sprintf("scenario %q: %s", sp.Name, summary.Aborted)
		cancelAll()
	}
	return summary
}

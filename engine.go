package resonate

import (
	"context"

	"github.com/jecklgamis/resonate/internal/engine"
)

// Stage is one segment of a staged load schedule (see Options.Stages).
type Stage = engine.Stage

// Options controls the shape of the load: how long/how many iterations to
// run, at what rate, and with how much concurrency.
type Options = engine.Options

// Run executes gen according to opts until ctx is cancelled, the configured
// Duration/Stages schedule elapses, or Requests have been sent (whichever
// comes first), then returns the aggregated Summary. Run does not close
// gen — the caller owns its lifecycle (defer gen.Close()).
func Run(ctx context.Context, gen Generator, opts Options) Summary {
	return engine.Run(ctx, gen, opts)
}

// Validate rejects an Options that's malformed in a way that would
// otherwise degrade into silent no-op behavior or an unthrottled busy loop
// (e.g. negative Rate/Workers) — call it before Run.
func Validate(opts Options) error { return engine.Validate(opts) }

// ValidateAbort rejects assertions that can't stop a run early (a count such as
// requests/success, an unknown metric, a negative AbortAfter).
func ValidateAbort(assertions []Assertion) error { return engine.ValidateAbort(assertions) }

// ResolveWorkers returns the worker count Run will actually use for opts —
// either opts.Workers if set, or the rate-aware default when it's 0.
func ResolveWorkers(opts Options) int { return engine.ResolveWorkers(opts) }

// ScenarioSpec is one scenario of a multi-scenario run: a Generator driven by
// its own Options, optionally starting StartAfter into the run. See RunMulti.
type ScenarioSpec = engine.Spec

// MultiOptions is what RunMulti takes beyond the scenarios themselves.
type MultiOptions = engine.MultiOptions

// RunMulti runs every spec concurrently — each with its own load profile —
// and returns one Summary: its headline numbers combine every scenario, and
// its Scenarios field holds each one's own Summary in spec order. It validates
// specs first (see ValidateMulti) and does not close the generators.
func RunMulti(ctx context.Context, specs []ScenarioSpec, mo MultiOptions) (Summary, error) {
	return engine.RunMulti(ctx, specs, mo)
}

// ValidateMulti rejects a scenario set RunMulti would refuse (no scenarios, a
// missing or duplicate name, a negative StartAfter, an invalid Options).
func ValidateMulti(specs []ScenarioSpec) error { return engine.ValidateMulti(specs) }

// StepLookup returns, for ValidateScopedSteps, the step names of a named
// scenario of specs (or, given "", of all of them together).
func StepLookup(specs []ScenarioSpec) func(scenario string) (known []string, supported bool) {
	return engine.StepLookup(specs)
}

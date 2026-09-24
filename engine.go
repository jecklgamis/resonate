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

// ResolveWorkers returns the worker count Run will actually use for opts —
// either opts.Workers if set, or the rate-aware default when it's 0.
func ResolveWorkers(opts Options) int { return engine.ResolveWorkers(opts) }

// Package generator defines the protocol-agnostic interface load generators implement.
package generator

import (
	"context"
	"time"
)

// Result captures the outcome of a single request/operation.
type Result struct {
	Timestamp  time.Time
	Latency    time.Duration
	StatusCode int // protocol-specific status (HTTP status, WS close code, ...)
	BytesIn    int64
	BytesOut   int64
	Error      error
	Protocol   string

	// successOverride, if non-nil, replaces the default 2xx/3xx-based
	// Success() calculation — set by doHTTP when a target's ExpectStatus
	// check ran, so e.g. an expected 404 can count as a success (and an
	// unexpected 200 can count as a failure). nil (the zero value, so every
	// Result literal elsewhere in the codebase that doesn't know about this
	// field is unaffected) means "use the default range check."
	successOverride *bool
}

// Success reports whether the result should be counted as a success.
func (r Result) Success() bool {
	if r.successOverride != nil {
		return *r.successOverride
	}
	return r.Error == nil && r.StatusCode >= 200 && r.StatusCode < 400
}

// Generator performs one unit of work ("iteration") and reports the result of
// each HTTP call it made — a single-request generator returns a one-element
// slice; a multi-step flow returns one Result per step executed.
//
// vuID identifies the calling virtual user, so a Generator can assign it a
// sticky identity/session (e.g. from an identities pool) and cache
// once-per-VU setup work. It's stable for that VU's lifetime, which is
// usually — but not always — the engine goroutine's entire lifetime: if
// engine.Options.Iterations bounds a VU to a fixed number of iterations, a
// fresh vuID takes over the same goroutine once that VU departs, so distinct
// vuID values may arrive on the same goroutine over a run. Implementations
// must be safe for concurrent use by multiple goroutines, each active vuID
// distinct at any given moment.
type Generator interface {
	Do(ctx context.Context, vuID int) []Result
	Protocol() string
	Close() error
}

package engine

import (
	"context"
	"time"

	"github.com/jecklgamis/resonate/internal/report"
)

// abortCheckInterval is how often a running test is checked against its
// abort assertions. A var so tests needn't wait whole seconds.
var abortCheckInterval = time.Second

// aborter stops a run early when an abort assertion fails. It is only ever
// used from collect's goroutine (the one consuming results), so it needs no
// locking; every method is safe to call on a nil *aborter and does nothing,
// so runs without abort assertions pay for one nil check per result.
type aborter struct {
	rules  []report.Assertion
	cancel context.CancelFunc
	last   time.Time
	reason string // set once, when the run is stopped
}

// stopped reports whether the run has been stopped by a failed assertion.
func (ab *aborter) stopped() bool { return ab != nil && ab.reason != "" }

// observe checks the rules if abortCheckInterval has passed since the last
// check, stopping the run on the first failure. start is when the run began,
// which AbortAfter is measured from.
func (ab *aborter) observe(agg *report.Aggregator, start time.Time) {
	if ab == nil || ab.reason != "" {
		return
	}
	now := time.Now()
	if now.Sub(ab.last) < abortCheckInterval {
		return
	}
	ab.last = now
	elapsed := now.Sub(start)

	var due []report.Assertion
	for _, a := range ab.rules {
		if elapsed >= a.AbortAfter {
			due = append(due, a)
		}
	}
	if len(due) == 0 {
		return
	}
	snap := agg.Snapshot(elapsed)
	if snap.Requests == 0 {
		return // nothing to judge yet; "0 requests" would trip every rate
	}
	failures, err := snap.EvaluatePartial(due)
	if err != nil || len(failures) == 0 {
		return // an unknown metric is rejected by Validate before the run starts
	}
	ab.reason = failures[0].String()
	ab.cancel()
}

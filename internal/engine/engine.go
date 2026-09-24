// Package engine drives a generator.Generator at a configured rate and
// concurrency, aggregating results into a report.Summary.
package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

// Stage is one segment of a staged load schedule (see Options.Stages).
// Workers and Rate ramp linearly from the previous stage's end values (0 for
// the first stage) to this stage's values, over Duration. Setting Workers
// and/or Rate equal to the previous stage's end value holds it constant
// instead of ramping — that's how you express a steady-state period after a
// warm-up ramp. A zero Duration is an instantaneous jump to the target
// values.
type Stage struct {
	Duration time.Duration
	Workers  int     // target concurrent workers ("virtual users") by the end of this stage
	Rate     float64 // target iterations/sec by the end of this stage
}

// Options controls the shape of the load: how long/how many iterations to
// run, at what rate, and with how much concurrency. An "iteration" is one
// call to Generator.Do — for a single-request generator that's one HTTP call;
// for a multi-step flow it's one full pass through the flow. Duration,
// Requests, and Rate all count iterations, not individual HTTP calls.
type Options struct {
	Duration time.Duration // 0 = unbounded (use Requests instead); ignored if Stages is set
	Requests uint64        // 0 = unbounded (use Duration/Stages instead)
	Rate     float64       // target iterations/sec; 0 = send as fast as Workers allow; ignored if Stages is set
	Workers  int           // max concurrent in-flight iterations ("virtual users"); ignored if Stages is set

	// MaxWorkers, if > 0, switches a flat (non-staged) Rate>0 run to open
	// model: iteration starts are paced strictly to Rate regardless of how
	// long prior iterations take, and concurrency is allowed to grow — up to
	// MaxWorkers in-flight iterations at once — instead of being capped at
	// Workers. This is what constant-arrival-rate means: the
	// offered load doesn't throttle itself just because the target is slow.
	// MaxWorkers is the hard ceiling on that growth (Go's runtime, not the
	// target's real capacity, is what would otherwise limit it) — if even
	// MaxWorkers in-flight iterations can't keep up with Rate, iterations
	// start queuing for a free slot and the run reports Saturated (see
	// report.Summary) instead of silently falling short. 0 (default) keeps
	// the existing closed-model behavior: Workers is a hard concurrency cap
	// and the achieved rate degrades toward Workers/latency under load
	// instead of growing to compensate. Ignored when Rate is 0 — see
	// Validate.
	//
	// With Stages set, MaxWorkers instead raises the ceiling on ramping
	// concurrency past whatever the stages' own Workers values reach: each
	// stage's Rate>0 iterations are still paced to that stage's target rate,
	// but (when MaxWorkers exceeds the stages' own worker ramp) are no
	// longer capped at the ramping Workers value to sustain it — this is
	// ramping-arrival-rate. 0 (default) keeps the prior staged behavior:
	// Stage.Workers is the hard concurrency cap at every point in the ramp,
	// same as flat's closed-model default. Either way, a stage with Rate==0
	// runs closed-model / ramping-vus: no rate pacing at all, iterations run
	// back-to-back as fast as that stage's ramping Workers value allows.
	MaxWorkers int

	// Stages, if non-empty, replaces Duration/Rate/Workers with a staged
	// ramp schedule (open model via Rate, closed model via Workers, or
	// both). The run lasts for the sum of all stage durations.
	Stages []Stage

	// Iterations, if > 0, bounds how many iterations a single virtual user
	// (the vuID passed to Generator.Do) runs before it departs. 0 (default)
	// means a VU runs for the entire test — the current, sustained-worker
	// behavior. When a VU departs and the overall stop condition (Duration/
	// Stages/Requests) hasn't been reached yet, its slot immediately starts
	// a fresh VU with a new vuID — so an identities pool larger than
	// Workers gets exercised by more than Workers distinct virtual users
	// over a run (e.g. to simulate session churn / periodic
	// re-authentication) instead of Workers eternal sessions. If Iterations
	// is the *only* stop signal (no Duration, Stages, or Requests), the
	// run is exactly Workers*Iterations iterations with no replenishment —
	// the classic "N virtual users, each K iterations" pattern.
	Iterations uint64

	// OnResult, if set, is invoked for every result as it completes (e.g.
	// for live progress reporting). It must return quickly.
	OnResult func(generator.Result)
}

// ResolveWorkers returns the concurrency a flat (non-staged) run will
// actually use: Workers if set, otherwise a Rate-aware default rather than a
// flat 1. Workers isn't auto-tuned once running (see the concurrency warning
// in cli.runAndReport for that) — this is only a better starting guess, on
// the assumption of up to ~1s of latency headroom; a genuinely slower target
// still needs an explicit, larger --workers.
func ResolveWorkers(o Options) int {
	if o.Workers > 0 {
		return o.Workers
	}
	if o.Rate > 0 {
		w := int(math.Ceil(o.Rate))
		if w < 10 {
			w = 10
		}
		return w
	}
	return 10
}

func (o Options) workers() int {
	return ResolveWorkers(o)
}

// Validate rejects Options that would silently misbehave rather than error:
// a negative Rate or Workers isn't a valid configuration and shouldn't be
// treated the same as "unset" without telling the caller.
func Validate(o Options) error {
	if o.Rate < 0 {
		return fmt.Errorf("rate must not be negative, got %v", o.Rate)
	}
	if o.Workers < 0 {
		return fmt.Errorf("workers must not be negative, got %d", o.Workers)
	}
	if o.MaxWorkers < 0 {
		return fmt.Errorf("max workers must not be negative, got %d", o.MaxWorkers)
	}
	if o.MaxWorkers > 0 && len(o.Stages) == 0 {
		if o.Rate <= 0 {
			return fmt.Errorf("max workers (open model) requires rate > 0")
		}
		if o.Workers > 0 && o.MaxWorkers < o.Workers {
			return fmt.Errorf("max workers (%d) must not be less than workers (%d)", o.MaxWorkers, o.Workers)
		}
	}
	for i, s := range o.Stages {
		if s.Rate < 0 {
			return fmt.Errorf("stage %d: rate must not be negative, got %v", i, s.Rate)
		}
		if s.Workers < 0 {
			return fmt.Errorf("stage %d: workers must not be negative, got %d", i, s.Workers)
		}
		if s.Duration < 0 {
			return fmt.Errorf("stage %d: duration must not be negative, got %v", i, s.Duration)
		}
	}
	return nil
}

// Run executes the load test according to opts until ctx is cancelled, the
// configured Duration/Stages schedule elapses, or Requests have been sent
// (whichever comes first), then returns the aggregated summary.
func Run(ctx context.Context, a generator.Generator, opts Options) report.Summary {
	if opts.Iterations > 0 && opts.Requests == 0 && opts.Duration == 0 && len(opts.Stages) == 0 {
		// No other stop signal: run exactly Workers*Iterations iterations,
		// with no VU replenishment (the global Requests cap below already
		// prevents any slot from starting a new VU generation once hit).
		opts.Requests = uint64(opts.workers()) * opts.Iterations
	}
	if len(opts.Stages) > 0 {
		return runStaged(ctx, a, opts)
	}
	if opts.MaxWorkers > 0 {
		return runOpenModel(ctx, a, opts)
	}
	return runFlat(ctx, a, opts)
}

// runOpenModel implements constant-arrival-rate scheduling: a rate.Limiter
// paces iteration starts to opts.Rate independent of how long each iteration
// takes, and concurrency grows on demand (a dynamic pool of "slots", each
// holding one VU's identity/iteration state) up to opts.MaxWorkers in-flight
// iterations rather than being capped at opts.Workers.
//
// A slot is a VU's persistent identity across iterations (vuID + its
// iteration count for Options.Iterations/replenishment purposes, exactly
// like a goroutine "slot" in runFlat) but is only "occupied" by a goroutine
// while an iteration using it is in flight; between iterations it sits idle
// in the slots channel. Ownership of a given slot's vuID/iteration-count
// state is exclusive at any moment — a slot is only ever read from the
// channel by one goroutine at a time, so no lock is needed around vuIDs/
// vuIters despite many goroutines running concurrently.
//
// If every slot is occupied when a rate token is minted, the next iteration
// must wait for one to free up: that's the run failing to sustain Rate at
// the configured MaxWorkers ceiling, recorded as saturation (see
// report.Summary.Saturated) rather than silently degrading the achieved
// rate the way a closed, fixed-worker model would.
func runOpenModel(ctx context.Context, a generator.Generator, opts Options) report.Summary {
	if opts.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Duration)
		defer cancel()
	}

	limiter := rate.NewLimiter(rate.Limit(opts.Rate), max(1, int(opts.Rate)))

	slots := make(chan int, opts.MaxWorkers)
	vuIDs := make([]int, opts.MaxWorkers)
	vuIters := make([]uint64, opts.MaxWorkers)
	for i := 0; i < opts.MaxWorkers; i++ {
		vuIDs[i] = i
		slots <- i
	}
	var nextVU atomic.Int64
	nextVU.Store(int64(opts.MaxWorkers))

	results := make(chan generator.Result, opts.MaxWorkers*4)
	var sent uint64
	var sendMu sync.Mutex

	var saturated atomic.Bool
	var curInFlight, peakInFlight atomic.Int64

	var inFlight sync.WaitGroup
	dispatchDone := make(chan struct{})

	go func() {
		defer close(dispatchDone)
		for {
			sendMu.Lock()
			if opts.Requests > 0 && sent >= opts.Requests {
				sendMu.Unlock()
				return
			}
			sent++
			sendMu.Unlock()

			if err := limiter.Wait(ctx); err != nil {
				return
			}

			var slot int
			select {
			case slot = <-slots:
			default:
				// Every slot is busy: Rate can't be sustained at MaxWorkers
				// concurrency right now. Still do the iteration once a slot
				// frees up (never drop offered load), just flag it.
				saturated.Store(true)
				select {
				case slot = <-slots:
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				slots <- slot
				return
			}

			if n := curInFlight.Add(1); n > peakInFlight.Load() {
				peakInFlight.Store(n)
			}
			inFlight.Add(1)
			go func(slot int) {
				defer inFlight.Done()
				defer func() {
					curInFlight.Add(-1)
					slots <- slot
				}()

				vuID := vuIDs[slot]
				for _, r := range a.Do(ctx, vuID) {
					select {
					case results <- r:
					case <-ctx.Done():
					}
				}

				if opts.Iterations > 0 {
					vuIters[slot]++
					if vuIters[slot] >= opts.Iterations {
						vuIDs[slot] = int(nextVU.Add(1)) - 1 // this VU departs; a fresh one takes the slot
						vuIters[slot] = 0
					}
				}
			}(slot)
		}
	}()

	go func() {
		<-dispatchDone
		inFlight.Wait()
		close(results)
	}()

	agg := report.NewAggregator()
	start := time.Now()
	for r := range results {
		agg.Add(r)
		if opts.OnResult != nil {
			opts.OnResult(r)
		}
	}
	elapsed := time.Since(start)

	summary := agg.Summary(elapsed)
	summary.Saturated = saturated.Load()
	summary.PeakConcurrency = int(peakInFlight.Load())
	return summary
}

func runFlat(ctx context.Context, a generator.Generator, opts Options) report.Summary {
	if opts.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Duration)
		defer cancel()
	}

	var limiter *rate.Limiter
	if opts.Rate > 0 {
		limiter = rate.NewLimiter(rate.Limit(opts.Rate), max(1, int(opts.Rate)))
	}

	results := make(chan generator.Result, opts.workers()*4)
	var sent uint64
	var sendMu sync.Mutex
	sendDone := false

	// nextVU hands out fresh, globally-unique vuIDs as VUs depart (when
	// Iterations bounds their lifetime). A shared counter — rather than each
	// slot incrementing by a fixed stride — guarantees an identities pool
	// gets fully cycled through over a run regardless of how its size
	// relates to Workers (a per-slot stride can otherwise land back on the
	// same identity%len(identities) every time).
	var nextVU atomic.Int64
	nextVU.Store(int64(opts.workers()))

	var wg sync.WaitGroup
	for i := 0; i < opts.workers(); i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			vuID := slot
			var vuIter uint64
			for {
				sendMu.Lock()
				if sendDone || (opts.Requests > 0 && sent >= opts.Requests) {
					sendMu.Unlock()
					return
				}
				sent++
				sendMu.Unlock()

				if limiter != nil {
					if err := limiter.Wait(ctx); err != nil {
						return
					}
				}
				if ctx.Err() != nil {
					return
				}

				for _, r := range a.Do(ctx, vuID) {
					select {
					case results <- r:
					case <-ctx.Done():
						return
					}
				}

				if opts.Iterations > 0 {
					vuIter++
					if vuIter >= opts.Iterations {
						vuID = int(nextVU.Add(1)) - 1 // this VU departs; a fresh one takes the slot
						vuIter = 0
					}
				}
			}
		}(i)
	}

	go func() {
		wg.Wait()
		sendMu.Lock()
		sendDone = true
		sendMu.Unlock()
		close(results)
	}()

	agg := report.NewAggregator()
	start := time.Now()
	for r := range results {
		agg.Add(r)
		if opts.OnResult != nil {
			opts.OnResult(r)
		}
	}
	elapsed := time.Since(start)

	return agg.Summary(elapsed)
}

// scheduleAt returns the target (workers, rate) at elapsed time since the
// run started, per the staged ramp schedule described on Stage.
func scheduleAt(stages []Stage, elapsed time.Duration) (int, float64) {
	var cursor time.Duration
	prevWorkers, prevRate := 0, 0.0
	for _, s := range stages {
		if s.Duration <= 0 {
			if elapsed <= cursor {
				return s.Workers, s.Rate
			}
			prevWorkers, prevRate = s.Workers, s.Rate
			continue
		}

		end := cursor + s.Duration
		if elapsed < end {
			frac := float64(elapsed-cursor) / float64(s.Duration)
			w := prevWorkers + int(math.Round(frac*float64(s.Workers-prevWorkers)))
			r := prevRate + frac*(s.Rate-prevRate)
			return w, r
		}
		cursor = end
		prevWorkers, prevRate = s.Workers, s.Rate
	}
	return prevWorkers, prevRate
}

func totalDuration(stages []Stage) time.Duration {
	var total time.Duration
	for _, s := range stages {
		total += s.Duration
	}
	return total
}

func maxWorkers(stages []Stage) int {
	m := 0
	for _, s := range stages {
		if s.Workers > m {
			m = s.Workers
		}
	}
	if m == 0 {
		m = 1
	}
	return m
}

// runStaged drives a ramp schedule (opts.Stages). Each stage independently
// behaves as closed model / ramping-vus (Rate==0: no pacing, iterations run
// back-to-back, concurrency ramps via Workers) or open model /
// ramping-arrival-rate (Rate>0: iterations paced to the ramping rate,
// concurrency ramps via Workers unless opts.MaxWorkers raises the ceiling —
// see the Options.MaxWorkers doc comment) — a schedule can mix both by
// zeroing Rate on some stages and not others (e.g. an unthrottled warm-up
// ramp followed by a rate-limited steady state).
func runStaged(ctx context.Context, a generator.Generator, opts Options) report.Summary {
	ctx, cancel := context.WithTimeout(ctx, totalDuration(opts.Stages))
	defer cancel()

	workerCap := maxWorkers(opts.Stages)
	if opts.MaxWorkers > workerCap {
		workerCap = opts.MaxWorkers
	}

	// tokens paces iterations to the current target rate. A shared
	// rate.Limiter doesn't work here: SetLimitAt only affects reservations
	// made *after* the change, so a Wait() call made while the rate is low
	// (e.g. early in a ramp) commits to a long delay and doesn't speed up
	// as the rate later rises — early low-rate waits all resolve around the
	// same later moment, producing a delayed burst instead of a smooth
	// ramp. Instead, a ticker accumulates fractional "credit" from the
	// current rate each tick and mints tokens as credit crosses 1, so the
	// rate in effect is always the one at token-mint time, not call time.
	const tick = 20 * time.Millisecond
	tokens := make(chan struct{}, workerCap)

	var desiredWorkers atomic.Int64
	var desiredRateBits atomic.Int64 // float64 bits; no atomic.Float64 in the stdlib
	scheduleStart := time.Now()
	w0, r0 := scheduleAt(opts.Stages, 0)
	desiredWorkers.Store(int64(w0))
	desiredRateBits.Store(int64(math.Float64bits(r0)))

	// saturated/peakInFlight mirror runOpenModel's saturation tracking: if
	// the ticker ever fills the tokens buffer faster than workers drain it,
	// the current stage's target rate isn't achievable at the concurrency
	// actually available right now (whether that ceiling is a ramping
	// Workers value or opts.MaxWorkers) — flagged rather than left to show
	// up only as a quieter-than-expected achieved rate.
	var saturated atomic.Bool
	var curInFlight, peakInFlight atomic.Int64

	go func() {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		var credit float64
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				w, r := scheduleAt(opts.Stages, now.Sub(scheduleStart))
				desiredWorkers.Store(int64(w))
				desiredRateBits.Store(int64(math.Float64bits(r)))

				credit += r * tick.Seconds()
				for credit >= 1 {
					select {
					case tokens <- struct{}{}:
						credit--
					default:
						credit = 0 // consumers can't keep up; drop backlog rather than burst later
						saturated.Store(true)
					}
				}
			}
		}
	}()

	results := make(chan generator.Result, workerCap*4)
	var sent uint64
	var sendMu sync.Mutex
	sendDone := false

	// See the equivalent comment in runFlat: a shared counter (rather than a
	// per-slot stride) guarantees full identities-pool coverage over a run.
	var nextVU atomic.Int64
	nextVU.Store(int64(workerCap))

	var wg sync.WaitGroup
	for i := 0; i < workerCap; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			vuID := slot
			var vuIter uint64
			for {
				w := desiredWorkers.Load()
				r := math.Float64frombits(uint64(desiredRateBits.Load()))

				// Concurrency is gated by the ramping Workers value except
				// in open model (r>0 and MaxWorkers raised the ceiling),
				// where every slot up to workerCap competes for tokens
				// instead — the rate pacing below is what actually limits
				// throughput there, not this gate.
				gated := r <= 0 || opts.MaxWorkers <= 0
				if gated && int64(slot) >= w {
					select {
					case <-time.After(50 * time.Millisecond):
						continue
					case <-ctx.Done():
						return
					}
				}

				sendMu.Lock()
				if sendDone || (opts.Requests > 0 && sent >= opts.Requests) {
					sendMu.Unlock()
					return
				}
				sent++
				sendMu.Unlock()

				if r > 0 {
					select {
					case <-tokens:
					case <-ctx.Done():
						return
					}
				}
				if ctx.Err() != nil {
					return
				}

				if n := curInFlight.Add(1); n > peakInFlight.Load() {
					peakInFlight.Store(n)
				}
				for _, res := range a.Do(ctx, vuID) {
					select {
					case results <- res:
					case <-ctx.Done():
						curInFlight.Add(-1)
						return
					}
				}
				curInFlight.Add(-1)

				if opts.Iterations > 0 {
					vuIter++
					if vuIter >= opts.Iterations {
						vuID = int(nextVU.Add(1)) - 1 // this VU departs; a fresh one takes the slot
						vuIter = 0
					}
				}
			}
		}(i)
	}

	go func() {
		wg.Wait()
		sendMu.Lock()
		sendDone = true
		sendMu.Unlock()
		close(results)
	}()

	agg := report.NewAggregator()
	start := time.Now()
	for r := range results {
		agg.Add(r)
		if opts.OnResult != nil {
			opts.OnResult(r)
		}
	}
	elapsed := time.Since(start)

	summary := agg.Summary(elapsed)
	summary.Saturated = saturated.Load()
	summary.PeakConcurrency = int(peakInFlight.Load())
	return summary
}

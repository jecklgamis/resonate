package resonate

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Suite runs several Scenarios at the same time, each with its own load
// profile — the library counterpart of a scenario file's `scenarios:` section.
// Every Scenario is built and configured on its own, exactly as for
// Scenario.Run (any protocol, any execution model); the Suite just names them,
// optionally delays one, and runs them together:
//
//	browse := resonate.NewScenario().Get(base + "/products").Workers(50).Duration(2 * time.Minute)
//	checkout := resonate.NewScenario().Step("POST", base+"/cart").Name("add").Step("POST", base+"/pay").Name("pay").
//		Rate(20).MaxWorkers(300).Duration(2 * time.Minute).
//		Assert(resonate.Assertion{Metric: "latency_p95", Step: "pay", Max: &p95}) // checked on checkout alone
//
//	summary, err := resonate.NewSuite().
//		Add("browse", browse).
//		AddAfter("checkout", 10*time.Second, checkout).
//		Run(ctx)
//
// The returned Summary combines every scenario; Summary.Scenarios holds each
// one's own, in the order they were added.
type Suite struct {
	entries    []suiteEntry
	assertions []Assertion
	defaults   []func(*Scenario) *Scenario
}

// suiteEntry is one scenario of a Suite: a ready-made Scenario (Add), or a
// function that builds one on top of the suite's defaults (AddWith).
type suiteEntry struct {
	name       string
	scenario   *Scenario
	build      func(*Scenario) *Scenario
	startAfter time.Duration
}

// NewSuite starts an empty Suite.
func NewSuite() *Suite { return &Suite{} }

// Add adds a scenario that starts when the run does. name labels it in the
// report and in scoped assertions, and must be unique within the Suite.
func (u *Suite) Add(name string, s *Scenario) *Suite {
	return u.AddAfter(name, 0, s)
}

// AddAfter is Add for a scenario that starts startAfter into the run. The wait
// is not part of the scenario's own Duration.
func (u *Suite) AddAfter(name string, startAfter time.Duration, s *Scenario) *Suite {
	u.entries = append(u.entries, suiteEntry{name: name, scenario: s, startAfter: startAfter})
	return u
}

// Defaults sets up what every scenario added with AddWith or AddAfterWith has in
// common — the library counterpart of a scenario file's shared http/ws/grpc/sse
// section. Each such scenario starts as a new Scenario, has every function given
// here applied to it in the order Defaults was called (whether before or after
// AddWith), and is then handed to its own build function, so a setting the
// scenario makes itself replaces the default:
//
//	suite := resonate.NewSuite().
//		Defaults(func(s *resonate.Scenario) *resonate.Scenario {
//			return s.BaseURL("http://localhost:8080").Timeout(5 * time.Second)
//		}).
//		AddWith("browse", func(s *resonate.Scenario) *resonate.Scenario {
//			return s.Get("/products").Workers(50).Duration(2 * time.Minute)
//		}).
//		AddWith("admin", func(s *resonate.Scenario) *resonate.Scenario {
//			return s.BaseURL("http://localhost:9090").Get("/stats").Rate(5).Duration(2 * time.Minute) // own BaseURL
//		})
//
// A default is for what applies to every scenario: BaseURL, Timeout, TLS and
// transport options, a Feeder, or even a Duration or an Assert (which then
// applies to each scenario on its own). It must not start a request —
// Get/Post/Step/Header/Body and the like describe one scenario's requests and
// belong in its build function; a default that does fails the run with an error
// naming the scenario. Defaults do not apply to a Scenario given to Add, which
// is already built. A setting that holds one value (BaseURL, Timeout, Feeder,
// Duration, ...) is replaced by the scenario's own; one that adds to a pool
// (Identity) adds to what the default put there — unlike a scenario file, where
// a list the scenario sets replaces the shared one.
func (u *Suite) Defaults(fn ...func(*Scenario) *Scenario) *Suite {
	u.defaults = append(u.defaults, fn...)
	return u
}

// AddWith is Add for a scenario built on the suite's Defaults: build receives a
// new Scenario with the defaults already applied and returns the finished one
// (usually the same one, so it can chain). The scenario is built afresh by each
// Run.
func (u *Suite) AddWith(name string, build func(*Scenario) *Scenario) *Suite {
	return u.AddAfterWith(name, 0, build)
}

// AddAfterWith is AddWith for a scenario that starts startAfter into the run.
func (u *Suite) AddAfterWith(name string, startAfter time.Duration, build func(*Scenario) *Scenario) *Suite {
	u.entries = append(u.entries, suiteEntry{name: name, build: build, startAfter: startAfter})
	return u
}

// materialize returns the entry's Scenario: the one given to Add, or a new one
// built from the defaults and the entry's build function.
func (u *Suite) materialize(e suiteEntry) (*Scenario, error) {
	if e.build == nil {
		return e.scenario, nil
	}
	s := NewScenario()
	for i, d := range u.defaults {
		if d == nil {
			return nil, fmt.Errorf("default %d is nil", i)
		}
		if s = d(s); s == nil {
			return nil, fmt.Errorf("default %d returned nil (return the *Scenario it was given)", i)
		}
		if s.hasRequests() {
			return nil, fmt.Errorf("default %d starts a request (Get/Post/Step/Header/Body/... belong in the scenario's own build function; a default may only set what every scenario shares)", i)
		}
	}
	if s = e.build(s); s == nil {
		return nil, fmt.Errorf("build function returned nil (return the *Scenario it was given)")
	}
	return s, nil
}

// Assert adds assertions about the run as a whole: unless an Assertion sets
// Scenario, it is checked against the combined results of every scenario (and
// its Step may name a step from any of them). An Assertion with Scenario set
// is checked against that scenario alone. An Assertion with Abort set must
// name a Scenario — a run-wide assertion can only be judged at the end. (An
// assertion added with a Scenario's own Assert is scoped to that Scenario
// automatically.)
func (u *Suite) Assert(assertions ...Assertion) *Suite {
	u.assertions = append(u.assertions, assertions...)
	return u
}

// Run builds every scenario (so a mistake in any of them fails before a single
// request is sent), runs them concurrently, closes their generators and
// evaluates the assertions. Like Scenario.Run it returns the Summary even when
// assertions fail or the run was aborted — an abort in any scenario stops all
// of them — alongside a non-nil error.
func (u *Suite) Run(ctx context.Context) (Summary, error) {
	if len(u.entries) == 0 {
		return Summary{}, fmt.Errorf("resonate: suite has no scenarios (call Add first)")
	}

	var gens []Generator
	defer func() {
		for _, g := range gens {
			g.Close()
		}
	}()

	names := make([]string, len(u.entries))
	specs := make([]ScenarioSpec, len(u.entries))
	var all []Assertion
	for i, e := range u.entries {
		if e.scenario == nil && e.build == nil {
			return Summary{}, fmt.Errorf("resonate: scenario %q is nil", e.name)
		}
		sc, err := u.materialize(e)
		if err != nil {
			return Summary{}, fmt.Errorf("resonate: scenario %q: %w", e.name, err)
		}
		gen, opts, err := sc.prepare()
		if err != nil {
			return Summary{}, fmt.Errorf("scenario %q: %w", e.name, err)
		}
		gens = append(gens, gen)
		names[i] = e.name
		specs[i] = ScenarioSpec{Name: e.name, Generator: gen, Options: opts, StartAfter: e.startAfter}
		for _, a := range sc.assertions {
			a.Scenario = e.name // a scenario's own assertions are about that scenario
			all = append(all, a)
		}
	}
	if err := ValidateMulti(specs); err != nil {
		return Summary{}, fmt.Errorf("resonate: %w", err)
	}

	for _, a := range u.assertions {
		if a.Abort {
			i := slices.Index(names, a.Scenario)
			if a.Scenario == "" || i < 0 {
				return Summary{}, fmt.Errorf("resonate: assertion (%s): Abort needs a Scenario that names one of the suite's scenarios (a run-wide assertion can only be checked at the end)", a.Metric)
			}
			a.Scenario = "" // the scenario's own engine run sees it unscoped
			specs[i].Options.AbortOn = append(specs[i].Options.AbortOn, a)
			if err := ValidateAbort(specs[i].Options.AbortOn); err != nil {
				return Summary{}, fmt.Errorf("resonate: scenario %q: %w", names[i], err)
			}
		}
	}
	all = append(all, u.assertions...)
	if err := ValidateScenarios(all, names); err != nil {
		return Summary{}, fmt.Errorf("resonate: %w", err)
	}
	if err := ValidateScopedSteps(all, StepLookup(specs)); err != nil {
		return Summary{}, fmt.Errorf("resonate: %w", err)
	}

	summary, err := RunMulti(ctx, specs, MultiOptions{})
	if err != nil {
		return Summary{}, fmt.Errorf("resonate: %w", err)
	}
	if summary.Aborted != "" {
		return summary, fmt.Errorf("resonate: run aborted: %s", summary.Aborted)
	}
	return summary, evaluateAssertions(summary, all)
}

package generator

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jecklgamis/resonate/internal/tmpl"
)

// FlowStep is one entry in an ordered flow: either a request, or a
// control-flow block wrapping nested Steps — never both. A request step
// sets Method/URL (and optionally the other request fields); a
// control-flow step sets exactly one of Repeat/During/If plus Steps, and
// leaves Method/URL unset.
//
// Extract pulls values out of a request step's response into named vars
// (available to later steps in the same flow, or — for setup steps — to
// every iteration that virtual user runs afterward), keyed by variable
// name and valued by an extraction rule (see extractValue).
type FlowStep struct {
	Method string
	URL    string
	Query  map[string]string
	Header map[string]string
	Body   string

	// RawBody, if non-nil, is sent as the request body exactly as given,
	// bypassing templating entirely — see HTTPTarget.RawBody. Mutually
	// exclusive with Body.
	RawBody []byte

	Extract       map[string]string
	ExpectStatus  []int
	ExpectHeaders map[string]string
	ExpectBody    map[string]string

	// Pause, if > 0, sleeps this long immediately before this step's
	// request — think-time between steps. If PauseMax is
	// also set (and greater than Pause), the sleep is a random duration
	// drawn uniformly from [Pause, PauseMax) instead of the fixed Pause.
	Pause    time.Duration
	PauseMax time.Duration

	// Repeat, if > 0, makes this a control-flow step: Steps runs this many
	// times in a row before the flow continues past this entry. Mutually
	// exclusive with During/If.
	Repeat int

	// During, if > 0, makes this a control-flow step: Steps runs
	// repeatedly for this long (checked between iterations, not
	// mid-iteration) before the flow continues past this entry. Mutually
	// exclusive with Repeat/If.
	During time.Duration

	// If, if non-empty, makes this a control-flow step: a template
	// condition (e.g. "{{eq .Vars.status \"ready\"}}") re-rendered every
	// time this entry is reached; Steps runs only if it renders "true" or
	// "1". Mutually exclusive with Repeat/During.
	If string

	// Steps holds the nested steps for a Repeat/During/If control-flow
	// entry; unused (must be empty) on a request step.
	Steps []FlowStep
}

// FlowOptions configures a FlowGenerator.
type FlowOptions struct {
	HTTP HTTPOptions

	// Identities, if non-empty, is a pool of named values (e.g. credentials
	// or account IDs). Each virtual user (vuID) is assigned exactly one
	// identity, round-robin over the pool, sticky for that VU's lifetime,
	// and exposed to templates as {{.Identity.<key>}}.
	Identities []map[string]string
}

// FlowGenerator runs an ordered sequence of HTTP steps per iteration,
// threading extracted values from one step's response into later steps'
// templates ({{.Vars.<name>}}). An optional Setup sequence runs once per
// virtual user before its first iteration, seeding that VU's Vars for every
// iteration it runs afterward (e.g. logging in once and reusing a token) —
// if engine.Options.Iterations bounds VU lifetimes, a fresh VU (new vuID)
// re-runs Setup independently when it takes over a slot. A failed request
// step (or an exhausted context) stops the rest of the flow/setup sequence,
// including unwinding out of any enclosing Repeat/During/If block.
type FlowGenerator struct {
	setup      []compiledStep
	flow       []compiledStep
	client     *http.Client
	maxBody    int64
	identities []map[string]string
	feeder     *Feeder
	seq        uint64

	states sync.Map // int(vuID) -> *vuState
}

type controlKind int

const (
	controlNone controlKind = iota
	controlRepeat
	controlDuring
	controlIf
)

// compiledStep is either a request (control == controlNone) or a
// control-flow block (repeat/during/ifCond set accordingly, nested holds
// the compiled Steps) — see FlowStep.
type compiledStep struct {
	httpFields
	extract  map[string]string
	pause    time.Duration
	pauseMax time.Duration

	control controlKind
	repeat  int
	during  time.Duration
	ifCond  tmpl.Field
	nested  []compiledStep
}

type vuState struct {
	once sync.Once
	vars map[string]string
}

func NewFlowGenerator(flow, setup []FlowStep, opts FlowOptions) (*FlowGenerator, error) {
	if len(flow) == 0 {
		return nil, fmt.Errorf("flow must have at least one step")
	}

	compiledFlow, err := compileSteps(flow, opts.HTTP.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("flow: %w", err)
	}
	compiledSetup, err := compileSteps(setup, opts.HTTP.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}

	client, err := newHTTPClient(opts.HTTP)
	if err != nil {
		return nil, err
	}
	return &FlowGenerator{
		setup:      compiledSetup,
		flow:       compiledFlow,
		client:     client,
		maxBody:    opts.HTTP.MaxResponseBody,
		identities: opts.Identities,
		feeder:     opts.HTTP.Feeder,
	}, nil
}

func compileSteps(steps []FlowStep, baseURL string) ([]compiledStep, error) {
	compiled := make([]compiledStep, len(steps))
	for i, s := range steps {
		c, err := compileStep(s, baseURL)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		compiled[i] = c
	}
	return compiled, nil
}

func compileStep(s FlowStep, baseURL string) (compiledStep, error) {
	set := 0
	if s.Repeat > 0 {
		set++
	}
	if s.During > 0 {
		set++
	}
	if s.If != "" {
		set++
	}
	if set > 1 {
		return compiledStep{}, fmt.Errorf("repeat/during/if are mutually exclusive on one step")
	}

	if set == 1 {
		if s.Method != "" || s.URL != "" {
			return compiledStep{}, fmt.Errorf("a repeat/during/if step can't also set method/url")
		}
		if len(s.Steps) == 0 {
			return compiledStep{}, fmt.Errorf("repeat/during/if requires at least one nested step")
		}
		nested, err := compileSteps(s.Steps, baseURL)
		if err != nil {
			return compiledStep{}, err
		}
		switch {
		case s.Repeat > 0:
			return compiledStep{control: controlRepeat, repeat: s.Repeat, nested: nested}, nil
		case s.During > 0:
			return compiledStep{control: controlDuring, during: s.During, nested: nested}, nil
		default:
			cond, err := tmpl.Compile("if", s.If)
			if err != nil {
				return compiledStep{}, err
			}
			return compiledStep{control: controlIf, ifCond: cond, nested: nested}, nil
		}
	}

	if len(s.Steps) > 0 {
		return compiledStep{}, fmt.Errorf("steps is only valid on a repeat/during/if step")
	}
	if s.PauseMax > 0 && s.PauseMax < s.Pause {
		return compiledStep{}, fmt.Errorf("pause_max must be >= pause")
	}
	f, err := compileHTTPFields(s.Method, s.URL, s.Query, s.Header, s.Body, s.RawBody, baseURL, s.ExpectStatus, s.ExpectHeaders, s.ExpectBody)
	if err != nil {
		return compiledStep{}, err
	}
	return compiledStep{httpFields: f, extract: s.Extract, pause: s.Pause, pauseMax: s.PauseMax}, nil
}

func (a *FlowGenerator) Protocol() string { return "http" }

func (a *FlowGenerator) Close() error {
	a.client.CloseIdleConnections()
	a.feeder.Close()
	return nil
}

func (a *FlowGenerator) identityFor(vuID int) map[string]string {
	if len(a.identities) == 0 {
		return nil
	}
	return a.identities[vuID%len(a.identities)]
}

// setupVarsFor runs this worker's setup steps exactly once, caching the
// resulting vars, and returns a fresh copy for the caller to mutate freely
// over the course of one flow iteration.
func (a *FlowGenerator) setupVarsFor(ctx context.Context, vuID int, identity map[string]string) map[string]string {
	v, _ := a.states.LoadOrStore(vuID, &vuState{})
	st := v.(*vuState)

	st.once.Do(func() {
		vars := map[string]string{}
		data := tmpl.Data{VU: vuID, Identity: identity, Vars: vars, Feeder: a.feeder.Next()}
		var results []Result
		runSteps(ctx, a.client, a.setup, data, vars, a.maxBody, &results)
		st.vars = vars
	})

	cp := make(map[string]string, len(st.vars))
	for k, v := range st.vars {
		cp[k] = v
	}
	return cp
}

func (a *FlowGenerator) Do(ctx context.Context, vuID int) []Result {
	identity := a.identityFor(vuID)
	vars := a.setupVarsFor(ctx, vuID, identity)
	seq := atomic.AddUint64(&a.seq, 1) - 1

	data := tmpl.Data{Seq: seq, VU: vuID, Identity: identity, Vars: vars, Feeder: a.feeder.Next()}
	results := make([]Result, 0, len(a.flow))
	runSteps(ctx, a.client, a.flow, data, vars, a.maxBody, &results)
	return results
}

// runSteps runs steps in order, recursing into Repeat/During/If blocks.
// vars is mutated in place (data.Vars aliases the same map, so extracted
// values are visible to later steps and re-evaluated If conditions without
// rebuilding data). It returns false as soon as a request step fails or
// ctx is done, unwinding out of every enclosing block so the whole flow
// stops rather than just the current block.
func runSteps(ctx context.Context, client *http.Client, steps []compiledStep, data tmpl.Data, vars map[string]string, maxBody int64, results *[]Result) bool {
	for _, step := range steps {
		if ctx.Err() != nil {
			return false
		}

		switch step.control {
		case controlRepeat:
			for i := 0; i < step.repeat; i++ {
				if ctx.Err() != nil {
					return false
				}
				if !runSteps(ctx, client, step.nested, data, vars, maxBody, results) {
					return false
				}
			}
		case controlDuring:
			deadline := time.Now().Add(step.during)
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					return false
				}
				if !runSteps(ctx, client, step.nested, data, vars, maxBody, results) {
					return false
				}
			}
		case controlIf:
			cond, err := step.ifCond.Render(data)
			if err == nil && isTruthy(cond) {
				if !runSteps(ctx, client, step.nested, data, vars, maxBody, results) {
					return false
				}
			}
		default:
			if step.pause > 0 || step.pauseMax > 0 {
				if !sleepCtx(ctx, pauseDuration(step.pause, step.pauseMax)) {
					return false
				}
			}
			result, extracted := doHTTP(ctx, client, step.httpFields, data, step.extract, maxBody)
			*results = append(*results, result)
			if result.Error != nil || !result.Success() {
				return false
			}
			for k, v := range extracted {
				vars[k] = v
			}
		}
	}
	return true
}

func isTruthy(s string) bool {
	return s == "true" || s == "1"
}

// pauseDuration returns min, or — if max is set and greater than min — a
// random duration drawn uniformly from [min, max).
func pauseDuration(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int64N(int64(max-min)))
}

// sleepCtx sleeps for d, returning false early (without completing the
// sleep) if ctx is done first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

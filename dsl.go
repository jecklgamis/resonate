package resonate

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Scenario is a fluent builder over HTTPTarget(s)/FlowStep(s) + Options,
// for callers who'd rather chain calls than build struct literals
// directly. It's a thin convenience layer on top of
// NewHTTPGenerator/NewFlowGenerator/Run — nothing here does anything you
// couldn't do with the struct-literal API too.
//
// By default (Method/Get/Post/... called with no preceding Target/Step/
// Setup call) a Scenario builds a single-target HTTPGenerator — the
// common case. Target starts an additional independent target (round-robin
// HTTPGenerator, like the "targets:" list in a scenario YAML file); Step/
// Setup switch to a multi-step FlowGenerator (ordered steps with response
// chaining via Extract, like a scenario YAML file's "flow:"/"setup:").
// Target and Step/Setup are mutually exclusive within one Scenario.
type Scenario struct {
	targets    []HTTPTarget
	flow       []FlowStep
	setup      []FlowStep
	identities []map[string]string
	httpOpts   HTTPOptions
	opts       Options

	cur      *stepRef
	active   *[]FlowStep   // where Step/Setup/Repeat/During/If append next; nil outside flow-building mode
	stack    []*[]FlowStep // enclosing active lists, popped by End
	buildErr error         // set by FileBody/RawFileBody on a read failure, surfaced by Build/Run
}

// stepRef lets Header/Query/Body/Expect*/Extract/Pause mutate "whichever
// request is currently being built" — the last HTTPTarget or FlowStep
// appended — without duplicating those setters once per underlying type.
type stepRef struct {
	method        *string
	url           *string
	query         *map[string]string
	header        *map[string]string
	body          *string
	rawBody       *[]byte
	expectStatus  *[]int
	expectHeaders *map[string]string
	expectBody    *map[string]string
	extract       *map[string]string // nil for an HTTPTarget-backed ref (Target/implicit single target)
	pause         *time.Duration     // nil for an HTTPTarget-backed ref
	pauseMax      *time.Duration     // nil for an HTTPTarget-backed ref
}

func targetRef(t *HTTPTarget) *stepRef {
	return &stepRef{
		method: &t.Method, url: &t.URL,
		query: &t.Query, header: &t.Header, body: &t.Body, rawBody: &t.RawBody,
		expectStatus: &t.ExpectStatus, expectHeaders: &t.ExpectHeaders, expectBody: &t.ExpectBody,
	}
}

func flowStepRef(fs *FlowStep) *stepRef {
	return &stepRef{
		method: &fs.Method, url: &fs.URL,
		query: &fs.Query, header: &fs.Header, body: &fs.Body, rawBody: &fs.RawBody,
		expectStatus: &fs.ExpectStatus, expectHeaders: &fs.ExpectHeaders, expectBody: &fs.ExpectBody,
		extract: &fs.Extract, pause: &fs.Pause, pauseMax: &fs.PauseMax,
	}
}

// NewScenario starts a new Scenario with resonate's default HTTP options.
func NewScenario() *Scenario {
	return &Scenario{httpOpts: DefaultHTTPOptions()}
}

// ensureCurrent lazily starts a step on first use, so Method/Get/Post/
// Header/... work with no preceding Target/Step/Setup call — the common
// single-request case. If a flow (Step/Setup/Repeat/During/If) is already
// in progress, the new step joins whichever list is currently active
// (top-level flow/setup, or the nested Steps of an open Repeat/During/If
// block); otherwise it starts the single implicit HTTP target.
func (s *Scenario) ensureCurrent() {
	if s.cur != nil {
		return
	}
	if s.active != nil {
		*s.active = append(*s.active, FlowStep{})
		s.cur = flowStepRef(&(*s.active)[len(*s.active)-1])
		return
	}
	s.targets = append(s.targets, HTTPTarget{})
	s.cur = targetRef(&s.targets[len(s.targets)-1])
}

// Target starts an additional independent request target: an HTTPGenerator
// built from a Scenario with more than one Target cycles through them
// round-robin, one per iteration (the same as a scenario YAML file's
// "targets:" list). Subsequent Header/Query/Body/Expect* calls apply to
// this target until the next Target/Step/Setup call.
func (s *Scenario) Target(method, url string) *Scenario {
	s.targets = append(s.targets, HTTPTarget{})
	s.cur = targetRef(&s.targets[len(s.targets)-1])
	return s.Method(method, url)
}

// Step appends a request to the scenario's multi-step flow: once any Step
// (or Setup) call is made, the Scenario builds a FlowGenerator instead of
// an HTTPGenerator, running every step in order per iteration. Subsequent
// Header/Query/Body/Expect*/Extract/Pause calls apply to this step until
// the next Target/Step/Setup/Repeat/During/If call. Inside an open
// Repeat/During/If block (see those methods and End), Step appends to
// that block's nested steps instead of the top-level flow.
func (s *Scenario) Step(method, url string) *Scenario {
	if len(s.stack) == 0 {
		s.active = &s.flow
	}
	*s.active = append(*s.active, FlowStep{})
	s.cur = flowStepRef(&(*s.active)[len(*s.active)-1])
	return s.Method(method, url)
}

// Setup appends a request to the scenario's setup sequence, run once per
// virtual user before its first flow iteration (e.g. logging in once and
// reusing an extracted token for every iteration that VU runs
// afterward) — like Step, this switches the Scenario to build a
// FlowGenerator. Subsequent Header/Query/Body/Expect*/Extract/Pause calls
// apply to this step until the next Target/Step/Setup/Repeat/During/If
// call. Only meaningful at the top level, not inside a Repeat/During/If
// block.
func (s *Scenario) Setup(method, url string) *Scenario {
	if len(s.stack) == 0 {
		s.active = &s.setup
	}
	*s.active = append(*s.active, FlowStep{})
	s.cur = flowStepRef(&(*s.active)[len(*s.active)-1])
	return s.Method(method, url)
}

// Repeat opens a control-flow block: every Step (or nested Repeat/During/
// If) added before the matching End runs n times in a row when the flow
// reaches this point. Only meaningful alongside Step/Setup (a FlowStep
// scenario); mutually exclusive with During/If at the same nesting level.
func (s *Scenario) Repeat(n int) *Scenario {
	return s.openBlock(FlowStep{Repeat: n})
}

// During opens a control-flow block: every Step (or nested Repeat/During/
// If) added before the matching End repeats for d when the flow reaches
// this point (checked between iterations, not mid-iteration). Only
// meaningful alongside Step/Setup; mutually exclusive with Repeat/If at the
// same nesting level.
func (s *Scenario) During(d time.Duration) *Scenario {
	return s.openBlock(FlowStep{During: d})
}

// If opens a control-flow block: every Step (or nested Repeat/During/If)
// added before the matching End runs only if cond — a template condition
// re-rendered every time the flow reaches this point, with {{.Vars.*}}
// available — renders "true" or "1". Only meaningful alongside Step/Setup;
// mutually exclusive with Repeat/During at the same nesting level.
func (s *Scenario) If(cond string) *Scenario {
	return s.openBlock(FlowStep{If: cond})
}

func (s *Scenario) openBlock(block FlowStep) *Scenario {
	if s.active == nil {
		s.active = &s.flow
	}
	parent := s.active
	*parent = append(*parent, block)
	newBlock := &(*parent)[len(*parent)-1]
	s.stack = append(s.stack, parent)
	s.active = &newBlock.Steps
	s.cur = nil // a control-flow step isn't a request; Header/etc. shouldn't target it
	return s
}

// End closes the Repeat/During/If block most recently opened, returning
// subsequent Step/Setup/Repeat/During/If calls to the enclosing list. A
// no-op if no block is currently open.
func (s *Scenario) End() *Scenario {
	if len(s.stack) == 0 {
		return s
	}
	s.active = s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	s.cur = nil
	return s
}

// Identity adds one identity to the pool used by flow steps: each virtual
// user is assigned one identity round-robin, sticky for its lifetime, and
// exposed to templates as {{.Identity.<key>}}. Only meaningful alongside
// Step/Setup.
func (s *Scenario) Identity(kv map[string]string) *Scenario {
	s.identities = append(s.identities, kv)
	return s
}

func (s *Scenario) Method(method, url string) *Scenario {
	s.ensureCurrent()
	*s.cur.method = method
	*s.cur.url = url
	return s
}

func (s *Scenario) Get(url string) *Scenario    { return s.Method("GET", url) }
func (s *Scenario) Post(url string) *Scenario   { return s.Method("POST", url) }
func (s *Scenario) Put(url string) *Scenario    { return s.Method("PUT", url) }
func (s *Scenario) Patch(url string) *Scenario  { return s.Method("PATCH", url) }
func (s *Scenario) Delete(url string) *Scenario { return s.Method("DELETE", url) }

func (s *Scenario) Header(key, value string) *Scenario {
	s.ensureCurrent()
	if *s.cur.header == nil {
		*s.cur.header = map[string]string{}
	}
	(*s.cur.header)[key] = value
	return s
}

func (s *Scenario) Query(key, value string) *Scenario {
	s.ensureCurrent()
	if *s.cur.query == nil {
		*s.cur.query = map[string]string{}
	}
	(*s.cur.query)[key] = value
	return s
}

func (s *Scenario) Body(body string) *Scenario {
	s.ensureCurrent()
	*s.cur.body = body
	return s
}

// JSONBody sets Body and a Content-Type: application/json header.
func (s *Scenario) JSONBody(body string) *Scenario {
	return s.Header("Content-Type", "application/json").Body(body)
}

// FileBody reads path's contents once (immediately) and sets it as Body —
// so, like Body, it's still re-rendered as a template on every request
// (e.g. a request/order template stored in its own file). Any read error
// is deferred and returned from Build/Run, so the fluent chain doesn't
// need to change shape for a call that can fail. For a large or binary
// file that shouldn't be templated at all, use RawFileBody instead.
func (s *Scenario) FileBody(path string) *Scenario {
	data, err := os.ReadFile(path)
	if err != nil {
		s.deferErr(fmt.Errorf("resonate: FileBody(%q): %w", path, err))
		return s
	}
	return s.Body(string(data))
}

// RawBody sets the request body to data exactly as given, bypassing
// templating entirely — see HTTPTarget.RawBody. Mutually exclusive with
// Body/JSONBody/FileBody on the same request (a Build/Run error if both
// are set). Only meaningful on a Target/Step/Setup that sends a body; a
// no-op if ensureCurrent has nothing to attach it to.
func (s *Scenario) RawBody(data []byte) *Scenario {
	s.ensureCurrent()
	if s.cur.rawBody == nil {
		return s
	}
	*s.cur.rawBody = data
	return s
}

// RawFileBody reads path's contents once (immediately) and sets it as
// RawBody — sent exactly as-is on every request, never templated. For a
// large or binary payload where per-request re-rendering would be
// wasteful, or where the raw bytes might otherwise be misread as
// containing "{{ }}" template syntax. Any read error is deferred and
// returned from Build/Run.
func (s *Scenario) RawFileBody(path string) *Scenario {
	data, err := os.ReadFile(path)
	if err != nil {
		s.deferErr(fmt.Errorf("resonate: RawFileBody(%q): %w", path, err))
		return s
	}
	return s.RawBody(data)
}

// deferErr records the first error passed to it, surfaced by Build/Run —
// used by builder methods (FileBody, RawFileBody) that can fail but must
// stay chainable.
func (s *Scenario) deferErr(err error) {
	if s.buildErr == nil {
		s.buildErr = err
	}
}

func (s *Scenario) ExpectStatus(codes ...int) *Scenario {
	s.ensureCurrent()
	*s.cur.expectStatus = codes
	return s
}

func (s *Scenario) ExpectHeader(key, value string) *Scenario {
	s.ensureCurrent()
	if *s.cur.expectHeaders == nil {
		*s.cur.expectHeaders = map[string]string{}
	}
	(*s.cur.expectHeaders)[key] = value
	return s
}

// ExpectBody adds a check for the given extract-style rule ("json:<path>",
// "yaml:<path>", "xml:<path>"), keyed by rule and valued by the expected
// result ("" to only require the rule evaluates without error).
func (s *Scenario) ExpectBody(rule, value string) *Scenario {
	s.ensureCurrent()
	if *s.cur.expectBody == nil {
		*s.cur.expectBody = map[string]string{}
	}
	(*s.cur.expectBody)[rule] = value
	return s
}

// Extract pulls a value out of the current step's response into a named
// variable — available to later flow steps (or, from a Setup step, every
// iteration that virtual user runs afterward) via {{.Vars.<name>}}. Only
// meaningful after Step/Setup; a no-op on an independent Target (which has
// nothing to chain into).
func (s *Scenario) Extract(name, rule string) *Scenario {
	s.ensureCurrent()
	if s.cur.extract == nil {
		return s
	}
	if *s.cur.extract == nil {
		*s.cur.extract = map[string]string{}
	}
	(*s.cur.extract)[name] = rule
	return s
}

// Pause sleeps for d immediately before the current step's request —
// think-time. Only meaningful after Step/Setup; a no-op on
// an independent Target (which has no notion of "before this request" in
// a sequence).
func (s *Scenario) Pause(d time.Duration) *Scenario {
	s.ensureCurrent()
	if s.cur.pause == nil {
		return s
	}
	*s.cur.pause = d
	return s
}

// PauseRange is like Pause, but sleeps a random duration drawn uniformly
// from [min, max) instead of a fixed duration.
func (s *Scenario) PauseRange(min, max time.Duration) *Scenario {
	s.ensureCurrent()
	if s.cur.pause == nil {
		return s
	}
	*s.cur.pause = min
	*s.cur.pauseMax = max
	return s
}

// BaseURL, if set, is prepended to any target/step URL that starts with
// "/", so a Scenario hitting one host can use relative
// paths instead of repeating "http://host:port" on every Target/Step/
// Setup call. A URL that doesn't start with "/" (already absolute, or a
// "{{...}}" template expression) is left untouched.
func (s *Scenario) BaseURL(url string) *Scenario {
	s.httpOpts.BaseURL = url
	return s
}

func (s *Scenario) Timeout(d time.Duration) *Scenario {
	s.httpOpts.Timeout = d
	return s
}

func (s *Scenario) Insecure() *Scenario {
	s.httpOpts.Insecure = true
	return s
}

func (s *Scenario) Rate(reqPerSec float64) *Scenario {
	s.opts.Rate = reqPerSec
	return s
}

func (s *Scenario) Workers(n int) *Scenario {
	s.opts.Workers = n
	return s
}

func (s *Scenario) MaxWorkers(n int) *Scenario {
	s.opts.MaxWorkers = n
	return s
}

func (s *Scenario) Duration(d time.Duration) *Scenario {
	s.opts.Duration = d
	return s
}

func (s *Scenario) Requests(n uint64) *Scenario {
	s.opts.Requests = n
	return s
}

// Stages replaces flat Duration/Rate/Workers with a staged ramp schedule
// (ramping-vus if a Stage's Rate is 0, ramping-arrival-rate otherwise) —
// see Stage and the README's "Ramp profiles" section. Works the same way
// regardless of whether the Scenario builds an HTTPGenerator (Target) or a
// FlowGenerator (Step/Setup) — Stages only shapes Options, which both
// share.
func (s *Scenario) Stages(stages ...Stage) *Scenario {
	s.opts.Stages = stages
	return s
}

// Iterations bounds how many iterations a single virtual user runs before
// it departs and a fresh one takes its place — see the README's "Virtual
// user lifecycle" section.
func (s *Scenario) Iterations(n uint64) *Scenario {
	s.opts.Iterations = n
	return s
}

// Build compiles the accumulated target(s)/step(s)/options into a
// Generator, without running it — for callers who want the Generator
// itself (e.g. to pass to their own retry/orchestration logic) rather than
// calling Run directly. Returns an *HTTPGenerator if the Scenario only
// used Target (or the default single implicit target), or an
// *FlowGenerator if it used Step/Setup.
func (s *Scenario) Build() (Generator, error) {
	if s.buildErr != nil {
		return nil, s.buildErr
	}
	isFlow := len(s.flow) > 0 || len(s.setup) > 0
	if isFlow && len(s.targets) > 0 {
		return nil, fmt.Errorf("resonate: scenario mixes Target (or an implicit Get/Post/... target) with Step/Setup — use one or the other, not both")
	}
	if isFlow {
		return NewFlowGenerator(s.flow, s.setup, FlowOptions{HTTP: s.httpOpts, Identities: s.identities})
	}
	if len(s.targets) == 0 {
		return nil, fmt.Errorf("resonate: scenario has no request configured (call Get/Post/Target/Step/Setup first)")
	}
	return NewHTTPGenerator(s.targets, s.httpOpts)
}

// Run builds the Scenario and runs it via Run, closing the Generator
// afterward.
func (s *Scenario) Run(ctx context.Context) (Summary, error) {
	gen, err := s.Build()
	if err != nil {
		return Summary{}, err
	}
	defer gen.Close()
	return Run(ctx, gen, s.opts), nil
}

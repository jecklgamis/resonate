package resonate

import (
	"github.com/jecklgamis/resonate/internal/generator"
)

// Result captures the outcome of a single request/operation.
type Result = generator.Result

// Generator performs one unit of work ("iteration") and reports the result
// of each HTTP call it made. See the three implementations below
// (HTTPGenerator, FlowGenerator, WSGenerator) and their constructors.
type Generator = generator.Generator

// --- HTTP: independent, stateless requests ---

// HTTPTarget describes a single HTTP request template. Any field may embed
// {{ }} template expressions (see "Request templating" in the README).
type HTTPTarget = generator.HTTPTarget

// HTTPOptions configures the transport shared by all requests.
type HTTPOptions = generator.HTTPOptions

// HTTPGenerator issues independent HTTP requests, cycling through a set of
// targets round-robin. For multi-step flows with response chaining, use
// FlowGenerator instead.
type HTTPGenerator = generator.HTTPGenerator

// DefaultHTTPOptions returns HTTPOptions with resonate's usual defaults
// (30s timeout, redirects followed).
func DefaultHTTPOptions() HTTPOptions { return generator.DefaultHTTPOptions() }

// NewHTTPGenerator builds a Generator that cycles through targets
// round-robin, one per iteration.
func NewHTTPGenerator(targets []HTTPTarget, opts HTTPOptions) (*HTTPGenerator, error) {
	return generator.NewHTTPGenerator(targets, opts)
}

// --- Flow: an ordered sequence of steps with response chaining ---

// FlowStep is one request in an ordered flow. Extract pulls values out of
// the response into named vars, available to later steps via
// {{.Vars.<name>}}.
type FlowStep = generator.FlowStep

// FlowOptions configures a FlowGenerator.
type FlowOptions = generator.FlowOptions

// FlowGenerator runs an ordered sequence of HTTP steps per iteration,
// threading extracted values from one step's response into later steps'
// templates.
type FlowGenerator = generator.FlowGenerator

// NewFlowGenerator builds a Generator that runs flow (and, once per
// virtual user, setup) as an ordered sequence of steps per iteration.
func NewFlowGenerator(flow, setup []FlowStep, opts FlowOptions) (*FlowGenerator, error) {
	return generator.NewFlowGenerator(flow, setup, opts)
}

// --- WebSocket: connect, exchange a message sequence, close ---

// WSMessage is one send — and optionally a matching receive — within a
// WebSocket connection's message sequence.
type WSMessage = generator.WSMessage

// WSTarget describes one WebSocket connection's lifecycle: dial, send the
// message sequence in order, close.
type WSTarget = generator.WSTarget

// WSOptions configures dialing shared by all connections.
type WSOptions = generator.WSOptions

// WSGenerator dials a fresh WebSocket connection every iteration, exchanges
// its configured message sequence, and closes it.
type WSGenerator = generator.WSGenerator

// DefaultWSOptions returns WSOptions with resonate's usual defaults (30s
// dial/read/write timeout).
func DefaultWSOptions() WSOptions { return generator.DefaultWSOptions() }

// NewWSGenerator builds a Generator that dials target fresh every
// iteration and exchanges its message sequence.
func NewWSGenerator(target WSTarget, opts WSOptions) (*WSGenerator, error) {
	return generator.NewWSGenerator(target, opts)
}

// --- Feeders: CSV/JSON-driven request data ---

// Feeder cycles or randomly samples rows of string-keyed data loaded once
// from a CSV or JSON file, exposed to templates as {{.Feeder.<column>}}.
type Feeder = generator.Feeder

// NewFeeder loads path (.csv or .json) into memory. mode is "sequential"
// (default: round-robin) or "random".
func NewFeeder(path, mode string) (*Feeder, error) {
	return generator.NewFeeder(path, mode)
}

package resonate

import (
	"github.com/jecklgamis/resonate/internal/generator"
)

// Result captures the outcome of a single request/operation.
type Result = generator.Result

// Generator performs one unit of work ("iteration") and reports the result
// of each call it made. See the implementations below (HTTPGenerator,
// FlowGenerator, WSGenerator, GRPCGenerator, SSEGenerator) and their
// constructors.
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

// --- gRPC: unary and streaming calls; schema from reflection, .proto files or a descriptor set ---

// GRPCCall is one RPC: a "package.Service/Method" name, its request(s) as
// protobuf JSON (templated) — Message for unary and server-streaming methods,
// Messages for client-streaming and bidirectional ones — and optional
// Receive/Duration bounds, metadata, extraction and checks. The method's
// schema decides which kind it is.
type GRPCCall = generator.GRPCCall

// GRPCTarget describes one iteration against one server: an address plus the
// calls made in order over a shared connection.
type GRPCTarget = generator.GRPCTarget

// GRPCOptions configures connections (TLS or plaintext, how many) and where
// the schema comes from (ProtoFiles, DescriptorSet, or server reflection).
type GRPCOptions = generator.GRPCOptions

// GRPCGenerator makes gRPC calls, reporting one Result per unary call and one
// per received message of a streaming call.
type GRPCGenerator = generator.GRPCGenerator

// DefaultGRPCOptions returns GRPCOptions with resonate's usual defaults (30s
// per-call timeout, one connection, TLS on).
func DefaultGRPCOptions() GRPCOptions { return generator.DefaultGRPCOptions() }

// NewGRPCGenerator builds a Generator that makes target's calls, in order,
// every iteration. It resolves the schema (which may mean a reflection round
// trip to the server) before returning.
func NewGRPCGenerator(target GRPCTarget, opts GRPCOptions) (*GRPCGenerator, error) {
	return generator.NewGRPCGenerator(target, opts)
}

// --- SSE: open a Server-Sent Events stream, read events, close ---

// SSETarget describes one Server-Sent Events stream: send a request, read the
// text/event-stream response until Events events arrive and/or Duration
// elapses, close. See the field docs for the exact stop and failure rules.
type SSETarget = generator.SSETarget

// SSEOptions configures the transport (via its HTTP field, so H2C,
// Insecure, etc. apply) and per-virtual-user identities.
type SSEOptions = generator.SSEOptions

// SSEGenerator opens a fresh stream every iteration and reports one Result
// for the connect plus one per event received.
type SSEGenerator = generator.SSEGenerator

// DefaultSSEOptions returns SSEOptions with resonate's usual defaults (30s
// connect timeout, redirects followed).
func DefaultSSEOptions() SSEOptions { return generator.DefaultSSEOptions() }

// NewSSEGenerator builds a Generator that opens target fresh every iteration.
func NewSSEGenerator(target SSETarget, opts SSEOptions) (*SSEGenerator, error) {
	return generator.NewSSEGenerator(target, opts)
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

// KafkaFeederOptions configures NewKafkaFeeder.
type KafkaFeederOptions = generator.KafkaFeederOptions

// NewKafkaFeeder returns a Feeder whose rows are the messages of a Kafka
// topic (a JSON object's fields, plus _key, _value, _partition, _offset,
// _timestamp and _header_<name>). The topic is read directly — no consumer
// group, no committed offsets — and by default wraps around to the start
// when it runs out. Close it when done.
func NewKafkaFeeder(opts KafkaFeederOptions) (*Feeder, error) {
	return generator.NewKafkaFeeder(opts)
}

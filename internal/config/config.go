// Package config defines the YAML scenario file format and loads it.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is the top-level shape of a resonate config file.
type Scenario struct {
	Protocol   string            `yaml:"protocol"` // "http", "ws", "grpc" or "sse"
	Load       LoadConfig        `yaml:"load"`
	HTTP       HTTPConfig        `yaml:"http"`
	WS         WSConfig          `yaml:"ws"`
	GRPC       GRPCConfig        `yaml:"grpc"`
	SSE        SSEConfig         `yaml:"sse"`
	Assertions []AssertionConfig `yaml:"assertions"`
}

// AssertionConfig checks one metric of the completed run's report against
// one or more conditions. At least one
// condition field should be set; several may be, combined with AND (e.g.
// min+max for an inclusive range). See report.AssertionMetrics for the
// supported Metric names. Threshold fields (Min/Max/GT/LT/Is/In/Around/
// AroundMargin/DeviatesAround) are duration strings ("500ms") for
// latency_* metrics, plain numbers otherwise (a ratio 0-1 for *_rate,
// requests/sec for rate, a count for requests/success); DeviatesPercent is
// always a plain percentage (10 means ±10%), regardless of metric.
type AssertionConfig struct {
	Metric string `yaml:"metric"`

	// Min/Max: actual >= Min / actual <= Max.
	Min string `yaml:"min"`
	Max string `yaml:"max"`

	// GT/LT: strict inequality — excludes the boundary, unlike Min/Max.
	GT string `yaml:"gt"`
	LT string `yaml:"lt"`

	// Is: actual must equal this value exactly.
	Is string `yaml:"is"`

	// In: actual must be one of these values.
	In []string `yaml:"in"`

	// Around/AroundMargin: actual must fall within Around ± AroundMargin,
	// an absolute margin — both must be set to take effect.
	// AroundExclusive excludes the two boundary values; default is
	// inclusive.
	Around          string `yaml:"around"`
	AroundMargin    string `yaml:"around_margin"`
	AroundExclusive bool   `yaml:"around_exclusive"`

	// DeviatesAround/DeviatesPercent: actual must fall within
	// DeviatesAround ± that percentage of DeviatesAround, a relative
	// margin — both must be set to take effect.
	// DeviatesExclusive excludes the two boundary values; default is
	// inclusive.
	DeviatesAround    string `yaml:"deviates_around"`
	DeviatesPercent   string `yaml:"deviates_percent"`
	DeviatesExclusive bool   `yaml:"deviates_exclusive"`
}

// LoadConfig controls how many iterations run, how fast, and with how much
// concurrency. An iteration is one pass through http.targets (a single
// request) or http.flow (all its steps) — Rate/Requests count iterations,
// not individual HTTP calls, so a multi-step flow's actual request rate is
// roughly rate * len(flow).
type LoadConfig struct {
	Duration string  `yaml:"duration"` // e.g. "30s"; mutually exclusive-ish with Requests
	Requests uint64  `yaml:"requests"`
	Rate     float64 `yaml:"rate"`    // iterations/sec, 0 = max throughput
	Workers  int     `yaml:"workers"` // concurrency ("virtual users")

	// MaxWorkers, if > 0, switches to open model: with Rate set, concurrency
	// is allowed to grow up to this many in-flight iterations to sustain the
	// target rate under latency, instead of capping at Workers (closed
	// model, where the achieved rate degrades toward Workers/latency
	// instead). Requires Rate > 0 unless Stages is set, where it instead
	// raises the ceiling on ramping concurrency past each stage's own
	// Workers value (ramping-arrival-rate) — see StageConfig and README's
	// "Open vs. closed model" section.
	MaxWorkers int `yaml:"max_workers"`

	// Stages, if non-empty, replaces Duration/Rate/Workers with a staged
	// ramp schedule: workers and rate ramp linearly from the previous
	// stage's end values (0 for the first stage) to this stage's values,
	// over its duration. Repeat a value across stages to hold it steady
	// instead of ramping (e.g. a warm-up ramp followed by a steady period).
	// A stage with duration "0" or "" is an instantaneous jump.
	Stages []StageConfig `yaml:"stages"`

	// Iterations, if > 0, bounds how many iterations a single virtual user
	// runs before it departs; a fresh VU (new identity, if using
	// http.identities) immediately takes its place if the run isn't done
	// yet. If it's the only stop condition set (no duration/requests/
	// stages), the run is exactly workers*iterations with no replenishment
	// — "N virtual users, each K iterations." See README's "Virtual user
	// lifecycle" section.
	Iterations uint64 `yaml:"iterations"`
}

// Duration parses LoadConfig.Duration, returning 0 if unset.
func (l LoadConfig) ParsedDuration() (time.Duration, error) {
	if l.Duration == "" {
		return 0, nil
	}
	return time.ParseDuration(l.Duration)
}

type StageConfig struct {
	Duration string  `yaml:"duration"`
	Workers  int     `yaml:"workers"`
	Rate     float64 `yaml:"rate"`
}

// ParsedDuration parses StageConfig.Duration, returning 0 if unset.
func (s StageConfig) ParsedDuration() (time.Duration, error) {
	if s.Duration == "" {
		return 0, nil
	}
	return time.ParseDuration(s.Duration)
}

type HTTPConfig struct {
	// Targets: independent, stateless requests, round-robin per iteration.
	// Mutually exclusive with Flow.
	Targets []HTTPTargetConfig `yaml:"targets"`

	// Flow: an ordered sequence of requests run once per iteration, each
	// able to use values extracted from earlier steps' responses
	// ({{.Vars.name}}). Mutually exclusive with Targets.
	Flow []HTTPTargetConfig `yaml:"flow"`

	// Setup: like Flow, but runs once per worker before its first
	// iteration; its extracted vars seed {{.Vars.*}} for every iteration
	// that worker runs afterward (e.g. log in once, reuse the token).
	// Only meaningful alongside Flow.
	Setup []HTTPTargetConfig `yaml:"setup"`

	// Identities: an optional pool of named values (e.g. credentials or
	// account IDs). Each concurrent worker ("virtual user") is assigned
	// exactly one identity, round-robin over the pool, sticky for its
	// lifetime, and exposed to templates as {{.Identity.<key>}}.
	Identities []map[string]string `yaml:"identities"`

	// Feeder: an optional CSV/JSON data source. One row is handed out per
	// iteration (round-robin or random, per Feeder.Mode) and exposed to
	// templates as {{.Feeder.<column>}} — for driving requests from a real
	// dataset rather than pure random values.
	Feeder FeederConfig `yaml:"feeder"`

	// BaseURL, if set, is prepended to any target/flow/setup url that
	// starts with "/", so a scenario hitting one host
	// doesn't need to repeat "http://host:port" on every entry. A url that
	// doesn't start with "/" (already absolute, or a template expression)
	// is left untouched.
	BaseURL string `yaml:"base_url"`

	Timeout           string `yaml:"timeout"`
	Insecure          bool   `yaml:"insecure"`
	NoFollowRedirects bool   `yaml:"no_follow_redirects"`
	MaxIdleConns      int    `yaml:"max_idle_conns"` // 0 = Go's default (100) per target host

	// MaxResponseBody caps how many response bytes are read/counted per
	// request before the rest is discarded (0 = unlimited). Protects
	// against a target streaming a huge/unbounded response exhausting
	// resonate's own memory under load.
	MaxResponseBody int64 `yaml:"max_response_body"`

	// CertFile/KeyFile, if both set, authenticate resonate to the target via
	// a client certificate (mTLS).
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// CAFile is a PEM bundle of additional CAs to trust — a safer
	// alternative to Insecure for a target signed by a private CA.
	CAFile string `yaml:"ca_file"`

	// H2C forces HTTP/2 over plaintext (prior knowledge); mutually exclusive
	// with Insecure/CertFile/KeyFile/CAFile, which are all TLS-only.
	H2C bool `yaml:"h2c"`

	// HTTP3 forces HTTP/3 (HTTP over QUIC, on UDP) for every request. Every
	// url must be https:// — QUIC is always TLS — and Insecure/CertFile/
	// KeyFile/CAFile apply as usual. Opt-in, not negotiated: there is no
	// Alt-Svc discovery. Mutually exclusive with H2C and DisableKeepAlive.
	HTTP3 bool `yaml:"http3"`

	// DisableKeepAlive forces a fresh connection per request instead of
	// reusing pooled connections; mutually exclusive with H2C and HTTP3.
	DisableKeepAlive bool `yaml:"disable_keepalive"`
}

// FeederConfig loads a pool of rows from a CSV or JSON file, one of which is
// exposed per iteration as {{.Feeder.<column>}}, for driving requests from a
// real-looking dataset instead of pure random values.
type FeederConfig struct {
	// File is a .csv (first row = column names) or .json (an array of flat
	// objects — nested objects/arrays are exposed as their raw JSON text)
	// file.
	File string `yaml:"file"`

	// Rows is an inline array of rows (each a column->value map), as an
	// alternative to File — mutually exclusive with it. Mode "stream" is
	// not allowed with Rows.
	Rows []map[string]string `yaml:"rows"`

	// Mode is "sequential" (default: rows handed out round-robin, wrapping
	// around), "random" (a row chosen uniformly at random each time), or
	// "stream". "sequential"/"random" read File into memory once, at
	// construction — simplest, but unsuitable for a file too large to
	// comfortably fit in RAM (e.g. an exported production dataset).
	// "stream" instead reads File incrementally in the background (like
	// sequential, it wraps around indefinitely), holding only a small
	// read-ahead buffer in memory regardless of File's size.
	Mode string `yaml:"mode"`

	// Kafka consumes a topic instead of reading File: each message is a
	// row. Mutually exclusive with File (and Mode, which only applies to
	// files).
	Kafka *KafkaFeederConfig `yaml:"kafka"`
}

// Configured reports whether any data source is set.
func (f FeederConfig) Configured() bool { return f.File != "" || len(f.Rows) > 0 || f.Kafka != nil }

// KafkaFeederConfig is a feeder fed by a Kafka topic. See
// internal/generator.KafkaFeederOptions for the semantics of each field.
type KafkaFeederConfig struct {
	Brokers []string `yaml:"brokers"`
	Topic   string   `yaml:"topic"`
	// Format is "json" (default) or "raw".
	Format string `yaml:"format"`
	// From is "start" (default) or "end".
	From string `yaml:"from"`
	// OnEnd is "wrap" (default for from: start) or "wait".
	OnEnd string `yaml:"on_end"`
	// Timeout bounds connecting and waiting for the first message (default 10s).
	Timeout string `yaml:"timeout"`

	TLS      bool   `yaml:"tls"`
	Insecure bool   `yaml:"insecure"`
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// SASLMechanism is PLAIN, SCRAM-SHA-256 or SCRAM-SHA-512.
	SASLMechanism string `yaml:"sasl_mechanism"`
	Username      string `yaml:"username"`
	Password      string `yaml:"password"`
	// PasswordEnv names an environment variable holding the password, so
	// it needn't be written into a checked-in scenario file.
	PasswordEnv string `yaml:"password_env"`
}

// HTTPTargetConfig describes one request template, used for both http.targets
// and http.flow/http.setup steps. URL, Query, Headers, and Body may all embed
// {{ }} template expressions (see internal/tmpl), re-rendered independently
// for every request. Extract pulls values out of the response into named
// vars (flow/setup only; ignored under targets) — see internal/generator's
// extractValue for the rule syntax ("json:<path>", "yaml:<path>",
// "xml:<path>", "regex:<pattern>", "css:<selector>", "header:<Name>",
// "status").
//
// A flow/setup entry is either a request (Method/URL set) or a
// control-flow block (exactly one of Repeat/During/If set, plus Steps) —
// never both; control-flow blocks are only meaningful under flow/setup,
// not targets. See internal/generator.FlowStep for the exact semantics.
type HTTPTargetConfig struct {
	Method   string            `yaml:"method"`
	URL      string            `yaml:"url"`
	Query    map[string]string `yaml:"query"`
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`
	BodyFile string            `yaml:"body_file"`

	// RawBodyFile, if set, reads the request body from this file once, at
	// construction, and sends it exactly as-is on every request — no
	// templating, unlike Body/BodyFile. For a large or binary payload
	// where per-request re-rendering would be wasteful, or where the raw
	// bytes might otherwise be misread as containing "{{ }}" template
	// syntax. Mutually exclusive with Body/BodyFile.
	RawBodyFile string `yaml:"raw_body_file"`

	Extract map[string]string `yaml:"extract"`

	// Pause, if set, sleeps this long immediately before this step's
	// request (flow/setup only) — think-time. If PauseMax is
	// also set (and greater than Pause), the sleep is a random duration
	// drawn uniformly from [Pause, PauseMax) instead.
	Pause    string `yaml:"pause"`
	PauseMax string `yaml:"pause_max"`

	// Repeat, if > 0, makes this a control-flow entry: Steps runs this
	// many times in a row. Mutually exclusive with During/If.
	Repeat int `yaml:"repeat"`

	// During, if set, makes this a control-flow entry: Steps runs
	// repeatedly for this long. Mutually exclusive with Repeat/If.
	During string `yaml:"during"`

	// If, if non-empty, makes this a control-flow entry: a template
	// condition re-rendered every time this entry is reached ({{.Vars.*}}
	// available); Steps runs only if it renders "true" or "1". Mutually
	// exclusive with Repeat/During.
	If string `yaml:"if"`

	// Steps holds the nested entries for a Repeat/During/If control-flow
	// entry; unused on a request entry.
	Steps []HTTPTargetConfig `yaml:"steps"`

	// ExpectStatus, if non-empty, overrides the default success criterion
	// (2xx/3xx) for this target/step: a response whose status isn't in this
	// list counts as a failed result (with an explanatory error), even if
	// it's otherwise a 2xx/3xx — e.g. expecting a 404 as the "correct"
	// response for a not-found check.
	ExpectStatus []int `yaml:"expect_status"`

	// ExpectHeaders, if non-empty, checks response headers (case-insensitive
	// name): a non-empty value requires an exact match, an empty value only
	// requires the header to be present. Combines with ExpectStatus (AND)
	// when both are set; either one alone still overrides the default
	// 2xx/3xx-only success criterion.
	ExpectHeaders map[string]string `yaml:"expect_headers"`

	// ExpectBody, if non-empty, checks the response body via the same rule
	// language as Extract ("json:<path>", "yaml:<path>", "xml:<path>", "header:<Name>",
	// "status") — keyed by rule, valued by the expected result: a non-empty
	// value requires an exact match, an empty value only requires the rule
	// to evaluate without error (e.g. the JSON/YAML/XML path exists). Combines
	// with ExpectStatus/ExpectHeaders (AND) when set alongside them.
	ExpectBody map[string]string `yaml:"expect_body"`
}

// ParsedPause parses Pause, returning 0 if unset.
func (t HTTPTargetConfig) ParsedPause() (time.Duration, error) {
	if t.Pause == "" {
		return 0, nil
	}
	return time.ParseDuration(t.Pause)
}

// ParsedPauseMax parses PauseMax, returning 0 if unset.
func (t HTTPTargetConfig) ParsedPauseMax() (time.Duration, error) {
	if t.PauseMax == "" {
		return 0, nil
	}
	return time.ParseDuration(t.PauseMax)
}

// ParsedDuring parses During, returning 0 if unset.
func (t HTTPTargetConfig) ParsedDuring() (time.Duration, error) {
	if t.During == "" {
		return 0, nil
	}
	return time.ParseDuration(t.During)
}

// ResolveBody returns the request body template source, reading BodyFile if
// set (its contents are treated as a template too).
func (t HTTPTargetConfig) ResolveBody() (string, error) {
	if t.BodyFile != "" {
		b, err := os.ReadFile(t.BodyFile)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return t.Body, nil
}

// WSConfig describes a single WebSocket connection lifecycle: dial once per
// iteration, run Messages in order, close.
type WSConfig struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`

	// Identities: an optional pool of named values (e.g. tokens or account
	// IDs). Each virtual user is assigned exactly one identity, round-robin
	// over the pool, sticky for its lifetime, and exposed to templates as
	// {{.Identity.<key>}}.
	Identities []map[string]string `yaml:"identities"`

	// Feeder: an optional CSV/JSON data source. One row is handed out per
	// iteration and exposed to templates as {{.Feeder.<column>}}.
	Feeder FeederConfig `yaml:"feeder"`

	// Messages: sent in order over one connection per iteration. Extracted
	// vars from an earlier message (Wait: true + Extract) are available to
	// later messages via {{.Vars.<name>}}.
	Messages []WSMessageConfig `yaml:"messages"`

	Timeout  string `yaml:"timeout"`
	Insecure bool   `yaml:"insecure"`
}

// WSMessageConfig is one entry in ws.messages. Body may embed {{ }}
// expressions (see internal/tmpl). Extract pulls values out of the response
// frame into named vars — only meaningful when Wait is true; see
// internal/generator's extractValue for the rule syntax. WS frames have no
// headers/status, so only "json:<path>"/"yaml:<path>"/"xml:<path>" rules
// are meaningful here.
type WSMessageConfig struct {
	Body    string            `yaml:"body"`
	Binary  bool              `yaml:"binary"`
	Wait    bool              `yaml:"wait"`
	Extract map[string]string `yaml:"extract"`

	// ExpectBody, if non-empty, checks the response frame via the same
	// rule language as Extract (json:<path>/yaml:<path>/xml:<path> only,
	// same caveat as Extract) — only meaningful alongside Wait: true.
	ExpectBody map[string]string `yaml:"expect_body"`
}

// SSEConfig describes one Server-Sent Events stream, opened fresh every
// iteration: send the request, read the text/event-stream response until
// Events events have arrived and/or Duration has elapsed, close. See
// internal/generator.SSETarget for the exact stop/failure semantics.
// URL, Query, Headers and Body may embed {{ }} template expressions.
type SSEConfig struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"` // default GET; POST is common for streaming APIs
	Query   map[string]string `yaml:"query"`
	Headers map[string]string `yaml:"headers"`

	Body        string `yaml:"body"`
	BodyFile    string `yaml:"body_file"`
	RawBodyFile string `yaml:"raw_body_file"`

	// Events, if > 0, ends the stream successfully after this many events.
	Events int `yaml:"events"`

	// Duration, if set, is how long to listen (a normal, successful end when
	// Events is unset; a deadline for receiving them when it is set).
	Duration string `yaml:"duration"`

	// Identities: an optional pool of named values (e.g. tokens), one
	// assigned per virtual user and exposed as {{.Identity.<key>}}.
	Identities []map[string]string `yaml:"identities"`

	// Feeder: an optional CSV/JSON data source, one row per iteration,
	// exposed as {{.Feeder.<column>}}.
	Feeder FeederConfig `yaml:"feeder"`

	// Timeout bounds connecting and receiving response headers — not the
	// stream itself, which Events/Duration bound.
	Timeout  string `yaml:"timeout"`
	Insecure bool   `yaml:"insecure"`

	// MaxResponseBody caps the bytes read per stream (0 = unlimited).
	MaxResponseBody int64 `yaml:"max_response_body"`

	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	CAFile   string `yaml:"ca_file"`

	// H2C forces prior-knowledge HTTP/2 over plaintext, like http.h2c.
	H2C bool `yaml:"h2c"`

	// HTTP3 uses HTTP/3 (QUIC over UDP), like http.http3; the url must be
	// https://. Mutually exclusive with H2C.
	HTTP3 bool `yaml:"http3"`
}

// ParsedDuration parses SSEConfig.Duration, returning 0 if unset.
func (c SSEConfig) ParsedDuration() (time.Duration, error) {
	if c.Duration == "" {
		return 0, nil
	}
	return time.ParseDuration(c.Duration)
}

// GRPCConfig describes gRPC calls — unary and streaming: each iteration makes
// Calls in order over a shared connection, stopping at the first failure.
//
// The schema (which messages a method takes and returns) comes from
// ProtoFiles, else DescriptorSet, else the server's reflection service.
type GRPCConfig struct {
	// Address is "host:port" (or any address grpc-go's resolver accepts).
	Address string `yaml:"address"`

	// Plaintext disables TLS. Otherwise TLS is used, configured by Insecure/
	// CAFile/CertFile/KeyFile like their http.* namesakes.
	Plaintext bool   `yaml:"plaintext"`
	Insecure  bool   `yaml:"insecure"`
	CAFile    string `yaml:"ca_file"`
	CertFile  string `yaml:"cert_file"`
	KeyFile   string `yaml:"key_file"`

	// Timeout is the per-call deadline (and the schema-lookup deadline).
	Timeout string `yaml:"timeout"`

	// Connections spreads virtual users over this many separate gRPC
	// connections (default 1). One connection is one HTTP/2 connection, which
	// servers typically cap at ~100 concurrent streams — raise this at high
	// concurrency or calls queue client-side and latency inflates.
	Connections int `yaml:"connections"`

	// ProtoFiles/ImportPaths: compile .proto sources. ImportPaths default to
	// the directories containing ProtoFiles.
	ProtoFiles  []string `yaml:"proto_files"`
	ImportPaths []string `yaml:"import_paths"`

	// DescriptorSet: a FileDescriptorSet file from
	// `protoc --descriptor_set_out=FILE --include_imports`.
	DescriptorSet string `yaml:"descriptor_set"`

	// Metadata is sent with every call (gRPC headers); values may embed
	// {{ }} template expressions.
	Metadata map[string]string `yaml:"metadata"`

	// Identities: an optional pool of named values (e.g. tokens), one per
	// virtual user, exposed as {{.Identity.<key>}}.
	Identities []map[string]string `yaml:"identities"`

	// Feeder: an optional CSV/JSON data source, one row per iteration,
	// exposed as {{.Feeder.<column>}}.
	Feeder FeederConfig `yaml:"feeder"`

	Calls []GRPCCallConfig `yaml:"calls"`
}

// GRPCCallConfig is one RPC. Message is the request as protobuf JSON and may
// embed {{ }} expressions; MessageFile reads it from a file instead (also
// templated). The response is rendered back to protobuf JSON (lowerCamelCase
// field names, zero values included) for Extract/ExpectBody.
//
// The method's schema says whether it is unary, server-streaming,
// client-streaming or bidirectional, and which of the fields below it takes:
// unary and server-streaming methods take Message (one request); client-
// streaming and bidirectional ones take Messages (a sequence). Receive and
// Duration bound a stream that has server responses.
type GRPCCallConfig struct {
	// Method is "package.Service/Method".
	Method      string `yaml:"method"`
	Message     string `yaml:"message"`
	MessageFile string `yaml:"message_file"`

	// Messages are the requests of a client-streaming or bidirectional call,
	// sent in order (each rendered fresh as it is sent), the whole list
	// Repeat times (default 1), SendInterval apart if set.
	Messages     []string `yaml:"messages"`
	Repeat       int      `yaml:"repeat"`
	SendInterval string   `yaml:"send_interval"`

	// Receive ends a server-streaming or bidirectional call successfully
	// after this many responses; the server closing the stream first is a
	// failure. Duration is a listen window: with Receive unset, its elapsing
	// is the normal successful end; with Receive set, it is the deadline for
	// getting them. With neither, the stream is read until the server closes
	// it, and still being open at grpc.timeout is a failure.
	Receive  int    `yaml:"receive"`
	Duration string `yaml:"duration"`

	// Metadata is sent with this call, overriding same-named keys in
	// grpc.metadata.
	Metadata map[string]string `yaml:"metadata"`

	// Extract pulls values from the response into {{.Vars.<name>}} for later
	// calls. Same rule language as http extract; header:<Name> reads response
	// headers/trailers and "status" is the gRPC status code number.
	Extract map[string]string `yaml:"extract"`

	// ExpectCode lists the gRPC status names (OK, NOT_FOUND, ...) that count
	// as success, replacing the OK-only default.
	ExpectCode []string `yaml:"expect_code"`

	// ExpectBody checks the response JSON with the same rule language as
	// Extract; an empty expected value only requires the rule to evaluate.
	ExpectBody map[string]string `yaml:"expect_body"`
}

// ParsedDuration parses Duration, returning 0 if unset.
func (c GRPCCallConfig) ParsedDuration() (time.Duration, error) {
	if c.Duration == "" {
		return 0, nil
	}
	return time.ParseDuration(c.Duration)
}

// ParsedSendInterval parses SendInterval, returning 0 if unset.
func (c GRPCCallConfig) ParsedSendInterval() (time.Duration, error) {
	if c.SendInterval == "" {
		return 0, nil
	}
	return time.ParseDuration(c.SendInterval)
}

// ResolveMessage returns the request message template source, reading
// MessageFile if set.
func (c GRPCCallConfig) ResolveMessage() (string, error) {
	if c.MessageFile != "" {
		b, err := os.ReadFile(c.MessageFile)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return c.Message, nil
}

// ParsedTimeout parses WSConfig.Timeout, returning 0 if unset.
func (w WSConfig) ParsedTimeout() (time.Duration, error) {
	if w.Timeout == "" {
		return 0, nil
	}
	return time.ParseDuration(w.Timeout)
}

// Load reads and parses a scenario file.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &s, nil
}

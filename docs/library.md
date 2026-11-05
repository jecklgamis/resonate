# Using resonate as a Go library

The same engine is importable directly: `go get
github.com/jecklgamis/resonate` and drive a load test from your own Go
program instead of shelling out to the binary or writing a scenario YAML
file.

The root `resonate` package is a thin facade (type aliases + wrapper
functions) over the `internal/generator`/`internal/engine`/
`internal/report` packages the CLI is built on — `internal/cli` and
`internal/tmpl` stay unexported. A library consumer builds
`HTTPTarget`/`FlowStep`/`WSMessage` values in Go directly rather than
going through flag parsing or YAML.

## The `Scenario` builder DSL

`resonate.NewScenario()` offers a fluent builder as an alternative to
the struct-literal API — chained calls cover request building, response
checks, single or multiple targets, multi-step flows, WebSocket, and
every execution model described in [Execution Models](execution-models.md).

The default, single-target case needs no extra setup — `Method`/`Get`/
`Post`/... implicitly start the (one) target on first use:

```go
summary, err := resonate.NewScenario().
	Post("http://localhost:8080/orders").
	Header("X-Request-Id", "{{uuid}}").
	JSONBody(`{"id": {{.Seq}}}`).
	ExpectStatus(201).
	Rate(50).MaxWorkers(200).Duration(30 * time.Second).
	Run(ctx)
```

`Target(method, url)` adds an *additional* independent target — with
more than one, the generator round-robins through them, same as a
scenario YAML file's `targets:` list:

```go
summary, err := resonate.NewScenario().
	Target("GET", "http://localhost:8080/a").
	Target("GET", "http://localhost:8080/b").
	Rate(50).Duration(30 * time.Second).
	Run(ctx)
```

`Step(method, url)`/`Setup(method, url)` switch to building a multi-step
*flow* instead — every step runs in order per iteration, with `Extract`
pulling a value into `{{.Vars.<name>}}` for later steps, and `Identity`
adding a round-robin identity pool exposed as `{{.Identity.<key>}}`;
same as a scenario YAML file's `flow:`/`setup:`/`identities:`:

```go
summary, err := resonate.NewScenario().
	Step("GET", "http://localhost:8080/login").
	Extract("token", "json:token").
	Step("POST", "http://localhost:8080/orders").
	Header("Authorization", "Bearer {{.Vars.token}}").
	ExpectStatus(201).
	Requests(100).Workers(10).
	Run(ctx)
```

`Target` and `Step`/`Setup` are mutually exclusive within one
`Scenario` — mixing them is a `Build()`/`Run()` error (they build two
different generator types).

Flow steps also support think-time and control flow: `Pause(d)`/
`PauseRange(min, max)` sleep before the current step; `Repeat(n)`,
`During(d)`, and `If(cond)` open a control-flow block — every `Step`
added until the matching `End()` runs inside it — and nest freely:

```go
summary, err := resonate.NewScenario().
	Step("GET", "http://localhost:8080/login").
	Extract("role", "json:role").
	Step("GET", "http://localhost:8080/dashboard").
	Pause(500 * time.Millisecond).
	Repeat(3).
		Step("GET", "http://localhost:8080/poll").
		End().
	If(`{{eq .Vars.role "admin"}}`).
		Step("GET", "http://localhost:8080/admin").
		End().
	Requests(100).Workers(10).
	Run(ctx)
```

A failed request step stops the whole flow, unwinding out of every
enclosing block, same as the YAML schema's control-flow blocks (see
[Scenarios](scenarios.md#pauses-and-control-flow)).

Method groups on `Scenario`:

- **Request building** — `Target`, `Step`, `Setup`, `Method`, `Get`,
  `Post`, `Put`, `Patch`, `Delete`, `Header`, `Query`, `Timeout`,
  `Insecure` — all apply to "whatever request is currently being
  built": the last `Target`/`Step`/`Setup` call, or the implicit single
  target.
- **Request body** — `Body`/`JSONBody` (inline string), `FileBody(path)`
  (templated, from a file), `RawBody(data)`/`RawFileBody(path)` (sent
  as-is, bypassing templating — for a large/binary payload or literal
  `"{{ }}"`). `Body`/`FileBody` and `RawBody`/`RawFileBody` are mutually
  exclusive; a read error is deferred to `Build()`/`Run()` rather than
  breaking the chain.
- **Base URL** — `BaseURL(url)` prepends `url` to any request URL
  starting with `/`; a `/`-prefixed URL with no `BaseURL` set fails fast
  at `Build()`/`Run()`.
- **Flow-only** — `Extract` (response chaining, no-op outside
  `Step`/`Setup`), `Identity` (round-robin identity pool), `Pause`/
  `PauseRange`, `Repeat`/`During`/`If`/`End`.
- **Checks** — `ExpectStatus`, `ExpectHeader`, `ExpectBody` — see
  [Status, Header, and Body Checks](usage.md#status-header-and-body-checks).
- **Feeders** — `Feeder(path, mode)` loads a CSV/JSON file (or
  `FeederRows(rows, mode)` takes rows directly) and hands out
  one row per iteration as `{{.Feeder.<column>}}` — see
  [Feeders](feeders.md). Applies to whichever kind of
  `Scenario` is being built (HTTP/flow, or the WS connection once `WS`
  has been called); a load error is deferred to `Build()`/`Run()`.
  A **Kafka topic** as the source isn't a builder method: build it with
  `resonate.NewKafkaFeeder(resonate.KafkaFeederOptions{Brokers: ..., Topic: ...})`
  (options mirror [the YAML](kafka-feeder.md#options); see [From Go](kafka-feeder.md#from-go)) and set
  it as `HTTPOptions.Feeder` / `WSOptions.Feeder` / `GRPCOptions.Feeder` (or
  `.HTTP.Feeder` on `FlowOptions`/`SSEOptions`); `Close()` it after the run.
- **Assertions** — `Assert(...Assertion)` accumulates assertions;
  `Run(ctx)` evaluates them and returns a non-nil error (with `Summary`
  still populated) if any fail or a `Metric` name is unrecognized — the
  DSL equivalent of `assertions:`/`--assert`. `Assertion` is the same
  struct-literal type used everywhere else — see
  [Scenarios](scenarios.md#assertions) for the full condition set.
  Callers who want individual `Failure`s should call `Summary.Evaluate`
  directly instead.
- **WebSocket** — `WS(url)` switches to building a `WSGenerator`: dial
  `url` fresh every iteration, then `Message(body)` appends a message,
  with `Wait()`/`Binary()`/`Extract`/`ExpectBody` applying to whichever
  message was most recently appended. `Header`, before any `Message`,
  sets a connection header. Mutually exclusive with `Target`/`Step`/
  `Setup`; `Extract`/`ExpectBody` on a message with no `Wait()` is a
  `Build()`/`Run()` error. See [WebSocket](scenarios.md#websocket).

  ```go
  summary, err := resonate.NewScenario().
  	WS("ws://localhost:8080/socket").
  	Identity(map[string]string{"token": "abc123"}).
  	Header("Authorization", "Bearer {{.Identity.token}}").
  	Message(`{"type":"subscribe","channel":"orders"}`).
  	Wait().
  	Extract("session_id", "json:session_id").
  	Message(`{"type":"ping","session":"{{.Vars.session_id}}"}`).
  	Wait().
  	Rate(20).Workers(10).Duration(30 * time.Second).
  	Run(ctx)
  ```
- **Execution model** — `Rate`, `Workers`, `MaxWorkers`, `Duration`,
  `Requests`, `Stages(...Stage)`, `Iterations(n)`.
- **Terminal calls** — `Build()` returns the underlying `Generator`
  (`*HTTPGenerator`, `*FlowGenerator`, or `*WSGenerator`); `Run(ctx)`
  builds and runs in one step, returning a `Summary` (and evaluating any
  `Assert`-ed assertions).

`Scenario`'s fields are unexported — no struct-literal escape hatch,
only the builder methods above.

Report writing is a separate, explicit step, so embedding a load test
in your program never causes a surprise disk write:

```go
if err := resonate.WriteHTMLFile("report.html", summary.HTMLReport()); err != nil {
	log.Fatal(err)
}
```

## Beyond `Scenario`

`Scenario` covers WebSocket, feeders, and assertions too, but the
struct-literal API it's built on is still available for cases that want
more control (e.g. building a `Generator` once and driving it with your
own retry loop) — `resonate.NewHTTPGenerator`, `resonate.NewFlowGenerator`,
`resonate.NewWSGenerator`, `resonate.NewSSEGenerator`,
`resonate.NewGRPCGenerator`, `resonate.Feeder`, `resonate.NewArrayFeeder`,
`resonate.Run`, `resonate.Summary`, `resonate.Assertion`/`Evaluate` cover
the same ground as `resonate hit`/`resonate run`, as Go APIs instead of
flags/YAML:

```go
gen, err := resonate.NewHTTPGenerator(
	[]resonate.HTTPTarget{{URL: "http://localhost:8080/health"}},
	resonate.DefaultHTTPOptions(),
)
if err != nil {
	log.Fatal(err)
}
defer gen.Close()

summary := resonate.Run(ctx, gen, resonate.Options{Rate: 50, Workers: 20, Duration: 30 * time.Second})
summary.Print(os.Stdout)
```

See [`resonate_test.go`](https://github.com/jecklgamis/resonate/blob/main/resonate_test.go)
for worked examples (a plain HTTP run, a multi-step flow, assertions, a
feeder, gRPC unary and streaming) written using only this public package.

### HTTP/3

HTTP/3 needs no new API: set `HTTP3` on `HTTPOptions` for the struct API, or call
`HTTP3()` on the fluent `Scenario` builder (for SSE, `opts.HTTP.HTTP3`). Every URL must
be `https://`, and a violation is an error from the constructor (or `Build()`/`Run()`)
before anything is sent. See the [HTTP/3 guide](http3.md#from-go) for examples.

```go
summary, err := resonate.NewScenario().
	Get("https://localhost:8443/proto").
	HTTP3().
	Insecure().
	Requests(100).Workers(10).
	Run(ctx)
if err != nil {
	log.Fatal(err)
}
summary.Print(os.Stdout)
```

### Server-Sent Events

Like gRPC, SSE is available through the struct API only; the fluent `Scenario`
builder has no SSE methods. An `SSETarget` is one stream's request plus when to
stop reading it; the fields match the
[`sse:` scenario block](sse.md).

```go
opts := resonate.DefaultSSEOptions()
opts.Identities = []map[string]string{{"token": "abc123"}}

gen, err := resonate.NewSSEGenerator(resonate.SSETarget{
	URL:      "http://localhost:8765/events",
	Header:   map[string]string{"Authorization": "Bearer {{.Identity.token}}"},
	Events:   10,               // succeed once 10 events arrive...
	Duration: 15 * time.Second, // ...or fail if they haven't by then
}, opts)
if err != nil {
	log.Fatal(err)
}
defer gen.Close()

summary := resonate.Run(ctx, gen, resonate.Options{Workers: 20, Duration: 30 * time.Second})
```

A stream is one connect `Result` plus one per event; `opts.HTTP` carries the
usual transport settings (`Timeout`, `Insecure`, certificates, ...).

### gRPC

gRPC is available through the struct API only — the fluent `Scenario`
builder has no gRPC methods. A `GRPCTarget` is a server address plus the
`GRPCCall`s made, in order, every iteration; the same fields as the
[`grpc:` scenario block](grpc.md) apply, so see that page for
what each means.

```go
opts := resonate.DefaultGRPCOptions()
opts.Plaintext = true // no TLS; or set Insecure / CAFile / CertFile / KeyFile
// opts.ProtoFiles = []string{"api/user.proto"} // otherwise: server reflection

gen, err := resonate.NewGRPCGenerator(resonate.GRPCTarget{
	Address: "localhost:50051",
	Calls: []resonate.GRPCCall{
		{ // unary: Message is the request, as protobuf JSON (templated)
			Method:  "acme.user.v1.UserService/CreateUser",
			Message: `{"name":"alice","age":{{randInt 18 65}}}`,
			Extract: map[string]string{"id": "json:id"},
		},
		{ // server streaming: stop after 5 messages
			Method:     "acme.feed.v1.Feed/Subscribe",
			Message:    `{"topic":"orders"}`,
			Receive:    5,
			ExpectBody: map[string]string{"json:topic": "orders"},
		},
		{ // client streaming: Messages are sent in order, Repeat times
			Method:   "acme.feed.v1.Feed/Upload",
			Messages: []string{`{"id":"{{uuid}}","payload":"x"}`},
			Repeat:   10,
		},
	},
}, opts)
if err != nil {
	log.Fatal(err) // includes a bad method name, a field that doesn't suit the call's kind, ...
}
defer gen.Close()

summary := resonate.Run(ctx, gen, resonate.Options{Rate: 50, Workers: 20, Duration: 30 * time.Second})
```

`NewGRPCGenerator` resolves the schema before it returns, which with server
reflection means a round trip to the server (so an unreachable server is an
error here, not a failed first request). A unary call is one `Result`; a
streaming call is one `Result` per received message.

## Examples project

[`examples/dsl-examples`](https://github.com/jecklgamis/resonate/tree/main/examples/dsl-examples)
is a full standalone project (its own `go.mod`, `go run .`) with eight
commands:

| Command | Demonstrates |
| --- | --- |
| `full-scenario` | POST of a randomized JSON payload, a fresh `X-Request-Id` per request, `ExpectStatus`, HTML report |
| `constant-vus` | Closed, flat — `Workers` alone, no `Rate` |
| `ramping-vus` | Closed, staged — `Stages` with `Rate` left at `0` |
| `constant-arrival-rate` | Open, flat — `Rate` + `Workers` + `MaxWorkers` |
| `ramping-arrival-rate` | Open, staged — `Stages` with `Rate` per stage, plus global `MaxWorkers` |
| `bounded-arrival-rate` | Open pacing, closed concurrency cap — same as above but no `MaxWorkers` |
| `per-vu-iterations` | `Iterations` + `Workers`, no `Duration`/`Requests`/`Stages` |
| `shared-iterations` | `Requests(N)` alone — fixed total regardless of `Workers` |

Named to match the table in [Execution Models](execution-models.md),
with real captured output in its own README.

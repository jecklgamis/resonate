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
checks, single or multiple targets, multi-step flows, WebSocket, Server-Sent Events,
gRPC, and every execution model described in [Execution Models](execution-models.md).
A `Scenario` is exactly one protocol: HTTP (`Get`/`Target`/`Step`/...), `WS`, `SSE` or `GRPC`;
mixing them is a `Build()`/`Run()` error.

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
- **HTTP transport** — `FollowRedirects(bool)`, `MaxIdleConns(n)`, `MaxResponseBody(bytes)`,
  `H2C()`, `DisableKeepAlive()`, `HTTP3()`, `CAFile(path)` and `ClientCert(cert, key)` (mTLS), next
  to `Timeout`/`Insecure`/`BaseURL`. They configure the one shared client, so they apply to HTTP,
  flow and SSE scenarios; on a WebSocket or gRPC scenario the HTTP-only ones (everything but
  `CAFile`/`ClientCert`, which gRPC honours) are a `Build()` error, whichever order you called them
  in. Conflicting combinations (`H2C` with `Insecure`, `DisableKeepAlive` or `HTTP3`) are rejected
  at construction.
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
- **Step names** — `Name(label)` labels the current target or step in the
  report's per-step breakdown (`Summary.Steps`) and for `Assertion.Step`; unnamed
  requests are labelled `METHOD url`. See
  [Per-Step Results](scenarios.md#per-step-results).
- **Assertions** — `Assert(...Assertion)` accumulates assertions;
  `Run(ctx)` evaluates them and returns a non-nil error (with `Summary`
  still populated) if any fail or a `Metric` name is unrecognized — the
  DSL equivalent of `assertions:`/`--assert`. `Assertion` is the same
  struct-literal type used everywhere else — see
  [Scenarios](scenarios.md#assertions) for the full condition set.
  Callers who want individual `Failure`s should call `Summary.Evaluate`
  directly instead. Set `Assertion.Step` to check one step's numbers. Set
  `Assertion.Abort` (with `AbortAfter`, which is `0` here, so no warm-up unless you
  give one) to stop the run when it fails: `Run` then returns the partial `Summary`
  with `Aborted` set and an error. With the struct-literal API, pass such assertions as
  `Options.AbortOn` to `resonate.Run`, which fills `Summary.Aborted`.
- **WebSocket** — `WS(url)` switches to building a `WSGenerator`: dial
  `url` fresh every iteration, then `Message(body)` appends a message,
  with `Wait()`/`Binary()`/`Extract`/`ExpectBody` applying to whichever
  message was most recently appended. `Header`, before any `Message`,
  sets a connection header. Mutually exclusive with `Target`/`Step`/
  `Setup`; `Extract`/`ExpectBody` on a message with no `Wait()` is a
  `Build()`/`Run()` error. See [WebSocket](scenarios.md#websocket). `Message`, `Wait` and `Binary`
  outside a WebSocket scenario (or `Wait`/`Binary` before any `Message`) are a `Build()`/`Run()`
  error rather than being silently ignored.

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
- **Server-Sent Events** — `SSE(method, url)` switches to building an `SSEGenerator`: every iteration
  opens a stream and reads events. `Method`/`Get`/`Post`, `Header`, `Query`, `Body`/`JSONBody`/
  `FileBody`/`RawBody` then describe that request exactly as for an HTTP target (so
  `SSE("GET", u).Post(u2)` retargets the stream's request); `Events(n)` and `StreamDuration(d)` say
  when to stop reading. Checks, `Extract`, `Pause` and `Name` don't apply to a stream. See
  [Server-Sent Events](sse.md).

  ```go
  summary, err := resonate.NewScenario().
  	SSE("POST", "http://localhost:8765/v1/stream").
  	Header("Authorization", "Bearer {{.Identity.token}}").
  	Identity(map[string]string{"token": "abc123"}).
  	JSONBody(`{"prompt":"hello"}`).
  	Events(10).
  	Workers(20).Duration(30 * time.Second).
  	Run(ctx)
  ```
- **gRPC** — `GRPC(address)` switches to building a `GRPCGenerator`; `Call("pkg.Service/Method")`
  appends a call, and `Message(json)` (one request) or `Messages(json...)` (a client stream),
  `Receive(n)`, `StreamDuration(d)`, `SendInterval(d)`, `SendRepeat(n)`, `Extract`, `ExpectBody`
  and `ExpectCode(names...)` apply to the most recent `Call`. `Metadata(k, v)` before any `Call`
  applies to every call, after one to that call only (`Header` is rejected: use `Metadata`).
  Connection settings: `Plaintext()`, `Insecure()`, `CAFile`, `ClientCert`, `Connections(n)`,
  `Timeout`, and the schema source — `ProtoFiles(...)`/`ImportPaths(...)`, `DescriptorSet(path)`,
  or server reflection by default (which means `Build()` can fail if the server is unreachable).
  The call's kind is read from the schema, so a field that doesn't suit it (`Receive` on a unary
  call) is a `Build()` error. See [gRPC](grpc.md).

  ```go
  summary, err := resonate.NewScenario().
  	GRPC("localhost:50051").Plaintext().
  	Metadata("authorization", "Bearer {{.Identity.token}}").
  	Identity(map[string]string{"token": "abc123"}).
  	Call("acme.user.v1.UserService/CreateUser").
  	Message(`{"name":"alice","age":{{randInt 18 65}}}`).
  	Extract("id", "json:id").
  	Call("acme.feed.v1.Feed/Subscribe").
  	Message(`{"topic":"orders"}`).
  	Receive(5).
  	Rate(20).Workers(10).Duration(30 * time.Second).
  	Run(ctx)
  ```
- **Execution model** — `Rate`, `Workers`, `MaxWorkers`, `Duration`,
  `Requests`, `Stages(...Stage)`, `Iterations(n)`.
- **Terminal calls** — `Build()` returns the underlying `Generator`
  (`*HTTPGenerator`, `*FlowGenerator`, `*WSGenerator`, `*SSEGenerator` or `*GRPCGenerator`); `Run(ctx)`
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

### Which methods apply to which protocol

A `Scenario` is one protocol, and most setters mean something different in each — or nothing. This
is the whole surface. **✓** applies. **—** doesn't, and is a `Build()`/`Run()` error. **no-op** is
accepted and does nothing — these are a few long-standing cases (a flow-only setter on a plain `Target`, say),
kept for compatibility; everything else that can't apply is rejected.

| Method | HTTP / flow | WebSocket | SSE | gRPC |
| --- | :---: | :---: | :---: | :---: |
| **Start the scenario** | `Get`/`Post`/`Put`/`Patch`/`Delete`/`Method`, `Target`, `Step`, `Setup` | `WS(url)` | `SSE(method, url)` | `GRPC(address)`, then `Call(method)` |
| `Header` | ✓ | ✓ (connection header, before any `Message`) | ✓ | — use `Metadata` |
| `Query`, `Body`, `JSONBody`, `FileBody`, `RawBody`, `RawFileBody` | ✓ | — use `Message` | ✓ | — use `Message` |
| `Name` | ✓ | — | — | — |
| `ExpectStatus`, `ExpectHeader` | ✓ | — | — | — |
| `ExpectBody` | ✓ | ✓ (per message, needs `Wait`) | — | ✓ (per call) |
| `Extract` | ✓ in a flow; no-op on a plain `Target` | ✓ (per message, needs `Wait`) | no-op | ✓ (per call) |
| `Pause`, `PauseRange` | ✓ in a flow; no-op on a plain `Target` | — | no-op | — |
| `Repeat`, `During`, `If`, `End` | ✓ (flow only) | — | — | — |
| `Identity` | ✓ in a flow; no-op on a plain `Target` | ✓ | ✓ | ✓ |
| `Feeder`, `FeederRows` | ✓ | ✓ | ✓ | ✓ |
| `Timeout`, `Insecure` | ✓ | ✓ | ✓ | ✓ |
| `BaseURL` | ✓ | no-op | ✓ | no-op |
| `HTTP3`, `H2C`, `DisableKeepAlive`, `FollowRedirects`, `MaxIdleConns`, `MaxResponseBody` | ✓ | — | ✓ | — |
| `CAFile`, `ClientCert` | ✓ | — | ✓ | ✓ |
| `Message` | — | ✓ appends a frame | — | ✓ sets the call's one request |
| `Wait`, `Binary` | — | ✓ | — | — |
| `Messages`, `SendInterval`, `SendRepeat`, `Receive`, `ExpectCode` | — | — | — | ✓ |
| `Metadata` | — | — | — | ✓ |
| `Plaintext`, `Connections`, `ProtoFiles`, `ImportPaths`, `DescriptorSet` | — | — | — | ✓ |
| `Events` | — | — | ✓ | — use `Receive` |
| `StreamDuration` | — | — | ✓ | ✓ (per call) |
| `Rate`, `Workers`, `MaxWorkers`, `Duration`, `Requests`, `Stages`, `Iterations` | ✓ | ✓ | ✓ | ✓ |
| `Assert` | ✓ (`Step` too) | ✓ (`Step` is rejected by `Run`) | ✓ (same) | ✓ (same) |
| `Build`, `Run` | ✓ | ✓ | ✓ | ✓ |

Three names are spelled differently from their YAML or struct-field names because the obvious one was
taken: `SendRepeat` (the gRPC call's `repeat`; `Repeat` opens a flow block), `StreamDuration` (an SSE
stream's or gRPC call's `duration`; `Duration` is the length of the load test) and `Events` (SSE) versus
`Receive` (gRPC) for "stop after n messages".

### Worked examples

**Assertions, per-step checks and aborting.** `Assertion` values are the same structs the rest of the
API uses; thresholds are in the metric's own unit (a ratio 0–1 for `success_rate`, requests/second for
`rate`, **seconds** for latency). `Run` returns the `Summary` even when assertions fail, so you can
still print it. See [Per-Step Results](scenarios.md#per-step-results) and
[Stopping a run early](scenarios.md#stopping-a-run-early).

```go
func ptr(v float64) *float64 { return &v }

summary, err := resonate.NewScenario().
	Step("POST", "http://localhost:8080/login").Name("login").
	Extract("token", "json:token").
	Step("GET", "http://localhost:8080/items").Name("browse").
	Header("Authorization", "Bearer {{.Vars.token}}").
	ExpectStatus(200).
	Rate(50).Workers(10).Duration(time.Minute).
	Assert(
		resonate.Assertion{Metric: "success_rate", Min: ptr(0.99)},                  // the whole run
		resonate.Assertion{Metric: "latency_p95", Step: "login", Max: ptr(0.300)},   // one step, 300ms
		resonate.Assertion{Metric: "success_rate", Min: ptr(0.9),                    // stop early if it
			Abort: true, AbortAfter: 10 * time.Second},                          // falls below 90%
	).
	Run(ctx)
summary.Print(os.Stdout) // printed whether or not the assertions held
if summary.Aborted != "" {
	log.Printf("stopped early: %s", summary.Aborted)
}
if err != nil { // failed assertions, an aborted run, or a build error
	log.Fatal(err)
}
```

`AbortAfter` is `0` here (checked from the first second); the CLI defaults it to `5s` — give it a
warm-up in a library too. For the failures individually, call `summary.Evaluate(assertions)` yourself.

**Feeders.** One row per iteration from a CSV/JSON file or inline rows, available as
`{{.Feeder.<column>}}` in every protocol. Modes: `"sequential"` (default), `"random"`, and — for
files too big for memory — `"stream"` (see [Feeders](feeders.md)).

```go
summary, err := resonate.NewScenario().
	Post("http://localhost:8080/login").
	JSONBody(`{"email":"{{.Feeder.email}}","password":"{{.Feeder.password}}"}`).
	Feeder("users.csv", "sequential").
	Requests(1000).Workers(20).
	Run(ctx)

// or rows given in code:
//	FeederRows([]map[string]string{
//		{"email": "alice@example.com", "password": "hunter2"},
//		{"email": "bob@example.com", "password": "hunter3"},
//	}, "random")
```

**Load shapes.** `Rate`/`Workers`/`Duration`/`Requests` are the flat models; `Stages` ramps workers
and/or rate between points (each stage ramps linearly from the previous one's end values, and a
repeated value holds); `MaxWorkers` turns a rate into an open-model arrival rate; `Iterations` bounds how
many iterations each virtual user runs. See [Execution Models](execution-models.md) for what each does.

```go
// Ramp to 50 virtual users over 30s, hold them for 2 minutes.
resonate.NewScenario().
	Get("http://localhost:8080/").
	Stages(
		resonate.Stage{Duration: 30 * time.Second, Workers: 50},
		resonate.Stage{Duration: 2 * time.Minute, Workers: 50},
	).
	Run(ctx)

// Ramp an arrival rate to 200 iterations/s, letting concurrency grow to 500 to sustain it.
resonate.NewScenario().
	Get("http://localhost:8080/").
	Stages(resonate.Stage{Duration: time.Minute, Rate: 200}).
	MaxWorkers(500).
	Run(ctx)

// 10 virtual users, each running exactly 5 iterations: 50 in total.
resonate.NewScenario().Get("http://localhost:8080/").Workers(10).Iterations(5).Run(ctx)
```

**Transport options.** These configure the one HTTP client every worker shares, so they apply to HTTP,
flow and SSE scenarios (and `CAFile`/`ClientCert` to gRPC).

```go
resonate.NewScenario().
	Get("https://api.internal:8443/health").
	CAFile("ca.pem").                       // trust a private CA
	ClientCert("client.pem", "client.key"). // mTLS
	FollowRedirects(false).                 // see the 302, don't follow it
	MaxIdleConns(500).                      // at least your peak concurrency
	MaxResponseBody(1 << 20).               // read at most 1 MiB of each response
	Rate(1000).MaxWorkers(500).Duration(time.Minute).
	Run(ctx)

// h2c: HTTP/2 over plaintext, no TLS. Conflicts with Insecure/CAFile/ClientCert/DisableKeepAlive/HTTP3.
resonate.NewScenario().Get("http://localhost:8080/").H2C().Run(ctx)
```

**gRPC streaming.** The call's kind comes from the schema; the setters just have to suit it.

```go
resonate.NewScenario().
	GRPC("localhost:50051").Plaintext().
	Metadata("authorization", "Bearer {{.Identity.token}}"). // every call
	Identity(map[string]string{"token": "abc123"}).

	Call("acme.feed.v1.Feed/Upload").                 // client streaming
	Messages(`{"id":"{{uuid}}","payload":"x"}`).
	SendRepeat(10).SendInterval(50 * time.Millisecond).

	Call("acme.feed.v1.Feed/Chat").                   // bidirectional
	Messages(`{"text":"hi"}`, `{"text":"bye"}`).
	Receive(2).StreamDuration(5 * time.Second).
	ExpectBody("json:text", "").

	Call("acme.user.v1.UserService/GetUser").         // unary, expecting NOT_FOUND
	Message(`{"id":"missing"}`).
	ExpectCode("NOT_FOUND").

	Workers(10).Duration(30 * time.Second).
	Run(ctx)
```

### What fails before the run starts

Everything below is reported by `Build()` or `Run()` before any request is sent (`Run` builds first, and also
checks the assertions), with the offending call named, rather than showing up as a quietly wrong run:

- mixing protocols (`WS` with `Target`, `SSE` with `GRPC`, ...), or a stray `Get`/`Header` on a WS or gRPC
  scenario;
- a setter that doesn't apply to the protocol (the **—** cells above), whichever order the calls were made in;
- `Message`/`Wait`/`Binary` outside a WebSocket scenario, `Call`-level setters before any `Call`, a second
  `Message` on one gRPC call (use `Messages`), `Extract`/`ExpectBody` on a WS message without `Wait`;
- a step-scoped `Assertion` naming a step that doesn't exist, or on a protocol with no steps;
- an `Abort` assertion on `requests`/`success`, or with a negative `AbortAfter`;
- everything the generators themselves validate: an unparseable URL, `HTTP3` with a non-`https` URL, `H2C`
  with TLS options, a streaming-only field on a unary gRPC call, a missing proto/CA/cert file. For gRPC with no
  `ProtoFiles`/`DescriptorSet`, `Build()` also performs server reflection, so an unreachable server fails here.

## Beyond `Scenario`

`Scenario` covers every protocol, feeders, and assertions, but the
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

The fluent form is shown under [the builder](#the-scenario-builder-dsl) above. The struct
API is the same thing spelled out: an `SSETarget` is one stream's request plus when to
stop reading it; the fields match the [`sse:` scenario block](sse.md).

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

The fluent form is shown under [the builder](#the-scenario-builder-dsl) above. The struct
API is the same thing spelled out: a `GRPCTarget` is a server address plus the
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

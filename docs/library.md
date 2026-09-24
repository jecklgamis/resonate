# Using resonate as a Go library

Everything else in this guide is about the CLI, but the same engine is
also importable directly: `go get github.com/jecklgamis/resonate` and
drive a load test from your own Go program instead of shelling out to the
binary or writing a scenario YAML file.

The root `resonate` package is a thin facade (type aliases + wrapper
functions) over the same `internal/generator`/`internal/engine`/
`internal/report` packages the CLI itself is built on — `internal/cli`
(the Cobra command tree) and `internal/tmpl` (templating internals) stay
unexported. A library consumer builds `HTTPTarget`/`FlowStep`/`WSMessage`
values in Go directly rather than going through flag parsing or YAML.

## The `Scenario` builder DSL

`resonate.NewScenario()` offers a fluent builder as an alternative to the
struct-literal API — chained calls cover request building, response
checks, single or multiple targets, multi-step flows with response
chaining, and every execution model described in
[Execution Models](execution-models.md).

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

`Target(method, url)` adds an *additional* independent target — with more
than one, the generator round-robins through them per iteration, same as
a scenario YAML file's `targets:` list:

```go
summary, err := resonate.NewScenario().
	Target("GET", "http://localhost:8080/a").
	Target("GET", "http://localhost:8080/b").
	Rate(50).Duration(30 * time.Second).
	Run(ctx)
```

`Step(method, url)`/`Setup(method, url)` switch the `Scenario` to build a
multi-step *flow* instead — every step runs in order per iteration, with
`Extract` pulling a value out of a step's response into
`{{.Vars.<name>}}` for later steps, and `Identity` adding a round-robin
identity pool exposed as `{{.Identity.<key>}}`; same as a scenario YAML
file's `flow:`/`setup:`/`identities:`:

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

`Target` and `Step`/`Setup` are mutually exclusive within one `Scenario`
— mixing them is a `Build()`/`Run()` error, since they build two
different generator types (`HTTPGenerator` vs. `FlowGenerator`).

Flow steps also support think-time (pauses) and control flow:
`Pause(d)`/`PauseRange(min, max)` sleep before the current step's
request; `Repeat(n)`, `During(d)`, and `If(cond)` open a control-flow
block — every `Step` added until the matching `End()` runs inside it —
and nest freely:

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
enclosing `Repeat`/`During`/`If` block, same as the YAML `flow:` schema's
control-flow blocks (see [Scenarios](scenarios.md#pauses-and-control-flow)).

Method groups on `Scenario`:

- **Request building** — `Target`, `Step`, `Setup`, `Method`, `Get`,
  `Post`, `Put`, `Patch`, `Delete`, `Header`, `Query`, `Timeout`,
  `Insecure` (`Header`/`Query`/... always apply to "whatever request is
  currently being built" — the last `Target`/`Step`/`Setup` call, or the
  implicit single target if none was made)
- **Request body** — `Body`/`JSONBody` (inline string), `FileBody(path)`
  (templated, sourced from a file), `RawBody(data)`/`RawFileBody(path)`
  (sent exactly as given/read, bypassing templating — for a large/binary
  payload or one containing literal `"{{ }}"`). `Body`/`FileBody` and
  `RawBody`/`RawFileBody` are mutually exclusive on one request; a read
  error from `FileBody`/`RawFileBody` is deferred to `Build()`/`Run()`
  rather than breaking the fluent chain.
- **Base URL** — `BaseURL(url)` prepends `url` to any request URL that
  starts with `/`, so a `Scenario` hitting one host can use relative
  paths instead of repeating `http://host:port` everywhere; a
  `/`-prefixed URL with no `BaseURL` set fails fast at `Build()`/`Run()`.
- **Flow-only** — `Extract` (response chaining, no-op outside a
  `Step`/`Setup`), `Identity` (round-robin identity pool), `Pause`/
  `PauseRange` (think-time before the current step), `Repeat`/`During`/
  `If`/`End` (control-flow blocks)
- **Checks** — `ExpectStatus`, `ExpectHeader`, `ExpectBody` (status code,
  response header, and JSON/YAML/XML/regex/CSS body checks — see
  [Status, Header, and Body Checks](usage.md#status-header-and-body-checks)
  for the check syntax)
- **Execution model** — `Rate`, `Workers`, `MaxWorkers`, `Duration`,
  `Requests`, `Stages(...Stage)`, `Iterations(n)`
- **Terminal calls** — `Build()` returns the underlying `Generator`
  (`*HTTPGenerator` or `*FlowGenerator`) for cases that need it directly;
  `Run(ctx)` builds and runs in one step, returning a `Summary`

`Scenario`'s fields are unexported by design — there's no struct-literal
escape hatch, only the chained builder methods above.

Report writing is a separate, explicit step rather than something
`Run()` does automatically, so embedding a load test in your program
never causes a surprise disk write:

```go
if err := resonate.WriteHTMLFile("report.html", summary.HTMLReport()); err != nil {
	log.Fatal(err)
}
```

## Beyond `Scenario`

For WebSocket, feeders, or assertions (which `Scenario` doesn't cover
yet), use the struct-literal API the DSL is itself built on —
`resonate.NewHTTPGenerator`, `resonate.NewFlowGenerator`,
`resonate.NewWSGenerator`, `resonate.Feeder`, `resonate.Run`,
`resonate.Summary`, `resonate.Assertion`/`Evaluate`, and so on cover the
same ground as `resonate hit`/`resonate run`, just as Go APIs instead of
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
at the repo root for worked examples (a plain HTTP run, a multi-step flow
with response chaining, assertions, and a feeder) — it's written using
only this public package, the same way an external consumer would.

## Examples project

[`examples/dsl-examples`](https://github.com/jecklgamis/resonate/tree/main/examples/dsl-examples)
is a full standalone project (its own `go.mod`, runnable with `go run
.`) with eight commands sharing one module:

| Command | Demonstrates |
| --- | --- |
| `full-scenario` | A POST of a randomized JSON payload with a fresh random `X-Request-Id` header per request, an `ExpectStatus` check, and an HTML report |
| `constant-vus` | Closed, flat — `Workers` alone, no `Rate` |
| `ramping-vus` | Closed, staged — `Stages` with `Rate` left at `0` |
| `constant-arrival-rate` | Open, flat — `Rate` + `Workers` + `MaxWorkers` |
| `ramping-arrival-rate` | Open, staged — `Stages` with `Rate` set per stage, plus a global `MaxWorkers` |
| `bounded-arrival-rate` | Open pacing, closed concurrency cap — same as above but no `MaxWorkers` |
| `per-vu-iterations` | `Iterations` + `Workers`, no `Duration`/`Requests`/`Stages` |
| `shared-iterations` | `Requests(N)` alone — total is fixed regardless of `Workers` |

Named to match the table in [Execution Models](execution-models.md)
exactly, with real captured output from actual runs in its own README.

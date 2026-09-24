# dsl-examples

A standalone Go module demonstrating resonate as an embeddable library
(`import "github.com/jecklgamis/resonate"`) instead of the CLI or a
scenario YAML file — see the repo README's ["Using resonate as a Go
library"](../../README.md#using-resonate-as-a-go-library) section. Eight
commands, sharing this one `go.mod`: one request-building demo
(`full-scenario`) and one per execution model, named to match the table in
the repo README's ["Execution models: open vs.
closed"](../../README.md#execution-models-open-vs-closed) section
exactly.

## `full-scenario`

POSTs a randomized JSON payload to an endpoint you supply, with a fresh
random `X-Request-Id` header on every request (`{{uuid}}`), and checks
the response is actually a `201` rather than any `2xx` (`ExpectStatus`) —
built with `resonate.NewScenario()`'s fluent builder DSL rather than the
struct-literal API:

```go
summary, err := resonate.NewScenario().
	Post(url).
	Header("X-Request-Id", "{{uuid}}").
	JSONBody(`{"id": {{.Seq}}, "item": "{{randChoice "widget" "gadget" "gizmo"}}", "qty": {{randInt 1 10}}}`).
	ExpectStatus(http.StatusCreated).
	Requests(requests).
	Workers(workers).
	Run(ctx)
```

Point it at your own API:

```sh
go run ./full-scenario -url http://localhost:8080/orders
```

Flags: `-url` (default `http://localhost:8080/orders`), `-requests`
(default `10`), `-workers` (default `2`). You'll see the run summary
printed, then a self-contained `report.html` written to `full-scenario/`.

## Execution models

Each of the seven demos below runs one `resonate.NewScenario()` against a
local server with a 50ms artificial delay per request, then prints the
summary. Real output from an actual run of each is included so you know
what to expect — your numbers will vary run to run, but the *shape*
(which fields are zero, which model achieves closer to target, etc.)
should match.

### `constant-vus` — closed, flat

`Workers` alone, no `Rate` at all: a fixed pool of VUs hammering the
target back-to-back as fast as it responds — a classic fixed-concurrency
benchmark. Achieved rate settles near `workers / latency`.

```sh
go run ./constant-vus
```

```
workers:          20
server latency:   50ms
expected rate:    ~400.0 req/s (workers / latency)

requests          1171
achieved rate     390.2 req/s
mean latency      51.400859ms
```

### `ramping-vus` — closed, staged

`Stages` with every `Rate` left at `0`: concurrency ramps 0→20→0 over
three stages, iterations run back-to-back unthrottled at whatever
concurrency each stage currently allows. There's no rate target here, so
`Saturated` never applies (closed model doesn't track it) — but
`PeakConcurrency` is tracked for staged runs regardless of open/closed,
and lands right at the ramp's peak.

```sh
go run ./ramping-vus
```

```
schedule:         ramp 0->20 workers over 1s, hold 20 for 1s, ramp down to 0 over 1s
server latency:   50ms

requests          781
achieved rate     260.3 req/s
mean latency      50.981714ms
peak concurrency  20
```

### `constant-arrival-rate` — open, flat

`Rate` + `Workers` + `MaxWorkers`: iteration starts are paced strictly to
the target rate, concurrency is free to grow up to `MaxWorkers` to sustain
it.

```sh
go run ./constant-arrival-rate
```

```
target rate:      1000 req/s
server latency:   50ms
workers:          20 (max: 200)

requests          5972
achieved rate     1194.3 req/s
mean latency      50.495667ms
saturated         true
peak concurrency  200
```

Note the achieved rate reads *above* target, not exactly on it —
resonate's rate limiter's burst size equals the target rate itself, so
every run gets one rate-value's worth of "free" tokens up front. That
overshoot is a roughly constant absolute amount (shrinks as a fraction of
the total the longer the run goes), not a bug in this example.

### `ramping-arrival-rate` — open, staged

`Stages` with `Rate` set on each, plus a global `MaxWorkers`: both the
target rate *and* the concurrency ceiling ramp, and concurrency is free to
exceed each stage's own `Workers` value (up to `MaxWorkers`) to keep pace.

```sh
go run ./ramping-arrival-rate
```

```
target rate:      500 req/s
server latency:   50ms
schedule:         ramp 0->500 req/s over 1s, then hold 1s
workers per stage: 20 (max: 200)

requests          737
achieved rate     368.2 req/s
mean latency      51.226543ms
saturated         false
peak concurrency  30
```

Peak concurrency (30) exceeding the stage's own `Workers` value (20) is
the tell — `MaxWorkers` let it grow past that per-stage cap. Achieved rate
is averaged over the 1s ramp *and* the 1s hold, so it undershoots the
final target rate; a longer hold phase would push it closer to (and
eventually above, per the burst note) 500 req/s.

### `bounded-arrival-rate` — open pacing, closed concurrency cap

Same `Stages`/target rate as `ramping-arrival-rate`, but with no
`MaxWorkers` set: rate is still paced, but concurrency stays hard-capped
at each stage's own `Workers` value — so throughput can undershoot the
target if `Workers` isn't generous enough.

```sh
go run ./bounded-arrival-rate
```

```
target rate:      500 req/s
server latency:   50ms
schedule:         ramp 0->500 req/s over 1s, then hold 1s (concurrency capped at 20, no MaxWorkers)

requests          581
achieved rate     290.4 req/s
mean latency      50.95421ms
saturated         true
peak concurrency  20
```

Compare directly with `ramping-arrival-rate` above (identical schedule
and target rate): peak concurrency here lands *exactly* at the `Workers`
cap (20, vs. 30 there) with `saturated: true` — direct proof concurrency,
not the rate limiter, was the bottleneck, and the achieved rate is
correspondingly lower.

### `per-vu-iterations`

`Iterations` + `Workers`, no `Duration`/`Requests`/`Stages`: "N virtual
users, each K iterations" — a deterministic total (`workers * iterations`)
that's exact and self-terminating.

```sh
go run ./per-vu-iterations
```

```
workers:          5
iterations/VU:    10
expected total:   50 (workers * iterations, no Duration/Requests set -- self-terminating)

requests          50
success rate      100%
```

### `shared-iterations`

`Requests(N)` alone: "send exactly N requests total, however many workers
it takes" — `Workers` only affects concurrency, not the total.

```sh
go run ./shared-iterations
```

```
requests target:  50
workers:          5 (irrelevant to the total -- just concurrency)

requests          50
success rate      100%
```

## Building everything

```sh
make build            # bin/full-scenario + one binary per execution model
make run-all-models    # run every execution-model demo in sequence
```

or individually, e.g. `make run-constant-vus`. Run `make help` for the
full target list.

## Note on the module path

This example lives inside the resonate repo, so its `go.mod` uses a
`replace` directive pointing at the local checkout (`../..`) instead of a
published version. That's also exactly what you'd do to try out an
unreleased resonate change from a real standalone project:

```
require github.com/jecklgamis/resonate v0.0.0
replace github.com/jecklgamis/resonate => /path/to/your/local/checkout
```

Once resonate has tagged releases, drop the `replace` line and just run
`go get github.com/jecklgamis/resonate@latest`.

# Architecture

A quick map of the codebase for anyone extending resonate.

**Request flow:** three entry points (`resonate hit`, `resonate run`, the
Go library) all build a `generator.Generator`, which `engine.Run` drives
at the configured concurrency/rate, streaming `generator.Result`s into a
`report.Aggregator` → `report.Summary` is printed and written to the
four report formats (see [Reports](reports.md)).

![Three entry points — resonate hit, resonate run, and the Go library — all construct a generator.Generator, which internal/engine drives on a schedule, streaming Results into internal/report, which produces four output formats.](architecture-flow.svg)

- **root package (`resonate`)** — a thin public facade over
  `generator`/`engine`/`report` (type aliases + wrapper functions), for
  embedding the engine in your own program. `dsl.go`'s `Scenario` is the
  exception — a fluent builder with its own state (current
  request/message, a control-flow block stack) layered on top, covering
  HTTP targets, flows, and WebSocket. See [Library](library.md) and
  `examples/dsl-examples`.
- **`internal/generator`** — protocol-agnostic `Generator` interface plus
  `HTTPGenerator` (independent requests), `FlowGenerator` (multi-step,
  response chaining, per-worker identities), and `WSGenerator` (WebSocket).
- **`internal/tmpl`** — per-request templating (`{{ }}`) for URLs, query
  params, headers, bodies. See [Request Templating](usage.md#request-templating).
- **`internal/engine`** — worker pool + rate limiter driving a
  `Generator` (flat, open-model, or staged ramp), streaming results; also
  bounds/rotates each VU's lifetime (`load.iterations`). See
  [Execution Models](execution-models.md).
- **`internal/report`** — aggregates results into latency percentiles,
  success rate, throughput, status/error breakdowns; `assert.go`
  evaluates `assertions:` against a completed `Summary`.
- **`internal/config`** — YAML scenario schema and loader. See
  [Scenarios](scenarios.md).
- **`internal/cli`** — Cobra commands (`hit`, `run`).
- **`cmd/gendocs`** — regenerates the [CLI Reference](cli/resonate.md)
  from the cobra command tree.

For the deeper design rationale (why `runStaged` doesn't use
`rate.Limiter.SetLimitAt`, the validation philosophy, per-package
testing patterns), see
[`CLAUDE.md`](https://github.com/jecklgamis/resonate/blob/main/CLAUDE.md)
in the repo.

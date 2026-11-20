# CLAUDE.md

Guidance for Claude Code (claude.ai/code) in this repository. This file is deliberately short: rules, commands and gotchas only. The long-form design rationale and bug history live in [`docs/internals.md`](docs/internals.md) — read the relevant section there before changing a subsystem it covers.

## What this is

`resonate` is a Go CLI load generator (HTTP incl. h2c/HTTP/3, WebSocket, gRPC, Server-Sent Events, MCP), currently beta. Module path `github.com/jecklgamis/resonate`; the same engine is importable as a library via the root `resonate` package.

- `resonate hit <url>` — one-off, flag-driven, **HTTP only**.
- `resonate run <scenario.yaml>` — reusable multi-target/multi-step scenarios; the only way to use WS, gRPC, SSE and MCP.
- `resonate compare baseline.json report.json --fail-on 'latency_p95>10%'` — regression gate between two JSON reports.
- A `scenarios:` section in a scenario file (or `resonate.NewSuite()` in Go) runs several workloads concurrently, each with its own load profile — see `docs/multiple-scenarios.md`.

**Stable contract:** the YAML schema, `report.json`/`results.jsonl` fields and exit codes (`docs/reports.md#stability`). `internal/cli/contract_test.go` guards it — don't break it casually.

## Layout

Request flow: `cli` parses flags/YAML → builds a `generator.Generator` → `engine.Run` drives it at the configured concurrency/rate → results stream into `report.Aggregator` → `report.Summary` printed (text or `--json`).

| Path | Role |
|---|---|
| root package (`doc.go`, `generator.go`, `engine.go`, `report.go`) | Public API: type aliases / one-line wrappers over `internal/*`. New exported internals are **not** public until added here — deliberate friction. |
| `dsl.go`, `dsl_protocols.go` | Fluent `Scenario` builder over the facade; covers every protocol. Parity rule below. |
| `internal/generator` | `Generator` interface (`Do(ctx, vuID) []Result`); HTTP, flow, WS, gRPC, SSE, MCP generators, feeders, extraction, compression, proto bodies. |
| `internal/engine` | `Run` → `runFlat` (closed), `runOpenModel` (constant arrival rate), `runStaged` (ramps); VU lifecycle; abort-on-threshold. `multi.go`: `RunMulti` runs several generators concurrently, each with its own `Options`. |
| `internal/report` | Aggregator (HDR histograms), per-step breakdown, assertions, `compare`, text/JSON/HTML output. |
| `internal/config` | YAML scenario schema + `Load`. `scenarios.go`: the ordered `scenarios:` section and `Resolve` (shared-section merge). |
| `internal/cli` | Cobra commands (`NewRootCommand`), `runAndReport`, assertion parsing. `run_multi.go`: the same for a file with `scenarios:`. |
| `internal/tmpl` | `{{ }}` templating. |
| `cmd/gendocs` | Generates `docs/cli/*.md`. |
| `examples/test*server` | Demo gRPC/SSE/MCP/HTTP3 servers + the shipped example scenarios' tests. |

## Conventions

@.claude/conventions/code-style.md
@.claude/conventions/testing.md
@.claude/conventions/workflow.md

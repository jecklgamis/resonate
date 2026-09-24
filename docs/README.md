# resonate

A load generator for HTTP and WebSocket: `resonate hit <url>` for a
quick, flag-driven one-off run against a single URL, and
`resonate run <scenario.yaml>` for reusable, multi-target/multi-step
scenarios.

> **Alpha**: APIs, flags, and the YAML schema may change without notice.

This site covers **installing and using** resonate. For building/testing
the project itself, see
[CONTRIBUTING.md](https://github.com/jecklgamis/resonate/blob/main/CONTRIBUTING.md);
for the repo's top-level overview, see its
[README](https://github.com/jecklgamis/resonate#readme).

## Features

| Feature | Details |
| --- | --- |
| Two entry points | `resonate hit <url>` — flag-driven, single-URL, quick runs; `resonate run <scenario.yaml>` — reusable, multi-target/multi-step/WebSocket |
| Execution models | Closed model (fixed concurrency) and open model (constant-arrival-rate, auto-scaling concurrency to sustain a target rate) — flat or ramped via staged schedules |
| Multi-step flows | Response chaining (`{{.Vars.*}}`), per-virtual-user identities, think-time/control flow (`pause`, `repeat`, `during`, `if`) |
| Request templating | `{{ }}` expressions for unique IDs, random payloads, fresh auth headers |
| Data feeders | CSV/JSON feeders (with a streaming mode for files too large to fit in memory) for driving requests from real data |
| Base URL & file bodies | `base_url` to avoid repeating a host per target; templated or raw file-sourced request bodies for large/binary payloads |
| Response checks | Per-request `expect_status`/`expect_headers`/`expect_body`, matched via JSONPath, XPath, regex, or CSS selectors, overriding the default 2xx/3xx success criterion |
| WebSocket support | Connect, send/receive a message sequence, chain extracted values the same way HTTP flows do |
| Assertions / CI gating | Non-zero exit on a failed threshold — `resonate hit --assert 'metric<op>threshold'` or a scenario file's `assertions:` (richer condition set: `gt`/`lt`/`is`/`in`/`around`/`deviatesAround`) |
| Reporting | Four formats every run by default: stdout summary, self-contained HTML, JSON, raw per-request JSON Lines dump |
| HTTP transport controls | mTLS, custom CA trust, h2c (HTTP/2 cleartext), keep-alive control |
| Go library | Usable as `go get github.com/jecklgamis/resonate` — thin public facade over the CLI's engine, plus a fluent `Scenario` builder DSL |

## Quick Start

### 1. Install

```sh
go build -o bin/resonate ./cmd/resonate
```

Or download a prebuilt binary — see [Install](install.md).

### 2. Send some load

```sh
resonate hit http://localhost:8080/health --rate 50 --duration 30s --workers 20
```

### 3. Look at the results

Every run writes `report.html`, `report.json`, and `results.jsonl` to the
current directory in addition to the stdout summary — open `report.html`
in a browser for KPI cards, a requests-over-time chart, and a latency
distribution histogram.

See [Usage](usage.md) for the full quickstart (including a scenario file
for multi-target/multi-step/WebSocket runs), and [Reports](reports.md) for
what each of the four report formats contains.

## Where to go next

- **[Install](install.md)** — prebuilt binaries, build from source, shell
  completion.
- **[Usage](usage.md)** — `resonate hit` vs. `resonate run`, request
  templating, status code checks, live progress, dry runs.
- **[Scenarios](scenarios.md)** — the YAML schema: targets, feeders, flows,
  identities, WebSocket, assertions.
- **[Execution Models](execution-models.md)** — workers, rate, open vs.
  closed model, ramp profiles, virtual user lifecycle.
- **[Reports](reports.md)** — the stdout summary, HTML report, JSON
  report, and raw results dump.
- **[Library](library.md)** — using resonate as a Go library, the
  `Scenario` builder DSL, and the `examples/dsl-examples` project.
- **[Architecture](architecture.md)** — a map of the codebase for anyone
  extending resonate.
- **[CLI Reference](cli/resonate.md)** — every flag, generated straight
  from the command tree so it can't drift from the code.

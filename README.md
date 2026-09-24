# resonate

[![CI](https://github.com/jecklgamis/resonate/actions/workflows/ci.yml/badge.svg)](https://github.com/jecklgamis/resonate/actions/workflows/ci.yml)

A load generator for HTTP and WebSocket.

> **Heads up**: this project is under active development and in alpha. APIs, flags, and the YAML schema may change without notice. Use at your own risk.

Two entry points: `resonate hit <url>` for quick, flag-driven one-off runs
against a single URL, and `resonate run <scenario.yaml>` for reusable,
multi-target/multi-step/WebSocket scenarios described in YAML. The same
engine is also usable directly as a Go library.

**For the full guide — install, usage, the scenario YAML schema, execution
models, reports, the Go library, and a generated CLI flag reference — see
[`docs/`](docs)**, browsable as plain Markdown here on GitHub or served
locally with `docsify serve docs`. This README is just a quick overview
and pointers into that guide.

## Install

```sh
go build -o bin/resonate ./cmd/resonate
```

Or download a prebuilt binary for Linux, macOS, or Windows from the
[releases page](https://github.com/jecklgamis/resonate/releases). See
[docs/install.md](docs/install.md) for shell completion and checking the
installed version.

## Quick start

```sh
resonate hit http://localhost:8080/health --rate 50 --duration 30s --workers 20
```

For multi-target or reusable setups, describe the run in YAML instead:

```sh
cat > scenario.yaml << 'EOF'
protocol: http

load:
  duration: 30s
  rate: 50
  workers: 20

http:
  timeout: 5s
  targets:
    - method: POST
      url: http://localhost:8080/users
      headers:
        Content-Type: application/json
      body: '{"id": "{{uuid}}", "name": "{{randChoice "alice" "bob" "carol"}}", "age": {{randInt 18 65}}}'

assertions:
  - metric: latency_p95
    max: 500ms
  - metric: success_rate
    min: "0.95"
EOF

resonate run scenario.yaml --json
```

Every run writes a stdout summary plus three files (`report.html`,
`report.json`, `results.jsonl`) by default — see
[docs/reports.md](docs/reports.md).

See [docs/usage.md](docs/usage.md) for `resonate hit` vs. `resonate run`,
request templating, and status/header/body checks;
[docs/scenarios.md](docs/scenarios.md) for the full YAML schema (targets,
flows, feeders, identities, WebSocket, assertions);
[docs/execution-models.md](docs/execution-models.md) for workers/rate,
open vs. closed model, and ramp profiles; and
[`docs/cli`](docs/cli/resonate.md) for the generated, always-in-sync flag
reference for every command.

## Using resonate as a Go library

The same engine is importable directly — `go get
github.com/jecklgamis/resonate` — instead of shelling out to the binary
or writing a scenario YAML file. `resonate.NewScenario()` offers a fluent
builder DSL: single or multiple targets, multi-step flows with response
chaining, status/header/body checks, and every execution model, via
chained calls:

```go
import "github.com/jecklgamis/resonate"

summary, err := resonate.NewScenario().
	Get("http://localhost:8080/health").
	Rate(50).Workers(20).Duration(30 * time.Second).
	Run(ctx)
if err != nil {
	log.Fatal(err)
}
summary.Print(os.Stdout)
```

A struct-literal API (`resonate.NewHTTPGenerator`, `resonate.Run`, ...)
is also available underneath the DSL for cases it doesn't cover yet
(WebSocket, feeders, assertions). See [docs/library.md](docs/library.md)
for the full API of both, and
[`dsl.go`](dsl.go)/[`dsl_test.go`](dsl_test.go)/[`resonate_test.go`](resonate_test.go)
at the repo root for the implementation and worked examples. For a full
standalone project demonstrating every execution model, see
[`examples/dsl-examples`](examples/dsl-examples).

## Architecture

See [docs/architecture.md](docs/architecture.md) for a map of the
codebase (package responsibilities, request flow) for anyone extending
resonate.

## Feedback

Found a bug or have a feature request? Please open an issue on
[GitHub](https://github.com/jecklgamis/resonate/issues). Want to contribute
a fix or a feature? See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE).

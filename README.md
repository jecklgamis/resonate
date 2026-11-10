# resonate

[![CI](https://github.com/jecklgamis/resonate/actions/workflows/ci.yml/badge.svg)](https://github.com/jecklgamis/resonate/actions/workflows/ci.yml)

A load generator for HTTP (including HTTP/3), WebSocket, Server-Sent Events and gRPC: `resonate hit <url>`
for quick, flag-driven one-off runs, `resonate run <scenario.yaml>` for
reusable multi-target/multi-step/WebSocket/SSE/gRPC scenarios, and the same
engine as a Go library.

> **Beta**: the scenario YAML schema, the `report.json` / `results.jsonl` formats and the exit codes are stable and versioned (see [Stability](docs/reports.md#stability)). Flags and features may still be added, and the Go library API may still change before 1.0.
>
> A scenario file can read local files and environment variables
> (`body_file`, `feeder`, `env "VAR_NAME"`) and send them wherever it's
> configured to — treat it as trusted input, like a shell script. See
> [SECURITY.md](SECURITY.md) for reporting a vulnerability.

**See the [full guide](https://jecklgamis.github.io/resonate/)** —
install, usage, the scenario YAML schema, execution models, reports, the
Go library, and a generated CLI flag reference. Also browsable as plain
Markdown in [`docs/`](docs), or served locally with `docsify serve docs`.

## Install

```sh
brew install jecklgamis/tap/resonate
```

Or build from source:

```sh
go build -o bin/resonate ./cmd/resonate
```

Or download a prebuilt binary from the
[releases page](https://github.com/jecklgamis/resonate/releases). See
[docs/install.md](docs/install.md) for details.

## Quick start

POST a random JSON payload on every request (`{{uuid}}`, `{{randChoice ...}}` and
`{{randInt ...}}` are re-rendered per request), and fail the run with a non-zero exit code
if p95 latency or the success rate misses its threshold:

```sh
resonate hit http://localhost:8080/users \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"id": "{{uuid}}", "name": "{{randChoice "alice" "bob" "carol"}}", "age": {{randInt 18 65}}}' \
  --rate 50 --duration 30s --workers 20 --timeout 5s \
  --assert 'latency_p95<=500ms' \
  --assert 'success_rate>=0.95'
```

For a reusable, checked-in test, describe the same run in a scenario file and use
`resonate run`:

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

resonate run scenario.yaml
```

Every run writes a stdout summary plus `report.html`/`report.json`/
`results.jsonl` by default. For multi-target/multi-step/WebSocket/SSE/gRPC
scenarios in YAML, see [docs/usage.md](docs/usage.md) and
[docs/scenarios.md](docs/scenarios.md); for gRPC (unary and streaming), the
[gRPC guide](docs/grpc.md); for Server-Sent Events, the [SSE guide](docs/sse.md); for HTTP/3, the
[HTTP/3 guide](docs/http3.md); to run several workloads at once, each with its own load profile, [multiple scenarios](docs/multiple-scenarios.md).

## Go library

The same run as the quick start's `resonate hit` command and scenario file, written with
the Go DSL: it POSTs the same random JSON payload and applies the same two assertions.
Save it as `main.go` in a new directory:

```go
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jecklgamis/resonate"
)

func main() {
	p95, minSuccess := 0.5, 0.95 // latency_p95 is in seconds; success_rate is a 0-1 ratio

	summary, err := resonate.NewScenario().
		Post("http://localhost:8080/users").
		JSONBody(`{"id": "{{uuid}}", "name": "{{randChoice "alice" "bob" "carol"}}", "age": {{randInt 18 65}}}`).
		Timeout(5*time.Second).
		Rate(50).Workers(20).Duration(30*time.Second).
		Assert(
			resonate.Assertion{Metric: "latency_p95", Max: &p95},
			resonate.Assertion{Metric: "success_rate", Min: &minSuccess},
		).
		Run(context.Background())
	summary.Print(os.Stdout) // the report is valid even when an assertion fails
	if err != nil {
		log.Fatal(err)
	}
}
```

Then run it:

```sh
go mod init loadtest   # once, in the new directory
go mod tidy            # fetches github.com/jecklgamis/resonate
go run .
```

A failed assertion makes `Run` return an error (after the report is printed), which
`log.Fatal` turns into a non-zero exit, like `resonate run`.

`go get github.com/jecklgamis/resonate` — see
[docs/library.md](docs/library.md) for the full `Scenario` DSL and
struct-literal API.

## More

- [docs/architecture.md](docs/architecture.md) — codebase map for anyone
  extending resonate
- [Issues](https://github.com/jecklgamis/resonate/issues) — bugs and
  feature requests
- [CONTRIBUTING.md](CONTRIBUTING.md) — how to contribute
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) — community standards
- [SECURITY.md](SECURITY.md) — reporting a vulnerability
- [LICENSE](LICENSE) — Apache License 2.0

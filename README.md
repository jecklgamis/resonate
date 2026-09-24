# resonate

[![CI](https://github.com/jecklgamis/resonate/actions/workflows/ci.yml/badge.svg)](https://github.com/jecklgamis/resonate/actions/workflows/ci.yml)

A load generator for HTTP and WebSocket: `resonate hit <url>` for
quick, flag-driven one-off runs, `resonate run <scenario.yaml>` for
reusable multi-target/multi-step/WebSocket scenarios, and the same
engine as a Go library.

> **Heads up**: this project is under active development and in alpha. APIs, flags, and the YAML schema may change without notice. Use at your own risk.
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
go build -o bin/resonate ./cmd/resonate
```

Or download a prebuilt binary from the
[releases page](https://github.com/jecklgamis/resonate/releases). See
[docs/install.md](docs/install.md) for details.

## Quick start

```sh
resonate hit http://localhost:8080/health --rate 50 --duration 30s --workers 20
```

Every run writes a stdout summary plus `report.html`/`report.json`/
`results.jsonl` by default. For multi-target/multi-step/WebSocket
scenarios in YAML, see [docs/usage.md](docs/usage.md) and
[docs/scenarios.md](docs/scenarios.md).

## Go library

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

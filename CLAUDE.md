# CLAUDE.md

Guidance for Claude Code (claude.ai/code) in this repository. This file is deliberately short: rules, commands and gotchas only. The long-form design rationale and bug history live in [`docs/internals.md`](docs/internals.md) — read the relevant section there before changing a subsystem it covers.

## What this is

`resonate` is a Go CLI load generator (HTTP incl. h2c/HTTP/3, WebSocket, gRPC, Server-Sent Events), currently beta. Module path `github.com/jecklgamis/resonate`; the same engine is importable as a library via the root `resonate` package.

- `resonate hit <url>` — one-off, flag-driven, **HTTP only**.
- `resonate run <scenario.yaml>` — reusable multi-target/multi-step scenarios; the only way to use WS, gRPC and SSE.
- `resonate compare baseline.json report.json --fail-on 'latency_p95>10%'` — regression gate between two JSON reports.

**Stable contract:** the YAML schema, `report.json`/`results.jsonl` fields and exit codes (`docs/reports.md#stability`). `internal/cli/contract_test.go` guards it — don't break it casually.

## Commands

```sh
make build            # go build -o bin/resonate ./cmd/resonate
make test             # go test ./...
make check            # fmt-check + vet + test — run before considering work done
make fmt              # gofmt -w .
make docs             # regenerate docs/cli/*.md (cmd/gendocs) — never hand-edit docs/cli
make docs-check       # fail if docs/cli is stale — run after adding/renaming/removing a flag
go test ./... -race   # ALWAYS when touching internal/engine or internal/generator
go test ./internal/engine/... -run TestScheduleAtRampsLinearly -v   # single test
go run ./cmd/resonate hit http://localhost:8080/health --duration 5s
make example-grpc-server   # :50051 (ARGS=-reflection=false)
make example-sse-server    # :8765
make example-http3-server  # :8443 UDP+TCP (ARGS=-h3-only)
```

No linter beyond `go vet`/`gofmt`. `go.mod` pins `go 1.27.1`. CI runs `make check` + `make docs-check` on push/PR to `main`; `release.yml` cross-builds (`CGO_ENABLED=0`) and publishes on any `v*` tag.

## Layout

Request flow: `cli` parses flags/YAML → builds a `generator.Generator` → `engine.Run` drives it at the configured concurrency/rate → results stream into `report.Aggregator` → `report.Summary` printed (text or `--json`).

| Path | Role |
|---|---|
| root package (`doc.go`, `generator.go`, `engine.go`, `report.go`) | Public API: type aliases / one-line wrappers over `internal/*`. New exported internals are **not** public until added here — deliberate friction. |
| `dsl.go`, `dsl_protocols.go` | Fluent `Scenario` builder over the facade; covers every protocol. Parity rule below. |
| `internal/generator` | `Generator` interface (`Do(ctx, vuID) []Result`); HTTP, flow, WS, gRPC, SSE generators, feeders, extraction, compression, proto bodies. |
| `internal/engine` | `Run` → `runFlat` (closed), `runOpenModel` (constant arrival rate), `runStaged` (ramps); VU lifecycle; abort-on-threshold. |
| `internal/report` | Aggregator (HDR histograms), per-step breakdown, assertions, `compare`, text/JSON/HTML output. |
| `internal/config` | YAML scenario schema + `Load`. |
| `internal/cli` | Cobra commands (`NewRootCommand`), `runAndReport`, assertion parsing. |
| `internal/tmpl` | `{{ }}` templating. |
| `cmd/gendocs` | Generates `docs/cli/*.md`. |
| `examples/test*server` | Demo gRPC/SSE/HTTP3 servers + the shipped example scenarios' tests. |

## Rules that hold for every protocol

Each was learned from a real bug (details in `docs/internals.md`):

- **Anything that can fail instantly with zero network I/O must be validated at construction or bounded** — otherwise a worker with no `--rate` free-spins at millions of iterations/sec. Examples: `validateLiteralURL`, HTTP/3's `requireHTTPSForHTTP3`, gRPC's `WaitForReady(true)`, and "still open at `timeout` is a failure" for streams with no `events`/`duration`/`receive`.
- **Fail fast at construction** for any bad config that would otherwise silently no-op. Same for builder setters that don't apply to the scenario's protocol (`Build()` error, not ignored).
- **A call or stream cut off by the run's own end is dropped, not reported** — `runEnded(ctx)` must check `ctx.Err()` **and** the deadline against the clock. (Plain HTTP/flow still reports end-of-run `context deadline exceeded`; pre-existing, changing it alters behavior.)
- **Streams are reported by their parts** (WS, SSE, gRPC streaming): a connect `Result` plus one per message/event, latency = gap since the previous; `requests` counts messages. Results return only when the stream ends, hence the bounds above.
- **Opt-in transports never fall back** (HTTP/3). Tests must assert what the *server* observed, not just that the call succeeded.
- **`resonate hit` exits 0 even if every request fails** unless `--assert` is given. Probe with `curl` or add an assertion when scripting.
- **Reports must never carry secrets** (they get emailed): the Run settings table uses `safeURL`; never list header values, bodies, identities or Kafka credentials. Known gap: the Errors table shows Go's `url.Error` text, which includes the query string.

## Gotchas worth knowing before editing

- **Templating (`internal/tmpl`):** fields with no `{{` skip templating (fast path — don't break it); keep `Option("missingkey=zero")` (removing it renders `<no value>`); funcs that can fail return `(string, error)` and surface as per-request errors, never panics.
- **Extraction (`extract.go`):** real JSONPath/XPath/regex/CSS. `json:items[0].id` bracket syntax is required; the old dot-index form no longer works.
- **HTTP transport:** one shared `*http.Client` per generator; connection pooling is Go's. `MaxIdleConnsPerHost` (`--max-idle-conns`, default 100) is a measured footgun at high concurrency — raise it with `max_workers`. Default TCP transport is HTTP/1.1 (custom TLS config disables auto-HTTP/2).
- **Feeders:** `Close()` may only be called after every `Next()` caller has stopped (true structurally after `engine.Run` returns) — otherwise a stream/Kafka `Next()` can block forever. `Next()` is nil-receiver-safe. `"stream"` mode validates the whole file at construction.
- **Engine:** `runStaged` deliberately avoids `rate.Limiter.SetLimitAt` (credit-ticker instead — read the comment block before changing). VU rotation uses a shared atomic `nextVU`, not a per-slot stride. `AbortOn` evaluates `Snapshot`, not `Summary()`, and discards results after the trip. CLI defaults `abort_after` to 5s; the library default is 0.
- **Builder (`dsl.go`):** `s.active`/`s.stack` hold pointers to slice *fields*, never raw elements (append reallocation). Fallible setters use `deferErr` and surface at `Build()`. **Parity rule:** anything a protocol's YAML/struct API can express must be reachable from `Scenario`; adding an `Options` field means adding the setter, and `docs/library.md`'s applicability table must be regenerated by probe when applicability changes.
- **Report:** HTML is deliberately script-free (hover is pure CSS; `.hit` must be immediately followed by its `.tip`, and tips emitted after all bars). Fixed percentiles are p50/p75/p90/p95/p99 — keep text, HTML, JSON and assertion names in step when adding one. `compare` can only gate the fixed percentiles; an unevaluable gate is an error, never a pass.
- **gRPC:** construction may do network I/O (reflection). Schema source priority: `proto_files` > `descriptor_set` > reflection. `Result.StatusCode` is the HTTP-mapped code; the real gRPC code is in `Error`.
- **Response/request compression and protobuf-over-HTTP:** see `docs/internals.md`; protobuf bodies are not yet in the fluent builder.

## Docs and examples

- `docs/` is the source of truth for user-facing behavior — keep it current and don't duplicate it. Root `README.md` stays a short overview that links into `docs/`; add detail to the relevant `docs/*.md`.
- Each protocol with depth has **one** guide (`grpc.md`, `sse.md`, `http3.md`, `feeders.md`/`kafka-feeder.md`); `scenarios.md` just points to it.
- Every complete ```yaml scenario in a guide is tested: strict-decoded with `KnownFields(true)` (because `config.Load` silently ignores unknown keys) and run through the real `cli.NewRootCommand` against the example server with only the host rewritten. So full scenarios must target the example server's real endpoints; a long-running illustration is exempted from *running* by an invisible `<!-- docs-test: parse-only -->` line above the fence.
- Example servers: stdlib/dynamic-message only (no codegen), `call`/`done` access-style logging, `-log-requests=false` + periodic summary for load tests, never log credentials, default port avoids 8080 (SSE uses 8765).
- **Shipping a new example server** = `make example-<proto>-server` target + guide section + a test running the shipped examples and guide against it + a line in `docs/architecture.md`'s table.

## Adding a protocol

generator → facade alias in root `generator.go` → config struct + a link in the `allOf` protocol chain and a `$defs` entry in `scenario.schema.json` (extend the generated list; don't hand-edit nested `if/else`) → `buildGenerator` case in `internal/cli/run.go` (and the protocol list in its unsupported-protocol error) → builder support in `dsl_protocols.go` → tests at every layer → guide + sidebar + cross-links → update this file. The JSON Schema has no automated test: validate with a draft 2020-12 validator against every `examples/*.yaml` and full `docs/` scenario, plus negative cases.

## Testing patterns (follow these, don't invent new ones)

- **Root package:** `resonate_test.go` is `package resonate_test` and imports only the public package — the facade's contract test. If it breaks without touching the four facade files, the public surface moved unintentionally.
- **`internal/generator`:** `httptest.Server` handlers that record requests (`recordingServer`/`jsonHandler`); `newTestCA` for TLS/mTLS; set `ConnState` on `httptest.NewUnstartedServer` *before* `Start()`; gRPC tests use in-process servers (grpc-go `health`, hand-registered `dynamicpb` services) — no generated code; Kafka tests use `kfake`.
- **`internal/engine`:** `fakeGenerator` drives `Run` directly — no network.
- **`internal/cli`:** `captureStdout`/`captureStderr` redirect the real fds (`runAndReport` writes to them directly); keep per-file helpers distinct (e.g. `grpcRunJSON`) to avoid collisions.
- **Example servers:** their tests run the *shipped* example YAMLs and doc scenarios through the real CLI, asserting exact request counts; they're in the root module so `make check` covers them.
- **Mutation-check tests that guard a race, leak or ordering rule** (delete the fix, confirm the test fails) — several here passed vacuously until checked.

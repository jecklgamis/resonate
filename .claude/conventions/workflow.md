# Workflow

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
make example-mcp-server    # :8766 (ARGS=-sse for event-stream replies, ARGS=-session-ttl=10s)
make example-python-mcp-server  # :8767, official Python SDK, needs uv (ARGS=--json-response / --stateless)
make test-mcp-interop      # opt-in: resonate vs that server in all four variants (needs uv + network)
make example-http3-server  # :8443 UDP+TCP (ARGS=-h3-only)
```

No linter beyond `go vet`/`gofmt`. `go.mod` pins the Go version (the toolchain may auto-update the nested `examples/dsl-examples` module — don't commit that). CI runs `make check` + `make docs-check` on push/PR to `main`; `release.yml` cross-builds (`CGO_ENABLED=0`) and publishes on any `v*` tag.

## Adding a protocol

generator → facade alias in root `generator.go` → config struct + a link in the `allOf` protocol chain and a `$defs` entry in `scenario.schema.json` (extend the generated list; don't hand-edit nested `if/else`) → `buildGenerator` case in `internal/cli/run.go` (and the protocol list in its unsupported-protocol error) → builder support in `dsl_protocols.go` → tests at every layer → guide + sidebar + cross-links → update this file. The JSON Schema has no automated test: validate with a draft 2020-12 validator against every `examples/*.yaml` and full `docs/` scenario, plus negative cases.

## Docs and examples

- `docs/` is the source of truth for user-facing behavior — keep it current and don't duplicate it. Root `README.md` stays a short overview that links into `docs/`; add detail to the relevant `docs/*.md`.
- Each protocol with depth has **one** guide (`grpc.md`, `sse.md`, `mcp.md`, `http3.md`, `feeders.md`/`kafka-feeder.md`); `scenarios.md` just points to it.
- Every complete ```yaml scenario in a guide is tested: strict-decoded with `KnownFields(true)` (because `config.Load` silently ignores unknown keys) and run through the real `cli.NewRootCommand` against the example server with only the host rewritten. So full scenarios must target the example server's real endpoints; a long-running illustration is exempted from *running* by an invisible `<!-- docs-test: parse-only -->` line above the fence.
- Example servers: stdlib/dynamic-message only (no codegen), `call`/`done` access-style logging, `-log-requests=false` + periodic summary for load tests, never log credentials, default port avoids 8080 (SSE uses 8765, MCP 8766).
- **Shipping a new example server** = `make example-<proto>-server` target + guide section + a test running the shipped examples and guide against it + a line in `docs/architecture.md`'s table.

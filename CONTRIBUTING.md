# Contributing to resonate

Thanks for taking the time to contribute. resonate is in alpha, so expect
some churn — but bug reports, fixes, and small focused improvements are
welcome. Participation is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
Found a security vulnerability instead of a regular bug? See
[SECURITY.md](SECURITY.md) — please don't open a public issue for it.

## Before you start

For anything beyond a small fix (new flags, new YAML fields, a new
execution mode, etc.), please open an issue first to discuss the approach.
It's a lot easier to align on design before code is written than after.

## Development setup

Install Go, the version declared in `go.mod`, from the official
[Go downloads page](https://go.dev/dl/). A guide for Mac and Vscode is here:

```sh
brew install go
go version # verify the installation
```

In VS Code, press `Cmd+Shift+X` to open Extensions, search for `Go`, and
install **Go** published by **Go Team at Google** (`golang.go`).

Install the required Go tools from VS Code: press `Cmd+Shift+P`, run
`Go: Install/Update Tools`, select `gopls` and `dlv`, then confirm the
installation.

Clone the repository, download its Go module dependencies, and run the checks:

```sh
git clone https://github.com/jecklgamis/resonate.git
cd resonate
go mod download
make build          # bin/resonate
make test           # go test ./...
make check          # fmt-check + vet + test — run this before opening a PR
```

Other useful targets: `make fmt` (gofmt), `make run ARGS='http https://example.com --duration 5s'`.
See `make help` for the full list.

When touching `internal/engine` or `internal/generator` (both use goroutines,
atomics, and `sync.Map`), always run the race detector:

```sh
go test ./... -race
```

`make coverage` runs the full suite with `-race` and a coverage profile,
printing the total at the end; CI runs it on every push/PR and uploads
`coverage.out` as a build artifact. There's no enforced minimum threshold —
use it to spot untested branches in whatever you're changing, not as a gate.

## Code style

- No linter beyond `go vet`/`gofmt` — keep it that way, don't introduce one
  without discussing it first.
- Default to no comments; only add one when the *why* isn't obvious from
  the code (a hidden constraint, a workaround for a specific bug, a subtle
  invariant).
- Don't add abstractions, config knobs, or error handling for cases that
  can't happen — see `CLAUDE.md`'s validation philosophy for where the line
  is (fail fast on bad config, don't add defensive code for internal
  invariants).
- Follow the existing testing patterns rather than introducing new ones —
  see `CLAUDE.md`'s "Testing patterns already in use" for what each package
  already does (`httptest.Server` in `internal/generator`, a `fakeGenerator` in
  `internal/engine`, stdout/stderr capture in `internal/cli`).

`CLAUDE.md` has the full architecture rundown if you want more context
before diving in.

## Docs

`docs/` (the docsify guide) is the source of truth for user-facing
behavior (flags, YAML schema, templating functions, concurrency model).
If your change affects any of that, update the relevant `docs/*.md` page
in the same PR — don't let it drift. `README.md` is deliberately a short
overview that links into `docs/`; it shouldn't grow detailed reference
content back into it.

`docs/cli/*.md` is generated from the cobra command tree (`make docs`), so
it can't drift on its own — but if you add/remove/rename a flag, regenerate
it and commit the result. CI's `make docs-check` fails the build if you
forget.

Every package has a doc comment (`// Package foo ...` above `package foo`) —
keep it accurate when a package's role changes. Read them with
`go doc ./internal/engine` (always works, no setup) or `make godoc`
(installs and runs `pkgsite` for a pkg.go.dev-style local UI — a nicer
browsing experience, but depending on your `pkgsite` version/environment it
can be less reliable than plain `go doc`).

## Submitting a change

1. Fork the repo and create a branch off `main`.
2. Make your change, with tests. `make check` must pass, and CI
   (`.github/workflows/ci.yml`) runs the same checks on your PR.
3. Keep PRs focused — one change per PR is easier to review than several
   bundled together.
4. Open a PR against `main` describing what changed and why.

## Reporting bugs / requesting features

Open an issue on [GitHub](https://github.com/jecklgamis/resonate/issues).
For a bug, include the command/scenario file that reproduces it, what you
expected, and what happened instead.

## License

By contributing, you agree your contributions will be licensed under the
project's [Apache License 2.0](LICENSE).

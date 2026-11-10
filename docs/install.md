# Install

## Homebrew (macOS and Linux)

```sh
brew install jecklgamis/tap/resonate
```

The formula lives in
[jecklgamis/homebrew-tap](https://github.com/jecklgamis/homebrew-tap) and is
updated automatically on every release. Upgrade with `brew upgrade resonate`.

## Using a Prebuilt Binary

Download a release for your platform (Linux, macOS, or Windows;
amd64/arm64) from
[GitHub Releases](https://github.com/jecklgamis/resonate/releases). Each
release archive includes the `resonate` binary plus `README.md` and
`LICENSE`.

## Building from Source

Requires Go (see [`go.mod`](https://github.com/jecklgamis/resonate/blob/main/go.mod)
for the pinned version).

```sh
git clone https://github.com/jecklgamis/resonate.git
cd resonate
go build -o bin/resonate ./cmd/resonate
```

`make build` does the same thing; `make install` installs it onto
`$GOBIN`/`$GOPATH/bin` instead. See
[CONTRIBUTING.md](https://github.com/jecklgamis/resonate/blob/main/CONTRIBUTING.md)
for the full dev workflow (tests, `-race`, doc regeneration).

## Shell Completion

Completion scripts for bash, zsh, fish, and PowerShell are built in — no
extra setup needed beyond loading the script for your shell:

```sh
# zsh, current session only
source <(resonate completion zsh)

# zsh, persisted (macOS/Homebrew)
resonate completion zsh > $(brew --prefix)/share/zsh/site-functions/_resonate

# bash, current session only
source <(resonate completion bash)
```

Run `resonate completion <shell> --help` for the full install instructions
for your shell.

## Checking the Version

```sh
resonate --version
```

Reports `dev` for a local/`go install` build, or the release tag (e.g.
`v1.0.0-beta.2`) for a downloaded binary.

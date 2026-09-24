package main

import (
	"fmt"
	"os"

	"github.com/jecklgamis/resonate/internal/cli"
)

// version is overridden at build time via
// -ldflags "-X main.version=vX.Y.Z" (see .github/workflows/release.yml).
var version = "dev"

func main() {
	if err := cli.Execute(version); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

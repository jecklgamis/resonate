// Package cli wires up resonate's command-line interface.
package cli

import (
	"github.com/spf13/cobra"
)

// NewRootCommand builds the resonate command tree without executing it —
// split out from Execute so cmd/gendocs can generate reference docs from it
// without spawning a real CLI invocation.
func NewRootCommand(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "resonate",
		Short:         "resonate is a load generator",
		Long:          "resonate drives configurable load against HTTP and WebSocket targets.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newHitCommand())
	root.AddCommand(newRunCommand())

	return root
}

// Execute runs the resonate CLI. version is reported by --version; it's
// "dev" for local/`go install` builds and set via -ldflags for releases
// (see .github/workflows/release.yml).
func Execute(version string) error {
	return NewRootCommand(version).Execute()
}

// Package cli wires up resonate's command-line interface.
package cli

import (
	"github.com/spf13/cobra"
)

func NewRootCommand(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "resonate",
		Short: "Load test HTTP and WebSocket services",
		Long: "resonate drives configurable load against HTTP and WebSocket targets.\n\n" +
			"Quick start:\n" +
			"  resonate hit https://example.com --duration 1s\n" +
			"  resonate run scenario.yaml",
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

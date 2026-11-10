// Package cli wires up resonate's command-line interface.
package cli

import (
	"github.com/spf13/cobra"
)

func NewRootCommand(version string) *cobra.Command {
	buildVersion = version
	root := &cobra.Command{
		Use:   "resonate",
		Short: "Load test HTTP, WebSocket, gRPC and SSE services",
		Long: "resonate drives configurable load against HTTP, WebSocket, gRPC and Server-Sent Events targets.\n\n" +
			"Quick start:\n" +
			"  resonate hit https://example.com --duration 2s\n" +
			"  resonate run scenario.yaml",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// An unknown or malformed flag is a usage error (exit code 2), like a
	// wrong number of arguments.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })

	root.AddCommand(newHitCommand())
	root.AddCommand(newRunCommand())
	root.AddCommand(newCompareCommand())
	root.AddCommand(newSchemaCommand())

	return root
}

// Execute runs the resonate CLI. version is reported by --version; it's
// "dev" for local/`go install` builds and set via -ldflags for releases
// (see .github/workflows/release.yml).
func Execute(version string) error {
	return NewRootCommand(version).Execute()
}

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
		Use:   "resonate",
		Short: "Load test HTTP and WebSocket services",
		Long: "resonate drives configurable load against HTTP and WebSocket targets.\n\n" +
			"Quick start:\n" +
			"  resonate hit https://example.com --duration 10s\n" +
			"  resonate run scenario.yaml\n\n" +
			"Use 'hit' for a one-off HTTP test with request, rate, TLS, templating,\n" +
			"response-check, and report options. Use 'run' for reusable YAML scenarios,\n" +
			"including multi-step flows and WebSocket tests. Add --dry-run to send one\n" +
			"real request or iteration; use --help on a command to see all its options.",
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

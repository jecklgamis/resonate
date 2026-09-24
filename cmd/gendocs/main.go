// Command gendocs regenerates docs/cli/*.md from the cobra command tree
// (flags, descriptions, examples) so the CLI reference can never drift from
// the actual flag definitions. Run via `make docs`; CI's docs-check job
// fails the build if the committed output is stale.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/jecklgamis/resonate/internal/cli"
)

const outDir = "docs/cli"

func main() {
	// "dev" here: the reference docs describe flags/usage, not a specific
	// build's version, and a real version would make every regeneration
	// diff spuriously.
	root := cli.NewRootCommand("dev")
	disableAutoGenTag(root)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
	if err := doc.GenMarkdownTree(root, outDir); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

// disableAutoGenTag strips cobra's "Auto generated on <date>" footer, which
// would otherwise make every regeneration produce a diff even with no
// actual command/flag changes — defeating the point of a docs-are-stale CI
// check.
func disableAutoGenTag(cmd *cobra.Command) {
	cmd.DisableAutoGenTag = true
	for _, c := range cmd.Commands() {
		disableAutoGenTag(c)
	}
}

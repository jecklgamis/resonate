package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jecklgamis/resonate/internal/config"
)

func newSchemaCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for scenario YAML",
		Long:  "Print the complete JSON Schema for scenario YAML to stdout, for validating and authoring resonate scenarios.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), config.ScenarioSchema())
			return err
		},
	}
}

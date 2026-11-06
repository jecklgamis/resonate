package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jecklgamis/resonate/internal/report"
)

func newCompareCommand() *cobra.Command {
	var failOn []string

	cmd := &cobra.Command{
		Use:   "compare <baseline.json> <current.json>",
		Short: "Compare two JSON reports and fail on a regression",
		Long: `Compare two runs' JSON reports (the report.json each 'run'/'hit' writes, or
--json output) and print every headline metric side by side, then each
named step's metrics when both runs have it.

Add --fail-on to gate a pipeline: the command exits non-zero when any rule
matches. A rule is <metric><op><threshold> over the change from baseline to
current — a threshold ending in % is relative to the baseline, otherwise it is
an absolute change in the metric's unit (a duration for latency metrics, a plain
number for the rest; success_rate and error_rate are ratios from 0 to 1). A
step's metric is written <step>:<metric>.`,
		Example: `  # p95 more than 10% slower, or throughput down more than 5%
  resonate compare baseline.json report.json --fail-on 'latency_p95>10%' --fail-on 'rate<-5%'

  # error rate up by more than one point; one step's p99 more than 20ms slower
  resonate compare baseline.json report.json --fail-on 'error_rate>0.01' --fail-on 'login:latency_p99>20ms'`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("expected a baseline and a current report, got %d argument(s); try '%s --help'", len(args), cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			gates := make([]report.Gate, 0, len(failOn))
			for _, expr := range failOn {
				g, err := report.ParseGate(expr)
				if err != nil {
					return fmt.Errorf("--fail-on %w", err)
				}
				gates = append(gates, g)
			}

			base, err := readSummaryFile(args[0])
			if err != nil {
				return err
			}
			cur, err := readSummaryFile(args[1])
			if err != nil {
				return err
			}
			for _, side := range []struct {
				label string
				s     report.Summary
			}{{"baseline", base}, {"current", cur}} {
				if side.s.Aborted != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: the %s run was aborted early (%s), so its numbers cover only part of a run\n", side.label, side.s.Aborted)
				}
			}

			out := cmd.OutOrStdout()
			report.WriteComparison(out, report.Compare(base, cur))
			if len(gates) == 0 {
				return nil
			}

			fmt.Fprintln(out)
			failed := 0
			for _, g := range gates {
				res, err := g.Check(base, cur)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, res)
				if res.Failed {
					failed++
				}
			}
			if failed > 0 {
				cmd.SilenceUsage = true
				return fmt.Errorf("%d of %d regression rule(s) matched", failed, len(gates))
			}
			return nil
		},
	}

	cmd.Flags().StringArrayVar(&failOn, "fail-on", nil, "Exit non-zero if a metric's change from baseline matches, e.g. 'latency_p95>10%' or 'login:latency_p99>20ms' (repeatable)")
	return cmd
}

func readSummaryFile(path string) (report.Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return report.Summary{}, err
	}
	defer f.Close()
	s, err := report.ReadSummary(f)
	if err != nil {
		return report.Summary{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

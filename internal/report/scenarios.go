package report

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"time"
)

// ScenarioSummary is one named scenario's results in a multi-scenario run. It
// embeds that scenario's own Summary, so in report.json each entry of
// "scenarios" has exactly the same fields as the top level.
type ScenarioSummary struct {
	Name string `json:"name"`

	// StartAfter is how long after the run began this scenario was scheduled
	// to start.
	StartAfter time.Duration `json:"start_after,omitempty"`

	Summary
}

// scenario returns the named scenario's summary.
func (s Summary) scenario(name string) (Summary, bool) {
	for _, sc := range s.Scenarios {
		if sc.Name == name {
			return sc.Summary, true
		}
	}
	return Summary{}, false
}

// scenarioNames lists the scenarios in the order they were configured.
func (s Summary) scenarioNames() []string {
	names := make([]string, len(s.Scenarios))
	for i, sc := range s.Scenarios {
		names[i] = sc.Name
	}
	return names
}

// printScenarios writes each scenario's own report after the combined one.
func (s Summary) printScenarios(w io.Writer) {
	for _, sc := range s.Scenarios {
		fmt.Fprintf(w, "\nScenario: %s", sc.Name)
		if sc.StartAfter > 0 {
			fmt.Fprintf(w, " (started after %s)", sc.StartAfter)
		}
		fmt.Fprintln(w)
		sc.Summary.Print(w)
	}
}

// ValidateScenarios rejects an assertion scoped to a scenario that doesn't
// exist, so a typo fails before the run instead of as "recorded no requests"
// after it.
func ValidateScenarios(assertions []Assertion, known []string) error {
	for i, a := range assertions {
		if a.Scenario == "" {
			continue
		}
		if !slices.Contains(known, a.Scenario) {
			return fmt.Errorf("assertions[%d] (%s): unknown scenario %q (scenarios: %s)", i, a.Metric, a.Scenario, nameList(known))
		}
	}
	return nil
}

// nameList renders names for an error message: "none" or a sorted list.
func nameList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	sorted := slices.Clone(names)
	sort.Strings(sorted)
	return fmt.Sprint(sorted)
}

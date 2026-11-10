package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/generator"
)

// scenarioRun builds a Summary the way the engine does — through Aggregators —
// with two scenarios whose numbers are far enough apart that an assertion
// checked against the wrong one gives the wrong answer.
func scenarioRun(t *testing.T) Summary {
	t.Helper()
	build := func(step string, latency time.Duration, ok, bad int) Summary {
		agg := NewAggregator()
		now := time.Now()
		for i := 0; i < ok; i++ {
			agg.Add(generator.Result{Timestamp: now, Latency: latency, StatusCode: 200, Step: step})
		}
		for i := 0; i < bad; i++ {
			agg.Add(generator.Result{Timestamp: now, Latency: latency, StatusCode: 500, Step: step})
		}
		return agg.Summary(time.Second)
	}
	fast := build("home", 10*time.Millisecond, 100, 0) // 100% ok, 10ms
	slow := build("pay", 800*time.Millisecond, 50, 50) // 50% ok, 800ms

	all := NewAggregator()
	now := time.Now()
	for i := 0; i < 100; i++ {
		all.Add(generator.Result{Timestamp: now, Latency: 10 * time.Millisecond, StatusCode: 200, Step: "home"})
	}
	for i := 0; i < 100; i++ {
		code := 200
		if i >= 50 {
			code = 500
		}
		all.Add(generator.Result{Timestamp: now, Latency: 800 * time.Millisecond, StatusCode: code, Step: "pay"})
	}
	s := all.Summary(time.Second)
	s.Scenarios = []ScenarioSummary{
		{Name: "browse", Summary: fast},
		{Name: "checkout", StartAfter: 2 * time.Second, Summary: slow},
	}
	return s
}

func TestScenarioScopedAssertionUsesThatScenariosNumbers(t *testing.T) {
	s := scenarioRun(t)

	// browse is healthy, checkout is not; the combined run is in between.
	failures, err := s.Evaluate([]Assertion{
		{Metric: "success_rate", Scenario: "browse", Min: f64(0.99)},   // passes: browse is 100%
		{Metric: "success_rate", Scenario: "checkout", Min: f64(0.99)}, // fails: checkout is 50%
		{Metric: "success_rate", Min: f64(0.99)},                       // fails: combined is 75%
		{Metric: "latency_p95", Scenario: "browse", Max: f64(0.05)},    // passes: 10ms
		{Metric: "latency_p95", Scenario: "checkout", Max: f64(0.05)},  // fails: 800ms
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range failures {
		got = append(got, assertionLabel(f.Metric, f.Scenario, f.Step))
	}
	want := []string{"success_rate{scenario=checkout}", "success_rate", "latency_p95{scenario=checkout}"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("failed assertions = %v, want %v", got, want)
	}
}

func TestScenarioAndStepScopeCombine(t *testing.T) {
	s := scenarioRun(t)
	// "pay" only exists in checkout; "home" only in browse.
	failures, err := s.Evaluate([]Assertion{
		{Metric: "success_rate", Scenario: "checkout", Step: "pay", Min: f64(0.9)},
		{Metric: "success_rate", Scenario: "browse", Step: "home", Min: f64(0.9)},
		{Metric: "success_rate", Scenario: "browse", Step: "pay", Min: f64(0.9)}, // pay did not run in browse
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %+v, want 2 (pay in checkout is 50%%; pay never ran in browse)", failures)
	}
	if failures[0].Scenario != "checkout" || failures[0].Step != "pay" {
		t.Errorf("first failure = %+v", failures[0])
	}
	if r := failures[1].Reasons[0]; !strings.Contains(r, `step "pay" recorded no requests`) || !strings.Contains(r, "home") {
		t.Errorf("second failure reason = %q, want the step-missing message listing browse's steps", r)
	}
	if got := failures[0].String(); !strings.HasPrefix(got, "success_rate{scenario=checkout,step=pay}: ") {
		t.Errorf("String() = %q", got)
	}
}

func TestAssertionOnAScenarioThatIsNotThereFailsInsteadOfPassing(t *testing.T) {
	s := scenarioRun(t)
	failures, err := s.Evaluate([]Assertion{{Metric: "success_rate", Scenario: "ghost", Min: f64(0)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 || !strings.Contains(failures[0].Reasons[0], `scenario "ghost" recorded no requests`) {
		t.Fatalf("failures = %+v", failures)
	}
	if !strings.Contains(failures[0].Reasons[0], "browse, checkout") {
		t.Errorf("reason %q should list the scenarios that exist", failures[0].Reasons[0])
	}
	// Partial evaluation (the abort check) skips it rather than failing.
	if f, _ := s.EvaluatePartial([]Assertion{{Metric: "success_rate", Scenario: "ghost", Min: f64(1)}}); len(f) != 0 {
		t.Errorf("EvaluatePartial reported %+v for a scenario that has not started", f)
	}
}

func TestScenarioScopedUnknownMetricIsStillAnError(t *testing.T) {
	s := scenarioRun(t)
	if _, err := s.Evaluate([]Assertion{{Metric: "nope", Scenario: "ghost", Min: f64(0)}}); err == nil {
		t.Error("an unknown metric must be an error even when the scenario is also missing")
	}
	if _, err := s.Evaluate([]Assertion{{Metric: "nope", Scenario: "browse", Min: f64(0)}}); err == nil {
		t.Error("an unknown metric must be an error for a real scenario too")
	}
}

func TestValidateScenarios(t *testing.T) {
	as := []Assertion{{Metric: "rate"}, {Metric: "rate", Scenario: "a"}, {Metric: "rate", Scenario: "typo"}}
	err := ValidateScenarios(as, []string{"b", "a"})
	if err == nil || !strings.Contains(err.Error(), `assertions[2]`) || !strings.Contains(err.Error(), `unknown scenario "typo"`) || !strings.Contains(err.Error(), "[a b]") {
		t.Errorf("error = %v, want assertions[2] naming the typo and listing the real scenarios sorted", err)
	}
	if err := ValidateScenarios(as[:2], []string{"a"}); err != nil {
		t.Errorf("valid assertions rejected: %v", err)
	}
}

func TestValidateScopedSteps(t *testing.T) {
	lookup := func(scenario string) ([]string, bool) {
		switch scenario {
		case "web":
			return []string{"home", "pay"}, true
		case "rpc":
			return nil, false // a protocol that names no steps
		default: // the combined run
			return []string{"home", "pay", "extra"}, true
		}
	}
	tests := []struct {
		name string
		a    Assertion
		want string // "" = valid
	}{
		{"step in its scenario", Assertion{Metric: "rate", Scenario: "web", Step: "pay"}, ""},
		{"combined step", Assertion{Metric: "rate", Step: "extra"}, ""},
		{"no step at all", Assertion{Metric: "rate", Scenario: "rpc"}, ""},
		{"step not in that scenario", Assertion{Metric: "rate", Scenario: "web", Step: "extra"}, `unknown step "extra"`},
		{"step on a protocol without steps", Assertion{Metric: "rate", Scenario: "rpc", Step: "x"}, "only apply to http scenarios"},
		{"unknown step in combined run", Assertion{Metric: "rate", Step: "nope"}, `unknown step "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateScopedSteps([]Assertion{{Metric: "x"}, tt.a}, lookup)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("error = %v, want %q", err, tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), "assertions[1]"):
				t.Errorf("error %v should point at assertions[1] (its place in the full list)", err)
			}
		})
	}
}

func TestSummaryJSONCarriesScenariosAndRoundTrips(t *testing.T) {
	s := scenarioRun(t)
	var buf bytes.Buffer
	if err := s.PrintJSON(&buf); err != nil {
		t.Fatal(err)
	}

	// Each scenario has the same fields as the top level, flat, plus name.
	var raw struct {
		Requests  int `json:"requests"`
		Scenarios []map[string]json.RawMessage
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Scenarios) != 2 {
		t.Fatalf("scenarios in JSON = %d", len(raw.Scenarios))
	}
	for _, key := range []string{"name", "requests", "success_rate", "latencies", "steps", "duration"} {
		if _, ok := raw.Scenarios[1][key]; !ok {
			t.Errorf("scenario entry has no %q field: %v", key, keys(raw.Scenarios[1]))
		}
	}
	if string(raw.Scenarios[1]["start_after"]) != "2000000000" {
		t.Errorf("start_after = %s, want 2000000000 (ns, like every duration in the report)", raw.Scenarios[1]["start_after"])
	}

	back, err := ReadSummary(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Scenarios) != 2 || back.Scenarios[1].Name != "checkout" || back.Scenarios[1].Requests != 100 {
		t.Errorf("round trip lost the scenarios: %+v", back.Scenarios)
	}
	if back.Scenarios[1].StartAfter != 2*time.Second {
		t.Errorf("start_after round-tripped as %s", back.Scenarios[1].StartAfter)
	}
}

func keys(m map[string]json.RawMessage) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// A report without scenarios must not change shape at all: no "scenarios" key.
func TestSingleScenarioJSONHasNoScenariosKey(t *testing.T) {
	agg := NewAggregator()
	agg.Add(generator.Result{Timestamp: time.Now(), Latency: time.Millisecond, StatusCode: 200})
	var buf bytes.Buffer
	if err := agg.Summary(time.Second).PrintJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "scenarios") {
		t.Errorf("a single-scenario report mentions scenarios:\n%s", buf.String())
	}
}

func TestPrintListsEachScenarioAfterTheCombinedReport(t *testing.T) {
	var buf bytes.Buffer
	scenarioRun(t).Print(&buf)
	out := buf.String()

	combined := strings.Index(out, "Requests      [total, rate]            200")
	browse := strings.Index(out, "Scenario: browse\n")
	checkout := strings.Index(out, "Scenario: checkout (started after 2s)\n")
	if combined < 0 || browse < 0 || checkout < 0 || !(combined < browse && browse < checkout) {
		t.Fatalf("want combined, then browse, then checkout; got offsets %d %d %d in:\n%s", combined, browse, checkout, out)
	}
	if !strings.Contains(out[browse:checkout], "Requests      [total, rate]            100") {
		t.Errorf("browse section lacks its own request count:\n%s", out[browse:checkout])
	}
}

func TestPrintOmitsScenarioSectionsForASingleRun(t *testing.T) {
	var buf bytes.Buffer
	Summary{Requests: 1}.Print(&buf)
	if strings.Contains(buf.String(), "Scenario") {
		t.Errorf("single-run output mentions a scenario:\n%s", buf.String())
	}
}

// The combined line says only which scenario saturated; a combined peak would
// be made up, so the warning appears under the scenario that has it.
func TestPrintPutsTheSaturationWarningUnderItsScenario(t *testing.T) {
	s := scenarioRun(t)
	s.Saturated = true
	s.Scenarios[1].Saturated = true
	s.Scenarios[1].PeakConcurrency = 17
	var buf bytes.Buffer
	s.Print(&buf)
	out := buf.String()
	if n := strings.Count(out, "target rate could not always be sustained"); n != 1 {
		t.Fatalf("saturation warning printed %d times, want once:\n%s", n, out)
	}
	if strings.Index(out, "peak concurrency hit 17") < strings.Index(out, "Scenario: checkout") {
		t.Errorf("the warning should sit in checkout's section:\n%s", out)
	}
}

func TestWriteHTMLListsScenariosAndStaysScriptFree(t *testing.T) {
	s := scenarioRun(t)
	s.Scenarios[0].Aborted = "scenario \"browse\": success_rate: got 0.00"
	as := []Assertion{{Metric: "success_rate", Scenario: "checkout", Min: f64(0.99)}, {Metric: "success_rate", Min: f64(0.5)}}
	failures, err := s.Evaluate(as)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteHTML(&buf, HTMLReport{Title: "t", Summary: s, Assertions: as, Failures: failures}); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	if strings.Contains(html, "<script") {
		t.Error("the HTML report must stay script-free")
	}
	for _, want := range []string{"<h2>Scenarios</h2>", "browse", "checkout", ">2s<", "ABORTED"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	// The scenario-scoped assertion is labelled with its scenario and shown as failed,
	// while the run-wide one with the same metric passes: labels must not collide.
	if !strings.Contains(html, "success_rate{scenario=checkout}") {
		t.Error("HTML lacks the scenario-scoped assertion's label")
	}
	if strings.Count(html, `badge fail">FAIL`) != 1 || strings.Count(html, `badge pass">PASS`) != 1 {
		t.Errorf("want exactly one FAIL (checkout) and one PASS (combined) badge; got %d FAIL, %d PASS",
			strings.Count(html, `badge fail">FAIL`), strings.Count(html, `badge pass">PASS`))
	}
}

func TestWriteHTMLWithoutScenariosHasNoScenariosSection(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteHTML(&buf, HTMLReport{Title: "t", Summary: Summary{Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "<h2>Scenarios</h2>") {
		t.Error("a single-scenario report grew a Scenarios section")
	}
}

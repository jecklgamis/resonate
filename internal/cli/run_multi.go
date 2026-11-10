package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/engine"
	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

// builtScenario is one scenario of a multi-scenario file once built: the
// engine's view of it, plus what the CLI needs to report on it.
type builtScenario struct {
	spec engine.Spec
	cfg  config.Scenario
}

// runMultiFile runs a file that defines `scenarios:`: every scenario is built
// up front (so any bad one fails before a single request is sent), then all
// run concurrently, each with its own load profile, and one combined report —
// with a section per scenario — is written. out carries the same report and
// output options a single-scenario run takes; out.Assertions is parsed by the
// caller and may be scoped to a scenario.
func runMultiFile(path string, file *config.Scenario, out runOpts) error {
	resolved, err := file.Resolve()
	if err != nil {
		return usageError(err)
	}
	names := file.Scenarios.Names()
	if err := report.ValidateScenarios(out.Assertions, names); err != nil {
		return err
	}
	for i, a := range out.Assertions {
		if a.Abort && a.Scenario == "" {
			return fmt.Errorf("assertions[%d] (%s): abort needs a scenario: in a file with scenarios: — a run-wide assertion can only be checked at the end of the run", i, a.Metric)
		}
	}

	built := make([]builtScenario, 0, len(resolved))
	closeAll := func() {
		for _, b := range built {
			b.spec.Generator.Close()
		}
	}
	for _, r := range resolved {
		opts, err := loadOptionsFrom(r.Config.Load)
		if err != nil {
			closeAll()
			return fmt.Errorf("scenario %q: %w", r.Name, err)
		}
		// A scenario's own engine run sees its abort rules unscoped: the
		// scenario is the whole world to it. RunMulti adds the name back.
		for _, a := range abortRules(out.Assertions) {
			if a.Scenario == r.Name {
				a.Scenario = ""
				opts.AbortOn = append(opts.AbortOn, a)
			}
		}
		g, err := buildGenerator(&r.Config)
		if err != nil {
			closeAll()
			return fmt.Errorf("scenario %q: %w", r.Name, err)
		}
		built = append(built, builtScenario{
			spec: engine.Spec{Name: r.Name, Generator: g, Options: opts, StartAfter: r.StartAfter},
			cfg:  r.Config,
		})
	}
	defer closeAll()

	specs := make([]engine.Spec, len(built))
	for i, b := range built {
		specs[i] = b.spec
	}
	if err := report.ValidateScopedSteps(out.Assertions, engine.StepLookup(specs)); err != nil {
		return err
	}
	if err := engine.ValidateMulti(specs); err != nil {
		return err
	}

	out.Settings = multiScenarioSettings(path, built)
	if out.Title == "" {
		out.Title = path
	}
	return runAndReportMulti(specs, built, out)
}

// runAndReportMulti is runAndReport for several scenarios: the same progress
// line, results file, console report, assertion check and report files, with
// the per-scenario sections added.
func runAndReportMulti(specs []engine.Spec, built []builtScenario, out runOpts) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if out.DryRun {
		return dryRunMulti(ctx, specs, out.JSON)
	}

	var onResult []func(string, generator.Result)
	if !out.Quiet {
		p := newProgressPrinter()
		onResult = append(onResult, func(_ string, r generator.Result) { p.onResult(r) })
		stopProgress := p.start()
		defer stopProgress()
	}
	var rw *resultsWriter
	if out.ResultsFile != "" {
		var err error
		rw, err = newResultsWriter(out.ResultsFile)
		if err != nil {
			return fmt.Errorf("opening results file: %w", err)
		}
		onResult = append(onResult, rw.onResultFor)
	}
	var mo engine.MultiOptions
	if len(onResult) > 0 {
		mo.OnResult = func(scenario string, r generator.Result) {
			for _, f := range onResult {
				f(scenario, r)
			}
		}
	}

	started := time.Now()
	summary, err := engine.RunMulti(ctx, specs, mo)
	finished := time.Now()
	if err != nil {
		if rw != nil {
			rw.Close()
		}
		return err
	}

	// RunMulti has returned, so every OnResult call is done: flush the file
	// before anything else can fail, as runAndReport does.
	if rw != nil {
		if err := rw.Close(); err != nil {
			return fmt.Errorf("writing results file: %w", err)
		}
	}

	for i, sc := range summary.Scenarios {
		if w := concurrencyWarning(specs[i].Options, sc.Summary); w != "" {
			fmt.Fprintf(os.Stderr, "scenario %q: %s\n", sc.Name, w)
		}
	}

	if out.JSON {
		if err := summary.PrintJSON(os.Stdout); err != nil {
			return err
		}
	} else {
		summary.Print(os.Stdout)
	}

	failures, err := summary.Evaluate(out.Assertions)
	if err != nil {
		return err
	}

	if out.HTMLReport != "" {
		settings := append(append([]report.Setting{}, out.Settings...), environmentSettings()...)
		const layout = "2006-01-02 15:04:05 MST"
		settings = append(settings,
			report.Setting{Name: "Started", Value: started.Format(layout)},
			report.Setting{Name: "Finished", Value: finished.Format(layout)})
		if err := writeHTMLReport(out.HTMLReport, out.Title, summary, out.Assertions, failures, settings); err != nil {
			return fmt.Errorf("writing html report: %w", err)
		}
	}
	if out.JSONReport != "" {
		if err := writeJSONReport(out.JSONReport, summary); err != nil {
			return fmt.Errorf("writing json report: %w", err)
		}
	}

	failErr := reportAssertionFailures(failures, len(out.Assertions))
	if summary.Aborted != "" {
		return assertionError(fmt.Errorf("run aborted: %s", summary.Aborted))
	}
	return failErr
}

// dryRunMulti sends one real iteration of every scenario, one after another
// (there is no load to overlap), and prints each one's result(s).
func dryRunMulti(ctx context.Context, specs []engine.Spec, jsonOut bool) error {
	if jsonOut {
		all := make(map[string][]generator.Result, len(specs))
		for _, sp := range specs {
			all[sp.Name] = sp.Generator.Do(ctx, 0)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(all)
	}
	for _, sp := range specs {
		fmt.Printf("Scenario: %s\n", sp.Name)
		printDryRunResults(sp.Generator.Do(ctx, 0))
	}
	return nil
}

// multiScenarioSettings lists, for the HTML report's Run settings table, what
// each scenario tests and how its load is shaped — the single-scenario rows,
// once per scenario with the scenario's name in front. The same rule applies
// as there: nothing that could carry a secret.
func multiScenarioSettings(path string, built []builtScenario) []report.Setting {
	var l settingList
	l = append(l, scenarioFileSetting(path))
	names := make([]string, len(built))
	for i, b := range built {
		names[i] = b.spec.Name
	}
	l.add("Scenarios", fmt.Sprint(len(names))+": "+strings.Join(names, ", "))

	for _, b := range built {
		rows := scenarioSettings(path, &b.cfg)[1:] // [0] is the file row, shown once above
		rows = append(rows, runSettings(b.spec.Options)...)
		if b.spec.StartAfter > 0 {
			rows = append(rows, report.Setting{Name: "Starts after", Value: b.spec.StartAfter.String()})
		}
		for _, r := range rows {
			l = append(l, report.Setting{Name: b.spec.Name + ": " + r.Name, Value: r.Value})
		}
	}
	return l
}

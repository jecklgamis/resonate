package cli

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/engine"
	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

func newRunCommand() *cobra.Command {
	var jsonOut, quiet, dryRun bool
	var htmlReport, jsonReport, resultsFile string

	cmd := &cobra.Command{
		Use:   "run <scenario.yaml>",
		Short: "Run a load test scenario from a config file",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("expected one scenario file, got %d; try '%s --help' or '%s run scenario.yaml'", len(args), cmd.CommandPath(), cmd.Root().CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			scenario, err := config.Load(args[0])
			if err != nil {
				return err
			}

			opts, err := loadOptionsFrom(scenario.Load)
			if err != nil {
				return err
			}
			if err := engine.Validate(opts); err != nil {
				return err
			}

			a, err := buildGenerator(scenario)
			if err != nil {
				return err
			}

			assertions, err := assertionsFrom(scenario.Assertions)
			if err != nil {
				return err
			}

			return runAndReport(a, opts, runOpts{JSON: jsonOut, Quiet: quiet, DryRun: dryRun, Assertions: assertions, HTMLReport: htmlReport, JSONReport: jsonReport, ResultsFile: resultsFile, Title: args[0]})
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the report as JSON")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress the periodic progress line on stderr")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Send exactly one real iteration and print its result(s), instead of running the full load test")
	cmd.Flags().StringVar(&htmlReport, "html-report", "report.html", "Write a self-contained HTML report to this path (\"\" disables it)")
	cmd.Flags().StringVar(&jsonReport, "json-report", "report.json", "Write the JSON report to this path (\"\" disables it; independent of --json, which controls stdout)")
	cmd.Flags().StringVar(&resultsFile, "results-file", "results.jsonl", "Write one JSON object per individual request (JSON Lines) to this path as results complete (\"\" disables it)")
	return cmd
}

func assertionsFrom(cfgs []config.AssertionConfig) ([]report.Assertion, error) {
	assertions := make([]report.Assertion, 0, len(cfgs))
	for i, c := range cfgs {
		if c.Metric == "" {
			return nil, fmt.Errorf("assertions[%d]: metric is required (one of %v)", i, report.AssertionMetrics)
		}
		if !report.IsKnownAssertionMetric(c.Metric) {
			return nil, fmt.Errorf("assertions[%d]: unknown metric %q (expected one of %v, or \"latency_p<N>\" for any percentile N 0-100)", i, c.Metric, report.AssertionMetrics)
		}

		a, set, err := assertionFrom(i, c)
		if err != nil {
			return nil, err
		}
		if set == 0 {
			return nil, fmt.Errorf("assertions[%d] (%s): at least one condition is required (min/max/gt/lt/is/in/around+around_margin/deviates_around+deviates_percent)", i, c.Metric)
		}
		assertions = append(assertions, a)
	}
	return assertions, nil
}

// assertionFrom parses one config.AssertionConfig's threshold fields into a
// report.Assertion, returning how many conditions were set (0 is an error
// the caller reports, so every field's parse error can be surfaced first).
func assertionFrom(i int, c config.AssertionConfig) (report.Assertion, int, error) {
	a := report.Assertion{Metric: c.Metric, AroundExclusive: c.AroundExclusive, DeviatesExclusive: c.DeviatesExclusive}
	set := 0

	parse := func(field, value string) (*float64, error) {
		if value == "" {
			return nil, nil
		}
		v, err := report.ParseThreshold(c.Metric, value)
		if err != nil {
			return nil, fmt.Errorf("assertions[%d] (%s): %s: %w", i, c.Metric, field, err)
		}
		return &v, nil
	}

	var err error
	if a.Min, err = parse("min", c.Min); err != nil {
		return a, 0, err
	}
	if a.Max, err = parse("max", c.Max); err != nil {
		return a, 0, err
	}
	if a.GT, err = parse("gt", c.GT); err != nil {
		return a, 0, err
	}
	if a.LT, err = parse("lt", c.LT); err != nil {
		return a, 0, err
	}
	if a.Is, err = parse("is", c.Is); err != nil {
		return a, 0, err
	}
	if a.Around, err = parse("around", c.Around); err != nil {
		return a, 0, err
	}
	if a.AroundMargin, err = parse("around_margin", c.AroundMargin); err != nil {
		return a, 0, err
	}
	if (a.Around == nil) != (a.AroundMargin == nil) {
		return a, 0, fmt.Errorf("assertions[%d] (%s): around and around_margin must be set together", i, c.Metric)
	}
	if a.DeviatesAround, err = parse("deviates_around", c.DeviatesAround); err != nil {
		return a, 0, err
	}
	if c.DeviatesPercent != "" {
		v, perr := strconv.ParseFloat(c.DeviatesPercent, 64)
		if perr != nil {
			return a, 0, fmt.Errorf("assertions[%d] (%s): deviates_percent: invalid number %q", i, c.Metric, c.DeviatesPercent)
		}
		a.DeviatesPercent = &v
	}
	if (a.DeviatesAround == nil) != (a.DeviatesPercent == nil) {
		return a, 0, fmt.Errorf("assertions[%d] (%s): deviates_around and deviates_percent must be set together", i, c.Metric)
	}
	if len(c.In) > 0 {
		a.In = make([]float64, len(c.In))
		for j, s := range c.In {
			v, err := report.ParseThreshold(c.Metric, s)
			if err != nil {
				return a, 0, fmt.Errorf("assertions[%d] (%s): in[%d]: %w", i, c.Metric, j, err)
			}
			a.In[j] = v
		}
	}

	for _, isSet := range []bool{
		a.Min != nil, a.Max != nil, a.GT != nil, a.LT != nil, a.Is != nil,
		len(a.In) > 0, a.Around != nil, a.DeviatesAround != nil,
	} {
		if isSet {
			set++
		}
	}
	return a, set, nil
}

func loadOptionsFrom(cfg config.LoadConfig) (engine.Options, error) {
	if len(cfg.Stages) > 0 {
		stages := make([]engine.Stage, 0, len(cfg.Stages))
		for i, s := range cfg.Stages {
			d, err := s.ParsedDuration()
			if err != nil {
				return engine.Options{}, fmt.Errorf("invalid load.stages[%d].duration: %w", i, err)
			}
			stages = append(stages, engine.Stage{Duration: d, Workers: s.Workers, Rate: s.Rate})
		}
		return engine.Options{Stages: stages, Requests: cfg.Requests, Iterations: cfg.Iterations, MaxWorkers: cfg.MaxWorkers}, nil
	}

	duration, err := cfg.ParsedDuration()
	if err != nil {
		return engine.Options{}, fmt.Errorf("invalid load.duration: %w", err)
	}
	if duration == 0 && cfg.Requests == 0 && cfg.Iterations == 0 {
		return engine.Options{}, fmt.Errorf("scenario must set load.duration, load.requests, load.iterations, or load.stages")
	}
	return engine.Options{
		Duration:   duration,
		Requests:   cfg.Requests,
		Rate:       cfg.Rate,
		Workers:    cfg.Workers,
		MaxWorkers: cfg.MaxWorkers,
		Iterations: cfg.Iterations,
	}, nil
}

func buildGenerator(s *config.Scenario) (generator.Generator, error) {
	switch s.Protocol {
	case "", "http":
		return buildHTTPGenerator(s.HTTP)
	case "ws":
		return buildWSGenerator(s.WS)
	default:
		return nil, fmt.Errorf("unsupported protocol %q (expected \"http\" or \"ws\")", s.Protocol)
	}
}

func buildHTTPGenerator(cfg config.HTTPConfig) (generator.Generator, error) {
	hasTargets := len(cfg.Targets) > 0
	hasFlow := len(cfg.Flow) > 0
	switch {
	case hasTargets && hasFlow:
		return nil, fmt.Errorf("http.targets and http.flow are mutually exclusive; use one or the other")
	case !hasTargets && !hasFlow:
		return nil, fmt.Errorf("scenario must set http.targets or http.flow")
	}
	if hasTargets && (len(cfg.Setup) > 0 || len(cfg.Identities) > 0) {
		return nil, fmt.Errorf("http.setup and http.identities require http.flow; they have no effect with http.targets")
	}

	opts, err := httpOptionsFrom(cfg)
	if err != nil {
		return nil, err
	}

	if hasFlow {
		flow, err := resolveSteps(cfg.Flow)
		if err != nil {
			return nil, fmt.Errorf("flow: %w", err)
		}
		setup, err := resolveSteps(cfg.Setup)
		if err != nil {
			return nil, fmt.Errorf("setup: %w", err)
		}
		return generator.NewFlowGenerator(flow, setup, generator.FlowOptions{
			HTTP:       opts,
			Identities: cfg.Identities,
		})
	}

	targets := make([]generator.HTTPTarget, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		body, rawBody, err := resolveBody(t)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.URL, err)
		}
		targets = append(targets, generator.HTTPTarget{
			Method:        t.Method,
			URL:           t.URL,
			Query:         t.Query,
			Header:        t.Headers,
			Body:          body,
			RawBody:       rawBody,
			ExpectStatus:  t.ExpectStatus,
			ExpectHeaders: t.ExpectHeaders,
			ExpectBody:    t.ExpectBody,
		})
	}
	return generator.NewHTTPGenerator(targets, opts)
}

func warnIfBothBodySet(t config.HTTPTargetConfig) {
	if t.Body != "" && t.BodyFile != "" {
		fmt.Fprintf(os.Stderr, "warning: %s: body is ignored because body_file is also set\n", t.URL)
	}
}

// resolveBody returns the templated body (Body/BodyFile) and/or the raw,
// unrendered body (RawBodyFile) for one target/step. RawBodyFile takes
// priority — with a warning — if Body/BodyFile is also set, since they're
// mutually exclusive at the generator level.
func resolveBody(t config.HTTPTargetConfig) (body string, rawBody []byte, err error) {
	if t.RawBodyFile != "" {
		if t.Body != "" || t.BodyFile != "" {
			fmt.Fprintf(os.Stderr, "warning: %s: body/body_file is ignored because raw_body_file is also set\n", t.URL)
		}
		rawBody, err = os.ReadFile(t.RawBodyFile)
		if err != nil {
			return "", nil, fmt.Errorf("reading raw_body_file: %w", err)
		}
		return "", rawBody, nil
	}
	warnIfBothBodySet(t)
	body, err = t.ResolveBody()
	if err != nil {
		return "", nil, err
	}
	return body, nil, nil
}

func resolveSteps(cfgSteps []config.HTTPTargetConfig) ([]generator.FlowStep, error) {
	steps := make([]generator.FlowStep, 0, len(cfgSteps))
	for i, s := range cfgSteps {
		step, err := resolveStep(s)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func resolveStep(s config.HTTPTargetConfig) (generator.FlowStep, error) {
	if len(s.Steps) > 0 {
		nested, err := resolveSteps(s.Steps)
		if err != nil {
			return generator.FlowStep{}, err
		}
		during, err := s.ParsedDuring()
		if err != nil {
			return generator.FlowStep{}, fmt.Errorf("invalid during: %w", err)
		}
		return generator.FlowStep{Repeat: s.Repeat, During: during, If: s.If, Steps: nested}, nil
	}

	body, rawBody, err := resolveBody(s)
	if err != nil {
		return generator.FlowStep{}, fmt.Errorf("%s: %w", s.URL, err)
	}
	pause, err := s.ParsedPause()
	if err != nil {
		return generator.FlowStep{}, fmt.Errorf("invalid pause: %w", err)
	}
	pauseMax, err := s.ParsedPauseMax()
	if err != nil {
		return generator.FlowStep{}, fmt.Errorf("invalid pause_max: %w", err)
	}
	return generator.FlowStep{
		Method:        s.Method,
		URL:           s.URL,
		Query:         s.Query,
		Header:        s.Headers,
		Body:          body,
		RawBody:       rawBody,
		Extract:       s.Extract,
		ExpectStatus:  s.ExpectStatus,
		ExpectHeaders: s.ExpectHeaders,
		ExpectBody:    s.ExpectBody,
		Pause:         pause,
		PauseMax:      pauseMax,
	}, nil
}

func buildWSGenerator(cfg config.WSConfig) (generator.Generator, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("ws.url is required")
	}
	if len(cfg.Messages) == 0 {
		return nil, fmt.Errorf("ws.messages must have at least one entry")
	}

	opts := generator.DefaultWSOptions()
	if cfg.Timeout != "" {
		d, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid ws.timeout: %w", err)
		}
		opts.Timeout = d
	}
	opts.Insecure = cfg.Insecure
	opts.Identities = cfg.Identities

	if cfg.Feeder.File != "" {
		f, err := generator.NewFeeder(cfg.Feeder.File, cfg.Feeder.Mode)
		if err != nil {
			return nil, fmt.Errorf("ws.feeder: %w", err)
		}
		opts.Feeder = f
	}

	messages := make([]generator.WSMessage, 0, len(cfg.Messages))
	for i, m := range cfg.Messages {
		if !m.Wait && len(m.Extract) > 0 {
			return nil, fmt.Errorf("ws.messages[%d]: extract has no effect without wait: true", i)
		}
		if !m.Wait && len(m.ExpectBody) > 0 {
			return nil, fmt.Errorf("ws.messages[%d]: expect_body has no effect without wait: true", i)
		}
		messages = append(messages, generator.WSMessage{
			Body:       m.Body,
			Binary:     m.Binary,
			Wait:       m.Wait,
			Extract:    m.Extract,
			ExpectBody: m.ExpectBody,
		})
	}

	return generator.NewWSGenerator(generator.WSTarget{
		URL:      cfg.URL,
		Header:   cfg.Headers,
		Messages: messages,
	}, opts)
}

func httpOptionsFrom(cfg config.HTTPConfig) (generator.HTTPOptions, error) {
	opts := generator.DefaultHTTPOptions()
	if cfg.Timeout != "" {
		d, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return opts, fmt.Errorf("invalid http.timeout: %w", err)
		}
		opts.Timeout = d
	}
	opts.BaseURL = cfg.BaseURL
	opts.Insecure = cfg.Insecure
	opts.FollowRedirects = !cfg.NoFollowRedirects
	opts.MaxIdleConns = cfg.MaxIdleConns
	opts.MaxResponseBody = cfg.MaxResponseBody
	opts.CertFile = cfg.CertFile
	opts.KeyFile = cfg.KeyFile
	opts.CAFile = cfg.CAFile
	opts.H2C = cfg.H2C
	opts.DisableKeepAlive = cfg.DisableKeepAlive

	if cfg.Feeder.File != "" {
		f, err := generator.NewFeeder(cfg.Feeder.File, cfg.Feeder.Mode)
		if err != nil {
			return opts, fmt.Errorf("http.feeder: %w", err)
		}
		opts.Feeder = f
	}
	return opts, nil
}

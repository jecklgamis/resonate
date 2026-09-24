package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/jecklgamis/resonate/internal/engine"
	"github.com/jecklgamis/resonate/internal/generator"
	"github.com/jecklgamis/resonate/internal/report"
)

// runOpts bundles the reporting/preview flags shared by resonate hit and
// resonate run, so runAndReport doesn't grow an ever-longer positional
// signature as more of them are added.
type runOpts struct {
	JSON        bool
	Quiet       bool // suppress the periodic progress line on stderr
	DryRun      bool // send exactly one real iteration (vuID 0) and print its results instead of running the full load
	Assertions  []report.Assertion
	HTMLReport  string // path to write a self-contained HTML report to; "" disables it. Defaults to "report.html" (see hit.go/run.go flag defaults).
	JSONReport  string // path to write the JSON summary to; "" disables it. Defaults to "report.json" — independent of JSON, which controls the *stdout* format.
	ResultsFile string // path to write one JSON object per individual generator.Result (JSON Lines), as they complete; "" disables it. Defaults to "results.jsonl" (see hit.go/run.go flag defaults) — same on-by-default convention as HTMLReport/JSONReport, though it's a raw per-request dump rather than a summary and can get large on a high-volume run.
	Title       string // report title (e.g. the target URL or scenario file name)
}

// runAndReport executes the engine against a and writes the final report in
// the chosen format. Cancels cleanly on SIGINT/SIGTERM. Unless out.Quiet, a
// periodic one-line progress update (elapsed, sent, ok/err, req/s) is
// printed to stderr every second — stderr-only so a --json report on stdout
// stays parseable even with progress on.
//
// If out.Assertions is set, they're checked after the report is printed (so
// results are visible either way) and a non-nil error is returned on any
// failure — this is what gives a scenario file's assertions: a non-zero
// process exit code for CI gating.
func runAndReport(a generator.Generator, opts engine.Options, out runOpts) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer a.Close()

	if out.DryRun {
		return dryRun(ctx, a, out.JSON)
	}

	var onResult []func(generator.Result)
	if !out.Quiet {
		p := newProgressPrinter()
		onResult = append(onResult, p.onResult)
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
		onResult = append(onResult, rw.onResult)
	}
	if len(onResult) > 0 {
		opts.OnResult = func(r generator.Result) {
			for _, f := range onResult {
				f(r)
			}
		}
	}

	summary := engine.Run(ctx, a, opts)

	// All OnResult calls already happened synchronously inside engine.Run
	// (see its doc comment), so the file is fully written by now — close it
	// (flushing the buffer) before anything else can fail and short-circuit
	// the function, so a run's raw results are never silently lost.
	if rw != nil {
		if err := rw.Close(); err != nil {
			return fmt.Errorf("writing results file: %w", err)
		}
	}

	if w := concurrencyWarning(opts, summary); w != "" {
		fmt.Fprintln(os.Stderr, w)
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
		if err := writeHTMLReport(out.HTMLReport, out.Title, summary, out.Assertions, failures); err != nil {
			return fmt.Errorf("writing html report: %w", err)
		}
	}
	if out.JSONReport != "" {
		if err := writeJSONReport(out.JSONReport, summary); err != nil {
			return fmt.Errorf("writing json report: %w", err)
		}
	}

	return reportAssertionFailures(failures, len(out.Assertions))
}

// resultsWriter appends one JSON object per generator.Result (JSON Lines) to a
// file as results complete — a raw, unaggregated dump for offline
// reprocessing (custom percentiles, diffing two runs, feeding other
// tooling) that doesn't require re-running the load test. onResult is only
// ever called from engine.Run's single result-consuming goroutine (see its
// doc comment), so no locking is needed here despite the run being
// otherwise concurrent.
type resultsWriter struct {
	f   *os.File
	w   *bufio.Writer
	enc *json.Encoder
}

// resultRecord mirrors generator.Result for JSON output — Result.Error is an
// error interface, which encoding/json can't usefully marshal on its own
// (most error implementations have no exported fields), so it's flattened
// to a message string here instead.
type resultRecord struct {
	Timestamp  time.Time `json:"timestamp"`
	LatencyMS  float64   `json:"latency_ms"`
	StatusCode int       `json:"status_code"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	Error      string    `json:"error,omitempty"`
	Protocol   string    `json:"protocol"`
}

func newResultsWriter(path string) (*resultsWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(f)
	return &resultsWriter{f: f, w: w, enc: json.NewEncoder(w)}, nil
}

func (rw *resultsWriter) onResult(r generator.Result) {
	rec := resultRecord{
		Timestamp:  r.Timestamp,
		LatencyMS:  float64(r.Latency) / float64(time.Millisecond),
		StatusCode: r.StatusCode,
		BytesIn:    r.BytesIn,
		BytesOut:   r.BytesOut,
		Protocol:   r.Protocol,
	}
	if r.Error != nil {
		rec.Error = r.Error.Error()
	}
	// Best-effort: a single line failing to encode/write shouldn't abort an
	// otherwise-successful load test. Close()'s Flush error is what
	// surfaces a genuine problem (e.g. disk full) to the caller.
	_ = rw.enc.Encode(rec)
}

func (rw *resultsWriter) Close() error {
	if err := rw.w.Flush(); err != nil {
		rw.f.Close()
		return err
	}
	return rw.f.Close()
}

// reportAssertionFailures prints any assertion failures to stderr and
// returns a non-nil error if there were any, giving `resonate run
// scenario.yaml` a non-zero exit code for CI gating.
func reportAssertionFailures(failures []report.Failure, total int) error {
	if len(failures) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "%d of %d assertion(s) failed:\n", len(failures), total)
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "  - %s\n", f)
	}
	return fmt.Errorf("%d assertion(s) failed", len(failures))
}

// writeHTMLReport renders and writes the self-contained HTML report to path.
func writeHTMLReport(path, title string, summary report.Summary, assertions []report.Assertion, failures []report.Failure) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return report.WriteHTML(f, report.HTMLReport{
		Title:      title,
		Summary:    summary,
		Assertions: assertions,
		Failures:   failures,
	})
}

// writeJSONReport writes the summary as JSON to path — independent of the
// --json flag, which controls the *stdout* format instead.
func writeJSONReport(path string, summary report.Summary) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return summary.PrintJSON(f)
}

// dryRun sends exactly one real iteration against the target(s) and prints
// its result(s) in detail, without running the full load test — a way to
// sanity-check a scenario (auth works, extract rules match, the target
// actually responds as expected) before committing to a real run.
func dryRun(ctx context.Context, a generator.Generator, jsonOut bool) error {
	results := a.Do(ctx, 0)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}
	for i, r := range results {
		status := "ok"
		if r.Error != nil {
			status = "ERROR: " + r.Error.Error()
		} else if !r.Success() {
			status = fmt.Sprintf("non-success status %d", r.StatusCode)
		}
		fmt.Printf("[%d] %s  status=%d  latency=%s  bytes_out=%d  bytes_in=%d  %s\n",
			i, r.Protocol, r.StatusCode, r.Latency.Round(time.Microsecond), r.BytesOut, r.BytesIn, status)
	}
	return nil
}

// progressPrinter accumulates result counts via engine.Options.OnResult
// (called synchronously from the engine's single result-consuming loop, so
// these updates never race with each other) and prints a summary line to
// stderr on a fixed tick from a separate goroutine. On a real terminal it
// overwrites the same line with \r + an ANSI clear; anywhere else (piped to
// a file, CI logs, `nohup`) those control characters don't mean anything to
// the reader, so it prints one line per tick instead — checked once at
// start(), not per line, since a redirect doesn't change mid-run.
type progressPrinter struct {
	startTime      time.Time
	sent, ok, errs atomic.Int64
	stopCh, doneCh chan struct{}
}

func newProgressPrinter() *progressPrinter {
	return &progressPrinter{startTime: time.Now(), stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

func (p *progressPrinter) onResult(r generator.Result) {
	p.sent.Add(1)
	if r.Success() {
		p.ok.Add(1)
	} else {
		p.errs.Add(1)
	}
}

func (p *progressPrinter) line() string {
	elapsed := time.Since(p.startTime)
	sent := p.sent.Load()
	rate := float64(sent) / elapsed.Seconds()
	return fmt.Sprintf("%s elapsed | %d sent (%d ok, %d err) | %.1f/s",
		elapsed.Round(time.Second), sent, p.ok.Load(), p.errs.Load(), rate)
}

// start begins printing and returns a function that stops it (and, on a
// terminal, clears the last progress line so the final report doesn't get
// printed right after it on the same line).
func (p *progressPrinter) start() func() {
	tty := term.IsTerminal(int(os.Stderr.Fd()))

	go func() {
		defer close(p.doneCh)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case <-ticker.C:
				if tty {
					fmt.Fprint(os.Stderr, "\r"+p.line())
				} else {
					fmt.Fprintln(os.Stderr, p.line())
				}
			}
		}
	}()
	return func() {
		close(p.stopCh)
		<-p.doneCh
		if tty && p.sent.Load() > 0 {
			fmt.Fprint(os.Stderr, "\r\033[K") // clear the last progress line
		}
	}
}

// concurrencyWarning flags a run that fell short of its target rate because
// --workers wasn't enough to keep that many requests in flight at the
// target's observed latency — as opposed to falling short for other reasons
// (errors, a short run, or the target genuinely can't go faster). Returns ""
// when nothing looks wrong, or when the run used a staged schedule (workers
// varies over time there, so this simple check doesn't apply).
func concurrencyWarning(opts engine.Options, summary report.Summary) string {
	if opts.Rate <= 0 || len(opts.Stages) > 0 || summary.Latencies.Mean <= 0 {
		return ""
	}
	if opts.MaxWorkers > 0 {
		// Open model: Summary.Saturated (printed by summary.Print itself)
		// already covers this case with more precise, live-tracked info.
		return ""
	}

	workers := engine.ResolveWorkers(opts)
	sustainable := float64(workers) / summary.Latencies.Mean.Seconds()
	if sustainable >= opts.Rate*0.95 {
		return ""
	}

	suggested := int(math.Ceil(opts.Rate * summary.Latencies.Mean.Seconds() * 1.2))
	if suggested <= workers {
		suggested = workers + 1
	}
	return fmt.Sprintf(
		"warning: achieved %.1f req/s vs the requested %.1f req/s — %d workers can't keep enough requests in flight at this target's ~%s average latency. Try --workers %d (or load.workers: %d in a scenario file).",
		summary.Rate, opts.Rate, workers, summary.Latencies.Mean.Round(time.Millisecond), suggested, suggested,
	)
}

// parseHeaders turns repeated "-H 'Key: Value'" flags into a map. Values may
// contain {{ }} template expressions (see internal/tmpl).
func parseHeaders(raw []string) (map[string]string, error) {
	headers := make(map[string]string, len(raw))
	for _, h := range raw {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("invalid header %q, expected \"Key: Value\"", h)
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return headers, nil
}

// parseQuery turns repeated "-Q 'key=value'" flags into a map. Values may
// contain {{ }} template expressions (see internal/tmpl).
func parseQuery(raw []string) (map[string]string, error) {
	query := make(map[string]string, len(raw))
	for _, q := range raw {
		k, v, ok := strings.Cut(q, "=")
		if !ok {
			return nil, fmt.Errorf("invalid query param %q, expected \"key=value\"", q)
		}
		query[strings.TrimSpace(k)] = v
	}
	return query, nil
}

// assertOperators lists the operators parseAssertions recognizes, longest
// symbol first (">=" before ">", etc.) so a two-character operator is never
// mistaken for its one-character prefix.
var assertOperators = []struct {
	symbol string
	apply  func(a *report.Assertion, v float64)
}{
	{">=", func(a *report.Assertion, v float64) { a.Min = &v }},
	{"<=", func(a *report.Assertion, v float64) { a.Max = &v }},
	{"==", func(a *report.Assertion, v float64) { a.Is = &v }},
	{">", func(a *report.Assertion, v float64) { a.GT = &v }},
	{"<", func(a *report.Assertion, v float64) { a.LT = &v }},
}

// parseAssertions turns repeated "--assert 'metric<op>threshold'" flags
// (e.g. "success_rate>=0.95", "latency_p95<=500ms") into []report.Assertion
// for resonate hit's --assert flag — the same condition/metric machinery
// scenario files' assertions use (via config.AssertionConfig), just a
// terser single-expression syntax suited to a repeatable CLI flag rather
// than YAML's per-field form. Supported operators: >=, <=, ==, >, < —
// mapping to Assertion's Min/Max/Is/GT/LT respectively (see report.Assertion
// for the full condition set; the CLI flag only exposes these five, not
// In/Around/DeviatesAround, which don't fit a single "metric op value"
// expression).
func parseAssertions(raw []string) ([]report.Assertion, error) {
	assertions := make([]report.Assertion, 0, len(raw))
	for _, expr := range raw {
		a, err := parseAssertExpr(expr)
		if err != nil {
			return nil, err
		}
		assertions = append(assertions, a)
	}
	return assertions, nil
}

func parseAssertExpr(expr string) (report.Assertion, error) {
	for _, op := range assertOperators {
		idx := strings.Index(expr, op.symbol)
		if idx < 0 {
			continue
		}
		metric := strings.TrimSpace(expr[:idx])
		valueStr := strings.TrimSpace(expr[idx+len(op.symbol):])
		if metric == "" || valueStr == "" {
			return report.Assertion{}, fmt.Errorf("invalid --assert %q, expected \"metric<op>threshold\" (e.g. \"success_rate>=0.95\")", expr)
		}
		if !report.IsKnownAssertionMetric(metric) {
			return report.Assertion{}, fmt.Errorf("invalid --assert %q: unknown metric %q (expected one of %v, or \"latency_p<N>\" for any percentile N 0-100)", expr, metric, report.AssertionMetrics)
		}
		v, err := report.ParseThreshold(metric, valueStr)
		if err != nil {
			return report.Assertion{}, fmt.Errorf("invalid --assert %q: %w", expr, err)
		}
		a := report.Assertion{Metric: metric}
		op.apply(&a, v)
		return a, nil
	}
	return report.Assertion{}, fmt.Errorf("invalid --assert %q, expected \"metric<op>threshold\" with one of >=, <=, ==, >, < (e.g. \"latency_p95<=500ms\")", expr)
}

// parseExpectBody turns repeated "--expect-body 'rule=value'" flags into a
// map, e.g. "json:id=123" or "xml://user/@id=42". Splits on the *last* "="
// rather than the first, since a JSONPath/XPath rule can itself contain "="
// (e.g. a filter expression like json:$.items[?(@.status=="ok")].id).
func parseExpectBody(raw []string) (map[string]string, error) {
	body := make(map[string]string, len(raw))
	for _, e := range raw {
		i := strings.LastIndex(e, "=")
		if i < 0 {
			return nil, fmt.Errorf("invalid expect-body %q, expected \"rule=value\" (e.g. \"json:id=123\")", e)
		}
		body[strings.TrimSpace(e[:i])] = strings.TrimSpace(e[i+1:])
	}
	return body, nil
}

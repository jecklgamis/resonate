package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jecklgamis/resonate/internal/engine"
	"github.com/jecklgamis/resonate/internal/generator"
)

func newHitCommand() *cobra.Command {
	var (
		method       string
		headers      []string
		query        []string
		body         string
		bodyFile     string
		rawBodyFile  string
		duration     time.Duration
		requests     uint64
		reqRate      float64
		workers      int
		maxWorkers   int
		maxIdleConns int
		iterations   uint64
		timeout      time.Duration
		insecure     bool
		noRedir      bool
		maxRespBody  int64
		certFile     string
		keyFile      string
		caFile       string
		h2c          bool
		noKeepalive  bool
		expectStatus []int
		expectHeader []string
		expectBody   []string
		jsonOut      bool
		quiet        bool
		dryRun       bool
		htmlReport   string
		jsonReport   string
		resultsFile  string
		assertExprs  []string
	)

	cmd := &cobra.Command{
		Use:   "hit <url>",
		Short: "Send HTTP load to a single URL",
		Long: "Send HTTP load to a single URL. The url, headers, query values, and body\n" +
			"may all contain {{ }} template expressions, re-rendered per request, e.g.\n" +
			"--body '{\"id\": {{.Seq}}, \"tok\": \"{{uuid}}\"}'. See README for the function list.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]

			hdr, err := parseHeaders(headers)
			if err != nil {
				return err
			}
			expectHdr, err := parseHeaders(expectHeader)
			if err != nil {
				return err
			}
			expectBd, err := parseExpectBody(expectBody)
			if err != nil {
				return err
			}
			q, err := parseQuery(query)
			if err != nil {
				return err
			}
			assertions, err := parseAssertions(assertExprs)
			if err != nil {
				return err
			}

			bodyStr := body
			if bodyFile != "" {
				if body != "" {
					fmt.Fprintln(os.Stderr, "warning: --body is ignored because --body-file is also set")
				}
				b, err := os.ReadFile(bodyFile)
				if err != nil {
					return fmt.Errorf("reading body file: %w", err)
				}
				bodyStr = string(b)
			}

			var rawBody []byte
			if rawBodyFile != "" {
				if body != "" || bodyFile != "" {
					fmt.Fprintln(os.Stderr, "warning: --body/--body-file is ignored because --raw-body-file is also set")
					bodyStr = ""
				}
				b, err := os.ReadFile(rawBodyFile)
				if err != nil {
					return fmt.Errorf("reading raw body file: %w", err)
				}
				rawBody = b
			}

			if duration == 0 && requests == 0 && iterations == 0 {
				duration = 10 * time.Second
			}

			a, err := generator.NewHTTPGenerator(
				[]generator.HTTPTarget{{Method: method, URL: url, Query: q, Header: hdr, Body: bodyStr, RawBody: rawBody, ExpectStatus: expectStatus, ExpectHeaders: expectHdr, ExpectBody: expectBd}},
				generator.HTTPOptions{
					Timeout:          timeout,
					Insecure:         insecure,
					FollowRedirects:  !noRedir,
					MaxIdleConns:     maxIdleConns,
					MaxResponseBody:  maxRespBody,
					CertFile:         certFile,
					KeyFile:          keyFile,
					CAFile:           caFile,
					H2C:              h2c,
					DisableKeepAlive: noKeepalive,
				},
			)
			if err != nil {
				return err
			}

			opts := engine.Options{
				Duration:   duration,
				Requests:   requests,
				Rate:       reqRate,
				Workers:    workers,
				MaxWorkers: maxWorkers,
				Iterations: iterations,
			}
			if err := engine.Validate(opts); err != nil {
				return err
			}
			return runAndReport(a, opts, runOpts{JSON: jsonOut, Quiet: quiet, DryRun: dryRun, Assertions: assertions, HTMLReport: htmlReport, JSONReport: jsonReport, ResultsFile: resultsFile, Title: url})
		},
	}

	cmd.Flags().StringVarP(&method, "method", "X", "GET", "HTTP method")
	cmd.Flags().StringArrayVarP(&headers, "header", "H", nil, "Request header \"Key: Value\" (repeatable, templated)")
	cmd.Flags().StringArrayVarP(&query, "query", "Q", nil, "Query param \"key=value\" (repeatable, templated)")
	cmd.Flags().StringVarP(&body, "body", "d", "", "Request body (templated)")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Read request body from file (templated)")
	cmd.Flags().StringVar(&rawBodyFile, "raw-body-file", "", "Read request body from file and send it as-is, bypassing templating (for large/binary payloads); overrides --body/--body-file")
	cmd.Flags().DurationVar(&duration, "duration", 0, "Test duration, e.g. 30s (default 10s if --requests not set)")
	cmd.Flags().Uint64Var(&requests, "requests", 0, "Total number of requests to send (overrides --duration as the stop condition)")
	cmd.Flags().Uint64Var(&iterations, "iterations", 0, "Requests per virtual user before it departs (0 = runs for the whole test); with no --duration/--requests, total = --workers * --iterations")
	cmd.Flags().Float64Var(&reqRate, "rate", 0, "Target requests/sec across all workers (0 = unlimited)")
	cmd.Flags().IntVar(&workers, "workers", 0, "Number of concurrent workers (0 = 10, or rate-aware if --rate is set)")
	cmd.Flags().IntVar(&maxWorkers, "max-workers", 0, "Open model: with --rate, allow concurrency to grow up to this many in-flight requests to sustain the target rate under latency, instead of capping at --workers (0 = disabled, closed model)")
	cmd.Flags().IntVar(&maxIdleConns, "max-idle-conns", 0, "Max idle (keep-alive) connections per target host (0 = Go's default, 100)")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "Per-request timeout")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Skip TLS certificate verification")
	cmd.Flags().BoolVar(&noRedir, "no-redirect", false, "Do not follow HTTP redirects")
	cmd.Flags().Int64Var(&maxRespBody, "max-response-body", 0, "Max response bytes to read/count per request; the rest is discarded (0 = unlimited)")
	cmd.Flags().StringVar(&certFile, "cert", "", "Client certificate file for mTLS (requires --key)")
	cmd.Flags().StringVar(&keyFile, "key", "", "Client private key file for mTLS (requires --cert)")
	cmd.Flags().StringVar(&caFile, "ca-cert", "", "Additional CA certificate(s) (PEM) to trust, e.g. for a private/internal CA")
	cmd.Flags().BoolVar(&h2c, "h2c", false, "Force HTTP/2 over plaintext (prior knowledge); mutually exclusive with --insecure/--cert/--key/--ca-cert")
	cmd.Flags().BoolVar(&noKeepalive, "no-keepalive", false, "Disable HTTP keep-alive: open a fresh connection per request instead of reusing pooled connections; mutually exclusive with --h2c")
	cmd.Flags().IntSliceVar(&expectStatus, "expect-status", nil, "Comma-separated status codes that count as success (overrides the default 2xx/3xx check), e.g. --expect-status 200,201,404")
	cmd.Flags().StringArrayVar(&expectHeader, "expect-header", nil, "Response header check \"Key: Value\" (exact match), or \"Key:\" to just require it's present (repeatable)")
	cmd.Flags().StringArrayVar(&expectBody, "expect-body", nil, "Response body check \"rule=value\" using the same rule language as extract (\"json:<JSONPath>\", \"yaml:<JSONPath>\", \"xml:<XPath>\"), e.g. --expect-body 'json:status=ok' (repeatable)")
	cmd.Flags().StringArrayVar(&assertExprs, "assert", nil, "Assert a report metric after the run, e.g. --assert 'success_rate>=0.95' or --assert 'latency_p95<=500ms' (operators: >=, <=, ==, >, <; repeatable); a failed assertion gives a non-zero exit code for CI gating")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print the report as JSON")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress the periodic progress line on stderr")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Send exactly one real request and print its result, instead of running the full load test")
	cmd.Flags().StringVar(&htmlReport, "html-report", "report.html", "Write a self-contained HTML report to this path (\"\" disables it)")
	cmd.Flags().StringVar(&jsonReport, "json-report", "report.json", "Write the JSON report to this path (\"\" disables it; independent of --json, which controls stdout)")
	cmd.Flags().StringVar(&resultsFile, "results-file", "results.jsonl", "Write one JSON object per individual request (JSON Lines) to this path as results complete (\"\" disables it)")

	return cmd
}

# Reports

Every run (not `--dry-run`) gives you the results four ways: a stdout
summary, an HTML report, a JSON report, and a raw per-request dump. All
four are on by default, no flags needed, and are written after the run
finishes (including a failed run) so you always get something to look at.

## Stdout Summary

Printed after every run: requests/rate, success ratio, latency
percentiles, status codes, errors. Pass `--json` to print the same data as
JSON instead of text — handy for piping into `jq` or another tool.

The latency line lists the minimum, mean, the **p50, p75, p90, p95 and p99**
percentiles, and the maximum:

```text
Latencies     [min, mean, p50, p75, p90, p95, p99, max]  255µs, 727µs, 683µs, 923µs, 1.113ms, 1.29ms, 2.349ms, 2.398ms
```

The same values are in the JSON report as `latencies.min`, `mean`, `stddev`, `p50`,
`p75`, `p90`, `p95`, `p99` and `max` (durations in nanoseconds), and any of them can
be asserted on (for example `latency_p75`; see [Assertions](scenarios.md#assertions)).
The percentiles are computed from a histogram with about 0.1% error.

### Steps

With two or more named (or default-labelled) steps, a `Steps:` table follows the totals:
each step's requests, success rate and p50/p95/p99. See
[Per-Step Results](scenarios.md#per-step-results). A run stopped by an `abort: true`
assertion ends with an `ABORTED: ...` line (see
[Stopping a run early](scenarios.md#stopping-a-run-early)).

### What counts as a request

For HTTP, one request is one HTTP call. The other protocols report in finer
grain, so `requests` — and the rate derived from it — can exceed the number
of iterations:

- **WebSocket:** one result for the connect plus one per message.
- **Server-Sent Events:** one result for the connect plus one per event received
  (see [Server-Sent Events](sse.md#what-gets-reported)).
- **gRPC:** one result per unary call, and for a streaming call one result
  per *received* message (see [gRPC](grpc.md#what-a-stream-reports)), so a stream
  delivering 5 messages adds 5 requests. gRPC statuses show in the status
  breakdown as their HTTP equivalents (`OK` 200, `NOT_FOUND` 404,
  `UNAVAILABLE` 503, ...); the error breakdown keeps the real gRPC code.

## HTML Report

`report.html`, written to the current directory by default. A single
self-contained file (inline CSS/SVG, no scripts, no external assets) — opens
standalone, attachable/emailable/archivable as-is. Includes KPI cards, a
run-settings table, a requests-over-time chart, a latency-over-time chart, a
response-time histogram, the latency
percentile table (p50, p75, p90, p95, p99), status/error breakdowns, and (for
`resonate run` with `assertions`) a pass/fail table. The footer links back to the
[resonate repository](https://github.com/jecklgamis/resonate). Change the path with
`--html-report`, or pass `""` to disable it.

### The "Requests over time" chart

Stacked bars of requests per time bucket, with failures (red) at the base and
successes (green) above, on a grid:

- **Axes.** The y axis is the request count on round gridlines (0, 50, 100, ...), and
  the x axis is the time of day along dashed vertical gridlines.
- **Hover.** Point anywhere in a bucket's column (not only at the bar) to see its time of
  day and offset from the start of the run (`10:00:07  (+7s)`), the number of requests
  with how many succeeded and failed, the rate per second, and the mean latency of that
  bucket. It is plain CSS, so it works in any browser without scripts.
- **Bucket width.** A bar is one second wide for runs up to two minutes. A longer run is
  divided into about 120 buckets, so a bar is wider (a ten-minute run has 5s bars); the
  chart then says `each bar = 5s`, and the tooltip's per-second rate accounts for it.

The first bar of a run paced with `rate` is often about twice the others; see
[Request Counts and the Startup Burst](execution-models.md#request-counts-and-the-startup-burst).

### The "Latency over time" chart

Three lines, p50 (green), p95 (blue) and p99 (red), each computed from the requests
that finished in that time bucket. The run-wide percentiles in the table hide a slowdown
that starts partway through; this chart shows when it started and whether it keeps
getting worse. The y axis is in milliseconds (microseconds if every latency is under
1ms), and hovering a bucket shows its three values. A bucket with no requests leaves a
gap in the lines rather than dropping to zero.

A bucket with only a handful of requests has a noisy p99 (with fewer than 100 requests
the p99 is simply the slowest one), so a spiky p99 line on a low-rate run is not
necessarily a problem. The buckets are the same as in the requests chart, so a `5s` bar
there means each point here covers 5 seconds.

### The "Response time distribution" chart

A histogram of every request's latency in 20 bins. The bins are **log-spaced**: each
bin's upper edge is a constant factor above the previous one, and the labels along the
bottom mark every fourth edge. A log scale keeps a long tail visible, where equal-width
bins would put nearly every request in the first bar and show the slow ones as an
unreadable sliver. Hover a bar for its range and count.

### The "Run settings" table

How the run was set up, so a report opened later still says what produced it. Only rows
that apply are shown:

| Row | What it says |
| --- | --- |
| Scenario file | The path and a short SHA-256 of the file's contents, so you can tell whether it has changed since (`resonate run` only) |
| Protocol | `http`, `ws`, `sse` or `grpc` |
| Target | Where the load went: scheme, host and path, up to four, then `+N more` |
| Shape | What one iteration does, such as `3 flow steps, 1 setup step`, `2 targets (round-robin)`, `2 calls per iteration` |
| Identities | How many identities the pool has |
| Feeder | The source, such as `file users.csv (stream)` or `Kafka topic users, from start` |
| Transport | Anything other than the default: HTTP/3, h2c, TLS verification off, client certificate, custom CA, no keep-alive, redirects not followed, or gRPC plaintext |
| Timeout, Max idle connections, Connections, Max response body | The client tuning that was set. These explain a lot of high-concurrency latency |
| Duration or Requests, Rate, Workers, Max workers, Iterations per VU, Stages | The load shape |
| Resonate | The version and the platform and Go version it was built with |
| Started, Finished | Wall-clock times of the run, with time zone |

It deliberately leaves out anything that could be a secret, because reports get emailed
and attached. A URL is cut at its `?` (and at the first `{{` of a template), and a
`user:password@` part is dropped, so a token in a query string never appears. Header
values, request bodies, identity contents and Kafka credentials are never listed.

The report does not carry the scenario itself. Error messages are the exception to the
rule above: they are Go's own text, which can include the full request URL (with its
query string), so look at the *Errors* table before sharing a report from a URL with a
token in it.

`resonate hit` fills in the same table from its flags. A report written from Go with
`resonate.WriteHTML` shows the table when you fill in `HTMLReport.Settings`.

## JSON Report

`report.json`, written to the current directory by default — the same
`Summary` structure `--json` prints to stdout, just saved to a file
regardless of what the stdout format is. Useful for archiving a run or
feeding it into other tooling. Each `time_series` bucket carries `p50_latency`,
`p95_latency` and `p99_latency` alongside `mean_latency`, and `latency_histogram`
bins are log-spaced (`le` is each bin's upper edge). Change the path with `--json-report`, or
pass `""` to disable it. Independent of `--json`, which only controls the
*stdout* format.

Two fields are present only when they apply: `steps` (one entry per step, with its own
`requests`, `success_rate`, `latencies`, `status_codes` and `errors`; see
[Per-Step Results](scenarios.md#per-step-results)) and `aborted` (the failed assertion that
stopped the run early). The HTML report shows them as a *Steps* table and an *ABORTED* banner.

### Stability

`report.json` and `results.jsonl` are a stable contract, since other tools (such as
[resonate-fleet](https://github.com/jecklgamis/resonate-fleet)) parse them.

- `report.json` carries `schema_version` (currently `1`). It changes only for a breaking
  change to a documented field: a rename, a removal or a change of unit. Adding a field
  never bumps it, so readers must ignore fields they don't know.
- Stable `report.json` fields: `requests`, `success`, `success_rate`, `duration`
  (nanoseconds), `requests_per_sec`, `bytes_in`, `bytes_out`, `latencies.*` (`min`, `mean`,
  `stddev`, `p50`, `p75`, `p90`, `p95`, `p99`, `max`; nanoseconds), `status_codes`, `errors`,
  `steps`, `aborted`, `time_series` and `latency_histogram`.
- Stable `results.jsonl` fields per line: `timestamp` (RFC 3339), `latency_ms`,
  `status_code`, `bytes_in`, `bytes_out`, `protocol` and `error` (only on failure).
- The `resonate run` flags `--quiet`, `--html-report`, `--json-report` and `--results-file`,
  and the [exit codes](usage.md#exit-codes), are stable too.

`internal/cli/contract_test.go` runs the exact invocation resonate-fleet uses and checks
these fields, so a breaking change fails the build.

## Comparing Runs

`resonate compare` reads two JSON reports and prints every headline metric side by side,
then each step's metrics when both runs have it. Keep a `report.json` from a known-good run
as the baseline and compare each new run against it:

```sh
resonate run scenario.yaml --json-report current.json
resonate compare baseline.json current.json
```

```text
Metric                    Baseline         Current      Change
requests                      1000            1000       +0.0%
rate                        100.00           92.00       -8.0%
success_rate               100.00%         100.00%       +0.0%
latency_p50                   50ms            75ms      +50.0%
latency_p95                  100ms           150ms      +50.0%
latency_p99                  200ms           300ms      +50.0%
latency_max                  300ms           450ms      +50.0%
login:latency_p95             25ms          37.5ms      +50.0%
```

(The real table also lists the mean, p75, p90 and `error_rate`, and the same set for every step.)

With `--fail-on` it exits non-zero when a regression rule matches, so a pipeline can gate on
it. A rule is `<metric><op><threshold>` over the change from baseline to current
(`>`, `>=`, `<`, `<=`); repeat the flag for several rules. A threshold ending in `%` is
relative to the baseline, anything else is an absolute change in the metric's unit (a duration
for latency metrics, a plain number for the rest; `success_rate` and `error_rate` are ratios from
0 to 1). Prefix a metric with `<step>:` to gate on one step.

```sh
resonate compare baseline.json current.json \
  --fail-on 'latency_p95>10%' \
  --fail-on 'rate<-5%' \
  --fail-on 'error_rate>0.01' \
  --fail-on 'checkout:latency_p99>50ms'
```

That fails the build if p95 is more than 10% slower, throughput is down more than 5%, the
error rate is up by more than a point, or the `checkout` step's p99 is more than 50ms slower.
Each rule prints a verdict line, so a failing pipeline says which one tripped:

```text
FAIL  latency_p95>10%: 100ms -> 150ms (+50.0%)
FAIL  rate<-5%: 100.00 -> 92.00 (-8.0%)
ok    error_rate>0.01: 0.00% -> 0.00% (+0.0%)
```

Notes:

- A rule that can't be evaluated is an error, never a silent pass: a step missing from either
  run, or an unknown metric. Percentiles other than p50/p75/p90/p95/p99 aren't in the JSON
  report, so they can't be compared.
- A relative rule on a metric whose baseline is zero (no errors before, some now) fails, since a
  percentage of zero is undefined; use an absolute threshold for those, such as `error_rate>0.01`.
- A baseline or current run that was aborted early is flagged on stderr, since its numbers cover
  only part of a run.

## Raw Results (JSON Lines)

`results.jsonl`, written alongside the other reports — one JSON object
per request, as it completes, instead of just the aggregate. For offline
reprocessing (custom percentiles, diffing runs) without re-running the
test. Change the path with `--results-file`, or pass `""` to disable it
— it can get large on a high-volume run.

Each line looks like:

```json
{"timestamp":"2026-01-01T00:00:00Z","latency_ms":1.23,"status_code":200,"bytes_in":512,"bytes_out":128,"protocol":"http"}
```

`error` (omitted on success) holds the error message as a string, since a
Go `error` value can't be usefully marshaled on its own.

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
requests-over-time chart, a response-time histogram, the latency
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

## JSON Report

`report.json`, written to the current directory by default — the same
`Summary` structure `--json` prints to stdout, just saved to a file
regardless of what the stdout format is. Useful for archiving a run or
feeding it into other tooling. Change the path with `--json-report`, or
pass `""` to disable it. Independent of `--json`, which only controls the
*stdout* format.

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

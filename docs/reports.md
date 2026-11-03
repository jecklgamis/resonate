# Reports

Every run (not `--dry-run`) gives you the results four ways: a stdout
summary, an HTML report, a JSON report, and a raw per-request dump. All
four are on by default, no flags needed, and are written after the run
finishes (including a failed run) so you always get something to look at.

## Stdout Summary

Printed after every run: requests/rate, success ratio, latency
percentiles, status codes, errors. Pass `--json` to print the same data as
JSON instead of text — handy for piping into `jq` or another tool.

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
self-contained file (inline CSS/SVG, no external assets) — opens
standalone, attachable/emailable/archivable as-is. Includes KPI cards, a
requests-over-time chart, a response-time histogram, the latency
percentile table, status/error breakdowns, and (for `resonate run` with
`assertions`) a pass/fail table. Change the path with `--html-report`,
or pass `""` to disable it.

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

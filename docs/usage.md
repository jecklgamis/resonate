# Usage

## `resonate hit` vs. `resonate run`

| | `resonate hit <url>` (CLI) | `resonate run <scenario.yaml>` (YAML) |
| --- | --- | --- |
| Input | CLI flags | YAML scenario file |
| Protocol | HTTP only | HTTP or WebSocket (`protocol: ws`) |
| Targets | One URL | Multiple targets (round-robin), or a multi-step `flow` |
| State across requests | None — always independent | `flow` supports response chaining (`{{.Vars.*}}`) and per-VU `identities` |
| Setup steps | Not available | `http.setup` — runs once per VU before its first iteration (e.g. log in once, reuse the token) |
| Ramping load | Flat only (`--rate`/`--workers`/`--max-workers`) | Flat *or* `load.stages` (ramp profiles — ramping-vus, ramping-arrival-rate) |
| Assertions / CI gating | `--assert` (5 operators) | `assertions:` (7 condition kinds) |
| Data-driven requests | Only via templating functions | `http.feeder`/`ws.feeder` — one row of a CSV/JSON file per iteration (`{{.Feeder.*}}`) |
| mTLS / CA / h2c / keep-alive | Yes (`--cert`/`--key`/`--ca-cert`/`--h2c`/`--no-keepalive`) | Yes, as `http.*` scenario fields |
| Use case | Quick, one-off "is this endpoint OK" check | Reusable, checked-into-source-control, realistic multi-request scenarios; CI-gated load tests |

Everything else is identical underneath: same engine (open/closed model,
workers/rate), same four reports (see [Reports](reports.md)), same
templating functions, same `--dry-run`/`--quiet`/`--json` behavior.

## `resonate hit` — Quick, Flag-Driven Runs

For a one-off HTTP load test against a single URL, gated on p95 latency
and success rate (`--assert` gives a non-zero exit if either fails, for
CI — see [Assertions](#assertions) below for the full syntax):

```sh
resonate hit http://localhost:8080/health --rate 50 --duration 30s --workers 20 \
  --assert 'latency_p95<=500ms' \
  --assert 'success_rate>=0.95'
```

A `POST` with a randomized body, using the built-in template functions
(see [Request Templating](#request-templating)):

```sh
resonate hit http://localhost:8080/users \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"id": "{{uuid}}", "name": "{{randChoice "alice" "bob" "carol"}}", "age": {{randInt 18 65}}}' \
  --rate 50 --duration 30s --workers 20 \
  --assert 'latency_p95<=500ms' \
  --assert 'success_rate>=0.95'
```

For a large or binary body, `--body-file`/`--raw-body-file` read it from
a file instead of `-d` — the latter skips templating entirely, so it's
also the safe choice for a body that happens to contain literal `{{ }}`.

`resonate hit` is HTTP-only, flag-driven, and single-target — for
WebSocket, multi-target, multi-step flows, or anything reusable, use
`resonate run` instead. See the [CLI Reference](cli/resonate_hit.md) for
every flag.

## `resonate run` — Scenario Files

For multi-target or reusable setups, describe the run in YAML and execute
with `resonate run`:

```sh
cat > scenario.yaml << 'EOF'
protocol: http

load:
  duration: 30s
  rate: 50
  workers: 20

http:
  timeout: 5s
  targets:
    - method: POST
      url: http://localhost:8080/users
      headers:
        Content-Type: application/json
      body: '{"id": "{{uuid}}", "name": "{{randChoice "alice" "bob" "carol"}}", "age": {{randInt 18 65}}}'

assertions:
  - metric: latency_p95
    max: 500ms

  - metric: success_rate
    min: "0.95"
EOF

resonate run scenario.yaml --json
```

See [`examples/http-basic.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-basic.yaml),
[Scenarios](scenarios.md) for the full YAML schema (targets, flows,
identities, WebSocket, assertions), and the
[CLI Reference](cli/resonate_run.md) for `resonate run`'s own flags.

## Request Templating

`url`, `query`, `headers`, and `body` (on both `resonate hit` flags and
scenario file targets) can embed `{{ }}` template expressions, re-rendered
independently for every request. A field with no `{{` is left as a plain
literal — no overhead.

| Expression | Description |
| --- | --- |
| `.Seq` | Monotonic request counter, unique per request, starts at 0 |
| `.VU` | The calling virtual user's id — for per-worker sharding/partitioning without an identities pool |
| `base64 s` / `base64Decode s` | Base64 encode/decode (e.g. Basic-Auth headers) |
| `env "VAR_NAME"` | Reads an environment variable (e.g. for tokens/secrets) |
| `hmacSHA256 "key" s` | Hex-encoded HMAC-SHA256, keyed — for request-signing auth schemes |
| `jsonEscape s` | Escape `s` for safe embedding inside a JSON string literal you've already quoted |
| `md5 s` / `sha256 s` | Hex-encoded digest |
| `now "layout"` | `"unix"`, `"unixmilli"`, or a Go time layout |
| `padLeft n "pad" s` / `padRight n "pad" s` | Pad `s` to at least `n` bytes (e.g. zero-padded numeric IDs) |
| `randBool` | Random `true`/`false` |
| `randChoice "a" "b" "c"` | Picks one argument at random |
| `randDate "min" "max" "layout"` | Random point in time between two RFC3339 bounds, formatted like `now` |
| `randFloat min max` | Random float in `[min, max)` |
| `randInt min max` | Random int in `[min, max)` |
| `randString n` | Random alphanumeric string of length `n` |
| `truncate n s` | Cap `s` to at most `n` runes |
| `upper s` / `lower s` / `trim s` | Uppercase/lowercase/trim surrounding whitespace |
| `urlEncode s` / `urlDecode s` | Percent-encode/decode `s` for safe use as a query string value |
| `uuid` | Random UUID (filler data, not crypto-secure) |

See [`examples/http-templated.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-templated.yaml).
Scenario files can also drive requests from a real CSV/JSON dataset
instead of pure random values — see [Feeders](scenarios.md#feeders).

## Status, Header, and Body Checks

By default, a response counts as successful if its status is 2xx/3xx.
`expect_status`/`expect_headers`/`expect_body` — `--expect-status`/
`--expect-header`/`--expect-body` on `resonate hit`, or per-entry on a
scenario file's `targets`/`flow`/`setup` — override that for a specific
request. All three can be set together (every check must pass); any one
alone still replaces the default 2xx/3xx-only rule.

- `expect_status`: a status not in the list counts as failed even if it's
  otherwise a 2xx/3xx, and a status you do list counts as success even
  outside the default range (e.g. treating a 404 as the correct response
  for a not-found check).
- `expect_headers`: a non-empty value requires an exact match (name
  matching is case-insensitive); an empty value only requires the header
  to be present.
- `expect_body`: checks the response body, keyed by rule and valued by the
  expected result — real JSONPath (`json:status`, `json:items[0].id`, a
  leading `$` is optional), the same JSONPath language against a YAML body
  (`yaml:status`), real XPath 1.0 (`xml://user/name`, `xml://user/@id`),
  `header:<Name>`, or `status`. A non-empty value requires an exact match;
  an empty value only requires the rule to evaluate successfully (the
  path/element matches something). Note this is a narrower rule set than
  `extract` below — no `regex:`/`css:` here.

```sh
resonate hit http://localhost:8080/users/999999 --expect-status 404
resonate hit http://localhost:8080/orders -X POST \
  --expect-header 'Content-Type: application/json' \
  --expect-body 'json:status=created'
```

```yaml
http:
  targets:
    - url: http://localhost:8080/orders
      method: POST
      expect_status: [201]   # a 200 now counts as a failure
      expect_headers:
        Content-Type: application/json
        X-Request-Id: ""     # just needs to be present
      expect_body:
        "json:status": "created"
        "json:id": ""        # just needs to exist
```

Invalid `expect_status` codes (outside 100-599), blank `expect_headers`
names, or unrecognized `expect_body` rule prefixes are rejected at
construction, before any request is sent. Any check that fails is
reported with an explanatory error naming which check(s) failed and why;
a failing check on a `flow`/`setup` step stops the rest of that
iteration, same as any other failed step (see
[Multi-Step Flows](scenarios.md#multi-step-flows-and-response-chaining)).
See [`examples/http-status-checks.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-status-checks.yaml).

## Assertions

Both commands can check metrics from the completed run's report against
thresholds and exit non-zero if any fail — for gating a CI pipeline on
load-test results. `resonate hit` uses a repeatable
`--assert 'metric<op>threshold'` flag (see the example at the top of this
page); `resonate run` has the same capability, with more conditions, via
a scenario file's `assertions:` list — see
[Scenarios](scenarios.md#assertions). Each `--assert`/`assertions:` entry
is checked independently (combined with AND). `--assert` operators:

| Operator | Meaning |
| --- | --- |
| `>=` | Value must be at least the threshold |
| `<=` | Value must be at most the threshold |
| `==` | Value must equal the threshold exactly |
| `>` | Value must be strictly greater than the threshold |
| `<` | Value must be strictly less than the threshold |

See [Scenarios](scenarios.md#assertions) for the full metric list and the
richer condition set (`gt`/`lt`/`is`/`in`/`around`/`deviatesAround`)
available to a scenario file's `assertions:`, and
[`examples/http-assertions.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-assertions.yaml)
for a worked example.

## Live Progress

Unless `--quiet`, both commands print a one-line progress update to
stderr every second — elapsed time, requests sent, ok/error counts,
current req/s. It's stderr-only, so a `--json` report on stdout stays
parseable even with progress on.

## Dry Run

`--dry-run` sends exactly one real iteration and prints its result(s) —
status, latency, bytes, any error — instead of running the full load
test. Use it to sanity-check a scenario (auth works, `extract` rules
actually match, the target responds the way you expect) before committing
to a real run.

## Validation

Both `resonate hit` and `resonate run` fail fast — before any request is
sent — on:

- A literal (non-templated) URL that isn't an absolute `scheme://host` URL.
  This matters beyond typo-catching: a URL that fails to parse resolves
  locally with no network I/O, so with no `--rate`/`load.rate` set, an
  unvalidated bad URL would otherwise let the worker loop free-spin at
  millions of iterations/sec for the full run instead of failing
  immediately. A *templated* URL (containing `{{`) can't be validated until
  it's rendered per-request, so malformed values there still only surface
  as a per-request error result.
- Negative `--rate`/`load.rate`, `--workers`/`load.workers`, or stage
  `workers`/`rate`/`duration` — these are rejected rather than silently
  treated as "unset."
- `http.setup`/`http.identities` set without `http.flow` — they only apply
  to flows, so using them with `http.targets` errors instead of silently
  doing nothing.
- `http.targets`/`http.flow` set together, or neither set.
- No stop condition (`duration`/`requests`/`iterations`/`stages`) on a
  scenario file.
- An `expect_status`/`--expect-status` code outside the valid HTTP status
  range (100-599), a blank `expect_headers`/`--expect-header` name, or an
  `expect_body`/`--expect-body` rule with an unrecognized prefix (not
  `json:`, `yaml:`, `xml:`, `header:`, or `status`).

Three things warn (to stderr) rather than error, since there's a sane
fallback: `--body` and `--body-file` (or a step's `body`/`body_file`) both
set — the file wins; `--raw-body-file` set together with `--body`/
`--body-file` (or `raw_body_file` with `body`/`body_file`) — the raw file
wins; and a run whose achieved rate fell short specifically because of
insufficient `--workers` (see [Execution Models](execution-models.md)).

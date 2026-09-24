# Usage

## `resonate hit` vs. `resonate run`

| | `resonate hit <url>` (CLI) | `resonate run <scenario.yaml>` (YAML) |
| --- | --- | --- |
| Input | CLI flags | YAML scenario file |
| Protocol | HTTP only | HTTP or WebSocket (`protocol: ws`) |
| Targets | One URL | Multiple targets (round-robin), or a multi-step `flow` |
| State across requests | None | `flow` supports response chaining (`{{.Vars.*}}`) and per-VU `identities` |
| Setup steps | Not available | `http.setup` — runs once per VU before its first iteration |
| Ramping load | Flat only (`--rate`/`--workers`/`--max-workers`) | Flat or `load.stages` (ramp profiles) |
| Assertions / CI gating | `--assert` (5 operators) | `assertions:` (7 condition kinds) |
| Data-driven requests | Templating functions only | `http.feeder`/`ws.feeder` — one CSV/JSON row per iteration (`{{.Feeder.*}}`) |
| mTLS / CA / h2c / keep-alive | Yes (`--cert`/`--key`/`--ca-cert`/`--h2c`/`--no-keepalive`) | Yes, as `http.*` fields |
| Use case | Quick, one-off endpoint check | Reusable, checked-into-source-control scenarios; CI-gated load tests |

Everything else is identical underneath: engine (open/closed model,
workers/rate), the four reports (see [Reports](reports.md)), templating
functions, and `--dry-run`/`--quiet`/`--json`.

## `resonate hit` — Quick, Flag-Driven Runs

A one-off HTTP load test, gated on p95 latency and success rate
(non-zero exit if either fails — see [Assertions](#assertions)):

```sh
resonate hit http://localhost:8080/health --rate 50 --duration 30s --workers 20 \
  --assert 'latency_p95<=500ms' \
  --assert 'success_rate>=0.95'
```

A `POST` with a randomized body (see [Request Templating](#request-templating)):

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
a file instead of `-d` — the latter skips templating, so it's also the
safe choice for a body containing literal `{{ }}`.

`resonate hit` is HTTP-only, flag-driven, single-target. For WebSocket,
multi-target, multi-step flows, or anything reusable, use `resonate run`
instead. See the [CLI Reference](cli/resonate_hit.md) for every flag.

## `resonate run` — Scenario Files

For multi-target or reusable setups, describe the run in YAML:

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
[Scenarios](scenarios.md) for the full YAML schema, and the
[CLI Reference](cli/resonate_run.md) for `resonate run`'s own flags.

## Request Templating

`url`, `query`, `headers`, and `body` can embed `{{ }}` template
expressions, re-rendered independently per request. A field with no
`{{` is a plain literal — no overhead.

| Expression | Description |
| --- | --- |
| `.Seq` | Monotonic request counter, unique per request, starts at 0 |
| `.VU` | Calling virtual user's id — for per-worker sharding without an identities pool |
| `base64 s` / `base64Decode s` | Base64 encode/decode (e.g. Basic-Auth headers) |
| `env "VAR_NAME"` | Reads an environment variable (e.g. for tokens/secrets) |
| `hmacSHA256 "key" s` | Hex-encoded HMAC-SHA256, keyed — for request-signing |
| `jsonEscape s` | Escape `s` for embedding inside an already-quoted JSON string |
| `md5 s` / `sha256 s` | Hex-encoded digest |
| `now "layout"` | `"unix"`, `"unixmilli"`, or a Go time layout |
| `padLeft n "pad" s` / `padRight n "pad" s` | Pad `s` to at least `n` bytes |
| `randBool` | Random `true`/`false` |
| `randChoice "a" "b" "c"` | Picks one argument at random |
| `randDate "min" "max" "layout"` | Random time between two RFC3339 bounds, formatted like `now` |
| `randFloat min max` | Random float in `[min, max)` |
| `randInt min max` | Random int in `[min, max)` |
| `randString n` | Random alphanumeric string of length `n` |
| `truncate n s` | Cap `s` to at most `n` runes |
| `upper s` / `lower s` / `trim s` | Uppercase/lowercase/trim whitespace |
| `urlEncode s` / `urlDecode s` | Percent-encode/decode `s` for a query value |
| `uuid` | Random UUID (filler data, not crypto-secure) |

See [`examples/http-templated.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-templated.yaml).
Scenario files can also drive requests from a real CSV/JSON dataset — see
[Feeders](scenarios.md#feeders).

## Status, Header, and Body Checks

By default a response is successful if its status is 2xx/3xx.
`expect_status`/`expect_headers`/`expect_body` (`--expect-status`/
`--expect-header`/`--expect-body` on `resonate hit`, or per-entry on a
scenario file's `targets`/`flow`/`setup`) override that. All three can be
combined (every check must pass); any one alone replaces the default rule.

- `expect_status`: a status not in the list fails even if it's a
  2xx/3xx, and a listed status succeeds even outside that range (e.g.
  treating a 404 as the correct response for a not-found check).
- `expect_headers`: a non-empty value requires an exact match
  (case-insensitive name); an empty value only requires presence.
- `expect_body`: checks the body, keyed by rule and valued by the
  expected result — JSONPath (`json:status`, `json:items[0].id`, leading
  `$` optional), the same language against YAML (`yaml:status`), XPath 1.0
  (`xml://user/name`, `xml://user/@id`), `header:<Name>`, or `status`. A
  non-empty value requires an exact match; empty only requires the rule
  to evaluate. Narrower than `extract` — no `regex:`/`css:` here.

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
names, or unrecognized `expect_body` prefixes are rejected at
construction. A failing check reports which one(s) failed and why, and
stops the rest of that `flow`/`setup` iteration, same as any failed step
(see [Multi-Step Flows](scenarios.md#multi-step-flows-and-response-chaining)).
See [`examples/http-status-checks.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-status-checks.yaml).

## Assertions

Both commands check metrics from the completed report against
thresholds and exit non-zero if any fail — for CI gating. `resonate hit`
uses a repeatable `--assert 'metric<op>threshold'` flag; `resonate run`
uses a scenario file's `assertions:` list, with more conditions — see
[Scenarios](scenarios.md#assertions). Entries combine with AND.
`--assert` operators:

| Operator | Meaning |
| --- | --- |
| `>=` | Value must be at least the threshold |
| `<=` | Value must be at most the threshold |
| `==` | Value must equal the threshold exactly |
| `>` | Value must be strictly greater than the threshold |
| `<` | Value must be strictly less than the threshold |

See [Scenarios](scenarios.md#assertions) for the full metric list and
richer condition set (`gt`/`lt`/`is`/`in`/`around`/`deviatesAround`), and
[`examples/http-assertions.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-assertions.yaml)
for a worked example.

## Live Progress

Unless `--quiet`, both commands print a one-line progress update to
stderr every second — elapsed time, requests sent, ok/error counts,
current req/s. Stderr-only, so a `--json` report on stdout stays parseable.

## Dry Run

`--dry-run` sends exactly one real iteration and prints its result(s) —
status, latency, bytes, any error — instead of running the full test.
Use it to sanity-check a scenario before committing to a real run.

## Validation

Both commands fail fast, before any request is sent, on:

- A literal (non-templated) URL that isn't an absolute `scheme://host`
  URL — a URL that fails to parse resolves with no network I/O, so with
  no `--rate`/`load.rate` set the worker loop would otherwise free-spin
  at millions of iterations/sec instead of failing immediately. A
  *templated* URL can't be validated until rendered, so bad values there
  surface as a per-request error instead.
- Negative `--rate`/`load.rate`, `--workers`/`load.workers`, or stage
  `workers`/`rate`/`duration`.
- `http.setup`/`http.identities` set without `http.flow`.
- `http.targets`/`http.flow` set together, or neither set.
- No stop condition (`duration`/`requests`/`iterations`/`stages`) on a
  scenario file.
- An `expect_status`/`--expect-status` code outside 100-599, a blank
  `expect_headers`/`--expect-header` name, or an `expect_body`/
  `--expect-body` rule with an unrecognized prefix.

Three things warn instead, since there's a sane fallback: `--body` and
`--body-file` both set (the file wins); `--raw-body-file` set alongside
either (the raw file wins); and a run whose achieved rate fell short
because of insufficient `--workers` (see
[Execution Models](execution-models.md)).

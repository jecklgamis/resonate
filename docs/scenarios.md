# Scenarios

Scenario files (`resonate run <scenario.yaml>`) describe multi-target or
reusable load tests in YAML. This page covers `http.targets`/`http.flow`,
base URLs, feeders, identities, control flow, WebSocket, and assertions —
see [Execution Models](execution-models.md) for the `load:` block
(`rate`, `workers`, `stages`, `iterations`).

> **Treat a scenario file as trusted input, like a shell script.**
> `body_file`/`raw_body_file`/`feeder.file` read whatever local path they
> name and send its contents to whatever URL the scenario specifies, and
> the `env "VAR_NAME"` template function can put an environment variable's
> value into a request. Only run a scenario file (or `resonate hit`
> invocation) from a source you trust — the same rule you'd apply to a
> `Makefile` or CI config, not to a passive data file.

## Independent Targets

`http.targets`: independent, stateless requests, round-robin per
iteration — no state carried between requests, matching how most REST
APIs work.

```yaml
http:
  targets:
    - method: GET
      url: http://localhost:8080/users
    - method: POST
      url: http://localhost:8080/orders
      body: '{"item_id": {{randInt 1 100}}}'
```

Any target's status/header/body checks can be overridden with
`expect_status`/`expect_headers`/`expect_body` — see
[Status, Header, and Body Checks](usage.md#status-header-and-body-checks).
A body can also come from a file instead of an inline string: `body_file`
(templated, like `body`) or `raw_body_file` (sent exactly as-is — for a
large/binary payload, or one that would otherwise be misread as
containing `{{ }}` template syntax).

### Base URL

`http.base_url`, if set, is prepended to any `targets`/`flow`/`setup`
`url` that starts with `/`, so a scenario hitting one host doesn't need
to repeat it on every entry:

```yaml
http:
  base_url: http://localhost:8080
  targets:
    - url: /users
    - url: /orders
```

A `url` that doesn't start with `/` (already absolute, or a `{{ }}`
template) is left untouched; a relative `url` with no `base_url` set
fails fast at construction, the same way a malformed absolute URL would.
Only `targets`/`flow`/`setup` resolve against `base_url` — `ws` scenarios
aren't covered by this. See
[`examples/http-base-url.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-base-url.yaml).

## Feeders

`http.feeder`/`ws.feeder` hand out one row of data per iteration from a CSV
or JSON file — exposed to templates as `{{.Feeder.<column>}}` — instead of
relying purely on random values, for replaying real-looking data
(accounts, emails, product ids, ...) or actual exported production data.

```yaml
http:
  feeder:
    file: users.csv     # or users.json
    mode: sequential     # "sequential" (default, round-robin), "random", or "stream"
  targets:
    - method: POST
      url: http://localhost:8080/login
      body: '{"email": "{{.Feeder.email}}", "password": "{{.Feeder.password}}"}'
```

`users.csv` (first row is the column names):

```csv
email,password
alice@example.com,hunter2
bob@example.com,hunter3
```

Notes:
- `mode: sequential` (default) hands out rows round-robin, wrapping back
  to the first row once exhausted; `mode: random` picks a row uniformly
  at random each time instead. Both load the whole file into memory once,
  at construction.
- `mode: stream` is for a file too large to comfortably fit in memory
  (e.g. an exported production dataset) — reads incrementally in the
  background instead, holding only a small buffer regardless of file
  size. Behaves like `sequential` (round-robin, wraps around); there's no
  streaming `random` yet.
- For `http.flow`, one row is picked per iteration and stays the same
  across every step in that flow (like `.Identity`); `http.setup` picks
  its own row once per virtual user.
- A missing/empty `{{.Feeder.column}}` key renders as `""`, same as
  `{{.Vars.*}}`.
- Construction fails fast either way (missing file, malformed data, or an
  empty dataset) before any request is sent.

See [`examples/http-feeder.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-feeder.yaml)
and [`examples/data/users.csv`](https://github.com/jecklgamis/resonate/blob/main/examples/data/users.csv).

## Multi-Step Flows and Response Chaining

`http.flow`: an ordered sequence of requests run once per iteration, each
able to use values extracted from earlier steps' responses
(`{{.Vars.*}}`). Mutually exclusive with `targets`. Use this when a later
step needs a value from an earlier step's response (e.g. `POST /orders`
returns an id, `GET /orders/{id}` needs it).

```yaml
http:
  timeout: 5s

  # One identity per virtual user, assigned round-robin over the pool and
  # sticky for that worker's lifetime.
  identities:
    - username: alice
      account_id: "1001"
    - username: bob
      account_id: "1002"

  # Runs once per worker, before its first iteration. Extracted vars seed
  # {{.Vars.*}} for every iteration that worker runs afterward — "log in
  # once, reuse the token." Not counted in the report.
  setup:
    - method: POST
      url: http://localhost:8080/login
      body: '{"username": "{{.Identity.username}}"}'
      extract:
        token: json:access_token

  # Runs every iteration, steps in order. Each step can reference vars
  # extracted by any earlier step in the same flow (or from setup).
  flow:
    - method: POST
      url: http://localhost:8080/orders
      headers:
        Authorization: "Bearer {{.Vars.token}}"
      body: '{"account_id": "{{.Identity.account_id}}", "item": "widget"}'
      extract:
        order_id: json:id

    - method: GET
      url: "http://localhost:8080/orders/{{.Vars.order_id}}"
      headers:
        Authorization: "Bearer {{.Vars.token}}"
```

Notes:
- `extract` rules: `json:<JSONPath>` (real JSONPath, e.g. `json:id`,
  `json:items[0].id`, a leading `$` is optional), `yaml:<JSONPath>` (the
  same JSONPath language, against a YAML body), `xml:<XPath>` (real
  XPath 1.0, e.g. `xml://user/name`, `xml://user/@id`), `regex:<pattern>`
  (RE2 against the raw body text), `css:<selector>` (a CSS selector
  against an HTML body), `header:<Name>`, or `status`.
- If a flow step fails or returns a non-2xx/3xx status, the rest of that
  iteration's steps are skipped — the flow resumes fresh next iteration.
  A step can also set `expect_status`/`expect_headers`/`expect_body` to
  override what counts as success — see
  [Status, Header, and Body Checks](usage.md#status-header-and-body-checks).
- `identities` is optional; without it, `{{.Identity.*}}` renders empty.
- `{{.Vars.*}}` for a key that was never extracted renders as `""` rather
  than erroring.

See [`examples/http-flow.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-flow.yaml).

### Pauses and control flow

A flow/setup entry can also be a think-time pause or a control-flow
block instead of a plain request:

```yaml
flow:
  - method: GET
    url: http://localhost:8080/dashboard
    pause: 500ms        # sleep this long before the request
    pause_max: 1.5s      # optional: random duration in [pause, pause_max)

  - repeat: 3             # run the nested steps this many times in a row
    steps:
      - method: GET
        url: http://localhost:8080/poll

  - during: 10s            # run the nested steps repeatedly for this long
    steps:
      - method: GET
        url: http://localhost:8080/ping

  - if: '{{eq .Vars.role "admin"}}'   # run the nested steps only if true
    steps:
      - method: GET
        url: http://localhost:8080/admin
```

`repeat`/`during`/`if` are mutually exclusive on one entry, require at
least one nested `steps` entry, and nest freely. A failed request step
stops the whole flow, unwinding out of every enclosing block. See
[`examples/http-flow-control.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-flow-control.yaml).

## WebSocket

`protocol: ws` (scenario files only, not `resonate hit`) dials one
connection per iteration, sends `ws.messages` in order, then closes it.
Messages support the same response chaining as `flow`: a message with
`wait: true` and `extract` makes its response available to later messages
via `{{.Vars.*}}`.

```yaml
protocol: ws

load:
  duration: 30s
  rate: 20
  workers: 10

ws:
  url: ws://localhost:8080/socket
  identities:
    - token: abc123
  messages:
    - body: '{"type":"subscribe","channel":"orders"}'
      wait: true
      extract:
        session_id: json:session_id
    - body: '{"type":"ping","session":"{{.Vars.session_id}}"}'
      wait: true
```

A connection isn't reused across iterations — each iteration is a fresh
dial, message sequence, and close. `binary: true` on a message sends it
as a binary frame instead of text.

See [`examples/ws-basic.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/ws-basic.yaml).

## Assertions

A top-level `assertions:` list checks metrics from the completed run's
report against thresholds. If any fail, `resonate run` prints the report
as usual, then the failures, then exits with a non-zero status — for
gating a CI pipeline on load-test results. `resonate hit` has an
equivalent, single-expression `--assert 'metric<op>threshold'` flag (e.g.
`--assert 'success_rate>=0.99'`) covering the same metrics with a simpler
operator set — see [Usage](usage.md#assertions).

```yaml
assertions:
  - metric: success_rate
    min: "0.99"

  - metric: latency_p95
    max: 500ms

  - metric: rate
    min: "40"
    max: "60"
```

Each entry needs at least one condition; several may be set together,
combined with AND. Thresholds are duration strings (`"500ms"`) for
`latency_*` metrics, plain numbers otherwise. Supported metrics:
`requests`, `success`, `success_rate`, `error_rate`, `rate`,
`latency_min`, `latency_mean`, `latency_stddev`, `latency_p50`,
`latency_p90`, `latency_p95`, `latency_p99`, `latency_max`, and
`latency_p<N>` for any percentile `N` (`0`-`100`, fractional allowed —
e.g. `latency_p99.9`), not just the four fixed ones.

Beyond `min`/`max` (`gte`/`lte`), the following assertion conditions
are also supported: `gt`/`lt` (strict inequality), `is` (exact
equality), `in` (a list of valid values), `around`/`around_margin`
(within an absolute margin of a center value), and
`deviates_around`/`deviates_percent` (within a relative/percentage
margin of a target value) — the latter two are field pairs, both must be
set together, and both default to inclusive bounds (`around_exclusive`/
`deviates_exclusive: true` to exclude the boundary).

```yaml
assertions:
  - metric: rate
    gt: "0"                  # strictly greater than zero, not just >= 0

  - metric: requests
    in: ["100", "200"]       # exactly one of a small set of valid totals

  - metric: latency_mean
    around: "200ms"
    around_margin: "20ms"    # within 180ms-220ms inclusive

  - metric: rate
    deviates_around: "1000"
    deviates_percent: "5"    # within ±5% of 1000 req/s
    deviates_exclusive: true # ...but not exactly at the 5% boundary
```

An unknown metric name, or an entry with no condition set at all, is
rejected at load time — before any request is sent.

# Scenarios

Scenario files (`resonate run <scenario.yaml>`) describe multi-target or
reusable load tests in YAML. This page covers `http.targets`/`http.flow`,
base URLs, feeders, identities, control flow, WebSocket, Server-Sent Events,
gRPC, and assertions —
see [Execution Models](execution-models.md) for the `load:` block
(`rate`, `workers`, `stages`, `iterations`).

For machine-readable schema discovery, run `resonate schema`. The
same JSON Schema is checked into the repository at
[internal/config/scenario.schema.json](https://github.com/jecklgamis/resonate/blob/main/internal/config/scenario.schema.json).

> **Treat a scenario file as trusted input, like a shell script.**
> `body_file`/`raw_body_file`/`feeder.file` read a local path and send its
> contents wherever the scenario specifies, and `env "VAR_NAME"` can put
> an environment variable into a request. Only run a scenario file (or
> `resonate hit` invocation) from a source you trust.

## Independent Targets

`http.targets`: independent, stateless requests, round-robin per
iteration — no state carried between requests.

```yaml
http:
  targets:
    - method: GET
      url: http://localhost:8080/users
    - method: POST
      url: http://localhost:8080/orders
      body: '{"item_id": {{randInt 1 100}}}'
```

Any target's checks can be overridden with `expect_status`/
`expect_headers`/`expect_body` — see
[Status, Header, and Body Checks](usage.md#status-header-and-body-checks).
A body can also come from a file: `body_file` (templated, like `body`) or
`raw_body_file` (sent exactly as-is — for a large/binary payload, or one
that would otherwise be misread as containing `{{ }}`).

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
fails fast at construction, same as a malformed absolute URL. Only
`targets`/`flow`/`setup` resolve against `base_url` — `ws` and `grpc`
scenarios don't. See
[`examples/http-base-url.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-base-url.yaml).

## Feeders

`http.feeder` (and `ws.feeder`, `sse.feeder`, `grpc.feeder`) hands out one row of data per
iteration, exposed to templates as `{{.Feeder.<column>}}`, for replaying a real dataset instead of
random values. The rows come from a CSV or JSON file, or from a Kafka topic:

```yaml
http:
  feeder:
    file: users.csv     # or users.json; mode: sequential (default), random or stream
  targets:
    - method: POST
      url: http://localhost:8080/login
      body: '{"email": "{{.Feeder.email}}", "password": "{{.Feeder.password}}"}'
```

See the [Feeders guide](feeders.md) for file formats, the three modes, sizing, when a row is
picked, inline `rows:` (an array feeder), and the Go API, and the [Kafka Feeder guide](kafka-feeder.md) for topics. Runnable
examples: [`examples/http-feeder.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-feeder.yaml)
and [`examples/http-kafka-feeder.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-kafka-feeder.yaml).

## Multi-Step Flows and Response Chaining

`http.flow`: an ordered sequence of requests run once per iteration,
each able to use values extracted from earlier steps' responses
(`{{.Vars.*}}`). Mutually exclusive with `targets`. Use this when a
later step needs a value from an earlier step (e.g. `POST /orders`
returns an id, `GET /orders/{id}` needs it).

```yaml
http:
  timeout: 5s

  # One identity per virtual user, assigned round-robin and sticky for
  # that worker's lifetime.
  identities:
    - username: alice
      account_id: "1001"
    - username: bob
      account_id: "1002"

  # Runs once per worker, before its first iteration. Extracted vars seed
  # {{.Vars.*}} for every iteration afterward — "log in once, reuse the
  # token." Not counted in the report.
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
  `json:items[0].id`, leading `$` optional), `yaml:<JSONPath>` (same
  language against YAML), `xml:<XPath>` (real XPath 1.0, e.g.
  `xml://user/name`, `xml://user/@id`), `regex:<pattern>` (RE2 against
  the raw body), `css:<selector>` (against an HTML body), `header:<Name>`,
  or `status`.
- A failed or non-2xx/3xx step skips the rest of that iteration's steps
  (resuming fresh next iteration). A step can set `expect_status`/
  `expect_headers`/`expect_body` to override what counts as success —
  see [Status, Header, and Body Checks](usage.md#status-header-and-body-checks).
- `identities` is optional; without it, `{{.Identity.*}}` renders empty.
- `{{.Vars.*}}` for a never-extracted key renders as `""` rather than
  erroring.

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

## HTTP/3

`http3: true` under `http:` sends every request over HTTP/3 (HTTP over QUIC, on UDP)
instead of HTTP/1.1. It is opt-in, not negotiated, and every URL must be `https://`.
It also works under `sse:`.

```yaml
http:
  http3: true
  targets:
    - url: https://localhost:8443/proto
```

The full story — the CLI flag, the Go options, what it does and doesn't do,
troubleshooting, and a runnable example server — is in the **[HTTP/3 guide](http3.md)**.

## WebSocket

`protocol: ws` (scenario files only) dials one connection per iteration,
sends `ws.messages` in order, then closes it. Messages support the same
response chaining as `flow`: a message with `wait: true` and `extract`
makes its response available to later messages via `{{.Vars.*}}`.

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

## Server-Sent Events

`protocol: sse` (scenario files only) opens one stream per iteration: it sends the
request, reads the `text/event-stream` response, then closes it. Use it for anything
that pushes events over a long-lived HTTP response: live feeds, notifications, and the
streaming responses of many LLM and chat APIs. Each iteration reports one connect
result plus one result per event.

```yaml
protocol: sse

load:
  duration: 30s
  workers: 20

sse:
  url: http://localhost:8765/events
  events: 10          # succeed once 10 events arrive...
  duration: 15s       # ...or fail if they haven't within 15s
  timeout: 5s         # bounds connecting + response headers, not the stream
  headers:
    Authorization: Bearer {{.Identity.token}}
  identities:
    - token: abc123
    - token: def456
```

Everything else — the request fields, what counts as an event, when a stream ends
(`events` and `duration`), what gets reported, load profiles, troubleshooting, and a
runnable example server — is in the **[Server-Sent Events guide](sse.md)**.

## gRPC

`protocol: grpc` (scenario files only) makes gRPC calls — unary, and
server-, client- and bidirectional-streaming. Each iteration runs
`grpc.calls` in order over a shared connection and stops at the first failed
call, so a login-then-use flow chains with `extract` like HTTP flows and
WebSocket messages do. The schema comes from server reflection, `.proto`
files or a descriptor set.

```yaml
protocol: grpc

load:
  duration: 30s
  rate: 100
  workers: 50

grpc:
  address: localhost:50051
  plaintext: true
  calls:
    - method: acme.user.v1.UserService/GetUser
      message: '{"id":"{{randInt 1 1000}}"}'
      expect_code: [OK, NOT_FOUND]
```

Everything else — connecting and TLS, the schema sources, messages and
templating, metadata and auth, response checks, streaming, how results are
reported, tuning, troubleshooting, and a runnable example server — is in the
**[gRPC guide](grpc.md)**.

## Assertions

A top-level `assertions:` list checks metrics from the completed
report against thresholds. If any fail, `resonate run` prints the
report, then the failures, then exits non-zero. `resonate hit` has an
equivalent `--assert 'metric<op>threshold'` flag with a simpler
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

Each entry needs at least one condition; several combine with AND.
Thresholds are duration strings (`"500ms"`) for `latency_*` metrics,
plain numbers otherwise. Supported metrics: `requests`, `success`,
`success_rate`, `error_rate`, `rate`, `latency_min`, `latency_mean`,
`latency_stddev`, `latency_p50`, `latency_p75`, `latency_p90`, `latency_p95`,
`latency_p99`, `latency_max`, and `latency_p<N>` for any percentile `N`
(`0`-`100`, fractional allowed — e.g. `latency_p99.9`).

Beyond `min`/`max` (`gte`/`lte`): `gt`/`lt` (strict), `is` (exact
equality), `in` (a list of valid values), `around`/`around_margin`
(within an absolute margin of a center value), and
`deviates_around`/`deviates_percent` (within a relative margin of a
target value) — the latter two are field pairs, both required together,
defaulting to inclusive bounds (`around_exclusive`/
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

An unknown metric name, or an entry with no condition set, is rejected
at load time.

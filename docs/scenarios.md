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

`http.feeder`/`ws.feeder` hand out one row of data per iteration from a
CSV or JSON file, exposed to templates as `{{.Feeder.<column>}}` — for
replaying real-looking data instead of pure random values.

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
- `mode: sequential` (default) hands out rows round-robin, wrapping
  around; `mode: random` picks a row uniformly at random each time. Both
  load the whole file into memory once, at construction.
- `mode: stream` is for a file too large to fit in memory — reads
  incrementally in the background, holding only a small buffer. Behaves
  like `sequential`; no streaming `random` yet.
- For `http.flow`, one row is picked per iteration and stays the same
  across every step (like `.Identity`); `http.setup` picks its own row
  once per virtual user.
- A missing/empty `{{.Feeder.column}}` key renders as `""`, same as
  `{{.Vars.*}}`.
- Construction fails fast (missing file, malformed data, empty dataset)
  before any request is sent.

See [`examples/http-feeder.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-feeder.yaml)
and [`examples/data/users.csv`](https://github.com/jecklgamis/resonate/blob/main/examples/data/users.csv).

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

`protocol: sse` (scenario files only) opens one stream per iteration: it sends
the request, reads the `text/event-stream` response, then closes it. Use it for
anything that pushes events over a long-lived HTTP response — live feeds,
notifications, and the streaming responses of many LLM and chat APIs. A fresh
request is made every iteration; a stream is never reused.

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

Streaming APIs usually take a `POST` with a body and send events until they are
done, closing the stream themselves, so no `events` or `duration` is needed:

```yaml
protocol: sse

load:
  requests: 50
  workers: 10

sse:
  url: http://localhost:8765/v1/stream
  method: POST
  headers:
    Content-Type: application/json
  body: '{"prompt":"hello from vu {{.VU}}","stream":true}'
```

`method` defaults to `GET`. `url`, `query`, `headers` and `body` support
templates, and `body_file` / `raw_body_file` work as for HTTP. `identities` and
`feeder` behave as for the other protocols. `insecure`, `ca_file`,
`cert_file`/`key_file`, `h2c` and `max_response_body` (which caps the bytes read
per stream) are available under `sse`, like their `http.*` namesakes. `resonate
hit` has no SSE flag.

**What an event is.** An event is a block of lines with at least one `data:`
field, ended by a blank line. Several `data:` lines are still one event.
Comment lines (`: ...`, often used as keep-alives) and blocks with no `data:`
field aren't events and aren't counted.

**When a stream ends.** `events` and `duration` decide:

| `events` | `duration` | The stream ends when... | Outcome |
| --- | --- | --- | --- |
| `N` | unset | `N` events have arrived | Success. The server closing it first, or `timeout` elapsing first, is a failure. |
| `N` | `D` | `N` events have arrived | Success. `D` elapsing first is a failure. |
| unset | `D` | `D` elapses, or the server closes it | Success: a listen window. |
| unset | unset | the server closes it | Success. **Still open at `timeout` is a failure** (the error says "set sse duration or events to end it sooner"). |

That last rule is deliberate: without it a never-ending stream would pin its
worker for the whole run. `timeout` also bounds connecting and receiving the
response headers, so a `duration` longer than `timeout` works.

**What gets reported.** Each iteration gives one *connect* result (latency = time
to the response headers; it fails on a non-2xx/3xx status, or a `Content-Type`
other than `text/event-stream`) plus one result per event (latency = the gap
since the previous event, or since the headers for the first; bytes = that
event's size on the wire, including any comment lines before it). So `requests`
in the report counts connects *and* events, as a WebSocket scenario counts its
connect and each message. A stream that ends unsuccessfully adds one last failed
result with the reason, such as `stream ended after 4 of 10 events`. Results are
held until the stream ends, so memory grows with the events per stream: give
long-lived streams an `events` or a `duration`.

A run that ends while a stream is still connecting or still open isn't counted as
a failure. SSE doesn't reconnect or send `Last-Event-ID`, and doesn't look at
event names or payloads; if you need to check what the events contain, that is
what a future `expect_body` for events would be for.

See [`examples/sse-basic.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/sse-basic.yaml)
(a `GET` that reads 10 events) and
[`examples/sse-post.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/sse-post.yaml)
(a streaming API called with a `POST`).

### Trying the SSE examples

`examples/testsseserver` is a small Server-Sent Events server (standard library
only) that serves what the SSE examples and the scenarios above call, so you can
run and validate them without a service of your own. It keeps no state, so it is a
demo target and not a benchmark.

```sh
make example-sse-server                  # listens on :8765
resonate run examples/sse-basic.yaml --dry-run
resonate run examples/sse-post.yaml --dry-run
```

| Endpoint | What it does | What it shows |
| --- | --- | --- |
| `GET /events` | Sends 12 events 20ms apart (with a keep-alive comment before every fourth), then closes. `?n=` and `?interval=` override. | `events: 10` ending a stream the server would have kept going. |
| `POST /v1/stream` | An LLM-style API: streams the `prompt` a word at a time as `{"token": ...}` events, then `[DONE]`, then closes. A missing or empty prompt is a `400`. | A stream the server ends itself, so no `events` or `duration`. |
| `GET /endless` | Never ends. | A stream with no bound failing at `timeout`. |
| `GET /json` | A `200` that isn't an event stream. | The content-type check. |
| `GET /error` | `503`, or the status in `?code=`. | A non-2xx connect. |
| `GET /healthz` | `ok`. | That the server is up. |

Flags: `-addr` (default `:8765`; a scenario's `url` must name the same port),
`-events` (the `/events` count, default 12), `-event-interval` (default 20ms),
`-log-requests` and `-stats-interval` (below).

**Logging.** The server logs like an access log: a `call` line when a request
arrives and a `done` line when it finishes (status, duration, events sent, bytes),
plus a line about what each stream did. A stream shows up the moment it opens, not
only when it ends, and credentials are never echoed (only the scheme of an
`Authorization` header). When resonate reads its `events` and hangs up, the server
notices:

```text
call GET   /events                from=[::1]:51843 auth=Bearer
events: stream opened, 12 events 20ms apart
events: client went away after 10 of 12 events
done GET   /events                200 204.514ms events=10 bytes=372 from=[::1]:51843
```

That is a lot of output under load, so for a performance run pass
`-log-requests=false` (`make example-sse-server ARGS=-log-requests=false`): the
per-request lines stop and, every `-stats-interval` (default 10s) while there is
traffic, one summary line is printed instead. A request counts as failed when the
server returns a 5xx; a 4xx, such as the `400` for a missing prompt, is the client's
doing and doesn't. Ctrl-C prints the totals and shuts down.

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
`latency_stddev`, `latency_p50`, `latency_p90`, `latency_p95`,
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

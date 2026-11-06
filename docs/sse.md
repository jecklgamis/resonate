# Server-Sent Events

resonate load-tests Server-Sent Events (SSE) endpoints: servers that answer an
HTTP request with a response that stays open and pushes events down it. That is
how live feeds and notifications work, and how many LLM and chat APIs stream their
answers a token at a time. A scenario (`protocol: sse`) opens a stream per
iteration, reads events from it, and reports on both the connection and every
event.

This page is the full guide: a [quick start](#quick-start), a checklist for
[testing your own service](#testing-your-own-service), the reference for the
[request](#the-request), [what counts as an event](#what-counts-as-an-event),
[when a stream ends](#ending-a-stream), [what gets reported](#what-gets-reported),
how to [size a load](#load-profiles), [troubleshooting](#troubleshooting), and
the [example server](#the-example-server) you can run against.

## Quick start

No service of your own needed: the repository ships an example SSE server. From a
checkout of the repo:

```sh
make example-sse-server        # in one terminal: listens on :8765
```

Save this as `sse.yaml`:

```yaml
protocol: sse

load:
  duration: 10s
  rate: 20
  workers: 10

sse:
  url: http://localhost:8765/events
  events: 10          # each stream succeeds once 10 events have arrived
  timeout: 5s         # bounds connecting and the response headers

assertions:
  - metric: success_rate
    min: "0.99"
```

First make one real iteration and look at what happened:

```sh
resonate run sse.yaml --dry-run
```

```text
[0]  sse  status=200  latency=2.626ms   bytes_out=0  bytes_in=0   ok
[1]  sse  status=200  latency=21.163ms  bytes_out=0  bytes_in=47  ok
[2]  sse  status=200  latency=19.954ms  bytes_out=0  bytes_in=33  ok
    ... seven more events ...
[10] sse  status=200  latency=19.958ms  bytes_out=0  bytes_in=33  ok
```

Result 0 is the *connect*; each of the other ten is an *event*. Then run the
whole thing:

```sh
resonate run sse.yaml
```

You get the usual stdout summary plus `report.html`, `report.json` and
`results.jsonl` (see [Reports](reports.md)). `rate: 20` means 20 *iterations*
(streams) per second, and each stream here is eleven results, so expect roughly
220 to 240 requests/s in the report. The run exits non-zero if the assertion fails.

## Testing your own service

1. **Confirm the endpoint really is an event stream.** `curl -N -H 'Accept:
   text/event-stream' <url>` (add `-X POST -d ...` for a POST API) should print
   `data:` lines as they happen and `Content-Type: text/event-stream` in the
   headers (`-i`). If it doesn't, resonate will tell you so with a content-type
   error.
2. **Decide how each stream should end.** Does the server finish and close it (an
   LLM reply) or does it go on forever (a feed)? That decides whether you need
   `events` or `duration`. See [Ending a stream](#ending-a-stream).
3. **Sort out authentication:** a header, usually. See [Authentication and
   per-user data](#authentication-and-per-user-data).
4. **Check one iteration** with `--dry-run` and read the results.
5. **Start gently,** then ramp with `load.stages` or by raising `rate` and
   `workers` between runs. See [Load profiles](#load-profiles).
6. **Add assertions** for the numbers you care about, so the run can fail a build.
7. **Remember what you are loading.** A stream holds a connection and often a
   backend job open for its whole life, so the thing that limits you is usually
   concurrent streams, not requests per second.

## The request

```yaml
sse:
  url: http://localhost:8765/v1/stream
  method: POST
  headers:
    Content-Type: application/json
  body: '{"prompt":"hello from vu {{.VU}}"}'
  events: 10
  timeout: 5s
```

| Field | Meaning |
| --- | --- |
| `url` | Required. An absolute `http://` or `https://` URL (or a `{{ }}` template). Supports templates. |
| `method` | Default `GET`. Streaming APIs are often `POST`. |
| `query` | Query parameters, as a map. Supports templates. |
| `headers` | Request headers. Support templates. |
| `body`, `body_file`, `raw_body_file` | The request body, as for HTTP: `body` and `body_file` are templated, `raw_body_file` is sent as-is. |
| `events` | End the stream successfully after this many events. See [Ending a stream](#ending-a-stream). |
| `duration` | How long to listen. See [Ending a stream](#ending-a-stream). |
| `timeout` | Bounds connecting and receiving the response headers (default `30s`). Also the cap on a stream with no `events` or `duration`. |
| `identities`, `feeder` | Per-user and per-iteration data, as for the other protocols. |
| `insecure`, `ca_file`, `cert_file`, `key_file` | TLS options, as for `http`. |
| `h2c` | Plaintext HTTP/2 (prior knowledge). See [Connections](#connections-and-limits). |
| `http3` | Use HTTP/3 (QUIC over UDP); the `url` must be `https://`. See the [HTTP/3 guide](http3.md). Can't be combined with `h2c`. |
| `max_response_body` | Cap the bytes read per stream; the stream then looks as if the server closed it. |

resonate adds `Accept: text/event-stream` and `Cache-Control: no-cache` unless you
set those headers yourself. `resonate hit` has no SSE flag: SSE is scenario-file
(and library) only.

The templates see `{{.Seq}}`, `{{.VU}}`, `{{.Identity.<key>}}` and
`{{.Feeder.<column>}}`; the functions are listed under
[Request Templating](usage.md#request-templating). There is no `{{.Vars.*}}`:
SSE has no `extract`, since there is nothing between streams to chain.

## What counts as an event

An event is a block of lines with at least one `data:` field, ended by a blank
line. These details are what the count follows:

- **Several `data:` lines** in one block are still one event.
- **Comment lines** (`: keep-alive`) and **blocks with no `data:` field** (just an
  `id:`, `event:` or `retry:`) are not events and are not counted.
- A `data` line with no colon counts as a `data:` field with an empty value.
- Lines end in `\n` or `\r\n`. A lone `\r` is not recognised.
- A **single line** longer than 4 MiB ends the stream with an error.

resonate doesn't interpret event contents (the `event:` name, `id:`, or the data);
it counts events and measures them. It does not reconnect or send `Last-Event-ID`.

## Ending a stream

`events` and `duration` decide when a stream is done:

| `events` | `duration` | The stream ends when... | Outcome |
| --- | --- | --- | --- |
| `N` | unset | `N` events have arrived (resonate then closes it) | Success. The server closing it first, or `timeout` elapsing first, is a failure. |
| `N` | `D` | `N` events have arrived | Success. `D` elapsing first is a failure. |
| unset | `D` | `D` elapses, or the server closes it | Success: a listen window. |
| unset | unset | the server closes it | Success. **Still open at `timeout` is a failure.** |

That last rule is deliberate: without it a never-ending stream would pin its worker
for the whole run. The error says what to do:

```text
stream still open after the 30s timeout; set sse duration or events to end it sooner
```

`timeout` has two jobs: it bounds connecting and receiving the response headers (so
a `duration` longer than `timeout` is fine), and it is the cap when neither `events`
nor `duration` is set.

A **streaming API that finishes its reply and closes** (the LLM case) needs neither:

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
  body: '{"prompt":"hello from vu {{.VU}}"}'
```

A **feed that never ends** needs a bound, `events` or `duration` (or both). To
measure only how long the first event takes, stop after one:

```yaml
protocol: sse

load:
  requests: 100
  workers: 10

sse:
  url: http://localhost:8765/v1/stream
  method: POST
  headers:
    Content-Type: application/json
  body: '{"prompt":"one two three four five"}'
  events: 1
```

That gives two results per iteration: the connect (time to the response headers) and
the first event (time from the headers to the first event), which is the "time to
first token" of a streaming API.

## What gets reported

A stream isn't one request, so, like a WebSocket connection, it is reported by its
parts. Each iteration gives:

- **One connect result.** Its latency is the time to the response headers. It fails
  on a non-2xx/3xx status, or on a `Content-Type` other than `text/event-stream`.
  `bytes_out` is the request body.
- **One result per event.** Its latency is the gap since the previous event (the
  first: since the headers arrived). `bytes_in` is that event's size on the wire,
  including any comment lines before it.
- **A final failed result** if the stream ends badly, carrying the reason:
  `stream ended after 4 of 10 events`, `timed out after 15s with 3 of 10 events`,
  or the transport error.

So `requests` in the report counts connects *and* events, and events per second falls
out of the usual rate. The latency percentiles mix connects and event gaps; to look
at one of them alone, shape the scenario for it (for example `events: 1`, above).

Details worth knowing:

- A **non-2xx status** is a failed connect result with that status code, shown in the
  status breakdown (and no events follow).
- Results are held until the stream ends, so **memory grows with the events per
  stream**: give long-lived streams an `events` or a `duration`.
- A run that ends while a stream is still **connecting** or still **open** is not
  counted as a failure, and neither is a stream resonate closes itself once `events`
  is reached.
- `bytes` and `latency` are measured on resonate's side; there is no check on what the
  events contain.

## Authentication and per-user data

Streams are usually authenticated with a header. For per-user credentials, give each
virtual user an identity:

```yaml
protocol: sse

load:
  requests: 20
  workers: 4

sse:
  url: http://localhost:8765/events
  events: 5
  headers:
    Authorization: Bearer {{.Identity.token}}
  identities:
    - token: abc123
    - token: def456
```

Identities are handed out round-robin, one per virtual user, and stay with that user
(see [Multi-Step Flows](scenarios.md#multi-step-flows-and-response-chaining) for how
they behave over a VU's lifetime). For data that should change every iteration (a
different user, topic or prompt), use a `feeder`, with the same `file` and `mode` as for
[HTTP](feeders.md):

```yaml
sse:
  url: http://localhost:8765/events?user={{.Feeder.email}}
  feeder:
    file: examples/data/users.csv
```

## Load profiles

SSE scenarios use the same `load:` block as every other protocol, so everything in
[Execution Models](execution-models.md) applies. Two things are specific to streams:

- **`rate` and `requests` count iterations,** that is, whole streams. A stream that
  delivers ten events is eleven results in the report.
- **Concurrency is open streams.** Each worker holds one stream open for as long as
  it lasts, so `workers` is how many streams are open at once. To hold a number of
  streams open for a while, give each a `duration` and set `workers` to that number:

<!-- docs-test: parse-only -->
```yaml
protocol: sse

load:
  duration: 60s
  workers: 500          # 500 streams open at once

sse:
  url: http://localhost:8765/endless
  duration: 55s         # each stream listens for 55s, then the worker opens another
```

A ramp, gated on the results:

```yaml
protocol: sse

load:
  stages:
    - { duration: 10s, workers: 20, rate: 50 }    # ramp up
    - { duration: 30s, workers: 20, rate: 50 }    # hold
    - { duration: 5s,  workers: 0,  rate: 0 }     # ramp down

sse:
  url: http://localhost:8765/events
  events: 10

assertions:
  - metric: success_rate
    min: "0.999"
```

## Connections and limits

By default every stream gets its own HTTP/1.1 connection, so *N* open streams are *N*
connections from the machine running resonate (and *N* file descriptors: the default
limit is often low, 256 on macOS, so raise it with `ulimit -n` before holding hundreds
or thousands of streams open).

`h2c: true` uses plaintext HTTP/2 instead, which multiplexes streams over shared
connections; a server typically allows about 100 concurrent streams on one connection,
past which new streams wait. The client and server on one machine also share its CPU,
so treat localhost numbers as relative.

## Troubleshooting

Most problems show up at `--dry-run` or when the scenario loads.

| You see | What it means and what to do |
| --- | --- |
| `unexpected content-type "application/json" (want text/event-stream)` | The endpoint answered with something other than an event stream: the wrong path, or the server only streams for certain requests (a `POST`, an `Accept` or another header, a body field like `"stream": true`). |
| A connect result with `status=401`, `403` or `404` (no error text) | The server refused or didn't find it. Check the URL and the credentials. |
| `connect timed out after 5s` | No response headers within `timeout`. The server is slow to start streaming, or unreachable. |
| `stream still open after the 30s timeout; set sse duration or events to end it sooner` | An unbounded stream. Add `events` or `duration`. |
| `stream ended after 4 of 10 events` | The server closed the stream before sending `events` events. |
| `timed out after 15s with 3 of 10 events` | The events came too slowly for `duration` (or `timeout`). |
| Far fewer events than you expect | Comment lines and blocks without a `data:` field aren't counted; check what the server really sends with `curl -N`. |
| Events arrive in bursts, or only when the stream ends | Something between resonate and the server (a proxy, a CDN) is buffering the response. Turn response buffering off for the SSE path there. |
| `bufio.Scanner: token too long` | One line in the stream exceeded 4 MiB. |
| `sse.url is required` / `invalid url ...: must be an absolute URL` | The scenario has no `url`, or it isn't `http://` or `https://`. |
| `invalid sse.duration` / `invalid sse.timeout` | Not a Go duration such as `500ms`, `5s`, `2m`. |
| `sse events must be >= 0` | A negative `events`. |
| Many streams fail to open at high `workers` | Likely a connection or file-descriptor limit on the resonate side, or a per-client connection limit on the server. See [Connections and limits](#connections-and-limits). |

## From Go

SSE is available through the fluent `Scenario` builder (`SSE(method, url)`, then `Events`/
`StreamDuration` and the usual request setters) and through the struct API
(`resonate.NewSSEGenerator`, `SSETarget`, `SSEOptions`), with every field described on this
page. See [Library](library.md#server-sent-events) for worked examples of both.

## The example server

`examples/testsseserver` is a small SSE server (standard library only) that serves
what the SSE examples and the scenarios on this page call, so you can run and
validate them without a service of your own. It keeps no state, so it is a demo
target and not a benchmark.

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

Flags: `-addr` (default `:8765`; a scenario's `url` must name the same port), `-events`
(the `/events` count, default 12), `-event-interval` (default 20ms), `-log-requests`
and `-stats-interval` (below).

### Its logs

The server logs like an access log: a `call` line when a request arrives and a `done`
line when it finishes (status, duration, events sent, bytes), plus a line about what
each stream did. A stream shows up the moment it opens, not only when it ends, and
credentials are never echoed (only the scheme of an `Authorization` header). When
resonate reads its `events` and hangs up, the server notices:

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

## Example scenarios

| File | Shows | Needs |
| --- | --- | --- |
| [`sse-basic.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/sse-basic.yaml) | A `GET` that reads 10 events, with identities and an auth header | The example SSE server |
| [`sse-post.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/sse-post.yaml) | A streaming API called with a `POST`, ended by the server | The example SSE server |

## Not supported

- **`resonate hit` has no SSE flags;** SSE is scenario-file (and library) only.
- **No checks or extraction on event contents.** `expect_body` and `extract` work on
  HTTP responses, not on SSE events, and there is no filter on event names.
- **No reconnecting and no `Last-Event-ID`:** a dropped stream is a finished (or failed)
  stream, not something resonate resumes.
- **A lone `\r` line ending** isn't recognised.

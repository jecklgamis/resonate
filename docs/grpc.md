# gRPC

resonate load-tests gRPC services: unary calls and all three streaming
kinds (server, client and bidirectional), driven from a scenario file
(`protocol: grpc`) or from Go. Messages are written as JSON and templated
like HTTP bodies, there is no code generation, and the schema comes from
server reflection, `.proto` files or a descriptor set.

This page is the full guide: a [quick start](#quick-start), a checklist for
[testing your own service](#testing-your-own-service), the reference for every
part of a gRPC scenario, [streaming](#streaming), how to
[read the results](#reading-the-results), [troubleshooting](#troubleshooting),
and the [example server](#the-example-server) you can run against.

## Quick start

No service of your own needed: the repository ships an example gRPC server.
From a checkout of the repo:

```sh
make example-grpc-server        # in one terminal: listens on :50051
```

Save this as `grpc.yaml`:

```yaml
protocol: grpc

load:
  duration: 10s
  rate: 50
  workers: 10

grpc:
  address: localhost:50051
  plaintext: true               # the example server speaks no TLS
  calls:
    - method: acme.user.v1.UserService/CreateUser
      message: '{"name":"{{randChoice "alice" "bob"}}","age":{{randInt 18 65}}}'
      extract:
        user_id: json:id        # keep the new user's id for the next call
    - method: acme.user.v1.UserService/GetUser
      message: '{"id":"{{.Vars.user_id}}"}'
      expect_body:
        json:name: ""           # the response must have a name

assertions:
  - metric: success_rate
    min: "0.99"
```

First make one real iteration and look at what happened:

```sh
resonate run grpc.yaml --dry-run
```

```text
[0] grpc  status=200  latency=471µs  bytes_out=7  bytes_in=29  ok
[1] grpc  status=200  latency=122µs  bytes_out=8  bytes_in=29  ok
```

Each call is one result. Then run the whole thing:

```sh
resonate run grpc.yaml
```

You get the usual stdout summary plus `report.html`, `report.json` and
`results.jsonl` (see [Reports](reports.md)). `rate: 50` means 50
*iterations* per second, and each iteration here is two calls, so expect
about 100 requests/s. The run exits non-zero if the assertion fails, which is
how you gate a CI job on it.

## Testing your own service

1. **Find out how to reach it:** the address, whether it uses TLS, and whether
   it exposes server reflection. A tool like `grpcurl <address> list` shows
   both the services and whether reflection is on.
2. **Pick the schema source.** With reflection on, nothing else is needed. With
   it off, point resonate at your `.proto` files (`proto_files`) or a
   descriptor set. See [Where the schema comes from](#where-the-schema-comes-from).
3. **Write a scenario** with the address, `plaintext: true` if it has no TLS,
   and the calls: `method` as `package.Service/Method`, `message` as JSON.
4. **Check one iteration** with `--dry-run`. Errors here are cheap: a wrong
   method name, a message that doesn't match the request type, or an
   unreachable server all say so plainly (see [Troubleshooting](#troubleshooting)).
5. **Run a gentle load first,** then ramp it with `load.stages` or by raising
   `rate` between runs. See [Load profiles](#load-profiles).
6. **Add assertions** for the numbers you care about, so the run can fail a build.
7. **Make sure the service itself is safe under concurrency before blaming
   the numbers.** A load test is also a concurrency test; see the
   [worked example](#worked-example-a-real-service).

## Connecting

```yaml
grpc:
  address: localhost:50051
  plaintext: true
  timeout: 5s
  connections: 2
```

| Field | Meaning |
| --- | --- |
| `address` | Required. `host:port`, or anything grpc-go's resolver accepts, such as `unix:///path/to.sock`. |
| `plaintext` | Connect without TLS. Otherwise TLS is used. |
| `insecure` | Skip verifying the server's certificate. TLS only. |
| `ca_file` | Extra CA certificates (PEM) to trust, for a private CA. TLS only. |
| `cert_file`, `key_file` | A client certificate, for mutual TLS. Both or neither. TLS only. |
| `timeout` | The deadline for each unary call, and for the schema lookup. Default `30s`. For streams see [Ending a stream](#ending-a-stream). |
| `connections` | Spread virtual users over this many separate connections. Default `1`. See [Connections](#connections-and-concurrency). |

`plaintext` can't be combined with `insecure`, `ca_file`, `cert_file` or
`key_file`; that is rejected up front rather than silently ignored.

## Where the schema comes from

resonate needs each method's request and response types. In order of
precedence:

1. **`proto_files`**, plus `import_paths`: compiled in-process, no `protoc`
   needed. `import_paths` are the roots your files and their imports resolve
   against; they default to the directories containing `proto_files`, so
   imports that sit beside the files just work. Standard imports such as
   `google/protobuf/timestamp.proto` are built in.
2. **`descriptor_set`**: a file written with
   `protoc --include_imports --descriptor_set_out=api.pb api/user.proto` (or
   `buf build -o api.pb`). `--include_imports` is required.
3. Otherwise **server reflection**, queried once when the scenario loads (both
   the `v1` and `v1alpha` services are tried).

`proto_files` and `descriptor_set` are mutually exclusive, and
`import_paths` without `proto_files` is an error. Relative paths are relative
to where you run `resonate`.

Which to use? Reflection is zero-config, so it's the default choice for
development servers. Many production servers turn it off; then `proto_files`
is simplest, and a prebuilt `descriptor_set` is handy when you'd rather not
ship `.proto` sources. [`examples/grpc-proto-files.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/grpc-proto-files.yaml)
shows the `.proto` route, with its [`.proto`](https://github.com/jecklgamis/resonate/blob/main/examples/proto/user.proto).

Reflection is the one check that touches the network at load time: a server
that is down, or has reflection off with no other source given, fails the
scenario immediately, with an error saying which.

## Calls

`grpc.calls` is the sequence each iteration makes, in order, over a shared
connection. The iteration stops at the first failed call. One call is just a
sequence of one; a login-then-use flow chains calls with
[`extract`](#chaining-calls).

Each call needs a `method` (`package.Service/Method`, with an optional leading
`/`). The method's schema decides whether it is unary or one of the streaming
kinds; you never declare the kind. What the call's other fields mean depends on
it:

| Kind | Request | Fields | Ends when |
| --- | --- | --- | --- |
| Unary | one | `message` | the response arrives |
| Server streaming | one | `message`, `receive`, `duration` | see [Ending a stream](#ending-a-stream) |
| Client streaming | a sequence | `messages`, `repeat`, `send_interval` | the single response arrives |
| Bidirectional | a sequence | `messages`, `repeat`, `send_interval`, `receive`, `duration` | see [Ending a stream](#ending-a-stream) |

A field that doesn't suit the method's kind (`receive` on a unary call,
`message` on a client-streaming one, and so on) is rejected when the scenario
loads, with an error naming the kind. Everything about streams is in
[Streaming](#streaming).

## Messages

`message` is the request as protobuf JSON. It supports the usual `{{ }}`
templates (the functions are listed under
[Request Templating](usage.md#request-templating)), plus these values:

| Template | Value |
| --- | --- |
| `{{.Seq}}` | A counter, unique per iteration and constant across the calls within one. |
| `{{.VU}}` | The virtual user's number. |
| `{{.Identity.<key>}}` | The VU's [identity](#metadata-and-authentication). |
| `{{.Feeder.<column>}}` | This iteration's row from a [feeder](feeders.md). |
| `{{.Vars.<name>}}` | A value [extracted](#chaining-calls) from an earlier call. |

Details worth knowing:

- Write field names as in the `.proto` (`created_at`) or in lowerCamelCase
  (`createdAt`); both parse. Enums can be names or numbers, and well-known types
  take their JSON forms (a `Timestamp` is an RFC 3339 string).
- A message with no `{{` is parsed once, when the scenario loads, so a typo or a
  wrong field name fails before the run starts. A templated message that renders
  to invalid JSON fails that call instead.
- `message_file` reads the message from a file (also templated). If both
  `message` and `message_file` are set, the file wins, with a warning.
- An omitted `message` sends the empty request, so a method that takes no
  arguments needs none.

## Metadata and authentication

gRPC headers are called metadata. `grpc.metadata` is sent on every call, and a
call's own `metadata` adds to it, overriding a same-named key. Keys are
lowercased and values support templates.

For per-user credentials, give each virtual user an identity:

```yaml
protocol: grpc

load:
  requests: 20
  workers: 4

grpc:
  address: localhost:50051
  plaintext: true
  metadata:
    authorization: Bearer {{.Identity.token}}
  identities:
    - token: abc123
    - token: def456
  calls:
    - method: acme.user.v1.UserService/GetUser
      message: '{"id":"nobody"}'
      expect_code: [NOT_FOUND]
```

Identities are handed out round-robin, one per virtual user, and stay with that
user. See [Multi-Step Flows](scenarios.md#multi-step-flows-and-response-chaining)
for how they behave over a VU's lifetime. For data that changes every iteration
(ids, names) use a `feeder`, with the same `file` and `mode` options as for
[HTTP](feeders.md).

## Checking responses

By default a call succeeds on status `OK` and anything else is a failure.

- **`expect_code`** lists the status names that count as success, so an expected
  error can pass: `[OK, NOT_FOUND]`. Names are gRPC's canonical ones:
  `OK`, `CANCELLED`, `UNKNOWN`, `INVALID_ARGUMENT`, `DEADLINE_EXCEEDED`,
  `NOT_FOUND`, `ALREADY_EXISTS`, `PERMISSION_DENIED`, `RESOURCE_EXHAUSTED`,
  `FAILED_PRECONDITION`, `ABORTED`, `OUT_OF_RANGE`, `UNIMPLEMENTED`, `INTERNAL`,
  `UNAVAILABLE`, `DATA_LOSS`, `UNAUTHENTICATED`.
- **`expect_body`** checks the response with the same rule language as HTTP
  ([Status, Header, and Body Checks](usage.md#status-header-and-body-checks)):
  `json:<path>`, `yaml:<path>`, `regex:<pattern>`, `header:<Name>` and
  `status`. A non-empty expected value must match exactly; an empty one only
  requires the rule to evaluate, so `json:name: ""` means "has a name". Expected
  values are literal, not templated. With `expect_body` but no `expect_code`,
  the status must still be `OK`.

The response is rendered back to protobuf JSON for these rules: canonical
lowerCamelCase field names, with zero-valued fields included (so a `count` that
is 0 is still there). 64-bit integers appear as JSON strings, as the protobuf
JSON mapping says. `header:<Name>` reads response headers and trailers, and
`status` is the gRPC status code *number* (0 for `OK`).

## Chaining calls

`extract` pulls values out of a response into variables that later calls in the
same iteration read as `{{.Vars.<name>}}`; this is how
[the quick start](#quick-start) passes the new user's id to `GetUser`:

```yaml
    - method: acme.user.v1.UserService/CreateUser
      message: '{"name":"alice","age":30}'
      extract:
        user_id: json:id
```

The rules are the same as for `expect_body`. A variable that was never
extracted renders as an empty string, not an error.

## Streaming

A streaming call is configured with the fields from [the table above](#calls).
All three kinds are in `examples/grpc-streaming.yaml`; here is a runnable
scenario with one of each:

```yaml
protocol: grpc

load:
  requests: 5
  workers: 2

grpc:
  address: localhost:50051
  plaintext: true
  timeout: 10s
  calls:
    - method: acme.feed.v1.Feed/Subscribe     # server streaming
      message: '{"topic":"orders"}'
      receive: 5
    - method: acme.feed.v1.Feed/Upload        # client streaming
      messages: ['{"id":"{{uuid}}","payload":"hello"}']
      repeat: 10
      send_interval: 10ms
      expect_body:
        json:accepted: "10"
    - method: acme.feed.v1.Feed/Chat          # bidirectional
      messages: ['{"text":"hi"}', '{"text":"there"}']
      receive: 3
```

### Sending

`messages` are sent in order, the whole list `repeat` times (default 1), with
`send_interval` between consecutive messages, and then the send side is closed.
Each message is rendered *as it is sent*, so `{{uuid}}` and `{{randInt ...}}`
differ every time.

Sending runs concurrently with receiving, so on a bidirectional call the sends
do not wait for replies (there is no lockstep "send one, wait for the reply"
mode). Templates see `{{.Vars.*}}` as they were when the stream opened, not
values extracted from messages that arrive mid-stream.

### Ending a stream

For server-streaming and bidirectional calls, `receive` and `duration` decide
when a stream is done:

| `receive` | `duration` | The stream ends when... | Outcome |
| --- | --- | --- | --- |
| `N` | unset | `N` messages have arrived (resonate then cancels it) | Success. The server closing it first, or `timeout` elapsing first, is a failure. |
| `N` | `D` | `N` messages have arrived | Success. `D` elapsing first is a failure. |
| unset | `D` | `D` elapses, or the server closes it | Success: a listen window. |
| unset | unset | the server closes it | Success. **Still open at `timeout` is a failure** ("set receive or duration"). |

That last rule is deliberate: without it a never-ending stream would pin its
worker for the whole run. `timeout` is also what bounds opening the stream.
Client-streaming calls end when their one response arrives, and a `receive` on
one is rejected. A run that ends mid-stream is never counted as a failure.

### What a stream reports

A stream isn't one request, so, like a WebSocket connection, it is reported by
its messages: **one result per received message**, whose latency is the gap
since the previous one (the first: since the stream opened, which for a
client-streaming call spans all the sends, so it is the upload's round trip).
The `requests` in the report therefore count messages, and messages/sec falls
out of the usual rate.

For the scenario above, `--dry-run` shows five `Subscribe` events about 20ms
apart, the one `Upload` response, and the `Chat` echoes plus the goodbye:

```text
[0] grpc  status=200  latency=21.093ms  bytes_out=8  bytes_in=26  ok
[1] grpc  status=200  latency=19.516ms  bytes_out=0  bytes_in=26  ok
    ... three more Subscribe events ...
[5] grpc  status=200  latency=459.928ms  bytes_out=430  bytes_in=4  ok
[6] grpc  status=200  latency=205µs  bytes_out=31  bytes_in=23  ok
[7] grpc  status=200  latency=1µs  bytes_out=0  bytes_in=20  ok
[8] grpc  status=200  latency=4µs  bytes_out=0  bytes_in=26  ok
```

- A stream that fails adds one last failed result carrying the reason:
  `stream ended after 2 of 5 messages`, `timed out after 10s with 3 of 5
  messages`, or the server's own `rpc error: code = ...`.
- A stream that ends cleanly having delivered nothing still counts, as one
  successful result.
- The bytes you sent are added to the first result.
- Results are held until the stream ends, so memory grows with the messages
  per stream: give long-lived streams a `receive` or a `duration`.

### Checks on streams

`expect_body` must hold for **every** received message; the first that doesn't
fails the call and ends the stream. `extract` runs on every message, so the last
message's values are what later calls see (and `header:` rules see response
headers only, since trailers arrive after the last message). `expect_code` is
checked against the status the *server* ends the stream with, so an expected
`NOT_FOUND` after the last message can pass; a stream that resonate itself ends
(`receive` reached, `duration` elapsed) has no server status to check.

## Load profiles

gRPC scenarios use the same `load:` block as every other protocol, so
everything in [Execution Models](execution-models.md) applies: closed and open
model, `stages` ramps, `iterations` per virtual user, and so on. Two things are
specific to gRPC:

- `rate` and `requests` count **iterations**, that is, whole call sequences. A
  two-call sequence at `rate: 100` makes about 200 calls/s, and a streaming
  call contributes one request per received message.
- With `workers` set high, mind the [connection limit](#connections-and-concurrency).

A ramp-hold-ramp-down profile, gated on the results:

```yaml
protocol: grpc

load:
  stages:
    - { duration: 10s, workers: 20, rate: 100 }   # ramp up 0 -> 100 iterations/s
    - { duration: 30s, workers: 20, rate: 100 }   # hold
    - { duration: 5s,  workers: 0,  rate: 0 }     # ramp down

grpc:
  address: localhost:50051
  plaintext: true
  calls:
    - method: acme.user.v1.UserService/CreateUser
      message: '{"name":"alice","age":30}'

assertions:
  - metric: success_rate
    min: "0.999"
  - metric: latency_p99
    max: 100ms
```

To find a service's limit, raise `rate` across runs until the assertions fail or
the report's saturation warning appears.

## Reading the results

Every run writes the [four reports](reports.md). For gRPC specifically:

- **Requests** are unary calls plus received stream messages, as described
  above.
- **Latency** for a unary call is the call itself, measured around the RPC, so
  message rendering and JSON parsing aren't in it.
- **Bytes in/out** are the encoded protobuf sizes of the messages.
- **Status codes** are reported as their HTTP equivalents, because the report's
  status breakdown and success rate are HTTP-shaped. The error breakdown keeps
  the real gRPC code (`rpc error: code = NotFound desc = ...`).

| gRPC status | Shown as |
| --- | --- |
| `OK` | 200 |
| `INVALID_ARGUMENT`, `FAILED_PRECONDITION`, `OUT_OF_RANGE` | 400 |
| `UNAUTHENTICATED` | 401 |
| `PERMISSION_DENIED` | 403 |
| `NOT_FOUND` | 404 |
| `ALREADY_EXISTS`, `ABORTED` | 409 |
| `RESOURCE_EXHAUSTED` | 429 |
| `CANCELLED` | 499 |
| `UNKNOWN`, `INTERNAL`, `DATA_LOSS` | 500 |
| `UNIMPLEMENTED` | 501 |
| `UNAVAILABLE` | 503 |
| `DEADLINE_EXCEEDED` | 504 |

A result shown with status `0` is resonate's own verdict rather than something
the server returned: a stream that ended early or timed out, or a message that
failed to render.

A call still in flight when the run itself ends (the `duration` elapsing) is
dropped rather than reported, since the server never got to answer it.
Otherwise every run would end with a handful of spurious `DEADLINE_EXCEEDED`
failures.

## Connections and concurrency

Each gRPC connection is one HTTP/2 connection, and servers typically allow only
about 100 concurrent streams on it. Past that, calls queue *client-side* and
latency inflates for reasons that have nothing to do with the server's speed.
`connections: N` spreads virtual users over `N` separate connections (VU *i*
uses connection *i* mod *N*). The default is 1, so raise it when `workers` (or
`max_workers`) goes well past 100.

Calls use gRPC's *wait-for-ready* mode: against a server that is down, a call
blocks until `timeout` and then fails, rather than failing instantly. An
instant failure would let a worker loop with no `rate` set spin at millions of
calls per second.

## Performance notes

- **Check the target under concurrency first.** Load that crashes the service
  tells you about the service, not about throughput.
- **Quiet the server.** Per-request logging can cost more than the handler.
  Run the target with request logging off when measuring.
- **Same machine, shared CPUs.** A localhost test has the client and server
  competing for the same cores; treat the numbers as relative.
- **Static messages are cheaper.** A message with no `{{` is parsed once; a
  templated one is rendered and parsed per call.
- **Streams hold results in memory** until they end (see above).
- **Message size.** gRPC's default 4 MiB receive limit applies to responses.

## Troubleshooting

Most problems show up at `--dry-run` or at load time, before any load is sent.

| You see | What it means and what to do |
| --- | --- |
| `fetching the schema via server reflection: ... connection refused` | The server isn't reachable at `address`: wrong host or port, or it isn't running. |
| `the server doesn't offer reflection: ...` | Reflection is off. Add `proto_files` or `descriptor_set`. |
| `service "x" not found in the schema` | A typo in `method`, or the file defining it isn't in `proto_files` or the descriptor set. |
| `service "x" has no method "y" (has: ...)` | The error lists the methods the service has. |
| `invalid method "x": expected "package.Service/Method"` | The `method` isn't in that form. |
| `parsing message as pkg.Request JSON: ...` | The `message` doesn't match the request type: an unknown field, or a wrong type. |
| `... is a unary RPC: it takes one request (message); ...` | A streaming-only field on a unary call. The error names the method's kind; see [Calls](#calls). |
| `... is a client-streaming RPC: give its requests as messages, not message` | Use `messages` for client-streaming and bidirectional calls. |
| `invalid expect_code "X"` | Use a status name from [the list](#checking-responses). |
| `plaintext disables TLS and can't be combined with ...` | Drop `plaintext`, or drop the TLS options. |
| `proto_files and descriptor_set are mutually exclusive` | Use one. |
| `compiling proto_files: ...` | A syntax error, or an import that doesn't resolve. Set `import_paths`. |
| `building descriptors from "x.pb" (was it written with --include_imports?)` | Regenerate the descriptor set with `--include_imports`. |
| `x509: certificate signed by unknown authority` | Give `ca_file`, or `insecure: true` for a throwaway test. |
| Every call fails with `DeadlineExceeded` and `connection error` | resonate can't connect; calls wait for `timeout` before failing. See [Connections](#connections-and-concurrency). |
| `check failed: code: got NOT_FOUND (...), want one of [OK]` | The call returned a status you didn't expect. Add it to `expect_code` if it's correct. |
| `stream still open after the 30s timeout; set receive or duration` | An endless stream with no bound. Add `receive` or `duration`. |
| `stream ended after 2 of 5 messages` | The server closed the stream before sending `receive` messages. |
| `timed out after 10s with 3 of 5 messages` | The stream was too slow for `duration`/`timeout`. |
| Latency climbs as `workers` grows | Likely the per-connection stream limit: raise `connections`. |
| The server crashes or errors under load | The target isn't safe under concurrency. See the [worked example](#worked-example-a-real-service). |

## From Go

gRPC is available through the fluent `Scenario` builder (`GRPC(address)`, then `Call(method)` and
its `Message`/`Receive`/`Metadata`/... setters) and through the struct API
(`resonate.NewGRPCGenerator`, `GRPCTarget`, `GRPCCall`, `GRPCOptions`), with every field
described on this page. See [Library](library.md#grpc) for worked examples of both.

## The example server

`examples/testgrpcserver` is a small gRPC server that implements the services
the example scenarios call, so you can run and validate them without a service
of your own: `acme.user.v1.UserService` (unary) and `acme.feed.v1.Feed`
(server, client and bidirectional streaming), from the `.proto` files in
`examples/proto`. It keeps everything in memory (users accumulate under load),
so it is a demo target, not a benchmark of anything.

```sh
make example-grpc-server                          # :50051, reflection on
resonate run examples/grpc-basic.yaml --dry-run
resonate run examples/grpc-streaming.yaml --dry-run

make example-grpc-server ARGS=-reflection=false   # reflection off
resonate run examples/grpc-proto-files.yaml --dry-run
```

Run `resonate` from the repository root, since the `proto_files` path in the
example is relative to it. `grpc-basic.yaml` and `grpc-streaming.yaml` rely on
reflection, so against `-reflection=false` they fail with an error saying the
server doesn't offer it. That is the case `grpc-proto-files.yaml` exists for.

Flags: `-addr` (default `:50051`), `-reflection` (default on),
`-event-interval` (the gap between `Feed/Subscribe` events, default 20ms),
`-log-requests` and `-stats-interval` (below).

How it behaves, which the examples' checks depend on:

- `CreateUser` rejects an empty name or a negative age with `INVALID_ARGUMENT`.
- `GetUser` of an unknown id is `NOT_FOUND`.
- `Subscribe` echoes the topic and numbers its events from 1.
- `Upload` answers with how many items and payload bytes it received.
- `Chat` echoes every message, then says goodbye when you stop sending, so `n`
  messages in give `n + 1` out.

### Its logs

The server logs like an access log, so you can watch what `--dry-run` or a real
run does to it. Every method called gets a `call` line when it arrives and a
`done` line when it finishes (status, duration, messages received `in` and sent
`out`, and the client), plus a line per client connection and a few lines about
what the handlers did. A streaming call shows up the moment it opens, not only
when it ends. The reflection lookups resonate makes to learn the schema are
methods too, so they appear:

```text
conn connected    [::1]:60771
call grpc.reflection.v1.ServerReflection/ServerReflectionInfo bidi-streaming   from=[::1]:60771
done grpc.reflection.v1.ServerReflection/ServerReflectionInfo OK                     342µs  in=1 out=1  from=[::1]:60771
call acme.feed.v1.Feed/Subscribe            server-streaming from=[::1]:60771
subscribe topic="orders" opened (an event every 20ms until the client stops)
subscribe topic="orders" closed after 5 events
done acme.feed.v1.Feed/Subscribe            OK                 101.439ms  in=1 out=5  from=[::1]:60771
```

That is a lot of output under load, so for a performance run pass
`-log-requests=false` (`make example-grpc-server ARGS=-log-requests=false`).
The per-call lines stop, and every `-stats-interval` (default 10s) while there is
traffic one summary line is printed instead:
`stats 412 RPCs in the last 10s (0 failed): CreateUser=200 GetUser=212 | total 412 (0 failed)`.
A call counts as failed when the server returns an error status; a client
hanging up on a stream (which is how resonate ends one after `receive`) does
not. Ctrl-C prints the totals and shuts down.

## Worked example: a real service

[`examples/grpc-kvstore.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/grpc-kvstore.yaml)
load-tests the key-value store in
[go-grpc-example](https://github.com/jecklgamis/go-grpc-example), a real Go gRPC
server (`kvstore.KeyValueStore`: `Put` and `Get`). It ramps to 1,000
iterations/s, each a `Put` then a `Get` of the same key, and gates the run on
`success_rate` and `latency_p99`. What running it taught:

- **The first run crashed the server.** Its in-memory store was a plain Go map
  with no lock, so concurrent requests killed it within about a second
  (`fatal error: concurrent map read and map write`). The fix was a mutex
  around the store, and a rebuilt server then ran the full ramp cleanly, even
  with Go's race detector on. A load test is a concurrency test, so run one
  before trusting any throughput number.
- **Rebuild the server before re-testing.** A stale binary from before the fix
  would still crash.
- **Quiet the server** for the run (`2>/dev/null`), since it logged every
  request.
- **`rate` counts iterations,** so the report showed about twice the iteration
  rate in requests.
- **Ramps give clean numbers.** `load.stages` ramps up, holds and ramps down, so
  the report shows steady state and not a cold start.

## Example scenarios

| File | Shows | Needs |
| --- | --- | --- |
| [`grpc-basic.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/grpc-basic.yaml) | Unary calls, chaining, metadata, identities, `connections` | The example server |
| [`grpc-proto-files.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/grpc-proto-files.yaml) | Schema from a `.proto` file, for a server without reflection | The example server with `-reflection=false` |
| [`grpc-streaming.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/grpc-streaming.yaml) | Server, client and bidirectional streaming | The example server |
| [`grpc-kvstore.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/grpc-kvstore.yaml) | A ramped, asserted test of a real service | [go-grpc-example](https://github.com/jecklgamis/go-grpc-example) |

## Not supported

- **`resonate hit` has no gRPC flags;** gRPC is scenario-file (and library) only,
  like WebSocket.
- **gRPC-Web and the Connect protocol** are not spoken. A Connect or grpc-gateway
  server's unary JSON endpoints can be hit as ordinary HTTP.
- **Bidirectional calls aren't lockstep:** sends and receives run concurrently.
- **No tuning knobs for** compression, keepalives, retries, load-balancing policy
  or message-size limits; gRPC's defaults apply.
- **Descriptor sets must include their imports** (`--include_imports`).

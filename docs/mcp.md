# MCP (Model Context Protocol)

resonate load-tests [MCP](https://modelcontextprotocol.io) servers: the tool servers AI
agents call. A scenario (`protocol: mcp`) describes a session and the calls an agent
makes on it. resonate opens one session per virtual user, makes the calls in order every
iteration, checks the answers, and reports session setup and every call as separate
steps. Templates, identities, feeders, assertions, and the open and closed load models
work as they do for every other protocol.

MCP is only available through `resonate run` (and the Go library), like WebSocket, gRPC
and SSE. It speaks the **streamable HTTP** transport, so the target is a URL. Servers that
only speak stdio are [not supported](#not-supported).

## Quick start

No server of your own needed: the repository ships an example MCP server. From a checkout
of the repo:

```sh
make example-mcp-server        # in one terminal: listens on :8766
```

Save this as `mcp.yaml`:

```yaml
protocol: mcp

load:
  duration: 10s
  rate: 20
  workers: 5

mcp:
  url: http://localhost:8766/mcp
  timeout: 5s
  calls:
    - tool: add
      arguments: {a: 2, b: 3}
      expect_result:
        json:structuredContent.result: "5"
    - tool: echo
      arguments: {message: "hello from vu {{.VU}}"}

assertions:
  - metric: success_rate
    min: "0.999"
```

Check what happens once before loading, then run it:

```sh
resonate run mcp.yaml --dry-run
resonate run mcp.yaml
```

The report breaks the run down per step. Session setup shows up as `initialize` and
`initialized`, each call under its tool name:

```
Steps:
  Step          Requests   Success         p50         p95         p99
  initialize           5   100.00%     7.852ms     7.881ms     7.881ms
  initialized          5   100.00%        79µs       114µs       114µs
  add                219   100.00%       463µs       554µs       824µs
  echo               219   100.00%       298µs       379µs       482µs
```

## How a session works

Every virtual user owns one MCP session, opened the first time that user runs an iteration:

1. `initialize`: announces the protocol version and a `clientInfo`, and keeps the
   `Mcp-Session-Id` header the server returns. Reported as the step `initialize`.
2. `notifications/initialized`: tells the server the handshake is done. Reported as the
   step `initialized`.
3. Each iteration then makes the `calls` in order over the session, stopping at the first
   failure.

Every request after `initialize` carries the `Mcp-Session-Id` (a server that issues none is
fine: it is simply not sent) and an `MCP-Protocol-Version` header with the version the
server chose. A session is reused for the rest of that user's life, so `workers` is how many
sessions are open at once.

- **A failed setup is retried.** If `initialize` or `initialized` fails, that iteration
  stops with the failure reported, and the next iteration tries again.
- **An expired session is reopened.** A server that answers a session with HTTP 404 has
  forgotten it. That call fails, and the user opens a new session on its next iteration.
- **Sessions are never closed.** The engine has no "virtual user departed" hook, so resonate
  does not send `DELETE`. A server that keeps sessions for ever will accumulate them over a
  long run; the example server's `-session-ttl` shows how one should reap them.

Replies may be plain JSON or a `text/event-stream`; resonate handles both. On a stream it
reads events until the response to its request arrives and skips everything else
(progress and logging notifications, server-initiated requests).

## The calls

Each entry in `mcp.calls` is one JSON-RPC request, and sets exactly one of:

| Key | Sends | Default step name |
| --- | --- | --- |
| `tool: <name>` | `tools/call` with `arguments` | the tool name |
| `list: tools`, `resources` or `prompts` | `tools/list`, `resources/list` or `prompts/list` | `tools/list`, ... |
| `method: <name>` | any other JSON-RPC method, with `params` | the method |

`name:` overrides the step name. The names `initialize` and `initialized` are reserved for
session setup. Calls sharing a name are reported together.

```yaml
calls:
  - tool: add
    arguments: {a: 2, b: 3}
  - list: resources
  - method: resources/read
    name: read-demo
    params: {uri: "file:///demo.txt"}
```

### Arguments and params

`arguments` (and `params`) take one of two forms:

- **A mapping.** It is rendered to JSON; every string value may embed `{{ }}` expressions
  and renders as a JSON string. Numbers, booleans and nulls stay as written.
- **A string holding a raw JSON document**, itself a template. Use this when a templated
  value must be a JSON number or boolean, because `"{{.Seq}}"` in a mapping is a string:

  ```yaml
  arguments: '{"a": {{randInt 1 100}}, "b": {{.Vars.sum}}}'
  ```

The result is checked to be valid JSON after templating; a template that renders broken
JSON fails that call with a clear error and sends nothing.

Templates see the same data as every protocol: `{{.Seq}}`, `{{.VU}}`,
`{{.Identity.<key>}}`, `{{.Feeder.<column>}}` and `{{.Vars.<name>}}`. Beyond what your calls
extract, `{{.Vars.session_id}}` holds the session's `Mcp-Session-Id`.

## Checking results

`expect_result` checks the JSON-RPC `result` object with the same rule language as
`extract` elsewhere: `json:<path>`, `yaml:<path>`, `xml:<path>` or `regex:<pattern>`. An
empty expected value only requires the rule to evaluate. (`header:`, `status` and `css:`
make no sense for a result and are rejected when the scenario loads.)

`extract` pulls values out of the same `result` into `{{.Vars.<name>}}` for later calls in
the iteration:

```yaml
protocol: mcp

load:
  duration: 10s
  rate: 20
  workers: 4

mcp:
  url: http://localhost:8766/mcp
  calls:
    - tool: add
      arguments: '{"a": {{randInt 1 100}}, "b": {{randInt 1 100}}}'
      extract:
        sum: json:structuredContent.result
    - tool: multiply
      arguments: '{"a": {{.Vars.sum}}, "b": 2}'
      expect_result:
        json:structuredContent.result: ""
```

### Tool errors

A tool that ran and failed does not produce a JSON-RPC error: the server answers normally
with `isError: true` in the result, and HTTP 200. resonate treats that as a **failure**
(`mcp: tool error: <text>`), because a server whose tools all error is not healthy. To test an
error path on purpose, say so:

```yaml
protocol: mcp

load:
  duration: 10s
  rate: 20
  workers: 4

mcp:
  url: http://localhost:8766/mcp
  calls:
    - tool: fail
      name: failing-tool
      expect_tool_error: true      # isError: true is the expected outcome
```

With `expect_tool_error: true` the call passes when the tool reports an error and fails when
it unexpectedly succeeds. It applies to `tool:` calls only. A JSON-RPC error, such as an
unknown tool, is always a failure.

## What gets reported

One `Result` per JSON-RPC request:

- `initialize` and `initialized` the first time each virtual user runs (and again after an
  expired session). `initialized` normally answers `202`.
- One result per call, with the HTTP status as its status code. Latency is the time until
  the JSON-RPC response arrived; for an event-stream reply that is the response event, not
  the moment the stream closed. `bytes_out` is the request, `bytes_in` the reply.

So `requests` counts setup requests and calls, and the **per-step table** is where the
numbers mean something: setup cost, each tool, side by side.

A call fails, and the report's Errors table separates the causes with stable messages:

| Error | Meaning |
| --- | --- |
| `mcp: timed out after 5s` | no complete reply within `timeout` |
| `mcp: unexpected HTTP status 503` | a non-200 reply (a notification such as `initialized` also accepts `202` and `204`) |
| `mcp: HTTP 400: json-rpc error -32600: <message>` | a non-200 reply whose body is a JSON-RPC error that explains it |
| `mcp: json-rpc error -32602: <message>` | the server answered with a JSON-RPC `error` |
| `mcp: tool error: <text>` | the tool ran and returned `isError: true` |
| `check failed: result "<rule>": got "x", want "y"` | an `expect_result` rule did not hold |
| `mcp: stream ended without a response to request N` | an event stream closed without answering |
| `mcp: unexpected content-type "<type>"` | the reply was neither JSON nor an event stream |

Server-supplied text in these messages is truncated, so the table stays small.

## Authentication and per-user data

`headers` are sent on every request and may embed templates, so a bearer token per virtual
user is an `identities` pool:

```yaml
protocol: mcp

load:
  duration: 30s
  rate: 50
  workers: 10

mcp:
  url: http://localhost:8766/mcp
  headers:
    Authorization: Bearer {{.Identity.token}}
  identities:
    - token: alice
    - token: bob
  calls:
    - tool: echo
      arguments: {message: "hi from {{.Identity.token}}"}
```

Each virtual user gets one identity, round-robin, for its whole life, so its session is
always opened with the same credentials. A `feeder` hands out one row of CSV or JSON data per
iteration, available as `{{.Feeder.<column>}}` (see [Feeders](feeders.md)).

Secrets never appear in reports: the run settings table shows the URL without its query
string and never lists header values.

## Load profiles

A virtual user is a session, so the load model decides how sessions behave:

- **Closed** (`workers: 20`, no `rate`): 20 sessions, each making calls back to back.
- **Open** (`rate: 100`, `max_workers: 200`): arrivals are paced; when replies slow down the
  engine adds virtual users up to `max_workers`, and **each new user opens its own session**.
  The `initialize` steps in the report show how many it opened.
- **Session churn**: `load.iterations: 5` retires a virtual user after five iterations and
  starts a fresh one, which opens a new session. Use it to load-test the handshake itself,
  or how the server copes with sessions coming and going.

See [Execution Models](execution-models.md) for the load models themselves.

## Connections and limits

`mcp` takes the usual HTTP transport settings: `timeout` (bounds one whole JSON-RPC
exchange, including reading an event-stream reply), `insecure`, `ca_file`,
`cert_file`/`key_file` (mTLS), `h2c`, `http3` (the URL must be `https://`) and
`max_response_body` (bytes read per reply; with none set a 16 MiB safety cap applies, and a
reply over the cap fails to parse rather than exhausting memory). The HTTP client is shared
by every virtual user, so connection pooling is Go's, as for HTTP.

For example, a server behind a private CA that wants a client certificate:

<!-- docs-test: parse-only -->
```yaml
protocol: mcp

load:
  duration: 30s
  rate: 20
  workers: 5

mcp:
  url: https://mcp.internal.example.com/mcp
  ca_file: /etc/ssl/internal-ca.pem
  cert_file: client.pem
  key_file: client-key.pem
  calls:
    - list: tools
```

## Troubleshooting

- **`HTTP 400` on every call after `initialize`.** The server wants the
  session id and did not get one: it did not return `Mcp-Session-Id` on `initialize`, or a
  proxy in front stripped it. Run `resonate run <file> --dry-run` and look at the first
  results.
- **`HTTP 404`.** The session expired or the server restarted. resonate
  opens a new one on the next iteration; if it happens constantly, the server's session
  timeout is shorter than the gap between a user's iterations.
- **`unexpected content-type`.** The URL is not the MCP endpoint (a health page or a login
  redirect). Check the path.
- **A `json:` rule does not match.** Rules see the JSON-RPC `result`, not the whole message.
  For a tool, the text a client sees is `content[0].text` and the structured form is
  `structuredContent`.
- **Everything fails with `tool error`.** The tool ran and reported `isError: true`; the
  text after the colon is the server's own message. Fix the arguments, or set
  `expect_tool_error: true` if that is the point.
- **Latency looks too good.** A closed-model run with few workers can hide a slow server;
  see [Execution Models](execution-models.md#open-vs-closed-model).

## From Go

```go
summary, err := resonate.NewScenario().
	MCP("http://localhost:8766/mcp").
	Header("Authorization", "Bearer {{.Identity.token}}").
	Identity(map[string]string{"token": "alice"}).
	Tool("add", map[string]any{"a": 2, "b": 3}).
	ExpectResult("json:structuredContent.result", "5").
	Tool("fail", nil).ExpectToolError().
	Rate(50).Workers(10).Duration(30 * time.Second).
	Run(ctx)
```

`MCP(url)` switches the scenario to MCP; `Tool`, `MCPList` and `MCPMethod` add calls, and
`Name`, `Extract`, `ExpectResult` and `ExpectToolError` then apply to the latest one.
`ProtocolVersion` and `ClientInfo` set what `initialize` announces. See [Library](library.md).

## The example server

`examples/testmcpserver` is a small MCP server (standard library only) for trying the
examples and this guide without a server of your own:

```sh
make example-mcp-server                      # :8766, plain JSON replies
make example-mcp-server ARGS=-sse            # reply as event streams
make example-mcp-server ARGS=-session-ttl=10s
```

| Endpoint | Serves |
| --- | --- |
| `POST /mcp` | JSON-RPC: `initialize`, `notifications/*`, `ping`, `tools/list`, `tools/call`, `resources/list`, `resources/read`, `prompts/list` |
| `DELETE /mcp` | ends a session |
| `GET /healthz` | `ok` |

Tools: `add`, `multiply`, `echo`, `solve_quadratic`, `sleep` (answers after `ms`
milliseconds) and `fail` (always a tool error). Like a real stateful server it requires the
`Mcp-Session-Id` from `initialize` on every other request (400 without, 404 for an unknown or
expired one). With `-sse` every reply is an event stream, and a tool call is preceded by a
log notification, which resonate must skip.

It logs like an access log: a `call` line as a request arrives and a `done` line when it
finishes, with the JSON-RPC method. That is a lot of output under load, so for a
performance run use `-log-requests=false`, which prints a one-line summary every
`-stats-interval` instead. Credentials are never logged.

## Example scenarios

[`examples/mcp-basic.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/mcp-basic.yaml)
(exact answers and a per-tool latency limit),
[`mcp-explore.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/mcp-explore.yaml)
(lists and generic methods),
[`mcp-chaining.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/mcp-chaining.yaml)
(extract, raw-JSON arguments, identities) and
[`mcp-errors.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/mcp-errors.yaml)
(expected tool errors). Each runs against the example server.

## Not supported

- **stdio servers.** Many MCP servers only speak stdio. Running one means spawning it as a
  subprocess per virtual user, which is a different kind of test; put an HTTP transport (or
  a bridge) in front of it.
- **Server-initiated messages.** Progress, logging and sampling requests that arrive on a
  reply stream are skipped, not answered or counted. There is no long-lived `GET` stream.
- **Ending sessions.** No `DELETE` is sent (see [How a session works](#how-a-session-works)).
- **OAuth flows.** Use `headers` with a token you already have.
- **Batch requests.** A batch *reply* is understood, but resonate never sends batches.

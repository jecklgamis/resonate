# Feeders

A **feeder** supplies data to a test. Each iteration takes one row from the feeder and exposes it to
the request templates as `{{.Feeder.<column>}}`, so the load comes from a real dataset (users,
product ids, recorded payloads) instead of purely random values.

There are two kinds of source:

- a **file**, CSV or JSON, with three ways to hand out its rows (`sequential`, `random`, `stream`);
- a **Kafka topic**, where each message is a row. See [Kafka](#kafka) below, and the
  [Kafka Feeder guide](kafka-feeder.md) for the detail.

Feeders belong to scenario files and the Go library. `resonate hit` has no feeder flags.

## Quick start

`users.csv`, where the first row names the columns:

```csv
email,password
alice@example.com,hunter2
bob@example.com,hunter3
carol@example.com,hunter4
```

```yaml
protocol: http

load:
  duration: 30s
  rate: 10
  workers: 5

http:
  feeder:
    file: users.csv
  targets:
    - method: POST
      url: http://localhost:8080/login
      body: '{"email": "{{.Feeder.email}}", "password": "{{.Feeder.password}}"}'
```

```sh
resonate run scenario.yaml
```

The first request logs in as alice, the second as bob, the third as carol, the fourth as alice
again, and so on. The runnable [`examples/http-feeder.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-feeder.yaml)
uses [`examples/data/users.csv`](https://github.com/jecklgamis/resonate/blob/main/examples/data/users.csv).

## Choosing a source

| | Rows come from | Order | Memory | Use it for |
| --- | --- | --- | --- | --- |
| `file`, `mode: sequential` (default) | A CSV/JSON file, loaded once | In file order, wrapping around | The whole file, in memory | Small and medium datasets |
| `file`, `mode: random` | The same | Uniformly random, with replacement | The whole file, in memory | Sampling, when order doesn't matter |
| `file`, `mode: stream` | A CSV/JSON file, read as the test runs | In file order, wrapping around | A small fixed buffer | Files too big for memory |
| `rows` | Rows written inline in the YAML (or passed from Go) | In order, wrapping around; `mode: random` for random | The rows themselves | A handful of values, no file needed (an *array feeder*) |
| `kafka` | A topic's messages | Per partition, in order | A small fixed buffer | Very large datasets, or data produced by another system |

All of them hand out rows from one source shared by every worker, so the dataset is spread across
the whole run rather than replayed separately by each worker.

## Inline rows

`rows` is an array feeder: the data lives in the scenario instead of a file. It is mutually
exclusive with `file` and `kafka`, supports `mode: sequential` (default) or `random` (not
`stream`), and must not be empty.

```yaml
http:
  feeder:
    rows:
      - {email: alice@example.com, password: hunter2}
      - {email: bob@example.com, password: hunter3}
    mode: random
```

See [`examples/http-feeder-rows.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-feeder-rows.yaml).

## File feeders

```yaml
http:
  feeder:
    file: users.csv      # or users.json; the extension picks the format
    mode: sequential     # sequential (default), random or stream
```

### CSV

The first row is the column names. Every later row must have the same number of fields as the
header. Quoting follows standard CSV, so a value can contain commas or newlines when quoted. Every
value is text.

### JSON

The file is an array of flat objects:

```json
[
  {"id": 1, "name": "alice", "premium": true, "tags": ["a", "b"]},
  {"id": 2, "name": "bob",   "premium": false, "tags": []}
]
```

Each top-level field is a column. Values become text the way you'd expect:

| JSON value | Becomes |
| --- | --- |
| `"alice"` | `alice` |
| `1`, `1.50` | `1`, `1.5` |
| `true` | `true` |
| `null` | an empty string |
| `{"x": 1}`, `[1, 2]` | the raw JSON text, `{"x":1}` and `[1,2]`, so you can embed it in a body |

### Modes

- **`sequential`** (default). Rows go out round-robin, starting again from the first after the last.
  The whole file is read into memory once, at startup.
- **`random`**. Each call picks a row uniformly at random, *with replacement*, so over a short run
  some rows repeat and others never appear. Also loaded into memory.
- **`stream`**. Behaves like `sequential` but reads the file as the test runs, keeping only 1000
  rows in memory whatever the file's size. At the end of the file it reopens it and starts again.
  It checks the whole file once when the run starts.
  There is no streaming `random`.

### How big a file can be

`sequential` and `random` hold the file as a table of text, which costs far more than the file's
size, and the ratio depends on the rows: many short rows cost the most. Measured: a 36 MB CSV of
2 million narrow rows used about 40 times its size in memory, and an 83 MB CSV of 2 million rows with
three columns used about 11 times (934 MB). So expect roughly 10 to 40 times, and keep `sequential`
and `random` files to tens of MB. Use `mode: stream` or [Kafka](#kafka) for anything bigger: that
83 MB file ran in `stream` mode with 30 MB of memory.

`stream` reads the whole file once when the run starts, one row at a time, to check it. A malformed
row anywhere in the file (a CSV row with the wrong number of fields, invalid JSON) is reported with
its row number before any request is sent, instead of being mistaken for the end of the file. That
scan takes time in proportion to the file: the whole run on the 83 MB file, scan included, took under
a second on a laptop. After it, the file is read again in the background as the test runs.

### What is checked before the run

A feeder that can't work fails before any request is sent:

| Message | Meaning |
| --- | --- |
| `unsupported file extension ... (want .csv or .json)` | Only those two formats are read |
| `no such file or directory` | The path is wrong. A relative path is relative to where you run `resonate` |
| `record on line N: wrong number of fields` | A CSV row doesn't match the header (`sequential` and `random`); `stream` says `row N` instead |
| `"x.csv" has no data rows` | The file is empty or has only a header |
| `must be a JSON array of objects` | The JSON is an object, or an array of something else |
| `invalid mode "x" (want "sequential", "random", or "stream")` | A typo in `mode` |

## When a row is picked

One row is taken per **iteration**, and it is the same row for everything in that iteration.

| Scenario | An iteration is | The row is used by |
| --- | --- | --- |
| `http` with `targets` | one request | that request |
| `http` with `flow` | one pass over all the steps | **every step**, so a login and the requests after it see the same user |
| `http` with `setup` | the setup steps, run once per virtual user | the setup steps; it takes its own row |
| `ws` | one connection | every message on it |
| `sse` | one stream | the request that opens it |
| `grpc` | one `calls` sequence | every call in it, in `message` and `metadata` |

A feeder row is *per iteration*. That differs from `identities`, which are sticky per virtual user,
and from `{{.Vars.*}}`, which come from earlier responses. You can use all three together:

```yaml
protocol: http

load:
  duration: 30s
  workers: 5

http:
  identities:
    - token: abc123
    - token: def456
  feeder:
    file: users.json
    mode: random
  flow:
    - method: POST
      url: http://localhost:8080/orders
      headers:
        Authorization: Bearer {{.Identity.token}}
      body: '{"user": "{{.Feeder.name}}"}'
      extract:
        order_id: json:id
    - method: GET
      url: http://localhost:8080/orders/{{.Vars.order_id}}?owner={{.Feeder.name}}
```

A column the row doesn't have, or one with an empty value, renders as an empty string, the same as
a variable that was never extracted.

The templated fields are `url`, `query`, `headers` and `body` for HTTP and SSE, the message `body`
for WebSocket, and `message` and `metadata` for gRPC.

### The same feeder for the other protocols

Under `ws`, `sse` and `grpc` the `feeder:` block is the same, and sits in that protocol's section.
These scenarios need a server for their protocol, so they are only checked for correct syntax.

<!-- docs-test: parse-only -->
```yaml
protocol: ws

load:
  duration: 30s
  workers: 5

ws:
  url: ws://localhost:8080/socket
  feeder:
    file: users.csv
  messages:
    - body: '{"type": "login", "email": "{{.Feeder.email}}"}'
      wait: true
```

<!-- docs-test: parse-only -->
```yaml
protocol: sse

load:
  duration: 30s
  workers: 5

sse:
  url: http://localhost:8765/events?user={{.Feeder.email}}
  events: 10
  feeder:
    file: users.csv
```

<!-- docs-test: parse-only -->
```yaml
protocol: grpc

load:
  duration: 30s
  workers: 5

grpc:
  address: localhost:50051
  plaintext: true
  feeder:
    file: users.csv
  calls:
    - method: acme.user.v1.UserService/CreateUser
      message: '{"name": "{{.Feeder.email}}", "age": 30}'
```

## Kafka

Instead of a file, a feeder can read a Kafka topic, with each message a row. It suits datasets too
big for a file, and data that another system produces while the test runs.

```yaml
http:
  feeder:
    kafka:
      brokers: ["localhost:9092"]
      topic: users
```

A JSON message's fields become columns, and every row also has `_key`, `_value`, `_partition`,
`_offset`, `_timestamp` and `_header_<name>`. The main choices are:

| Setting | Effect |
| --- | --- |
| `from: start` (default) / `end` | Read from the oldest message, or only messages produced after the run begins |
| `on_end: wrap` (default) / `wait` | Start the topic over when it runs out, or keep waiting for new messages |
| `format: json` (default) / `raw` | Treat the value as a JSON object, or expose only the `_` columns |

Things that are different from a file feeder:

- It reads the topic directly, with **no consumer group and no committed offsets**, so it never
  disturbs other consumers and every run starts from the same place.
- Memory does not grow with the topic. A 250,005-message topic took about 30 MB more than a run
  with no feeder.
- There is no `mode`. Order is per partition; across partitions it is not defined.
- It can need credentials (`tls`, `sasl_mechanism`, `username`, `password_env`).

The [Kafka Feeder guide](kafka-feeder.md) has the full option table, the row columns, setting up a
local broker to try it, authentication, behaviour on an idle topic, and troubleshooting. A runnable
scenario is [`examples/http-kafka-feeder.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-kafka-feeder.yaml).

## From Go

```go
// A file. mode is "sequential" (default), "random" or "stream".
feeder, err := resonate.NewFeeder("users.csv", "sequential")

// Or rows given directly (an array feeder). mode is "sequential" or "random".
feeder, err := resonate.NewArrayFeeder([]map[string]string{{"id": "1"}, {"id": "2"}}, "sequential")

// Or a Kafka topic.
feeder, err := resonate.NewKafkaFeeder(resonate.KafkaFeederOptions{
	Brokers: []string{"localhost:9092"},
	Topic:   "users",
})
```

Set it as `Feeder` on `HTTPOptions`, `WSOptions` or `GRPCOptions`, or as `HTTP.Feeder` on
`FlowOptions` and `SSEOptions`. `feeder.Next()` returns the next row as a `map[string]string`, and
is safe to call from many goroutines. Call `feeder.Close()` after the run has returned: it stops the
background reader of a `stream` or Kafka feeder, and does nothing for the others.

The fluent `Scenario` builder takes a file directly, `Feeder("users.csv", "sequential")`, or rows,
`FeederRows(rows, "sequential")`. A Kafka
feeder goes through the options structs above. See [Library](library.md).

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Every request has an empty value | The column name doesn't match the header or JSON key; names are case-sensitive |
| Rows repeat sooner than expected | The dataset is smaller than `rate × duration`; it wraps around. `random` also repeats, by design |
| Out of memory, or the machine slows | A large file in `sequential` or `random` mode; use `stream` or Kafka |
| `stream` mode is slow to start | It scans the whole file once to validate it; a multi-GB file takes a while |
| A JSON field shows up as `{"x":1}` | Nested values are exposed as raw JSON text; flatten the data if you need parts of it |
| A relative `file:` path isn't found | It is resolved from the directory you run `resonate` in |

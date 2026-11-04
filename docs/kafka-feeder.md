# Kafka Feeder

A [feeder](feeders.md) hands each iteration one row of data, exposed to
templates as `{{.Feeder.<column>}}`. A **Kafka feeder** takes those rows from the messages
of a Kafka topic instead of a CSV/JSON file: replay a large, real dataset, or feed a test
from data another system is producing.

For file feeders and the concepts common to all feeders, see the [Feeders guide](feeders.md).

It is a *data source* only. Resonate reads the topic; it does not produce load **to** Kafka.

It works under every protocol (`http`, `ws`, `sse`, `grpc`) and in the Go library.

## Quick start

You need a Kafka broker and a topic of JSON messages. Any broker works. These commands start a
single-node broker from a Homebrew install (Kafka 4.x, KRaft mode, no ZooKeeper):

```sh
export PATH=/opt/homebrew/opt/kafka/bin:$PATH
LOGS=$(mktemp -d)
sed "s#^log.dirs=.*#log.dirs=$LOGS#" /opt/homebrew/etc/kafka/server.properties > /tmp/kafka-server.properties
kafka-storage format --standalone -t $(kafka-storage random-uuid) -c /tmp/kafka-server.properties
kafka-server-start /tmp/kafka-server.properties        # leave running
```

Use `server.properties` (the combined broker + controller). Homebrew's `broker.properties` and
`controller.properties` are the two halves of a split deployment, and a lone broker will not start.

Create the topic and put some messages in it:

```sh
kafka-topics --bootstrap-server localhost:9092 --create --topic users --partitions 3
for n in alice bob carol dave erin; do
  echo "{\"email\":\"$n@example.com\",\"password\":\"pw-$n\"}"
done | kafka-console-producer --bootstrap-server localhost:9092 --topic users
```

Then a scenario, [`examples/http-kafka-feeder.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-kafka-feeder.yaml):

```yaml
protocol: http

load:
  duration: 30s
  rate: 10
  workers: 5

http:
  timeout: 5s
  feeder:
    kafka:
      brokers: ["localhost:9092"]
      topic: users
  targets:
    - method: POST
      url: "http://localhost:8080/login"
      body: |
        {"email": "{{.Feeder.email}}", "password": "{{.Feeder.password}}"}
```

```sh
resonate run examples/http-kafka-feeder.yaml
```

Every iteration takes the next message and sends its `email` and `password`. With five messages and
roughly 300 requests, the five rows repeat about 60 times each (see [wrapping](#when-the-topic-runs-out)).

## What a row contains

For `format: json` (the default) the message value must be a JSON object. Its top-level
fields become columns: strings as they are, numbers and booleans as text, and nested objects or
arrays as their raw JSON text. A message that is not a JSON object is skipped.

Every row, in either format, also has these columns. They start with `_` so they can't collide with
a field of your data by accident (if one does, the metadata wins):

| Column | Value |
| --- | --- |
| `{{.Feeder._key}}` | The message key (empty if none) |
| `{{.Feeder._value}}` | The whole message value, as text |
| `{{.Feeder._partition}}` | Partition number |
| `{{.Feeder._offset}}` | Offset within the partition |
| `{{.Feeder._timestamp}}` | Message timestamp, Unix milliseconds |
| `{{.Feeder._header_<name>}}` | The value of header `<name>`, for each header present |

`format: raw` gives *only* these columns, which suits messages that are not JSON: send the
payload as it is with `body: '{{.Feeder._value}}'`.

A column that isn't there renders as `""`, as everywhere else in resonate.

## Options

| Field | Default | Meaning |
| --- | --- | --- |
| `brokers` | required | Seed brokers, `host:port`. The client discovers the rest |
| `topic` | required | The topic to read |
| `format` | `json` | `json` or `raw`, as above |
| `from` | `start` | `start`: the oldest retained message. `end`: only messages produced after the run begins |
| `on_end` | `wrap` (`wait` with `from: end`) | `wrap` or `wait`, see below |
| `timeout` | `10s` | Connecting, and waiting for the first message |
| `tls`, `insecure`, `ca_file`, `cert_file`, `key_file` | off | TLS, with the same meaning as for `http` |
| `sasl_mechanism`, `username`, `password`, `password_env` | none | `PLAIN`, `SCRAM-SHA-256` or `SCRAM-SHA-512` |

The feeder block is `feeder.file` *or* `feeder.kafka`, never both, and `mode` (a file option) is
rejected beside `kafka`.

### When the topic runs out

- **`on_end: wrap`** (the default for `from: start`). Once every partition has been read up to its
  end, the feeder reads the topic again from the start, so rows cycle the way a file feeder's do.
  The end of each partition is recorded when a pass begins, so messages produced during a pass
  show up in the next one.
- **`on_end: wait`**. Keep consuming whatever is produced next. Use it with `from: end` to feed the
  test from live traffic: only messages produced after the run starts are used.

`from: end` with `on_end: wrap` is rejected: there would be nothing to wrap around.

If a `wait` topic goes quiet, a worker is given the most recent row again after one second (an
empty row if there has never been one). A quiet topic therefore slows the test down; it can't hang it.

### Authentication

This needs a SASL/TLS-enabled cluster, so the example is only checked for correct syntax.

<!-- docs-test: parse-only -->
```yaml
protocol: http
load:
  requests: 1000
  workers: 10
http:
  feeder:
    kafka:
      brokers: ["broker-1.example.com:9093", "broker-2.example.com:9093"]
      topic: users
      tls: true
      ca_file: /etc/ssl/kafka-ca.pem
      sasl_mechanism: SCRAM-SHA-512
      username: loadtest
      password_env: KAFKA_PASSWORD
  targets:
    - url: https://api.example.com/users/{{.Feeder.id}}
```

Prefer `password_env` to `password`: a scenario file is plain YAML and usually checked in. Setting
both is an error, as is naming a variable that isn't set.

## Behaviour worth knowing

- **No consumer group, no committed offsets.** Resonate assigns the topic's partitions to itself and
  never commits. A run neither disturbs nor depends on anyone else's consumers, and every run starts
  from the same place. (The flip side: it can't resume where a previous run stopped.)
- **Memory does not grow with the topic.** A background consumer fills a queue of at most 1000 rows
  and pauses when it is full. Resonate also holds the Kafka client's own fetch buffers, so memory
  depends on message size, not on how many messages the topic has. Measured on a laptop with a
  250,005-message topic of ~50-byte JSON messages: 58 MB at peak for a 3 s run and 62 MB for a 30 s
  run, against 28 MB for a `resonate hit` that uses no feeder. Contrast a file feeder in `sequential`
  or `random` mode, which loads the whole file into memory; use `mode: stream` for a big file.
- **Order across partitions is not defined.** Within one partition, messages come in order.
- **Failures surface before the run.** Unreachable brokers, a missing topic, bad options, and (for
  `from: start`) a topic with no usable message are all reported before any request is sent.
  `from: end` cannot require a message, because none exists yet.
- **Use enough data.** Each message is used once per pass. If the run sends more requests than the
  topic has messages, rows repeat; for unique requests make the topic at least `rate × duration`
  messages long.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `can't reach brokers [...]` | Nothing listening at `brokers`, or the broker's *advertised* address isn't reachable from where resonate runs |
| `topic "x": UNKNOWN_TOPIC_OR_PARTITION` | The topic doesn't exist; create it first (resonate doesn't) |
| `topic "x" has no messages` | `from: start` on an empty topic; produce some, or use `from: end` |
| `no usable message ... within 10s` | Messages exist but none parsed under `format: json`; check the data, or use `format: raw` |
| `on_end "wrap" needs from "start"` | `from: end` can't wrap; use `on_end: wait` |
| Every request has an empty field | The field name differs from the JSON key (names are case-sensitive), or the message isn't flat JSON |
| A lone broker won't start | Use the combined `server.properties`, not `broker.properties` |

## From Go

The `Scenario` builder's `Feeder(path, mode)` is file-only. A Kafka feeder is built with
`NewKafkaFeeder` and set on the options of whichever generator you use:

```go
feeder, err := resonate.NewKafkaFeeder(resonate.KafkaFeederOptions{
	Brokers: []string{"localhost:9092"},
	Topic:   "users",
	// Format, From, OnEnd, Timeout, TLS/Insecure/CAFile/CertFile/KeyFile,
	// SASLMechanism/Username/Password: same meaning as the YAML.
})
if err != nil {
	log.Fatal(err)
}
defer feeder.Close() // only after the run has returned

gen, err := resonate.NewHTTPGenerator(
	[]resonate.HTTPTarget{{
		Method: "POST",
		URL:    "http://localhost:8080/login",
		Body:   `{"email": "{{.Feeder.email}}"}`,
	}},
	resonate.HTTPOptions{Feeder: feeder},
)
```

`feeder.Next()` returns the next row as a `map[string]string`, so it can also be used directly.

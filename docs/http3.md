# HTTP/3

HTTP/3 is HTTP over QUIC: the same requests and responses, carried on UDP with TLS
1.3 built in instead of on TCP. resonate can send any HTTP load (and Server-Sent
Events streams) over HTTP/3. It is **opt-in**: nothing uses it unless you ask, and
if the target doesn't speak it, requests fail rather than quietly falling back to TCP.

You can ask for it from all three places resonate runs:

| Where | How |
| --- | --- |
| The CLI | `resonate hit https://host/path --http3` |
| A scenario file | `http3: true` under `http:` (or under `sse:`) |
| Go | `HTTPOptions.HTTP3 = true`, or `.HTTP3()` on the fluent `Scenario` builder |

This page is the full guide: a [quick start](#quick-start), each [entry point](#using-it)
in turn, [what it does and doesn't do](#what-it-does-and-doesnt-do), how to
[confirm requests really used HTTP/3](#confirming-requests-used-http3),
[connections and limits](#connections-and-limits), [troubleshooting](#troubleshooting),
and the [example server](#the-example-server) you can run against.

## Quick start

HTTP/3 servers aren't something most machines have lying around, so the repository
ships one. From a checkout of the repo:

```sh
make example-http3-server        # in one terminal: listens on :8443 (UDP and TCP)
```

It serves the same handlers over HTTP/3 (UDP) *and* over ordinary HTTPS (TCP) on the
same port, with a self-signed certificate (hence `--insecure`). `GET /proto` reports
which protocol version the request arrived over, so you can see the difference opting
in makes. Without `--http3` the request travels over TCP:

```sh
resonate hit https://localhost:8443/proto --insecure \
  --expect-body 'json:proto=HTTP/1.1' --duration 2s
```

With it, the very same URL arrives over QUIC:

```sh
resonate hit https://localhost:8443/proto --http3 --insecure \
  --expect-body 'json:proto=HTTP/3.0' --duration 2s
```

Both succeed. Swap the expectations and they fail with `body "json:proto": got
"HTTP/3.0", want "HTTP/1.1"`, which shows the check is real. `--expect-body` is how
you confirm what a run used; see [Confirming requests used HTTP/3](#confirming-requests-used-http3).

## Using it

### On the command line

`resonate hit` takes `--http3`. The URL must be `https://`, and the TLS flags work as
they do for HTTP: `--insecure`, `--ca-cert`, and `--cert`/`--key` for mutual TLS.

```sh
resonate hit https://api.example.com/health --http3 --rate 100 --duration 30s --workers 20
```

`--http3` can't be combined with `--h2c` or `--no-keepalive`; resonate says so before it
sends anything.

### In a scenario file

Set `http3: true` under `http:`. Every `targets`, `flow` and `setup` URL then goes over
HTTP/3, including relative URLs resolved through `base_url` (the resolved URL must be
`https://` too).

```yaml
protocol: http

load:
  duration: 10s
  rate: 20
  workers: 5

http:
  http3: true
  insecure: true          # the example server's certificate is self-signed
  timeout: 5s
  targets:
    - method: GET
      url: https://localhost:8443/proto
      expect_body:
        json:proto: HTTP/3.0   # fails if the request somehow arrived over TCP

assertions:
  - metric: success_rate
    min: "0.99"
```

It works for multi-step flows and `base_url`:

```yaml
protocol: http

load:
  requests: 20
  workers: 2

http:
  http3: true
  insecure: true
  base_url: https://localhost:8443
  flow:
    - method: GET
      url: /healthz
    - method: POST
      url: /echo
      headers:
        Content-Type: application/json
      body: '{"hello":"quic","seq":{{.Seq}}}'
      expect_body:
        json:hello: quic
```

and for Server-Sent Events, where the key goes under `sse:` ([SSE guide](sse.md)):

```yaml
protocol: sse

load:
  duration: 10s
  workers: 5

sse:
  url: https://localhost:8443/events
  http3: true
  insecure: true
  events: 10
  timeout: 5s
```

To trust the certificate instead of skipping verification, give it as the CA. The
example server can save its certificate with `-cert-out` (see
[the example server](#the-example-server)):

<!-- docs-test: parse-only -->
```yaml
protocol: http

load:
  duration: 10s
  workers: 5

http:
  http3: true
  ca_file: cert.pem       # instead of insecure: true
  targets:
    - url: https://localhost:8443/healthz
```

### From Go

The struct API takes the option on `HTTPOptions`:

```go
opts := resonate.DefaultHTTPOptions()
opts.HTTP3 = true
opts.Insecure = true // a self-signed target; prefer opts.CAFile

gen, err := resonate.NewHTTPGenerator(
	[]resonate.HTTPTarget{{URL: "https://localhost:8443/proto"}},
	opts,
)
if err != nil {
	log.Fatal(err) // includes a non-https:// URL: nothing is sent
}
defer gen.Close()

summary := resonate.Run(ctx, gen, resonate.Options{Rate: 50, Workers: 20, Duration: 30 * time.Second})
summary.Print(os.Stdout)
```

The fluent builder has a matching `HTTP3()`:

```go
summary, err := resonate.NewScenario().
	Get("https://localhost:8443/proto").
	HTTP3().
	Insecure().
	Rate(50).Workers(20).Duration(30 * time.Second).
	Run(ctx)
if err != nil {
	log.Fatal(err) // includes a non-https:// URL: nothing is sent
}
summary.Print(os.Stdout)
```

`HTTP3()` on a WebSocket scenario is an error at `Build()`/`Run()`, whichever order you
call it in, since WebSocket has no HTTP/3 transport here. For SSE, set it on the
embedded HTTP options:

```go
opts := resonate.DefaultSSEOptions()
opts.HTTP.HTTP3 = true
opts.HTTP.Insecure = true

gen, err := resonate.NewSSEGenerator(resonate.SSETarget{
	URL:    "https://localhost:8443/events",
	Events: 10,
}, opts)
if err != nil {
	log.Fatal(err)
}
defer gen.Close()
```

## What it does and doesn't do

- **Opt-in, never negotiated.** resonate doesn't read `Alt-Svc` headers or try HTTP/3
  and fall back. With `http3` on, every request uses HTTP/3 or fails.
- **`https://` only.** QUIC is always TLS 1.3, so a plain `http://` URL can never
  work. It is rejected when the scenario loads (or the generator is built), not per
  request: otherwise a bad URL would fail instantly and a run with no `rate` would spin.
  A URL built from a template (`{{ }}`) can't be checked until it is rendered, so a bad
  one there fails per request instead.
- **TLS options apply as usual:** `insecure`, `ca_file`, and `cert_file`/`key_file`
  for mutual TLS.
- **Can't be combined with `h2c`** (plaintext HTTP/2, a different protocol) **or
  `disable_keepalive`** (QUIC has no per-request connection to turn off). `max_idle_conns`
  is ignored, since there is no idle pool.
- **Everything else is unchanged:** templates, flows, `extract`, `expect_*`, feeders,
  assertions, load profiles and the reports all work as they do over TCP.
- **Not available for** gRPC (HTTP/2 only here) **or WebSocket**.

## Confirming requests used HTTP/3

Because the reports don't label the protocol version, don't assume: check. The simplest
way is a target that reports it. The example server's `/proto` does, and the
`expect_body` above turns that into a pass/fail. Against your own service, look at
the server side: its access log, or a response header it adds with the protocol.

An unreachable HTTP/3 port is not subtle: with `http3` on, a target that doesn't speak
it yields timeouts and a success rate near zero. If the success rate is high, requests
are going over HTTP/3.

## Connections and limits

QUIC multiplexes every request to a host over **one connection** (one UDP flow), so
`workers: 100` does not mean 100 connections. Servers cap concurrent streams per
connection, commonly around 100, and requests beyond that wait for a free stream,
which shows up as latency. There is no setting to open several connections yet, so a
very high-concurrency run against a single host can measure that limit instead of
the server.

Other things specific to UDP:

- **Firewalls.** UDP is often blocked where TCP isn't; an unreachable port looks like
  timeouts.
- **Socket buffers.** QUIC needs larger UDP buffers than the OS default on some
  systems, and the QUIC library may log a warning about it. At high rates, raise the
  OS limit (on Linux, `net.core.rmem_max` and `net.core.wmem_max`).
- **CPU.** Encrypting and framing every packet in user space costs more CPU than TCP
  with kernel offload, so the load generator can become the bottleneck sooner. Treat
  localhost numbers as relative.

## Troubleshooting

The first group fail when the scenario loads or the command starts, before any request:

| You see | What it means and what to do |
| --- | --- |
| `invalid url "http://...": http3 requires an https:// URL (QUIC is always TLS)` | Use an `https://` URL (or a relative one with an `https://` `base_url`). |
| `http3 and h2c are different protocols and can't be combined` | Drop `h2c`/`--h2c`. |
| `disable-keepalive isn't supported with http3: QUIC multiplexes all requests over one connection` | Drop `disable_keepalive`/`--no-keepalive`. |

These show up as failed requests:

| You see | What it means and what to do |
| --- | --- |
| `CRYPTO_ERROR 0x12a (local): tls: failed to verify certificate: x509: certificate signed by unknown authority` | The server's certificate isn't trusted. Give the CA as `ca_file`/`--ca-cert`, or use `insecure`/`--insecure` for a throwaway test. |
| `context deadline exceeded` on every request | Nothing answered on that UDP port within `timeout`: the target doesn't speak HTTP/3 there, the port is wrong, or UDP is blocked. |
| It works without `http3`, fails with it | The service serves HTTPS on TCP but not HTTP/3 (or not on that port). HTTP/3 and TCP listeners are separate. |
| It works with `http3`, but you suspect TCP was used | It wasn't: there is no fallback. To prove it, check the protocol on the server side. See [above](#confirming-requests-used-http3). |
| Latency climbs with `workers` | Likely the per-connection stream limit. See [Connections and limits](#connections-and-limits). |

## The example server

`examples/testhttp3server` is a small HTTP/3 server for trying all of the above without a
service of your own. It serves the same handlers over HTTP/3 (UDP) and HTTPS on TCP
(HTTP/1.1, and HTTP/2 where the client offers it) on one port, with a self-signed
certificate it generates at startup. It keeps no state, so it is a demo target, not a
benchmark.

```sh
make example-http3-server                  # :8443, UDP and TCP
resonate run examples/http-http3.yaml --dry-run
resonate run examples/sse-http3.yaml --dry-run
```

| Endpoint | What it does |
| --- | --- |
| `GET /proto` | JSON with the protocol version (`HTTP/3.0`, `HTTP/1.1`, ...), TLS version and ALPN of *this* request. |
| `POST /echo` | Echoes the request body. |
| `GET /events` | A finite Server-Sent Events stream (`?n=`, `?interval=`); each event carries the protocol it was sent over. |
| `GET /healthz` | `ok`. |

Flags:

| Flag | Effect |
| --- | --- |
| `-addr` | Where to listen, default `:8443`. A scenario's `url` must name the same port. |
| `-h3-only` | No TCP listener: a client that doesn't use `--http3` then can't connect at all. |
| `-cert-out FILE` | Also write the server's certificate to `FILE`, to use as `ca_file`/`--ca-cert` instead of `insecure`. |
| `-events`, `-event-interval` | Shape the `/events` stream (default 12 events, 20ms apart). |
| `-log-requests`, `-stats-interval` | Logging, below. |

Responses over TCP carry an `Alt-Svc: h3=":8443"` header, as a real deployment would
advertise HTTP/3. resonate doesn't act on it (HTTP/3 is opt-in), but a browser or `curl` would.

### Its logs

The server logs like an access log: a `call` line when a request arrives and a `done`
line when it finishes, each naming the **protocol version** the request used, so you can
watch `--http3` take effect:

```text
call HTTP/1.1 GET   /proto               from=[::1]:64867
done HTTP/1.1 GET   /proto               200     481µs bytes=70 from=[::1]:64867
call HTTP/3.0 GET   /proto               from=127.0.0.1:61654
done HTTP/3.0 GET   /proto               200     250µs bytes=76 from=127.0.0.1:61654
```

(The first request is resonate without `--http3`: its default transport speaks
HTTP/1.1. A browser or `curl` would show up as `HTTP/2.0` on the same TCP port.)

That is a lot of output under load, so for a performance run pass `-log-requests=false`
(`make example-http3-server ARGS=-log-requests=false`). The per-request lines stop and,
every `-stats-interval` (default 10s) while there is traffic, one summary line is printed,
broken down by protocol version. A request counts as failed when the server returns a
5xx. Ctrl-C prints the totals and shuts down.

## Example scenarios

| File | Shows | Needs |
| --- | --- | --- |
| [`http-http3.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-http3.yaml) | HTTP load over HTTP/3, with a check that the request really arrived over it | The example HTTP/3 server |
| [`sse-http3.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/sse-http3.yaml) | Server-Sent Events over HTTP/3 | The example HTTP/3 server |

## Not supported

- **Negotiation and fallback.** No `Alt-Svc` discovery, no trying HTTP/3 then TCP.
- **WebSocket and gRPC over HTTP/3.**
- **More than one QUIC connection per host,** so no way to spread load across connections.
- **Reporting the negotiated version.** The reports don't say which protocol a request used.
- **0-RTT and connection migration.**

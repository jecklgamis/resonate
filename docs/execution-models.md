# Execution Models

`workers` is the number of concurrent goroutines ("virtual users") in
flight; `rate` is the target number of *iterations* per second across all
of them combined — independent knobs, not multiplied together. An
iteration is one pass through `targets` (a single request) or `flow` (all
its steps).

If `workers` isn't set but `rate` is, it defaults to `max(10, ceil(rate))`.
If the target is slower than that (or `workers` was set too low), actual
throughput falls short of `rate` — resonate detects this after the run and
warns on stderr with a suggested `--workers` value.

## Open vs. Closed Model

**Closed model** (the default): `workers` is a hard concurrency cap. If
the target slows down, each worker just spends longer per iteration and
the achieved rate degrades toward `workers / latency` — the offered load
throttles itself instead of holding a fixed target rate.

**Open model** (set `max_workers` alongside `rate`): iteration starts are
paced strictly to `rate`, independent of how long prior iterations take,
and concurrency is allowed to grow — up to `max_workers` in flight — to
keep up. This is often called `constant-arrival-rate`.
`max_workers` is a hard ceiling; if even that many in-flight iterations
can't sustain `rate`, the report sets `rate_saturated: true` /
`peak_concurrency` instead of silently falling short.

```sh
# closed model: throughput caps at ~workers/latency if the target is slow
resonate hit http://localhost:8080/slow --rate 100 --workers 20 --duration 30s

# open model: rate is paced strictly; concurrency grows up to 200 to sustain it
resonate hit http://localhost:8080/slow --rate 100 --max-workers 200 --duration 30s
```

Outside `stages`, `max_workers` requires `rate` to be set.

| Model | Flags / Config | Practical Uses |
| --- | --- | --- |
| Closed (constant-vus) | `--workers N` (no `--rate`) | Fixed pool of real clients/sessions; simplest smoke/soak test |
| Closed, ramping (ramping-vus) | `load.stages` with `rate: 0` per stage | Gradual warm-up; simulate user count growing/shrinking without a throughput target |
| Open (constant-arrival-rate) | `--rate R --max-workers M` | Reproduce real production traffic shape; find the breaking point at a fixed request rate |
| Open, ramping (ramping-arrival-rate) | `load.stages` with `rate` set + `load.max_workers` | Traffic ramp simulating a launch/spike; capacity planning |
| Bounded arrival-rate | `load.stages` with `rate` set, `max_workers` unset | Rate-limit-aware testing with a hard concurrency ceiling (e.g. matching a downstream connection pool) |
| Per-VU iterations | `--iterations K --workers N` | "Each of N users does exactly K actions" — deterministic, reproducible |
| Shared iterations | `--requests N` | "Send exactly N requests total" — exact-cost tests, apples-to-apples comparisons |

## Connection Reuse at High Concurrency

`workers`/`max_workers` govern concurrent *iterations*, not connections —
a VU has no 1:1 relationship to a TCP connection. All HTTP connection
pooling is handled by one shared client per run (Go's `net/http.Transport`
under the hood): concurrent requests can open as many connections as they
need, but only `--max-idle-conns`/`http.max_idle_conns` of them (default
100) are kept open *idle* for reuse — beyond that, a connection is closed
right after its one request instead of kept warm.

That default becomes a real trap once `--max-workers` is raised past it:
a high-concurrency run with, say, `--max-workers 1000` but
`--max-idle-conns` left at 100 spends time re-dialing (TCP, plus TLS if
HTTPS) most connections — inflating tail latency and reducing achieved
rate for reasons that have nothing to do with the target's capacity.
Raise `--max-idle-conns`/`http.max_idle_conns` to at least match
`--max-workers`/`load.max_workers` any time you push concurrency up.
Measured on a 10k rps run, identical everything else: 7.3k/s achieved +
p99 1.46s at the default 100 idle conns, vs. 10.4k/s achieved + p99 736ms
at `--max-idle-conns 1000`.

```sh
# 1000 max in-flight requests, but only 100 idle connections kept for
# reuse — most connections get re-dialed per request under load.
resonate hit http://localhost:8080/ --rate 10000 --workers 100 --max-workers 1000

# same load, but --max-idle-conns raised to match --max-workers: connections
# stay warm, tail latency drops sharply, achieved rate meets target.
resonate hit http://localhost:8080/ --rate 10000 --workers 100 --max-workers 1000 --max-idle-conns 1000
```

## Ramp Profiles (Staged Load)

`load.stages` replaces flat `duration`/`rate`/`workers` with a schedule:
`workers` and `rate` ramp linearly from the previous stage's end values
(0 for the first stage) to each stage's target, over its duration. Repeat
a stage's values to hold steady instead of ramping. Each stage is
independently closed or open model, same distinction as above.

```yaml
load:
  max_workers: 200   # only matters for stages below that also set rate
  stages:
    - duration: 30s
      workers: 20
      rate: 0         # ramping-vus: ramp 0 -> 20 concurrent workers, unthrottled
    - duration: 2m
      workers: 20
      rate: 100       # ramping-arrival-rate: hold 100/s, concurrency free to grow past 20 up to max_workers
```

This avoids slamming a service with instant full load. `stages` is only
available via scenario files, not `resonate hit`. See
[`examples/http-stages.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-stages.yaml).

## Virtual User Lifecycle

By default a `worker` is a *sustained* virtual user: it loops for the
whole test. `load.iterations` bounds that instead — each VU runs exactly
`iterations` iterations, then departs, and a fresh VU (new identity, if
using `http.identities`) immediately takes over that slot.

```yaml
load:
  duration: 5m
  workers: 20
  rate: 50
  iterations: 10   # each simulated user makes 10 requests, then a new one logs in
```

- **`iterations` alone** (no `duration`/`requests`/`stages`): the run is
  exactly `workers * iterations` iterations, no replenishment — the
  classic "N virtual users, each K iterations" batch pattern.
- **`iterations` + `duration`/`requests`/`stages`**: VUs churn
  continuously for as long as the test runs — useful for simulating
  session expiry/re-authentication under sustained load.

`--iterations` also works on `resonate hit`, for the "N workers, K each"
pattern. See [`examples/http-vu-lifecycle.yaml`](https://github.com/jecklgamis/resonate/blob/main/examples/http-vu-lifecycle.yaml).

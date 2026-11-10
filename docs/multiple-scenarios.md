# Multiple Scenarios

A scenario file normally has one `load:` block, so one load profile. To run **several
workloads at the same time, each with its own load profile**, put them under `scenarios:`.
This is what Gatling calls several populations in one `setUp(...)`, and what k6 calls
`options.scenarios`: for example, 50 users browsing at a steady concurrency while 20
checkouts per second arrive at a constant rate, ten seconds later.

```yaml
protocol: http

http:                       # shared by every scenario
  base_url: http://localhost:8080
  timeout: 5s

scenarios:
  browse:                   # closed model: 50 concurrent users, ramped up
    load:
      stages:
        - { duration: 30s, workers: 50 }
        - { duration: 2m,  workers: 50 }
    http:
      targets:
        - { name: products, method: GET, url: /products }

  checkout:                 # open model: 20 new checkouts a second
    start_after: 10s
    load:
      duration: 2m
      rate: 20
      workers: 20
      max_workers: 300
    http:
      flow:
        - { name: add, method: POST, url: /cart, extract: { cart_id: "json:id" }, pause: 1s }
        - { name: pay, method: POST, url: "/cart/{{.Vars.cart_id}}/pay" }

assertions:
  - { scenario: checkout, step: pay, metric: latency_p95, max: 800ms }
  - { metric: success_rate, min: "0.99" }
```

Run it as any other scenario: `resonate run scenarios.yaml`. The scenarios start together,
run concurrently, and produce **one report** with a section for each.

## What a scenario is

Each entry under `scenarios:` is the same thing a single-scenario file is, plus a start
delay:

| Key | |
| --- | --- |
| `load` | **Required.** This scenario's own [load profile](execution-models.md): any of the closed, open, ramping or per-VU models. |
| `protocol` | `http`, `ws`, `grpc` or `sse`. Defaults to the file's `protocol`, which defaults to `http`. |
| `http` / `ws` / `grpc` / `sse` | The scenario's protocol section, the same keys as in a single-scenario file. Only the one matching its protocol. |
| `start_after` | A duration (`10s`, `1m30s`). The scenario starts this long after the run does. Not counted in its own `duration`. |

Scenario names are free-form, unique, and kept in the order you wrote them, which is the
order every report lists them in. A scenario can be any protocol, so one file can drive an
HTTP API and an SSE stream, or a gRPC service, together. A scenario is built on its own:
each has its own connection pool, feeder cursor and identity pool.

Everything is validated before the first request is sent. A typo in a scenario, an unknown
key anywhere in the file, a bad `load`, or an assertion naming a scenario or step that doesn't
exist fails the run at startup. (Files with `scenarios:` are parsed strictly. A
single-scenario file still ignores keys it doesn't know.)

## Shared sections

The file's top-level `http`, `ws`, `grpc` and `sse` sections are **defaults** for the
scenarios of that protocol. A scenario's own section is laid over them:

```yaml
protocol: http

http:
  base_url: http://localhost:8080
  timeout: 5s
  insecure: true

scenarios:
  public:
    load: { duration: 1m, workers: 20 }
    http:
      targets: [{ method: GET, url: /health }]      # inherits base_url, timeout, insecure

  admin:
    load: { duration: 1m, rate: 5 }
    http:
      timeout: 30s                                  # replaces the shared 5s
      base_url: http://localhost:8080/admin         # replaces the shared base_url
      targets: [{ method: GET, url: /stats }]
```

The merge is **by key**: a key a scenario sets replaces the shared one outright, even if it
sets it to `false`, `0` or `""`. Lists and maps (`identities`, `headers`, `targets`) are
replaced, never concatenated. Put the requests (`targets` or `flow`) in the scenario itself:
a shared `targets` together with a scenario's `flow` would give that scenario both, which is
rejected.

`load` is not shared. Each scenario sets its own, and a top-level `load:` in a file with
`scenarios:` is an error, because a load that applied to some scenarios and not others
would be easy to misread.

## Starting later

`start_after` delays one scenario while the others run:

```yaml
protocol: http

http:
  base_url: http://localhost:8080

scenarios:
  baseline:
    load: { duration: 3m, workers: 10 }
    http:
      targets: [{ method: GET, url: /health }]

  spike:                    # hits at 1m, for one minute
    start_after: 1m
    load: { duration: 1m, rate: 500, max_workers: 2000 }
    http:
      targets: [{ method: GET, url: /health }]
```

The whole run lasts until the last scenario finishes: here three minutes, with the spike
running from the first to the second minute. Pressing Ctrl-C (or an `abort`, below) stops every
scenario. One that hadn't started yet makes no requests and is still listed in the report.

## Assertions

An assertion without `scenario:` is checked against the **combined** results of every
scenario, and its `step:` may name a step from any of them. Add `scenario:` to check one
scenario alone, which is what you want when the scenarios have different goals:

```yaml
protocol: http

http:
  base_url: http://localhost:8080

scenarios:
  browse:
    load: { duration: 1m, workers: 20 }
    http:
      targets: [{ name: home, method: GET, url: / }]
  checkout:
    load: { duration: 1m, rate: 10, max_workers: 50 }
    http:
      flow:
        - { name: add, method: POST, url: /cart }
        - { name: pay, method: POST, url: /pay }

assertions:
  - { metric: success_rate, min: "0.99" }                                 # everything combined
  - { scenario: browse,   metric: latency_p95, max: 200ms }               # browse only
  - { scenario: checkout, step: pay, metric: latency_p99, max: 1s }       # one step of checkout
```

`scenario:` and `step:` combine; the step must belong to that scenario. A name that doesn't
exist is an error before the run starts. A scenario-scoped assertion on a protocol with no steps
(`ws`, `grpc`, `sse`) can't use `step:`. Failures name both:
`latency_p99{scenario=checkout,step=pay}: got 1.4s, want <= 1s`.

### Aborting

`abort: true` stops the run early when an assertion fails (see
[Stopping a run early](scenarios.md#stopping-a-run-early)). In a file with scenarios it
must name a `scenario:`, because a combined number can only be judged at the end. When it
trips, **every scenario stops**, not just that one, and the report says which one tripped it:

```yaml
assertions:
  - { scenario: checkout, metric: success_rate, min: "0.95", abort: true, abort_after: 10s }
```

## The report

**Console.** The usual summary comes first, for everything combined, followed by a
`Scenario: <name>` section with the same lines for each one.

**`report.json`.** The top level is the combined result, with exactly the fields of a
single-scenario report. A new `scenarios` array holds one entry per scenario, in file order.
Each entry has the same fields again, flat, plus `name` and `start_after`:

```json
{
  "schema_version": 1,
  "requests": 4200,
  "success_rate": 0.998,
  "scenarios": [
    { "name": "browse",   "requests": 3000, "success_rate": 1.0,   "latencies": { "p95": 4100000 } },
    { "name": "checkout", "requests": 1200, "success_rate": 0.995, "start_after": 10000000000 }
  ]
}
```

How the combined numbers are defined:

- Requests, success rate, bytes, percentiles and the time series cover every scenario's
  requests together; `duration` is the whole run, including any `start_after` waits.
- `steps` merges steps that share a label across scenarios. Give steps different names if
  you want them kept apart, or read the per-scenario `steps`.
- `rate_saturated` is `true` if any scenario's open-model run was saturated; `peak_concurrency`
  is reported per scenario only, since a combined peak isn't tracked.
- `aborted` names the scenario that stopped the run.

Adding the `scenarios` array does not change `schema_version`; a report from a
single-scenario run has no `scenarios` key at all.

**HTML.** A *Scenarios* table lists each one's requests, rate, success and latencies. The
*Run settings* table gets rows for each scenario, prefixed with its name. Charts show the
combined run.

**`results.jsonl`.** Each line gains a `"scenario"` field with the scenario that made the request.

## Connections and resources

All scenarios run in one process, so they share its CPU, memory and file descriptors; at
very high combined rates, watch the machine running resonate as well as the target.

Each scenario has its own HTTP client, so its own connection pool: `max_idle_conns` (and the
rest of the `http:` transport settings) applies per scenario, and one scenario's connections
never queue behind another's. Feeders and identity pools are per scenario too, so each starts
at its first row or identity.

## Coming from Gatling or k6

| Gatling / k6 | resonate |
| --- | --- |
| `constantUsersPerSec(n)` / `constant-arrival-rate` | `rate: n` with `max_workers` |
| `constantConcurrentUsers(n)` / `constant-vus` | `workers: n`, no `rate` |
| `rampUsers`, `rampConcurrentUsers` / `ramping-vus` | `stages` with `rate: 0` |
| `rampUsersPerSec` / `ramping-arrival-rate` | `stages` with `rate`, plus `max_workers` |
| `atOnceUsers(n)` / `per-vu-iterations` | `workers: n` with `iterations: 1` |
| `nothingFor(t)` / `startTime` | `start_after: t` |
| `setUp(a.inject(..), b.inject(..))` / `options.scenarios` | `scenarios:` |
| `details("request")` assertions / thresholds on a tag | `scenario:` and `step:` on an assertion |

A Gatling *user* is one pass through a scenario, which is a resonate *iteration*;
`iterations: 1` makes each virtual user run exactly one. See
[Execution Models](execution-models.md#virtual-user-lifecycle).

## Limits

- Every scenario brings its own `load`; there is no per-request weighting or throttling
  inside a scenario (`targets` are served round-robin).
- `abort` stops every scenario, not one. There is no "stop only this scenario" rule.
- `resonate hit` runs one HTTP request shape; scenarios are `resonate run` only.
- A `--dry-run` sends one iteration of each scenario, one after another, and ignores `start_after`.

## From Go

The library has the same thing: `resonate.NewSuite()` takes several `Scenario` builders, each
with its own load, and runs them together. See [Library](library.md#running-several-scenarios).

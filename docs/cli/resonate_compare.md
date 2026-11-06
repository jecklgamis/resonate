## resonate compare

Compare two JSON reports and fail on a regression

### Synopsis

Compare two runs' JSON reports (the report.json each 'run'/'hit' writes, or
--json output) and print every headline metric side by side, then each
named step's metrics when both runs have it.

Add --fail-on to gate a pipeline: the command exits non-zero when any rule
matches. A rule is <metric><op><threshold> over the change from baseline to
current — a threshold ending in % is relative to the baseline, otherwise it is
an absolute change in the metric's unit (a duration for latency metrics, a plain
number for the rest; success_rate and error_rate are ratios from 0 to 1). A
step's metric is written <step>:<metric>.

```
resonate compare <baseline.json> <current.json> [flags]
```

### Examples

```
  # p95 more than 10% slower, or throughput down more than 5%
  resonate compare baseline.json report.json --fail-on 'latency_p95>10%' --fail-on 'rate<-5%'

  # error rate up by more than one point; one step's p99 more than 20ms slower
  resonate compare baseline.json report.json --fail-on 'error_rate>0.01' --fail-on 'login:latency_p99>20ms'
```

### Options

```
      --fail-on stringArray   Exit non-zero if a metric's change from baseline matches, e.g. 'latency_p95>10%' or 'login:latency_p99>20ms' (repeatable)
  -h, --help                  help for compare
```

### SEE ALSO

* [resonate](resonate.md)	 - Load test HTTP, WebSocket, gRPC and SSE services


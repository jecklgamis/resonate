## resonate

Load test HTTP and WebSocket services

### Synopsis

resonate drives configurable load against HTTP and WebSocket targets.

Quick start:
  resonate hit https://example.com --duration 10s
  resonate run scenario.yaml

Use 'hit' for a one-off HTTP test with request, rate, TLS, templating,
response-check, and report options. Use 'run' for reusable YAML scenarios,
including multi-step flows and WebSocket tests. Add --dry-run to send one
real request or iteration; use --help on a command to see all its options.

### Options

```
  -h, --help   help for resonate
```

### SEE ALSO

* [resonate hit](resonate_hit.md)	 - Send HTTP load to a single URL
* [resonate run](resonate_run.md)	 - Run a load test scenario from a config file


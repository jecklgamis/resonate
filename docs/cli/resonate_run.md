## resonate run

Run a load test scenario from a config file

### Synopsis

Run a load test scenario from a scenario config YAML file.

```
resonate run <scenario.yaml> [flags]
```

### Examples

```
  # Full schema: run 'resonate schema'. More examples: docs/scenarios.md.
  protocol: http
  load:
    duration: 30s
    rate: 50
    workers: 10
  http:
    timeout: 5s
    base_url: http://localhost:8080
    targets:
      - method: GET
        url: /health
```

### Options

```
      --dry-run               Send exactly one real iteration and print its result(s), instead of running the full load test
  -h, --help                  help for run
      --html-report string    Write a self-contained HTML report to this path ("" disables it) (default "report.html")
      --json                  Print the report as JSON
      --json-report string    Write the JSON report to this path ("" disables it; independent of --json, which controls stdout) (default "report.json")
  -q, --quiet                 Suppress the periodic progress line on stderr
      --results-file string   Write one JSON object per individual request (JSON Lines) to this path as results complete ("" disables it) (default "results.jsonl")
```

### SEE ALSO

* [resonate](resonate.md)	 - Load test HTTP and WebSocket services


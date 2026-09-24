## resonate run

Run a load test scenario from a config file

```
resonate run <scenario.yaml> [flags]
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

* [resonate](resonate.md)	 - resonate is a load generator


## resonate hit

Send HTTP load to a single URL

### Synopsis

Send HTTP load to a single URL. The url, headers, query values, and body
may all contain {{ }} template expressions, re-rendered per request, e.g.
--body '{"id": {{.Seq}}, "tok": "{{uuid}}"}'. See README for the function list.

```
resonate hit <url> [flags]
```

### Options

```
      --assert stringArray          Assert a report metric after the run, e.g. --assert 'success_rate>=0.95' or --assert 'latency_p95<=500ms' (operators: >=, <=, ==, >, <; repeatable); a failed assertion gives a non-zero exit code for CI gating
  -d, --body string                 Request body (templated)
      --body-file string            Read request body from file (templated)
      --ca-cert string              Additional CA certificate(s) (PEM) to trust, e.g. for a private/internal CA
      --cert string                 Client certificate file for mTLS (requires --key)
      --dry-run                     Send exactly one real request and print its result, instead of running the full load test
      --duration duration           Test duration, e.g. 30s (default 10s if --requests not set)
      --expect-body stringArray     Response body check "rule=value" using the same rule language as extract ("json:<JSONPath>", "yaml:<JSONPath>", "xml:<XPath>"), e.g. --expect-body 'json:status=ok' (repeatable)
      --expect-header stringArray   Response header check "Key: Value" (exact match), or "Key:" to just require it's present (repeatable)
      --expect-status ints          Comma-separated status codes that count as success (overrides the default 2xx/3xx check), e.g. --expect-status 200,201,404
      --h2c                         Force HTTP/2 over plaintext (prior knowledge); mutually exclusive with --insecure/--cert/--key/--ca-cert
  -H, --header stringArray          Request header "Key: Value" (repeatable, templated)
  -h, --help                        help for hit
      --html-report string          Write a self-contained HTML report to this path ("" disables it) (default "report.html")
      --insecure                    Skip TLS certificate verification
      --iterations uint             Requests per virtual user before it departs (0 = runs for the whole test); with no --duration/--requests, total = --workers * --iterations
      --json                        Print the report as JSON
      --json-report string          Write the JSON report to this path ("" disables it; independent of --json, which controls stdout) (default "report.json")
      --key string                  Client private key file for mTLS (requires --cert)
      --max-idle-conns int          Max idle (keep-alive) connections per target host (0 = Go's default, 100)
      --max-response-body int       Max response bytes to read/count per request; the rest is discarded (0 = unlimited)
      --max-workers int             Open model: with --rate, allow concurrency to grow up to this many in-flight requests to sustain the target rate under latency, instead of capping at --workers (0 = disabled, closed model)
  -X, --method string               HTTP method (default "GET")
      --no-keepalive                Disable HTTP keep-alive: open a fresh connection per request instead of reusing pooled connections; mutually exclusive with --h2c
      --no-redirect                 Do not follow HTTP redirects
  -Q, --query stringArray           Query param "key=value" (repeatable, templated)
  -q, --quiet                       Suppress the periodic progress line on stderr
      --rate float                  Target requests/sec across all workers (0 = unlimited)
      --raw-body-file string        Read request body from file and send it as-is, bypassing templating (for large/binary payloads); overrides --body/--body-file
      --requests uint               Total number of requests to send (overrides --duration as the stop condition)
      --results-file string         Write one JSON object per individual request (JSON Lines) to this path as results complete ("" disables it) (default "results.jsonl")
      --timeout duration            Per-request timeout (default 30s)
      --workers int                 Number of concurrent workers (0 = 10, or rate-aware if --rate is set)
```

### SEE ALSO

* [resonate](resonate.md)	 - resonate is a load generator


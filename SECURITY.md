# Security Policy

resonate is alpha software; expect rough edges, but security reports are
taken seriously regardless of project maturity.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for a suspected security
vulnerability. Instead, use GitHub's private vulnerability reporting for
this repository: go to the **Security** tab →
**Report a vulnerability**. This opens a private draft advisory visible
only to the maintainer and lets us coordinate a fix before any public
disclosure.

If that option isn't available (e.g. it hasn't been enabled on this
repo yet), open a regular issue asking for a private channel to be set
up, without describing the vulnerability itself.

Please include:

- The version/commit you're using.
- Steps to reproduce, or a minimal scenario file/flags that trigger it.
- The impact you believe it has.

## Scope and what to expect

resonate is a load-generation CLI/library that dials network targets
and runs scenario files that can read local files (`body_file`,
`raw_body_file`, `feeder.file`) and environment variables (the `env`
template function). A scenario or CLI invocation is closer to a small
script than a passive config file — **only run scenario files from
sources you trust**, the same way you would a shell script or a CI
config. Reports about resonate faithfully doing what an attacker-crafted
scenario file *tells it to do* (reading a file it was pointed at,
sending an env var it was told to send) are expected behavior, not
vulnerabilities in resonate itself — the trust boundary is the scenario
file/CLI invocation, not resonate's parsing of it.

Vulnerabilities of interest include (non-exhaustively): TLS/certificate
verification being silently bypassed outside of `--insecure`, a crash
or resource exhaustion triggerable by a *target server's* response
(not by the operator's own config), path traversal or unintended file
access beyond what a scenario file explicitly configures, or dependency
vulnerabilities affecting resonate's actual usage of them.

## Response

This is a solo-maintained alpha project — there's no formal SLA, but
reports will be acknowledged and triaged as soon as reasonably
possible.

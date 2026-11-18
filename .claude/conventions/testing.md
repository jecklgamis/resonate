# Testing

## Testing patterns (follow these, don't invent new ones)

- **Root package:** `resonate_test.go` is `package resonate_test` and imports only the public package — the facade's contract test. If it breaks without touching the four facade files, the public surface moved unintentionally.
- **`internal/generator`:** `httptest.Server` handlers that record requests (`recordingServer`/`jsonHandler`); `newTestCA` for TLS/mTLS; set `ConnState` on `httptest.NewUnstartedServer` *before* `Start()`; gRPC tests use in-process servers (grpc-go `health`, hand-registered `dynamicpb` services) — no generated code; Kafka tests use `kfake`.
- **`internal/engine`:** `fakeGenerator` drives `Run` directly — no network.
- **`internal/cli`:** `captureStdout`/`captureStderr` redirect the real fds (`runAndReport` writes to them directly); keep per-file helpers distinct (e.g. `grpcRunJSON`) to avoid collisions.
- **Example servers:** their tests run the *shipped* example YAMLs and doc scenarios through the real CLI, asserting exact request counts; they're in the root module so `make check` covers them.
- **Mutation-check tests that guard a race, leak or ordering rule** (delete the fix, confirm the test fails) — several here passed vacuously until checked.

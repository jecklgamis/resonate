package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. runAndReport writes the report straight to
// os.Stdout (not through cobra's SetOut), so this is the only way to observe
// a command's real output end-to-end.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return string(out)
}

// captureStderr is captureStdout's counterpart for os.Stderr, used to check
// warnings runAndReport/RunE print directly rather than returning as errors.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	fn()

	w.Close()
	os.Stderr = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	return string(out)
}

func TestHitCommandEndToEndJSON(t *testing.T) {
	t.Chdir(t.TempDir()) // runAndReport now writes report.html/report.json by default; keep the repo clean
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "3", "--workers", "1", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	var summary struct {
		Requests    int     `json:"requests"`
		SuccessRate float64 `json:"success_rate"`
	}
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	if summary.Requests != 3 {
		t.Errorf("Requests = %d, want 3", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1 (all 200s)", summary.SuccessRate)
	}
}

func TestHitCommandDryRunSendsExactlyOneRequest(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--dry-run", "--rate", "50"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server got %d hits, want exactly 1 for --dry-run (the full load test must not run)", got)
	}
	if !strings.Contains(out, "status=200") {
		t.Errorf("dry-run output = %q, want it to show the request's status", out)
	}
}

func TestHitCommandQuietSuppressesProgress(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--quiet"})

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
		})
	})
	if strings.Contains(stderr, "elapsed") {
		t.Errorf("stderr = %q, --quiet should suppress the progress line", stderr)
	}
}

func TestHitCommandMaxIdleConnsFlagAccepted(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "2", "--workers", "1", "--max-idle-conns", "5", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, `"requests": 2`) {
		t.Errorf("output = %q, want a normal completed run with --max-idle-conns set", out)
	}
}

func TestHitCommandExpectStatusAcceptsNonDefaultCode(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--expect-status", "404", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, `"success_rate": 1`) {
		t.Errorf("output = %q, want success_rate 1 (404 explicitly expected)", out)
	}
}

func TestHitCommandExpectStatusInvalidCodeErrors(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "--expect-status", "999", "--requests", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for an invalid --expect-status code")
	}
}

func TestHitCommandAssertPassExitsZero(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "5", "--workers", "1", "--quiet", "--assert", "success_rate>=0.99"})

	var err error
	captureStdout(t, func() {
		err = cmd.Execute()
	})
	if err != nil {
		t.Errorf("Execute error: %v, want nil (assertion should pass)", err)
	}
}

func TestHitCommandAssertFailExitsNonZero(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{srv.URL, "--requests", "5", "--workers", "1", "--quiet", "--assert", "latency_p95<=1ns"})

	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmd.Execute()
		})
	})
	if err == nil {
		t.Fatal("expected a non-nil error when an assertion fails (for a non-zero process exit)")
	}
	if !strings.Contains(stderr, "latency_p95") {
		t.Errorf("stderr = %q, want it to name the failed metric", stderr)
	}
}

func TestHitCommandAssertInvalidExpressionErrors(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "--requests", "1", "--assert", "not-a-valid-expression"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for a malformed --assert expression")
	}
}

func TestHitCommandExpectHeaderAcceptsMatchingResponse(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--expect-header", "Content-Type: application/json", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, `"success_rate": 1`) {
		t.Errorf("output = %q, want success_rate 1 (Content-Type matches)", out)
	}
}

func TestHitCommandExpectHeaderRejectsMismatch(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--expect-header", "Content-Type: application/json", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, `"success_rate": 0`) {
		t.Errorf("output = %q, want success_rate 0 (Content-Type mismatch)", out)
	}
}

func TestHitCommandExpectHeaderExistsOnlySyntax(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "whatever")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--expect-header", "X-Request-Id:", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, `"success_rate": 1`) {
		t.Errorf("output = %q, want success_rate 1 (header present, no value required)", out)
	}
}

func TestHitCommandExpectHeaderMalformedFlagErrors(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "--expect-header", "no-colon-here", "--requests", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for a malformed --expect-header value (missing colon)")
	}
}

func TestHitCommandExpectBodyJSONAcceptsMatchingResponse(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--expect-body", "json:status=ok", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, `"success_rate": 1`) {
		t.Errorf("output = %q, want success_rate 1 (json:status matches)", out)
	}
}

func TestHitCommandExpectBodyRejectsMismatch(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error"}`))
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--expect-body", "json:status=ok", "--json"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})
	if !strings.Contains(out, `"success_rate": 0`) {
		t.Errorf("output = %q, want success_rate 0 (json:status mismatch)", out)
	}
}

func TestHitCommandExpectBodyMalformedFlagErrors(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "--expect-body", "no-equals-here", "--requests", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for a malformed --expect-body value (missing =)")
	}
}

func TestHitCommandExpectBodyUnknownRuleErrors(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "--expect-body", "bogus:rule=x", "--requests", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected a construction-time error for an unrecognized --expect-body rule prefix")
	}
}

func TestHitCommandTemplatedRequest(t *testing.T) {
	t.Chdir(t.TempDir())
	var gotPath, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Get("X-Seq")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{
		srv.URL + "/items/{{.Seq}}",
		"-H", "X-Seq: {{.Seq}}",
		"--requests", "1", "--workers", "1", "--json",
	})

	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	if gotPath != "/items/0" {
		t.Errorf("request path = %q, want /items/0", gotPath)
	}
	if gotHeader != "0" {
		t.Errorf("X-Seq header = %q, want 0", gotHeader)
	}
}

func TestHitCommandMissingURLArg(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error with no URL argument")
	}
}

func TestHitCommandInvalidHeaderFlag(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "-H", "no-colon"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for a malformed -H flag")
	}
}

func TestHitCommandInvalidURLFailsFastNotSlow(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	// No --requests/--duration: this would fall back to the 10s default
	// duration if the bad URL weren't caught at construction time.
	cmd.SetArgs([]string{"not-a-valid-url"})

	start := time.Now()
	var runErr error
	captureStdout(t, func() {
		runErr = cmd.Execute()
	})
	elapsed := time.Since(start)

	if runErr == nil {
		t.Fatal("expected an error for an invalid literal URL")
	}
	if elapsed > time.Second {
		t.Errorf("command took %v to fail, want near-instant (it must not fall through to a 10s run)", elapsed)
	}
}

func TestHitCommandNegativeRateErrors(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "--rate", "-5", "--requests", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for a negative --rate")
	}
}

func TestHitCommandNegativeWorkersErrors(t *testing.T) {
	cmd := newHitCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"http://example.test", "--workers", "-1", "--requests", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for negative --workers")
	}
}

func TestHitCommandBothBodyFlagsWarns(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	bodyFile := t.TempDir() + "/body.json"
	if err := os.WriteFile(bodyFile, []byte(`{"from":"file"}`), 0o644); err != nil {
		t.Fatalf("writing temp body file: %v", err)
	}

	cmd := newHitCommand()
	cmd.SetArgs([]string{
		srv.URL, "--body", "from-flag", "--body-file", bodyFile,
		"--requests", "1", "--workers", "1", "--json",
	})

	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "--body") || !strings.Contains(stderr, "--body-file") {
		t.Errorf("stderr = %q, want a warning mentioning both --body and --body-file", stderr)
	}
}

func TestHitCommandRawBodyFileSentUnrendered(t *testing.T) {
	t.Chdir(t.TempDir())
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rawFile := t.TempDir() + "/payload.bin"
	raw := []byte(`literal {{ not a template }} bytes`)
	if err := os.WriteFile(rawFile, raw, 0o644); err != nil {
		t.Fatalf("writing temp raw body file: %v", err)
	}

	cmd := newHitCommand()
	cmd.SetArgs([]string{
		srv.URL, "--raw-body-file", rawFile,
		"--requests", "1", "--workers", "1", "--json",
	})

	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	if gotBody != string(raw) {
		t.Errorf("server received body %q, want %q (unrendered)", gotBody, string(raw))
	}
}

func TestHitCommandRawBodyFileOverridesBodyWithWarning(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rawFile := t.TempDir() + "/payload.bin"
	if err := os.WriteFile(rawFile, []byte("raw"), 0o644); err != nil {
		t.Fatalf("writing temp raw body file: %v", err)
	}

	cmd := newHitCommand()
	cmd.SetArgs([]string{
		srv.URL, "--body", "from-flag", "--raw-body-file", rawFile,
		"--requests", "1", "--workers", "1", "--json",
	})

	var stderr string
	captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute error: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "--raw-body-file") {
		t.Errorf("stderr = %q, want a warning mentioning --raw-body-file", stderr)
	}
}

func TestHitCommandResultsFileWritesOneJSONLinePerRequest(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	resultsPath := "results.jsonl"
	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "5", "--workers", "1", "--quiet", "--results-file", resultsPath})

	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	data, err := os.ReadFile(resultsPath)
	if err != nil {
		t.Fatalf("reading results file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5 (one per request)", len(lines))
	}
	for i, line := range lines {
		var rec struct {
			StatusCode int     `json:"status_code"`
			LatencyMS  float64 `json:"latency_ms"`
			Protocol   string  `json:"protocol"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d isn't valid JSON: %v\nline: %s", i, err, line)
		}
		if rec.StatusCode != http.StatusOK {
			t.Errorf("line %d: status_code = %d, want 200", i, rec.StatusCode)
		}
		if rec.Protocol != "http" {
			t.Errorf("line %d: protocol = %q, want \"http\"", i, rec.Protocol)
		}
	}
}

func TestHitCommandResultsFileWrittenByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--quiet"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	if _, err := os.Stat("results.jsonl"); err != nil {
		t.Errorf("results.jsonl not written by default: %v", err)
	}
}

func TestHitCommandResultsFileEmptyStringDisablesIt(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cmd := newHitCommand()
	cmd.SetArgs([]string{srv.URL, "--requests", "1", "--workers", "1", "--quiet", "--results-file", ""})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
	})

	if _, err := os.Stat("results.jsonl"); !os.IsNotExist(err) {
		t.Error("a results file was written despite --results-file \"\"")
	}
}

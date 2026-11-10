package cli

// Contract tests: the resonate-fleet worker (and CI pipelines) depend on the
// exact invocation, output files and exit codes checked here. If one of these
// fails, you are making a breaking change; read docs/reports.md#stability and
// docs/usage.md#exit-codes before "fixing" the test.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func contractServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func contractScenario(url, extra string) string {
	return fmt.Sprintf(`protocol: http
load:
  requests: 20
  workers: 2
http:
  targets:
    - method: GET
      url: %s
%s`, url, extra)
}

// workerArgs is the command line resonate-fleet's worker runs (task.go), in
// the task directory with the scenario saved as scenario.yaml.
var workerArgs = []string{
	"run", "scenario.yaml",
	"--quiet",
	"--html-report", "report.html",
	"--json-report", "report.json",
	"--results-file", "results.jsonl",
}

func runInDir(t *testing.T, scenario string, args []string) (dir string, err error) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scenario.yaml"), []byte(scenario), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	root := NewRootCommand("test")
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(args)
	return dir, root.Execute()
}

func TestContractWorkerInvocationWritesReports(t *testing.T) {
	srv := contractServer(t)
	dir, err := runInDir(t, contractScenario(srv.URL, ""), workerArgs)
	if err != nil {
		t.Fatalf("worker invocation failed: %v", err)
	}
	for _, name := range []string{"report.html", "report.json", "results.jsonl"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err != nil || st.Size() == 0 {
			t.Errorf("%s missing or empty after a run (err=%v)", name, err)
		}
	}
}

// The fields resonate-fleet reads from report.json, with their JSON types.
func TestContractReportJSONFields(t *testing.T) {
	srv := contractServer(t)
	dir, err := runInDir(t, contractScenario(srv.URL, ""), workerArgs)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("report.json is not valid JSON: %v", err)
	}
	if v, ok := rep["schema_version"].(float64); !ok || int(v) != 1 {
		t.Errorf("schema_version = %v, want 1", rep["schema_version"])
	}
	for _, k := range []string{"requests", "success", "success_rate", "duration", "requests_per_sec", "bytes_in", "bytes_out"} {
		if _, ok := rep[k].(float64); !ok {
			t.Errorf("report.json %q missing or not a number: %v", k, rep[k])
		}
	}
	if rep["requests"].(float64) != 20 {
		t.Errorf("requests = %v, want 20", rep["requests"])
	}
	// Durations are integer nanoseconds.
	if d := rep["duration"].(float64); d <= 0 {
		t.Errorf("duration = %v, want positive nanoseconds", d)
	}
	lat, ok := rep["latencies"].(map[string]any)
	if !ok {
		t.Fatalf("latencies missing or not an object: %v", rep["latencies"])
	}
	for _, k := range []string{"min", "mean", "stddev", "p50", "p75", "p90", "p95", "p99", "max"} {
		if _, ok := lat[k].(float64); !ok {
			t.Errorf("latencies.%s missing or not a number: %v", k, lat[k])
		}
	}
	codes, ok := rep["status_codes"].(map[string]any)
	if !ok || codes["200"] != float64(20) {
		t.Errorf("status_codes = %v, want {\"200\": 20}", rep["status_codes"])
	}
}

// The fields resonate-fleet reads from each results.jsonl line.
func TestContractResultsJSONLFields(t *testing.T) {
	srv := contractServer(t)
	dir, err := runInDir(t, contractScenario(srv.URL, ""), workerArgs)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", n, err)
		}
		if s, ok := line["timestamp"].(string); !ok || s == "" {
			t.Errorf("line %d: timestamp missing or not a string: %v", n, line["timestamp"])
		}
		for _, k := range []string{"latency_ms", "status_code", "bytes_in", "bytes_out"} {
			if _, ok := line[k].(float64); !ok {
				t.Errorf("line %d: %q missing or not a number: %v", n, k, line[k])
			}
		}
		if line["protocol"] != "http" {
			t.Errorf("line %d: protocol = %v, want http", n, line["protocol"])
		}
	}
	if n != 20 {
		t.Errorf("results.jsonl has %d lines, want 20", n)
	}
}

func TestContractExitCodes(t *testing.T) {
	srv := contractServer(t)

	t.Run("success is 0", func(t *testing.T) {
		_, err := runInDir(t, contractScenario(srv.URL, ""), workerArgs)
		if got := ExitCode(err); got != ExitOK {
			t.Errorf("exit code = %d (%v), want 0", got, err)
		}
	})
	t.Run("failed assertion is 3", func(t *testing.T) {
		extra := "assertions:\n  - metric: requests\n    min: 1000000\n"
		_, err := runInDir(t, contractScenario(srv.URL, extra), workerArgs)
		if got := ExitCode(err); got != ExitAssertion {
			t.Errorf("exit code = %d (%v), want 3", got, err)
		}
	})
	t.Run("unknown flag is 2", func(t *testing.T) {
		_, err := runInDir(t, contractScenario(srv.URL, ""), []string{"run", "scenario.yaml", "--no-such-flag"})
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("exit code = %d (%v), want 2", got, err)
		}
	})
	t.Run("missing scenario file is 2", func(t *testing.T) {
		_, err := runInDir(t, contractScenario(srv.URL, ""), []string{"run", "nope.yaml"})
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("exit code = %d (%v), want 2", got, err)
		}
	})
	t.Run("wrong argument count is 2", func(t *testing.T) {
		_, err := runInDir(t, contractScenario(srv.URL, ""), []string{"run"})
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("exit code = %d (%v), want 2", got, err)
		}
	})
}

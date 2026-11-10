package cli

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jecklgamis/resonate/internal/report"
)

// pathCounter is a server that counts requests per path and remembers when
// each path was first hit, so a test can say what ran, how often, and when.
type pathCounter struct {
	*httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	first  map[string]time.Time
	status map[string]int
}

func newPathCounter(t *testing.T) *pathCounter {
	t.Helper()
	pc := &pathCounter{hits: map[string]int{}, first: map[string]time.Time{}, status: map[string]int{}}
	pc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pc.mu.Lock()
		pc.hits[r.URL.Path]++
		if _, ok := pc.first[r.URL.Path]; !ok {
			pc.first[r.URL.Path] = time.Now()
		}
		code := pc.status[r.URL.Path]
		pc.mu.Unlock()
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(pc.Close)
	return pc
}

func (pc *pathCounter) count(path string) int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.hits[path]
}

func (pc *pathCounter) total() int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	n := 0
	for _, c := range pc.hits {
		n += c
	}
	return n
}

func (pc *pathCounter) setStatus(path string, code int) {
	pc.mu.Lock()
	pc.status[path] = code
	pc.mu.Unlock()
}

func (pc *pathCounter) firstHit(path string) time.Time {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.first[path]
}

// multiRun is what one `resonate run` of a scenarios file produced.
type multiRun struct {
	err            error
	stdout, stderr string
	dir            string
}

func (r multiRun) summary(t *testing.T) report.Summary {
	t.Helper()
	f, err := os.Open(filepath.Join(r.dir, "report.json"))
	if err != nil {
		t.Fatalf("no report.json: %v (err=%v)", err, r.err)
	}
	defer f.Close()
	s, err := report.ReadSummary(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runScenarios runs a scenario file through the real run command, with every
// report file written into a fresh temp dir.
func runScenarios(t *testing.T, yaml string, extraArgs ...string) multiRun {
	t.Helper()
	dir := t.TempDir()
	path := writeTempScenario(t, yaml)
	cmd := newRunCommand()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	args := []string{path, "--quiet",
		"--html-report", filepath.Join(dir, "report.html"),
		"--json-report", filepath.Join(dir, "report.json"),
		"--results-file", filepath.Join(dir, "results.jsonl")}
	cmd.SetArgs(append(args, extraArgs...))
	r := multiRun{dir: dir}
	r.stderr = captureStderr(t, func() {
		r.stdout = captureStdout(t, func() { r.err = cmd.Execute() })
	})
	return r
}

func scenario(t *testing.T, s report.Summary, name string) report.ScenarioSummary {
	t.Helper()
	for _, sc := range s.Scenarios {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("report has no scenario %q (has %v)", name, s.Scenarios)
	return report.ScenarioSummary{}
}

func TestRunScenariosFileRunsEveryScenarioWithItsOwnProfile(t *testing.T) {
	pc := newPathCounter(t)
	r := runScenarios(t, `
protocol: http
http:
  base_url: `+pc.URL+`
scenarios:
  browse:                        # closed model, exactly 40 iterations
    load: {requests: 40, workers: 2}
    http:
      targets:
        - {name: products, method: GET, url: /products}
  checkout:                      # open model, exactly 10 iterations of a 2-step flow
    load: {requests: 10, workers: 1, rate: 200, max_workers: 4}
    http:
      flow:
        - {name: add, method: GET, url: /cart}
        - {name: pay, method: GET, url: /pay}
`)
	if r.err != nil {
		t.Fatalf("run failed: %v\nstderr: %s", r.err, r.stderr)
	}

	// The server is the ground truth: each scenario made exactly the requests
	// its own load asked for.
	if got := pc.count("/products"); got != 40 {
		t.Errorf("/products hits = %d, want 40", got)
	}
	if pc.count("/cart") != 10 || pc.count("/pay") != 10 {
		t.Errorf("/cart=%d /pay=%d, want 10 each", pc.count("/cart"), pc.count("/pay"))
	}

	s := r.summary(t)
	if s.Requests != 60 || pc.total() != 60 {
		t.Errorf("combined requests = %d (server saw %d), want 60", s.Requests, pc.total())
	}
	browse, checkout := scenario(t, s, "browse"), scenario(t, s, "checkout")
	if browse.Requests != 40 || checkout.Requests != 20 {
		t.Errorf("per-scenario requests = %d / %d, want 40 / 20", browse.Requests, checkout.Requests)
	}
	if s.Scenarios[0].Name != "browse" || s.Scenarios[1].Name != "checkout" {
		t.Errorf("scenarios listed as %s, %s; want file order", s.Scenarios[0].Name, s.Scenarios[1].Name)
	}
	if len(browse.Steps) != 1 || len(checkout.Steps) != 2 {
		t.Errorf("per-scenario steps = %d / %d, want 1 / 2", len(browse.Steps), len(checkout.Steps))
	}
	if len(s.Steps) != 3 {
		t.Errorf("combined steps = %d, want products/add/pay", len(s.Steps))
	}
	if s.SchemaVersion != report.SchemaVersion {
		t.Errorf("schema_version = %d", s.SchemaVersion)
	}

	for _, want := range []string{"Scenario: browse", "Scenario: checkout"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	html, err := os.ReadFile(filepath.Join(r.dir, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "<h2>Scenarios</h2>") || !strings.Contains(string(html), "browse: Rate") {
		t.Error("the HTML report should list the scenarios and per-scenario run settings")
	}
}

// results.jsonl tags each request with its scenario, and still has every
// field the contract promises.
func TestRunScenariosResultsFileTagsEachRequestWithItsScenario(t *testing.T) {
	pc := newPathCounter(t)
	r := runScenarios(t, `
http: {base_url: `+pc.URL+`}
scenarios:
  a:
    load: {requests: 7, workers: 1}
    http: {targets: [{method: GET, url: /a}]}
  b:
    load: {requests: 5, workers: 1}
    http: {targets: [{method: GET, url: /b}]}
`)
	if r.err != nil {
		t.Fatal(r.err)
	}
	f, err := os.Open(filepath.Join(r.dir, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	counts := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"timestamp", "latency_ms", "status_code", "bytes_in", "bytes_out", "protocol"} {
			if _, ok := rec[k]; !ok {
				t.Fatalf("results line lacks the stable field %q: %s", k, sc.Text())
			}
		}
		counts[rec["scenario"].(string)]++
	}
	if counts["a"] != 7 || counts["b"] != 5 || len(counts) != 2 {
		t.Errorf("results per scenario = %v, want a:7 b:5", counts)
	}
}

func TestRunScenarioSingleFileResultsHaveNoScenarioField(t *testing.T) {
	pc := newPathCounter(t)
	r := runScenarios(t, "load: {requests: 3, workers: 1}\nhttp: {targets: [{method: GET, url: "+pc.URL+"/x}]}\n")
	if r.err != nil {
		t.Fatal(r.err)
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, "results.jsonl"))
	if strings.Contains(string(b), `"scenario"`) {
		t.Errorf("a single-scenario results file grew a scenario field:\n%s", b)
	}
	if s := r.summary(t); len(s.Scenarios) != 0 {
		t.Errorf("single-scenario report has scenarios: %+v", s.Scenarios)
	}
}

// The shared section is a default, and a scenario's own keys beat it.
func TestRunScenariosInheritSharedSectionAndOverrideIt(t *testing.T) {
	shared, other := newPathCounter(t), newPathCounter(t)
	r := runScenarios(t, `
http:
  base_url: `+shared.URL+`
scenarios:
  inherits:
    load: {requests: 6, workers: 1}
    http: {targets: [{method: GET, url: /who}]}
  overrides:
    load: {requests: 4, workers: 1}
    http:
      base_url: `+other.URL+`
      targets: [{method: GET, url: /who}]
`)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if shared.count("/who") != 6 || other.count("/who") != 4 {
		t.Errorf("shared server saw %d, other saw %d; want 6 and 4", shared.count("/who"), other.count("/who"))
	}
}

func TestRunScenariosStartAfterDelaysOnlyThatScenario(t *testing.T) {
	pc := newPathCounter(t)
	start := time.Now()
	r := runScenarios(t, `
http: {base_url: `+pc.URL+`}
scenarios:
  now:
    load: {requests: 3, workers: 1}
    http: {targets: [{method: GET, url: /now}]}
  later:
    start_after: 600ms
    load: {requests: 3, workers: 1}
    http: {targets: [{method: GET, url: /later}]}
`)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if d := pc.firstHit("/now").Sub(start); d > 400*time.Millisecond {
		t.Errorf("'now' began %s in; it has no start_after", d)
	}
	if d := pc.firstHit("/later").Sub(start); d < 600*time.Millisecond {
		t.Errorf("'later' began %s in, before its 600ms start_after", d)
	}
	s := r.summary(t)
	if got := scenario(t, s, "later").StartAfter; got != 600*time.Millisecond {
		t.Errorf("report start_after = %s", got)
	}
}

func TestRunScenariosScopedAssertionsGateOnTheRightScenario(t *testing.T) {
	pc := newPathCounter(t)
	pc.setStatus("/broken", http.StatusInternalServerError)
	scenarios := `
http: {base_url: ` + pc.URL + `}
scenarios:
  healthy:
    load: {requests: 20, workers: 2}
    http: {targets: [{name: ok-step, method: GET, url: /fine}]}
  failing:
    load: {requests: 20, workers: 2}
    http: {targets: [{name: bad-step, method: GET, url: /broken}]}
`
	t.Run("passes when only the healthy scenario is asserted", func(t *testing.T) {
		r := runScenarios(t, scenarios+`
assertions:
  - {scenario: healthy, metric: success_rate, min: "1"}
  - {scenario: healthy, step: ok-step, metric: success_rate, is: "1"}
`)
		if r.err != nil {
			t.Fatalf("the healthy scenario is 100%% but: %v\n%s", r.err, r.stderr)
		}
	})

	t.Run("fails, naming the scenario, when the broken one is asserted", func(t *testing.T) {
		r := runScenarios(t, scenarios+`
assertions:
  - {scenario: healthy, metric: success_rate, min: "1"}
  - {scenario: failing, metric: success_rate, min: "0.9"}
`)
		if r.err == nil || ExitCode(r.err) != ExitAssertion {
			t.Fatalf("err = %v (exit %d), want an assertion failure (exit %d)", r.err, ExitCode(r.err), ExitAssertion)
		}
		if !strings.Contains(r.stderr, "success_rate{scenario=failing}") || strings.Contains(r.stderr, "success_rate{scenario=healthy}") {
			t.Errorf("stderr should blame only the failing scenario:\n%s", r.stderr)
		}
		if !strings.Contains(r.stderr, "1 of 2 assertion(s) failed") {
			t.Errorf("stderr = %s", r.stderr)
		}
	})

	t.Run("a run-wide assertion sees both scenarios combined", func(t *testing.T) {
		// 20 of 40 requests succeed: below 0.9, above 0.4.
		if r := runScenarios(t, scenarios+"assertions:\n  - {metric: success_rate, min: \"0.4\"}\n"); r.err != nil {
			t.Errorf("combined success is 50%%, min 0.4 should pass: %v", r.err)
		}
		r := runScenarios(t, scenarios+"assertions:\n  - {metric: success_rate, min: \"0.9\"}\n")
		if ExitCode(r.err) != ExitAssertion {
			t.Errorf("combined success is 50%%, min 0.9 should fail with exit %d, got %v", ExitAssertion, r.err)
		}
	})

	t.Run("a step is found in the combined run too", func(t *testing.T) {
		r := runScenarios(t, scenarios+"assertions:\n  - {step: bad-step, metric: success_rate, max: \"0.1\"}\n")
		if r.err != nil {
			t.Errorf("bad-step has 0%% success, max 0.1 should pass: %v", r.err)
		}
	})
}

// A same-named assertion for two scenarios must be judged separately, in the
// console output and in the HTML report.
func TestRunScenariosReportsPassAndFailPerScopedAssertion(t *testing.T) {
	pc := newPathCounter(t)
	pc.setStatus("/broken", http.StatusInternalServerError)
	r := runScenarios(t, `
http: {base_url: `+pc.URL+`}
scenarios:
  healthy:
    load: {requests: 10, workers: 1}
    http: {targets: [{method: GET, url: /fine}]}
  failing:
    load: {requests: 10, workers: 1}
    http: {targets: [{method: GET, url: /broken}]}
assertions:
  - {scenario: healthy, metric: success_rate, min: "0.9"}
  - {scenario: failing, metric: success_rate, min: "0.9"}
`)
	if ExitCode(r.err) != ExitAssertion {
		t.Fatalf("err = %v", r.err)
	}
	html, _ := os.ReadFile(filepath.Join(r.dir, "report.html"))
	h := string(html)
	if strings.Count(h, `badge pass">PASS`) != 1 || strings.Count(h, `badge fail">FAIL`) != 1 {
		t.Errorf("HTML should show one PASS (healthy) and one FAIL (failing); got %d PASS, %d FAIL",
			strings.Count(h, `badge pass">PASS`), strings.Count(h, `badge fail">FAIL`))
	}
}

func TestRunScenariosRejectsBadFilesBeforeSendingAnything(t *testing.T) {
	pc := newPathCounter(t)
	ok := "    load: {requests: 5, workers: 1}\n    http: {targets: [{method: GET, url: /x}]}\n"
	head := "http: {base_url: " + pc.URL + "}\n"
	tests := []struct {
		name     string
		yaml     string
		wantCode int
		want     string
	}{
		{"unknown key in a scenario", head + "scenarios:\n  a:\n    start_afer: 1s\n" + ok, ExitUsage, `unknown key "start_afer"`},
		{"typo in the shared section", "http: {base_urll: x}\nscenarios:\n  a:\n" + ok, ExitUsage, "base_urll"},
		{"typo in an assertion", head + "scenarios:\n  a:\n" + ok + "assertions:\n  - {metric: rate, mn: \"1\"}\n", ExitUsage, "mn"},
		{"top-level load", head + "load: {requests: 1}\nscenarios:\n  a:\n" + ok, ExitUsage, "top-level load"},
		{"duplicate scenario", head + "scenarios:\n  a:\n" + ok + "  a:\n" + ok, ExitUsage, "defined twice"},
		{"assertion names a missing scenario", head + "scenarios:\n  a:\n" + ok + "assertions:\n  - {scenario: typo, metric: rate, min: \"1\"}\n", ExitError, `unknown scenario "typo"`},
		{"assertion names a missing step", head + "scenarios:\n  a:\n" + ok + "assertions:\n  - {scenario: a, step: nope, metric: rate, min: \"1\"}\n", ExitError, `unknown step "nope"`},
		{"step that exists only in another scenario", head + "scenarios:\n  a:\n    load: {requests: 5, workers: 1}\n    http: {targets: [{name: step-a, method: GET, url: /x}]}\n  b:\n    load: {requests: 5, workers: 1}\n    http: {targets: [{name: step-b, method: GET, url: /y}]}\nassertions:\n  - {scenario: a, step: step-b, metric: rate, min: \"1\"}\n", ExitError, `unknown step "step-b"`},
		{"abort without a scenario", head + "scenarios:\n  a:\n" + ok + "assertions:\n  - {metric: success_rate, min: \"0.9\", abort: true}\n", ExitError, "abort needs a scenario"},
		{"scenario without a load", head + "scenarios:\n  a:\n    http: {targets: [{method: GET, url: /x}]}\n", ExitError, "scenario \"a\""},
		{"scenario with no requests", head + "scenarios:\n  a:\n    load: {requests: 1}\n", ExitError, `scenario "a"`},
		{"second scenario invalid", head + "scenarios:\n  a:\n" + ok + "  b:\n    load: {requests: 1}\n    http: {targets: [{method: GET, url: /x}], flow: [{method: GET, url: /y}]}\n", ExitError, `scenario "b"`},
		{"unreachable relative url", "scenarios:\n  a:\n" + ok, ExitError, `scenario "a"`},
		{"invalid load", head + "scenarios:\n  a:\n    load: {requests: 1, workers: -2}\n    http: {targets: [{method: GET, url: /x}]}\n", ExitError, `scenario "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runScenarios(t, tt.yaml)
			if r.err == nil {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if !strings.Contains(r.err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", r.err, tt.want)
			}
			if got := ExitCode(r.err); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d (error: %v)", got, tt.wantCode, r.err)
			}
			if pc.total() != 0 {
				t.Errorf("the file was invalid but %d request(s) were sent", pc.total())
			}
		})
	}
}

func TestRunSingleScenarioFileRejectsAScenarioScopedAssertion(t *testing.T) {
	pc := newPathCounter(t)
	r := runScenarios(t, "load: {requests: 3, workers: 1}\nhttp: {targets: [{method: GET, url: "+pc.URL+"/x}]}\nassertions:\n  - {scenario: a, metric: rate, min: \"1\"}\n")
	if r.err == nil || !strings.Contains(r.err.Error(), "only apply to a file with scenarios:") {
		t.Errorf("err = %v", r.err)
	}
	if pc.total() != 0 {
		t.Errorf("sent %d requests despite the invalid file", pc.total())
	}
}

// An abort rule on one scenario stops the whole run, quickly, and the exit
// code and message say which scenario tripped it.
func TestRunScenariosAbortInOneScenarioStopsTheRun(t *testing.T) {
	pc := newPathCounter(t)
	pc.setStatus("/broken", http.StatusInternalServerError)
	start := time.Now()
	r := runScenarios(t, `
http: {base_url: `+pc.URL+`}
scenarios:
  steady:
    load: {duration: 1m, workers: 2}
    http: {targets: [{method: GET, url: /fine}]}
  breaking:
    load: {duration: 1m, workers: 2}
    http: {targets: [{method: GET, url: /broken}]}
assertions:
  - {scenario: breaking, metric: success_rate, min: "0.9", abort: true, abort_after: 0s}
`)
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("took %s of a 1m run; the abort should have stopped everything", took)
	}
	if ExitCode(r.err) != ExitAssertion || !strings.Contains(r.err.Error(), `scenario "breaking"`) {
		t.Fatalf("err = %v (exit %d), want an abort naming the scenario", r.err, ExitCode(r.err))
	}
	s := r.summary(t)
	if !strings.Contains(s.Aborted, `scenario "breaking"`) {
		t.Errorf("report aborted = %q", s.Aborted)
	}
	if scenario(t, s, "steady").Requests == 0 {
		t.Error("the steady scenario ran until the abort; its results should be in the report")
	}
}

func TestRunScenariosDryRunSendsOneIterationOfEach(t *testing.T) {
	pc := newPathCounter(t)
	yaml := `
http: {base_url: ` + pc.URL + `}
scenarios:
  web:
    load: {duration: 1m, workers: 50}
    http: {targets: [{method: GET, url: /web}]}
  flow:
    load: {duration: 1m, workers: 50}
    http:
      flow:
        - {method: GET, url: /one}
        - {method: GET, url: /two}
`
	r := runScenarios(t, yaml, "--dry-run")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if pc.count("/web") != 1 || pc.count("/one") != 1 || pc.count("/two") != 1 || pc.total() != 3 {
		t.Errorf("dry run sent web=%d one=%d two=%d (total %d), want exactly one iteration of each", pc.count("/web"), pc.count("/one"), pc.count("/two"), pc.total())
	}
	if !strings.Contains(r.stdout, "Scenario: web") || !strings.Contains(r.stdout, "Scenario: flow") {
		t.Errorf("dry-run output should be grouped by scenario:\n%s", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "report.json")); err == nil {
		t.Error("a dry run must not write a report")
	}

	r = runScenarios(t, yaml, "--dry-run", "--json")
	if r.err != nil {
		t.Fatal(r.err)
	}
	var byScenario map[string][]map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &byScenario); err != nil {
		t.Fatalf("--dry-run --json output is not a scenario→results object: %v\n%s", err, r.stdout)
	}
	if len(byScenario["web"]) != 1 || len(byScenario["flow"]) != 2 {
		t.Errorf("dry-run JSON = %v, want web:1 flow:2 results", byScenario)
	}
}

func TestRunScenariosJSONStdoutCarriesScenarios(t *testing.T) {
	pc := newPathCounter(t)
	r := runScenarios(t, `
http: {base_url: `+pc.URL+`}
scenarios:
  a:
    load: {requests: 4, workers: 1}
    http: {targets: [{method: GET, url: /a}]}
`, "--json")
	if r.err != nil {
		t.Fatal(r.err)
	}
	var s report.Summary
	if err := json.Unmarshal([]byte(r.stdout), &s); err != nil {
		t.Fatalf("stdout is not a JSON report: %v\n%s", err, r.stdout)
	}
	if len(s.Scenarios) != 1 || s.Scenarios[0].Name != "a" || s.Scenarios[0].Requests != 4 {
		t.Errorf("scenarios = %+v", s.Scenarios)
	}
}

// One file can mix protocols; here HTTP next to an SSE stream, which also
// proves a scenario can inherit nothing and bring its own protocol section.
func TestRunScenariosMixesProtocols(t *testing.T) {
	pc := newPathCounter(t)
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			w.Write([]byte("data: tick\n\n"))
			fl.Flush()
		}
	}))
	t.Cleanup(sse.Close)

	r := runScenarios(t, `
protocol: http
http: {base_url: `+pc.URL+`}
scenarios:
  api:
    load: {requests: 5, workers: 1}
    http: {targets: [{method: GET, url: /api}]}
  stream:
    protocol: sse
    load: {requests: 2, workers: 1}
    sse:
      url: `+sse.URL+`
      events: 3
`)
	if r.err != nil {
		t.Fatalf("run failed: %v\n%s", r.err, r.stderr)
	}
	s := r.summary(t)
	if api := scenario(t, s, "api"); api.Requests != 5 {
		t.Errorf("api requests = %d", api.Requests)
	}
	// An SSE iteration is a connect result plus one per event: 2 × (1 + 3).
	if st := scenario(t, s, "stream"); st.Requests != 8 {
		t.Errorf("stream requests = %d, want 8", st.Requests)
	}
	// The SSE scenario has no step names, so a step assertion on it is refused.
	r = runScenarios(t, `
protocol: http
http: {base_url: `+pc.URL+`}
scenarios:
  stream:
    protocol: sse
    load: {requests: 1, workers: 1}
    sse: {url: `+sse.URL+`, events: 3}
assertions:
  - {scenario: stream, step: x, metric: rate, min: "1"}
`)
	if r.err == nil || !strings.Contains(r.err.Error(), "only apply to http scenarios") {
		t.Errorf("err = %v, want a step assertion on SSE refused", r.err)
	}
}

// The existing contract for single-scenario files must be untouched: the same
// file, run now, gives the same report shape and exit code as before.
func TestRunSingleScenarioFileIsUnaffectedByMultiScenarioSupport(t *testing.T) {
	pc := newPathCounter(t)
	r := runScenarios(t, "load: {requests: 12, workers: 3}\nhttp: {targets: [{method: GET, url: "+pc.URL+"/x}]}\nassertions:\n  - {metric: success_rate, min: \"1\"}\n")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if pc.count("/x") != 12 {
		t.Errorf("hits = %d, want 12", pc.count("/x"))
	}
	if strings.Contains(r.stdout, "Scenario") {
		t.Errorf("single-scenario stdout mentions scenarios:\n%s", r.stdout)
	}
	// And a lenient file stays lenient: an unknown top-level key is ignored.
	r = runScenarios(t, "mystery_key: 1\nload: {requests: 2, workers: 1}\nhttp: {targets: [{method: GET, url: "+pc.URL+"/x}]}\n")
	if r.err != nil {
		t.Errorf("a single-scenario file with an unknown key used to load and must still: %v", r.err)
	}
}

// The CLI builds every scenario's generator, so it must close them all when
// the run is over — including when an assertion fails and the command errors.
func TestRunScenariosClosesEveryGenerator(t *testing.T) {
	var mu sync.Mutex
	open := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch st {
		case http.StateNew:
			open++
		case http.StateClosed, http.StateHijacked:
			open--
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	for name, assertions := range map[string]string{
		"passing":           "",
		"failing assertion": "assertions:\n  - {scenario: a, metric: requests, is: \"-1\"}\n",
	} {
		r := runScenarios(t, `
http: {base_url: `+srv.URL+`}
scenarios:
  a:
    load: {requests: 20, workers: 2}
    http: {targets: [{method: GET, url: /a}]}
  b:
    load: {requests: 20, workers: 2}
    http: {targets: [{method: GET, url: /b}]}
`+assertions)
		if (name == "failing assertion") != (r.err != nil) {
			t.Fatalf("%s: err = %v", name, r.err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			n := open
			mu.Unlock()
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d connection(s) still open after the command returned", name, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

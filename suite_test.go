package resonate_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jecklgamis/resonate"
)

// Like resonate_test.go, this file imports only the public package: it is the
// contract test for running several scenarios at once.

// countingServer counts requests per path; paths listed in failing answer 500.
type countingServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newCountingServer(t *testing.T, failing ...string) *countingServer {
	t.Helper()
	cs := &countingServer{hits: map[string]int{}}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.hits[r.URL.Path]++
		cs.mu.Unlock()
		for _, p := range failing {
			if r.URL.Path == p {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *countingServer) count(path string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.hits[path]
}

func (cs *countingServer) total() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	for _, c := range cs.hits {
		n += c
	}
	return n
}

func ptr(v float64) *float64 { return &v }

func TestSuiteRunsScenariosTogetherWithOwnProfiles(t *testing.T) {
	cs := newCountingServer(t)
	browse := resonate.NewScenario().Get(cs.URL + "/products").Name("products").Workers(2).Requests(30)
	checkout := resonate.NewScenario().
		Step("GET", cs.URL+"/cart").Name("add").
		Step("GET", cs.URL+"/pay").Name("pay").
		Rate(500).Workers(1).MaxWorkers(4).Requests(10)

	s, err := resonate.NewSuite().
		Add("browse", browse).
		AddAfter("checkout", 200*time.Millisecond, checkout).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if cs.count("/products") != 30 || cs.count("/cart") != 10 || cs.count("/pay") != 10 {
		t.Errorf("server saw products=%d cart=%d pay=%d, want 30/10/10", cs.count("/products"), cs.count("/cart"), cs.count("/pay"))
	}
	if s.Requests != 50 {
		t.Errorf("combined requests = %d, want 50", s.Requests)
	}
	if len(s.Scenarios) != 2 || s.Scenarios[0].Name != "browse" || s.Scenarios[1].Name != "checkout" {
		t.Fatalf("scenarios = %+v, want browse then checkout", s.Scenarios)
	}
	if s.Scenarios[0].Requests != 30 || s.Scenarios[1].Requests != 20 {
		t.Errorf("per-scenario requests = %d / %d, want 30 / 20", s.Scenarios[0].Requests, s.Scenarios[1].Requests)
	}
	if s.Scenarios[1].StartAfter != 200*time.Millisecond {
		t.Errorf("checkout StartAfter = %s", s.Scenarios[1].StartAfter)
	}
	if len(s.Steps) != 3 {
		t.Errorf("combined steps = %d, want products/add/pay", len(s.Steps))
	}
}

func TestSuiteScenarioAssertionsAreScopedToTheirScenario(t *testing.T) {
	cs := newCountingServer(t, "/broken")
	healthy := resonate.NewScenario().Get(cs.URL + "/fine").Workers(1).Requests(10).
		Assert(resonate.Assertion{Metric: "success_rate", Min: ptr(0.99)})
	broken := resonate.NewScenario().Get(cs.URL + "/broken").Workers(1).Requests(10).
		Assert(resonate.Assertion{Metric: "success_rate", Min: ptr(0.99)})

	// Both scenarios assert the same thing; only the broken one can fail it.
	// Were the assertions run-wide, the combined 50% would fail "healthy" too.
	s, err := resonate.NewSuite().Add("healthy", healthy).Add("broken", broken).Run(context.Background())
	if err == nil {
		t.Fatal("the broken scenario's own assertion should fail the run")
	}
	if !strings.Contains(err.Error(), "1 of 2 assertion(s) failed") || !strings.Contains(err.Error(), "success_rate{scenario=broken}") {
		t.Errorf("err = %v, want exactly the broken scenario's assertion", err)
	}
	if strings.Contains(err.Error(), "scenario=healthy") {
		t.Errorf("err = %v blames the healthy scenario", err)
	}
	if s.Requests != 20 {
		t.Errorf("the Summary should still be returned alongside the error: requests=%d", s.Requests)
	}
}

func TestSuiteRunWideAndScopedAssertions(t *testing.T) {
	cs := newCountingServer(t, "/broken")
	mk := func() *resonate.Suite {
		return resonate.NewSuite().
			Add("healthy", resonate.NewScenario().Get(cs.URL+"/fine").Name("fine-step").Workers(1).Requests(10)).
			Add("broken", resonate.NewScenario().Get(cs.URL+"/broken").Name("broken-step").Workers(1).Requests(10))
	}

	if _, err := mk().Assert(resonate.Assertion{Metric: "success_rate", Min: ptr(0.4)}).Run(context.Background()); err != nil {
		t.Errorf("combined success is 50%%; min 0.4 should hold: %v", err)
	}
	if _, err := mk().Assert(resonate.Assertion{Metric: "success_rate", Min: ptr(0.9)}).Run(context.Background()); err == nil {
		t.Error("combined success is 50%; min 0.9 should fail")
	}
	// A suite-level assertion may be scoped to a scenario, and to a step in it.
	if _, err := mk().Assert(
		resonate.Assertion{Metric: "success_rate", Scenario: "healthy", Min: ptr(1)},
		resonate.Assertion{Metric: "success_rate", Scenario: "broken", Step: "broken-step", Max: ptr(0)},
	).Run(context.Background()); err != nil {
		t.Errorf("scoped assertions should hold: %v", err)
	}
	// A run-wide Step may come from any scenario.
	if _, err := mk().Assert(resonate.Assertion{Metric: "success_rate", Step: "fine-step", Min: ptr(1)}).Run(context.Background()); err != nil {
		t.Errorf("run-wide step assertion: %v", err)
	}
}

func TestSuiteAbortInOneScenarioStopsTheRun(t *testing.T) {
	cs := newCountingServer(t, "/broken")
	start := time.Now()
	s, err := resonate.NewSuite().
		Add("steady", resonate.NewScenario().Get(cs.URL+"/fine").Workers(2).Duration(time.Minute)).
		Add("breaking", resonate.NewScenario().Get(cs.URL+"/broken").Workers(2).Duration(time.Minute)).
		Assert(resonate.Assertion{Metric: "success_rate", Scenario: "breaking", Min: ptr(0.9), Abort: true}).
		Run(context.Background())
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("took %s of a 1m run; the abort should stop every scenario", took)
	}
	if err == nil || !strings.Contains(err.Error(), "aborted") || !strings.Contains(err.Error(), `scenario "breaking"`) {
		t.Fatalf("err = %v, want an abort naming the scenario", err)
	}
	if s.Aborted == "" || s.Scenarios[0].Requests == 0 {
		t.Errorf("Summary should carry the abort and the steady scenario's results: aborted=%q steady=%d", s.Aborted, s.Scenarios[0].Requests)
	}
}

func TestSuiteScenarioOwnAbortAssertionStopsTheRun(t *testing.T) {
	cs := newCountingServer(t, "/broken")
	start := time.Now()
	_, err := resonate.NewSuite().
		Add("steady", resonate.NewScenario().Get(cs.URL+"/fine").Workers(2).Duration(time.Minute)).
		Add("breaking", resonate.NewScenario().Get(cs.URL+"/broken").Workers(2).Duration(time.Minute).
			Assert(resonate.Assertion{Metric: "success_rate", Min: ptr(0.9), Abort: true})).
		Run(context.Background())
	if time.Since(start) > 30*time.Second || err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("a Scenario's own abort assertion should stop the suite quickly; took %s, err=%v", time.Since(start), err)
	}
}

func TestSuiteRejectsMistakesBeforeSendingAnything(t *testing.T) {
	cs := newCountingServer(t)
	ok := func() *resonate.Scenario { return resonate.NewScenario().Get(cs.URL + "/x").Workers(1).Requests(5) }
	tests := []struct {
		name  string
		suite *resonate.Suite
		want  string
	}{
		{"empty", resonate.NewSuite(), "no scenarios"},
		{"nil scenario", resonate.NewSuite().Add("a", nil), `scenario "a" is nil`},
		{"duplicate name", resonate.NewSuite().Add("a", ok()).Add("a", ok()), `duplicate name`},
		{"empty name", resonate.NewSuite().Add("", ok()), "name is required"},
		{"negative start", resonate.NewSuite().AddAfter("a", -time.Second, ok()), "must not be negative"},
		{"build error names the scenario", resonate.NewSuite().Add("good", ok()).Add("bad", resonate.NewScenario()), `scenario "bad"`},
		{"invalid options name the scenario", resonate.NewSuite().Add("a", ok()).Add("b", resonate.NewScenario().Get(cs.URL+"/x").Workers(-1).Requests(1)), `scenario "b"`},
		{"assertion on an unknown scenario", resonate.NewSuite().Add("a", ok()).
			Assert(resonate.Assertion{Metric: "rate", Scenario: "typo", Min: ptr(1)}), `unknown scenario "typo"`},
		{"assertion on a step the scenario lacks", resonate.NewSuite().Add("a", ok()).
			Assert(resonate.Assertion{Metric: "rate", Scenario: "a", Step: "nope", Min: ptr(1)}), `unknown step "nope"`},
		{"run-wide abort", resonate.NewSuite().Add("a", ok()).
			Assert(resonate.Assertion{Metric: "success_rate", Min: ptr(0.9), Abort: true}), "Abort needs a Scenario"},
		{"abort on an unknown scenario", resonate.NewSuite().Add("a", ok()).
			Assert(resonate.Assertion{Metric: "success_rate", Scenario: "typo", Min: ptr(0.9), Abort: true}), "Abort needs a Scenario"},
		{"abort on a count", resonate.NewSuite().Add("a", ok()).
			Assert(resonate.Assertion{Metric: "requests", Scenario: "a", Min: ptr(1), Abort: true}), `scenario "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.suite.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
			if cs.total() != 0 {
				t.Errorf("sent %d request(s) for an invalid suite", cs.total())
			}
		})
	}
}

// RunMulti is the lower-level entry point Suite is built on; callers who
// already hold Generators can use it directly.
func TestRunMultiViaFacade(t *testing.T) {
	cs := newCountingServer(t)
	mk := func(path string) resonate.Generator {
		g, err := resonate.NewHTTPGenerator([]resonate.HTTPTarget{{URL: cs.URL + path}}, resonate.DefaultHTTPOptions())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { g.Close() })
		return g
	}
	specs := []resonate.ScenarioSpec{
		{Name: "a", Generator: mk("/a"), Options: resonate.Options{Requests: 12, Workers: 2}},
		{Name: "b", Generator: mk("/b"), Options: resonate.Options{Requests: 8, Workers: 1}, StartAfter: 50 * time.Millisecond},
	}
	if err := resonate.ValidateMulti(specs); err != nil {
		t.Fatal(err)
	}
	var seen sync.Map
	s, err := resonate.RunMulti(context.Background(), specs, resonate.MultiOptions{
		OnResult: func(scenario string, _ resonate.Result) { n, _ := seen.LoadOrStore(scenario, new(int)); *(n.(*int))++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cs.count("/a") != 12 || cs.count("/b") != 8 || s.Requests != 20 {
		t.Errorf("a=%d b=%d combined=%d, want 12/8/20", cs.count("/a"), cs.count("/b"), s.Requests)
	}
	if a, _ := seen.Load("a"); a == nil || *(a.(*int)) != 12 {
		t.Errorf("OnResult saw a=%v", a)
	}
	failures, err := s.Evaluate([]resonate.Assertion{{Metric: "requests", Scenario: "b", Is: ptr(8)}, {Metric: "requests", Scenario: "a", Is: ptr(8)}})
	if err != nil || len(failures) != 1 || failures[0].Scenario != "a" {
		t.Errorf("failures = %+v err=%v, want only scenario a's (12 ≠ 8)", failures, err)
	}
	if err := resonate.ValidateMulti(nil); err == nil {
		t.Error("ValidateMulti(nil) should fail")
	}
}

// A Suite owns the generators it builds, so when Run returns their pooled
// keep-alive connections must be gone — otherwise every Run leaks sockets.
func TestSuiteClosesItsGenerators(t *testing.T) {
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

	for name, a := range map[string][]resonate.Assertion{
		"passing": nil,
		"failing": {{Metric: "requests", Scenario: "a", Is: ptr(-1)}}, // the run completes, the assertion fails
	} {
		_, _ = resonate.NewSuite().
			Add("a", resonate.NewScenario().Get(srv.URL).Workers(2).Requests(20)).
			Add("b", resonate.NewScenario().Get(srv.URL).Workers(2).Requests(20)).
			Assert(a...).
			Run(context.Background())

		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			n := open
			mu.Unlock()
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s run: %d connection(s) still open after Run returned; the suite did not close its generators", name, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestSuiteDefaultsApplyToEveryAddWithScenario(t *testing.T) {
	cs := newCountingServer(t)
	s, err := resonate.NewSuite().
		Defaults(func(s *resonate.Scenario) *resonate.Scenario {
			// BaseURL resolves the relative URLs below; Requests/Workers are shared load.
			return s.BaseURL(cs.URL).Workers(1).Requests(6)
		}).
		AddWith("a", func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/a") }).
		AddAfterWith("b", 50*time.Millisecond, func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/b") }).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cs.count("/a") != 6 || cs.count("/b") != 6 || s.Requests != 12 {
		t.Errorf("a=%d b=%d combined=%d, want 6/6/12: the defaults did not reach both scenarios", cs.count("/a"), cs.count("/b"), s.Requests)
	}
	if s.Scenarios[1].StartAfter != 50*time.Millisecond {
		t.Errorf("AddAfterWith start = %s", s.Scenarios[1].StartAfter)
	}
}

func TestSuiteScenarioOwnSettingBeatsTheDefault(t *testing.T) {
	shared, other := newCountingServer(t), newCountingServer(t)
	_, err := resonate.NewSuite().
		Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.BaseURL(shared.URL).Workers(1).Requests(4) }).
		AddWith("inherits", func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/x") }).
		AddWith("overrides", func(s *resonate.Scenario) *resonate.Scenario { return s.BaseURL(other.URL).Get("/x").Requests(9) }).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if shared.count("/x") != 4 || other.count("/x") != 9 {
		t.Errorf("shared saw %d, other saw %d; want 4 and 9 (the scenario's own BaseURL and Requests win)", shared.count("/x"), other.count("/x"))
	}
}

func TestSuiteDefaultsApplyInOrderEvenWhenCalledAfterAddWith(t *testing.T) {
	cs := newCountingServer(t)
	_, err := resonate.NewSuite().
		AddWith("a", func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/a") }). // before Defaults on purpose
		Defaults(func(s *resonate.Scenario) *resonate.Scenario {
			return s.BaseURL("http://wrong.invalid").Workers(1).Requests(2)
		}).
		Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.BaseURL(cs.URL).Requests(5) }). // later default wins
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cs.count("/a") != 5 {
		t.Errorf("hits = %d, want 5 (the later default's BaseURL and Requests)", cs.count("/a"))
	}
}

// A default Assert applies to each scenario on its own, like one written in each.
func TestSuiteDefaultAssertionIsPerScenario(t *testing.T) {
	cs := newCountingServer(t, "/broken")
	_, err := resonate.NewSuite().
		Defaults(func(s *resonate.Scenario) *resonate.Scenario {
			return s.BaseURL(cs.URL).Workers(1).Requests(10).Assert(resonate.Assertion{Metric: "success_rate", Min: ptr(0.99)})
		}).
		AddWith("healthy", func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/fine") }).
		AddWith("broken", func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/broken") }).
		Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 of 2 assertion(s) failed") || !strings.Contains(err.Error(), "success_rate{scenario=broken}") {
		t.Errorf("err = %v, want only the broken scenario to fail its copy of the default assertion", err)
	}
}

// Defaults are for what scenarios share; they do not reach a ready-made
// Scenario given to Add.
func TestSuiteDefaultsDoNotTouchAddedScenarios(t *testing.T) {
	cs := newCountingServer(t)
	_, err := resonate.NewSuite().
		Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.BaseURL(cs.URL) }).
		Add("prebuilt", resonate.NewScenario().Get("/relative").Workers(1).Requests(1)).
		Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), `scenario "prebuilt"`) {
		t.Errorf("err = %v: a prebuilt scenario with a relative URL must not have been given the default BaseURL", err)
	}
	if cs.total() != 0 {
		t.Errorf("sent %d request(s)", cs.total())
	}
}

func TestSuiteMaterializesScenariosFreshOnEveryRun(t *testing.T) {
	cs := newCountingServer(t)
	suite := resonate.NewSuite().
		Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.BaseURL(cs.URL).Workers(1).Requests(3) }).
		AddWith("a", func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/a") })
	for i := 1; i <= 2; i++ {
		if _, err := suite.Run(context.Background()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if got := cs.count("/a"); got != 3*i {
			t.Fatalf("after run %d the server saw %d, want %d: a second Run must behave like the first", i, got, 3*i)
		}
	}
}

func TestSuiteRejectsBadDefaultsAndBuildFunctions(t *testing.T) {
	cs := newCountingServer(t)
	get := func(s *resonate.Scenario) *resonate.Scenario { return s.Get(cs.URL + "/x").Workers(1).Requests(1) }
	keep := func(s *resonate.Scenario) *resonate.Scenario { return s }
	// bounded ends after one request, so a missing check fails the test instead of running forever.
	bounded := func(s *resonate.Scenario) *resonate.Scenario { return s.Workers(1).Requests(1) }
	tests := []struct {
		name  string
		suite *resonate.Suite
		want  string
	}{
		{"default starts a request", resonate.NewSuite().Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.Get(cs.URL + "/d") }).AddWith("a", bounded), "starts a request"},
		{"default sets a header", resonate.NewSuite().Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.Header("X", "y") }).AddWith("a", get), "starts a request"},
		{"default starts a flow step", resonate.NewSuite().Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.Step("GET", cs.URL) }).AddWith("a", get), "starts a request"},
		{"default returns nil", resonate.NewSuite().Defaults(func(s *resonate.Scenario) *resonate.Scenario { return nil }).AddWith("a", get), "returned nil"},
		{"nil default", resonate.NewSuite().Defaults(nil).AddWith("a", get), "default 0 is nil"},
		{"build returns nil", resonate.NewSuite().AddWith("a", func(*resonate.Scenario) *resonate.Scenario { return nil }), "build function returned nil"},
		{"nil build function", resonate.NewSuite().AddWith("a", nil), "is nil"},
		{"build leaves no request", resonate.NewSuite().Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.BaseURL(cs.URL) }).AddWith("a", keep), `scenario "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.suite.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), `"a"`) {
				t.Errorf("err = %v should name the scenario", err)
			}
			if cs.total() != 0 {
				t.Errorf("sent %d request(s) for an invalid suite", cs.total())
			}
		})
	}
}

// Add and AddWith can be mixed in one suite, in the order given.
func TestSuiteMixesAddAndAddWith(t *testing.T) {
	cs := newCountingServer(t)
	s, err := resonate.NewSuite().
		Defaults(func(s *resonate.Scenario) *resonate.Scenario { return s.BaseURL(cs.URL).Workers(1).Requests(3) }).
		Add("plain", resonate.NewScenario().Get(cs.URL+"/plain").Workers(1).Requests(2)).
		AddWith("shared", func(s *resonate.Scenario) *resonate.Scenario { return s.Get("/shared") }).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cs.count("/plain") != 2 || cs.count("/shared") != 3 {
		t.Errorf("plain=%d shared=%d, want 2 and 3", cs.count("/plain"), cs.count("/shared"))
	}
	if s.Scenarios[0].Name != "plain" || s.Scenarios[1].Name != "shared" {
		t.Errorf("order = %s, %s", s.Scenarios[0].Name, s.Scenarios[1].Name)
	}
}

// Single-valued settings are replaced by the scenario's own, but Identity adds
// to a pool, so a scenario's identities are added to the defaults' — the
// documented difference from a scenario file, where a list is replaced.
func TestSuiteIdentityAddsToTheDefaultsPool(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("X-User")] = true
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	_, err := resonate.NewSuite().
		Defaults(func(s *resonate.Scenario) *resonate.Scenario {
			return s.Identity(map[string]string{"user": "from-default"})
		}).
		AddWith("a", func(s *resonate.Scenario) *resonate.Scenario {
			return s.Identity(map[string]string{"user": "from-scenario"}).
				Step("GET", srv.URL).Header("X-User", "{{.Identity.user}}").
				Workers(2).Requests(20)
		}).
		Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen["from-default"] || !seen["from-scenario"] {
		t.Errorf("identities used = %v, want both the default's and the scenario's (the pool is added to)", seen)
	}
}

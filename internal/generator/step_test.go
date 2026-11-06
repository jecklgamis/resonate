package generator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func okServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPGeneratorLabelsResultsWithStepName(t *testing.T) {
	srv := okServer(t)
	g, err := NewHTTPGenerator([]HTTPTarget{
		{Name: "home", URL: srv.URL + "/"},
		{URL: srv.URL + "/search?q=secret&token=abc#frag"},
	}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	var got []string
	for i := 0; i < 2; i++ {
		for _, r := range g.Do(context.Background(), 0) {
			got = append(got, r.Step)
		}
	}
	want := []string{"home", "GET " + srv.URL + "/search"}
	if !slices.Equal(got, want) {
		t.Errorf("steps = %q, want %q (an unnamed target is labelled METHOD url with the query and fragment stripped)", got, want)
	}
	if names := g.StepNames(); !slices.Equal(names, want) {
		t.Errorf("StepNames() = %q, want %q", names, want)
	}
}

func TestStepNameNeverCarriesCredentialsOrQuery(t *testing.T) {
	f := httpFields{method: "POST", rawURL: "https://user:hunter2@api.example.com/v1/login?key=abc"}
	got := stepName("", f)
	if got != "POST https://api.example.com/v1/login" {
		t.Errorf("stepName = %q", got)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "abc") {
		t.Errorf("stepName leaked a secret: %q", got)
	}
}

func TestStepNameKeepsTemplateSource(t *testing.T) {
	f := httpFields{method: "GET", rawURL: "http://h/users/{{.Vars.id}}"}
	if got := stepName("", f); got != "GET http://h/users/{{.Vars.id}}" {
		t.Errorf("stepName = %q; the label must be the template, not a per-request value, or every request becomes its own step", got)
	}
}

func TestFlowGeneratorLabelsEachStepAndNestedSteps(t *testing.T) {
	srv := okServer(t)
	g, err := NewFlowGenerator([]FlowStep{
		{Name: "login", Method: "POST", URL: srv.URL + "/login"},
		{Repeat: 2, Steps: []FlowStep{{Name: "browse", URL: srv.URL + "/items"}}},
		{URL: srv.URL + "/logout"},
	}, nil, FlowOptions{HTTP: DefaultHTTPOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	var got []string
	for _, r := range g.Do(context.Background(), 0) {
		got = append(got, r.Step)
	}
	want := []string{"login", "browse", "browse", "GET " + srv.URL + "/logout"}
	if !slices.Equal(got, want) {
		t.Errorf("steps = %q, want %q", got, want)
	}
	wantNames := []string{"login", "browse", "GET " + srv.URL + "/logout"}
	if names := g.StepNames(); !slices.Equal(names, wantNames) {
		t.Errorf("StepNames() = %q, want %q (each name once, in flow order)", names, wantNames)
	}
}

func TestFlowSetupStepsAreNotListedAsSteps(t *testing.T) {
	srv := okServer(t)
	g, err := NewFlowGenerator(
		[]FlowStep{{Name: "work", URL: srv.URL + "/work"}},
		[]FlowStep{{Name: "auth", URL: srv.URL + "/auth"}},
		FlowOptions{HTTP: DefaultHTTPOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if names := g.StepNames(); !slices.Equal(names, []string{"work"}) {
		t.Errorf("StepNames() = %q; setup results are never reported, so asserting on one could never pass", names)
	}
	for _, r := range g.Do(context.Background(), 0) {
		if r.Step == "auth" {
			t.Error("a setup step's result was reported")
		}
	}
}

func TestFlowStepRejectsNameOnControlBlock(t *testing.T) {
	_, err := NewFlowGenerator([]FlowStep{
		{Name: "loop", Repeat: 2, Steps: []FlowStep{{URL: "http://localhost:1/"}}},
	}, nil, FlowOptions{HTTP: DefaultHTTPOptions()})
	if err == nil || !strings.Contains(err.Error(), "can't have a name") {
		t.Errorf("err = %v, want a rejection of a name on a repeat/during/if block", err)
	}
}

func TestStepLabelSetEvenWhenRequestFails(t *testing.T) {
	g, err := NewHTTPGenerator([]HTTPTarget{{Name: "dead", URL: "http://127.0.0.1:1/"}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Error == nil {
		t.Fatalf("expected one failed result, got %+v", res)
	}
	if res[0].Step != "dead" {
		t.Errorf("Step = %q on a transport error; failures must be attributed to their step too", res[0].Step)
	}
}

func TestStepLabelSetWhenTheRequestCannotBeBuilt(t *testing.T) {
	// A template that renders an unparseable URL fails in buildRequest, before
	// any network I/O — a separate path from a failed round trip.
	g, err := NewHTTPGenerator([]HTTPTarget{{Name: "broken", URL: `{{"http://a b/"}}`}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)
	if len(res) != 1 || res[0].Error == nil {
		t.Fatalf("expected one failed result, got %+v", res)
	}
	if res[0].Step != "broken" {
		t.Errorf("Step = %q on a request that failed to build", res[0].Step)
	}
}

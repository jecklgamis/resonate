package resonate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jecklgamis/resonate"
)

// This file exercises resonate's public facade the way an external
// consumer would: only the resonate package is imported, never
// resonate/internal/*. If a change to the internal packages ever breaks
// this file, it's a sign the public API surface moved underneath it.

func TestHTTPGeneratorRunEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewHTTPGenerator(
		[]resonate.HTTPTarget{{URL: srv.URL}},
		resonate.DefaultHTTPOptions(),
	)
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{
		Requests: 10,
		Workers:  2,
	})

	if summary.Requests != 10 {
		t.Errorf("Requests = %d, want 10", summary.Requests)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("SuccessRate = %v, want 1 (every request returned 200)", summary.SuccessRate)
	}
}

func TestFlowGeneratorChainsExtractedValues(t *testing.T) {
	var gotOrderPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orders" {
			w.Write([]byte(`{"id":"order-42"}`))
			return
		}
		gotOrderPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewFlowGenerator(
		[]resonate.FlowStep{
			{Method: "POST", URL: srv.URL + "/orders", Extract: map[string]string{"orderID": "json:id"}},
			{Method: "GET", URL: srv.URL + "/orders/{{.Vars.orderID}}"},
		},
		nil,
		resonate.FlowOptions{HTTP: resonate.DefaultHTTPOptions()},
	)
	if err != nil {
		t.Fatalf("NewFlowGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 2, Workers: 1})
	if summary.SuccessRate != 1 {
		t.Fatalf("SuccessRate = %v, want 1", summary.SuccessRate)
	}
	if gotOrderPath != "/orders/order-42" {
		t.Errorf("second step's path = %q, want /orders/order-42 (chained from the first step's response)", gotOrderPath)
	}
}

func TestAssertionEvaluate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewHTTPGenerator([]resonate.HTTPTarget{{URL: srv.URL}}, resonate.DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 5, Workers: 1})

	min := 0.99
	failures, err := summary.Evaluate([]resonate.Assertion{{Metric: "success_rate", Min: &min}})
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1 (every request 500s)", len(failures))
	}
}

func TestValidateRejectsNegativeRate(t *testing.T) {
	if err := resonate.Validate(resonate.Options{Rate: -1}); err == nil {
		t.Fatal("expected an error for a negative Rate")
	}
}

func TestFeederDrivesRequestData(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/users.csv"
	if err := writeFile(path, "email\nalice@example.com\n"); err != nil {
		t.Fatalf("writing feeder file: %v", err)
	}

	feeder, err := resonate.NewFeeder(path, "sequential")
	if err != nil {
		t.Fatalf("NewFeeder error: %v", err)
	}
	row := feeder.Next()
	if row["email"] != "alice@example.com" {
		t.Errorf("Feeder.Next()[\"email\"] = %q, want alice@example.com", row["email"])
	}
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o644)
}

func TestWriteHTMLFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	gen, err := resonate.NewHTTPGenerator([]resonate.HTTPTarget{{URL: srv.URL}}, resonate.DefaultHTTPOptions())
	if err != nil {
		t.Fatalf("NewHTTPGenerator error: %v", err)
	}
	defer gen.Close()

	summary := resonate.Run(context.Background(), gen, resonate.Options{Requests: 2, Workers: 1})

	path := t.TempDir() + "/report.html"
	if err := resonate.WriteHTMLFile(path, resonate.HTMLReport{Title: "test report", Summary: summary}); err != nil {
		t.Fatalf("WriteHTMLFile error: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written report: %v", err)
	}
	if !strings.HasPrefix(string(contents), "<!doctype html>") {
		t.Error("written file does not start with a doctype")
	}
	if !strings.Contains(string(contents), "test report") {
		t.Error("written file does not contain the given Title")
	}
}

func TestWriteHTMLFileInvalidPathErrors(t *testing.T) {
	if err := resonate.WriteHTMLFile("/nonexistent/dir/report.html", resonate.HTMLReport{}); err == nil {
		t.Fatal("expected an error for an unwritable path")
	}
}

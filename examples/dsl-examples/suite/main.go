// Command suite runs two workloads at the same time, each with its own load
// profile, using resonate.NewSuite(): the library counterpart of a scenario
// file's `scenarios:` section.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/jecklgamis/resonate"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pay" {
			time.Sleep(30 * time.Millisecond) // payments are the slow part
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p95 := 0.2 // seconds
	minSuccess := 0.99

	summary, err := resonate.NewSuite().
		// What every scenario shares: set up each one before its own settings.
		Defaults(func(s *resonate.Scenario) *resonate.Scenario {
			return s.BaseURL(srv.URL).Timeout(5 * time.Second).
				Assert(resonate.Assertion{Metric: "success_rate", Min: &minSuccess}) // checked per scenario
		}).
		// Closed model: 20 concurrent users, back to back.
		AddWith("browse", func(s *resonate.Scenario) *resonate.Scenario {
			return s.Get("/products").Name("products").Workers(20).Duration(3 * time.Second)
		}).
		// Open model: 40 checkouts a second, starting one second in.
		AddAfterWith("checkout", time.Second, func(s *resonate.Scenario) *resonate.Scenario {
			return s.Step("POST", "/cart").Name("add").
				Step("POST", "/pay").Name("pay").
				Rate(40).Workers(10).MaxWorkers(100).Duration(2 * time.Second).
				Assert(resonate.Assertion{Metric: "latency_p95", Step: "pay", Max: &p95})
		}).
		Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	fmt.Printf("%-10s %8s %10s %9s %12s\n", "scenario", "requests", "rate", "success", "p95")
	for _, sc := range summary.Scenarios {
		fmt.Printf("%-10s %8d %8.1f/s %8.1f%% %12s\n",
			sc.Name, sc.Requests, sc.Rate, sc.SuccessRate*100, sc.Latencies.P95)
	}
	fmt.Printf("%-10s %8d %8.1f/s %8.1f%% %12s\n",
		"combined", summary.Requests, summary.Rate, summary.SuccessRate*100, summary.Latencies.P95)
}

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/jecklgamis/resonate"
)

const (
	requests = 50
	workers  = 5
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Requests(requests).
		Workers(workers).
		Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	fmt.Printf("requests target:  %d\n", requests)
	fmt.Printf("workers:          %d (irrelevant to the total -- just concurrency)\n\n", workers)

	fmt.Printf("requests          %d\n", summary.Requests)
	fmt.Printf("success rate      %.0f%%\n", summary.SuccessRate*100)
}

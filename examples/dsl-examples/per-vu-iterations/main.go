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
	workers    = 5
	iterations = 10
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Workers(workers).
		Iterations(iterations).
		Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	fmt.Printf("workers:          %d\n", workers)
	fmt.Printf("iterations/VU:    %d\n", iterations)
	fmt.Printf("expected total:   %d (workers * iterations, no Duration/Requests set -- self-terminating)\n\n", workers*iterations)

	fmt.Printf("requests          %d\n", summary.Requests)
	fmt.Printf("success rate      %.0f%%\n", summary.SuccessRate*100)
}

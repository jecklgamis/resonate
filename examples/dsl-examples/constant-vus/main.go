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

const (
	duration    = 3 * time.Second
	workers     = 20
	serverDelay = 50 * time.Millisecond
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Workers(workers).
		Duration(duration).
		Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	fmt.Printf("workers:          %d\n", workers)
	fmt.Printf("server latency:   %s\n", serverDelay)
	fmt.Printf("expected rate:    ~%.1f req/s (workers / latency)\n\n", float64(workers)/serverDelay.Seconds())

	fmt.Printf("requests          %d\n", summary.Requests)
	fmt.Printf("achieved rate     %.1f req/s\n", summary.Rate)
	fmt.Printf("mean latency      %s\n", summary.Latencies.Mean)
}

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
	targetRate  = 500
	rampUp      = 1 * time.Second
	hold        = 1 * time.Second
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
		Stages(
			resonate.Stage{Duration: rampUp, Workers: workers, Rate: targetRate},
			resonate.Stage{Duration: hold, Workers: workers, Rate: targetRate},
		).
		Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	fmt.Printf("target rate:      %.0f req/s\n", float64(targetRate))
	fmt.Printf("server latency:   %s\n", serverDelay)
	fmt.Printf("schedule:         ramp 0->%d req/s over %s, then hold %s (concurrency capped at %d, no MaxWorkers)\n", targetRate, rampUp, hold, workers)
	fmt.Println()

	fmt.Printf("requests          %d\n", summary.Requests)
	fmt.Printf("achieved rate     %.1f req/s\n", summary.Rate)
	fmt.Printf("mean latency      %s\n", summary.Latencies.Mean)
	fmt.Printf("saturated         %v\n", summary.Saturated)
	fmt.Printf("peak concurrency  %d\n", summary.PeakConcurrency)
}

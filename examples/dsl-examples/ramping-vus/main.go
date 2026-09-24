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

const serverDelay = 50 * time.Millisecond

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	summary, err := resonate.NewScenario().
		Get(srv.URL).
		Stages(
			resonate.Stage{Duration: 1 * time.Second, Workers: 20, Rate: 0},
			resonate.Stage{Duration: 1 * time.Second, Workers: 20, Rate: 0},
			resonate.Stage{Duration: 1 * time.Second, Workers: 0, Rate: 0},
		).
		Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	fmt.Printf("schedule:         ramp 0->20 workers over 1s, hold 20 for 1s, ramp down to 0 over 1s\n")
	fmt.Printf("server latency:   %s\n\n", serverDelay)

	fmt.Printf("requests          %d\n", summary.Requests)
	fmt.Printf("achieved rate     %.1f req/s\n", summary.Rate)
	fmt.Printf("mean latency      %s\n", summary.Latencies.Mean)
	fmt.Printf("peak concurrency  %d\n", summary.PeakConcurrency)
}

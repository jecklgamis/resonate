package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jecklgamis/resonate"
)

func main() {
	url := flag.String("url", "http://localhost:8080/orders", "endpoint to POST to")
	requests := flag.Uint64("requests", 10, "total requests to send")
	workers := flag.Int("workers", 2, "concurrent workers")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	summary, err := resonate.NewScenario().
		Post(*url).
		Header("X-Request-Id", "{{uuid}}").
		JSONBody(`{"id": {{.Seq}}, "item": "{{randChoice "widget" "gadget" "gizmo"}}", "qty": {{randInt 1 10}}}`).
		ExpectStatus(http.StatusCreated).
		Requests(*requests).
		Workers(*workers).
		Run(ctx)
	if err != nil {
		log.Fatalf("running scenario: %v", err)
	}

	summary.Print(os.Stdout)

	if err := resonate.WriteHTMLFile("report.html", resonate.HTMLReport{
		Title:   fmt.Sprintf("dsl-examples demo: POST %s", *url),
		Summary: summary,
	}); err != nil {
		log.Fatalf("writing report.html: %v", err)
	}
	fmt.Println("\nwrote report.html")
}

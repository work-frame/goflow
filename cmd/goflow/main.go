package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/work-frame/goflow/internal/cli"
	"github.com/work-frame/goflow/internal/client"
)

func main() {
	baseURL := os.Getenv("GOFLOW_API_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	// Ctrl+C cancels the request in flight instead of leaving the terminal waiting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	// GOFLOW_TOKEN is a stopgap until "goflow login" (Task 20). The token is only
	// sent in the Authorization header, never printed.
	code := cli.Run(ctx, os.Args[1:], client.New(baseURL, os.Getenv("GOFLOW_TOKEN")), os.Stdout, os.Stderr)
	stop() // called directly, not deferred: os.Exit skips defers
	os.Exit(code)
}

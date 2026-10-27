package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/work-frame/goflow/internal/cli"
	"github.com/work-frame/goflow/internal/client"
	"github.com/work-frame/goflow/internal/credentials"
)

func main() {
	baseURL := os.Getenv("GOFLOW_API_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	// Ctrl+C cancels the request in flight instead of leaving the terminal waiting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], baseURL)
	stop() // called directly, not deferred: os.Exit skips defers
	os.Exit(code)
}

func run(ctx context.Context, args []string, baseURL string) int {
	path, err := credentials.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	store := credentials.File{Path: path}

	if len(args) == 1 {
		switch args[0] {
		case "login":
			prompter := cli.NewTerminalPrompter(os.Stdin, os.Stderr)
			return cli.Login(ctx, prompter, client.New(baseURL, ""), store, baseURL, os.Stdout, os.Stderr)
		case "logout":
			return cli.Logout(ctx, client.New(baseURL, ""), store, os.Stdout, os.Stderr)
		}
	}

	// GOFLOW_TOKEN wins, for scripts and CI. Otherwise use the saved login,
	// but only if this API issued it and it hasn't expired.
	token := os.Getenv("GOFLOW_TOKEN")
	if token == "" {
		token, err = store.TokenFor(baseURL, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	}
	return cli.Run(ctx, args, client.New(baseURL, token), os.Stdout, os.Stderr)
}

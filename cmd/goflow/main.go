package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/work-frame/goflow/internal/cli"
)

func main() {
	path, err := dataPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	os.Exit(cli.Run(os.Args[1:], path, os.Stdout, os.Stderr))
}

// dataPath returns where jobs are stored.
// GOFLOW_DATA_FILE overrides the default of ~/.goflow/jobs.json.
func dataPath() (string, error) {
	if p := os.Getenv("GOFLOW_DATA_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".goflow", "jobs.json"), nil
}

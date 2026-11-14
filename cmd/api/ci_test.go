package main

import (
	"regexp"
	"strings"
	"testing"
)

// The CI workflow runs code from other repositories with this repository
// checked out. A tag like v7 can be moved by its owner to point at
// different code, so every action is pinned to a full commit SHA; the token
// can only read; and no step uses a secret. CI tests with the same
// PostgreSQL and Valkey images as compose.yaml and the same Go minor
// version as the Dockerfile, so it checks what runs (FAILURES.md, case 55).
func TestCIWorkflowPinsEveryActionAndOnlyReads(t *testing.T) {
	parallelTest(t)
	wf := readRepoFile(t, ".github/workflows/ci.yml")

	uses := regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*(\S+)`).FindAllStringSubmatch(wf, -1)
	if len(uses) < 4 {
		t.Fatalf("found %d uses: lines, want at least 4 (checkout and setup-go in each job): is the file still parsed?", len(uses))
	}
	pinned := regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)
	checkouts := 0
	for _, u := range uses {
		if !pinned.MatchString(u[1]) {
			t.Errorf("action %s isn't pinned to a full commit SHA: whoever controls its tag controls what runs here", u[1])
		}
		if strings.HasPrefix(u[1], "actions/checkout@") {
			checkouts++
		}
	}
	if n := strings.Count(wf, "persist-credentials: false"); n != checkouts {
		t.Errorf("%d checkouts but %d with persist-credentials: false: the token would stay on disk for later steps", checkouts, n)
	}

	if !regexp.MustCompile(`(?m)^permissions:\n  contents: read\n`).MatchString(wf) {
		t.Error("the workflow doesn't set permissions to contents: read at the top")
	}
	if m := regexp.MustCompile(`(?m)^\s*[\w-]+:\s*write\b`).FindString(wf); m != "" {
		t.Errorf("the workflow asks for write access: %q", strings.TrimSpace(m))
	}
	for _, banned := range []string{"secrets.", "pull_request_target", "@latest"} {
		if strings.Contains(wf, banned) {
			t.Errorf("the workflow contains %q", banned)
		}
	}

	for _, want := range []string{
		"run: ./scripts/check.sh\n",
		"GOFLOW_TEST_DATABASE_URL: ",
		"GOFLOW_TEST_REDIS_URL: ",
		"run: go install golang.org/x/vuln/cmd/govulncheck@v",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("the workflow lacks %q", want)
		}
	}

	compose := readRepoFile(t, "compose.yaml")
	for _, image := range []string{"postgres:17", "valkey/valkey:8"} {
		if !strings.Contains(compose, "image: "+image+" ") && !strings.Contains(compose, "image: "+image+"\n") {
			t.Fatalf("compose.yaml no longer runs %s: update this test and the workflow together", image)
		}
		if !strings.Contains(wf, "image: "+image+"\n") {
			t.Errorf("the workflow doesn't run %s, the image compose.yaml runs", image)
		}
	}

	from := regexp.MustCompile(`(?m)^FROM golang:(\d+\.\d+)-`).FindStringSubmatch(readRepoFile(t, "Dockerfile"))
	if from == nil {
		t.Fatal("no FROM golang:X.Y- line in the Dockerfile")
	}
	want := `go-version: "` + from[1] + `.x"`
	if n := strings.Count(wf, want); n != 2 {
		t.Errorf("found %s %d times, want 2 (one per job): CI should build with the Go the image builds with", want, n)
	}
}

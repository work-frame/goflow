package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// runCheck runs scripts/check.sh with only the given variables set and a
// PATH that holds nothing: past the URL check, the script stops at its
// first command (gofmt), so this never runs the suite inside the suite.
func runCheck(t *testing.T, env ...string) (output string, code int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("bash not found: check.sh needs it")
	}
	cmd := exec.Command(bash, "scripts/check.sh")
	cmd.Dir = "../.."
	cmd.Env = append([]string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(out), cmd.ProcessState.ExitCode()
}

// A test that needs PostgreSQL or Redis skips itself without its URL, and
// a skipped test prints "ok". So a full check refuses to start without
// both (Task 49: the Redis tests skipped on the laptop for months).
func TestCheckRefusesToSkipTheDatabaseTests(t *testing.T) {
	const db, redis = "GOFLOW_TEST_DATABASE_URL", "GOFLOW_TEST_REDIS_URL"
	cases := []struct {
		name    string
		env     []string
		missing []string
	}{
		{"neither", nil, []string{db, redis}},
		{"no Redis", []string{db + "=postgres://x"}, []string{redis}},
		{"no PostgreSQL", []string{redis + "=redis://x"}, []string{db}},
		{"empty Redis", []string{db + "=postgres://x", redis + "="}, []string{redis}},
		{"quick check asked wrongly", []string{"GOFLOW_QUICK_CHECK=yes"}, []string{db, redis}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := runCheck(t, c.env...)
			if code != 1 {
				t.Errorf("exit code %d, want 1", code)
			}
			if strings.Contains(out, "==> gofmt") {
				t.Errorf("check.sh started the checks without every URL:\n%s", out)
			}
			for _, name := range c.missing {
				if !strings.Contains(out, name) {
					t.Errorf("output doesn't name %s:\n%s", name, out)
				}
			}
			if !strings.Contains(out, "GOFLOW_QUICK_CHECK=1") {
				t.Errorf("output doesn't say how to run a quick check:\n%s", out)
			}
		})
	}
}

// With both URLs, or with GOFLOW_QUICK_CHECK=1, the checks start. A
// quick check says so first, so its result can't pass for a full one.
func TestCheckStartsWithBothURLsOrWhenAskedForAQuickCheck(t *testing.T) {
	out, _ := runCheck(t, "GOFLOW_TEST_DATABASE_URL=postgres://x", "GOFLOW_TEST_REDIS_URL=redis://x")
	if !strings.Contains(out, "==> gofmt") || strings.Contains(out, "QUICK") {
		t.Errorf("with both URLs, want a full check:\n%s", out)
	}

	out, _ = runCheck(t, "GOFLOW_QUICK_CHECK=1")
	if !strings.Contains(out, "==> QUICK CHECK") || !strings.Contains(out, "==> gofmt") {
		t.Errorf("with GOFLOW_QUICK_CHECK=1, want a quick check that says so:\n%s", out)
	}
}

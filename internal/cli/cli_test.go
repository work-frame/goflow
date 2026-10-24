package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// run calls Run with in-memory output buffers and returns what it produced.
func run(t *testing.T, path string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(args, path, &out, &errOut)
	return code, out.String(), errOut.String()
}

func newDataPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "jobs.json")
}

func mustCreate(t *testing.T, path, title string) string {
	t.Helper()
	code, out, errOut := run(t, path, "job", "create", title)
	if code != exitOK {
		t.Fatalf("create %q: exit %d, stderr: %s", title, code, errOut)
	}
	return strings.TrimSpace(out)
}

func TestCreateThenGet(t *testing.T) {
	path := newDataPath(t)

	code, out, errOut := run(t, path, "job", "create", "send", "welcome", "email")
	if code != exitOK {
		t.Fatalf("create: exit %d, stderr: %s", code, errOut)
	}
	id := strings.TrimSpace(out)
	if len(id) != 32 {
		t.Fatalf("create printed %q, want a 32-character ID", id)
	}

	// Each Run call loads from disk, like a separate process would.
	code, out, errOut = run(t, path, "job", "get", id)
	if code != exitOK {
		t.Fatalf("get: exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, `"title": "send welcome email"`) {
		t.Errorf("get output missing title:\n%s", out)
	}
	if !strings.Contains(out, `"status": "pending"`) {
		t.Errorf("get output missing status:\n%s", out)
	}
}

func TestListShowsJobsInOrder(t *testing.T) {
	path := newDataPath(t)
	mustCreate(t, path, "first job")
	mustCreate(t, path, "second job")

	code, out, errOut := run(t, path, "job", "list")
	if code != exitOK {
		t.Fatalf("list: exit %d, stderr: %s", code, errOut)
	}
	if !strings.HasPrefix(out, "ID") {
		t.Errorf("list output missing header:\n%s", out)
	}
	first := strings.Index(out, "first job")
	second := strings.Index(out, "second job")
	if first == -1 || second == -1 || first > second {
		t.Errorf("list output wrong or out of order:\n%s", out)
	}
}

func TestListFiltersByStatus(t *testing.T) {
	path := newDataPath(t)
	mustCreate(t, path, "send email")

	_, out, _ := run(t, path, "job", "list", "--status", "pending")
	if !strings.Contains(out, "send email") {
		t.Errorf("pending filter should include the job:\n%s", out)
	}

	_, out, _ = run(t, path, "job", "list", "--status", "completed")
	if strings.Contains(out, "send email") {
		t.Errorf("completed filter should exclude the job:\n%s", out)
	}

	code, _, _ := run(t, path, "job", "list", "--status", "banana")
	if code != exitUsage {
		t.Errorf("invalid status: exit %d, want %d", code, exitUsage)
	}
}

func TestDelete(t *testing.T) {
	path := newDataPath(t)
	id := mustCreate(t, path, "send email")

	code, out, errOut := run(t, path, "job", "delete", id)
	if code != exitOK {
		t.Fatalf("delete: exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, id) {
		t.Errorf("delete output = %q, want it to mention %s", out, id)
	}

	code, _, errOut = run(t, path, "job", "get", id)
	if code != exitError || !strings.Contains(errOut, "not found") {
		t.Errorf("get after delete: exit %d, stderr %q", code, errOut)
	}

	code, _, _ = run(t, path, "job", "delete", id)
	if code != exitError {
		t.Errorf("second delete: exit %d, want %d", code, exitError)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"only job", []string{"job"}},
		{"wrong resource", []string{"user", "list"}},
		{"unknown command", []string{"job", "run"}},
		{"create without title", []string{"job", "create"}},
		{"get without id", []string{"job", "get"}},
		{"get with two ids", []string{"job", "get", "a", "b"}},
		{"list with stray arg", []string{"job", "list", "extra"}},
		{"list with unknown flag", []string{"job", "list", "--nope"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := run(t, newDataPath(t), tc.args...)
			if code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if !strings.Contains(errOut, "Usage:") {
				t.Errorf("stderr missing usage text:\n%s", errOut)
			}
		})
	}
}

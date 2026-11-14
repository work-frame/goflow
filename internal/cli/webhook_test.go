package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

func TestCreateWebhookFromAFile(t *testing.T) {
	f := newFake()
	path := filepath.Join(t.TempDir(), "order.json")
	if err := os.WriteFile(path, []byte(`{"order": 1042}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, f, "job", "create", "--webhook", "https://example.com/hook", "--body", "@"+path, "--max-attempts", "5", "order", "1042", "paid")
	if code != exitOK || strings.TrimSpace(stdout) == "" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	n := f.created[0]
	if n.Title != "order 1042 paid" || n.Type != job.TypeWebhook || n.MaxAttempts != 5 ||
		n.Webhook == nil || n.Webhook.URL != "https://example.com/hook" || string(n.Webhook.Body) != `{"order": 1042}` {
		t.Errorf("sent %+v (webhook %+v)", n, n.Webhook)
	}
	if strings.Contains(stderr, "shell history") {
		t.Error("a body from a file got the shell-history warning")
	}
}

// Daniel's condition: a body typed on the command line stays in shell
// history, so the CLI says so and points at @file.
func TestCreateWebhookFromTheCommandLineWarns(t *testing.T) {
	f := newFake()
	code, _, stderr := run(t, f, "job", "create", "--webhook", "https://example.com/hook", "--body", `{"order":1042}`, "paid")
	if code != exitOK || !strings.Contains(stderr, "shell history") || !strings.Contains(stderr, "@file") {
		t.Errorf("code %d, stderr %q; want a warning about shell history recommending @file", code, stderr)
	}
}

func TestCreateWebhookRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	os.WriteFile(path, []byte(`"`+strings.Repeat("a", job.MaxWebhookBodyBytes)+`"`), 0o600)
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--body", "{}", "x"}, exitUsage, "--body needs --webhook"},
		{[]string{"--webhook", "https://example.com/h", "x"}, exitUsage, "--webhook needs --body"},
		{[]string{"--webhook", "https://example.com/h", "--body", "{", "x"}, exitError, "not JSON"},
		{[]string{"--webhook", "https://example.com/h", "--body", "@/no/such/file.json", "x"}, exitError, "no such file"},
		{[]string{"--webhook", "https://example.com/h", "--body", "@" + path, "x"}, exitError, "more than 65536"},
		{[]string{"--webhook", "https://example.com/h", "--body", "{}"}, exitUsage, "missing title"},
	} {
		f := newFake()
		code, _, stderr := run(t, f, append([]string{"job", "create"}, c.args...)...)
		if code != c.code || !strings.Contains(stderr, c.want) || len(f.created) != 0 {
			t.Errorf("%v: code %d, stderr %q, sent %d; want code %d saying %q and nothing sent", c.args, code, stderr, len(f.created), c.code, c.want)
		}
	}
}

func TestAttemptsTable(t *testing.T) {
	f := newFake()
	_, id, _ := run(t, f, "job", "create", "paid")
	f.attempts = []job.Attempt{
		{Attempt: 1, StartedAt: time.Now(), DurationMS: 10012, Outcome: job.OutcomeRetry, Error: "webhook: no complete answer within 10s"},
		{Attempt: 2, StartedAt: time.Now(), DurationMS: 85, StatusCode: 200, Outcome: job.OutcomeDelivered},
	}
	code, stdout, _ := run(t, f, "job", "attempts", strings.TrimSpace(id))
	if code != exitOK || !strings.Contains(stdout, "OUTCOME") || !strings.Contains(stdout, "10012ms") ||
		!strings.Contains(stdout, "delivered") || !strings.Contains(stdout, "no complete answer") {
		t.Errorf("code %d, table:\n%s", code, stdout)
	}
	if code, _, _ := run(t, f, "job", "attempts", "missing"); code != exitError {
		t.Errorf("unknown job: code %d, want %d", code, exitError)
	}
}

// The secret is printed once, alone on stdout, with the warning on stderr;
// after a rotation the CLI says until when the old one still signs.
func TestWebhookSecret(t *testing.T) {
	f := newFake()
	code, stdout, stderr := run(t, f, "webhook-secret")
	if code != exitOK || stdout != "whsec_first\n" || !strings.Contains(stderr, "only time") || strings.Contains(stderr, "previous") {
		t.Fatalf("first: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	_, stdout, stderr = run(t, f, "webhook-secret")
	if stdout != "whsec_second\n" || !strings.Contains(stderr, "previous secret keeps signing") {
		t.Errorf("rotation: stdout %q, stderr %q", stdout, stderr)
	}
	if code, _, _ := run(t, f, "webhook-secret", "extra"); code != exitUsage {
		t.Errorf("with an argument: code %d, want %d", code, exitUsage)
	}
}

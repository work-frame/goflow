package main

import (
	"regexp"
	"strings"
	"testing"
)

// The uptime check (Phase 16) runs on a schedule with nothing to protect:
// no token permissions, no secrets, no code from other repositories. It
// asks /readyz, which checks the database too, not /healthz.
func TestUptimeWorkflowNeedsNothing(t *testing.T) {
	parallelTest(t)
	wf := readRepoFile(t, ".github/workflows/uptime.yml")
	for _, want := range []string{"\npermissions: {}\n", "  schedule:\n    - cron: ", "  workflow_dispatch:\n", "/readyz", "--max-time 90"} {
		if !strings.Contains(wf, want) {
			t.Errorf("uptime.yml lacks %q", want)
		}
	}
	for _, banned := range []string{"secrets.", "uses:", "pull_request_target", ": write"} {
		if strings.Contains(wf, banned) {
			t.Errorf("uptime.yml contains %q", banned)
		}
	}
	if !regexp.MustCompile(`cron: "\d+ \*/6 \* \* \*"`).MatchString(wf) {
		t.Error("uptime.yml doesn't run every 6 hours: more often would keep the free instance awake")
	}
}

// The backup (Phase 16, decision H2) never exists unencrypted on disk: the
// dump is piped straight into gpg, and the drill decrypts straight into
// pg_restore. A backup script runs rarely and by hand, so this reads it
// (FAILURES.md, case 64).
func TestBackupIsEncryptedBeforeItTouchesTheDisk(t *testing.T) {
	parallelTest(t)
	b := readRepoFile(t, "scripts/backup.sh")
	for _, want := range []string{
		"set -euo pipefail", "umask 077", `read -rsp "Neon database URL (not shown): " url`,
		"gpg_args=(--symmetric --cipher-algo AES256 --output \"$tmp\")",
		"</dev/null |\n    gpg \"${gpg_args[@]}\"",
		"</dev/null | gpg \"${gpg_args[@]}\"",
		"backups never go there",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("backup.sh lacks %q", want)
		}
	}
	// Every pg_dump's output goes into gpg, never into a file.
	if n, m := strings.Count(b, "exec pg_dump")+strings.Count(b, "\n  pg_dump "), strings.Count(b, "gpg \"${gpg_args[@]}\""); n != 2 || m != 2 {
		t.Errorf("backup.sh has %d pg_dump calls and %d into gpg; want each dump piped into gpg", n, m)
	}
	if regexp.MustCompile(`pg_dump[^|\n]*>`).MatchString(b) {
		t.Error("backup.sh redirects pg_dump into a file")
	}
	if strings.Contains(b, "echo \"$url") || strings.Contains(b, "echo $url") {
		t.Error("backup.sh prints the database URL")
	}

	d := readRepoFile(t, "scripts/restore-drill.sh")
	for _, want := range []string{"set -euo pipefail", `gpg "${gpg_args[@]}" "$file" | restore_`, "already exists", "db=goflow_restore_drill"} {
		if !strings.Contains(d, want) {
			t.Errorf("restore-drill.sh lacks %q", want)
		}
	}
	if strings.Contains(d, "--output") {
		t.Error("restore-drill.sh writes the decrypted dump to a file")
	}
}

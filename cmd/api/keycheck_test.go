package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/auth"
)

// keyCheckAccount registers a user with a known email, logs in, and makes
// a key with the given name and scopes. The user goes when the test ends.
func keyCheckAccount(t *testing.T, client *http.Client, base, dbURL, name string, scopes []auth.Scope) (email, session, secret, keyID string) {
	t.Helper()
	b := make([]byte, 6)
	rand.Read(b)
	email = "e2e-" + hex.EncodeToString(b) + "@example.com"
	t.Cleanup(func() { deleteUser(t, dbURL, email) })
	creds := `{"email":"` + email + `","password":"` + e2ePassword + `"}`
	if status, _, body := request(t, client, http.MethodPost, base+"/api/v1/auth/register", creds); status != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", status, body)
	}
	status, _, body := request(t, client, http.MethodPost, base+"/api/v1/auth/login", creds)
	if status != http.StatusOK {
		t.Fatalf("login: status %d, body %s", status, body)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &login); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(map[string]any{"name": name, "scopes": scopes, "expires_in_days": 30, "password": e2ePassword})
	status, _, body = requestAs(t, client, http.MethodPost, base+"/api/v1/keys", login.Token, string(req))
	if status != http.StatusCreated {
		t.Fatalf("create key: status %d, body %s", status, body)
	}
	var created struct {
		Secret string `json:"secret"`
		Key    struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	return email, login.Token, created.Secret, created.Key.ID
}

// runKeyCheck runs scripts/key-check.sh against base with the three
// hidden answers on stdin. extra goes before the script, for bash -x.
func runKeyCheck(t *testing.T, base string, answers []string, extra ...string) (out string, code int) {
	t.Helper()
	return runKeyCheckEnv(t, nil, base, answers, extra...)
}

// runKeyCheckEnv is runKeyCheck with extra environment variables.
func runKeyCheckEnv(t *testing.T, env []string, base string, answers []string, extra ...string) (out string, code int) {
	t.Helper()
	args := append(extra, "../../scripts/key-check.sh", base)
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(strings.Join(answers, "\n") + "\n")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return buf.String(), ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run key-check.sh: %v", err)
	}
	return buf.String(), 0
}

// keyStatus is the key's status in the account's list.
func keyStatus(t *testing.T, client *http.Client, base, session, id string) string {
	t.Helper()
	status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/keys", session, "")
	if status != http.StatusOK {
		t.Fatalf("list keys: status %d, body %s", status, body)
	}
	var list struct {
		Keys []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	for _, k := range list.Keys {
		if k.ID == id {
			return k.Status
		}
	}
	return "missing"
}

// The script's output never holds a secret, or the email (condition 5).
func checkKeyCheckOutput(t *testing.T, out string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("key-check.sh printed a secret or the email (%.6s...):\n%s", s, out)
		}
	}
	if long := regexp.MustCompile(`[A-Za-z0-9_-]{32,}`).FindString(out); long != "" {
		t.Errorf("key-check.sh printed something token-like (%.6s...)", long)
	}
}

// All seven steps pass against a real server, and afterwards the key is
// revoked and the job it made is the account's, attributed to it.
func TestKeyCheckPassesAgainstARealServer(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := testClient(t, 10*time.Second)
	email, session, secret, keyID := keyCheckAccount(t, client, base, dbURL, "live test", createOnly)

	out, code := runKeyCheck(t, base, []string{email, e2ePassword, secret})
	if code != 0 {
		t.Fatalf("key-check.sh exit %d:\n%s", code, out)
	}
	for i := 1; i <= 7; i++ {
		if !strings.Contains(out, "PASS "+string(rune('0'+i))+"/7 ") {
			t.Errorf("no PASS line for step %d:\n%s", i, out)
		}
	}
	if !strings.Contains(out, "all 7 steps passed") {
		t.Errorf("no closing line:\n%s", out)
	}
	checkKeyCheckOutput(t, out, secret, e2ePassword, email, session)
	if got := keyStatus(t, client, base, session, keyID); got != "revoked" {
		t.Errorf("key after the check: %s, want revoked", got)
	}
	// The step 2 job stays, named after the key, and only one was made.
	status, _, body := requestAs(t, client, http.MethodGet, base+"/api/v1/jobs", session, "")
	if status != http.StatusOK {
		t.Fatalf("list jobs: status %d", status)
	}
	var jobs struct {
		Jobs []struct {
			Title    string `json:"title"`
			APIKeyID string `json:"api_key_id"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(body, &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Jobs) != 1 || jobs.Jobs[0].APIKeyID != keyID || jobs.Jobs[0].Title != "live test (Task 88)" {
		t.Errorf("jobs after the check: %+v, want one job by key %s", jobs.Jobs, keyID)
	}
}

// A step that fails stops the script with exit 1, and the key is revoked
// anyway (condition 4). A key with jobs:read makes step 4 answer 200.
func TestKeyCheckRevokesTheKeyWhenAStepFails(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := testClient(t, 10*time.Second)
	email, session, secret, keyID := keyCheckAccount(t, client, base, dbURL, "live test", bothScopes)

	out, code := runKeyCheck(t, base, []string{email, e2ePassword, secret})
	if code != 1 {
		t.Fatalf("key-check.sh exit %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(out, "FAIL 4/7 list jobs with the key (scopes jobs:create,jobs:read): status 200, want 403") {
		t.Errorf("no FAIL line for step 4 with the actual status:\n%s", out)
	}
	if strings.Contains(out, "PASS 5/7") || strings.Contains(out, "all 7 steps passed") {
		t.Errorf("the script went on after a failed step:\n%s", out)
	}
	if !strings.Contains(out, "cleanup: key ...") {
		t.Errorf("no cleanup line:\n%s", out)
	}
	checkKeyCheckOutput(t, out, secret, e2ePassword, email, session)
	if got := keyStatus(t, client, base, session, keyID); got != "revoked" {
		t.Errorf("key after a failed check: %s, want revoked", got)
	}
	if status, _, _ := requestAs(t, client, http.MethodPost, base+"/api/v1/jobs", secret, `{"title":"x"}`); status != http.StatusUnauthorized {
		t.Errorf("the key after a failed check: status %d, want 401", status)
	}
}

// A key that isn't "live test" is left alone: the script stops at step 1
// and revokes nothing it hasn't identified. A wrong password stops it
// before any key is touched.
func TestKeyCheckTouchesOnlyTheLiveTestKey(t *testing.T) {
	parallelTest(t)
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := testClient(t, 10*time.Second)
	email, session, secret, keyID := keyCheckAccount(t, client, base, dbURL, "production", createOnly)

	out, code := runKeyCheck(t, base, []string{email, e2ePassword, secret})
	if code != 1 || !strings.Contains(out, "FAIL 1/7 want exactly one active key named \"live test\"") {
		t.Errorf("key-check.sh with another key: exit %d:\n%s", code, out)
	}
	out2, code := runKeyCheck(t, base, []string{email, "not the password", secret})
	if code != 1 || !strings.Contains(out2, "stop: login answered 401") {
		t.Errorf("key-check.sh with a wrong password: exit %d:\n%s", code, out2)
	}
	checkKeyCheckOutput(t, out+out2, secret, e2ePassword, "not the password", email, session)
	if got := keyStatus(t, client, base, session, keyID); got != "active" {
		t.Errorf("the other key after the check: %s, want active", got)
	}
}

// Every argument curl was started with, recorded by a stand-in curl
// first on PATH, holds no secret: the header and the login body went
// through /dev/fd (condition 1). Case 75 breaks this one too.
func TestKeyCheckKeepsSecretsOffCurlsCommandLine(t *testing.T) {
	parallelTest(t)
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal("curl is not installed")
	}
	bin := t.TempDir()
	argv := filepath.Join(t.TempDir(), "argv")
	shim := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" >>" + argv + "\nexec " + realCurl + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	dbURL := testDatabaseURL(t)
	base, _ := startServer(t, dbURL)
	client := testClient(t, 10*time.Second)
	email, _, secret, _ := keyCheckAccount(t, client, base, dbURL, "live test", createOnly)

	out, code := runKeyCheckEnv(t, []string{"PATH=" + bin + ":" + os.Getenv("PATH")}, base, []string{email, e2ePassword, secret})
	if code != 0 {
		t.Fatalf("key-check.sh exit %d:\n%s", code, out)
	}
	recorded, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(recorded), "/api/v1/"); n < 9 {
		t.Fatalf("the stand-in curl saw %d API calls, want at least 9: is it first on PATH?", n)
	}
	for _, s := range []string{secret, secret[len(secret)-12:], e2ePassword, email, "Bearer"} {
		if strings.Contains(string(recorded), s) {
			t.Errorf("curl's arguments held a secret or the email (%.6s...)", s)
		}
	}
}

// Run with bash -x, it refuses before reading anything (condition 1).
func TestKeyCheckRefusesShellTracing(t *testing.T) {
	parallelTest(t)
	secret := "gfk_" + strings.Repeat("A", 43)
	out, code := runKeyCheck(t, "http://127.0.0.1:1", []string{"a@example.com", e2ePassword, secret}, "-x")
	if code != 2 || !strings.Contains(out, "stop: shell tracing is on") {
		t.Errorf("key-check.sh under bash -x: exit %d:\n%s", code, out)
	}
	checkKeyCheckOutput(t, out, secret, e2ePassword)
}

// What a test can't watch, the source holds: the key, the password and the
// session reach curl only through /dev/fd, never its arguments (condition
// 1). Case 75 breaks this one.
func TestKeyCheckScriptRules(t *testing.T) {
	parallelTest(t)
	src := readRepoFile(t, "scripts/key-check.sh")
	for _, want := range []string{
		"if [[ $- == *x* ]]; then",
		"set -euo pipefail",
		`printf 'header = "Authorization: Bearer %s"\n' "$1"`,
		"trap cleanup EXIT",
		"trap 'exit 130' INT TERM",
		`read -rsp 'Account password (hidden): ' pw`,
		`read -rsp 'API key from the dashboard dialog (hidden): ' key`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("key-check.sh lacks %q", want)
		}
	}
	if strings.Index(src, "if [[ $- == *x* ]]; then") > strings.Index(src, "read -rsp") {
		t.Error("key-check.sh must refuse tracing before it reads anything")
	}
	var code []string // the lines that run, comments left out
	for _, line := range strings.Split(src, "\n") {
		if l := strings.TrimSpace(line); !strings.HasPrefix(l, "#") {
			code = append(code, l)
		}
	}
	if n := strings.Count(strings.Join(code, "\n"), "Authorization"); n != 1 {
		t.Errorf("key-check.sh builds the Authorization header in %d places, want 1", n)
	}
	for _, banned := range []string{"set -x", "-H \"Authorization", "-H 'Authorization", "--user", "-u \"$", "tee ", "--output", "> /tmp", ">/tmp", "mktemp", "--trace", "--verbose"} {
		if strings.Contains(src, banned) {
			t.Errorf("key-check.sh contains %q", banned)
		}
	}
	secretVar := regexp.MustCompile(`\$\{?(key|pw|session|login)\b`)
	curls := 0
	for _, l := range code {
		if strings.Contains(l, `curl "`) {
			curls++
			if !strings.Contains(l, `-K <(auth_config "$3")`) {
				t.Errorf("key-check.sh calls curl without the header from /dev/fd: %s", l)
			}
			if secretVar.MatchString(l) {
				t.Errorf("key-check.sh puts a secret on curl's command line: %s", l)
			}
		}
		if (strings.HasPrefix(l, "echo") || strings.HasPrefix(l, "printf")) && secretVar.MatchString(l) {
			t.Errorf("key-check.sh prints a secret: %s", l)
		}
	}
	if curls != 2 {
		t.Errorf("key-check.sh calls curl in %d places, want the 2 in call()", curls)
	}
}

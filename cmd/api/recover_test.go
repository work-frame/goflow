package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/secrets"
)

// recoverSource builds an age-encrypted custom-format dump of a small
// database with a foreign key, an index and a CHECK, the way the nightly
// workflow makes them (pg_dump -Fc, then age to a public key). It returns
// the file and the private key.
func recoverSource(t *testing.T) (file, identity string) {
	t.Helper()
	for _, c := range []string{"pg_dump", "pg_restore", "age", "age-keygen"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("%s is not installed; skipping the recover.sh tests", c)
		}
	}
	srcURL, src := emptyDatabase(t)
	// pg_dump refuses a server newer than itself; say so instead of failing.
	var server int
	if err := src.QueryRow("SELECT current_setting('server_version_num')::int / 10000").Scan(&server); err != nil {
		t.Fatal(err)
	}
	v, err := exec.Command("pg_dump", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	var client int
	if f := strings.Fields(string(v)); len(f) >= 3 { // pg_dump (PostgreSQL) 17.6 ...
		client, _ = strconv.Atoi(strings.SplitN(f[2], ".", 2)[0])
	}
	if client < server {
		t.Skipf("this pg_dump is version %d and the test server %d; pg_dump must be at least the server's version", client, server)
	}
	if _, err := src.Exec(`
		CREATE TABLE schema_migrations (version integer PRIMARY KEY);
		CREATE TABLE users (id integer PRIMARY KEY CHECK (id > 0));
		CREATE TABLE jobs (id integer PRIMARY KEY, user_id integer NOT NULL REFERENCES users (id), created_at timestamptz NOT NULL DEFAULT now());
		CREATE INDEX jobs_user_idx ON jobs (user_id);
		INSERT INTO schema_migrations VALUES (1), (2);
		INSERT INTO users VALUES (1);
		INSERT INTO jobs (id, user_id) VALUES (1, 1), (2, 1), (3, 1);`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keygen, err := exec.Command("age-keygen").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(keygen), "\n") {
		if strings.HasPrefix(line, "AGE-SECRET-KEY-1") {
			identity = line
		}
	}
	pub := exec.Command("age-keygen", "-y")
	pub.Stdin = strings.NewReader(identity + "\n")
	recipient, err := pub.Output()
	if err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "goflow-test.dump.age")
	dump := exec.Command("bash", "-c", `set -o pipefail; pg_dump -Fc --dbname="$1" | age -r "$2" >"$3"`, "bash", srcURL, strings.TrimSpace(string(recipient)), file)
	if out, err := dump.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump | age: %v\n%s", err, out)
	}
	return file, identity
}

// runRecover runs scripts/recover.sh with local tools, answering its
// prompts from lines: decision time, URL, endpoint, age key, "start the
// API?".
func runRecover(t *testing.T, file string, lines ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command("bash", "../../scripts/recover.sh", file)
	cmd.Env = append(os.Environ(), "GOFLOW_PG_TOOLS=local")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

func publicTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

// The happy path: an empty target gets every table, row and constraint,
// and the output never shows the URL or the age key.
func TestRecoverLoadsIntoAnEmptyDatabase(t *testing.T) {
	parallelTest(t)
	file, identity := recoverSource(t)
	target, db := emptyDatabase(t)
	stdout, stderr, err := runRecover(t, file, "", target, "127", identity, "no")
	if err != nil {
		t.Fatalf("recover.sh failed: %v\n%s\n%s", err, stdout, stderr)
	}
	var jobs, migrations int
	if err := db.QueryRow("SELECT (SELECT count(*) FROM jobs), (SELECT count(*) FROM schema_migrations)").Scan(&jobs, &migrations); err != nil {
		t.Fatal(err)
	}
	if jobs != 3 || migrations != 2 {
		t.Errorf("restored %d jobs and %d migrations, want 3 and 2", jobs, migrations)
	}
	for _, want := range []string{"foreign keys       1 / 1", "indexes            4 / 4", "CHECK constraints  1 / 1", "loaded", "==> times (UTC)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	u, _ := url.Parse(target)
	pw, _ := u.User.Password()
	for _, secret := range []string{target, identity, ":" + pw + "@"} {
		if strings.Contains(stdout+stderr, secret) {
			t.Errorf("recover.sh printed a secret (%d characters)", len(secret))
		}
	}
}

// Case 72: the script must never load into a database that holds anything,
// even one table that isn't GoFlow's. Production always does.
func TestRecoverRefusesANonEmptyTarget(t *testing.T) {
	parallelTest(t)
	file, identity := recoverSource(t)
	target, db := emptyDatabase(t)
	if _, err := db.Exec("CREATE TABLE stranger (x integer)"); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runRecover(t, file, "", target, "127", identity, "no")
	if err == nil || !strings.Contains(stderr, "the target isn't empty") {
		t.Fatalf("recover.sh didn't refuse a target with a table: err %v\n%s", err, stderr)
	}
	if got := publicTables(t, db); len(got) != 1 || got[0] != "stranger" {
		t.Errorf("the target now has tables %v; want only [stranger]", got)
	}
}

// The endpoint typed by hand must be the URL's; otherwise nothing at all
// happens, not even a connection.
func TestRecoverRefusesTheWrongEndpoint(t *testing.T) {
	parallelTest(t)
	file, identity := recoverSource(t)
	target, db := emptyDatabase(t)
	_, stderr, err := runRecover(t, file, "", target, "128", identity, "no")
	if err == nil || !strings.Contains(stderr, "isn't the endpoint in the URL") {
		t.Fatalf("recover.sh accepted the wrong endpoint: err %v\n%s", err, stderr)
	}
	if got := publicTables(t, db); len(got) != 0 {
		t.Errorf("the target has tables %v after a refusal", got)
	}
}

// A backup cut short in the download decrypts partway; nothing of it may
// reach the target.
func TestRecoverSendsNothingFromACutOffBackup(t *testing.T) {
	parallelTest(t)
	file, identity := recoverSource(t)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.dump.age")
	if err := os.WriteFile(cut, data[:len(data)*2/3], 0o600); err != nil {
		t.Fatal(err)
	}
	target, db := emptyDatabase(t)
	_, stderr, err := runRecover(t, cut, "", target, "127", identity, "no")
	if err == nil || !strings.Contains(stderr, "nothing was sent to the target") {
		t.Fatalf("recover.sh didn't stop on a cut-off backup: err %v\n%s", err, stderr)
	}
	if got := publicTables(t, db); len(got) != 0 {
		t.Errorf("the target has tables %v from a cut-off backup", got)
	}
}

// What can't be run in a test (Neon, docker, the local API) is held to
// its rules here.
func TestRecoverScriptRules(t *testing.T) {
	parallelTest(t)
	d := readRepoFile(t, "scripts/recover.sh")
	for _, want := range []string{
		"set -euo pipefail",
		`if ! sql="$(decrypt_ | to_sql_ | awk -f "$here/restore-filter.awk")"; then`,
		"-v ON_ERROR_STOP=1 --single-transaction",
		`target="$(verified_url "$url" /etc/goflow-ca.crt)"`,
		"*-pooler.*",
		`if [ "$objects" != 0 ] || [ "$migrations" != 0 ]; then`,
		`if [ -z "$typed" ] || [ "$typed" != "$endpoint" ]; then`,
		"PGPASSWORD=\"$pw\" PGURL=\"$target_nopw\" docker run --rm -i -e PGPASSWORD -e PGURL",
		"-p \"127.0.0.1:$api_port:8080\"",
		"-e GOFLOW_REGISTRATION=closed",
		"the target is missing structure the dump has",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("recover.sh lacks %q", want)
		}
	}
	// The emptiness check comes before the key is asked for and long before
	// the load; the load comes after the filter.
	order := []string{`stop "the target isn't empty`, `read -rsp "age private key`, `restore-filter.awk")"; then`, `--single-transaction >/dev/null; then`}
	for i := 1; i < len(order); i++ {
		if a, b := strings.Index(d, order[i-1]), strings.Index(d, order[i]); a < 0 || b < 0 || a > b {
			t.Errorf("recover.sh: %q must come before %q", order[i-1], order[i])
		}
	}
	if strings.Count(d, "--single-transaction") != 1 {
		t.Error("recover.sh loads into the target in more than one place")
	}
	// Secrets are used only where they must be: never echoed, never on a
	// command line, never written to a file.
	for _, banned := range []string{`echo "$url`, `echo "$target`, `echo "$pw`, `echo "$skey`, `echo "$identity`, "--output", "tee ", "ON_ERROR_STOP=0", "set -x", `psql "$target`, `--dbname="$target"`} {
		if strings.Contains(d, banned) {
			t.Errorf("recover.sh contains %q", banned)
		}
	}
	var uses []string
	for _, line := range strings.Split(d, "\n") {
		if strings.Contains(line, "$skey") || strings.Contains(line, "${skey") {
			uses = append(uses, strings.TrimSpace(line))
		}
	}
	if len(uses) != 3 || !strings.HasPrefix(uses[0], "skey=${skey//") || !strings.HasPrefix(uses[1], `key_id=$(secrets_key_id "$skey")`) || !strings.Contains(uses[2], `GOFLOW_SECRETS_KEY="$skey" docker run -d`) {
		t.Errorf("recover.sh uses the secrets key in other ways than cleaning, checking and handing it to docker (%d lines): %s", len(uses), strings.Join(uses, " | "))
	}
	uses = nil
	for _, line := range strings.Split(d, "\n") {
		if strings.Contains(line, "$identity") {
			uses = append(uses, strings.TrimSpace(line))
		}
	}
	if len(uses) != 2 || !strings.HasPrefix(uses[0], `[[ "$identity" =~ ^AGE-SECRET-KEY-1`) || !strings.HasPrefix(uses[1], "decrypt_() { age --decrypt --identity <(") {
		t.Errorf("recover.sh uses the age key in other ways than checking it and piping it to age (%d lines): %s", len(uses), strings.Join(uses, " | "))
	}
}

// keyid.sh must compute exactly the ID the API logs, and refuse what the
// API refuses, without printing the key.
func TestSecretsKeyIDMatchesTheAPI(t *testing.T) {
	parallelTest(t)
	run := func(key string) (string, string, error) {
		cmd := exec.Command("bash", "../../scripts/secrets-key-id.sh")
		cmd.Stdin = strings.NewReader(key + "\n")
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		return o.String(), e.String(), err
	}
	for i := 0; i < 5; i++ {
		b := make([]byte, secrets.KeySize)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		key := base64.StdEncoding.EncodeToString(b)
		kr, err := secrets.ParseKeys(key, "")
		if err != nil {
			t.Fatal(err)
		}
		out, stderr, err := run(" " + key[:20] + "\n" + key[20:] + " ") // pasted with a stray break
		if err != nil {
			t.Fatalf("secrets-key-id.sh: %v\n%s", err, stderr)
		}
		if got := strings.TrimSpace(out); got != "key_id="+kr.CurrentID() {
			t.Errorf("script says %q, the API logs key_id=%s", got, kr.CurrentID())
		}
		if strings.Contains(out+stderr, key) || strings.Contains(out+stderr, key[:20]) {
			t.Error("secrets-key-id.sh printed the key")
		}
	}
	short := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7, 9}, 12))
	long := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7, 9}, 24))
	same := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	urlsafe := strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff}, 16)))
	for _, bad := range []string{"", "hello", short, long, same, urlsafe, strings.TrimSuffix(same, "=")} {
		if _, err := secrets.ParseKeys(bad, ""); err == nil {
			t.Fatalf("the API accepts %q; this test's bad keys are wrong", bad)
		}
		out, stderr, err := run(bad)
		if err == nil || out != "" {
			t.Errorf("secrets-key-id.sh accepted a key the API refuses (%d characters): %q", len(bad), out)
		}
		if len(bad) > 8 && strings.Contains(stderr, bad) {
			t.Error("secrets-key-id.sh printed the refused key")
		}
	}
}

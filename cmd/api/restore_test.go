package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/database"
)

// The restore drill (Task 69b) checks the whole plain SQL dump with
// scripts/restore-filter.awk before psql sees any of it. drillDump is
// shaped like pg_restore -f - output from Neon: one pg_attver option
// stock PostgreSQL rejects, and a data row that looks like one.
const drillDump = `--
-- PostgreSQL database dump
--

SET statement_timeout = 0;
SET client_encoding = 'UTF8';

CREATE TABLE public.drill_parent (
    id integer NOT NULL,
    CONSTRAINT drill_parent_id_check CHECK ((id > 0))
);

ALTER TABLE ONLY public.drill_parent ALTER COLUMN id SET (pg_attver=34043088);

CREATE TABLE public.drill_child (
    id integer NOT NULL,
    parent_id integer NOT NULL
);

CREATE TABLE public.drill_note (
    note text NOT NULL
);

COPY public.drill_parent (id) FROM stdin;
1
2
\.

COPY public.drill_child (id, parent_id) FROM stdin;
1	1
\.

COPY public.drill_note (note) FROM stdin;
ALTER TABLE ONLY public.jobs ALTER COLUMN type SET (pg_attver=1);
\.

ALTER TABLE ONLY public.drill_parent
    ADD CONSTRAINT drill_parent_pkey PRIMARY KEY (id);

CREATE INDEX drill_child_parent_idx ON public.drill_child USING btree (parent_id);

ALTER TABLE ONLY public.drill_child
    ADD CONSTRAINT drill_child_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.drill_parent(id);

--
-- PostgreSQL database dump complete
--

`

const attverLine = "ALTER TABLE ONLY public.drill_parent ALTER COLUMN id SET (pg_attver=34043088);"

// cutOffDump is drillDump as a stream cut short after the data: the
// tables and rows have arrived, the constraints and the trailer haven't.
func cutOffDump(t *testing.T) string {
	t.Helper()
	i := strings.Index(drillDump, "ALTER TABLE ONLY public.drill_parent\n    ADD")
	if i < 0 {
		t.Fatal("drillDump changed: no constraint section to cut at")
	}
	return drillDump[:i]
}

// runFilter runs restore-filter.awk on in, as restore-drill.sh does.
func runFilter(t *testing.T, in string) (stdout, stderr string, err error) {
	t.Helper()
	if _, lerr := exec.LookPath("awk"); lerr != nil {
		t.Fatal("awk is not installed: restore-drill.sh needs it")
	}
	cmd := exec.Command("awk", "-f", "../../scripts/restore-filter.awk")
	cmd.Stdin = strings.NewReader(in)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

func TestRestoreFilterRemovesOnlyNeonsColumnOption(t *testing.T) {
	parallelTest(t)
	out, stderr, err := runFilter(t, drillDump)
	if err != nil {
		t.Fatalf("filter refused a complete dump: %v\n%s", err, stderr)
	}
	if strings.Contains(out, attverLine) {
		t.Error("the pg_attver line is still in the SQL")
	}
	if !strings.Contains(stderr, "removed: "+attverLine) {
		t.Errorf("the removed line isn't printed for audit; stderr:\n%s", stderr)
	}
	// The data row that looks like an option is data: kept, untouched.
	if !strings.Contains(out, "COPY public.drill_note (note) FROM stdin;\nALTER TABLE ONLY public.jobs ALTER COLUMN type SET (pg_attver=1);\n\\.\n") {
		t.Error("a COPY row was changed or removed")
	}
	if want := "-- goflow-drill: removed=1 foreign_keys=1 indexes=2 checks=1\n"; !strings.HasSuffix(out, want) {
		t.Errorf("summary line: want %q at the end, got the last 80 bytes %q", want, out[max(0, len(out)-80):])
	}
	// Everything else arrives unchanged and in order.
	if got, want := strings.TrimSuffix(out, "-- goflow-drill: removed=1 foreign_keys=1 indexes=2 checks=1\n"), strings.Replace(drillDump, attverLine+"\n", "", 1); got != want {
		t.Error("the filter changed more than the pg_attver line")
	}
}

// A dump cut short must stop the drill before PostgreSQL sees any of it:
// psql --single-transaction commits whatever it has read when its input
// ends (FAILURES.md, case 65).
func TestRestoreFilterRefusesACutOffDump(t *testing.T) {
	parallelTest(t)
	out, stderr, err := runFilter(t, cutOffDump(t))
	if err == nil {
		t.Error("the filter passed a dump with no \"dump complete\" trailer")
	}
	if out != "" {
		t.Errorf("the filter printed %d bytes of SQL for a refused dump; it must print none", len(out))
	}
	if err != nil && !strings.Contains(stderr, "the dump was cut off") {
		t.Errorf("stderr doesn't say why:\n%s", stderr)
	}
}

func TestRestoreFilterFailsClosed(t *testing.T) {
	parallelTest(t)
	cases := []struct{ name, from, to, why string }{
		{"another column option", attverLine, "ALTER TABLE ONLY public.drill_parent ALTER COLUMN id SET (n_distinct=5);", "unexpected column option"},
		{"pg_attver with more", attverLine, "ALTER TABLE ONLY public.drill_parent ALTER COLUMN id SET (pg_attver=1, n_distinct=5);", "unexpected shape"},
		{"pg_attver elsewhere", "SET client_encoding = 'UTF8';", "SET client_encoding = 'UTF8'; -- pg_attver", "unexpected shape"},
		{"no header", "-- PostgreSQL database dump\n", "-- something else\n", "doesn't start like pg_dump"},
		{"cut inside COPY", "1\t1\n\\.\n", "1\t1\n", "ends inside a COPY"},
		{"SQL after the trailer", "-- PostgreSQL database dump complete\n--\n", "-- PostgreSQL database dump complete\n--\nDROP TABLE public.drill_parent;\n", "after the \"dump complete\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := strings.Replace(drillDump, c.from, c.to, 1)
			if in == drillDump {
				t.Fatal("the change didn't apply")
			}
			if c.name == "cut inside COPY" {
				in = in[:strings.Index(in, "1\t1\n")+len("1\t1\n")]
			}
			out, stderr, err := runFilter(t, in)
			if err == nil || out != "" {
				t.Errorf("passed (err %v, %d bytes of SQL); want refused with no SQL", err, len(out))
			}
			if !strings.Contains(stderr, c.why) {
				t.Errorf("stderr lacks %q:\n%s", c.why, stderr)
			}
		})
	}
}

// emptyDatabase creates a database of its own for this test, named
// goflow_restore_drill_test_<random> (never goflow, goflow_test or the
// drill's own), and drops it when the test ends.
func emptyDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	base := os.Getenv("GOFLOW_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("GOFLOW_TEST_DATABASE_URL not set; skipping test that needs PostgreSQL")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	admin.SetMaxOpenConns(1)
	name := randomDatabaseName("goflow_restore_drill_test_")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := withDatabaseName(base, name)
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		admin.Close()
	})
	return u, db
}

// load runs psql the way restore-drill.sh does.
func load(t *testing.T, psql, url, sqlText string) error {
	t.Helper()
	cmd := exec.Command(psql, url, "-X", "-q", "-v", "ON_ERROR_STOP=1", "--single-transaction")
	cmd.Stdin = strings.NewReader(sqlText)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("psql: %v\n%s", err, out)
	}
	return err
}

func tableCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM pg_tables WHERE schemaname = 'public'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func needPsql(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql is not installed; the filter tests above still run")
	}
	return p
}

// Why the trailer check exists, shown on a database of the test's own:
// fed the cut-off dump directly, psql --single-transaction reaches the end
// of its input and commits the tables and rows that arrived.
func TestPsqlCommitsACutOffDump(t *testing.T) {
	parallelTest(t)
	psql := needPsql(t)
	u, db := emptyDatabase(t)
	sqlText := strings.Replace(cutOffDump(t), attverLine+"\n", "", 1)
	if err := load(t, psql, u, sqlText); err != nil {
		t.Fatalf("psql refused the cut-off SQL: %v", err)
	}
	if n := tableCount(t, db); n != 3 {
		t.Fatalf("psql committed %d tables; this test expects the 3 the cut-off dump creates", n)
	}
}

// The drill's order, on a database of the test's own: the filter checks
// the whole dump, and psql runs only if it passed. A cut-off dump leaves
// the database empty (FAILURES.md, case 65).
func TestACutOffDumpRestoresNothing(t *testing.T) {
	parallelTest(t)
	psql := needPsql(t)
	u, db := emptyDatabase(t)
	out, _, err := runFilter(t, cutOffDump(t))
	if err == nil {
		t.Error("the filter passed a cut-off dump; loading it as restore-drill.sh would")
		_ = load(t, psql, u, out)
	}
	if n := tableCount(t, db); n != 0 {
		t.Errorf("a cut-off dump was committed: %d tables restored", n)
	}
}

// A complete dump restores whole, through the same filter and psql flags.
func TestACompleteDumpRestores(t *testing.T) {
	parallelTest(t)
	psql := needPsql(t)
	u, db := emptyDatabase(t)
	out, stderr, err := runFilter(t, drillDump)
	if err != nil {
		t.Fatalf("filter: %v\n%s", err, stderr)
	}
	if err := load(t, psql, u, out); err != nil {
		t.Fatalf("psql: %v", err)
	}
	var fks, idx, checks, notes int
	if err := db.QueryRow(`SELECT
		(SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public' AND c.contype = 'f'),
		(SELECT count(*) FROM pg_indexes WHERE schemaname = 'public'),
		(SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public' AND c.contype = 'c'),
		(SELECT count(*) FROM drill_note WHERE note LIKE 'ALTER TABLE ONLY%')`).Scan(&fks, &idx, &checks, &notes); err != nil {
		t.Fatal(err)
	}
	if fks != 1 || idx != 2 || checks != 1 || notes != 1 {
		t.Errorf("restored foreign keys %d, indexes %d, checks %d, note rows %d; want 1, 2, 1, 1 (the filter's own counts)", fks, idx, checks, notes)
	}
}

// The drill never targets another database, and the backup and the row
// counts always verify the server's name (Task 69b).
func TestRestoreDrillScriptRules(t *testing.T) {
	parallelTest(t)
	d := readRepoFile(t, "scripts/restore-drill.sh")
	for _, want := range []string{
		"set -euo pipefail",
		`if ! sql="$(decrypt_ | to_sql_ | awk -f "$here/restore-filter.awk")"; then`,
		"-v ON_ERROR_STOP=1 --single-transaction",
		"db=goflow_restore_drill",
		"must be a local server",
		"must start with goflow_restore_drill",
		"already exists",
		"is missing structure the dump has",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("restore-drill.sh lacks %q", want)
		}
	}
	// psql gets the SQL only after the check, from the variable.
	if strings.Index(d, `printf '%s\n' "$sql" | load_`) < strings.Index(d, "restore-filter.awk\")\"; then") {
		t.Error("restore-drill.sh loads the SQL before checking it")
	}
	if strings.Count(d, "| load_") != 1 || strings.Contains(d, "| psql") {
		t.Error("restore-drill.sh pipes into psql somewhere other than after the check")
	}
	for _, banned := range []string{"--output", "ON_ERROR_STOP=0", "ON_ERROR_STOP=off", "> \"$", "tee "} {
		if strings.Contains(d, banned) {
			t.Errorf("restore-drill.sh contains %q (writes the dump to disk, or ignores errors)", banned)
		}
	}

	// An age private key reaches age through a pipe, never a file, and is
	// dropped as soon as the dump is decrypted (Phase 18; FAILURES.md,
	// case 67).
	for _, want := range []string{
		`read -rsp "age private key, AGE-SECRET-KEY-1... (not shown): " identity`,
		`decrypt_() { age --decrypt --identity <(printf '%s\n' "$identity") "$file"; }`,
		"is neither .gpg nor .age",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("restore-drill.sh lacks %q", want)
		}
	}
	if n := strings.Count(d, "unset identity"); n != 3 {
		t.Errorf("restore-drill.sh unsets the age key %d times; want 3 (bad key, failed check, after decrypting)", n)
	}
	// The key is used in exactly two places: checked, then piped to age.
	// Anything else (a file, an echo, an export) is a new way for it to leak.
	var uses []string
	for _, line := range strings.Split(d, "\n") {
		if strings.Contains(line, "$identity") || strings.Contains(line, "${identity") || strings.Contains(line, "export identity") {
			uses = append(uses, strings.TrimSpace(line))
		}
	}
	if len(uses) != 2 || !strings.HasPrefix(uses[0], `[[ "$identity" =~ ^AGE-SECRET-KEY-1`) || !strings.HasPrefix(uses[1], "decrypt_() { age --decrypt --identity <(") {
		t.Errorf("restore-drill.sh uses the age key in other ways than checking it and piping it to age (%d lines): %s", len(uses), strings.Join(uses, " | "))
	}

	for _, f := range []string{"scripts/backup.sh", "scripts/count-rows.sh"} {
		s := readRepoFile(t, f)
		if !strings.Contains(s, `. "$(dirname "$0")/pgurl.sh"`) || strings.Count(s, "verified_url \"$url\"") != 2 {
			t.Errorf("%s doesn't build both of its URLs with verified_url", f)
		}
		if strings.Contains(s, "sslrootcert=/etc") {
			t.Errorf("%s adds sslrootcert itself, without verify-full", f)
		}
	}
}

func TestVerifiedURL(t *testing.T) {
	parallelTest(t)
	cases := []struct{ in, want string }{
		{"postgresql://u:p@ep-x.eu-central-1.aws.neon.tech/neondb?sslmode=require&channel_binding=require",
			"postgresql://u:p@ep-x.eu-central-1.aws.neon.tech/neondb?channel_binding=require&sslmode=verify-full&sslrootcert=/ca.crt"},
		{"postgres://u:p@db.example.com:5432/x", "postgres://u:p@db.example.com:5432/x?sslmode=verify-full&sslrootcert=/ca.crt"},
		{"postgres://u:p@db.example.com/x?sslmode=disable&sslrootcert=/tmp/evil", "postgres://u:p@db.example.com/x?sslmode=verify-full&sslrootcert=/ca.crt"},
		{"postgres://u:p@db.example.com/x?sslmode=verify-ca&sslmode=require", "postgres://u:p@db.example.com/x?sslmode=verify-full&sslrootcert=/ca.crt"},
		{"postgres://goflow:goflow@127.0.0.1:5432/postgres?sslmode=disable", "postgres://goflow:goflow@127.0.0.1:5432/postgres?sslmode=disable"},
		{"postgres://goflow:goflow@localhost/goflow", "postgres://goflow:goflow@localhost/goflow"},
		{"postgres://u:p@localhost.evil.com/x", "postgres://u:p@localhost.evil.com/x?sslmode=verify-full&sslrootcert=/ca.crt"},
		{"postgres://u:p@127.0.0.1.nip.io/x", "postgres://u:p@127.0.0.1.nip.io/x?sslmode=verify-full&sslrootcert=/ca.crt"},
	}
	for _, c := range cases {
		cmd := exec.Command("bash", "-c", `set -euo pipefail; . ../../scripts/pgurl.sh; verified_url "$1" /ca.crt`, "bash", c.in)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.in, err, out)
		}
		if got := strings.TrimSuffix(string(out), "\n"); got != c.want {
			t.Errorf("verified_url(%s)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

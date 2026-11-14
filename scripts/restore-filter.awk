# Checks a plain SQL dump (pg_restore -f -) before restore-drill.sh sends
# any of it to psql (Task 69b). Plain POSIX awk: gawk and mawk both run it.
#
# Reads the whole dump into memory and prints nothing until all of it has
# passed. Then prints it to stdout, minus the removed lines, followed by
# one comment line with what it counted:
#   -- goflow-drill: removed=N foreign_keys=N indexes=N checks=N
# On any failure it prints the reasons to stderr, nothing to stdout, and
# exits 1.
#
# The checks:
#   - the dump starts as pg_dump's output does and ends with its trailer,
#     "-- PostgreSQL database dump complete". Without the trailer, gpg or
#     pg_restore stopped early, and psql --single-transaction would commit
#     whatever part arrived (FAILURES.md, case 65)
#   - Neon's PostgreSQL writes a column option stock PostgreSQL rejects:
#       ALTER TABLE ONLY public.jobs ALTER COLUMN type SET (pg_attver=34043088);
#     Lines of exactly that shape are removed and printed to stderr.
#     Any other "ALTER COLUMN ... SET (" line, or pg_attver anywhere else,
#     stops the drill: an option nobody has looked at is never dropped
#     silently
#   - rows inside COPY ... FROM stdin; are data, so they are passed through
#     untouched and never matched
BEGIN {
	n = 0; removed = 0; fks = 0; indexes = 0; checks = 0
	incopy = 0; header = 0; trailer = 0; after = 0; bad = 0
	attver = "^ALTER TABLE ONLY [a-z_][a-z0-9_]*\\.[a-z_][a-z0-9_]* ALTER COLUMN [a-z_][a-z0-9_]* SET \\(pg_attver=[0-9]+\\);$"
}

incopy {
	if ($0 == "\\.") incopy = 0
	line[++n] = $0
	next
}

{
	if (NR <= 3 && $0 == "-- PostgreSQL database dump") header = 1
	if (trailer && $0 != "" && $0 != "--" && $0 !~ /^\\unrestrict [A-Za-z0-9]+$/) after = 1
	if ($0 == "-- PostgreSQL database dump complete") trailer = 1
}

$0 ~ attver {
	removed++
	print "removed: " $0 > "/dev/stderr"
	next
}

/pg_attver/ {
	bad++
	print "stop: pg_attver in a line of an unexpected shape (line " NR "): " $0 > "/dev/stderr"
	next
}

/ALTER COLUMN .* SET \(/ {
	bad++
	print "stop: unexpected column option (line " NR "): " $0 > "/dev/stderr"
	next
}

/^COPY .* FROM stdin;$/ { incopy = 1 }
/^    ADD CONSTRAINT .* FOREIGN KEY / { fks++ }
/^    ADD CONSTRAINT .* (PRIMARY KEY|UNIQUE) / { indexes++ }
/^CREATE (UNIQUE )?INDEX / { indexes++ }
/^    CONSTRAINT .* CHECK / { checks++ }

{ line[++n] = $0 }

END {
	if (!header) { bad++; print "stop: this doesn't start like pg_dump's output" > "/dev/stderr" }
	if (incopy) { bad++; print "stop: the dump ends inside a COPY: it was cut off" > "/dev/stderr" }
	if (!trailer) { bad++; print "stop: no \"-- PostgreSQL database dump complete\" line: the dump was cut off" > "/dev/stderr" }
	if (after) { bad++; print "stop: SQL after the \"dump complete\" line" > "/dev/stderr" }
	if (bad) {
		print "stop: " bad " problem(s); nothing was sent to PostgreSQL" > "/dev/stderr"
		exit 1
	}
	for (i = 1; i <= n; i++) print line[i]
	print "-- goflow-drill: removed=" removed " foreign_keys=" fks " indexes=" indexes " checks=" checks
}

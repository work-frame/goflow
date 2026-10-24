package database

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// migrationFiles holds every .sql file in migrations/, compiled into the binary.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey identifies GoFlow's migration lock in Postgres.
// Any fixed number works, as long as every GoFlow process uses the same one.
const migrationLockKey int64 = 7_281_904_550

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies every embedded migration that has not run yet, in version order,
// and returns the versions it applied. It is safe to run from several processes at once.
func Migrate(ctx context.Context, db *sql.DB) ([]int, error) {
	return migrate(ctx, db, migrationFiles)
}

func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) ([]int, error) {
	migrations, err := loadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	if err := ensureMigrationsTable(ctx, db); err != nil {
		return nil, err
	}

	var applied []int
	for _, m := range migrations {
		ran, err := applyMigration(ctx, db, m)
		if err != nil {
			return applied, err
		}
		if ran {
			applied = append(applied, m.version)
		}
	}
	return applied, nil
}

// loadMigrations reads migrations/NNNN_description.sql files and sorts them by version.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("database: read migrations: %w", err)
	}

	var migrations []migration
	seen := make(map[int]string)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || path.Ext(name) != ".sql" {
			continue
		}

		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil || version <= 0 {
			return nil, fmt.Errorf("database: migration %q: name must look like 0001_description.sql", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("database: migrations %q and %q share version %d", other, name, version)
		}
		seen[version] = name

		body, err := fs.ReadFile(fsys, path.Join("migrations", name))
		if err != nil {
			return nil, fmt.Errorf("database: read migration %q: %w", name, err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(body)})
	}

	// Sort by number, not by file name: as text, "10_x.sql" comes before "9_x.sql".
	slices.SortFunc(migrations, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	return migrations, nil
}

func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	return inLockedTx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				version    INTEGER     PRIMARY KEY,
				applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
			)`)
		if err != nil {
			return fmt.Errorf("database: create schema_migrations: %w", err)
		}
		return nil
	})
}

// applyMigration runs one migration and records it, in the same transaction.
// Either both happen or neither does. It reports whether the migration ran.
func applyMigration(ctx context.Context, db *sql.DB, m migration) (bool, error) {
	ran := false
	err := inLockedTx(ctx, db, func(tx *sql.Tx) error {
		var done bool
		err := tx.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", m.version,
		).Scan(&done)
		if err != nil {
			return fmt.Errorf("database: check migration %s: %w", m.name, err)
		}
		if done {
			return nil
		}

		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("database: apply migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations (version) VALUES ($1)", m.version,
		); err != nil {
			return fmt.Errorf("database: record migration %s: %w", m.name, err)
		}
		ran = true
		return nil
	})
	return ran, err
}

// inLockedTx runs fn in a transaction that holds GoFlow's migration lock.
// Other processes calling it wait until this transaction ends.
// The lock is released automatically on commit or rollback.
func inLockedTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer tx.Rollback() // does nothing once Commit has succeeded

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("database: migration lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

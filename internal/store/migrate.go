package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
)

// migration is one forward-only schema change. Versions start at 1 and are
// contiguous; the database's applied version is tracked in PRAGMA user_version.
//
// Each migration file runs inside a transaction managed by applyMigration, so
// files must not contain BEGIN/COMMIT. SQLite ignores PRAGMA foreign_keys
// inside a transaction: if a future table rebuild (the 12-step ALTER
// procedure) must drop a table that other tables reference, applyMigration
// first needs to disable foreign keys on a dedicated connection and run
// PRAGMA foreign_key_check before committing.
type migration struct {
	version int
	name    string
	sql     string
}

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// loadMigrations reads every *.sql file at the root of fsys. File names must
// look like 0001_create_articles.sql, and versions must run 1..N with no gaps
// or duplicates.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, name := range names {
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("migration %q: name must match NNNN_description.sql", name)
		}
		version, _ := strconv.Atoi(m[1]) // regexp guarantees digits
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: version, name: name, sql: string(body)})
	}
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q: expected version %d (versions must be contiguous from 1)", m.name, i+1)
		}
	}
	return ms, nil
}

// migrate applies every migration newer than the database's user_version, each
// in its own transaction. It is safe to run repeatedly and concurrently: the
// version is re-read under the write lock before each migration is applied.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}

	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	if current > len(ms) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", current, len(ms))
	}

	for _, m := range ms[current:] {
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	if current >= m.version {
		return nil // applied by a concurrent migrator
	}
	if current != m.version-1 {
		return fmt.Errorf("database is at version %d, cannot apply version %d", current, m.version)
	}

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	// PRAGMA does not accept bound parameters; version is an int we parsed.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

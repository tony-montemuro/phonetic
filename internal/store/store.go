// Package store is phonetic's SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

func embeddedMigrationsFS() (fs.FS, error) {
	return fs.Sub(embeddedMigrations, "migrations")
}

// Store wraps the application database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies any
// pending schema migrations. The parent directory must already exist.
//
// Concurrent Opens of an existing database are safe. Concurrent creation of a
// new file is not: the initial switch to WAL may fail with SQLITE_BUSY.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("open %s: database path must not contain '?'", path)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	migrations, err := embeddedMigrationsFS()
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(ctx, db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// dsn builds a modernc.org/sqlite connection string. The connection
// parameters are applied by the driver to every pooled connection:
//   - WAL lets readers proceed while a writer is active; synchronous=NORMAL is
//     the recommended (durable across application crashes) pairing for WAL.
//   - busy_timeout makes concurrent writers wait instead of failing with
//     SQLITE_BUSY. Note: the driver's busy wait does not observe context
//     cancellation, so a blocked statement can wait the full timeout.
//   - txlock=immediate takes the write lock at BEGIN, avoiding the
//     deadlock-prone read-to-write lock upgrade of deferred transactions.
//     Read-only transactions should pass sql.TxOptions{ReadOnly: true}; the
//     driver then issues a plain BEGIN and they don't queue behind writers.
//
// Writers are serialized by SQLite and coordinated through busy_timeout rather
// than a single-connection writer pool. That relies on write transactions
// being short: never hold one open across network, LLM or TTS calls.
func dsn(path string) string {
	q := url.Values{}
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "1")
	q.Set("_txlock", "immediate")
	// The driver splits the name at the first '?', so path must not contain one.
	return path + "?" + q.Encode()
}

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

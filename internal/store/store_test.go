package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func openRaw(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "test.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "phonetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustSub(t *testing.T) fs.FS {
	t.Helper()
	sub, err := embeddedMigrationsFS()
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func version(t *testing.T, db *sql.DB) int {
	t.Helper()
	v, err := userVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", name).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestOpenAppliesEmbeddedMigrationsIdempotently(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "phonetic.db")

	want, err := loadMigrations(mustSub(t))
	if err != nil {
		t.Fatal(err)
	}

	for i := range 2 { // second Open must be a no-op
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		if got := version(t, s.db); got != len(want) {
			t.Errorf("Open #%d: user_version = %d, want %d", i+1, got, len(want))
		}
		if !tableExists(t, s.db, "articles") {
			t.Errorf("Open #%d: articles table missing", i+1)
		}
		if err := s.Ping(ctx); err != nil {
			t.Errorf("Ping: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenConnectionSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "phonetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Hold several connections at once so settings are checked on more than
	// the first pooled connection.
	var conns []*sql.Conn
	for range 3 {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		for pragma, want := range map[string]string{
			"journal_mode": "wal",
			"synchronous":  "1", // NORMAL
			"busy_timeout": "5000",
			"foreign_keys": "1",
		} {
			var got string
			if err := c.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("conn %d: PRAGMA %s = %q, want %q", i, pragma, got, want)
			}
		}
		c.Close()
	}
}

func TestOpenRejectsQuestionMarkInPath(t *testing.T) {
	t.Parallel()
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "a?b.db"))
	if err == nil {
		t.Fatal("expected error")
	}
}

// Concurrent Opens of an existing, unmigrated database must race safely on
// migrations. (Concurrent *creation* of a brand-new file is not supported: the
// initial switch to WAL can fail with SQLITE_BUSY without consulting
// busy_timeout. phonetic is a single process, so that never happens.)
func TestOpenConcurrentMigrations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "phonetic.db")

	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Ping(); err != nil { // creates the file in WAL mode at version 0
		t.Fatal(err)
	}
	raw.Close()

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(ctx, path)
			if err != nil {
				errs <- err
				return
			}
			errs <- s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}
}

// _txlock=immediate: a second write transaction must wait for the first rather
// than start (and later fail on lock upgrade).
func TestWriteTransactionsAreImmediate(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()

	tx1, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()

	// The driver's busy wait ignores context cancellation, so shorten
	// busy_timeout on a dedicated connection instead of using a deadline.
	c2, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.ExecContext(ctx, "PRAGMA busy_timeout = 50"); err != nil {
		t.Fatal(err)
	}
	tx2, err := c2.BeginTx(ctx, nil)
	if err == nil {
		tx2.Rollback()
		t.Fatal("second write transaction began while the first held the lock")
	}
	// Return the connection to the pool with its normal timeout.
	if _, err := c2.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		t.Fatal(err)
	}

	// Read-only transactions use a plain BEGIN and are not blocked.
	ro, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only BeginTx while writer active: %v", err)
	}
	var n int
	if err := ro.QueryRowContext(ctx, "SELECT count(*) FROM articles").Scan(&n); err != nil {
		t.Fatal(err)
	}
	ro.Rollback()
}

func TestArticlesSchemaConstraints(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()

	insert := func(url, status string, importance any) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO articles (url, status, image_importance, next_attempt_at, created_at, updated_at)
			VALUES (?, ?, ?, 0, 0, 0)`, url, status, importance)
		return err
	}

	if err := insert("https://example.com/a", "queued", nil); err != nil {
		t.Fatalf("valid insert: %v", err)
	}

	bad := map[string]error{
		"duplicate url":      insert("https://example.com/a", "queued", nil),
		"unknown status":     insert("https://example.com/b", "redy", nil),
		"importance too big": insert("https://example.com/c", "queued", 4),
		"strict type":        insert("https://example.com/d", "queued", "high"),
	}
	for name, err := range bad {
		if err == nil {
			t.Errorf("%s: insert succeeded, want constraint error", name)
		}
	}
}

// AUTOINCREMENT: deleting the newest row must not let its id be reused, since
// ids key feed GUIDs and artifact directories.
func TestArticleIDsAreNeverReused(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()

	insert := func(url string) int64 {
		t.Helper()
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO articles (url, status, next_attempt_at, created_at, updated_at)
			VALUES (?, 'queued', 0, 0, 0)`, url)
		if err != nil {
			t.Fatal(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	first := insert("https://example.com/1")
	if _, err := s.db.ExecContext(ctx, "DELETE FROM articles WHERE id = ?", first); err != nil {
		t.Fatal(err)
	}
	if second := insert("https://example.com/2"); second == first {
		t.Errorf("id %d was reused after delete", first)
	}
}

func TestMigrateAppliesInOrderAndResumes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)

	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);")},
	}
	if err := migrate(ctx, db, fsys); err != nil {
		t.Fatal(err)
	}
	if got := version(t, db); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}

	// A new binary ships an additional migration (multi-statement, and it
	// depends on table a from the first).
	fsys["0002_b.sql"] = &fstest.MapFile{Data: []byte(`
		CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id));
		CREATE INDEX b_a ON b (a_id);`)}
	if err := migrate(ctx, db, fsys); err != nil {
		t.Fatal(err)
	}
	if got := version(t, db); got != 2 {
		t.Fatalf("version = %d, want 2", got)
	}
	if !tableExists(t, db, "b") {
		t.Fatal("table b missing")
	}
}

func TestMigrateFailureRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)

	fsys := fstest.MapFS{
		"0001_ok.sql":  {Data: []byte("CREATE TABLE ok (id INTEGER PRIMARY KEY);")},
		"0002_bad.sql": {Data: []byte("CREATE TABLE half (id INTEGER PRIMARY KEY); THIS IS NOT SQL;")},
	}
	err := migrate(ctx, db, fsys)
	if err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("err = %v, want failure naming 0002_bad.sql", err)
	}
	if got := version(t, db); got != 1 {
		t.Errorf("version = %d, want 1 (failed migration must not bump it)", got)
	}
	if tableExists(t, db, "half") {
		t.Error("partial migration was not rolled back")
	}
}

func TestMigrateRejectsNewerDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if _, err := db.Exec("PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 1;")}}
	err := migrate(ctx, db, fsys)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v, want 'newer than this binary' error", err)
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fsys fstest.MapFS
	}{
		{"gap", fstest.MapFS{"0001_a.sql": {}, "0003_c.sql": {}}},
		{"duplicate version", fstest.MapFS{"0001_a.sql": {}, "0001_b.sql": {}}},
		{"starts at 2", fstest.MapFS{"0002_a.sql": {}}},
		{"bad name", fstest.MapFS{"1_a.sql": {}}},
		{"uppercase name", fstest.MapFS{"0001_Create.sql": {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadMigrations(tt.fsys); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestLoadMigrationsIgnoresNonSQL(t *testing.T) {
	t.Parallel()
	ms, err := loadMigrations(fstest.MapFS{"0001_a.sql": {}, "README.md": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Errorf("got %d migrations, want 1", len(ms))
	}
}

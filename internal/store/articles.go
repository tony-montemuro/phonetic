package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned when an article does not exist.
	ErrNotFound = errors.New("article not found")
	// ErrLeaseLost is returned when a lease-guarded write finds the lease no
	// longer held (expired and reclaimed, or released).
	ErrLeaseLost = errors.New("lease lost")
	// ErrNoWork is returned by ClaimArticle when nothing is runnable.
	ErrNoWork = errors.New("no runnable article")
)

// Status is an article's position in the pipeline. Non-terminal statuses name
// the stage the article is waiting for or running.
type Status string

const (
	StatusQueued       Status = "queued"
	StatusFetching     Status = "fetching"
	StatusExtracting   Status = "extracting"
	StatusScripting    Status = "scripting"
	StatusSynthesizing Status = "synthesizing"
	StatusReady        Status = "ready"
	StatusFailed       Status = "failed"
)

// IsStage reports whether s is a pipeline stage (a status work runs in).
func (s Status) IsStage() bool {
	for _, st := range stageOrder {
		if s == st {
			return true
		}
	}
	return false
}

// next returns the status after completing stage s.
func (s Status) next() (Status, error) {
	for i, st := range stageOrder {
		if s == st {
			if i == len(stageOrder)-1 {
				return StatusReady, nil
			}
			return stageOrder[i+1], nil
		}
	}
	return "", fmt.Errorf("status %q is not a pipeline stage", s)
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusReady, StatusFailed:
		return true
	}
	return s.IsStage()
}

// stageOrder is the pipeline's stage sequence; each stage advances to the
// next, and the last advances to StatusReady.
var stageOrder = []Status{StatusFetching, StatusExtracting, StatusScripting, StatusSynthesizing}

// Stages returns the pipeline stages in order.
func Stages() []Status { return append([]Status(nil), stageOrder...) }

// Article is one row of the articles table. Zero values stand for NULL.
type Article struct {
	ID            int64
	URL           string
	Status        Status
	FailedStage   Status
	Error         string
	Attempts      int
	NextAttemptAt time.Time
	// LockedUntil is the worker lease expiry; LeaseID identifies the lease
	// for compare-and-set writes. Both are zero when not leased.
	LockedUntil time.Time
	LeaseID     int64

	Title           string
	Author          string
	SiteName        string
	PublishedAt     time.Time
	WordCount       int
	ImageCount      int
	ImageImportance *int // nil when not yet analyzed
	ImageNotes      string

	AudioBytes int64
	Duration   time.Duration

	CreatedAt time.Time
	UpdatedAt time.Time
	ReadyAt   time.Time
}

// ListOptions filter ListArticles.
type ListOptions struct {
	Status Status // empty: any status
	Limit  int    // <= 0: 100
}

// Lease identifies a held lease: the article, the stage it was claimed for,
// and the lease token. It never changes while held; heartbeats move only the
// expiry.
type Lease struct {
	ArticleID int64
	Stage     Status
	Token     int64
}

// LeaseOf returns the lease a claimed article carries.
func LeaseOf(a Article) Lease {
	return Lease{ArticleID: a.ID, Stage: a.Status, Token: a.LeaseID}
}

const articleColumns = `id, url, status, failed_stage, error, attempts, next_attempt_at, locked_until, lease_id,
	title, author, site_name, published_at, word_count, image_count, image_importance, image_notes,
	audio_bytes, duration_ms, created_at, updated_at, ready_at`

// InsertArticle queues url (already normalized) unless it already exists.
// It returns the article and whether it was newly created.
func (s *Store) InsertArticle(ctx context.Context, url string, now time.Time) (Article, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Article{}, false, err
	}
	defer tx.Rollback()

	ms := now.UnixMilli()
	row := tx.QueryRowContext(ctx, `
		INSERT INTO articles (url, status, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (url) DO NOTHING
		RETURNING `+articleColumns,
		url, StatusQueued, ms, ms, ms)
	a, err := scanArticle(row)
	created := true
	if errors.Is(err, ErrNotFound) {
		created = false
		a, err = scanArticle(tx.QueryRowContext(ctx,
			`SELECT `+articleColumns+` FROM articles WHERE url = ?`, url))
	}
	if err != nil {
		return Article{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Article{}, false, err
	}
	return a, created, nil
}

// GetArticle returns the article with id, or ErrNotFound.
func (s *Store) GetArticle(ctx context.Context, id int64) (Article, error) {
	return scanArticle(s.db.QueryRowContext(ctx,
		`SELECT `+articleColumns+` FROM articles WHERE id = ?`, id))
}

// ListArticles returns articles newest first.
func (s *Store) ListArticles(ctx context.Context, opts ListOptions) ([]Article, error) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	query := `SELECT ` + articleColumns + ` FROM articles`
	var args []any
	if opts.Status != "" {
		query += ` WHERE status = ?`
		args = append(args, opts.Status)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, opts.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Article
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClaimArticle leases the next runnable article: a non-terminal status,
// next_attempt_at <= now, and no live lease. Articles are taken oldest first
// (by id), so one article flows through its stages before newer ones start.
// If stages is non-empty, only articles waiting for one of those stages are
// considered (queued counts as waiting for the first stage).
//
// A queued article moves to the first stage. attempts is incremented at
// claim time, so a stage that keeps crashing the process still accumulates
// attempts; the caller must fail an article whose Attempts already exceeds
// its limit instead of running it again. Returns ErrNoWork when nothing is
// runnable.
func (s *Store) ClaimArticle(ctx context.Context, now time.Time, lease time.Duration, stages ...Status) (Article, error) {
	if lease <= 0 {
		return Article{}, fmt.Errorf("claim: lease must be positive, got %s", lease)
	}
	if len(stages) == 0 {
		stages = stageOrder
	}
	var runnable []Status
	for _, st := range stages {
		if !st.IsStage() {
			return Article{}, fmt.Errorf("claim: %q is not a pipeline stage", st)
		}
		runnable = append(runnable, st)
		if st == stageOrder[0] {
			runnable = append(runnable, StatusQueued)
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(runnable)), ", ")

	ms := now.UnixMilli()
	args := []any{StatusQueued, stageOrder[0], now.Add(lease).UnixMilli(), newLeaseID(), ms}
	for _, st := range runnable {
		args = append(args, st)
	}
	args = append(args, ms, ms)

	// One statement, so the select-and-update is atomic.
	a, err := scanArticle(s.db.QueryRowContext(ctx, `
		UPDATE articles
		SET status = CASE WHEN status = ? THEN ? ELSE status END,
		    locked_until = ?,
		    lease_id = ?,
		    attempts = attempts + 1,
		    updated_at = ?
		WHERE id = (
			SELECT id FROM articles
			WHERE status IN (`+placeholders+`)
			  AND next_attempt_at <= ?
			  AND (locked_until IS NULL OR locked_until <= ?)
			ORDER BY id
			LIMIT 1
		)
		RETURNING `+articleColumns, args...))
	if errors.Is(err, ErrNotFound) {
		return Article{}, ErrNoWork
	}
	return a, err
}

// ExtendLease moves the lease expiry to until, which must be after now. The
// lease token is unchanged.
func (s *Store) ExtendLease(ctx context.Context, l Lease, until, now time.Time) error {
	if until.UnixMilli() <= now.UnixMilli() { // compare at stored precision
		return fmt.Errorf("extend lease: expiry %s is not after now %s", until, now)
	}
	return s.leaseWrite(ctx, l, `locked_until = ?, updated_at = ?`, until.UnixMilli(), now.UnixMilli())
}

// CompleteStage advances the article past its leased stage and releases the
// lease, resetting attempts, error and failed_stage (so a reprocessed article
// ends clean). On reaching ready, ready_at is set only if it was unset, so
// re-processing an article keeps its original publish date. Returns the new
// status.
func (s *Store) CompleteStage(ctx context.Context, l Lease, now time.Time) (Status, error) {
	next, err := l.Stage.next()
	if err != nil {
		return "", err
	}
	ms := now.UnixMilli()
	var readyAt any // NULL unless becoming ready
	if next == StatusReady {
		readyAt = ms
	}
	err = s.leaseWrite(ctx, l, `
		status = ?, locked_until = NULL, lease_id = NULL, attempts = 0, error = NULL,
		failed_stage = NULL, next_attempt_at = ?, updated_at = ?, ready_at = COALESCE(ready_at, ?)`,
		next, ms, ms, readyAt)
	return next, err
}

// RetryStage records a failed attempt and schedules the stage to run again at
// retryAt, releasing the lease.
func (s *Store) RetryStage(ctx context.Context, l Lease, errMsg string, retryAt, now time.Time) error {
	return s.leaseWrite(ctx, l, `
		error = ?, next_attempt_at = ?, locked_until = NULL, lease_id = NULL, updated_at = ?`,
		errMsg, retryAt.UnixMilli(), now.UnixMilli())
}

// FailArticle marks the article failed at its leased stage and releases the
// lease. It stays failed until reprocessed.
func (s *Store) FailArticle(ctx context.Context, l Lease, errMsg string, now time.Time) error {
	return s.leaseWrite(ctx, l, `
		status = ?, failed_stage = ?, error = ?, locked_until = NULL, lease_id = NULL, updated_at = ?`,
		StatusFailed, l.Stage, errMsg, now.UnixMilli())
}

// ReleaseLease gives the article back without counting the attempt (used on
// shutdown, when a stage was interrupted rather than failed). Callers should
// use a fresh context, since the shutdown context is already cancelled. If
// the release never happens, ReleaseAllLeases clears it at the next start, and
// the interrupted attempt is then counted.
func (s *Store) ReleaseLease(ctx context.Context, l Lease, now time.Time) error {
	return s.leaseWrite(ctx, l, `
		locked_until = NULL, lease_id = NULL, attempts = MAX(attempts - 1, 0), updated_at = ?`,
		now.UnixMilli())
}

// ReleaseAllLeases clears every lease. phonetic is a single process, so at
// startup any lease belongs to a previous process that died; clearing them
// makes interrupted work runnable immediately instead of after expiry.
// Attempts are kept, so a crash counts as an attempt.
func (s *Store) ReleaseAllLeases(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE articles SET locked_until = NULL, lease_id = NULL, updated_at = ? WHERE locked_until IS NOT NULL`,
		now.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// leaseWrite applies set to the leased article if, and only if, the lease is
// still held: same token and still at the leased stage.
func (s *Store) leaseWrite(ctx context.Context, l Lease, set string, args ...any) error {
	args = append(args, l.ArticleID, l.Token, l.Stage)
	res, err := s.db.ExecContext(ctx,
		`UPDATE articles SET `+set+` WHERE id = ? AND lease_id = ? AND status = ?`, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanArticle(sc scanner) (Article, error) {
	var (
		a                                            Article
		failedStage, errMsg, title, author, siteName sql.NullString
		imageNotes                                   sql.NullString
		lockedUntil, leaseID, publishedAt, readyAt   sql.NullInt64
		wordCount, imageCount, imageImportance       sql.NullInt64
		audioBytes, durationMS                       sql.NullInt64
		nextAttemptAt, createdAt, updatedAt          int64
	)
	err := sc.Scan(&a.ID, &a.URL, &a.Status, &failedStage, &errMsg, &a.Attempts, &nextAttemptAt, &lockedUntil, &leaseID,
		&title, &author, &siteName, &publishedAt, &wordCount, &imageCount, &imageImportance, &imageNotes,
		&audioBytes, &durationMS, &createdAt, &updatedAt, &readyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Article{}, ErrNotFound
	}
	if err != nil {
		return Article{}, err
	}
	a.FailedStage = Status(failedStage.String)
	a.Error = errMsg.String
	a.NextAttemptAt = fromMillis(nextAttemptAt)
	a.LockedUntil = fromNullMillis(lockedUntil)
	a.LeaseID = leaseID.Int64
	a.Title, a.Author, a.SiteName = title.String, author.String, siteName.String
	a.PublishedAt = fromNullMillis(publishedAt)
	a.WordCount, a.ImageCount = int(wordCount.Int64), int(imageCount.Int64)
	if imageImportance.Valid {
		v := int(imageImportance.Int64)
		a.ImageImportance = &v
	}
	a.ImageNotes = imageNotes.String
	a.AudioBytes = audioBytes.Int64
	a.Duration = time.Duration(durationMS.Int64) * time.Millisecond
	a.CreatedAt, a.UpdatedAt = fromMillis(createdAt), fromMillis(updatedAt)
	a.ReadyAt = fromNullMillis(readyAt)
	return a, nil
}

// newLeaseID returns a random, non-zero lease token (zero means "not leased").
func newLeaseID() int64 { return rand.Int64N(math.MaxInt64) + 1 }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func fromNullMillis(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return fromMillis(n.Int64)
}

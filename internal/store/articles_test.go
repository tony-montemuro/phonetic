package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// t0 is a fixed reference time; tests pass explicit times to the store.
var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

const testLease = time.Minute

func mustInsert(t *testing.T, s *Store, url string, now time.Time) Article {
	t.Helper()
	a, created, err := s.InsertArticle(context.Background(), url, now)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatalf("InsertArticle(%q): expected new article", url)
	}
	return a
}

func mustGet(t *testing.T, s *Store, id int64) Article {
	t.Helper()
	a, err := s.GetArticle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustClaim(t *testing.T, s *Store, now time.Time, stages ...Status) Article {
	t.Helper()
	a, err := s.ClaimArticle(context.Background(), now, testLease, stages...)
	if err != nil {
		t.Fatalf("ClaimArticle: %v", err)
	}
	return a
}

func assertNothingClaimable(t *testing.T, s *Store, now time.Time, stages ...Status) {
	t.Helper()
	if a, err := s.ClaimArticle(context.Background(), now, testLease, stages...); !errors.Is(err, ErrNoWork) {
		t.Fatalf("ClaimArticle = (%d, %v), want ErrNoWork", a.ID, err)
	}
}

// mustFinish runs the oldest unfinished article through every remaining
// stage, reaching ready at now.
func mustFinish(t *testing.T, s *Store, now time.Time) Article {
	t.Helper()
	for {
		a := mustClaim(t, s, now)
		next, err := s.CompleteStage(context.Background(), LeaseOf(a), now)
		if err != nil {
			t.Fatal(err)
		}
		if next == StatusReady {
			return mustGet(t, s, a.ID)
		}
	}
}

func articleIDs(as []Article) []int64 {
	ids := make([]int64, len(as))
	for i, a := range as {
		ids[i] = a.ID
	}
	return ids
}

func TestStatusHelpers(t *testing.T) {
	t.Parallel()
	for _, st := range []Status{StatusQueued, StatusFetching, StatusExtracting, StatusScripting, StatusSynthesizing, StatusReady, StatusFailed} {
		if !st.Valid() {
			t.Errorf("%q should be valid", st)
		}
	}
	if Status("redy").Valid() {
		t.Error(`"redy" should be invalid`)
	}
	for _, st := range []Status{StatusQueued, StatusReady, StatusFailed} {
		if st.IsStage() {
			t.Errorf("%q is not a stage", st)
		}
	}
	if _, err := StatusReady.next(); err == nil {
		t.Error("ready has no next stage")
	}
	st := Stages()
	st[0] = "mutated"
	if Stages()[0] != StatusFetching {
		t.Error("Stages() exposed internal slice")
	}
}

// Every Status the Go code knows must satisfy the schema's CHECK constraints,
// so the two can't drift apart.
func TestStatusesMatchSchema(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	all := append([]Status{StatusQueued, StatusReady, StatusFailed}, Stages()...)
	for i, st := range all {
		var failedStage any
		if st.IsStage() {
			failedStage = st
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO articles (url, status, failed_stage, next_attempt_at, created_at, updated_at)
			VALUES (?, ?, ?, 0, 0, 0)`, fmt.Sprintf("https://example.com/%d", i), st, failedStage)
		if err != nil {
			t.Errorf("status %q rejected by schema: %v", st, err)
		}
	}
}

func TestInsertArticle(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()

	a := mustInsert(t, s, "https://example.com/a", t0)
	if a.ID == 0 || a.URL != "https://example.com/a" || a.Status != StatusQueued {
		t.Errorf("unexpected article %+v", a)
	}
	if !a.CreatedAt.Equal(t0) || !a.UpdatedAt.Equal(t0) || !a.NextAttemptAt.Equal(t0) {
		t.Errorf("timestamps not set to insert time: %+v", a)
	}
	if !a.LockedUntil.IsZero() || a.LeaseID != 0 || !a.ReadyAt.IsZero() || a.ImageImportance != nil || a.Attempts != 0 {
		t.Errorf("unexpected non-NULL fields: %+v", a)
	}

	dup, created, err := s.InsertArticle(ctx, "https://example.com/a", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created || dup.ID != a.ID || !dup.CreatedAt.Equal(t0) {
		t.Errorf("duplicate insert: created=%v article=%+v", created, dup)
	}
}

func TestConcurrentInsertSameURLCreatesOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[int64]bool{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, c, err := s.InsertArticle(context.Background(), "https://example.com/same", t0)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[a.ID] = true
			if c {
				created++
			}
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Errorf("created=%d distinct ids=%d, want 1 and 1", created, len(ids))
	}
}

func TestGetAndListArticles(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()

	if _, err := s.GetArticle(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetArticle(missing) err = %v, want ErrNotFound", err)
	}

	var ids []int64
	for i := range 3 {
		ids = append(ids, mustInsert(t, s, fmt.Sprintf("https://example.com/%d", i), t0).ID)
	}
	mustClaim(t, s, t0) // moves the oldest off "queued"

	if got := mustGet(t, s, ids[1]); got.ID != ids[1] {
		t.Fatalf("GetArticle = %+v", got)
	}

	all, err := s.ListArticles(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != ids[2] || all[2].ID != ids[0] {
		t.Errorf("ListArticles order = %v, want newest first", articleIDs(all))
	}
	queued, err := s.ListArticles(ctx, ListOptions{Status: StatusQueued})
	if err != nil || len(queued) != 2 {
		t.Errorf("queued = %v, %v; want 2 articles", articleIDs(queued), err)
	}
	limited, err := s.ListArticles(ctx, ListOptions{Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Errorf("Limit 1 = %v, %v", articleIDs(limited), err)
	}
}

func TestListReady(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	for i := range 4 {
		mustInsert(t, s, fmt.Sprintf("https://example.com/%d", i), t0)
	}
	// Finish 1, 2 and 3 out of id order: 1 last.
	first := mustFinish(t, s, t0.Add(time.Minute))
	second := mustFinish(t, s, t0.Add(3*time.Minute))
	third := mustFinish(t, s, t0.Add(2*time.Minute))
	// The fourth stays queued.

	got, err := s.ListReady(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{second.ID, third.ID, first.ID}
	if !slices.Equal(articleIDs(got), want) {
		t.Errorf("ListReady = %v, want %v (newest ready first, ready only)", articleIDs(got), want)
	}
	if limited, err := s.ListReady(ctx, 1); err != nil || len(limited) != 1 || limited[0].ID != second.ID {
		t.Errorf("ListReady(1) = %v, %v", articleIDs(limited), err)
	}
	if _, err := s.ListReady(ctx, 0); err == nil {
		t.Error("ListReady(0) accepted; it would silently return nothing")
	}
}

func TestClaimLeasesOldestFirst(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	first := mustInsert(t, s, "https://example.com/1", t0)
	second := mustInsert(t, s, "https://example.com/2", t0.Add(time.Second))
	now := t0.Add(time.Minute)

	a := mustClaim(t, s, now)
	if a.ID != first.ID {
		t.Fatalf("claimed %d, want oldest %d", a.ID, first.ID)
	}
	if a.Status != StatusFetching || a.Attempts != 1 || a.LeaseID == 0 || !a.LockedUntil.Equal(now.Add(testLease)) {
		t.Errorf("claimed article = %+v", a)
	}

	b := mustClaim(t, s, now)
	if b.ID != second.ID {
		t.Fatalf("claimed %d, want %d (first is leased)", b.ID, second.ID)
	}
	assertNothingClaimable(t, s, now)

	again := mustClaim(t, s, a.LockedUntil) // expired: claimable again
	if again.ID != first.ID || again.Attempts != 2 || again.LeaseID == a.LeaseID {
		t.Errorf("reclaimed = %+v (old lease id %d)", again, a.LeaseID)
	}
}

// An article that just finished a stage must be picked before newer queued
// articles, so the first submission becomes an episode first (depth-first).
func TestClaimIsDepthFirst(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	old := mustInsert(t, s, "https://example.com/old", t0)
	mustInsert(t, s, "https://example.com/new", t0.Add(time.Second))

	a := mustClaim(t, s, t0.Add(time.Minute))
	if _, err := s.CompleteStage(ctx, LeaseOf(a), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	next := mustClaim(t, s, t0.Add(3*time.Minute))
	if next.ID != old.ID || next.Status != StatusExtracting {
		t.Errorf("claimed %d at %s, want old article %d at extracting", next.ID, next.Status, old.ID)
	}
}

func TestClaimStageFilter(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	a := mustInsert(t, s, "https://example.com/a", t0)
	b := mustInsert(t, s, "https://example.com/b", t0)

	// Advance a to synthesizing.
	for range 3 {
		c := mustClaim(t, s, t0)
		if c.ID != a.ID {
			t.Fatalf("expected to claim %d, got %d", a.ID, c.ID)
		}
		if _, err := s.CompleteStage(ctx, LeaseOf(c), t0); err != nil {
			t.Fatal(err)
		}
	}

	// Filtering on fetching includes queued articles (b), not a.
	got := mustClaim(t, s, t0, StatusFetching)
	if got.ID != b.ID || got.Status != StatusFetching {
		t.Errorf("fetch-only claim = %d at %s, want %d at fetching", got.ID, got.Status, b.ID)
	}
	assertNothingClaimable(t, s, t0, StatusFetching, StatusExtracting, StatusScripting)

	got = mustClaim(t, s, t0, StatusSynthesizing)
	if got.ID != a.ID {
		t.Errorf("synth-only claim = %d, want %d", got.ID, a.ID)
	}

	if _, err := s.ClaimArticle(ctx, t0, testLease, StatusReady); err == nil {
		t.Error("filtering on a non-stage status should be an error")
	}
}

func TestClaimRespectsNextAttemptAt(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	mustInsert(t, s, "https://example.com/future", t0)
	assertNothingClaimable(t, s, t0.Add(-time.Millisecond))
	mustClaim(t, s, t0)
}

func TestLeaseValidation(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	mustInsert(t, s, "https://example.com/a", t0)

	for _, lease := range []time.Duration{0, -time.Second} {
		if _, err := s.ClaimArticle(ctx, t0, lease); err == nil {
			t.Errorf("ClaimArticle(lease=%s) should fail", lease)
		}
	}
	a := mustClaim(t, s, t0)
	for _, until := range []time.Time{t0, t0.Add(-time.Second), t0.Add(500 * time.Microsecond)} { // sub-ms rounds to now
		if err := s.ExtendLease(ctx, LeaseOf(a), until, t0); err == nil {
			t.Errorf("ExtendLease(until=%s, now=%s) should fail", until, t0)
		}
	}
}

func TestStageLifecycleToReady(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	id := mustInsert(t, s, "https://example.com/a", t0).ID

	now := t0
	for _, stage := range Stages() {
		a := mustClaim(t, s, now)
		if a.ID != id || a.Status != stage {
			t.Fatalf("claimed %d at %q, want %d at %q", a.ID, a.Status, id, stage)
		}
		next, err := s.CompleteStage(ctx, LeaseOf(a), now)
		if err != nil {
			t.Fatalf("CompleteStage(%s): %v", stage, err)
		}
		got := mustGet(t, s, id)
		if got.Status != next || got.Attempts != 0 || !got.LockedUntil.IsZero() || got.LeaseID != 0 || got.Error != "" {
			t.Fatalf("after %s: %+v", stage, got)
		}
		if next != StatusReady && !got.ReadyAt.IsZero() {
			t.Fatalf("ready_at set before ready: %+v", got)
		}
		if next == StatusReady && !got.ReadyAt.Equal(now) {
			t.Fatalf("ready_at = %s, want completion time %s", got.ReadyAt, now)
		}
		now = now.Add(time.Second)
	}
	assertNothingClaimable(t, s, now.Add(time.Hour))
}

// Re-processing a ready article keeps its first publish date and ends with
// failed_stage cleared.
func TestReprocessKeepsReadyAtAndClearsFailedStage(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	id := mustInsert(t, s, "https://example.com/a", t0).ID
	for range Stages() {
		a := mustClaim(t, s, t0)
		if _, err := s.CompleteStage(ctx, LeaseOf(a), t0); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a later reprocess of the last stage after a failure.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE articles SET status = ?, failed_stage = ? WHERE id = ?`,
		StatusSynthesizing, StatusSynthesizing, id); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(24 * time.Hour)
	a := mustClaim(t, s, later)
	if _, err := s.CompleteStage(ctx, LeaseOf(a), later); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, id)
	if !got.ReadyAt.Equal(t0) || got.FailedStage != "" {
		t.Errorf("after reprocess: ready_at=%s failed_stage=%q, want %s and empty", got.ReadyAt, got.FailedStage, t0)
	}
}

func TestRetryStage(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	mustInsert(t, s, "https://example.com/a", t0)

	a := mustClaim(t, s, t0)
	retryAt := t0.Add(30 * time.Second)
	if err := s.RetryStage(ctx, LeaseOf(a), "connection reset", retryAt, t0); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, a.ID)
	if got.Status != StatusFetching || got.Error != "connection reset" || got.Attempts != 1 ||
		!got.LockedUntil.IsZero() || got.LeaseID != 0 || !got.NextAttemptAt.Equal(retryAt) {
		t.Fatalf("after retry: %+v", got)
	}

	assertNothingClaimable(t, s, retryAt.Add(-time.Millisecond))
	if b := mustClaim(t, s, retryAt); b.Attempts != 2 || b.Status != StatusFetching {
		t.Errorf("retry claim = %+v", b)
	}
}

func TestFailArticle(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	mustInsert(t, s, "https://example.com/a", t0)

	a := mustClaim(t, s, t0)
	if err := s.FailArticle(ctx, LeaseOf(a), "404 not found", t0); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, a.ID)
	if got.Status != StatusFailed || got.FailedStage != StatusFetching || got.Error != "404 not found" ||
		!got.LockedUntil.IsZero() || got.LeaseID != 0 {
		t.Fatalf("after fail: %+v", got)
	}
	assertNothingClaimable(t, s, t0.Add(24*time.Hour))
}

func TestReleaseLeaseDoesNotCountAttempt(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	mustInsert(t, s, "https://example.com/a", t0)

	a := mustClaim(t, s, t0)
	if err := s.ReleaseLease(ctx, LeaseOf(a), t0); err != nil {
		t.Fatal(err)
	}
	if b := mustClaim(t, s, t0); b.ID != a.ID || b.Attempts != 1 {
		t.Errorf("after release+reclaim attempts = %d, want 1", b.Attempts)
	}
}

// Heartbeats move only the expiry, so the holder's token stays valid no matter
// how a heartbeat interleaves with its final write.
func TestExtendLeaseKeepsTokenValid(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	mustInsert(t, s, "https://example.com/a", t0)

	a := mustClaim(t, s, t0)
	l := LeaseOf(a)
	for i := 1; i <= 3; i++ {
		if err := s.ExtendLease(ctx, l, t0.Add(time.Duration(i)*5*time.Minute), t0); err != nil {
			t.Fatal(err)
		}
	}
	assertNothingClaimable(t, s, a.LockedUntil) // original expiry no longer applies
	if _, err := s.CompleteStage(ctx, l, t0); err != nil {
		t.Errorf("CompleteStage after extensions: %v", err)
	}
}

// A worker whose lease expired and was reclaimed must not be able to write.
func TestLeaseLostAfterReclaim(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	mustInsert(t, s, "https://example.com/a", t0)

	staleArticle := mustClaim(t, s, t0)
	stale := LeaseOf(staleArticle)
	fresh := LeaseOf(mustClaim(t, s, staleArticle.LockedUntil)) // expired, reclaimed

	writes := map[string]func() error{
		"complete": func() error { _, err := s.CompleteStage(ctx, stale, t0); return err },
		"retry":    func() error { return s.RetryStage(ctx, stale, "x", t0, t0) },
		"fail":     func() error { return s.FailArticle(ctx, stale, "x", t0) },
		"release":  func() error { return s.ReleaseLease(ctx, stale, t0) },
		"extend":   func() error { return s.ExtendLease(ctx, stale, t0.Add(time.Hour), t0) },
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s with stale lease: %v, want ErrLeaseLost", name, err)
		}
	}
	if _, err := s.CompleteStage(ctx, fresh, t0); err != nil {
		t.Errorf("new owner's write failed: %v", err)
	}
}

// The status guard rejects a write whose token matches but whose stage moved
// on (e.g. a reprocess reset the article under a stale holder).
func TestLeaseWriteStatusGuard(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	mustInsert(t, s, "https://example.com/a", t0)
	a := mustClaim(t, s, t0)
	if _, err := s.db.ExecContext(ctx, `UPDATE articles SET status = ? WHERE id = ?`, StatusExtracting, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteStage(ctx, LeaseOf(a), t0); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("CompleteStage with matching token but changed stage: %v, want ErrLeaseLost", err)
	}
}

func TestReleaseAllLeases(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	a := mustInsert(t, s, "https://example.com/a", t0)
	b := mustInsert(t, s, "https://example.com/b", t0)
	c := mustInsert(t, s, "https://example.com/c", t0)
	mustClaim(t, s, t0)
	mustClaim(t, s, t0)

	n, err := s.ReleaseAllLeases(ctx, t0)
	if err != nil || n != 2 {
		t.Fatalf("ReleaseAllLeases = %d, %v; want 2", n, err)
	}
	want := map[int64]int{a.ID: 2, b.ID: 2, c.ID: 1} // crashes count as attempts
	for range 3 {
		got := mustClaim(t, s, t0)
		if got.Attempts != want[got.ID] || got.LeaseID == 0 {
			t.Errorf("article %d: attempts=%d lease=%d, want attempts %d", got.ID, got.Attempts, got.LeaseID, want[got.ID])
		}
	}
	assertNothingClaimable(t, s, t0)
}

// Concurrent workers must never claim the same article twice.
func TestConcurrentClaimsAreExclusive(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	const articles, workers = 40, 8
	for i := range articles {
		mustInsert(t, s, fmt.Sprintf("https://example.com/%d", i), t0)
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]int{}
		wg      sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				a, err := s.ClaimArticle(context.Background(), t0, testLease)
				if errors.Is(err, ErrNoWork) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				claimed[a.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != articles {
		t.Errorf("claimed %d distinct articles, want %d", len(claimed), articles)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("article %d claimed %d times", id, n)
		}
	}
}

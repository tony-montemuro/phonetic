package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/store"
)

// fastOpts keeps tests quick without timing fragility: leases are long
// (nothing should expire unless a test means it to), polls and backoff short.
var fastOpts = Options{
	Workers:      2,
	Lease:        5 * time.Second,
	PollInterval: 50 * time.Millisecond,
	MaxAttempts:  3,
	BackoffBase:  time.Millisecond,
	BackoffMax:   5 * time.Millisecond,
}

// waitTimeout bounds every wait in these tests.
const waitTimeout = 10 * time.Second

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "phonetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insert(t *testing.T, s *store.Store, url string) int64 {
	t.Helper()
	a, _, err := s.InsertArticle(context.Background(), url, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return a.ID
}

// stagesWith returns instant stubs with stage overridden by st.
func stagesWith(stage store.Status, st Stage) map[store.Status]Stage {
	m := StubStages(0)
	m[stage] = st
	return m
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func hasStatus(st store.Status) func(store.Article) bool {
	return func(a store.Article) bool { return a.Status == st }
}

// harness runs a Runner in the background for one test.
type harness struct {
	t      *testing.T
	store  *store.Store
	runner *Runner
	cancel context.CancelFunc
	done   chan struct{}
}

func startRunner(t *testing.T, s *store.Store, stages map[store.Status]Stage, opts Options) *harness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := New(s, stages, logger, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, store: s, runner: r, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		r.Run(ctx)
	}()
	t.Cleanup(h.stop)
	return h
}

// stop cancels the runner and waits for Run to return (bounded).
func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(waitTimeout):
		h.t.Fatal("Run did not return after cancel")
	}
}

// waitFor polls the article until cond holds or the timeout passes.
func (h *harness) waitFor(id int64, cond func(store.Article) bool) store.Article {
	h.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		a, err := h.store.GetArticle(context.Background(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		if cond(a) {
			return a
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("article %d: condition not met within %s; last state %+v", id, waitTimeout, a)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countingStage records how many times it runs per article and then
// delegates to fn.
type countingStage struct {
	mu    sync.Mutex
	calls map[int64]int
	fn    func(ctx context.Context, a store.Article, call int) error
}

func newCountingStage(fn func(ctx context.Context, a store.Article, call int) error) *countingStage {
	return &countingStage{calls: map[int64]int{}, fn: fn}
}

func (c *countingStage) Run(ctx context.Context, a store.Article) error {
	c.mu.Lock()
	c.calls[a.ID]++
	n := c.calls[a.ID]
	c.mu.Unlock()
	if c.fn == nil {
		return nil
	}
	return c.fn(ctx, a, n)
}

func (c *countingStage) count(id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

// crash simulates n process crashes mid-stage: claims whose leases are then
// cleared at "startup", each counting as an attempt.
func crash(t *testing.T, s *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	for range n {
		if _, err := s.ClaimArticle(ctx, time.Now(), time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReleaseAllLeases(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	missingStage := StubStages(0)
	delete(missingStage, store.StatusScripting)
	tests := map[string]struct {
		stages map[store.Status]Stage
		opts   Options
	}{
		"missing stage impl": {missingStage, Options{}},
		"lease too short":    {StubStages(0), Options{Lease: 10 * time.Millisecond}},
		"stage without pool": {StubStages(0), Options{Pools: []Pool{{Workers: 1, Stages: []store.Status{store.StatusFetching}}}}},
		"non-stage in pool":  {StubStages(0), Options{Pools: []Pool{{Workers: 1, Stages: append(store.Stages(), store.StatusReady)}}}},
		"empty pool":         {StubStages(0), Options{Pools: []Pool{{Workers: 0, Stages: store.Stages()}}}},
		"stage in two pools": {StubStages(0), Options{Pools: []Pool{
			{Workers: 2, Stages: store.Stages()},
			{Workers: 1, Stages: []store.Status{store.StatusSynthesizing}},
		}}},
		"workers and pools": {StubStages(0), Options{Workers: 2, Pools: []Pool{{Workers: 1, Stages: store.Stages()}}}},
	}
	for name, tt := range tests {
		if _, err := New(s, tt.stages, slog.Default(), tt.opts); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestArticleRunsThroughAllStages(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	var mu sync.Mutex
	var order []store.Status
	stages := map[store.Status]Stage{}
	for _, st := range store.Stages() {
		stages[st] = StageFunc(func(_ context.Context, a store.Article) error {
			mu.Lock()
			order = append(order, a.Status)
			mu.Unlock()
			return nil
		})
	}
	h := startRunner(t, s, stages, fastOpts)
	id := insert(t, s, "https://example.com/a")

	a := h.waitFor(id, hasStatus(store.StatusReady))
	if a.ReadyAt.IsZero() || a.Attempts != 0 || !a.LockedUntil.IsZero() || a.LeaseID != 0 {
		t.Errorf("ready article = %+v", a)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(order) != fmt.Sprint(store.Stages()) {
		t.Errorf("stage order = %v, want %v", order, store.Stages())
	}
}

// A stage running longer than WriteTimeout must still have its result
// recorded: the write context starts after the stage, not before.
func TestStageLongerThanWriteTimeoutIsRecorded(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	opts := fastOpts
	opts.Workers = 1
	opts.WriteTimeout = 500 * time.Millisecond
	slow := newCountingStage(func(ctx context.Context, _ store.Article, _ int) error {
		select {
		case <-time.After(1200 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	h := startRunner(t, s, stagesWith(store.StatusScripting, slow), opts)
	id := insert(t, s, "https://example.com/a")
	h.waitFor(id, hasStatus(store.StatusReady))
	if n := slow.count(id); n != 1 {
		t.Errorf("slow stage ran %d times, want 1", n)
	}
}

func TestTransientFailureRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	flaky := newCountingStage(func(_ context.Context, _ store.Article, call int) error {
		if call < 3 {
			return errors.New("connection reset")
		}
		return nil
	})
	h := startRunner(t, s, stagesWith(store.StatusExtracting, flaky), fastOpts)
	id := insert(t, s, "https://example.com/a")

	a := h.waitFor(id, hasStatus(store.StatusReady))
	if n := flaky.count(id); n != 3 {
		t.Errorf("extract ran %d times, want 3", n)
	}
	if a.Error != "" {
		t.Errorf("error not cleared on success: %q", a.Error)
	}
}

func TestExhaustedRetriesFailArticle(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	broken := newCountingStage(func(context.Context, store.Article, int) error {
		return errors.New("still broken")
	})
	h := startRunner(t, s, stagesWith(store.StatusScripting, broken), fastOpts)
	id := insert(t, s, "https://example.com/a")

	a := h.waitFor(id, hasStatus(store.StatusFailed))
	if a.FailedStage != store.StatusScripting || a.Error != "still broken" || !a.LockedUntil.IsZero() {
		t.Errorf("failed article = %+v", a)
	}
	if n := broken.count(id); n != fastOpts.MaxAttempts {
		t.Errorf("stage ran %d times, want %d", n, fastOpts.MaxAttempts)
	}
}

func TestPermanentErrorFailsImmediately(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	notFound := newCountingStage(func(context.Context, store.Article, int) error {
		return Permanent(fmt.Errorf("fetch: %w", errors.New("404 not found")))
	})
	h := startRunner(t, s, stagesWith(store.StatusFetching, notFound), fastOpts)
	id := insert(t, s, "https://example.com/a")

	a := h.waitFor(id, hasStatus(store.StatusFailed))
	if n := notFound.count(id); n != 1 {
		t.Errorf("stage ran %d times, want 1", n)
	}
	if a.Error != "fetch: 404 not found" {
		t.Errorf("error = %q", a.Error)
	}
}

func TestPanickingStageIsRetried(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	panicky := newCountingStage(func(_ context.Context, _ store.Article, call int) error {
		if call == 1 {
			panic("boom")
		}
		return nil
	})
	h := startRunner(t, s, stagesWith(store.StatusFetching, panicky), fastOpts)
	id := insert(t, s, "https://example.com/a")
	h.waitFor(id, hasStatus(store.StatusReady))
	if n := panicky.count(id); n != 2 {
		t.Errorf("stage ran %d times, want 2", n)
	}
}

// Exactly MaxAttempts crashes exhaust the article without running it again.
func TestCrashLoopIsBoundedByAttempts(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	id := insert(t, s, "https://example.com/a")
	crash(t, s, fastOpts.MaxAttempts)

	never := newCountingStage(nil)
	h := startRunner(t, s, stagesWith(store.StatusFetching, never), fastOpts)
	a := h.waitFor(id, hasStatus(store.StatusFailed))
	if n := never.count(id); n != 0 {
		t.Errorf("stage ran %d times after attempts were exhausted", n)
	}
	if a.FailedStage != store.StatusFetching || a.Error == "" {
		t.Errorf("failed article = %+v", a)
	}
}

// One fewer crash than the limit still leaves one real run.
func TestCrashesBelowLimitStillRun(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	id := insert(t, s, "https://example.com/a")
	crash(t, s, fastOpts.MaxAttempts-1)

	once := newCountingStage(nil)
	h := startRunner(t, s, stagesWith(store.StatusFetching, once), fastOpts)
	h.waitFor(id, hasStatus(store.StatusReady))
	if n := once.count(id); n != 1 {
		t.Errorf("stage ran %d times, want 1", n)
	}
}

func TestShutdownReleasesLeaseWithoutCountingAttempt(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	started := make(chan struct{})
	blocking := StageFunc(func(ctx context.Context, _ store.Article) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	h := startRunner(t, s, stagesWith(store.StatusFetching, blocking), fastOpts)
	id := insert(t, s, "https://example.com/a")

	waitClosed(t, started, "stage start")
	h.stop()

	a, err := s.GetArticle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusFetching || a.Attempts != 0 || !a.LockedUntil.IsZero() || a.Error != "" {
		t.Errorf("after shutdown: %+v (want fetching, 0 attempts, no lease, no error)", a)
	}
}

// A stage that finishes successfully as shutdown begins is recorded as
// complete, not released.
func TestSuccessDuringShutdownIsRecorded(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	started := make(chan struct{})
	finishing := StageFunc(func(ctx context.Context, _ store.Article) error {
		close(started)
		<-ctx.Done()
		return nil // work happened to complete
	})
	h := startRunner(t, s, stagesWith(store.StatusFetching, finishing), fastOpts)
	id := insert(t, s, "https://example.com/a")

	waitClosed(t, started, "stage start")
	h.stop()

	a, err := s.GetArticle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != store.StatusExtracting || a.Attempts != 0 || !a.LockedUntil.IsZero() {
		t.Errorf("after shutdown: %+v (want extracting, unleased)", a)
	}
}

// A stage outliving its lease must be kept alive by the heartbeat: a
// concurrent claimer must never get the article.
func TestHeartbeatKeepsLongStageLeased(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	opts := fastOpts
	opts.Workers = 1
	opts.Lease = time.Second

	started := make(chan struct{})
	slow := newCountingStage(func(ctx context.Context, _ store.Article, _ int) error {
		close(started)
		select {
		case <-time.After(3 * time.Second): // 3 leases long
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	h := startRunner(t, s, stagesWith(store.StatusSynthesizing, slow), opts)
	id := insert(t, s, "https://example.com/a")
	waitClosed(t, started, "slow stage start")

	// Try to steal the article for the duration of the stage.
	stealDone := make(chan struct{})
	var stolen atomic.Bool
	go func() {
		defer close(stealDone)
		for {
			a, err := s.ClaimArticle(context.Background(), time.Now(), time.Minute)
			if err == nil {
				stolen.Store(a.ID == id)
				return
			}
			cur, _ := s.GetArticle(context.Background(), id)
			if cur.Status == store.StatusReady {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	waitClosed(t, stealDone, "stealer")
	if stolen.Load() {
		t.Fatal("article was claimed by someone else while its stage ran (lease not kept alive)")
	}
	h.waitFor(id, hasStatus(store.StatusReady))
	if n := slow.count(id); n != 1 {
		t.Errorf("slow stage ran %d times, want 1", n)
	}
}

// If the lease disappears mid-stage, the heartbeat cancels the stage and the
// worker records nothing.
func TestLostLeaseCancelsStageAndRecordsNothing(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	opts := fastOpts
	opts.Workers = 1
	opts.Lease = 300 * time.Millisecond

	started := make(chan struct{})
	var cancelled atomic.Bool
	stage := newCountingStage(func(ctx context.Context, _ store.Article, call int) error {
		if call > 1 {
			return nil
		}
		close(started)
		<-ctx.Done()
		cancelled.Store(true)
		return errors.New("would be recorded if the lease were still held")
	})
	h := startRunner(t, s, stagesWith(store.StatusFetching, stage), opts)
	id := insert(t, s, "https://example.com/a")

	waitClosed(t, started, "stage start")
	// Simulate the lease being taken away (as if expired and reclaimed).
	if _, err := s.ReleaseAllLeases(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	a := h.waitFor(id, hasStatus(store.StatusReady))
	if !cancelled.Load() {
		t.Error("stage was not cancelled after losing its lease")
	}
	if a.Error != "" {
		t.Errorf("abandoned result was recorded: error = %q", a.Error)
	}
}

func TestManyArticlesEachStageRunsOnce(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	opts := fastOpts
	opts.Workers = 4
	counters := map[store.Status]*countingStage{}
	stages := map[store.Status]Stage{}
	for _, st := range store.Stages() {
		c := newCountingStage(nil)
		counters[st], stages[st] = c, c
	}
	h := startRunner(t, s, stages, opts)

	var ids []int64
	for i := range 25 {
		ids = append(ids, insert(t, s, fmt.Sprintf("https://example.com/%d", i)))
		h.runner.Notify()
	}
	for _, id := range ids {
		h.waitFor(id, hasStatus(store.StatusReady))
	}
	for st, c := range counters {
		for _, id := range ids {
			if n := c.count(id); n != 1 {
				t.Errorf("stage %s ran %d times for article %d", st, n, id)
			}
		}
	}
}

// With polling effectively disabled, Notify plus stage-completion hand-offs
// alone must get every article done (nothing strands waiting for a poll).
func TestNotifyAloneDrainsQueue(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	opts := fastOpts
	opts.PollInterval = time.Hour
	opts.Workers = 3
	h := startRunner(t, s, StubStages(0), opts)
	time.Sleep(50 * time.Millisecond) // let the workers go idle

	var ids []int64
	for i := range 3 {
		ids = append(ids, insert(t, s, fmt.Sprintf("https://example.com/%d", i)))
		h.runner.Notify()
	}
	for _, id := range ids {
		h.waitFor(id, hasStatus(store.StatusReady))
	}
}

// A dedicated one-worker pool for synthesis never runs two syntheses at once,
// and finishing scripting wakes that pool without waiting for a poll.
func TestPoolsIsolateStages(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	opts := fastOpts
	opts.PollInterval = time.Hour // hand-offs must come from wake-ups
	opts.Workers = 0
	opts.Pools = []Pool{
		{Workers: 3, Stages: []store.Status{store.StatusFetching, store.StatusExtracting, store.StatusScripting}},
		{Workers: 1, Stages: []store.Status{store.StatusSynthesizing}},
	}
	var inFlight, maxInFlight atomic.Int32
	synth := StageFunc(func(ctx context.Context, _ store.Article) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		select {
		case <-time.After(20 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	h := startRunner(t, s, stagesWith(store.StatusSynthesizing, synth), opts)
	time.Sleep(50 * time.Millisecond)

	var ids []int64
	for i := range 6 {
		ids = append(ids, insert(t, s, fmt.Sprintf("https://example.com/%d", i)))
		h.runner.Notify()
	}
	for _, id := range ids {
		h.waitFor(id, hasStatus(store.StatusReady))
	}
	if m := maxInFlight.Load(); m != 1 {
		t.Errorf("max concurrent syntheses = %d, want 1", m)
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	r := &Runner{opts: Options{BackoffBase: 30 * time.Second, BackoffMax: 5 * time.Minute}.withDefaults()}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if got := r.backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := r.backoff(1000); got != 5*time.Minute {
		t.Errorf("backoff(1000) = %s, want cap", got)
	}
}

func TestPermanent(t *testing.T) {
	t.Parallel()
	base := errors.New("gone")
	p := Permanent(base)
	if !IsPermanent(p) || !IsPermanent(fmt.Errorf("wrap: %w", p)) {
		t.Error("IsPermanent should see through wrapping")
	}
	if !errors.Is(p, base) {
		t.Error("Permanent should unwrap to the original error")
	}
	if IsPermanent(base) || Permanent(nil) != nil {
		t.Error("unexpected IsPermanent/Permanent(nil) behavior")
	}
}

// An article claimed after shutdown began is handed back uncounted, without
// running its stage. Driven directly through process, since the window in
// which a real claim races shutdown is tiny.
func TestClaimDuringShutdownIsReleasedNotRun(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	id := insert(t, s, "https://example.com/a")
	never := newCountingStage(nil)
	r, err := New(s, stagesWith(store.StatusFetching, never), slog.New(slog.NewTextHandler(io.Discard, nil)), fastOpts)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.ClaimArticle(context.Background(), time.Now(), fastOpts.Lease)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already under way
	r.process(ctx, r.logger, a)

	if n := never.count(id); n != 0 {
		t.Errorf("stage ran %d times after shutdown began", n)
	}
	got, err := s.GetArticle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFetching || got.Attempts != 0 || !got.LockedUntil.IsZero() {
		t.Errorf("after release: %+v (want fetching, 0 attempts, unleased)", got)
	}
}

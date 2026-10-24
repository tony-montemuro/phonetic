// Package pipeline runs articles through the processing stages. Workers claim
// articles from the store, run the stage each article is at while keeping the
// lease alive, and record the outcome: advance, retry with backoff, fail, or
// (on shutdown) release without counting the attempt.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/tony-montemuro/phonetic/internal/store"
)

// Stage processes one article at one pipeline stage. It must honor ctx: on
// cancellation (shutdown or lost lease) it should return promptly. Return an
// error wrapped with Permanent to fail the article without retrying. Panics in
// Run are recovered and retried; panics in goroutines a stage starts itself
// are not, and crash the process.
type Stage interface {
	Run(ctx context.Context, a store.Article) error
}

// StageFunc adapts a function to Stage.
type StageFunc func(ctx context.Context, a store.Article) error

// Run calls f.
func (f StageFunc) Run(ctx context.Context, a store.Article) error { return f(ctx, a) }

// permanentError marks a failure that retrying can't fix.
type permanentError struct{ err error }

// Permanent wraps err so the article fails immediately instead of retrying
// (e.g. the URL returns 404).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// IsPermanent reports whether err (or anything it wraps) was marked Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// Pool is a group of workers that only claim articles waiting for the given
// stages. Separate pools keep a slow or exclusive stage (e.g. GPU synthesis,
// one at a time) from tying up workers that could be fetching.
type Pool struct {
	Workers int
	Stages  []store.Status
}

// minLease keeps heartbeats (every Lease/3) meaningful at the store's
// millisecond precision.
const minLease = 100 * time.Millisecond

// Options tune the runner. Zero values select the defaults.
type Options struct {
	// Pools partition the stages among worker groups; each stage must
	// belong to exactly one pool. Default: one pool of Workers workers
	// handling every stage.
	Pools        []Pool
	Workers      int           // size of the default pool (only without Pools); default 2
	Lease        time.Duration // lease length, renewed every Lease/3; default 2m, minimum 100ms
	PollInterval time.Duration // idle wait between claims; default 2s
	MaxAttempts  int           // attempts per stage before failing; default 3
	BackoffBase  time.Duration // first retry delay, doubling; default 30s
	BackoffMax   time.Duration // retry delay cap; default 30m
	// WriteTimeout bounds each store write made after a stage finishes (with
	// a fresh context, so results are recorded even during shutdown).
	// Default 10s.
	WriteTimeout time.Duration
}

func (o Options) withDefaults() Options {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if len(o.Pools) == 0 {
		o.Pools = []Pool{{Workers: o.Workers, Stages: store.Stages()}}
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	def(&o.Lease, 2*time.Minute)
	def(&o.PollInterval, 2*time.Second)
	def(&o.BackoffBase, 30*time.Second)
	def(&o.BackoffMax, 30*time.Minute)
	def(&o.WriteTimeout, 10*time.Second)
	return o
}

// errLeaseLost is the cancellation cause when a heartbeat finds the lease gone.
var errLeaseLost = errors.New("lease lost")

// pool is a configured Pool with its wake-up channel.
type pool struct {
	Pool
	wake chan struct{}
}

// Runner owns the worker pools.
type Runner struct {
	store  *store.Store
	stages map[store.Status]Stage
	logger *slog.Logger
	opts   Options
	pools  []*pool
	// owner maps each stage to the pool that claims it.
	owner map[store.Status]*pool
}

// New returns a Runner. stages must implement every pipeline stage, and each
// stage must belong to exactly one pool.
func New(st *store.Store, stages map[store.Status]Stage, logger *slog.Logger, opts Options) (*Runner, error) {
	if len(opts.Pools) > 0 && opts.Workers != 0 {
		return nil, errors.New("pipeline: set either Workers or Pools, not both")
	}
	opts = opts.withDefaults()
	if opts.Lease < minLease {
		return nil, fmt.Errorf("pipeline: lease %s is below the minimum %s", opts.Lease, minLease)
	}
	r := &Runner{store: st, stages: stages, logger: logger, opts: opts, owner: map[store.Status]*pool{}}
	for i, p := range opts.Pools {
		if p.Workers <= 0 || len(p.Stages) == 0 {
			return nil, fmt.Errorf("pipeline: pool %d needs workers and stages", i)
		}
		rp := &pool{Pool: p, wake: make(chan struct{}, 1)}
		r.pools = append(r.pools, rp)
		for _, s := range p.Stages {
			if !s.IsStage() {
				return nil, fmt.Errorf("pipeline: pool %d: %q is not a stage", i, s)
			}
			if r.owner[s] != nil {
				return nil, fmt.Errorf("pipeline: stage %q is in more than one pool", s)
			}
			r.owner[s] = rp
		}
	}
	for _, s := range store.Stages() {
		if stages[s] == nil {
			return nil, fmt.Errorf("pipeline: no implementation for stage %q", s)
		}
		if r.owner[s] == nil {
			return nil, fmt.Errorf("pipeline: no pool handles stage %q", s)
		}
	}
	return r, nil
}

// Notify signals that new articles were queued, waking an idle worker in the
// pool that handles the first stage. It never blocks.
func (r *Runner) Notify() {
	r.wake(store.Stages()[0])
}

// wake wakes an idle worker in the pool that handles stage. If a wake-up is
// already pending, the worker that takes it will see this work too.
func (r *Runner) wake(stage store.Status) {
	select {
	case r.owner[stage].wake <- struct{}{}:
	default:
	}
}

// Run starts the workers and blocks until ctx is cancelled and every worker
// has finished. Interrupted stages have their leases released. A stage that
// ignores cancellation delays Run indefinitely, so callers should bound how
// long they wait for it during shutdown; any lease left behind is cleared by
// Store.ReleaseAllLeases at the next start.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for pi, p := range r.pools {
		for w := range p.Workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.work(ctx, p, r.logger.With("pool", pi, "worker", w))
			}()
		}
	}
	wg.Wait()
}

// work is one worker's loop.
func (r *Runner) work(ctx context.Context, p *pool, logger *slog.Logger) {
	for ctx.Err() == nil {
		// The claim runs on a non-cancellable context: SQLite commits the
		// claim on the statement's first step, so cancelling mid-call could
		// leave a committed claim this worker never sees or releases.
		a, err := r.store.ClaimArticle(context.WithoutCancel(ctx), time.Now(), r.opts.Lease, p.Stages...)
		switch {
		case err == nil:
			r.process(ctx, logger, a)
			continue // more work may be waiting (often this article's next stage)
		case errors.Is(err, store.ErrNoWork):
		default:
			logger.Error("claiming article", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-p.wake:
		case <-time.After(r.opts.PollInterval):
		}
	}
}

// process runs the claimed article's stage and records the outcome.
func (r *Runner) process(ctx context.Context, logger *slog.Logger, a store.Article) {
	logger = logger.With("article", a.ID, "stage", a.Status, "attempt", a.Attempts)
	lease := store.LeaseOf(a)

	// Attempts are counted at claim time, so an article whose stage keeps
	// crashing the whole process arrives here over the limit: fail it rather
	// than run it again.
	if a.Attempts > r.opts.MaxAttempts {
		msg := fmt.Sprintf("gave up after %d attempts (process crashed or stage was interrupted)", a.Attempts-1)
		r.write(logger, "article failed: attempts exhausted by interruptions", func(ctx context.Context) error {
			return r.store.FailArticle(ctx, lease, msg, time.Now())
		})
		return
	}

	// The claim ran on a non-cancellable context, so shutdown may have begun
	// while it was in flight (e.g. it picked up the next stage of an article
	// another worker just finished during shutdown). Don't start new work:
	// hand it back uncounted. This narrows rather than closes the gap: a
	// stage can still start with a just-cancelled ctx, which is safe because
	// stages must honor ctx.
	if ctx.Err() != nil {
		r.write(logger, "claimed during shutdown; lease released", func(ctx context.Context) error {
			return r.store.ReleaseLease(ctx, lease, time.Now())
		})
		return
	}

	logger.Info("stage started")
	start := time.Now()
	stageCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	hb := startHeartbeat(r.store, r.opts.Lease, r.opts.WriteTimeout, lease, cancel, logger)
	err := r.runStage(stageCtx, logger, a)
	hb.stop()
	now := time.Now()

	switch {
	case errors.Is(context.Cause(stageCtx), errLeaseLost):
		// Someone else owns the article now; record nothing.
		logger.Warn("lease lost during stage; abandoning result", "err", err)
	case err == nil:
		var next store.Status
		ok := r.write(logger, "stage completed", func(ctx context.Context) error {
			var werr error
			next, werr = r.store.CompleteStage(ctx, lease, now)
			return werr
		}, "duration", now.Sub(start))
		if ok && next != store.StatusReady {
			r.wake(next)
		}
	case ctx.Err() != nil:
		r.write(logger, "stage interrupted by shutdown; lease released", func(ctx context.Context) error {
			return r.store.ReleaseLease(ctx, lease, now)
		})
	case IsPermanent(err) || a.Attempts >= r.opts.MaxAttempts:
		r.write(logger, "article failed", func(ctx context.Context) error {
			return r.store.FailArticle(ctx, lease, err.Error(), now)
		}, "err", err, "permanent", IsPermanent(err))
	default:
		retryAt := now.Add(r.backoff(a.Attempts))
		r.write(logger, "stage failed; will retry", func(ctx context.Context) error {
			return r.store.RetryStage(ctx, lease, err.Error(), retryAt, now)
		}, "err", err, "retry_at", retryAt)
	}
}

// runStage runs the stage, converting a panic into an error so one bad
// article can't take down the process. The stack is logged, not stored.
func (r *Runner) runStage(ctx context.Context, logger *slog.Logger, a store.Article) (err error) {
	defer func() {
		if p := recover(); p != nil {
			logger.Error("stage panicked", "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("stage panicked: %v", p)
		}
	}()
	return r.stages[a.Status].Run(ctx, a)
}

// write performs one outcome write with a fresh, bounded context and logs
// msg with the result. It reports whether the write succeeded.
func (r *Runner) write(logger *slog.Logger, msg string, fn func(context.Context) error, args ...any) bool {
	ctx, cancel := context.WithTimeout(context.Background(), r.opts.WriteTimeout)
	defer cancel()
	err := fn(ctx)
	switch {
	case err == nil:
		logger.Info(msg, args...)
	case errors.Is(err, store.ErrLeaseLost):
		logger.Warn(msg+" (not recorded: lease lost)", args...)
	default:
		logger.Error(msg+" (not recorded)", append(args, "write_err", err)...)
	}
	return err == nil
}

// backoff returns the delay before retry number attempt (1-based):
// BackoffBase doubling each time, capped at BackoffMax.
func (r *Runner) backoff(attempt int) time.Duration {
	d := r.opts.BackoffBase
	for i := 1; i < attempt && d < r.opts.BackoffMax; i++ {
		d *= 2
	}
	return min(d, r.opts.BackoffMax)
}

// heartbeat keeps a lease alive while a stage runs, extending its expiry
// every length/3. If an extension finds the lease lost, it cancels the stage
// with errLeaseLost. The lease token never changes, so the heartbeat can't
// interfere with the worker's final write. It runs until stopped, even while
// a cancelled stage winds down.
type heartbeat struct {
	store        *store.Store
	lease        store.Lease
	length       time.Duration
	writeTimeout time.Duration
	cancel       context.CancelCauseFunc
	logger       *slog.Logger
	quit         chan struct{}
	done         chan struct{}
}

func startHeartbeat(st *store.Store, length, writeTimeout time.Duration, l store.Lease,
	cancel context.CancelCauseFunc, logger *slog.Logger) *heartbeat {
	hb := &heartbeat{
		store: st, lease: l, length: length, writeTimeout: writeTimeout,
		cancel: cancel, logger: logger,
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go hb.loop()
	return hb
}

func (hb *heartbeat) loop() {
	defer close(hb.done)
	t := time.NewTicker(hb.length / 3)
	defer t.Stop()
	for {
		select {
		case <-hb.quit:
			return
		case <-t.C:
		}
		if !hb.extend() {
			return
		}
	}
}

// extend renews the lease once. It returns false when the lease is lost.
func (hb *heartbeat) extend() bool {
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), hb.writeTimeout)
	defer cancel()
	err := hb.store.ExtendLease(ctx, hb.lease, now.Add(hb.length), now)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrLeaseLost):
		hb.cancel(errLeaseLost)
		return false
	default:
		// Transient; the lease may still be valid. Try again next tick.
		hb.logger.Warn("extending lease", "err", err)
	}
	return true
}

// stop ends the heartbeat and waits for it to exit.
func (hb *heartbeat) stop() {
	close(hb.quit)
	<-hb.done
}

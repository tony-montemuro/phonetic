// Command phonetic runs the article-to-podcast service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/tony-montemuro/phonetic/internal/config"
	"github.com/tony-montemuro/phonetic/internal/fetch"
	"github.com/tony-montemuro/phonetic/internal/pipeline"
	"github.com/tony-montemuro/phonetic/internal/server"
	"github.com/tony-montemuro/phonetic/internal/stages"
	"github.com/tony-montemuro/phonetic/internal/store"
)

const (
	// httpShutdownTimeout bounds how long in-flight requests get to finish.
	httpShutdownTimeout = 10 * time.Second
	// workerShutdownTimeout bounds how long interrupted stages get to wind
	// down and release their leases. A lease left behind is cleared at the
	// next start (and that attempt counts).
	workerShutdownTimeout = 30 * time.Second
)

func main() {
	// Only -h/-help; all settings come from the environment. Explicit help
	// goes to stdout (so it can be paged); usage errors go to stderr.
	flags := flag.NewFlagSet("phonetic", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	switch err := flags.Parse(os.Args[1:]); {
	case errors.Is(err, flag.ErrHelp):
		usage(os.Stdout)
		os.Exit(0)
	case err != nil:
		fmt.Fprintf(os.Stderr, "phonetic: %v\n\n", err)
		usage(os.Stderr)
		os.Exit(2)
	case flags.NArg() > 0:
		fmt.Fprintf(os.Stderr, "phonetic: unexpected arguments: %v\n\n", flags.Args())
		usage(os.Stderr)
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "phonetic:", err)
		os.Exit(1)
	}
}

// usage explains that phonetic is configured through environment variables
// (so secrets stay out of the process's visible command line) and lists them.
func usage(out io.Writer) {
	fmt.Fprint(out, `Usage: phonetic

phonetic turns article URLs into a private podcast. It takes no flags besides
-h; it is configured entirely through environment variables (in production,
a systemd EnvironmentFile):

`)
	_ = config.WriteUsage(out) // nowhere to report a failure to print help
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting phonetic", "version", version(), "addr", cfg.Addr, "data_dir", cfg.DataDir,
		"workers", cfg.Workers, "fetch_allow_private", cfg.FetchAllowPrivate, "fetch_blocked", cfg.FetchBlocked)

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	// Exclusive lock first: everything below assumes this is the only
	// phonetic process using the data dir.
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()

	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "phonetic.db"))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer func() {
		// Close performs the final WAL checkpoint; surface failures.
		if err := st.Close(); err != nil {
			logger.Error("closing store", "err", err)
		}
	}()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// We hold the data-dir lock, so any lease still held belongs to a
	// previous run that died; make that work runnable now, not after expiry.
	n, err := st.ReleaseAllLeases(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("releasing stale leases: %w", err)
	}
	if n > 0 {
		logger.Warn("released stale leases from a previous run", "count", n)
	}

	// TODO(phase-3): remove StubDelay with the last stub stages.
	if cfg.StubDelay > 0 {
		logger.Warn("stub stages sleep between steps (PHONETIC_DEV_STUB_DELAY)", "delay", cfg.StubDelay)
	}
	if cfg.FetchAllowPrivate {
		logger.Warn("fetching private and local addresses is allowed (PHONETIC_FETCH_ALLOW_PRIVATE): SSRF protection is off")
	}
	fetcher := fetch.New(fetch.Options{AllowPrivate: cfg.FetchAllowPrivate, Blocked: cfg.FetchBlocked})
	// Real stages replace the stubs as phases land.
	impls := pipeline.StubStages(cfg.StubDelay)
	impls[store.StatusFetching] = stages.Fetch{Fetcher: fetcher, DataDir: cfg.DataDir}
	impls[store.StatusExtracting] = stages.Extract{Store: st, DataDir: cfg.DataDir}
	runner, err := pipeline.New(st, impls, logger, pipeline.Options{Workers: cfg.Workers})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler: server.New(server.Deps{
			Logger:   logger,
			DB:       st,
			Articles: st,
			Notifier: runner,
			APIToken: cfg.APIToken,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: audio downloads can legitimately take a long time.
		// Handlers that need one set it with http.ResponseController.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// Workers get their own context so they can be stopped after HTTP.
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		runner.Run(workerCtx)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("listening", "addr", ln.Addr().String())

	var runErr error
	select {
	case err := <-serveErr:
		runErr = fmt.Errorf("serve: %w", err)
		logger.Error("server failed; shutting down", "err", err)
	case <-ctx.Done():
		logger.Info("shutting down", "cause", context.Cause(ctx))
	}
	// Restore default signal handling so a second signal force-exits.
	stop()

	// Shutdown order: HTTP (no new submissions), then workers (release
	// leases), then the store (deferred above).
	shutdownHTTP(logger, srv)
	stopWorkers()
	select {
	case <-workersDone:
	case <-time.After(workerShutdownTimeout):
		// Deliberately carry on: the deferred store close runs under the
		// stuck workers (their later calls fail with "database is closed"),
		// and the process exits right after. SQLite's atomic commits keep the
		// data consistent, and leftover leases are cleared at the next start.
		logger.Error("workers did not stop in time; abandoning them", "timeout", workerShutdownTimeout)
		runErr = errors.Join(runErr, errWorkersAbandoned)
	}
	logger.Info("stopped")
	return runErr
}

var errWorkersAbandoned = errors.New("pipeline workers did not stop within the shutdown timeout")

// lockDataDir takes an exclusive, non-blocking lock on a file in dir so two
// phonetic processes can never share a data dir (the second would, for
// example, release the first one's live leases). The lock is released by
// unlock or automatically when the process exits.
func lockDataDir(dir string) (unlock func(), err error) {
	path := filepath.Join(dir, "phonetic.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("data dir %s is in use by another phonetic process", dir)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// shutdownHTTP drains in-flight requests, then force-closes stragglers.
func shutdownHTTP(logger *slog.Logger, srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	switch {
	case err == nil:
		return
	case errors.Is(err, context.DeadlineExceeded):
		// Expected when a client is mid-download (e.g. a long audio file);
		// not a failure of the service.
		logger.Warn("graceful shutdown timed out; closing remaining connections", "err", err)
	default:
		logger.Error("http shutdown", "err", err)
	}
	if err := srv.Close(); err != nil {
		logger.Warn("closing server", "err", err)
	}
}

// version reports the module version the Go toolchain stamps from VCS state at
// build time, e.g. "v0.0.0-20261020011846-fb030f8626ff+dirty".
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

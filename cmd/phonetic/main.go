// Command phonetic runs the article-to-podcast service.
package main

import (
	"context"
	"fmt"
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
	"github.com/tony-montemuro/phonetic/internal/server"
	"github.com/tony-montemuro/phonetic/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "phonetic:", err)
		os.Exit(1)
	}
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

	logger.Info("starting phonetic", "version", version(), "addr", cfg.Addr, "data_dir", cfg.DataDir)

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
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
	srv := &http.Server{
		Handler:           server.New(server.Deps{Logger: logger, DB: st}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: audio downloads can legitimately take a long time.
		// Handlers that need one set it with http.ResponseController.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	// Restore default signal handling so a second signal force-exits.
	stop()
	logger.Info("shutting down", "cause", context.Cause(ctx))

	// Shutdown order: stop HTTP (drain, then force-close stragglers), then
	// (later phases) stop workers, then close the store via the defer above.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Expected when a client is mid-download (e.g. a long audio file);
		// not a failure of the service.
		logger.Warn("graceful shutdown timed out; closing remaining connections", "err", err)
		if err := srv.Close(); err != nil {
			logger.Warn("closing server", "err", err)
		}
	}
	logger.Info("stopped")
	return nil
}

// shutdownTimeout bounds how long in-flight requests get to finish.
const shutdownTimeout = 10 * time.Second

// version reports the module version the Go toolchain stamps from VCS state at
// build time, e.g. "v0.0.0-20261020011846-fb030f8626ff+dirty".
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

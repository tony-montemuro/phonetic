// Command phonetic runs the article-to-podcast service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/tony-montemuro/phonetic/internal/config"
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

	<-ctx.Done()
	// Restore default signal handling so a second signal force-exits.
	stop()
	logger.Info("shutting down", "cause", context.Cause(ctx))
	return nil
}

// version reports the module version the Go toolchain stamps from VCS state at
// build time, e.g. "v0.0.0-20261020011846-fb030f8626ff+dirty".
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

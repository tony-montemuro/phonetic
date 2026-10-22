// Package config loads phonetic's runtime configuration from environment
// variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults suitable for local development.
const (
	DefaultAddr    = "127.0.0.1:8080"
	DefaultDataDir = "data"
	DefaultWorkers = 2
)

// Limits on configured values.
const (
	MinAPITokenLen = 16
	MaxWorkers     = 16
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// Addr is the TCP address the HTTP server listens on (host:port).
	Addr string
	// DataDir holds the SQLite database and per-article artifacts. Always
	// absolute after Load.
	DataDir string
	// LogLevel is the minimum level that gets logged.
	LogLevel slog.Level
	// APIToken is the bearer token required by /api/. Required.
	APIToken string
	// Workers is the number of pipeline workers.
	Workers int
	// StubDelay makes the placeholder pipeline stages sleep this long each.
	// Development/testing aid only.
	// TODO(phase-3): remove with the stub stages.
	StubDelay time.Duration
}

// Load builds a Config from the given environment lookup function (normally
// os.Getenv). Unset or blank variables fall back to defaults. All problems are
// reported together.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:     DefaultAddr,
		DataDir:  DefaultDataDir,
		LogLevel: slog.LevelInfo,
		Workers:  DefaultWorkers,
	}
	var errs []error

	if v := lookup(getenv, "PHONETIC_ADDR"); v != "" {
		cfg.Addr = v
	}
	if err := validateAddr(cfg.Addr); err != nil {
		errs = append(errs, fmt.Errorf("PHONETIC_ADDR %q: %w", cfg.Addr, err))
	}

	if v := lookup(getenv, "PHONETIC_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	// Resolve now so a relative path is unambiguous in logs (under systemd
	// the working directory is "/").
	if abs, err := filepath.Abs(cfg.DataDir); err != nil {
		errs = append(errs, fmt.Errorf("PHONETIC_DATA_DIR %q: %w", cfg.DataDir, err))
	} else {
		cfg.DataDir = abs
	}

	if v := lookup(getenv, "PHONETIC_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("PHONETIC_LOG_LEVEL: %w", err))
		}
	}

	cfg.APIToken = lookup(getenv, "PHONETIC_API_TOKEN")
	if len(cfg.APIToken) < MinAPITokenLen {
		// Never echo the token itself.
		errs = append(errs, fmt.Errorf("PHONETIC_API_TOKEN: required, at least %d characters", MinAPITokenLen))
	}

	if v := lookup(getenv, "PHONETIC_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxWorkers {
			errs = append(errs, fmt.Errorf("PHONETIC_WORKERS %q: must be 1-%d", v, MaxWorkers))
		} else {
			cfg.Workers = n
		}
	}

	if v := lookup(getenv, "PHONETIC_DEV_STUB_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("PHONETIC_DEV_STUB_DELAY %q: must be a non-negative duration like 2s", v))
		} else {
			cfg.StubDelay = d
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func lookup(getenv func(string) string, key string) string {
	return strings.TrimSpace(getenv(key))
}

// validateAddr requires host:port with an explicit, non-zero numeric port, so
// a typo can't silently bind a random ephemeral port.
func validateAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("port must be a number in 1-65535, got %q", port)
	}
	return nil
}

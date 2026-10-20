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
}

// Defaults suitable for local development.
const (
	DefaultAddr    = "127.0.0.1:8080"
	DefaultDataDir = "data"
)

// Load builds a Config from the given environment lookup function (normally
// os.Getenv). Unset or blank variables fall back to defaults. All problems are
// reported together.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:     DefaultAddr,
		DataDir:  DefaultDataDir,
		LogLevel: slog.LevelInfo,
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

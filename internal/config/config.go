// Package config loads phonetic's runtime configuration from environment
// variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
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
	// FetchAllowPrivate lets the fetcher reach private and local addresses.
	// For local development and tests only: it disables SSRF protection, so
	// it is refused unless Addr is a loopback address.
	FetchAllowPrivate bool
	// FetchBlocked lists extra networks the fetcher must refuse, e.g. the
	// home's WAN address, which some routers serve their admin page on.
	FetchBlocked []netip.Prefix
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

	if v := lookup(getenv, "PHONETIC_FETCH_ALLOW_PRIVATE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("PHONETIC_FETCH_ALLOW_PRIVATE %q: must be true or false", v))
		}
		cfg.FetchAllowPrivate = b
		if b && !isLoopbackAddr(cfg.Addr) {
			errs = append(errs, fmt.Errorf("PHONETIC_FETCH_ALLOW_PRIVATE disables SSRF protection; it is only allowed when PHONETIC_ADDR is a loopback address (got %q)", cfg.Addr))
		}
	}

	if v := lookup(getenv, "PHONETIC_FETCH_BLOCKED"); v != "" {
		for _, item := range strings.Split(v, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			p, err := parsePrefix(item)
			if err != nil {
				errs = append(errs, fmt.Errorf("PHONETIC_FETCH_BLOCKED %q: %w", item, err))
				continue
			}
			cfg.FetchBlocked = append(cfg.FetchBlocked, p)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Shortest prefixes PHONETIC_FETCH_BLOCKED accepts: shorter would block a
// large slice of the internet, and blocked fetches fail permanently.
const (
	minBlockedBitsV4 = 8
	minBlockedBitsV6 = 16
)

// parsePrefix accepts a CIDR prefix or a bare address (as a single-host
// prefix). IPv4-mapped IPv6 forms become plain IPv4 (the fetcher compares
// unmapped addresses); zones and overly broad prefixes are rejected.
func parsePrefix(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return netip.Prefix{}, err
		}
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		if a.Zone() != "" {
			return netip.Prefix{}, errors.New("zones are not allowed")
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if a := p.Addr(); a.Is4In6() {
		if p.Bits() < 96 {
			return netip.Prefix{}, errors.New("IPv4-mapped prefix must be /96 or longer")
		}
		p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	if (p.Addr().Is4() && p.Bits() < minBlockedBitsV4) || (p.Addr().Is6() && p.Bits() < minBlockedBitsV6) {
		return netip.Prefix{}, fmt.Errorf("prefix /%d is too broad", p.Bits())
	}
	return p.Masked(), nil
}

// isLoopbackAddr reports whether a host:port address listens on loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
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

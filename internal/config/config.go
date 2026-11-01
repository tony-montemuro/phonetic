// Package config loads phonetic's runtime configuration from environment
// variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tony-montemuro/phonetic/internal/tts"
)

// Defaults suitable for local development.
const (
	DefaultAddr     = "127.0.0.1:8080"
	DefaultDataDir  = "data"
	DefaultWorkers  = 2
	DefaultFeedUser = "phonetic"
)

// Limits on configured values.
const (
	MinSecretLen = 16 // API token and feed password
	MaxWorkers   = 16
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
	// PublicURL is the base URL clients reach phonetic at (feed, audio and
	// artwork links are built from it), e.g. https://pc.tailnet.ts.net or
	// http://10.8.0.1:8080 over WireGuard. No trailing slash. Defaults to
	// http://<Addr>.
	PublicURL string
	// Feed holds the HTTP basic auth credentials for the feed and audio.
	Feed Feed
	// Workers is the number of pipeline workers.
	Workers int
	// FetchAllowPrivate lets the fetcher reach private and local addresses.
	// For local development and tests only: it disables SSRF protection, so
	// it is refused unless Addr is a loopback address.
	FetchAllowPrivate bool
	// FetchBlocked lists extra networks the fetcher must refuse, e.g. the
	// home's WAN address, which some routers serve their admin page on.
	FetchBlocked []netip.Prefix
	// TTS configures the OpenAI-compatible speech API (Kokoro-FastAPI by
	// default).
	TTS TTS
}

// Feed holds the podcast feed's basic auth credentials.
type Feed struct {
	User     string
	Password string // never logged
}

// TTS configures the speech engine.
type TTS struct {
	URL    string // base URL, e.g. http://127.0.0.1:8880/v1
	Model  string
	Voice  string
	Speed  float64
	APIKey string // optional bearer token (hosted APIs); never logged
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
		Feed:     Feed{User: DefaultFeedUser},
		TTS: TTS{
			URL:   tts.DefaultBaseURL,
			Model: tts.DefaultModel,
			Voice: tts.DefaultVoice,
			Speed: tts.DefaultSpeed,
		},
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
	if len(cfg.APIToken) < MinSecretLen {
		// Never echo the token itself.
		errs = append(errs, fmt.Errorf("PHONETIC_API_TOKEN: required, at least %d characters", MinSecretLen))
	}

	if v := lookup(getenv, "PHONETIC_PUBLIC_URL"); v != "" {
		if err := validateHTTPURL(v); err != nil {
			errs = append(errs, fmt.Errorf("PHONETIC_PUBLIC_URL %q: %w", v, err))
		} else {
			// Normalized (e.g. spaces escaped), so links built on it are valid.
			u, _ := url.Parse(v)
			cfg.PublicURL = strings.TrimRight(u.String(), "/")
		}
	} else if host, _, err := net.SplitHostPort(cfg.Addr); err == nil {
		if a, err := netip.ParseAddr(host); host == "" || (err == nil && a.IsUnspecified()) {
			errs = append(errs, fmt.Errorf("PHONETIC_PUBLIC_URL: required when PHONETIC_ADDR (%q) listens on all addresses", cfg.Addr))
		} else {
			cfg.PublicURL = (&url.URL{Scheme: "http", Host: cfg.Addr}).String()
		}
	}

	if v := lookup(getenv, "PHONETIC_FEED_USER"); v != "" {
		cfg.Feed.User = v
	}
	if strings.Contains(cfg.Feed.User, ":") {
		errs = append(errs, errors.New("PHONETIC_FEED_USER: must not contain a colon (HTTP basic auth)"))
	}
	cfg.Feed.Password = lookup(getenv, "PHONETIC_FEED_PASSWORD")
	if len(cfg.Feed.Password) < MinSecretLen {
		// Never echo the password itself.
		errs = append(errs, fmt.Errorf("PHONETIC_FEED_PASSWORD: required, at least %d characters", MinSecretLen))
	}

	if v := lookup(getenv, "PHONETIC_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxWorkers {
			errs = append(errs, fmt.Errorf("PHONETIC_WORKERS %q: must be 1-%d", v, MaxWorkers))
		} else {
			cfg.Workers = n
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

	if v := lookup(getenv, "PHONETIC_TTS_URL"); v != "" {
		cfg.TTS.URL = v
	}
	if err := validateHTTPURL(cfg.TTS.URL); err != nil {
		errs = append(errs, fmt.Errorf("PHONETIC_TTS_URL %q: %w", cfg.TTS.URL, err))
	}
	if v := lookup(getenv, "PHONETIC_TTS_MODEL"); v != "" {
		cfg.TTS.Model = v
	}
	if v := lookup(getenv, "PHONETIC_TTS_VOICE"); v != "" {
		cfg.TTS.Voice = v
	}
	if v := lookup(getenv, "PHONETIC_TTS_SPEED"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f >= tts.MinSpeed && f <= tts.MaxSpeed) {
			errs = append(errs, fmt.Errorf("PHONETIC_TTS_SPEED %q: must be a number from %g to %g", v, tts.MinSpeed, tts.MaxSpeed))
		} else {
			cfg.TTS.Speed = f
		}
	}
	cfg.TTS.APIKey = lookup(getenv, "PHONETIC_TTS_API_KEY")
	if strings.ContainsFunc(cfg.TTS.APIKey, func(r rune) bool { return r <= ' ' || r > '~' }) {
		// Never echo the key itself.
		errs = append(errs, errors.New("PHONETIC_TTS_API_KEY: must be printable ASCII without spaces"))
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

// validateHTTPURL requires an absolute http(s) URL with a host, a valid
// port if any, and no credentials, query or fragment (paths are appended to
// it).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("must be an absolute http(s) URL with a host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
			return fmt.Errorf("port must be a number in 1-65535, got %q", p)
		}
	}
	if u.User != nil || strings.ContainsAny(raw, "?#") {
		return errors.New("must not contain credentials, a query or a fragment")
	}
	return nil
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

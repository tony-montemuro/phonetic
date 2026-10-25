package config

import (
	"log/slog"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const validToken = "0123456789abcdef"

// env returns a getenv over m, with a valid API token unless m sets one.
func env(m map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		if k == "PHONETIC_API_TOKEN" {
			return validToken
		}
		return ""
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	want := Config{Addr: DefaultAddr, DataDir: mustAbs(t, DefaultDataDir), LogLevel: slog.LevelInfo,
		APIToken: validToken, Workers: DefaultWorkers}
	for name, e := range map[string]map[string]string{
		"unset": nil,
		"blank": {"PHONETIC_ADDR": "  ", "PHONETIC_DATA_DIR": "\t", "PHONETIC_LOG_LEVEL": " ",
			"PHONETIC_WORKERS": " ", "PHONETIC_DEV_STUB_DELAY": " "},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(cfg, want) {
				t.Errorf("got %+v, want %+v", cfg, want)
			}
		})
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{
		"PHONETIC_ADDR":           " 100.64.0.1:9000 ",
		"PHONETIC_DATA_DIR":       "/var/lib/phonetic",
		"PHONETIC_LOG_LEVEL":      "debug",
		"PHONETIC_API_TOKEN":      " a-much-longer-secret-token ",
		"PHONETIC_WORKERS":        "4",
		"PHONETIC_DEV_STUB_DELAY": "1500ms",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{Addr: "100.64.0.1:9000", DataDir: "/var/lib/phonetic", LogLevel: slog.LevelDebug,
		APIToken: "a-much-longer-secret-token", Workers: 4, StubDelay: 1500 * time.Millisecond}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadValidAddrs(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", ":8080", "localhost:65535"} {
		if _, err := Load(env(map[string]string{"PHONETIC_ADDR": addr})); err != nil {
			t.Errorf("addr %q: unexpected error %v", addr, err)
		}
	}
}

func TestLoadRelativeDataDirIsResolved(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{"PHONETIC_DATA_DIR": "some/rel"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !filepath.IsAbs(cfg.DataDir) || cfg.DataDir != mustAbs(t, "some/rel") {
		t.Errorf("DataDir = %q, want absolute form of some/rel", cfg.DataDir)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      map[string]string
		wantErrs []string
	}{
		{"addr missing port", map[string]string{"PHONETIC_ADDR": "localhost"}, []string{"PHONETIC_ADDR"}},
		{"addr empty port", map[string]string{"PHONETIC_ADDR": "localhost:"}, []string{"PHONETIC_ADDR"}},
		{"addr named port", map[string]string{"PHONETIC_ADDR": ":http"}, []string{"PHONETIC_ADDR"}},
		{"addr port out of range", map[string]string{"PHONETIC_ADDR": ":99999"}, []string{"PHONETIC_ADDR"}},
		{"addr port zero", map[string]string{"PHONETIC_ADDR": ":0"}, []string{"PHONETIC_ADDR"}},
		{"bad log level", map[string]string{"PHONETIC_LOG_LEVEL": "loud"}, []string{"PHONETIC_LOG_LEVEL"}},
		{"missing token", map[string]string{"PHONETIC_API_TOKEN": ""}, []string{"PHONETIC_API_TOKEN"}},
		{"short token", map[string]string{"PHONETIC_API_TOKEN": "short"}, []string{"PHONETIC_API_TOKEN"}},
		{"15-char token", map[string]string{"PHONETIC_API_TOKEN": "0123456789abcde"}, []string{"PHONETIC_API_TOKEN"}},
		{"blank token", map[string]string{"PHONETIC_API_TOKEN": "      "}, []string{"PHONETIC_API_TOKEN"}},
		{"workers zero", map[string]string{"PHONETIC_WORKERS": "0"}, []string{"PHONETIC_WORKERS"}},
		{"workers too many", map[string]string{"PHONETIC_WORKERS": "17"}, []string{"PHONETIC_WORKERS"}},
		{"workers not a number", map[string]string{"PHONETIC_WORKERS": "two"}, []string{"PHONETIC_WORKERS"}},
		{"bad stub delay", map[string]string{"PHONETIC_DEV_STUB_DELAY": "soon"}, []string{"PHONETIC_DEV_STUB_DELAY"}},
		{"negative stub delay", map[string]string{"PHONETIC_DEV_STUB_DELAY": "-1s"}, []string{"PHONETIC_DEV_STUB_DELAY"}},
		{"bad allow-private", map[string]string{"PHONETIC_FETCH_ALLOW_PRIVATE": "sure"}, []string{"PHONETIC_FETCH_ALLOW_PRIVATE"}},
		{"bad blocked prefix", map[string]string{"PHONETIC_FETCH_BLOCKED": "1.2.3.4/33"}, []string{"PHONETIC_FETCH_BLOCKED"}},
		{"bad blocked address", map[string]string{"PHONETIC_FETCH_BLOCKED": "203.0.113.7, nope"}, []string{"PHONETIC_FETCH_BLOCKED"}},
		{"blocked /0", map[string]string{"PHONETIC_FETCH_BLOCKED": "0.0.0.0/0"}, []string{"too broad"}},
		{"blocked ::/0", map[string]string{"PHONETIC_FETCH_BLOCKED": "::/0"}, []string{"too broad"}},
		{"blocked /4", map[string]string{"PHONETIC_FETCH_BLOCKED": "16.0.0.0/4"}, []string{"too broad"}},
		{"blocked zone", map[string]string{"PHONETIC_FETCH_BLOCKED": "fe80::1%eth0"}, []string{"zones"}},
		{"blocked short mapped", map[string]string{"PHONETIC_FETCH_BLOCKED": "::ffff:0:0/80"}, []string{"IPv4-mapped"}},
		{"allow-private off loopback", map[string]string{"PHONETIC_ADDR": "100.64.0.1:8080", "PHONETIC_FETCH_ALLOW_PRIVATE": "true"}, []string{"loopback"}},
		{
			"all errors reported together",
			map[string]string{"PHONETIC_ADDR": "nope", "PHONETIC_LOG_LEVEL": "loud"},
			[]string{"PHONETIC_ADDR", "PHONETIC_LOG_LEVEL"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			for _, want := range tt.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestLoadErrorNeverEchoesToken(t *testing.T) {
	t.Parallel()
	_, err := Load(env(map[string]string{"PHONETIC_API_TOKEN": "s3cr3t", "PHONETIC_WORKERS": "lots"}))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error leaks token: %v", err)
	}
}

func TestLoadFetchSettings(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{
		"PHONETIC_FETCH_ALLOW_PRIVATE": "true",
		"PHONETIC_FETCH_BLOCKED":       " 203.0.113.7 , 2001:db8:1::/48,, 198.51.100.9/24, ::ffff:192.0.2.0/120 ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("203.0.113.7/32"),
		netip.MustParsePrefix("2001:db8:1::/48"),
		netip.MustParsePrefix("198.51.100.0/24"), // masked
		netip.MustParsePrefix("192.0.2.0/24"),    // mapped form converted to IPv4
	}
	if !cfg.FetchAllowPrivate || !slices.Equal(cfg.FetchBlocked, want) {
		t.Errorf("fetch settings = %v, %v; want true, %v", cfg.FetchAllowPrivate, cfg.FetchBlocked, want)
	}

	def, err := Load(env(nil))
	if err != nil || def.FetchAllowPrivate || def.FetchBlocked != nil {
		t.Errorf("defaults = %v, %v (%v); want SSRF protection on, no extra blocks", def.FetchAllowPrivate, def.FetchBlocked, err)
	}
}

func TestAllowPrivateRequiresLoopback(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:9000"} {
		if _, err := Load(env(map[string]string{"PHONETIC_ADDR": addr, "PHONETIC_FETCH_ALLOW_PRIVATE": "true"})); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "100.101.102.103:8080"} {
		if _, err := Load(env(map[string]string{"PHONETIC_ADDR": addr, "PHONETIC_FETCH_ALLOW_PRIVATE": "true"})); err == nil {
			t.Errorf("%s: allow-private accepted on a non-loopback address", addr)
		}
	}
}

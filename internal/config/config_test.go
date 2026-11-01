package config

import (
	"log/slog"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	validToken    = "0123456789abcdef"
	validPassword = "fedcba9876543210"
)

// env returns a getenv over m, with a valid API token and feed password
// unless m sets them.
func env(m map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		switch k {
		case "PHONETIC_API_TOKEN":
			return validToken
		case "PHONETIC_FEED_PASSWORD":
			return validPassword
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
		APIToken: validToken, Workers: DefaultWorkers, PublicURL: "http://" + DefaultAddr,
		Feed: Feed{User: "phonetic", Password: validPassword},
		TTS:  TTS{URL: "http://127.0.0.1:8880/v1", Model: "kokoro", Voice: "af_heart", Speed: 1}}
	for name, e := range map[string]map[string]string{
		"unset": nil,
		"blank": {"PHONETIC_ADDR": "  ", "PHONETIC_DATA_DIR": "\t", "PHONETIC_LOG_LEVEL": " ",
			"PHONETIC_WORKERS": " ", "PHONETIC_TTS_URL": " ",
			"PHONETIC_TTS_MODEL": " ", "PHONETIC_TTS_VOICE": " ", "PHONETIC_TTS_SPEED": " ", "PHONETIC_TTS_API_KEY": " "},
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
		"PHONETIC_ADDR":        " 100.64.0.1:9000 ",
		"PHONETIC_DATA_DIR":    "/var/lib/phonetic",
		"PHONETIC_LOG_LEVEL":   "debug",
		"PHONETIC_API_TOKEN":   " a-much-longer-secret-token ",
		"PHONETIC_PUBLIC_URL":  "https://pc.example.ts.net/",
		"PHONETIC_FEED_USER":   "me",
		"PHONETIC_WORKERS":     "4",
		"PHONETIC_TTS_URL":     "https://api.openai.com/v1",
		"PHONETIC_TTS_MODEL":   "tts-1",
		"PHONETIC_TTS_VOICE":   "alloy",
		"PHONETIC_TTS_SPEED":   "1.25",
		"PHONETIC_TTS_API_KEY": " sk-secret ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{Addr: "100.64.0.1:9000", DataDir: "/var/lib/phonetic", LogLevel: slog.LevelDebug,
		APIToken: "a-much-longer-secret-token", Workers: 4, PublicURL: "https://pc.example.ts.net",
		Feed: Feed{User: "me", Password: validPassword},
		TTS:  TTS{URL: "https://api.openai.com/v1", Model: "tts-1", Voice: "alloy", Speed: 1.25, APIKey: "sk-secret"}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadValidAddrs(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", ":8080", "localhost:65535"} {
		if _, err := Load(env(map[string]string{"PHONETIC_ADDR": addr, "PHONETIC_PUBLIC_URL": "https://pc.example"})); err != nil {
			t.Errorf("addr %q: unexpected error %v", addr, err)
		}
	}
}

// Without PHONETIC_PUBLIC_URL, links point at the listening address.
func TestLoadDefaultPublicURL(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]string{
		"127.0.0.1:8080":     "http://127.0.0.1:8080",
		"[::1]:9000":         "http://[::1]:9000",
		"10.8.0.1:8080":      "http://10.8.0.1:8080",
		"[fe80::1%en0]:8080": "http://[fe80::1%25en0]:8080",
	} {
		cfg, err := Load(env(map[string]string{"PHONETIC_ADDR": addr}))
		if err != nil || cfg.PublicURL != want {
			t.Errorf("addr %s: PublicURL = %q, %v; want %q", addr, cfg.PublicURL, err, want)
		}
	}
}

func TestLoadPublicURLIsNormalized(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(map[string]string{"PHONETIC_PUBLIC_URL": "https://pc.example/my podcasts//"}))
	if err != nil || cfg.PublicURL != "https://pc.example/my%20podcasts" {
		t.Errorf("PublicURL = %q, %v", cfg.PublicURL, err)
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
		{"missing feed password", map[string]string{"PHONETIC_FEED_PASSWORD": ""}, []string{"PHONETIC_FEED_PASSWORD"}},
		{"short feed password", map[string]string{"PHONETIC_FEED_PASSWORD": "short"}, []string{"PHONETIC_FEED_PASSWORD"}},
		{"feed user with colon", map[string]string{"PHONETIC_FEED_USER": "a:b"}, []string{"PHONETIC_FEED_USER"}},
		{"public url relative", map[string]string{"PHONETIC_PUBLIC_URL": "pc.example.ts.net"}, []string{"PHONETIC_PUBLIC_URL"}},
		{"public url query", map[string]string{"PHONETIC_PUBLIC_URL": "https://pc/?x"}, []string{"PHONETIC_PUBLIC_URL"}},
		{"public url needed for wildcard addr", map[string]string{"PHONETIC_ADDR": "0.0.0.0:8080"}, []string{"PHONETIC_PUBLIC_URL"}},
		{"public url needed for empty host", map[string]string{"PHONETIC_ADDR": ":8080"}, []string{"PHONETIC_PUBLIC_URL"}},
		{"missing token", map[string]string{"PHONETIC_API_TOKEN": ""}, []string{"PHONETIC_API_TOKEN"}},
		{"short token", map[string]string{"PHONETIC_API_TOKEN": "short"}, []string{"PHONETIC_API_TOKEN"}},
		{"15-char token", map[string]string{"PHONETIC_API_TOKEN": "0123456789abcde"}, []string{"PHONETIC_API_TOKEN"}},
		{"blank token", map[string]string{"PHONETIC_API_TOKEN": "      "}, []string{"PHONETIC_API_TOKEN"}},
		{"workers zero", map[string]string{"PHONETIC_WORKERS": "0"}, []string{"PHONETIC_WORKERS"}},
		{"workers too many", map[string]string{"PHONETIC_WORKERS": "17"}, []string{"PHONETIC_WORKERS"}},
		{"workers not a number", map[string]string{"PHONETIC_WORKERS": "two"}, []string{"PHONETIC_WORKERS"}},
		{"bad allow-private", map[string]string{"PHONETIC_FETCH_ALLOW_PRIVATE": "sure"}, []string{"PHONETIC_FETCH_ALLOW_PRIVATE"}},
		{"bad blocked prefix", map[string]string{"PHONETIC_FETCH_BLOCKED": "1.2.3.4/33"}, []string{"PHONETIC_FETCH_BLOCKED"}},
		{"bad blocked address", map[string]string{"PHONETIC_FETCH_BLOCKED": "203.0.113.7, nope"}, []string{"PHONETIC_FETCH_BLOCKED"}},
		{"blocked /0", map[string]string{"PHONETIC_FETCH_BLOCKED": "0.0.0.0/0"}, []string{"too broad"}},
		{"blocked ::/0", map[string]string{"PHONETIC_FETCH_BLOCKED": "::/0"}, []string{"too broad"}},
		{"blocked /4", map[string]string{"PHONETIC_FETCH_BLOCKED": "16.0.0.0/4"}, []string{"too broad"}},
		{"blocked zone", map[string]string{"PHONETIC_FETCH_BLOCKED": "fe80::1%eth0"}, []string{"zones"}},
		{"blocked short mapped", map[string]string{"PHONETIC_FETCH_BLOCKED": "::ffff:0:0/80"}, []string{"IPv4-mapped"}},
		{"tts url relative", map[string]string{"PHONETIC_TTS_URL": "localhost:8880/v1"}, []string{"PHONETIC_TTS_URL"}},
		{"tts url ftp", map[string]string{"PHONETIC_TTS_URL": "ftp://host/v1"}, []string{"PHONETIC_TTS_URL"}},
		{"tts url query", map[string]string{"PHONETIC_TTS_URL": "http://host/v1?x=1"}, []string{"PHONETIC_TTS_URL"}},
		{"tts url bare query", map[string]string{"PHONETIC_TTS_URL": "http://host/v1?"}, []string{"PHONETIC_TTS_URL"}},
		{"tts url bare fragment", map[string]string{"PHONETIC_TTS_URL": "http://host/v1#"}, []string{"PHONETIC_TTS_URL"}},
		{"tts url no hostname", map[string]string{"PHONETIC_TTS_URL": "http://:8880/v1"}, []string{"PHONETIC_TTS_URL"}},
		{"tts url bad port", map[string]string{"PHONETIC_TTS_URL": "http://host:99999/v1"}, []string{"PHONETIC_TTS_URL"}},
		{"tts api key with space", map[string]string{"PHONETIC_TTS_API_KEY": "sk-ab cd"}, []string{"PHONETIC_TTS_API_KEY"}},
		{"tts api key with control", map[string]string{"PHONETIC_TTS_API_KEY": "sk-ab\rcd"}, []string{"PHONETIC_TTS_API_KEY"}},
		{"tts url credentials", map[string]string{"PHONETIC_TTS_URL": "http://u:p@host/v1"}, []string{"PHONETIC_TTS_URL"}},
		{"tts speed too slow", map[string]string{"PHONETIC_TTS_SPEED": "0.1"}, []string{"PHONETIC_TTS_SPEED"}},
		{"tts speed too fast", map[string]string{"PHONETIC_TTS_SPEED": "5"}, []string{"PHONETIC_TTS_SPEED"}},
		{"tts speed NaN", map[string]string{"PHONETIC_TTS_SPEED": "NaN"}, []string{"PHONETIC_TTS_SPEED"}},
		{"tts speed word", map[string]string{"PHONETIC_TTS_SPEED": "fast"}, []string{"PHONETIC_TTS_SPEED"}},
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
	_, err := Load(env(map[string]string{"PHONETIC_API_TOKEN": "s3cr3t", "PHONETIC_FEED_PASSWORD": "pa55", "PHONETIC_WORKERS": "lots"}))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "pa55") {
		t.Errorf("error leaks a secret: %v", err)
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

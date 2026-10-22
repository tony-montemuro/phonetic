package config

import (
	"log/slog"
	"path/filepath"
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
			if cfg != want {
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
	if cfg != want {
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

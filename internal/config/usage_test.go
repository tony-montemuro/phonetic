package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Vars must list exactly the variables Load reads, so `phonetic -h` can't
// drift from the real configuration.
func TestVarsMatchWhatLoadReads(t *testing.T) {
	t.Parallel()
	// Probe with every variable unset and with every variable set, so a read
	// that only happens on one of those paths is still seen.
	var read []string
	record := func(value func(string) string) func(string) string {
		return func(k string) string {
			if !slices.Contains(read, k) {
				read = append(read, k)
			}
			return value(k)
		}
	}
	_, _ = Load(record(env(nil)))
	_, _ = Load(record(func(string) string { return "x" }))
	var documented []string
	for _, v := range Vars {
		documented = append(documented, v.Name)
	}
	slices.Sort(read)
	slices.Sort(documented)
	if !slices.Equal(read, documented) {
		t.Errorf("Load reads %v\nVars documents %v", read, documented)
	}
}

func TestWriteUsage(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	if err := WriteUsage(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, v := range Vars {
		if !strings.Contains(out, v.Name) || !strings.Contains(out, v.Help) {
			t.Errorf("usage missing %s", v.Name)
		}
	}
	if !strings.Contains(out, "127.0.0.1:8080") || !strings.Contains(out, "(required)") {
		t.Errorf("defaults missing from usage:\n%s", out)
	}
}

// The defaults shown for these variables are literals; keep them honest.
func TestUsageDefaultsMatchLoad(t *testing.T) {
	t.Parallel()
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]string{}
	for _, v := range Vars {
		shown[v.Name] = v.Default
	}
	if want := strings.ToLower(cfg.LogLevel.String()); shown["PHONETIC_LOG_LEVEL"] != want {
		t.Errorf("log level default shown %q, actual %q", shown["PHONETIC_LOG_LEVEL"], want)
	}
	if shown["PHONETIC_FETCH_ALLOW_PRIVATE"] != fmt.Sprint(cfg.FetchAllowPrivate) {
		t.Errorf("allow-private default shown %q, actual %v", shown["PHONETIC_FETCH_ALLOW_PRIVATE"], cfg.FetchAllowPrivate)
	}
	for name, actual := range map[string]string{
		"PHONETIC_TTS_URL":     cfg.TTS.URL,
		"PHONETIC_TTS_MODEL":   cfg.TTS.Model,
		"PHONETIC_TTS_VOICE":   cfg.TTS.Voice,
		"PHONETIC_TTS_SPEED":   fmt.Sprint(cfg.TTS.Speed),
		"PHONETIC_TTS_API_KEY": cfg.TTS.APIKey,
	} {
		if shown[name] != actual {
			t.Errorf("%s default shown %q, actual %q", name, shown[name], actual)
		}
	}
	if cfg.FetchBlocked != nil || shown["PHONETIC_FETCH_BLOCKED"] != "" {
		t.Errorf("blocked default shown %q, actual %v", shown["PHONETIC_FETCH_BLOCKED"], cfg.FetchBlocked)
	}
}

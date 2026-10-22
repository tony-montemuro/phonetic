package urlnorm

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// knownTracking is an independent, hard-coded copy of the tracking keys the
// fuzz test accepts as droppable, so the property doesn't trust the code under
// test. Entries ending in "_" are prefixes.
var knownTracking = []string{
	"utm_", "fbclid", "gclid", "dclid", "msclkid", "yclid", "twclid",
	"mc_cid", "mc_eid", "_hsenc", "_hsmi", "igshid", "si",
}

func looksTracking(pair string) bool {
	if strings.Contains(pair, ";") {
		return false
	}
	rawKey, _, _ := strings.Cut(pair, "=")
	key, err := url.QueryUnescape(rawKey)
	if err != nil {
		return false
	}
	for _, p := range knownTracking {
		if key == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(key, p)) {
			return true
		}
	}
	return false
}

// rawPairs returns the non-empty raw query pairs of rawURL.
func rawPairs(t *testing.T, rawURL string) []string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, pair := range strings.Split(u.RawQuery, "&") {
		if pair != "" {
			out = append(out, pair)
		}
	}
	return out
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"https://example.com/a", "https://example.com/a"},
		{"  https://example.com/a  ", "https://example.com/a"},
		{"HTTPS://Example.COM/Path", "https://example.com/Path"}, // path case kept
		{"https://example.com", "https://example.com/"},
		{"https://example.com.:443/a", "https://example.com/a"},
		{"http://example.com:80/a", "http://example.com/a"},
		{"https://example.com:8443/a", "https://example.com:8443/a"},
		{"https://example.com:08443/a", "https://example.com:8443/a"}, // leading zero
		{"https://example.com:0443/a", "https://example.com/a"},
		{"http://example.com:443/a", "http://example.com:443/a"}, // non-default for http
		{"https://example.com/a/", "https://example.com/a/"},     // trailing slash kept
		{"https://example.com/a#section-2", "https://example.com/a"},
		{"https://example.com/#!/post/1", "https://example.com/#!/post/1"}, // hash-bang route kept
		{"https://example.com/a?", "https://example.com/a"},
		// Query kept byte-for-byte and in order; only tracking pairs go.
		{"https://example.com/a?b=2&a=1", "https://example.com/a?b=2&a=1"},
		{"https://example.com/a?x=2&x=1", "https://example.com/a?x=2&x=1"},
		{"https://example.com/a?utm_source=x&id=7&fbclid=z", "https://example.com/a?id=7"},
		{"https://example.com/a?gclid=1&mc_cid=2", "https://example.com/a"},
		{"https://example.com/a?UTM_Medium=y", "https://example.com/a?UTM_Medium=y"}, // case-sensitive
		{"https://example.com/a?utmost=1", "https://example.com/a?utmost=1"},         // prefix needs "utm_"
		{"https://example.com/a?q=a%20b", "https://example.com/a?q=a%20b"},           // no +/%20 rewrite
		{"https://example.com/a?q=a+b", "https://example.com/a?q=a+b"},
		{"https://example.com/index.php?Article-Name", "https://example.com/index.php?Article-Name"}, // bare key
		{"https://example.com/a?p/123", "https://example.com/a?p/123"},
		{"https://example.com/a?id=1;page=2", "https://example.com/a?id=1;page=2"},             // semicolons kept
		{"https://example.com/a?id=%zz&p=1", "https://example.com/a?id=%zz&p=1"},               // bad escape kept
		{"https://example.com/a?a=1&&b=2&", "https://example.com/a?a=1&b=2"},                   // empty pairs dropped
		{"https://example.com/a?%zz=1&utm_x=2", "https://example.com/a?%zz=1"},                 // unparseable key kept
		{"https://example.com/a?utm_source=x;id=1", "https://example.com/a?utm_source=x;id=1"}, // ';' pair never dropped
		// Bytes illegal in a URL are escaped; nothing is decoded.
		{"https://example.com/a?q=a b", "https://example.com/a?q=a%20b"},
		{"https://example.com/a?q=a #f", "https://example.com/a?q=a%20"},
		{"http://0? #", "http://0/?%20"},
		{"https://example.com/a?x=é", "https://example.com/a?x=%C3%A9"},
		{"https://example.com/a?x=%C3%A9", "https://example.com/a?x=%C3%A9"},
		{`https://example.com/a?x="<>`, "https://example.com/a?x=%22%3C%3E"},
		// Host-scoped tracking.
		{"https://www.youtube.com/watch?v=abc&si=xyz", "https://www.youtube.com/watch?v=abc"},
		{"https://youtu.be/abc?si=xyz", "https://youtu.be/abc"},
		{"https://example.com/a?si=3&p=1", "https://example.com/a?si=3&p=1"},
		// IPv6.
		{"https://[2001:DB8::1]:443/a", "https://[2001:db8::1]/a"},
		{"https://[2001:db8::1]:8443/a", "https://[2001:db8::1]:8443/a"},
		{"https://example.com/caf%C3%A9", "https://example.com/caf%C3%A9"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := Normalize(tt.in)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			again, err := Normalize(got)
			if err != nil || again != got {
				t.Errorf("not a fixed point: %q -> %q (%v)", got, again, err)
			}
		})
	}
}

func TestNormalizeRejects(t *testing.T) {
	t.Parallel()
	tests := []string{
		"",
		"   ",
		"example.com/a",               // no scheme
		"/relative/path",              // relative
		"ftp://example.com/file",      // scheme
		"javascript:alert(1)",         // scheme
		"mailto:a@example.com",        // scheme
		"https://user:pw@host/a",      // credentials
		"https:///no-host",            // empty host
		"https://example.com:x/a",     // bad port
		"https://example.com:99999/a", // port out of range
		"https://example.com:0/a",     // port zero
		"https://example.com../a",     // empty label
		"https://../a",                // only dots
		"https://.example.com/a",      // leading dot
		"https://a..b.com/a",          // empty label
		"https://[fe80::1%25en0]/a",   // IPv6 zone
		"https://a%25b.com/a",         // '%' in host name
		"https://" + strings.Repeat("a", MaxLen) + ".com/", // input too long
		// Under MaxLen in, over MaxLen out (each é is 2 bytes in, 6 out).
		"https://example.com/" + strings.Repeat("é", 1000),
	}
	for _, in := range tests {
		name := in
		if len(name) > 60 {
			name = name[:60] + "..."
		}
		t.Run(name, func(t *testing.T) {
			_, err := Normalize(in)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error %v does not wrap ErrInvalid", err)
			}
		})
	}
}

// FuzzNormalize checks that nothing panics, that accepted inputs normalize to
// an accepted fixed point that is still http(s), and that every non-tracking
// query pair survives (dropping one could merge distinct articles).
// Run longer with: go test ./internal/urlnorm -run '^$' -fuzz FuzzNormalize -fuzztime 3000000x
// (A count rather than a duration: duration-based runs have been seen to stall
// on macOS while still reporting PASS.)
func FuzzNormalize(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/a?b=2&a=1#x",
		"HTTP://Example.com:80",
		"https://[::1]:443/p?utm_x=1",
		"https://example.com/%zz",
		"https://example.com/a?x=%",
		"https://example.com../a",
		"https://example.com/a?id=1;page=2&q=a+b",
		"https://example.com/a?bare&k=v&&",
		"https://example.com/é?x=é",
		"https://youtu.be/x?si=1&t=2#!/r",
		"http://0? #",
		"https://example.com/a?q=a #f",
		"https://example.com/a?utm_source=x;id=1&utm_y=2",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		once, err := Normalize(in)
		if err != nil {
			return
		}
		twice, err := Normalize(once)
		if err != nil {
			t.Fatalf("normalized form %q rejected: %v", once, err)
		}
		if once != twice {
			t.Fatalf("not a fixed point: %q -> %q -> %q", in, once, twice)
		}
		if !strings.HasPrefix(once, "http://") && !strings.HasPrefix(once, "https://") {
			t.Fatalf("output %q is not http(s)", once)
		}
		// Output pairs must be the input pairs (after escaping illegal bytes)
		// in order, minus only pairs with a known tracking key.
		inPairs := rawPairs(t, strings.TrimSpace(in))
		outPairs := rawPairs(t, once)
		j := 0
		for _, p := range inPairs {
			p = escapeQuery(p)
			if j < len(outPairs) && outPairs[j] == p {
				j++
				continue
			}
			if !looksTracking(p) {
				t.Fatalf("pair %q dropped or altered: %q -> %q", p, in, once)
			}
		}
		if j != len(outPairs) {
			t.Fatalf("output has unexpected pairs: %q -> %q", in, once)
		}
	})
}

func TestEscapeQuery(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":              "",
		"a=1&b=2":       "a=1&b=2",
		"q=a+b%20c":     "q=a+b%20c", // existing escapes and '+' untouched
		"q=%zz":         "q=%zz",     // invalid escapes untouched
		"q=a b":         "q=a%20b",
		"q=\t\x7f":      "q=%09%7F",
		"q=é":           "q=%C3%A9",
		"q={|}^`\\\"<>": "q=%7B%7C%7D%5E%60%5C%22%3C%3E",
		"a;b=c":         "a;b=c",
	}
	for in, want := range tests {
		got := escapeQuery(in)
		if got != want {
			t.Errorf("escapeQuery(%q) = %q, want %q", in, got, want)
		}
		if again := escapeQuery(got); again != got {
			t.Errorf("escapeQuery not idempotent: %q -> %q", got, again)
		}
	}
}

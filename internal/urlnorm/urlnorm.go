// Package urlnorm canonicalizes article URLs so the same article submitted
// twice (with a different fragment, tracking parameters, host case, etc.) is
// recognized as a duplicate.
//
// Normalization is deliberately conservative: two URLs that might name
// different resources must never normalize to the same string, and the
// normalized URL must fetch the same resource as the original. So nothing is
// ever decoded: the query is only ever percent-encoded where bytes are illegal
// in a URL (spaces, non-ASCII, ...), and the path gets net/url's standard
// escaping of the same kind.
package urlnorm

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// MaxLen bounds both accepted input and produced output length.
const MaxLen = 2048

// ErrInvalid is wrapped by every error Normalize returns.
var ErrInvalid = errors.New("invalid URL")

// Normalize returns the canonical form of raw:
//   - only absolute http/https URLs with a host are accepted; credentials,
//     IPv6 zones and empty host labels are rejected
//   - scheme and host are lowercased; one trailing dot on the host and the
//     default port are removed; the port loses leading zeros
//   - the fragment is removed, except "#!" hash-bang routes, which select
//     content on some sites
//   - tracking parameters are removed from the query; every other parameter
//     is kept in its original order and form, except that bytes illegal in a
//     URL are percent-encoded
//   - an empty path becomes "/"
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalid("empty")
	}
	if len(raw) > MaxLen {
		return "", invalid("longer than %d bytes", MaxLen)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", invalid("%v", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", invalid("scheme must be http or https")
	}
	if u.User != nil {
		return "", invalid("credentials are not allowed")
	}
	if u.Opaque != "" {
		return "", invalid("not a hierarchical URL")
	}

	host, err := normalizeHost(u.Hostname())
	if err != nil {
		return "", err
	}
	port, err := normalizePort(u.Scheme, u.Port())
	if err != nil {
		return "", err
	}
	u.RawQuery = escapeQuery(stripTracking(u.RawQuery, host))
	u.ForceQuery = false

	hostport := host
	if strings.Contains(host, ":") {
		hostport = "[" + host + "]" // IPv6 literal
	}
	if port != "" {
		hostport += ":" + port
	}
	u.Host = hostport

	// "#%21/x" and "#!/x" are both kept but stay distinct; only a missed
	// dedup, so not worth canonicalizing.
	if !strings.HasPrefix(u.Fragment, "!") {
		u.Fragment, u.RawFragment = "", ""
	}
	if u.Path == "" {
		u.Path = "/"
	}
	out := u.String()
	if len(out) > MaxLen {
		return "", invalid("normalized form longer than %d bytes", MaxLen)
	}
	return out, nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func normalizeHost(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	switch {
	case h == "":
		return "", invalid("missing host")
	case strings.Contains(h, "%") && strings.Contains(h, ":"):
		return "", invalid("IPv6 zones are not allowed")
	case strings.Contains(h, "%"):
		return "", invalid("host contains '%%'")
	case strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") || strings.Contains(h, ".."):
		return "", invalid("host has an empty label")
	}
	return h, nil
}

// normalizePort validates p and returns it without leading zeros, or "" when
// it is empty or the scheme's default.
func normalizePort(scheme, p string) (string, error) {
	if p == "" {
		return "", nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", invalid("port must be 1-65535, got %q", p)
	}
	if (scheme == "http" && n == 80) || (scheme == "https" && n == 443) {
		return "", nil
	}
	return strconv.Itoa(n), nil
}

// Tracking parameters never change which article a URL names. Keys match
// case-sensitively (servers treat them that way). Entries ending in "_" are
// prefixes.
var (
	globalTracking = []string{
		"utm_",
		"fbclid", "gclid", "dclid", "msclkid", "yclid", "twclid",
		"mc_cid", "mc_eid",
		"_hsenc", "_hsmi",
		"igshid",
	}
	// hostTracking holds short, ambiguous names that are only tracking on
	// specific sites.
	hostTracking = map[string][]string{
		"youtube.com":       {"si"},
		"www.youtube.com":   {"si"},
		"m.youtube.com":     {"si"},
		"music.youtube.com": {"si"},
		"youtu.be":          {"si"},
		"open.spotify.com":  {"si"},
	}
)

// stripTracking removes tracking pairs from a raw query string without
// decoding or re-encoding anything else, and drops empty pairs. host is the
// normalized host name (no port).
func stripTracking(rawQuery, host string) string {
	if rawQuery == "" {
		return ""
	}
	scoped := hostTracking[host]

	pairs := strings.Split(rawQuery, "&")
	kept := pairs[:0]
	for _, pair := range pairs {
		if pair == "" {
			continue
		}
		if isTrackingPair(pair, scoped) {
			continue
		}
		kept = append(kept, pair)
	}
	return strings.Join(kept, "&")
}

// isTrackingPair reports whether a raw "key=value" pair is safe to drop. A
// pair containing ';' is never dropped: servers that also split on ';' would
// see a real parameter hiding after it. Keys that can't be unescaped are kept.
func isTrackingPair(pair string, scoped []string) bool {
	if strings.Contains(pair, ";") {
		return false
	}
	rawKey, _, _ := strings.Cut(pair, "=")
	key, err := url.QueryUnescape(rawKey)
	return err == nil && (matches(globalTracking, key) || matches(scoped, key))
}

// escapeQuery percent-encodes bytes that are illegal in a URL query (controls,
// space, non-ASCII, and "<>\^`{|}) and leaves everything else, including
// existing escapes, untouched. It never decodes, so it can't change which
// resource the query names, and it is idempotent.
func escapeQuery(q string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		if c := q[i]; illegalInQuery(c) {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func illegalInQuery(c byte) bool {
	return c <= ' ' || c >= 0x7f || strings.IndexByte("\"<>\\^`{|}", c) >= 0
}

func matches(list []string, key string) bool {
	return slices.ContainsFunc(list, func(p string) bool {
		if strings.HasSuffix(p, "_") {
			return strings.HasPrefix(key, p)
		}
		return key == p
	})
}

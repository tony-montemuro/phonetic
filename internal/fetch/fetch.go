// Package fetch downloads article pages. It enforces timeouts and a size
// limit, accepts only HTML, converts the body to UTF-8, and refuses to
// connect to anything but public internet addresses: not loopback, private
// or other special-purpose ranges, and not this machine's own addresses or
// the networks it's attached to (with IPv6, LAN devices have public
// addresses). The check runs at connect time on the resolved IP, so
// redirects and DNS tricks can't get around it.
//
// Remaining gap: a home router that serves its admin interface on its own
// WAN IPv4 address to LAN clients ("hairpin"). Add that address to
// Options.Blocked if it matters.
package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

// Errors returned by Fetch. See IsPermanent for which ones are worth retrying.
var (
	// ErrBlockedAddress means the host resolved to a non-public address.
	ErrBlockedAddress = errors.New("address is not a public internet address")
	// ErrBadRedirect means a redirect pointed somewhere disallowed.
	ErrBadRedirect = errors.New("disallowed redirect")
	// ErrNotHTML means the response isn't an HTML page.
	ErrNotHTML = errors.New("response is not HTML")
	// ErrTooLarge means the body exceeded Options.MaxBytes.
	ErrTooLarge = errors.New("response body too large")
	// ErrTooManyRedirects means the redirect limit was hit.
	ErrTooManyRedirects = errors.New("too many redirects")
	// ErrUnsupportedEncoding means a Content-Encoding we can't decode.
	ErrUnsupportedEncoding = errors.New("unsupported Content-Encoding")
)

// StatusError is a non-2xx HTTP response.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.Code, http.StatusText(e.Code))
}

// IsPermanent reports whether retrying the fetch is pointless: the page is
// gone or forbidden, isn't HTML, is too big, points somewhere disallowed, is
// a redirect that can't be followed, has a bad TLS certificate, or the host
// doesn't exist. Timeouts, connection errors, 5xx and 429 are transient.
func IsPermanent(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return false
		}
		// 3xx only reaches us when it can't be followed (e.g. no Location).
		return se.Code >= 300 && se.Code < 500
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true
	}
	var (
		certErr     *tls.CertificateVerificationError
		hostErr     x509.HostnameError
		unknownAuth x509.UnknownAuthorityError
		invalidCert x509.CertificateInvalidError
	)
	if errors.As(err, &certErr) || errors.As(err, &hostErr) ||
		errors.As(err, &unknownAuth) || errors.As(err, &invalidCert) {
		return true
	}
	return errors.Is(err, ErrBlockedAddress) || errors.Is(err, ErrBadRedirect) ||
		errors.Is(err, ErrNotHTML) || errors.Is(err, ErrTooLarge) ||
		errors.Is(err, ErrTooManyRedirects) || errors.Is(err, ErrUnsupportedEncoding)
}

// Default option values.
const (
	DefaultTimeout      = 30 * time.Second
	DefaultMaxBytes     = 10 << 20
	DefaultMaxRedirects = 10
	DefaultUserAgent    = "Mozilla/5.0 (compatible; phonetic/1.0; personal article reader)"
	// maxHeaderWait caps how long the server may take to start responding.
	maxHeaderWait = 20 * time.Second
)

// Options configure a Fetcher. Zero values select the defaults.
type Options struct {
	Timeout      time.Duration // whole request, including body; default 30s
	MaxBytes     int64         // decoded body limit; default 10 MiB
	MaxRedirects int           // default 10
	UserAgent    string        // default: a browser-like UA naming phonetic
	// Blocked adds prefixes to refuse on top of the built-in rules (e.g.
	// the home's WAN address or delegated IPv6 prefix). Always enforced.
	Blocked []netip.Prefix
	// AllowPrivate disables the built-in address rules (non-public ranges
	// and this machine's networks); Blocked still applies. For tests and
	// local development only.
	AllowPrivate bool
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxRedirects <= 0 {
		o.MaxRedirects = DefaultMaxRedirects
	}
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgent
	}
	return o
}

// Result is a fetched page.
type Result struct {
	// URL is the final URL after redirects; relative links resolve against it.
	URL string
	// ContentType describes Body: always "text/html; charset=utf-8".
	ContentType string
	// OriginalContentType is the server's Content-Type (or the sniffed one).
	OriginalContentType string
	// Body is the page converted to valid UTF-8, without a BOM. Any
	// <meta charset> inside it still names the original encoding and must be
	// ignored.
	Body      []byte
	FetchedAt time.Time
}

// Fetcher downloads pages. It is safe for concurrent use.
type Fetcher struct {
	client *http.Client
	opts   Options
}

// New returns a Fetcher.
func New(opts Options) *Fetcher {
	opts = opts.withDefaults()
	var allow func(netip.Addr) error
	switch {
	case !opts.AllowPrivate:
		allow = defaultAllow(opts.Blocked, net.InterfaceAddrs)
	case len(opts.Blocked) > 0:
		allow = blockedOnly(opts.Blocked)
	}
	return newFetcher(opts, allow)
}

// newFetcher builds a Fetcher with an explicit address policy (nil allows
// everything). Tests use it to permit one loopback address but not others.
func newFetcher(opts Options, allow func(netip.Addr) error) *Fetcher {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if allow != nil {
		// Control runs for every connection attempt, after DNS resolution,
		// with the actual IP being dialed: redirects, Happy Eyeballs and DNS
		// rebinding are all covered.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrBlockedAddress, address)
			}
			return allow(ap.Addr())
		}
	}
	transport := &http.Transport{
		Proxy:                 nil, // never route through an environment proxy
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: min(maxHeaderWait, opts.Timeout),
	}
	return &Fetcher{
		opts: opts,
		client: &http.Client{
			Transport: transport,
			Timeout:   opts.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= opts.MaxRedirects {
					return ErrTooManyRedirects
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return fmt.Errorf("%w: to a %s URL", ErrBadRedirect, req.URL.Scheme)
				}
				if req.URL.User != nil {
					return fmt.Errorf("%w: URL contains credentials", ErrBadRedirect)
				}
				return nil
			},
		},
	}
}

// Fetch downloads rawURL and returns the page as UTF-8.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", f.opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Drain a little so the connection can be reused.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return Result{}, &StatusError{Code: resp.StatusCode}
	}
	// The transport transparently decodes gzip and removes the header; any
	// encoding left is one we can't read.
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return Result{}, fmt.Errorf("%w: %q", ErrUnsupportedEncoding, enc)
	}
	if resp.ContentLength > f.opts.MaxBytes {
		return Result{}, fmt.Errorf("%w: Content-Length %d", ErrTooLarge, resp.ContentLength)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, f.opts.MaxBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(raw)) > f.opts.MaxBytes {
		return Result{}, fmt.Errorf("%w: over %d bytes", ErrTooLarge, f.opts.MaxBytes)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(raw)
	}
	if !isHTML(contentType) {
		return Result{}, fmt.Errorf("%w: %s", ErrNotHTML, contentType)
	}

	body, err := toUTF8(raw, contentType)
	if err != nil {
		return Result{}, err
	}
	return Result{
		URL:                 resp.Request.URL.String(),
		ContentType:         "text/html; charset=utf-8",
		OriginalContentType: contentType,
		Body:                body,
		FetchedAt:           time.Now(),
	}, nil
}

func isHTML(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "text/html" || mediaType == "application/xhtml+xml"
}

// toUTF8 decodes the page following the HTML spec's sniffing order (BOM,
// then the Content-Type charset, then a <meta> in the first 1024 bytes),
// strips a leading BOM, and replaces any remaining invalid bytes.
//
// When none of those is conclusive, x/net falls back to windows-1252 after
// looking only at the first 1KB. If the whole body is valid UTF-8, that guess
// would turn "It’s" into "Itâ€™s", so UTF-8 wins instead. (Only that fallback
// is overridden: a meta naming e.g. a 7-bit ISO-2022 encoding is honored.)
func toUTF8(raw []byte, contentType string) ([]byte, error) {
	enc, name, certain := charset.DetermineEncoding(raw, contentType)
	var decoded []byte
	if name == "utf-8" || (!certain && name == "windows-1252" && utf8.Valid(raw)) {
		decoded = raw
	} else {
		var err error
		decoded, err = io.ReadAll(enc.NewDecoder().Reader(bytes.NewReader(raw)))
		if err != nil {
			return nil, fmt.Errorf("decoding %s: %w", name, err)
		}
	}
	decoded = bytes.TrimPrefix(decoded, []byte("\uFEFF"))
	if !utf8.Valid(decoded) {
		decoded = []byte(strings.ToValidUTF8(string(decoded), "\uFFFD"))
	}
	return decoded, nil
}

// globalUnicastV6 is the only IPv6 space handed out for the public internet
// today; everything outside it is refused.
var globalUnicastV6 = netip.MustParsePrefix("2000::/3")

// specialPurpose lists ranges beyond what netip's predicates cover (RFC 6890
// and friends) that must never be fetched.
var specialPurpose = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT (Tailscale addresses live here)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, incl. broadcast
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments (Teredo, ORCHIDv2, ...)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4 (embeds an IPv4 address)
	netip.MustParsePrefix("3fff::/20"),       // documentation (RFC 9637)
}

// isPublic reports whether addr is a globally routable unicast address.
func isPublic(addr netip.Addr) bool {
	addr = addr.Unmap() // ::ffff:127.0.0.1 is 127.0.0.1
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() ||
		addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsMulticast() {
		return false
	}
	if addr.Is6() && !globalUnicastV6.Contains(addr) {
		return false
	}
	for _, p := range specialPurpose {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// blockedOnly refuses only the configured prefixes (used when the built-in
// rules are off).
func blockedOnly(blocked []netip.Prefix) func(netip.Addr) error {
	return func(addr netip.Addr) error {
		addr = addr.Unmap()
		for _, p := range blocked {
			if p.Contains(addr) {
				return fmt.Errorf("%w: %s (configured block)", ErrBlockedAddress, addr)
			}
		}
		return nil
	}
}

// lanV6Bits is the widest IPv6 prefix treated as "ours" around each public
// interface address. Interfaces may report /128 (DHCPv6) or /64 while the LAN
// and its sibling networks (from a delegated prefix) share a /56; nobody else
// on the internet is in it.
const lanV6Bits = 56

// defaultAllow refuses non-public addresses, the extra blocked prefixes, and
// this machine's own addresses and attached networks (public IPv6 ones
// widened to /56). Interfaces are read on every dial: cheap, and it picks up
// address changes such as a new IPv6 prefix.
func defaultAllow(blocked []netip.Prefix, interfaceAddrs func() ([]net.Addr, error)) func(netip.Addr) error {
	return func(addr netip.Addr) error {
		addr = addr.Unmap()
		if !isPublic(addr) {
			return fmt.Errorf("%w: %s", ErrBlockedAddress, addr)
		}
		for _, p := range blocked {
			if p.Contains(addr) {
				return fmt.Errorf("%w: %s (configured block)", ErrBlockedAddress, addr)
			}
		}
		local, err := interfaceAddrs()
		if err != nil {
			// Fail closed: without the list we can't rule out the LAN.
			return fmt.Errorf("%w: listing local addresses: %v", ErrBlockedAddress, err)
		}
		for _, a := range local {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if p := localPrefix(ipNet); p.Contains(addr) {
				return fmt.Errorf("%w: %s is on a local network (%s)", ErrBlockedAddress, addr, p)
			}
		}
		return nil
	}
}

// localPrefix converts an interface address to the network to block: the
// reported prefix (public IPv6 widened to lanV6Bits). Malformed masks fall
// back to the address alone, never to "nothing".
func localPrefix(n *net.IPNet) netip.Prefix {
	ip, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}
	}
	ip = ip.Unmap()
	host := netip.PrefixFrom(ip, ip.BitLen())
	ones, bits := n.Mask.Size() // bits is 0 for non-contiguous masks
	switch {
	case ip.Is4() && bits == 128 && ones >= 96: // IPv4 with a 16-byte mask
		ones -= 96
	case bits != ip.BitLen():
		return host
	}
	if ip.Is6() && globalUnicastV6.Contains(ip) {
		ones = min(ones, lanV6Bits)
	}
	p, err := ip.Prefix(ones)
	if err != nil {
		return host
	}
	return p
}

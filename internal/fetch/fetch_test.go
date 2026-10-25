package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

const page = `<!doctype html><html><head><title>T</title></head><body><p>Hello</p></body></html>`

// local returns a Fetcher allowed to reach httptest servers on 127.0.0.1.
func local(opts Options) *Fetcher {
	opts.AllowPrivate = true
	return New(opts)
}

// serve starts a test server running h.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func htmlHandler(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Write([]byte(body))
	}
}

func srvWithLength(t *testing.T, declared, actual int) string {
	t.Helper()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		w.Write([]byte(strings.Repeat("a", actual)))
	})
	return srv.URL
}

func TestFetchHTML(t *testing.T) {
	t.Parallel()
	var gotUA string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		htmlHandler("text/html; charset=utf-8", page)(w, r)
	})
	before := time.Now()
	res, err := local(Options{}).Fetch(context.Background(), srv.URL+"/post")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != page || res.URL != srv.URL+"/post" || !strings.HasPrefix(res.ContentType, "text/html") {
		t.Errorf("result = %+v", res)
	}
	if res.FetchedAt.Before(before) {
		t.Errorf("FetchedAt = %s", res.FetchedAt)
	}
	if gotUA != DefaultUserAgent {
		t.Errorf("User-Agent = %q", gotUA)
	}
}

func TestFetchFollowsRedirectsAndReportsFinalURL(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
		case "/new":
			htmlHandler("text/html", page)(w, r)
		}
	})
	res, err := local(Options{}).Fetch(context.Background(), srv.URL+"/old")
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != srv.URL+"/new" {
		t.Errorf("final URL = %q, want %s/new", res.URL, srv.URL)
	}
}

func TestFetchRedirectLimit(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"x", http.StatusFound) // forever
	})
	_, err := local(Options{MaxRedirects: 3}).Fetch(context.Background(), srv.URL+"/")
	if !errors.Is(err, ErrTooManyRedirects) || !IsPermanent(err) {
		t.Errorf("err = %v, want permanent ErrTooManyRedirects", err)
	}
}

func TestFetchStatusErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code      int
		permanent bool
	}{
		{http.StatusNotFound, true},
		{http.StatusGone, true},
		{http.StatusForbidden, true},
		{http.StatusUnauthorized, true},
		{http.StatusTooManyRequests, false},
		{http.StatusRequestTimeout, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, tt := range tests {
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tt.code) })
		_, err := local(Options{}).Fetch(context.Background(), srv.URL)
		var se *StatusError
		if !errors.As(err, &se) || se.Code != tt.code {
			t.Errorf("%d: err = %v, want StatusError", tt.code, err)
			continue
		}
		if IsPermanent(err) != tt.permanent {
			t.Errorf("%d: IsPermanent = %v, want %v", tt.code, IsPermanent(err), tt.permanent)
		}
	}
}

func TestFetchRejectsNonHTML(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ contentType, body string }{
		{"application/pdf", "%PDF-1.7"},
		{"application/json", `{"a":1}`},
		{"image/png", "\x89PNG\r\n\x1a\n"},
		{"", "%PDF-1.7 sniffed"}, // no header: sniffed as non-HTML
		{"not a media type;;", page},
	} {
		srv := serve(t, htmlHandler(tt.contentType, tt.body))
		_, err := local(Options{}).Fetch(context.Background(), srv.URL)
		if !errors.Is(err, ErrNotHTML) || !IsPermanent(err) {
			t.Errorf("Content-Type %q: err = %v, want permanent ErrNotHTML", tt.contentType, err)
		}
	}
}

func TestFetchSniffsMissingContentType(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil // stop net/http from sniffing for us
		w.Write([]byte(page))
	})
	if _, err := local(Options{}).Fetch(context.Background(), srv.URL); err != nil {
		t.Errorf("sniffed HTML rejected: %v", err)
	}
}

func TestFetchAcceptsXHTML(t *testing.T) {
	t.Parallel()
	srv := serve(t, htmlHandler("application/xhtml+xml; charset=utf-8", page))
	if _, err := local(Options{}).Fetch(context.Background(), srv.URL); err != nil {
		t.Errorf("XHTML rejected: %v", err)
	}
}

func TestFetchSizeLimit(t *testing.T) {
	t.Parallel()
	big := "<html><body>" + strings.Repeat("a", 2000) + "</body></html>"
	srv := serve(t, htmlHandler("text/html", big))

	if _, err := local(Options{MaxBytes: int64(len(big))}).Fetch(context.Background(), srv.URL); err != nil {
		t.Errorf("body exactly at the limit rejected: %v", err)
	}
	_, err := local(Options{MaxBytes: int64(len(big)) - 1}).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrTooLarge) || !IsPermanent(err) {
		t.Errorf("err = %v, want permanent ErrTooLarge", err)
	}
}

// The limit applies to the decompressed body, so a small gzip bomb can't
// slip past it.
func TestFetchSizeLimitAppliesAfterDecompression(t *testing.T) {
	t.Parallel()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("<html><body>" + strings.Repeat("a", 1<<20) + "</body></html>"))
	zw.Close()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz.Bytes())
	})
	_, err := local(Options{MaxBytes: 64 << 10}).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v (compressed %d bytes), want ErrTooLarge", err, gz.Len())
	}
}

func TestFetchConvertsCharsetToUTF8(t *testing.T) {
	t.Parallel()
	latin1 := "<html><head></head><body><p>caf\xe9 na\xefve</p></body></html>" // ISO-8859-1 bytes
	tests := map[string]http.HandlerFunc{
		"header": htmlHandler("text/html; charset=iso-8859-1", latin1),
		"meta": htmlHandler("text/html",
			`<html><head><meta charset="windows-1252"></head><body><p>caf`+"\xe9 na\xefve</p></body></html>"),
	}
	for name, h := range tests {
		srv := serve(t, h)
		res, err := local(Options{}).Fetch(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(res.Body), "café naïve") {
			t.Errorf("%s: body not converted to UTF-8: %q", name, res.Body)
		}
	}
}

func TestFetchReplacesInvalidUTF8(t *testing.T) {
	t.Parallel()
	srv := serve(t, htmlHandler("text/html; charset=utf-8", "<p>bad \xff byte</p>"))
	res, err := local(Options{}).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Body), "bad \uFFFD byte") {
		t.Errorf("body = %q", res.Body)
	}
}

func TestFetchTimeoutIsTransient(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	start := time.Now()
	_, err := local(Options{Timeout: 200 * time.Millisecond}).Fetch(context.Background(), srv.URL)
	if err == nil || IsPermanent(err) {
		t.Fatalf("err = %v, want transient timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("timeout not enforced (%s)", time.Since(start))
	}
}

func TestFetchHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := local(Options{}).Fetch(ctx, srv.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context deadline", err)
	}
}

func TestFetchConnectionRefusedIsTransient(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listening now
	_, err = local(Options{}).Fetch(context.Background(), "http://"+addr+"/")
	if err == nil || IsPermanent(err) {
		t.Errorf("err = %v, want transient connection error", err)
	}
}

func TestFetchUnknownHostIsPermanent(t *testing.T) {
	if testing.Short() {
		t.Skip("needs DNS")
	}
	t.Parallel()
	// .invalid is reserved and never resolves (RFC 6761), but the lookup
	// still needs an honest resolver.
	_, err := New(Options{}).Fetch(context.Background(), "http://phonetic-test.invalid/")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Skipf("resolver doesn't return NXDOMAIN for .invalid (err = %v)", err)
	}
	if !IsPermanent(err) {
		t.Errorf("err = %v, want permanent DNS not-found", err)
	}
}

// Without AllowPrivate, local and private addresses are refused at connect
// time, including when reached through a redirect.
func TestFetchBlocksNonPublicAddresses(t *testing.T) {
	t.Parallel()
	srv := serve(t, htmlHandler("text/html", page))
	strict := New(Options{})

	for _, u := range []string{
		srv.URL, // 127.0.0.1
		strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), // name resolving to loopback
		strings.Replace(srv.URL, "127.0.0.1", "[::ffff:127.0.0.1]", 1),
	} {
		_, err := strict.Fetch(context.Background(), u)
		if !errors.Is(err, ErrBlockedAddress) || !IsPermanent(err) {
			t.Errorf("%s: err = %v, want permanent ErrBlockedAddress", u, err)
		}
	}
}

func TestFetchRejectsNonHTTPRedirect(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://example.com/file", http.StatusFound)
	})
	_, err := local(Options{}).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrBadRedirect) || !IsPermanent(err) {
		t.Errorf("err = %v, want permanent ErrBadRedirect", err)
	}
}

func TestFetchRejectsRedirectWithCredentials(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "http://user:pass@"+r.Host+"/next", http.StatusFound)
		}
	})
	if _, err := local(Options{}).Fetch(context.Background(), srv.URL+"/"); !errors.Is(err, ErrBadRedirect) {
		t.Errorf("err = %v, want ErrBadRedirect", err)
	}
}

// The main SSRF claim: a permitted first hop can't redirect to a blocked
// address. Uses a policy that allows exactly 127.0.0.1 and nothing else.
func TestFetchBlocksRedirectToDisallowedAddress(t *testing.T) {
	t.Parallel()
	only := netip.MustParseAddr("127.0.0.1")
	allowOne := func(a netip.Addr) error {
		if a.Unmap() == only {
			return nil
		}
		return ErrBlockedAddress
	}
	target := httptest.NewUnstartedServer(htmlHandler("text/html", page))
	// The target listens on IPv6 loopback, which the policy doesn't allow.
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback not available on this host: %v", err)
	}
	target.Listener = ln
	target.Start()
	t.Cleanup(target.Close)

	first := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
	})
	f := newFetcher(Options{}.withDefaults(), allowOne)
	if _, err := f.Fetch(context.Background(), first.URL); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("err = %v, want ErrBlockedAddress for the redirect target", err)
	}
	// Sanity: the same policy does allow the first hop on its own.
	direct := serve(t, htmlHandler("text/html", page))
	if _, err := f.Fetch(context.Background(), direct.URL); err != nil {
		t.Errorf("allowed address refused: %v", err)
	}
}

func TestIsPublic(t *testing.T) {
	t.Parallel()
	public := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111", "2001:4860:4860::8888"}
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1", // loopback
		"10.0.0.1", "172.16.5.4", "192.168.1.1", "fd00::1", "fc00::1", // private / ULA
		"169.254.169.254", "fe80::1", // link-local (cloud metadata lives here)
		"0.0.0.0", "::", "0.1.2.3", // unspecified / this network
		"100.64.0.1", "100.100.100.100", "100.127.255.254", // CGNAT / Tailscale
		"224.0.0.1", "ff02::1", "239.255.255.250", // multicast
		"255.255.255.255", "240.0.0.1", // reserved / broadcast
		"192.0.2.1", "198.51.100.7", "203.0.113.9", "2001:db8::1", // documentation
		"198.18.0.1", "192.0.0.8", // benchmarking / IETF
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", // IPv4-mapped
		"64:ff9b::7f00:1", "2002:7f00:1::", // NAT64 / 6to4 embedding 127.0.0.1
		"::7f00:1", "::a00:1", "::ffff:0:7f00:1", // IPv4-compatible / -translated
		"fec0::1", "3fff::1", "5f00::1", "fd7a:115c:a1e0::1", // site-local, docs, SRv6, Tailscale ULA
	}
	for _, s := range public {
		if !isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	for _, s := range blocked {
		if isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s should not be public", s)
		}
	}
}

func TestDefaultAllowBlocksLocalNetworksAndConfiguredPrefixes(t *testing.T) {
	t.Parallel()
	ifaces := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("2600:1700:abcd:1::10"), Mask: net.CIDRMask(64, 128)},   // LAN /64 (public!)
			&net.IPNet{IP: net.ParseIP("2a01:4f8:aa:bb::5"), Mask: net.CIDRMask(128, 128)},     // DHCPv6 /128
			&net.IPNet{IP: net.ParseIP("203.0.114.7").To4(), Mask: net.CIDRMask(24, 32)},       // public IPv4 LAN
			&net.IPNet{IP: net.ParseIP("198.51.99.9").To4(), Mask: net.IPMask{255, 0, 255, 0}}, // non-contiguous
		}, nil
	}
	allow := defaultAllow([]netip.Prefix{netip.MustParsePrefix("198.41.0.0/24")}, ifaces)

	for _, s := range []string{
		"2600:1700:abcd:1::10", // this machine
		"2600:1700:abcd:1::1",  // the router, same /64
		"2600:1700:abcd:7::1",  // sibling /64 in the same /56 (delegated prefix)
		"2a01:4f8:aa:bb::1",    // router next to a DHCPv6 /128
		"2a01:4f8:aa:11::1",    // same /56 as the /128
		"203.0.114.200",        // same IPv4 network
		"198.51.99.9",          // host with a malformed mask is still blocked
		"198.41.0.4",           // configured block
		"::ffff:203.0.114.200", // mapped form of a local address
		"127.0.0.1",            // still non-public
	} {
		if err := allow(netip.MustParseAddr(s)); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: err = %v, want ErrBlockedAddress", s, err)
		}
	}
	for _, s := range []string{"2600:1700:abce::1", "2a01:4f8:aa:100::1", "1.1.1.1", "198.51.99.10"} {
		if err := allow(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s: unexpectedly blocked: %v", s, err)
		}
	}

	failing := defaultAllow(nil, func() ([]net.Addr, error) { return nil, errors.New("boom") })
	if err := failing(netip.MustParseAddr("1.1.1.1")); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("interface listing failure should fail closed, got %v", err)
	}
}

// Proxy environment variables must be ignored: a proxy would make the
// connect-time address check see the proxy's address instead of the target's.
// (Checked on the transport directly; an env-var test can't fail, since Go
// never proxies localhost and reads the environment only once per process.)
func TestFetchNeverUsesAProxy(t *testing.T) {
	t.Parallel()
	for _, f := range []*Fetcher{New(Options{}), local(Options{})} {
		tr, ok := f.client.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil {
			t.Errorf("transport proxy must be nil, got %#v", f.client.Transport)
		}
	}
}

func TestFetchContentLengthChecks(t *testing.T) {
	t.Parallel()
	big := srvWithLength(t, 5000, 100)
	if _, err := local(Options{MaxBytes: 1000}).Fetch(context.Background(), big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("declared length over limit: err = %v, want ErrTooLarge", err)
	}
	// A body with no declared length is still caught by the read limit.
	liar := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.(http.Flusher).Flush() // chunked: no Content-Length at all
		w.Write([]byte("<html>" + strings.Repeat("a", 5000)))
	})
	if _, err := local(Options{MaxBytes: 1000}).Fetch(context.Background(), liar.URL); !errors.Is(err, ErrTooLarge) {
		t.Errorf("undeclared length over limit: err = %v, want ErrTooLarge", err)
	}
}

func TestFetchUnsupportedEncodingError(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "br")
		w.Write([]byte("garbage"))
	})
	_, err := local(Options{}).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrUnsupportedEncoding) || !IsPermanent(err) {
		t.Errorf("err = %v, want permanent ErrUnsupportedEncoding", err)
	}
}

// A long ASCII head pushes any non-ASCII past x/net's 1KB sniffing window;
// a valid-UTF-8 body must not be decoded as windows-1252.
func TestFetchUndeclaredUTF8IsNotMojibake(t *testing.T) {
	t.Parallel()
	body := "<html><head><script>" + strings.Repeat("var x = 1;", 150) + "</script></head>" +
		"<body><p>It\u2019s caf\u00e9</p></body></html>"
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html") // no charset
		w.Write([]byte(body))
	})
	res, err := local(Options{}).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Body), "It\u2019s caf\u00e9") {
		t.Errorf("UTF-8 body mangled: ...%s", res.Body[len(res.Body)-40:])
	}
	if res.ContentType != "text/html; charset=utf-8" || res.OriginalContentType != "text/html" {
		t.Errorf("content types = %q / %q", res.ContentType, res.OriginalContentType)
	}
}

func TestFetchBOMWinsAndIsStripped(t *testing.T) {
	t.Parallel()
	// UTF-8 BOM overrides a wrong header charset (per the HTML spec).
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
		w.Write([]byte("\xef\xbb\xbf<p>caf\u00e9</p>"))
	})
	res, err := local(Options{}).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "<p>caf\u00e9</p>" {
		t.Errorf("body = %q, want BOM stripped and UTF-8 kept", res.Body)
	}
}

func TestFetchBadCertificateIsPermanent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(htmlHandler("text/html", page)) // self-signed, untrusted
	t.Cleanup(srv.Close)
	_, err := local(Options{}).Fetch(context.Background(), srv.URL)
	if err == nil || !IsPermanent(err) {
		t.Errorf("err = %v, want a permanent certificate error", err)
	}
}

func TestFetchRedirectWithoutLocationIsPermanent(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusFound) })
	_, err := local(Options{}).Fetch(context.Background(), srv.URL)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusFound || !IsPermanent(err) {
		t.Errorf("err = %v, want permanent StatusError 302", err)
	}
}

func TestFetchHonorsSevenBitMetaCharset(t *testing.T) {
	t.Parallel()
	// ISO-2022-JP "\u65e5\u672c" is pure 7-bit ASCII on the wire.
	body := "<html><head><meta charset=\"iso-2022-jp\"></head><body><p>\x1b$BF|K\\\x1b(B</p></body></html>"
	srv := serve(t, htmlHandler("text/html", body))
	res, err := local(Options{}).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Body), "\u65e5\u672c") {
		t.Errorf("ISO-2022-JP not decoded: %q", res.Body)
	}
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/store"
)

const testToken = "test-token-0123456789"

type countingNotifier struct{ n atomic.Int32 }

func (c *countingNotifier) Notify() { c.n.Add(1) }

// failingArticles is an ArticleStore whose every call fails.
type failingArticles struct{}

func (failingArticles) InsertArticle(context.Context, string, time.Time) (store.Article, bool, error) {
	return store.Article{}, false, errors.New("disk on fire")
}
func (failingArticles) GetArticle(context.Context, int64) (store.Article, error) {
	return store.Article{}, errors.New("disk on fire")
}
func (failingArticles) ListArticles(context.Context, store.ListOptions) ([]store.Article, error) {
	return nil, errors.New("disk on fire")
}

// apiClient wraps a test server running the full handler against a real store.
type apiClient struct {
	t        *testing.T
	srv      *httptest.Server
	store    *store.Store
	notifier *countingNotifier
}

func newAPIClient(t *testing.T, articles ArticleStore) *apiClient {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "phonetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if articles == nil {
		articles = st
	}
	n := &countingNotifier{}
	srv := httptest.NewServer(New(Deps{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:       st,
		Articles: articles,
		Notifier: n,
		APIToken: testToken,
		Now:      func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
	}))
	t.Cleanup(srv.Close)
	return &apiClient{t: t, srv: srv, store: st, notifier: n}
}

// do sends a request with the valid token and decodes a JSON response into
// out (if non-nil).
func (c *apiClient) do(method, path, body string, out any) *http.Response {
	c.t.Helper()
	return c.doWithAuth(method, path, body, "Bearer "+testToken, out)
}

func (c *apiClient) doWithAuth(method, path, body, auth string, out any) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		c.t.Errorf("%s %s: Content-Type = %q", method, path, ct)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("%s %s: decoding response: %v", method, path, err)
		}
	}
	return resp
}

type errorBody struct{ Error string }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestCreateArticle(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)

	var a articleJSON
	resp := c.do("POST", "/api/articles", `{"url": "HTTPS://Example.com/Post?utm_source=x#top"}`, &a)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if a.ID == 0 || a.URL != "https://example.com/Post" || a.Status != store.StatusQueued {
		t.Errorf("article = %+v", a)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/articles/"+itoa(a.ID) {
		t.Errorf("Location = %q", loc)
	}
	if c.notifier.n.Load() != 1 {
		t.Errorf("notifier called %d times, want 1", c.notifier.n.Load())
	}

	// Same article, different spelling: deduplicated.
	var dup articleJSON
	resp = c.do("POST", "/api/articles", `{"url": "https://example.com/Post#comments"}`, &dup)
	if resp.StatusCode != http.StatusOK || dup.ID != a.ID {
		t.Errorf("duplicate: status %d, id %d (want 200, %d)", resp.StatusCode, dup.ID, a.ID)
	}
	if resp.Header.Get("Location") != "" || resp.Header.Get("Content-Location") != "/api/articles/"+itoa(a.ID) {
		t.Errorf("duplicate headers: Location=%q Content-Location=%q",
			resp.Header.Get("Location"), resp.Header.Get("Content-Location"))
	}
	if c.notifier.n.Load() != 1 {
		t.Error("duplicate submission should not notify workers")
	}
}

func TestCreateArticleRejectsBadInput(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)
	tests := []struct {
		name, body string
		want       int
	}{
		{"not json", `url=https://example.com`, http.StatusBadRequest},
		{"empty url", `{"url": ""}`, http.StatusBadRequest},
		{"missing url", `{}`, http.StatusBadRequest},
		{"bad scheme", `{"url": "ftp://example.com/a"}`, http.StatusBadRequest},
		{"relative", `{"url": "/a"}`, http.StatusBadRequest},
		{"too large", `{"url": "https://example.com/` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
		{"unknown field", `{"url": "https://example.com/a", "extra": 1}`, http.StatusBadRequest},
		{"typo field", `{"ur1": "https://example.com/a"}`, http.StatusBadRequest},
		{"trailing data", `{"url": "https://example.com/a"} garbage`, http.StatusBadRequest},
		{"two objects", `{"url": "https://example.com/a"}{"url": "https://example.com/b"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e errorBody
			resp := c.do("POST", "/api/articles", tt.body, &e)
			if resp.StatusCode != tt.want || e.Error == "" {
				t.Errorf("status %d error %q, want %d with a message", resp.StatusCode, e.Error, tt.want)
			}
		})
	}
	if c.notifier.n.Load() != 0 {
		t.Error("rejected submissions should not notify workers")
	}
}

func TestGetArticle(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)
	var created articleJSON
	c.do("POST", "/api/articles", `{"url": "https://example.com/a"}`, &created)

	var got articleJSON
	if resp := c.do("GET", "/api/articles/"+itoa(created.ID), "", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got.ID != created.ID || got.URL != created.URL {
		t.Errorf("got %+v, want %+v", got, created)
	}

	for _, path := range []string{
		"/api/articles/999", "/api/articles/abc", "/api/articles/0", "/api/articles/-1",
		// Non-canonical spellings of a real id.
		"/api/articles/+" + itoa(created.ID), "/api/articles/0" + itoa(created.ID),
	} {
		var e errorBody
		if resp := c.do("GET", path, "", &e); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestListArticles(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)

	var empty struct{ Articles []articleJSON }
	c.do("GET", "/api/articles", "", &empty)
	if empty.Articles == nil || len(empty.Articles) != 0 {
		t.Errorf("empty list should be [], got %#v", empty.Articles)
	}

	for _, u := range []string{"https://example.com/1", "https://example.com/2", "https://example.com/3"} {
		c.do("POST", "/api/articles", `{"url": "`+u+`"}`, nil)
	}
	var list struct{ Articles []articleJSON }
	c.do("GET", "/api/articles?limit=2&status=queued", "", &list)
	if len(list.Articles) != 2 || list.Articles[0].URL != "https://example.com/3" {
		t.Errorf("list = %+v", list.Articles)
	}

	for _, q := range []string{"?status=redy", "?limit=0", "?limit=501", "?limit=x"} {
		var e errorBody
		if resp := c.do("GET", "/api/articles"+q, "", &e); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestAPIRequiresBearerToken(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)
	routes := []struct{ method, path, body string }{
		{"POST", "/api/articles", `{"url": "https://example.com/a"}`},
		{"GET", "/api/articles", ""},
		{"GET", "/api/articles/1", ""},
	}
	auths := map[string]string{
		"none":         "",
		"wrong token":  "Bearer nope",
		"wrong scheme": "Basic " + testToken,
		"prefix only":  "Bearer " + testToken[:5],
		"empty bearer": "Bearer ",
	}
	for _, rt := range routes {
		for name, auth := range auths {
			var e errorBody
			resp := c.doWithAuth(rt.method, rt.path, rt.body, auth, &e)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: status %d, want 401", rt.method, rt.path, name, resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s with %s: missing WWW-Authenticate", rt.method, rt.path, name)
			}
		}
	}
	// Scheme is case-insensitive.
	if resp := c.doWithAuth("GET", "/api/articles", "", "bearer "+testToken, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("lowercase scheme: status %d, want 200", resp.StatusCode)
	}
	// Nothing was created by unauthorized POSTs.
	var list struct{ Articles []articleJSON }
	c.do("GET", "/api/articles", "", &list)
	if len(list.Articles) != 0 {
		t.Errorf("unauthorized POST created %d articles", len(list.Articles))
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)
	if resp := c.doWithAuth("GET", "/healthz", "", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status %d, want 200", resp.StatusCode)
	}
}

func TestAPIStoreErrorsAre500(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, failingArticles{})
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/api/articles", `{"url": "https://example.com/a"}`},
		{"GET", "/api/articles", ""},
		{"GET", "/api/articles/1", ""},
	} {
		var e errorBody
		resp := c.do(rt.method, rt.path, rt.body, &e)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s %s = %d, want 500", rt.method, rt.path, resp.StatusCode)
		}
		if c.notifier.n.Load() != 0 {
			t.Errorf("%s %s: notified workers despite failure", rt.method, rt.path)
		}
		if strings.Contains(e.Error, "disk on fire") {
			t.Errorf("%s %s leaked internal error: %q", rt.method, rt.path, e.Error)
		}
	}
}

// With no token configured (zero Deps beyond logging), every /api/ request is
// rejected before reaching a handler, whatever Authorization it carries.
func TestEmptyTokenLocksAPI(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(New(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: fakePinger{}}))
	defer srv.Close()
	for _, path := range []string{"/api/articles", "/api/articles/1", "/api/whatever"} {
		for _, method := range []string{"GET", "POST", "HEAD", "PUT"} {
			for _, auth := range []string{"", "Bearer ", "Bearer  ", "Bearer x"} {
				req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(`{"url": "https://example.com/a"}`))
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s auth=%q: status %d, want 401", method, path, auth, resp.StatusCode)
				}
			}
		}
	}
}

func TestNewPanicsOnIncompleteWiring(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("New with a token but no Articles/Notifier should panic")
		}
	}()
	New(Deps{Logger: slog.Default(), DB: fakePinger{}, APIToken: testToken})
}

func TestUnknownAPIRoutesAreAuthedJSON404(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)
	for _, rt := range []struct{ method, path string }{
		{"PUT", "/api/articles"},
		{"DELETE", "/api/articles/1"},
		{"GET", "/api/articles/"},
		{"GET", "/api/nope"},
	} {
		var e errorBody
		if resp := c.do(rt.method, rt.path, "", &e); resp.StatusCode != http.StatusNotFound || e.Error == "" {
			t.Errorf("%s %s with token: status %d %q, want JSON 404", rt.method, rt.path, resp.StatusCode, e.Error)
		}
		if resp := c.doWithAuth(rt.method, rt.path, "", "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without token: status %d, want 401", rt.method, rt.path, resp.StatusCode)
		}
	}
}

// Checks the wire format directly (decoding into articleJSON would hide a
// wrong omit tag) and the protective headers.
func TestArticleWireFormat(t *testing.T) {
	t.Parallel()
	c := newAPIClient(t, nil)
	var raw map[string]any
	resp := c.do("POST", "/api/articles", `{"url": "https://example.com/a"}`, &raw)
	for _, key := range []string{"id", "url", "status", "attempts", "created_at", "updated_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing %q in %v", key, raw)
		}
	}
	for _, key := range []string{"ready_at", "published_at", "error", "failed_stage", "image_importance", "title"} {
		if _, ok := raw[key]; ok {
			t.Errorf("unset field %q should be omitted: %v", key, raw)
		}
	}
	if raw["created_at"] != "2026-10-21T12:00:00Z" {
		t.Errorf("created_at = %v, want RFC 3339 from Deps.Now", raw["created_at"])
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing protective headers: %v", resp.Header)
	}
}

func TestNowDefaultsToWallClock(t *testing.T) {
	t.Parallel()
	before := time.Now()
	got := Deps{}.now()
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("Deps{}.now() = %s, not the current time", got)
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]int64{"1": 1, "42": 42, "9223372036854775807": 9223372036854775807} {
		if got, ok := parseID(raw); !ok || got != want {
			t.Errorf("parseID(%q) = %d, %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"", "0", "-1", "+1", "01", " 1", "1 ", "1e3", "abc", "9223372036854775808"} {
		if _, ok := parseID(raw); ok {
			t.Errorf("parseID(%q) should fail", raw)
		}
	}
}

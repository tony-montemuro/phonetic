package server

import (
	"context"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/artifact"
	"github.com/tony-montemuro/phonetic/internal/store"
)

const (
	testFeedUser     = "listener"
	testFeedPassword = "feed-password-0123456789"
	testPublicURL    = "https://pc.example.ts.net"
)

// fakeFeed is a FeedStore over a fixed list of articles (newest first).
type fakeFeed struct{ articles []store.Article }

func (f fakeFeed) ListReady(_ context.Context, limit int) ([]store.Article, error) {
	var out []store.Article
	for _, a := range f.articles {
		if a.Status == store.StatusReady && len(out) < limit {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f fakeFeed) GetArticle(_ context.Context, id int64) (store.Article, error) {
	for _, a := range f.articles {
		if a.ID == id {
			return a, nil
		}
	}
	return store.Article{}, store.ErrNotFound
}

// audioData is the fake content of article 1's audio.mp3.
const audioData = "ID3 not really an mp3, but bytes to serve"

// newFeedServer serves a ready article with audio (1), one ready from before
// audio existed (2) and one still in progress (3).
func newFeedServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	return newFeedServerFor(t, password, nil)
}

// newFeedServerFor is newFeedServer with extra articles listed first.
func newFeedServerFor(t *testing.T, password string, extra []store.Article) *httptest.Server {
	t.Helper()
	dataDir := t.TempDir()
	if err := artifact.For(dataDir, 1).WriteFile(artifact.Audio, []byte(audioData)); err != nil {
		t.Fatal(err)
	}
	ready := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	articles := fakeFeed{articles: append(extra, []store.Article{
		{ID: 3, URL: "https://example.com/c", Status: store.StatusSynthesizing},
		{ID: 2, URL: "https://example.com/b", Status: store.StatusReady, ReadyAt: ready},
		{ID: 1, URL: "https://example.com/a?x=1&y=2", Status: store.StatusReady, Title: "Fish & Chips",
			Author: "Jane Doe", SiteName: "Example", WordCount: 1234, ReadyAt: ready,
			AudioBytes: int64(len(audioData)), Duration: 90 * time.Second},
	}...)}
	srv := httptest.NewServer(New(Deps{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:           fakePinger{},
		Feed:         articles,
		FeedUser:     testFeedUser,
		FeedPassword: password,
		PublicURL:    testPublicURL,
		DataDir:      dataDir,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// feedRequest sends a GET (with the feed's credentials if auth) and returns
// the response with its body read.
func feedRequest(t *testing.T, srv *httptest.Server, path string, auth bool, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.SetBasicAuth(testFeedUser, testFeedPassword)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestFeedAndAudioRequireBasicAuth(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	for _, path := range []string{"/feed.xml", "/audio/1.mp3"} {
		for name, set := range map[string]func(*http.Request){
			"none":           func(*http.Request) {},
			"wrong password": func(r *http.Request) { r.SetBasicAuth(testFeedUser, "nope") },
			"wrong user":     func(r *http.Request) { r.SetBasicAuth("someone", testFeedPassword) },
			"bearer":         func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testFeedPassword) },
			"empty user":     func(r *http.Request) { r.SetBasicAuth("", testFeedPassword) },
		} {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
			set(req)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
				t.Errorf("%s with %s credentials: %d %q, want 401 with a Basic challenge",
					path, name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}
		}
		if resp, _ := feedRequest(t, srv, path, true, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("%s with credentials: %d", path, resp.StatusCode)
		}
	}
}

// Without a configured password the feed is locked, never open.
// Credentials are checked before anything else, so unknown ids don't reveal
// themselves to someone without them.
func TestAudioAuthBeforeLookup(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	if resp, _ := feedRequest(t, srv, "/audio/999.mp3", false, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown id without credentials: %d, want 401", resp.StatusCode)
	}
}

// Basic auth splits at the first colon, so passwords may contain colons.
func TestFeedPasswordWithColon(t *testing.T) {
	t.Parallel()
	const password = "pass:word:0123456789"
	srv := newFeedServer(t, password)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/feed.xml", nil)
	req.SetBasicAuth(testFeedUser, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
}

func TestFeedLockedWithoutPassword(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, "")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/feed.xml", nil)
	req.SetBasicAuth(testFeedUser, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
}

func TestFeed(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	resp, body := feedRequest(t, srv, "/feed.xml", true, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "application/rss+xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc struct {
		Channel struct {
			Image struct {
				URL string `xml:"url"`
			} `xml:"image"`
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				Description string `xml:"description"`
				GUID        string `xml:"guid"`
				Enclosure   struct {
					URL    string `xml:"url,attr"`
					Length int64  `xml:"length,attr"`
				} `xml:"enclosure"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("feed: %v\n%s", err, body)
	}
	// Only the ready article with audio is an episode.
	items := doc.Channel.Items
	if len(items) != 1 {
		t.Fatalf("%d episodes, want 1:\n%s", len(items), body)
	}
	it := items[0]
	if it.Title != "Fish & Chips" || it.Link != "https://example.com/a?x=1&y=2" || it.GUID != "phonetic-article-1" {
		t.Errorf("episode = %+v", it)
	}
	if it.Enclosure.URL != testPublicURL+"/audio/1.mp3" || it.Enclosure.Length != int64(len(audioData)) {
		t.Errorf("enclosure = %+v", it.Enclosure)
	}
	if want := `<p>From <a href="https://example.com/a?x=1&amp;y=2">Example</a> · 1,234 words</p>`; it.Description != want {
		t.Errorf("description = %q, want %q", it.Description, want)
	}
	if doc.Channel.Image.URL != testPublicURL+"/artwork.png" {
		t.Errorf("artwork = %q", doc.Channel.Image.URL)
	}
}

// Apps revalidate with the ETag and get 304 until the feed changes.
func TestFeedConditionalGet(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	resp, _ := feedRequest(t, srv, "/feed.xml", true, nil)
	etag := resp.Header.Get("ETag")
	if etag == "" || resp.Header.Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("ETag %q, Cache-Control %q", etag, resp.Header.Get("Cache-Control"))
	}
	resp, body := feedRequest(t, srv, "/feed.xml", true, map[string]string{"If-None-Match": etag})
	if resp.StatusCode != http.StatusNotModified || body != "" {
		t.Errorf("revalidation: %d with %d bytes, want 304 and no body", resp.StatusCode, len(body))
	}
	resp, _ = feedRequest(t, srv, "/feed.xml", true, map[string]string{"If-None-Match": `"stale"`})
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stale ETag: %d, want 200", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/feed.xml", nil)
	req.SetBasicAuth(testFeedUser, testFeedPassword)
	req.Header.Set("If-None-Match", etag)
	head, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != http.StatusNotModified {
		t.Errorf("HEAD revalidation: %d, want 304", head.StatusCode)
	}

	// A new episode changes the ETag.
	more := newFeedServerFor(t, testFeedPassword, []store.Article{{ID: 4, URL: "https://example.com/d",
		Status: store.StatusReady, ReadyAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), AudioBytes: 1}})
	if resp, _ := feedRequest(t, more, "/feed.xml", true, nil); resp.Header.Get("ETag") == etag {
		t.Error("ETag unchanged after a new episode")
	}
}

func TestAudio(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	resp, body := feedRequest(t, srv, "/audio/1.mp3", true, nil)
	if resp.StatusCode != http.StatusOK || body != audioData || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("audio: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges = %q", resp.Header.Get("Accept-Ranges"))
	}
	etag := resp.Header.Get("ETag")
	resp, body = feedRequest(t, srv, "/audio/1.mp3", true, map[string]string{"Range": "bytes=4-6"})
	if resp.StatusCode != http.StatusPartialContent || body != audioData[4:7] {
		t.Errorf("range: %d %q, want 206 %q", resp.StatusCode, body, audioData[4:7])
	}
	// Resuming: the range applies only if the file is still the same one.
	resp, _ = feedRequest(t, srv, "/audio/1.mp3", true, map[string]string{"Range": "bytes=4-6", "If-Range": etag})
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("If-Range with the current ETag: %d, want 206", resp.StatusCode)
	}
	resp, body = feedRequest(t, srv, "/audio/1.mp3", true, map[string]string{"Range": "bytes=4-6", "If-Range": `"1-0-0"`})
	if resp.StatusCode != http.StatusOK || body != audioData {
		t.Errorf("If-Range with a stale ETag: %d, want the whole file", resp.StatusCode)
	}
}

// Apple requires HEAD support on feeds and audio.
func TestHeadRequests(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	for _, path := range []string{"/feed.xml", "/audio/1.mp3"} {
		req, _ := http.NewRequest(http.MethodHead, srv.URL+path, nil)
		req.SetBasicAuth(testFeedUser, testFeedPassword)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
			t.Errorf("HEAD %s: %d, length %d", path, resp.StatusCode, resp.ContentLength)
		}
	}
}

func TestAudioNotFound(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	for _, path := range []string{
		"/audio/1",          // no suffix
		"/audio/01.mp3",     // non-canonical id
		"/audio/x.mp3",      // not an id
		"/audio/1.mp3.mp3",  // suffix twice
		"/audio/1.MP3",      // wrong case
		"/audio/2.mp3",      // ready, but no audio file
		"/audio/3.mp3",      // not ready
		"/audio/9.mp3",      // no such article
		"/audio/1.mp3%2F..", // escaped slash
		"/audio/%2e%2e",     // escaped dots
	} {
		if resp, _ := feedRequest(t, srv, path, true, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestArtworkNeedsNoAuth(t *testing.T) {
	t.Parallel()
	srv := newFeedServer(t, testFeedPassword)
	resp, body := feedRequest(t, srv, "/artwork.png", false, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(body, "\x89PNG") {
		t.Errorf("artwork: %d %q (%d bytes)", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
}

func TestEpisodeDescription(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a    store.Article
		want string
	}{
		{store.Article{URL: "https://example.com/x", SiteName: "Ex <ample>", WordCount: 999},
			`<p>From <a href="https://example.com/x">Ex &lt;ample&gt;</a> · 999 words</p>`},
		{store.Article{URL: "https://blog.example.org/post", WordCount: 1234567},
			`<p>From <a href="https://blog.example.org/post">blog.example.org</a> · 1,234,567 words</p>`},
		{store.Article{URL: "https://example.com/\"q\""},
			`<p>From <a href="https://example.com/&#34;q&#34;">example.com</a></p>`},
	}
	for _, c := range cases {
		if got := episodeDescription(c.a); got != c.want {
			t.Errorf("episodeDescription(%+v) =\n%s\nwant\n%s", c.a, got, c.want)
		}
	}
}

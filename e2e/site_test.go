//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// site is a fake article website for phonetic to fetch, running in the test
// process on 127.0.0.1 (instances run with PHONETIC_FETCH_ALLOW_PRIVATE).
//
//	/article/{name}  an article titled "Article {name}"
//	/slow/{name}     the same, but blocks while Slow is set (to hold a fetch
//	                 in flight for crash and shutdown tests)
//	/missing         404
//	/file.pdf        a PDF
//	/flaky           503 (transient)
//	/empty           HTML with no article content
//
// A blocked /slow fetch is held until phonetic disconnects. The fetcher gives
// up on its own after 20s (header timeout), so tests must act well within
// that.
type site struct {
	srv  *httptest.Server
	Slow atomic.Bool
	hits atomic.Int64 // requests received on /slow/
}

func newSite(t testing.TB) *site {
	t.Helper()
	s := &site{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /article/{name}", func(w http.ResponseWriter, r *http.Request) {
		writeArticle(w, r.PathValue("name"))
	})
	mux.HandleFunc("GET /slow/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.Slow.Load() {
			<-r.Context().Done() // until phonetic gives up, is stopped or is killed
			return
		}
		writeArticle(w, r.PathValue("name"))
	})
	mux.HandleFunc("GET /missing", http.NotFound)
	mux.HandleFunc("GET /flaky", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "try again later", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("GET /empty", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><title>Empty</title></head><body><p>Nothing here.</p></body></html>"))
	})
	mux.HandleFunc("GET /file.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.7 not an article"))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// URL returns the full URL of path on the site.
func (s *site) URL(path string) string { return s.srv.URL + path }

// waitHits waits until /slow/ has received n requests, proving a fetch is
// actually in flight (not just that the stage started).
func (s *site) waitHits(t testing.TB, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.hits.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("site got %d /slow/ requests, want %d", s.hits.Load(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// articleTitle is the title the site gives article name.
func articleTitle(name string) string { return "Article " + name }

func writeArticle(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	para := strings.Repeat("This paragraph has enough words to be read as the body of a real article. ", 6)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><title>%[1]s</title>
<meta name="author" content="Test Author"></head><body><nav><a href="/">Home</a></nav>
<article><h1>%[1]s</h1><p>%[2]s</p><figure><img src="/img/%[3]s.png" alt="Diagram"><figcaption>Figure 1</figcaption></figure>
<p>%[2]s</p></article><footer>Copyright</footer></body></html>`, articleTitle(name), para, name)
}

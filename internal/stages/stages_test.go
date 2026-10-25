package stages

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/artifact"
	"github.com/tony-montemuro/phonetic/internal/extract"
	"github.com/tony-montemuro/phonetic/internal/fetch"
	"github.com/tony-montemuro/phonetic/internal/pipeline"
	"github.com/tony-montemuro/phonetic/internal/store"
)

// articlePage is a page the extractor accepts; its image src is relative, so
// it resolves differently depending on the page URL.
var articlePage = `<!doctype html><html lang="en"><head><title>Stage Test Post</title>
<meta name="author" content="Jane Doe"></head><body><article><h1>Stage Test Post</h1>
<p>` + strings.Repeat("Plenty of words make this paragraph look like real article content. ", 8) + `</p>
<figure><img src="chart.png" alt="Chart"><figcaption>Figure 1</figcaption></figure>
<p>` + strings.Repeat("A second paragraph keeps the reader engaged with more sentences. ", 6) + `</p>
</article></body></html>`

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "phonetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// claimAt inserts url and advances it to stage, returning the claimed article.
func claimAt(t *testing.T, s *store.Store, url string, stage store.Status) store.Article {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.InsertArticle(ctx, url, time.Now()); err != nil {
		t.Fatal(err)
	}
	for {
		a, err := s.ClaimArticle(ctx, time.Now(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if a.Status == stage {
			return a
		}
		if _, err := s.CompleteStage(ctx, store.LeaseOf(a), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFetchOutput writes what the fetch stage would have produced.
func writeFetchOutput(t *testing.T, dir artifact.Dir, page, finalURL string) {
	t.Helper()
	if err := dir.WriteFile(artifact.RawHTML, []byte(page)); err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteJSON(artifact.FetchMeta, FetchMeta{URL: finalURL}); err != nil {
		t.Fatal(err)
	}
}

// fakeFetcher returns a fixed result or error and records the URL asked for.
type fakeFetcher struct {
	res fetch.Result
	err error

	mu  sync.Mutex
	got []string
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (fetch.Result, error) {
	f.mu.Lock()
	f.got = append(f.got, url)
	f.mu.Unlock()
	return f.res, f.err
}

// failingMetadata is a MetadataStore whose writes fail.
type failingMetadata struct{ err error }

func (f failingMetadata) SaveExtracted(context.Context, store.Lease, store.ExtractedMetadata, time.Time) error {
	return f.err
}

func TestFetchStageWritesArtifacts(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	fetchedAt := time.Date(2026, 10, 24, 12, 0, 0, 0, time.FixedZone("CDT", -5*3600))
	f := &fakeFetcher{res: fetch.Result{
		URL: "https://example.com/final", ContentType: "text/html; charset=utf-8",
		OriginalContentType: "text/html; charset=iso-8859-1", Body: []byte(articlePage), FetchedAt: fetchedAt,
	}}
	a := store.Article{ID: 7, URL: "https://example.com/start", Status: store.StatusFetching}
	if err := (Fetch{DataDir: dataDir, Fetcher: f}).Run(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 || f.got[0] != a.URL {
		t.Errorf("fetched %q, want %q", f.got, a.URL)
	}
	dir := artifact.For(dataDir, 7)
	body, err := dir.ReadFile(artifact.RawHTML)
	if err != nil || string(body) != articlePage {
		t.Errorf("raw.html = %d bytes, %v", len(body), err)
	}
	var meta FetchMeta
	if err := dir.ReadJSON(artifact.FetchMeta, &meta); err != nil {
		t.Fatal(err)
	}
	want := FetchMeta{URL: "https://example.com/final", ContentType: "text/html; charset=utf-8",
		OriginalContentType: "text/html; charset=iso-8859-1", FetchedAt: fetchedAt.UTC()}
	if meta != want || meta.FetchedAt.Location() != time.UTC {
		t.Errorf("fetch.json = %+v, want %+v", meta, want)
	}
}

func TestFetchStageClassifiesErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"404", &fetch.StatusError{Code: http.StatusNotFound}, true},
		{"not html", fmt.Errorf("wrapped: %w", fetch.ErrNotHTML), true},
		{"503", &fetch.StatusError{Code: http.StatusServiceUnavailable}, false},
		{"timeout", context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := Fetch{DataDir: t.TempDir(), Fetcher: &fakeFetcher{err: tt.err}}
			err := st.Run(context.Background(), store.Article{ID: 1, URL: "https://example.com/"})
			if err == nil || pipeline.IsPermanent(err) != tt.permanent {
				t.Errorf("err = %v, permanent = %v, want %v", err, pipeline.IsPermanent(err), tt.permanent)
			}
			if !errors.Is(err, tt.err) {
				t.Errorf("original error not wrapped: %v", err)
			}
		})
	}
}

func TestFetchStageWriteFailureIsTransient(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeFetcher{res: fetch.Result{URL: "https://example.com/", Body: []byte(articlePage)}}
	err := Fetch{DataDir: file, Fetcher: f}.Run(context.Background(), store.Article{ID: 1, URL: "https://example.com/"})
	if err == nil || pipeline.IsPermanent(err) {
		t.Errorf("err = %v, want a transient write error", err)
	}
}

func TestStagesDontWriteAfterCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dataDir := t.TempDir()
	f := &fakeFetcher{res: fetch.Result{URL: "https://example.com/", Body: []byte(articlePage)}}
	if err := (Fetch{DataDir: dataDir, Fetcher: f}).Run(ctx, store.Article{ID: 1}); !errors.Is(err, context.Canceled) {
		t.Errorf("fetch: err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(artifact.For(dataDir, 1).Path(artifact.RawHTML)); err == nil {
		t.Error("fetch wrote raw.html after cancellation")
	}

	dir := artifact.For(dataDir, 2)
	writeFetchOutput(t, dir, articlePage, "https://example.com/post")
	err := Extract{Store: failingMetadata{}, DataDir: dataDir}.Run(ctx, store.Article{ID: 2})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("extract: err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dir.Path(artifact.Extracted)); err == nil {
		t.Error("extract wrote extracted.json after cancellation")
	}
}

// Fetch and extract against a real HTTP server, through the real fetcher.
// The redirect lands in a subdirectory, so a relative image only resolves
// correctly against the final URL.
func TestFetchThenExtract(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/blog/2026/post", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(articlePage))
	}))
	t.Cleanup(srv.Close)

	s := openStore(t)
	dataDir := t.TempDir()
	fetchStage := Fetch{DataDir: dataDir, Fetcher: fetch.New(fetch.Options{AllowPrivate: true})}
	extractStage := Extract{Store: s, DataDir: dataDir}
	ctx := context.Background()

	a := claimAt(t, s, srv.URL+"/old", store.StatusFetching)
	if err := fetchStage.Run(ctx, a); err != nil {
		t.Fatal(err)
	}
	var meta FetchMeta
	if err := artifact.For(dataDir, a.ID).ReadJSON(artifact.FetchMeta, &meta); err != nil || meta.URL != srv.URL+"/blog/2026/post" {
		t.Fatalf("fetch.json URL = %q (%v), want the final URL", meta.URL, err)
	}
	if _, err := s.CompleteStage(ctx, store.LeaseOf(a), time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.ClaimArticle(ctx, time.Now(), time.Minute)
	if err != nil || a.Status != store.StatusExtracting {
		t.Fatalf("claim extracting: %+v, %v", a, err)
	}
	if err := extractStage.Run(ctx, a); err != nil {
		t.Fatal(err)
	}

	var art extract.Article
	if err := artifact.For(dataDir, a.ID).ReadJSON(artifact.Extracted, &art); err != nil {
		t.Fatal(err)
	}
	if art.Title != "Stage Test Post" || len(art.Images) != 1 || art.Images[0].URL != srv.URL+"/blog/2026/chart.png" {
		t.Errorf("extracted = %+v (image URL must resolve against the final URL)", art)
	}
	got, err := s.GetArticle(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Stage Test Post" || got.Author != "Jane Doe" || got.WordCount != art.WordCount || got.ImageCount != 1 {
		t.Errorf("stored metadata = %+v", got)
	}
}

// Re-running extract (e.g. on reprocess) overwrites its output and metadata.
func TestExtractStageIsReRunnable(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	a := claimAt(t, s, "https://example.com/rerun", store.StatusExtracting)
	dir := artifact.For(dataDir, a.ID)
	st := Extract{Store: s, DataDir: dataDir}

	writeFetchOutput(t, dir, articlePage, "https://example.com/rerun")
	if err := st.Run(ctx, a); err != nil {
		t.Fatal(err)
	}
	noImages := strings.Replace(strings.Replace(articlePage, `<meta name="author" content="Jane Doe">`, "", 1),
		`<figure><img src="chart.png" alt="Chart"><figcaption>Figure 1</figcaption></figure>`, "", 1)
	writeFetchOutput(t, dir, noImages, "https://example.com/rerun")
	if err := st.Run(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetArticle(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Author != "" || got.ImageCount != 0 {
		t.Errorf("re-extraction left stale metadata: %+v", got)
	}
	var art extract.Article
	if err := dir.ReadJSON(artifact.Extracted, &art); err != nil || len(art.Images) != 0 {
		t.Errorf("extracted.json not overwritten: %d images, %v", len(art.Images), err)
	}
}

func TestExtractStageInputErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(t *testing.T, dir artifact.Dir)
		permanent bool
		mention   string
	}{
		{"nothing fetched", func(*testing.T, artifact.Dir) {}, true, "re-fetched"},
		{"raw.html missing", func(t *testing.T, dir artifact.Dir) {
			if err := dir.WriteJSON(artifact.FetchMeta, FetchMeta{URL: "https://example.com/"}); err != nil {
				t.Fatal(err)
			}
		}, true, "re-fetched"},
		{"corrupt fetch.json", func(t *testing.T, dir artifact.Dir) {
			if err := dir.WriteFile(artifact.FetchMeta, []byte("{not json")); err != nil {
				t.Fatal(err)
			}
		}, true, "corrupt"},
		{"empty final URL", func(t *testing.T, dir artifact.Dir) { writeFetchOutput(t, dir, articlePage, "") }, true, "final URL"},
		{"relative final URL", func(t *testing.T, dir artifact.Dir) { writeFetchOutput(t, dir, articlePage, "/post") }, true, "final URL"},
		{"non-http final URL", func(t *testing.T, dir artifact.Dir) { writeFetchOutput(t, dir, articlePage, "ftp://example.com/") }, true, "final URL"},
		{"unparseable final URL", func(t *testing.T, dir artifact.Dir) { writeFetchOutput(t, dir, articlePage, "http://[::1") }, true, "final URL"},
		{"unreadable raw.html", func(t *testing.T, dir artifact.Dir) {
			writeFetchOutput(t, dir, articlePage, "https://example.com/")
			// A directory where the file should be: a read error that
			// isn't "missing", so it may be worth retrying.
			if err := os.Remove(dir.Path(artifact.RawHTML)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir.Path(artifact.RawHTML), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false, "reading fetch output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			tt.setup(t, artifact.For(dataDir, 1))
			err := Extract{Store: failingMetadata{}, DataDir: dataDir}.Run(context.Background(), store.Article{ID: 1})
			if err == nil || pipeline.IsPermanent(err) != tt.permanent || !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("err = %v (permanent %v), want permanent=%v mentioning %q", err, pipeline.IsPermanent(err), tt.permanent, tt.mention)
			}
		})
	}
}

func TestExtractStageNoContentIsPermanent(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	dir := artifact.For(dataDir, 1)
	writeFetchOutput(t, dir, "<html><body><p>Too short.</p></body></html>", "https://example.com/empty")
	err := Extract{Store: failingMetadata{}, DataDir: dataDir}.Run(context.Background(), store.Article{ID: 1})
	if !errors.Is(err, extract.ErrNoContent) || !pipeline.IsPermanent(err) {
		t.Errorf("err = %v, want permanent ErrNoContent", err)
	}
	if _, statErr := os.Stat(dir.Path(artifact.Extracted)); statErr == nil {
		t.Error("extracted.json written for a failed extraction")
	}
}

func TestExtractStageMetadataFailureIsTransient(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	writeFetchOutput(t, artifact.For(dataDir, 1), articlePage, "https://example.com/post")
	boom := errors.New("database is locked")
	err := Extract{Store: failingMetadata{err: boom}, DataDir: dataDir}.Run(context.Background(), store.Article{ID: 1})
	if !errors.Is(err, boom) || pipeline.IsPermanent(err) || !strings.Contains(err.Error(), "saving metadata") {
		t.Errorf("err = %v, want a wrapped transient error", err)
	}
}

// A stage run under a lost lease must not record metadata.
func TestExtractStageLostLease(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	a := claimAt(t, s, "https://example.com/lease", store.StatusExtracting)
	writeFetchOutput(t, artifact.For(dataDir, a.ID), articlePage, "https://example.com/lease")
	if _, err := s.ReleaseAllLeases(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	err := Extract{Store: s, DataDir: dataDir}.Run(ctx, a)
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("err = %v, want ErrLeaseLost", err)
	}
	got, err := s.GetArticle(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "" {
		t.Errorf("metadata written without the lease: %+v", got)
	}
}

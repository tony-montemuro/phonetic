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
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/artifact"
	"github.com/tony-montemuro/phonetic/internal/extract"
	"github.com/tony-montemuro/phonetic/internal/fetch"
	"github.com/tony-montemuro/phonetic/internal/pipeline"
	"github.com/tony-montemuro/phonetic/internal/store"
)

// articlePage is a page the extractor accepts.
var articlePage = `<!doctype html><html lang="en"><head><title>Stage Test Post</title>
<meta name="author" content="Jane Doe"></head><body><article><h1>Stage Test Post</h1>
<p>` + strings.Repeat("Plenty of words make this paragraph look like real article content. ", 8) + `</p>
<figure><img src="/chart.png" alt="Chart"><figcaption>Figure 1</figcaption></figure>
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

// fakeFetcher returns a fixed result or error.
type fakeFetcher struct {
	res fetch.Result
	err error
}

func (f fakeFetcher) Fetch(context.Context, string) (fetch.Result, error) { return f.res, f.err }

func TestFetchStageWritesArtifacts(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	fetchedAt := time.Date(2026, 10, 24, 12, 0, 0, 0, time.FixedZone("CDT", -5*3600))
	st := Fetch{DataDir: dataDir, Fetcher: fakeFetcher{res: fetch.Result{
		URL: "https://example.com/final", ContentType: "text/html; charset=utf-8",
		OriginalContentType: "text/html; charset=iso-8859-1", Body: []byte(articlePage), FetchedAt: fetchedAt,
	}}}
	a := store.Article{ID: 7, URL: "https://example.com/start", Status: store.StatusFetching}
	if err := st.Run(context.Background(), a); err != nil {
		t.Fatal(err)
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
		err       error
		permanent bool
	}{
		{&fetch.StatusError{Code: http.StatusNotFound}, true},
		{fmt.Errorf("wrapped: %w", fetch.ErrNotHTML), true},
		{&fetch.StatusError{Code: http.StatusServiceUnavailable}, false},
		{context.DeadlineExceeded, false},
	}
	for _, tt := range tests {
		st := Fetch{DataDir: t.TempDir(), Fetcher: fakeFetcher{err: tt.err}}
		err := st.Run(context.Background(), store.Article{ID: 1, URL: "https://example.com/"})
		if err == nil || pipeline.IsPermanent(err) != tt.permanent {
			t.Errorf("%v: err = %v, permanent = %v, want %v", tt.err, err, pipeline.IsPermanent(err), tt.permanent)
		}
		if !errors.Is(err, tt.err) {
			t.Errorf("%v: original error not wrapped: %v", tt.err, err)
		}
	}
}

// Fetch and extract against a real HTTP server, through the real fetcher.
func TestFetchThenExtract(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/post", http.StatusMovedPermanently)
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
	if art.Title != "Stage Test Post" || len(art.Images) != 1 || art.Images[0].URL != srv.URL+"/chart.png" {
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

func TestExtractStageMissingFetchOutputIsPermanent(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	a := claimAt(t, s, "https://example.com/missing", store.StatusExtracting)
	err := Extract{Store: s, DataDir: t.TempDir()}.Run(context.Background(), a)
	if err == nil || !pipeline.IsPermanent(err) {
		t.Errorf("err = %v, want permanent", err)
	}
}

func TestExtractStageNoContentIsPermanent(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	dataDir := t.TempDir()
	a := claimAt(t, s, "https://example.com/empty", store.StatusExtracting)
	dir := artifact.For(dataDir, a.ID)
	if err := dir.WriteFile(artifact.RawHTML, []byte("<html><body><p>Too short.</p></body></html>")); err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteJSON(artifact.FetchMeta, FetchMeta{URL: "https://example.com/empty"}); err != nil {
		t.Fatal(err)
	}
	err := Extract{Store: s, DataDir: dataDir}.Run(context.Background(), a)
	if !errors.Is(err, extract.ErrNoContent) || !pipeline.IsPermanent(err) {
		t.Errorf("err = %v, want permanent ErrNoContent", err)
	}
	if _, statErr := os.Stat(dir.Path(artifact.Extracted)); statErr == nil {
		t.Error("extracted.json written for a failed extraction")
	}
}

// A stage run under a lost lease must not record metadata.
func TestExtractStageLostLease(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	dataDir := t.TempDir()
	a := claimAt(t, s, "https://example.com/lease", store.StatusExtracting)
	dir := artifact.For(dataDir, a.ID)
	dir.WriteFile(artifact.RawHTML, []byte(articlePage))
	dir.WriteJSON(artifact.FetchMeta, FetchMeta{URL: "https://example.com/lease"})
	if _, err := s.ReleaseAllLeases(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	err := Extract{Store: s, DataDir: dataDir}.Run(context.Background(), a)
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("err = %v, want ErrLeaseLost", err)
	}
	if got, _ := s.GetArticle(context.Background(), a.ID); got.Title != "" {
		t.Errorf("metadata written without the lease: %+v", got)
	}
}

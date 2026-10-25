package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// claimAt inserts url, advances it to stage and returns the claimed article.
func claimAt(t *testing.T, s *Store, url string, stage Status) Article {
	t.Helper()
	ctx := context.Background()
	mustInsert(t, s, url, t0)
	for {
		a := mustClaim(t, s, t0)
		if a.Status == stage {
			return a
		}
		if _, err := s.CompleteStage(ctx, LeaseOf(a), t0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSaveExtracted(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	a := claimAt(t, s, "https://example.com/meta", StatusExtracting)

	published := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	m := ExtractedMetadata{Title: "Title", Author: "Jane", SiteName: "Site", PublishedAt: published, WordCount: 1234, ImageCount: 3}
	if err := s.SaveExtracted(ctx, LeaseOf(a), m, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, a.ID)
	if got.Title != "Title" || got.Author != "Jane" || got.SiteName != "Site" || !got.PublishedAt.Equal(published) ||
		got.WordCount != 1234 || got.ImageCount != 3 || !got.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("article = %+v", got)
	}
	// The lease is still held: the stage can complete afterwards.
	if got.Status != StatusExtracting || got.LeaseID != a.LeaseID {
		t.Errorf("SaveExtracted changed status or lease: %+v", got)
	}
	if _, err := s.CompleteStage(ctx, LeaseOf(a), t0); err != nil {
		t.Errorf("CompleteStage after SaveExtracted: %v", err)
	}
}

func TestSaveExtractedEmptyFieldsAreNull(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	a := claimAt(t, s, "https://example.com/meta", StatusExtracting)
	if err := s.SaveExtracted(context.Background(), LeaseOf(a), ExtractedMetadata{WordCount: 50}, t0); err != nil {
		t.Fatal(err)
	}
	var title, author, site, published, images any
	if err := s.db.QueryRow(`SELECT title, author, site_name, published_at, image_count FROM articles WHERE id = ?`, a.ID).
		Scan(&title, &author, &site, &published, &images); err != nil {
		t.Fatal(err)
	}
	if title != nil || author != nil || site != nil || published != nil {
		t.Errorf("empty fields stored as %v / %v / %v / %v, want NULL", title, author, site, published)
	}
	if images != int64(0) {
		t.Errorf("image_count = %#v, want 0 (not NULL: extracted, no images)", images)
	}
}

func TestSaveExtractedIsLeaseGuarded(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	a := claimAt(t, s, "https://example.com/meta", StatusExtracting)

	stale := LeaseOf(a)
	stale.Token++
	if err := s.SaveExtracted(ctx, stale, ExtractedMetadata{Title: "x"}, t0); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale token: err = %v, want ErrLeaseLost", err)
	}
	wrongStage := LeaseOf(a)
	wrongStage.Stage = StatusFetching
	if err := s.SaveExtracted(ctx, wrongStage, ExtractedMetadata{Title: "x"}, t0); err == nil || errors.Is(err, ErrLeaseLost) {
		t.Errorf("wrong stage: err = %v, want a stage error", err)
	}
	if got := mustGet(t, s, a.ID); got.Title != "" {
		t.Errorf("rejected writes changed the title to %q", got.Title)
	}
}

// Re-extraction (e.g. on reprocess) replaces everything, clearing fields the
// new extraction didn't find.
func TestSaveExtractedOverwritesAndClears(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := context.Background()
	a := claimAt(t, s, "https://example.com/meta", StatusExtracting)
	full := ExtractedMetadata{Title: "T", Author: "A", SiteName: "S", PublishedAt: t0, WordCount: 100, ImageCount: 4}
	if err := s.SaveExtracted(ctx, LeaseOf(a), full, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveExtracted(ctx, LeaseOf(a), ExtractedMetadata{WordCount: 10}, t0); err != nil {
		t.Fatal(err)
	}
	got := mustGet(t, s, a.ID)
	if got.Title != "" || got.Author != "" || got.SiteName != "" || !got.PublishedAt.IsZero() ||
		got.WordCount != 10 || got.ImageCount != 0 {
		t.Errorf("after second save: %+v", got)
	}
}

func TestSaveExtractedRejectsNegativeCounts(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	a := claimAt(t, s, "https://example.com/meta", StatusExtracting)
	if err := s.SaveExtracted(context.Background(), LeaseOf(a), ExtractedMetadata{WordCount: -1}, t0); err == nil {
		t.Error("negative word count accepted")
	}
}

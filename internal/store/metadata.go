package store

import (
	"context"
	"fmt"
	"time"
)

// ExtractedMetadata is what the extract stage learns about an article.
// Empty strings and a zero PublishedAt are stored as NULL; counts are stored
// as given (0 images is meaningful: extracted, none found).
type ExtractedMetadata struct {
	Title       string
	Author      string
	SiteName    string
	PublishedAt time.Time
	WordCount   int
	ImageCount  int
}

// SaveExtracted records extraction metadata for an article leased at the
// extracting stage, replacing all previous values (so re-extraction clears
// fields that are now empty). Like every stage write it is lease-guarded, so
// a worker that lost its lease can't overwrite the new owner's results.
func (s *Store) SaveExtracted(ctx context.Context, l Lease, m ExtractedMetadata, now time.Time) error {
	if l.Stage != StatusExtracting {
		return fmt.Errorf("SaveExtracted: lease is for stage %q, not %q", l.Stage, StatusExtracting)
	}
	if m.WordCount < 0 || m.ImageCount < 0 {
		return fmt.Errorf("SaveExtracted: negative count (words %d, images %d)", m.WordCount, m.ImageCount)
	}
	return s.leaseWrite(ctx, l, `
		title = ?, author = ?, site_name = ?, published_at = ?, word_count = ?, image_count = ?, updated_at = ?`,
		nullString(m.Title), nullString(m.Author), nullString(m.SiteName), nullMillis(m.PublishedAt),
		m.WordCount, m.ImageCount, now.UnixMilli())
}

// SaveAudio records the episode's size and length for an article leased at
// the synthesizing stage (lease-guarded, like every stage write).
func (s *Store) SaveAudio(ctx context.Context, l Lease, bytes int64, duration time.Duration, now time.Time) error {
	if l.Stage != StatusSynthesizing {
		return fmt.Errorf("SaveAudio: lease is for stage %q, not %q", l.Stage, StatusSynthesizing)
	}
	if bytes <= 0 || duration.Milliseconds() <= 0 {
		return fmt.Errorf("SaveAudio: implausible audio (%d bytes, %s)", bytes, duration)
	}
	return s.leaseWrite(ctx, l, `audio_bytes = ?, duration_ms = ?, updated_at = ?`,
		bytes, duration.Milliseconds(), now.UnixMilli())
}

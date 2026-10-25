// Package stages implements the real pipeline stages. Each reads the
// previous stage's artifacts from the article's directory and writes its own
// (see internal/artifact), so any stage can be re-run on its own.
package stages

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"time"

	"github.com/tony-montemuro/phonetic/internal/artifact"
	"github.com/tony-montemuro/phonetic/internal/extract"
	"github.com/tony-montemuro/phonetic/internal/fetch"
	"github.com/tony-montemuro/phonetic/internal/pipeline"
	"github.com/tony-montemuro/phonetic/internal/store"
)

// FetchMeta is the content of fetch.json.
type FetchMeta struct {
	// URL is the final URL after redirects.
	URL                 string    `json:"url"`
	ContentType         string    `json:"content_type"`
	OriginalContentType string    `json:"original_content_type"`
	FetchedAt           time.Time `json:"fetched_at"`
}

// Fetcher downloads pages (*fetch.Fetcher implements it).
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (fetch.Result, error)
}

// Fetch downloads the article page into raw.html and fetch.json.
type Fetch struct {
	Fetcher Fetcher
	DataDir string
}

// Run implements pipeline.Stage.
func (s Fetch) Run(ctx context.Context, a store.Article) error {
	res, err := s.Fetcher.Fetch(ctx, a.URL)
	if err != nil {
		err = fmt.Errorf("fetching: %w", err)
		if fetch.IsPermanent(err) {
			return pipeline.Permanent(err)
		}
		return err
	}
	// Don't write once cancelled (shutdown or lost lease): narrows the window
	// in which a superseded worker overwrites the new owner's files.
	if err := ctx.Err(); err != nil {
		return err
	}
	// Two separate atomic writes: a superseded worker could pair its
	// fetch.json with the new owner's raw.html. Accepted (see package
	// artifact): both describe the same URL, and stages are re-runnable.
	// Write errors (e.g. a full disk) are transient: retried with backoff.
	dir := artifact.For(s.DataDir, a.ID)
	if err := dir.WriteFile(artifact.RawHTML, res.Body); err != nil {
		return fmt.Errorf("saving page: %w", err)
	}
	meta := FetchMeta{
		URL:                 res.URL,
		ContentType:         res.ContentType,
		OriginalContentType: res.OriginalContentType,
		FetchedAt:           res.FetchedAt.UTC(),
	}
	if err := dir.WriteJSON(artifact.FetchMeta, meta); err != nil {
		return fmt.Errorf("saving fetch metadata: %w", err)
	}
	return nil
}

// MetadataStore records extraction results (*store.Store implements it).
type MetadataStore interface {
	SaveExtracted(ctx context.Context, l store.Lease, m store.ExtractedMetadata, now time.Time) error
}

// Extract turns raw.html into extracted.json and records the article's
// metadata (title, author, word and image counts...) in the store.
type Extract struct {
	Store   MetadataStore
	DataDir string
}

// Run implements pipeline.Stage.
func (s Extract) Run(ctx context.Context, a store.Article) error {
	dir := artifact.For(s.DataDir, a.ID)
	var meta FetchMeta
	if err := dir.ReadJSON(artifact.FetchMeta, &meta); err != nil {
		return inputErr(err)
	}
	page, err := dir.ReadFile(artifact.RawHTML)
	if err != nil {
		return inputErr(err)
	}
	pageURL, err := parseHTTPURL(meta.URL)
	if err != nil {
		return pipeline.Permanent(fmt.Errorf("fetch output has no usable final URL: %w; the article must be re-fetched", err))
	}

	// Extraction is CPU-bound and bounded by the fetcher's size limit, so it
	// doesn't watch ctx; check it before writing instead.
	art, err := extract.Extract(page, pageURL)
	if errors.Is(err, extract.ErrNoContent) {
		return pipeline.Permanent(err)
	}
	if err != nil {
		return fmt.Errorf("extracting: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dir.WriteJSON(artifact.Extracted, art); err != nil {
		return fmt.Errorf("saving extraction: %w", err)
	}
	err = s.Store.SaveExtracted(ctx, store.LeaseOf(a), store.ExtractedMetadata{
		Title:       art.Title,
		Author:      art.Byline,
		SiteName:    art.SiteName,
		PublishedAt: art.PublishedAt,
		WordCount:   art.WordCount,
		ImageCount:  len(art.Images),
	}, time.Now())
	if err != nil {
		return fmt.Errorf("saving metadata: %w", err)
	}
	return nil
}

// parseHTTPURL parses an absolute http(s) URL, the only kind a page can be
// fetched from (and relative links resolved against).
func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%q is not an absolute http(s) URL", raw)
	}
	return u, nil
}

// inputErr classifies a failure to read the fetch stage's output. A missing
// or corrupt file won't be fixed by retrying extract (the article must be
// re-fetched), so those are permanent; other read errors may be transient.
func inputErr(err error) error {
	var syntaxErr *jsontext.SyntacticError
	var semanticErr *json.SemanticError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return pipeline.Permanent(fmt.Errorf("fetch output missing; the article must be re-fetched: %w", err))
	case errors.As(err, &syntaxErr) || errors.As(err, &semanticErr):
		return pipeline.Permanent(fmt.Errorf("fetch output corrupt; the article must be re-fetched: %w", err))
	}
	return fmt.Errorf("reading fetch output: %w", err)
}

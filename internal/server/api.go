package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tony-montemuro/phonetic/internal/store"
	"github.com/tony-montemuro/phonetic/internal/urlnorm"
)

// ArticleStore is the persistence the API needs (*store.Store implements it).
type ArticleStore interface {
	InsertArticle(ctx context.Context, url string, now time.Time) (store.Article, bool, error)
	GetArticle(ctx context.Context, id int64) (store.Article, error)
	ListArticles(ctx context.Context, opts store.ListOptions) ([]store.Article, error)
}

// Notifier is told when new work is queued (*pipeline.Runner implements it).
type Notifier interface {
	Notify()
}

const (
	// maxBodyBytes bounds JSON request bodies.
	maxBodyBytes = 16 << 10
	// List limits for GET /api/articles.
	defaultListLimit = 100
	maxListLimit     = 500
)

// articleJSON is the API representation of an article.
type articleJSON struct {
	ID              int64        `json:"id"`
	URL             string       `json:"url"`
	Status          store.Status `json:"status"`
	FailedStage     store.Status `json:"failed_stage,omitzero"`
	Error           string       `json:"error,omitzero"`
	Attempts        int          `json:"attempts"`
	Title           string       `json:"title,omitzero"`
	Author          string       `json:"author,omitzero"`
	SiteName        string       `json:"site_name,omitzero"`
	PublishedAt     time.Time    `json:"published_at,omitzero"`
	WordCount       int          `json:"word_count,omitzero"`
	ImageCount      int          `json:"image_count,omitzero"`
	ImageImportance *int         `json:"image_importance,omitzero"`
	ImageNotes      string       `json:"image_notes,omitzero"`
	DurationMS      int64        `json:"duration_ms,omitzero"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	ReadyAt         time.Time    `json:"ready_at,omitzero"`
}

func toArticleJSON(a store.Article) articleJSON {
	return articleJSON{
		ID: a.ID, URL: a.URL, Status: a.Status, FailedStage: a.FailedStage, Error: a.Error,
		Attempts: a.Attempts, Title: a.Title, Author: a.Author, SiteName: a.SiteName,
		PublishedAt: a.PublishedAt, WordCount: a.WordCount, ImageCount: a.ImageCount,
		ImageImportance: a.ImageImportance, ImageNotes: a.ImageNotes,
		DurationMS: a.Duration.Milliseconds(),
		CreatedAt:  a.CreatedAt, UpdatedAt: a.UpdatedAt, ReadyAt: a.ReadyAt,
	}
}

// handleCreateArticle queues a URL. 202 for a new article, 200 with the
// existing one for a duplicate.
func handleCreateArticle(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "body must be exactly {\"url\": \"https://...\"}: "+err.Error())
			return
		}
		url, err := urlnorm.Normalize(req.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		a, created, err := d.Articles.InsertArticle(r.Context(), url, d.now())
		if err != nil {
			d.Logger.Error("inserting article", "url", url, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		self := "/api/articles/" + strconv.FormatInt(a.ID, 10)
		if !created {
			// Already known (any status, including failed: use reprocess to
			// retry). The body is that article.
			w.Header().Set("Content-Location", self)
			writeJSON(w, http.StatusOK, toArticleJSON(a))
			return
		}
		d.Logger.Info("article queued", "article", a.ID, "url", url)
		d.Notifier.Notify()
		w.Header().Set("Location", self)
		writeJSON(w, http.StatusAccepted, toArticleJSON(a))
	}
}

func handleGetArticle(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "article not found")
			return
		}
		a, err := d.Articles.GetArticle(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "article not found")
			return
		}
		if err != nil {
			d.Logger.Error("getting article", "article", id, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, toArticleJSON(a))
	}
}

// handleListArticles lists articles newest first. Query parameters: status
// (optional filter) and limit (1-500, default 100).
func handleListArticles(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		opts := store.ListOptions{Limit: defaultListLimit}
		if v := q.Get("status"); v != "" {
			opts.Status = store.Status(v)
			if !opts.Status.Valid() {
				writeError(w, http.StatusBadRequest, "unknown status "+strconv.Quote(v))
				return
			}
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxListLimit {
				writeError(w, http.StatusBadRequest, "limit must be 1-"+strconv.Itoa(maxListLimit))
				return
			}
			opts.Limit = n
		}
		as, err := d.Articles.ListArticles(r.Context(), opts)
		if err != nil {
			d.Logger.Error("listing articles", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		out := make([]articleJSON, len(as)) // non-nil: encodes as [] when empty
		for i, a := range as {
			out[i] = toArticleJSON(a)
		}
		writeJSON(w, http.StatusOK, map[string]any{"articles": out})
	}
}

// decodeJSON strictly decodes a bounded JSON body into v: unknown fields and
// trailing data are rejected.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// parseID parses a positive decimal ID, accepting only its canonical
// spelling (no sign, leading zeros or escapes), so each resource has exactly
// one URL.
func parseID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		return 0, false
	}
	return id, true
}

// Package server implements phonetic's HTTP interface.
package server

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"time"
)

// Pinger reports whether a dependency (the database) is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the server's collaborators and settings.
type Deps struct {
	Logger   *slog.Logger
	DB       Pinger
	Articles ArticleStore
	Notifier Notifier
	// APIToken guards /api/. An empty token locks the API (nothing matches).
	APIToken string
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// healthTimeout bounds how long /healthz waits on the database. A variable so
// tests can shorten it.
var healthTimeout = 2 * time.Second

// New returns the root handler for all routes. With an empty APIToken the
// API is locked (every /api/ request gets 401). With a token set, Articles and
// Notifier are required; New panics without them, since that is a wiring bug.
func New(d Deps) http.Handler {
	if d.APIToken != "" && (d.Articles == nil || d.Notifier == nil) {
		panic("server: APIToken is set but Articles or Notifier is nil")
	}
	auth := func(h http.Handler) http.Handler { return requireBearer(d.APIToken, h) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz(d.Logger, d.DB))
	mux.Handle("POST /api/articles", auth(handleCreateArticle(d)))
	mux.Handle("GET /api/articles", auth(handleListArticles(d)))
	mux.Handle("GET /api/articles/{id}", auth(handleGetArticle(d)))
	// Anything else under /api/ (unknown paths, other methods) gets auth and
	// a JSON 404, instead of ServeMux's plain-text 404/405 without auth.
	mux.Handle("/api/", auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})))
	return logRequests(d.Logger, mux)
}

// handleHealthz is a readiness check: it reports unavailable when the database
// can't be reached.
func handleHealthz(logger *slog.Logger, db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			logger.Error("health check failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// writeJSON marshals v completely before sending anything, so an encoding
// failure becomes a clean 500 instead of a truncated body under an
// already-sent status. Invalid UTF-8 (e.g. from a scraped page) is replaced
// with U+FFFD, as encoding/json v1 did; Deterministic sorts map keys.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v, json.Deterministic(true), jsontext.AllowInvalidUTF8(true))
	if err != nil {
		slog.Error("encoding JSON response", "err", err)
		status, body = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// A write error here can only be a broken client connection.
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

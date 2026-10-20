// Package server implements phonetic's HTTP interface.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Pinger reports whether a dependency (the database) is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the server's collaborators.
type Deps struct {
	Logger *slog.Logger
	DB     Pinger
}

// healthTimeout bounds how long /healthz waits on the database. A variable so
// tests can shorten it.
var healthTimeout = 2 * time.Second

// New returns the root handler for all routes.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz(d.Logger, d.DB))
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is already sent; an encode error can only be a broken client
	// connection, so there's nothing useful left to do with it.
	_ = json.NewEncoder(w).Encode(v)
}

package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// logRequests logs one line per request with its status and duration.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		// Deferred so a request whose handler panics is still logged.
		defer func() {
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"pattern", r.Pattern, // set by ServeMux on this same request
				"status", sw.status,
				"bytes", sw.bytes,
				"duration", time.Since(start),
				"remote", r.RemoteAddr,
			)
		}()
		next.ServeHTTP(sw, r)
	})
}

// statusWriter records the response status and size.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	// 1xx informational responses (e.g. 103 Early Hints) precede the final one.
	if !w.wroteHeader && code >= 200 {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom keeps the underlying writer's io.ReaderFrom fast path (sendfile
// for http.ServeFile) available through the wrapper.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	w.wroteHeader = true
	n, err := io.Copy(w.ResponseWriter, r)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush,
// deadlines, etc.).
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// hangingPinger blocks until the context ends, like an unresponsive database.
type hangingPinger struct{}

func (hangingPinger) Ping(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestServer(t *testing.T, db Pinger) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	srv := httptest.NewServer(New(Deps{Logger: logger, DB: db}))
	t.Cleanup(srv.Close)
	return srv, &logs
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
		wantBody   string
	}{
		{"healthy", nil, http.StatusOK, "ok"},
		{"db down", errors.New("disk on fire"), http.StatusServiceUnavailable, "unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, logs := newTestServer(t, fakePinger{tt.pingErr})

			resp, err := http.Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			var body struct{ Status string }
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Status != tt.wantBody {
				t.Errorf("status field = %q, want %q", body.Status, tt.wantBody)
			}
			if tt.pingErr != nil && !strings.Contains(logs.String(), "disk on fire") {
				t.Errorf("ping error not logged; logs:\n%s", logs)
			}
		})
	}
}

func TestHealthzTimesOutOnHangingDB(t *testing.T) {
	old := healthTimeout
	healthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthTimeout = old })

	srv, _ := newTestServer(t, hangingPinger{})
	start := time.Now()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("healthz took %s; timeout not applied", elapsed)
	}
}

func TestRouting(t *testing.T) {
	srv, _ := newTestServer(t, fakePinger{})

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodHead, "/healthz", http.StatusOK}, // GET patterns also match HEAD
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestLogRequestsRecordsStatusAndBytes(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := logRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusOK) // superfluous; must not change recorded status
		io.WriteString(w, "hello")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	out := logs.String()
	for _, want := range []string{"status=418", "bytes=5", "path=/x", "method=GET"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}
}

func TestLogRequestsLogsPanickingHandler(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	h := logRequests(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	func() {
		defer func() { recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/p", nil))
	}()
	if !strings.Contains(logs.String(), "path=/p") {
		t.Errorf("panicking request not logged: %s", logs.String())
	}
}

func TestLogRequestsIncludesPattern(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(http.ResponseWriter, *http.Request) {})
	logRequests(logger, mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/42", nil))
	if !strings.Contains(logs.String(), `pattern="GET /items/{id}"`) {
		t.Errorf("pattern not logged: %s", logs.String())
	}
}

// Uses a real server: httptest.ResponseRecorder treats a 1xx as final.
func TestLogRequestsIgnoresInformationalStatus(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	srv := httptest.NewServer(logRequests(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusTeapot)
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	srv.Close() // waits for the handler (and its deferred log) to finish
	if !strings.Contains(logs.String(), "status=418") {
		t.Errorf("want final status 418 logged: %s", logs.String())
	}
}

func TestStatusWriterImplicitOKAndReadFrom(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	n, err := sw.ReadFrom(strings.NewReader("abcdef"))
	if err != nil || n != 6 {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if sw.status != http.StatusOK || sw.bytes != 6 || rec.Body.String() != "abcdef" {
		t.Errorf("status=%d bytes=%d body=%q", sw.status, sw.bytes, rec.Body.String())
	}
	if http.NewResponseController(sw).Flush() != nil {
		t.Error("Flush via ResponseController should reach the recorder")
	}
}

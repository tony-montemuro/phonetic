package tts

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// wavHeader is a minimal (empty-data) WAV file, enough for the RIFF check.
var wavHeader = []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00")

// fakeEngine serves /v1/audio/speech, recording requests and replying with
// handle (or a WAV by default). Read the recordings only after the calls
// that make them return.
func fakeEngine(t *testing.T, handle http.HandlerFunc) (*httptest.Server, *[]speechRequest, *[]http.Header) {
	t.Helper()
	var mu sync.Mutex
	var reqs []speechRequest
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/speech" {
			http.NotFound(w, r)
			return
		}
		var req speechRequest
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		reqs = append(reqs, req)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if handle != nil {
			handle(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wavHeader)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, &headers
}

func TestSynthesizeSendsRequestAndWritesWAV(t *testing.T) {
	t.Parallel()
	srv, reqs, headers := fakeEngine(t, nil)
	c := NewOpenAI(OpenAI{BaseURL: srv.URL + "/v1/", Voice: "bf_emma", Speed: 1.25, APIKey: "sk-test"})
	var out bytes.Buffer
	if err := c.Synthesize(context.Background(), "Hello there.", &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), wavHeader) {
		t.Errorf("audio = %q", out.Bytes())
	}
	want := speechRequest{Model: DefaultModel, Input: "Hello there.", Voice: "bf_emma", ResponseFormat: "wav", Speed: 1.25, Stream: false}
	if len(*reqs) != 1 || (*reqs)[0] != want {
		t.Errorf("request = %+v, want %+v", *reqs, want)
	}
	if got := (*headers)[0].Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestSynthesizeWithoutAPIKeySendsNoAuth(t *testing.T) {
	t.Parallel()
	srv, _, headers := fakeEngine(t, nil)
	if err := NewOpenAI(OpenAI{BaseURL: srv.URL + "/v1"}).Synthesize(context.Background(), "x", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := (*headers)[0].Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

func TestSynthesizeErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		handle    http.HandlerFunc
		permanent bool
		mention   string
	}{
		{"bad input", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"detail":"text too long"}`, http.StatusBadRequest)
		}, true, "text too long"},
		{"validation error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"detail":"speed out of range"}`, http.StatusUnprocessableEntity)
		}, true, "422"},
		{"rate limited", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
		}, false, "429"},
		{"unavailable", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "loading model", http.StatusServiceUnavailable)
		}, false, "503"},
		{"too much audio", func(w http.ResponseWriter, r *http.Request) {
			w.Write(wavHeader[:12])
			chunk := make([]byte, 1<<20)
			for range maxAudioBytes>>20 + 1 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}, false, "exceeds"},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "model crashed", http.StatusInternalServerError)
		}, false, "500"},
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad key", http.StatusUnauthorized)
		}, false, "401"},
		{"json served as 200", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"error":"something went wrong"}`))
		}, false, "not WAV"},
		{"empty body", func(w http.ResponseWriter, r *http.Request) {}, false, "short response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := fakeEngine(t, tt.handle)
			err := NewOpenAI(OpenAI{BaseURL: srv.URL + "/v1"}).Synthesize(context.Background(), "x", &bytes.Buffer{})
			if err == nil || IsPermanent(err) != tt.permanent || !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("err = %v (permanent %v), want permanent=%v mentioning %q", err, IsPermanent(err), tt.permanent, tt.mention)
			}
		})
	}
}

func TestSynthesizeUnreachableIsTransient(t *testing.T) {
	t.Parallel()
	srv, _, _ := fakeEngine(t, nil)
	url := srv.URL
	srv.Close()
	err := NewOpenAI(OpenAI{BaseURL: url + "/v1"}).Synthesize(context.Background(), "x", &bytes.Buffer{})
	if err == nil || IsPermanent(err) {
		t.Errorf("err = %v, want a transient connection error", err)
	}
}

func TestSynthesizeHonorsContext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv, _, _ := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := NewOpenAI(OpenAI{BaseURL: srv.URL + "/v1"}).Synthesize(ctx, "x", &bytes.Buffer{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}

// Cancellation while the audio is arriving also stops the call.
func TestSynthesizeHonorsContextMidBody(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, _, _ := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(wavHeader[:12])
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	})
	err := NewOpenAI(OpenAI{BaseURL: srv.URL + "/v1"}).Synthesize(ctx, "x", &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want canceled", err)
	}
}

func TestDefaultsAndFingerprint(t *testing.T) {
	t.Parallel()
	c := NewOpenAI(OpenAI{})
	if c.BaseURL != DefaultBaseURL || c.Model != DefaultModel || c.Voice != DefaultVoice ||
		c.Speed != DefaultSpeed || c.MaxChunkBytes() != DefaultMaxBytes || c.FileExt() != "wav" {
		t.Errorf("defaults = %+v", c)
	}
	same := NewOpenAI(OpenAI{})
	otherVoice := NewOpenAI(OpenAI{Voice: "bf_emma"})
	otherSpeed := NewOpenAI(OpenAI{Speed: 1.1})
	otherModel := NewOpenAI(OpenAI{Model: "tts-1"})
	moved := NewOpenAI(OpenAI{BaseURL: "http://localhost:9999/v1", APIKey: "k"})
	if c.Fingerprint() != same.Fingerprint() || c.Fingerprint() != moved.Fingerprint() {
		t.Error("fingerprint not stable, or depends on where the engine runs")
	}
	for _, o := range []*OpenAI{otherVoice, otherSpeed, otherModel} {
		if c.Fingerprint() == o.Fingerprint() {
			t.Errorf("fingerprint ignores a difference: %+v", o)
		}
	}
}

// Optional check against a real engine: set PHONETIC_TEST_TTS_URL, e.g.
// http://127.0.0.1:8880/v1 with `make kokoro-up` running.
func TestRealEngine(t *testing.T) {
	url := os.Getenv("PHONETIC_TEST_TTS_URL")
	if url == "" {
		t.Skip("set PHONETIC_TEST_TTS_URL to test against a real engine")
	}
	var out bytes.Buffer
	if err := NewOpenAI(OpenAI{BaseURL: url}).Synthesize(context.Background(), "Testing one two three.", &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() < 10000 || !bytes.HasPrefix(out.Bytes(), []byte("RIFF")) {
		t.Errorf("suspicious audio: %d bytes starting %q", out.Len(), out.Bytes()[:min(12, out.Len())])
	}
}

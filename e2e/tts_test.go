//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/testutil"
)

// chunkSpeech is how much audio the fake engine returns per request.
const chunkSpeech = 400 * time.Millisecond

// speechRequest is the JSON body of POST /v1/audio/speech.
type speechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
}

// speechEngine is a fake OpenAI-compatible speech API (like Kokoro-FastAPI)
// running in the test process. Each request returns chunkSpeech of tone as
// WAV, after Delay. It records the requests and the peak number running at
// once.
type speechEngine struct {
	srv   *httptest.Server
	Delay atomic.Int64 // time.Duration each request takes
	Fail  atomic.Int32 // if non-zero, respond with this HTTP status

	inFlight atomic.Int32
	peak     atomic.Int32

	mu       sync.Mutex
	requests []speechRequest
}

func newSpeechEngine(t testing.TB) *speechEngine {
	t.Helper()
	e := &speechEngine{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		n := e.inFlight.Add(1)
		defer e.inFlight.Add(-1)
		for p := e.peak.Load(); n > p && !e.peak.CompareAndSwap(p, n); p = e.peak.Load() {
		}
		var req speechRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Input == "" || req.ResponseFormat != "wav" {
			http.Error(w, "bad request", http.StatusUnprocessableEntity)
			return
		}
		e.mu.Lock()
		e.requests = append(e.requests, req)
		e.mu.Unlock()
		if code := e.Fail.Load(); code != 0 {
			http.Error(w, "engine failure", int(code))
			return
		}
		select {
		case <-time.After(time.Duration(e.Delay.Load())):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(testutil.WAV(chunkSpeech, 24000))
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// BaseURL is the value for PHONETIC_TTS_URL.
func (e *speechEngine) BaseURL() string { return e.srv.URL + "/v1" }

// Requests returns a copy of the requests received so far.
func (e *speechEngine) Requests() []speechRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]speechRequest(nil), e.requests...)
}

// Peak is the most requests that were ever in flight at once.
func (e *speechEngine) Peak() int { return int(e.peak.Load()) }

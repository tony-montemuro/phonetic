// Package tts turns text into speech. Engines implement Synthesizer; the one
// implementation today speaks the OpenAI speech API, which Kokoro-FastAPI
// (local, GPU or CPU) and OpenAI itself both serve.
package tts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Synthesizer turns one chunk of text into audio.
type Synthesizer interface {
	// Synthesize writes the audio for text to w, in the format named by
	// FileExt. It must honor ctx. On error, w may hold partial output, which
	// the caller must discard.
	Synthesize(ctx context.Context, text string, w io.Writer) error
	// MaxChunkBytes is the longest text, in bytes, one call accepts (bytes
	// are at least the character count, so this is safe for APIs that limit
	// characters).
	MaxChunkBytes() int
	// FileExt is the audio format's file extension, e.g. "wav".
	FileExt() string
	// Fingerprint identifies the request settings that affect the audio
	// (model, voice, speed, format...), not where the engine runs. Cached
	// chunks made with other settings are re-synthesized.
	Fingerprint() string
}

// StatusError is a non-200 response from the speech API.
type StatusError struct {
	Code int
	Body string // start of the response body, for diagnosis
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("speech API: HTTP %d: %s", e.Code, e.Body)
}

// IsPermanent reports whether retrying the same request is pointless: the
// engine rejected it (400, 413, 422). That covers bad input, but Kokoro also
// answers an unknown model or voice with 400, so a misconfigured voice fails
// articles at their first chunk (with the engine's message). Everything else
// (unreachable engine, 5xx, 429, a bad API key) is worth retrying.
func IsPermanent(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			return true
		}
	}
	return false
}

// OpenAI defaults (Kokoro-FastAPI on the same machine).
const (
	DefaultBaseURL  = "http://127.0.0.1:8880/v1"
	DefaultModel    = "kokoro"
	DefaultVoice    = "af_heart"
	DefaultSpeed    = 1.0
	DefaultMaxBytes = 1500
	// The speech API's speed range (OpenAI's; Kokoro-FastAPI accepts the same).
	MinSpeed = 0.25
	MaxSpeed = 4.0
	// maxAudioBytes bounds one chunk's response (WAV at 24 kHz is about
	// 48 KB per second, so this is minutes of speech).
	maxAudioBytes = 64 << 20
	// maxErrorBody bounds how much of an error response is kept.
	maxErrorBody = 1 << 10
)

// OpenAI is a client for the OpenAI-compatible speech endpoint
// (POST {BaseURL}/audio/speech). It requests WAV, so chunks decode exactly
// before the final MP3 encode.
//
// Kokoro-FastAPI notes: it streams by default, which turns server errors
// into dropped connections and changes the output slightly, so requests
// disable streaming explicitly; and it may skip a sentence it fails on
// (logging it) rather than failing the request.
type OpenAI struct {
	BaseURL  string  // e.g. http://127.0.0.1:8880/v1
	APIKey   string  // sent as a bearer token if set (OpenAI); Kokoro needs none
	Model    string  // e.g. "kokoro", "tts-1"
	Voice    string  // e.g. "af_heart", "alloy"
	Speed    float64 // MinSpeed-MaxSpeed
	MaxBytes int     // per request; OpenAI's limit is 4096 characters
	Client   *http.Client
}

// NewOpenAI returns a client with defaults filled in for zero fields.
func NewOpenAI(c OpenAI) *OpenAI {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.Voice == "" {
		c.Voice = DefaultVoice
	}
	if c.Speed == 0 {
		c.Speed = DefaultSpeed
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.Client == nil {
		// Generous: a long chunk on a CPU-only machine can take minutes.
		c.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	return &c
}

// MaxChunkBytes implements Synthesizer.
func (c *OpenAI) MaxChunkBytes() int { return c.MaxBytes }

// FileExt implements Synthesizer.
func (c *OpenAI) FileExt() string { return "wav" }

// Fingerprint implements Synthesizer.
func (c *OpenAI) Fingerprint() string {
	body, err := json.Marshal(c.request(""), json.Deterministic(true))
	if err != nil {
		panic(err) // a struct of strings and numbers always marshals
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

// Synthesize implements Synthesizer.
func (c *OpenAI) Synthesize(ctx context.Context, text string, w io.Writer) error {
	body, err := json.Marshal(c.request(text))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/wav")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("speech API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	// Sanity-check that it's really WAV before handing it on: ffmpeg's
	// error for a JSON error body served as 200 would be much less clear.
	head := make([]byte, 12)
	if _, err := io.ReadFull(resp.Body, head); err != nil {
		return fmt.Errorf("speech API: short response: %w", err)
	}
	if string(head[:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return fmt.Errorf("speech API: response is not WAV (Content-Type %q)", resp.Header.Get("Content-Type"))
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxAudioBytes+1))
	if err != nil {
		return fmt.Errorf("speech API: reading audio: %w", err)
	}
	if n > maxAudioBytes {
		return fmt.Errorf("speech API: audio exceeds %d bytes", maxAudioBytes)
	}
	return nil
}

// request builds the speech request body for text.
func (c *OpenAI) request(text string) speechRequest {
	return speechRequest{Model: c.Model, Input: text, Voice: c.Voice, ResponseFormat: "wav", Speed: c.Speed}
}

// speechRequest is the body of POST /audio/speech. Stream is a
// Kokoro-FastAPI extension (default true there); false matches OpenAI's
// behavior.
type speechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed"`
	Stream         bool    `json:"stream"`
}

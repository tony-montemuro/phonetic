package stages

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/artifact"
	"github.com/tony-montemuro/phonetic/internal/audio"
	"github.com/tony-montemuro/phonetic/internal/extract"
	"github.com/tony-montemuro/phonetic/internal/pipeline"
	"github.com/tony-montemuro/phonetic/internal/store"
	"github.com/tony-montemuro/phonetic/internal/testutil"
	"github.com/tony-montemuro/phonetic/internal/tts"
)

// chunkAudio is how long each fake chunk's audio is.
const chunkAudio = 500 * time.Millisecond

// requireFFmpeg skips tests that need the real tools when they're missing.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if err := (audio.Tools{}).Check(); err != nil {
		t.Skip(err)
	}
}

// writeScript writes what the script stage would have produced.
func writeScript(t *testing.T, dir artifact.Dir, text string) {
	t.Helper()
	if err := dir.WriteFile(artifact.Script, []byte(text)); err != nil {
		t.Fatal(err)
	}
}

// fakeTTS returns chunkAudio of tone per chunk and records what it was asked
// to say. If failAt is set, any chunk containing it fails with err.
type fakeTTS struct {
	maxBytes    int
	fingerprint string
	failAt      string
	err         error

	mu   sync.Mutex
	said []string
}

func (f *fakeTTS) Synthesize(ctx context.Context, text string, w io.Writer) error {
	f.mu.Lock()
	f.said = append(f.said, text)
	f.mu.Unlock()
	if f.failAt != "" && strings.Contains(text, f.failAt) {
		return f.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := w.Write(testutil.WAV(chunkAudio, 24000))
	return err
}

func (f *fakeTTS) MaxChunkBytes() int  { return f.maxBytes }
func (f *fakeTTS) FileExt() string     { return "wav" }
func (f *fakeTTS) Fingerprint() string { return f.fingerprint }

func (f *fakeTTS) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.said...)
}

// threeParagraphs chunks into exactly three chunks at maxBytes 30.
const threeParagraphs = "First paragraph here.\n\nSecond paragraph here.\n\nThird paragraph here."

func TestSpokenIntro(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		art  extract.Article
		want string
	}{
		{"all parts", extract.Article{Title: "Big News", Byline: "Jane Doe", SiteName: "The Paper"},
			"Big News. By Jane Doe. From The Paper."},
		{"site suffix stripped", extract.Article{Title: "Big News - The Paper", SiteName: "The Paper"},
			"Big News. From The Paper."},
		{"pipe suffix stripped", extract.Article{Title: "Big News | The Paper", SiteName: "The Paper"},
			"Big News. From The Paper."},
		{"byline already says by", extract.Article{Title: "Big News", Byline: "by Jane Doe"},
			"Big News. by Jane Doe."},
		{"byline names the site", extract.Article{Title: "T", Byline: "The Paper Staff", SiteName: "The Paper"},
			"T. By The Paper Staff."},
		{"byline with colon", extract.Article{Title: "T", Byline: "By: Jane Doe"}, "T. By: Jane Doe."},
		{"byline written by", extract.Article{Title: "T", Byline: "Written by Jane Doe"}, "T. Written by Jane Doe."},
		{"byline name containing by", extract.Article{Title: "T", Byline: "Bobby Byrne"}, "T. By Bobby Byrne."},
		{"title is the site", extract.Article{Title: "The Paper", SiteName: "The Paper"}, "The Paper."},
		{"title punctuation kept", extract.Article{Title: "Why Now?"}, "Why Now?"},
		{"title only", extract.Article{Title: "Big News"}, "Big News."},
		{"nothing", extract.Article{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := spokenIntro(c.art); got != c.want {
				t.Errorf("spokenIntro = %q, want %q", got, c.want)
			}
		})
	}
}

func TestScriptStage(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	a := store.Article{ID: 1}
	dir := artifact.For(dataDir, a.ID)
	art := extract.Article{
		Title:    "Big News - The Paper",
		Byline:   "Jane Doe",
		SiteName: "The Paper",
		Blocks: []extract.Block{
			{Kind: extract.KindParagraph, Text: "Something happened."},
			{Kind: extract.KindCode, Text: "x := 1"},
		},
	}
	if err := dir.WriteJSON(artifact.Extracted, art); err != nil {
		t.Fatal(err)
	}
	s := Script{DataDir: dataDir}
	for range 2 { // re-runnable: same output
		if err := s.Run(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		got, err := dir.ReadFile(artifact.Script)
		if err != nil {
			t.Fatal(err)
		}
		want := "Big News. By Jane Doe. From The Paper.\n\nSomething happened.\n\n" + extract.CodeOmitted + "\n"
		if string(got) != want {
			t.Errorf("script = %q, want %q", got, want)
		}
	}
}

func TestScriptStageInputErrors(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	s := Script{DataDir: dataDir}
	err := s.Run(context.Background(), store.Article{ID: 1})
	if !pipeline.IsPermanent(err) {
		t.Errorf("missing extraction: err = %v, want permanent", err)
	}
	dir := artifact.For(dataDir, 2)
	if err := dir.WriteFile(artifact.Extracted, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	err = s.Run(context.Background(), store.Article{ID: 2})
	if !pipeline.IsPermanent(err) {
		t.Errorf("corrupt extraction: err = %v, want permanent", err)
	}
}

func TestSynthesizeStage(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	dir := artifact.For(dataDir, a.ID)
	writeScript(t, dir, threeParagraphs)
	engine := &fakeTTS{maxBytes: 30, fingerprint: "v1"}
	s := Synthesize{TTS: engine, Store: st, DataDir: dataDir}

	if err := s.Run(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := engine.calls(); len(got) != 3 || got[0] != "First paragraph here." {
		t.Errorf("synthesized %q, want the three paragraphs", got)
	}
	info, err := os.Stat(dir.Path(artifact.Audio))
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetArticle(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AudioBytes != info.Size() {
		t.Errorf("audio_bytes = %d, want file size %d", got.AudioBytes, info.Size())
	}
	// Three chunks plus a gap after each of the first two.
	want := 3*chunkAudio + 700*time.Millisecond
	if d := got.Duration - want; d < -100*time.Millisecond || d > 100*time.Millisecond {
		t.Errorf("duration = %s, want about %s", got.Duration, want)
	}
	if _, err := os.Stat(dir.Path(artifact.Chunks)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("chunks dir left behind: %v", err)
	}
}

// Chunks with nothing to say are skipped rather than sent to the engine.
func TestSynthesizeStageSkipsUnspeakableChunks(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	writeScript(t, artifact.For(dataDir, a.ID), "First paragraph here.\n\n* * *\n\nSecond paragraph here.")
	engine := &fakeTTS{maxBytes: 25}
	if err := (Synthesize{TTS: engine, Store: st, DataDir: dataDir}).Run(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if got := engine.calls(); len(got) != 2 || got[0] != "First paragraph here." || got[1] != "Second paragraph here." {
		t.Errorf("synthesized %q", got)
	}
}

func TestSynthesizeStageResumes(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	dir := artifact.For(dataDir, a.ID)
	writeScript(t, dir, threeParagraphs)
	engine := &fakeTTS{maxBytes: 30, fingerprint: "v1", failAt: "Third", err: errors.New("engine down")}
	s := Synthesize{TTS: engine, Store: st, DataDir: dataDir}

	err := s.Run(context.Background(), a)
	if err == nil || pipeline.IsPermanent(err) {
		t.Fatalf("err = %v, want a transient error", err)
	}
	if _, err := os.Stat(dir.Path(artifact.Audio)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("audio written despite a failed chunk: %v", err)
	}
	entries, err := os.ReadDir(dir.Path(artifact.Chunks))
	if err != nil || len(entries) != 2 {
		t.Fatalf("chunks after failure = %v (%v), want the 2 that succeeded", entries, err)
	}

	engine.failAt = ""
	before := len(engine.calls())
	if err := s.Run(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if again := engine.calls()[before:]; len(again) != 1 || again[0] != "Third paragraph here." {
		t.Errorf("retry synthesized %q, want only the missing chunk", again)
	}
}

// A cached chunk ffmpeg can't decode is discarded, so the retry recovers.
func TestSynthesizeStageCorruptChunk(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	dir := artifact.For(dataDir, a.ID)
	writeScript(t, dir, threeParagraphs)
	engine := &fakeTTS{maxBytes: 30, fingerprint: "v1"}
	bad := "0000-" + chunkKey("v1", "First paragraph here.") + ".wav"
	if err := dir.Sub(artifact.Chunks).WriteFile(bad, []byte("RIFF\x00\x00\x00\x00WAVEgarbage")); err != nil {
		t.Fatal(err)
	}
	s := Synthesize{TTS: engine, Store: st, DataDir: dataDir}
	if err := s.Run(context.Background(), a); err == nil || pipeline.IsPermanent(err) {
		t.Fatalf("err = %v, want a transient join failure", err)
	}
	if err := s.Run(context.Background(), a); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// Chunks from an attempt with an older script are removed, not joined.
func TestSynthesizeStageScriptChanged(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	dir := artifact.For(dataDir, a.ID)
	failing := &fakeTTS{maxBytes: 30, failAt: "Third", err: errors.New("down")}
	s := Synthesize{TTS: failing, Store: st, DataDir: dataDir}

	writeScript(t, dir, "Old first part.\n\nOld second part.\n\nThird old part.")
	if err := s.Run(context.Background(), a); err == nil {
		t.Fatal("expected failure")
	}
	writeScript(t, dir, "New first part.\n\nThird new part.")
	if err := s.Run(context.Background(), a); err == nil {
		t.Fatal("expected failure")
	}
	entries, err := os.ReadDir(dir.Path(artifact.Chunks))
	if err != nil {
		t.Fatal(err)
	}
	want := "0000-" + chunkKey("", "New first part.") + ".wav"
	if len(entries) != 1 || entries[0].Name() != want {
		t.Errorf("chunks = %v, want only %s", entries, want)
	}
}

func TestSynthesizeStageFingerprintChange(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	dir := artifact.For(dataDir, a.ID)
	writeScript(t, dir, threeParagraphs)

	// A first attempt with one voice leaves chunks behind...
	old := &fakeTTS{maxBytes: 30, fingerprint: "voice-a", failAt: "Third", err: errors.New("down")}
	if err := (Synthesize{TTS: old, Store: st, DataDir: dataDir}).Run(context.Background(), a); err == nil {
		t.Fatal("expected failure")
	}
	// ...which a retry with another voice must not reuse.
	engine := &fakeTTS{maxBytes: 30, fingerprint: "voice-b"}
	if err := (Synthesize{TTS: engine, Store: st, DataDir: dataDir}).Run(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if n := len(engine.calls()); n != 3 {
		t.Errorf("new voice synthesized %d chunks, want all 3", n)
	}
}

func TestSynthesizeStageErrors(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	dataDir := t.TempDir()

	t.Run("missing script", func(t *testing.T) {
		s := Synthesize{TTS: &fakeTTS{maxBytes: 30}, Store: st, DataDir: dataDir}
		if err := s.Run(context.Background(), store.Article{ID: 100}); !pipeline.IsPermanent(err) {
			t.Errorf("err = %v, want permanent", err)
		}
	})
	t.Run("empty script", func(t *testing.T) {
		writeScript(t, artifact.For(dataDir, 101), " \n\n * * * \n\n...")
		s := Synthesize{TTS: &fakeTTS{maxBytes: 30}, Store: st, DataDir: dataDir}
		if err := s.Run(context.Background(), store.Article{ID: 101}); !pipeline.IsPermanent(err) {
			t.Errorf("err = %v, want permanent", err)
		}
	})
	t.Run("engine rejects text", func(t *testing.T) {
		writeScript(t, artifact.For(dataDir, 102), threeParagraphs)
		engine := &fakeTTS{maxBytes: 30, failAt: "Second", err: &tts.StatusError{Code: 422, Body: "bad input"}}
		s := Synthesize{TTS: engine, Store: st, DataDir: dataDir}
		err := s.Run(context.Background(), store.Article{ID: 102})
		if !pipeline.IsPermanent(err) || !strings.Contains(err.Error(), "chunk 2 of 3") {
			t.Errorf("err = %v, want permanent naming chunk 2 of 3", err)
		}
	})
	t.Run("engine unavailable", func(t *testing.T) {
		writeScript(t, artifact.For(dataDir, 103), threeParagraphs)
		engine := &fakeTTS{maxBytes: 30, failAt: "First", err: &tts.StatusError{Code: 503}}
		s := Synthesize{TTS: engine, Store: st, DataDir: dataDir}
		if err := s.Run(context.Background(), store.Article{ID: 103}); err == nil || pipeline.IsPermanent(err) {
			t.Errorf("err = %v, want transient", err)
		}
	})
}

func TestSynthesizeStageCancelled(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	dir := artifact.For(dataDir, a.ID)
	writeScript(t, dir, threeParagraphs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := Synthesize{TTS: &fakeTTS{maxBytes: 30}, Store: st, DataDir: dataDir}
	if err := s.Run(ctx, a); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dir.Path(artifact.Audio)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("audio written after cancel: %v", err)
	}
	if got, _ := st.GetArticle(context.Background(), a.ID); got.AudioBytes != 0 {
		t.Errorf("audio metadata saved after cancel: %d bytes", got.AudioBytes)
	}
}

func TestSynthesizeStageLostLease(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	st := openStore(t)
	a := claimAt(t, st, "https://example.com/a", store.StatusSynthesizing)
	dataDir := t.TempDir()
	writeScript(t, artifact.For(dataDir, a.ID), threeParagraphs)
	stale := a
	stale.LeaseID++
	s := Synthesize{TTS: &fakeTTS{maxBytes: 30}, Store: st, DataDir: dataDir}
	if err := s.Run(context.Background(), stale); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("err = %v, want ErrLeaseLost", err)
	}
	if got, _ := st.GetArticle(context.Background(), a.ID); got.AudioBytes != 0 {
		t.Errorf("audio metadata saved under a lost lease: %d bytes", got.AudioBytes)
	}
}

package stages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/tony-montemuro/phonetic/internal/artifact"
	"github.com/tony-montemuro/phonetic/internal/audio"
	"github.com/tony-montemuro/phonetic/internal/extract"
	"github.com/tony-montemuro/phonetic/internal/pipeline"
	"github.com/tony-montemuro/phonetic/internal/store"
	"github.com/tony-montemuro/phonetic/internal/tts"
)

// Script writes script.txt, the text to speak: a short spoken intro (title,
// author, site) followed by the extracted article as SpokenText renders it.
// This is the passthrough version; Phase 6 replaces it with an LLM rewrite
// for listening.
type Script struct {
	DataDir string
}

// Run implements pipeline.Stage.
func (s Script) Run(ctx context.Context, a store.Article) error {
	dir := artifact.For(s.DataDir, a.ID)
	var art extract.Article
	if err := dir.ReadJSON(artifact.Extracted, &art); err != nil {
		return inputErr(err, "extraction output", "re-extracted")
	}
	text := strings.TrimSpace(spokenIntro(art) + "\n\n" + art.SpokenText())
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dir.WriteFile(artifact.Script, []byte(text+"\n")); err != nil {
		return fmt.Errorf("saving script: %w", err)
	}
	return nil
}

// bylineSaysBy matches bylines that already introduce the author ("by Jane",
// "By: Jane", "Written by Jane").
var bylineSaysBy = regexp.MustCompile(`(?i)(^|\s)by(:|\s)`)

// spokenIntro introduces the article the way a narrator would: "Title. By
// Author. From Site." Parts that are missing are left out; a site name the
// title already ends with ("Title - Site") is dropped from the title.
func spokenIntro(art extract.Article) string {
	title := art.Title
	if site := art.SiteName; site != "" {
		for _, sep := range []string{" - ", " | ", " \u2014 ", " \u2013 ", ": "} {
			if t, ok := strings.CutSuffix(title, sep+site); ok {
				title = t
				break
			}
		}
	}
	var parts []string
	if title != "" {
		parts = append(parts, sentence(title))
	}
	if by := art.Byline; by != "" {
		if !bylineSaysBy.MatchString(by) {
			by = "By " + by
		}
		parts = append(parts, sentence(by))
	}
	site := strings.ToLower(art.SiteName)
	if site != "" && !strings.Contains(strings.ToLower(art.Byline), site) && strings.ToLower(title) != site {
		parts = append(parts, sentence("From "+art.SiteName))
	}
	return strings.Join(parts, " ")
}

// sentence ends s with a period unless it already ends in punctuation.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}

// AudioStore records synthesis results (*store.Store implements it).
type AudioStore interface {
	SaveAudio(ctx context.Context, l store.Lease, bytes int64, duration time.Duration, now time.Time) error
}

// Synthesize turns script.txt into audio.mp3: it splits the script into
// chunks the engine accepts, synthesizes each into chunks/, joins them with
// ffmpeg, and records the episode's size and length.
//
// Chunk files are named by a hash of their text and the engine settings, so
// a retry (after a crash, a lost lease or a failed chunk) reuses the chunks
// already made, and changing the voice re-synthesizes them.
type Synthesize struct {
	TTS     tts.Synthesizer
	Audio   audio.Tools
	Store   AudioStore
	DataDir string
}

// Run implements pipeline.Stage.
func (s Synthesize) Run(ctx context.Context, a store.Article) error {
	dir := artifact.For(s.DataDir, a.ID)
	script, err := dir.ReadFile(artifact.Script)
	if err != nil {
		return inputErr(err, "script", "re-scripted")
	}
	chunks := slices.DeleteFunc(audio.Chunk(string(script), s.TTS.MaxChunkBytes()), unspeakable)
	if len(chunks) == 0 {
		return pipeline.Permanent(errors.New("script is empty"))
	}

	chunkDir := dir.Sub(artifact.Chunks)
	files := make([]string, len(chunks))
	names := make(map[string]bool, len(chunks))
	for i, text := range chunks {
		name := fmt.Sprintf("%04d-%s.%s", i, chunkKey(s.TTS.Fingerprint(), text), s.TTS.FileExt())
		files[i] = chunkDir.Path(name)
		names[name] = true
	}
	// Chunks left by an attempt with another script or voice are useless.
	if entries, err := os.ReadDir(dir.Path(artifact.Chunks)); err == nil {
		for _, e := range entries {
			if !names[e.Name()] {
				_ = os.RemoveAll(chunkDir.Path(e.Name()))
			}
		}
	}
	for i, text := range chunks {
		if _, err := os.Stat(files[i]); err == nil {
			continue // made by an earlier attempt
		}
		err := chunkDir.WriteStream(filepath.Base(files[i]), func(w io.Writer) error {
			return s.TTS.Synthesize(ctx, text, w)
		})
		if err != nil {
			err = fmt.Errorf("synthesizing chunk %d of %d: %w", i+1, len(chunks), err)
			if tts.IsPermanent(err) {
				return pipeline.Permanent(err)
			}
			return err
		}
	}

	// Don't write once cancelled (as in Fetch): ffmpeg would only be killed.
	if err := ctx.Err(); err != nil {
		return err
	}
	// Measure the file this worker made, before it's published (another
	// worker's could replace audio.mp3 right after).
	var size int64
	var duration time.Duration
	err = dir.WriteVia(artifact.Audio, func(tmp string) error {
		if err := s.Audio.Concat(ctx, files, tmp); err != nil {
			return err
		}
		info, err := os.Stat(tmp)
		if err != nil {
			return err
		}
		size = info.Size()
		duration, err = s.Audio.Duration(ctx, tmp)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			// A cached chunk may be what ffmpeg can't decode; without this,
			// every retry would reuse it and fail the same way.
			_ = chunkDir.RemoveAll()
		}
		return fmt.Errorf("joining audio: %w", err)
	}
	if size <= 0 || duration < time.Millisecond {
		return pipeline.Permanent(fmt.Errorf("ffmpeg produced no audio (%d bytes, %s)", size, duration))
	}
	if err := s.Store.SaveAudio(ctx, store.LeaseOf(a), size, duration, time.Now()); err != nil {
		return fmt.Errorf("saving audio metadata: %w", err)
	}
	// The chunks are only an intermediate. Removing them before the runner
	// records completion means a crash in between re-synthesizes everything;
	// rare enough to accept. A failed removal is harmless.
	_ = chunkDir.RemoveAll()
	return nil
}

// unspeakable reports whether a chunk has nothing to say ("* * *", "...").
// Engines reject such input (Kokoro with a 400), which would fail the
// article. Symbols packed into a chunk with real text are left alone.
func unspeakable(text string) bool {
	return !strings.ContainsFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) })
}

// chunkKey identifies a chunk's audio by its text and the engine settings.
func chunkKey(fingerprint, text string) string {
	sum := sha256.Sum256([]byte(fingerprint + "\x00" + text))
	return hex.EncodeToString(sum[:8])
}

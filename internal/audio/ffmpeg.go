package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Output encoding: mono MP3 at a speech-friendly bitrate, loudness-normalized
// to -16 LUFS (the usual podcast target) so episodes play at a consistent
// volume. 64 kb/s mono suits synthesized speech; a private feed isn't
// subject to Apple's directory guidance (128 kb/s).
const (
	sampleRate = 24000 // Kokoro's native rate
	bitrate    = "64k"
	loudnorm   = "loudnorm=I=-16:TP=-1.5:LRA=11"
	// gapSeconds of silence is added between chunks: each chunk is
	// synthesized separately and engines trim silence, so without it a
	// paragraph break that falls on a chunk boundary would get no pause.
	gapSeconds = "0.35"
)

// maxStderr bounds how much tool output is kept for error messages.
const maxStderr = 4 << 10

// Tools runs ffmpeg and ffprobe.
type Tools struct {
	FFmpeg  string // path or name on $PATH; default "ffmpeg"
	FFprobe string // default "ffprobe"
}

// Check verifies both tools can be found, so a missing install fails at
// startup rather than on the first article.
func (t Tools) Check() error {
	for _, name := range []string{t.ffmpeg(), t.ffprobe()} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("audio: %w (install ffmpeg)", err)
		}
	}
	return nil
}

// Concat joins the input audio files, in order, into a single MP3 at out,
// with a short pause between them. Every input is resampled to a common
// format first, so inputs may differ (the concat demuxer would silently
// corrupt mismatched inputs). On failure or cancellation, out is removed;
// callers wanting atomicity pass a temporary path and rename.
//
// ffmpeg holds every input open and buffers decoded audio for inputs it
// hasn't reached, so its memory grows with the total length (about 300 MB
// for 45 minutes). Fine for articles; a book would want the streaming
// concat demuxer with inputs normalized up front.
func (t Tools) Concat(ctx context.Context, inputs []string, out string) error {
	if len(inputs) == 0 {
		return errors.New("audio: nothing to concatenate")
	}
	// Absolute paths, so ffmpeg can't read "name:rest" as a protocol.
	out, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	var graph strings.Builder
	for i, in := range inputs {
		in, err := filepath.Abs(in)
		if err != nil {
			return err
		}
		args = append(args, "-i", in)
		fmt.Fprintf(&graph, "[%d:a]aresample=%d,aformat=sample_fmts=s16:channel_layouts=mono", i, sampleRate)
		if i < len(inputs)-1 {
			graph.WriteString(",apad=pad_dur=" + gapSeconds)
		}
		fmt.Fprintf(&graph, "[a%d];", i)
	}
	for i := range inputs {
		fmt.Fprintf(&graph, "[a%d]", i)
	}
	fmt.Fprintf(&graph, "concat=n=%d:v=0:a=1,%s[out]", len(inputs), loudnorm)
	args = append(args, "-filter_complex", graph.String(), "-map", "[out]",
		"-ac", "1", "-ar", strconv.Itoa(sampleRate), "-c:a", "libmp3lame", "-b:a", bitrate,
		"-id3v2_version", "3", "-f", "mp3", out)
	if err := t.run(ctx, t.ffmpeg(), nil, args...); err != nil {
		os.Remove(out) // don't leave a partial file
		return err
	}
	return nil
}

// Duration reports the length of an audio file.
func (t Tools) Duration(ctx context.Context, path string) (time.Duration, error) {
	path, err := filepath.Abs(path) // see Concat
	if err != nil {
		return 0, err
	}
	var stdout bytes.Buffer
	err = t.run(ctx, t.ffprobe(), &stdout,
		"-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	if err != nil {
		return 0, err
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(stdout.String()), 64)
	if err != nil || !(secs >= 0) || math.IsInf(secs, 0) { // rejects N/A, NaN, Inf
		return 0, fmt.Errorf("ffprobe %s: unexpected duration %q", filepath.Base(path), stdout.String())
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// run runs a tool, capturing stdout into stdout (if non-nil) and the start
// of stderr for error messages. A cancelled ctx is reported as ctx.Err().
func (t Tools) run(ctx context.Context, name string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second // bound the wait for output copying after a kill
	// Its own process group, so a signal meant for phonetic (Ctrl-C in a
	// terminal) doesn't also kill the tool mid-stage, which would count as a
	// failure; phonetic cancels ctx (and so kills the tool) as it shuts down.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (t Tools) ffmpeg() string {
	if t.FFmpeg != "" {
		return t.FFmpeg
	}
	return "ffmpeg"
}

func (t Tools) ffprobe() string {
	if t.FFprobe != "" {
		return t.FFprobe
	}
	return "ffprobe"
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// needFFmpeg skips the test when ffmpeg isn't installed.
func needFFmpeg(t *testing.T) Tools {
	t.Helper()
	tools := Tools{}
	if err := tools.Check(); err != nil {
		t.Skip(err)
	}
	return tools
}

// writeWAV writes a mono 16-bit PCM WAV with a sine tone of the given length
// at 24 kHz, like a TTS engine would return (no ffmpeg needed for test input).
func writeWAV(t *testing.T, path string, d time.Duration) {
	t.Helper()
	writeWAVRate(t, path, d, 24000)
}

func writeWAVRate(t *testing.T, path string, d time.Duration, rate int) {
	t.Helper()
	n := int(d.Seconds() * float64(rate))
	data := make([]byte, 2*n)
	for i := range n {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	var h []byte
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(36+len(data)))
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1) // PCM
	h = binary.LittleEndian.AppendUint16(h, 1) // mono
	h = binary.LittleEndian.AppendUint32(h, uint32(rate))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate*2))
	h = binary.LittleEndian.AppendUint16(h, 2)
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(len(data)))
	if err := os.WriteFile(path, append(h, data...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// probe returns ffprobe's stream fields for the first audio stream.
func probe(t *testing.T, tools Tools, path, fields string) string {
	t.Helper()
	out, err := exec.Command(tools.ffprobe(), "-v", "error", "-select_streams", "a:0", "-show_entries",
		"stream="+fields, "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func assertDuration(t *testing.T, got, want time.Duration) {
	t.Helper()
	if got < want-150*time.Millisecond || got > want+150*time.Millisecond {
		t.Errorf("duration = %s, want about %s", got, want)
	}
}

func TestConcatAndDuration(t *testing.T) {
	t.Parallel()
	tools := needFFmpeg(t)
	dir := t.TempDir()
	// A quote and a space in a path exercise argument handling.
	var inputs []string
	for i, d := range []time.Duration{700 * time.Millisecond, 1200 * time.Millisecond, 500 * time.Millisecond} {
		p := filepath.Join(dir, "chunk 'q' "+string(rune('a'+i))+".wav")
		writeWAV(t, p, d)
		inputs = append(inputs, p)
	}
	out := filepath.Join(dir, "audio.mp3")
	ctx := context.Background()
	if err := tools.Concat(ctx, inputs, out); err != nil {
		t.Fatal(err)
	}
	got, err := tools.Duration(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	assertDuration(t, got, 2400*time.Millisecond+2*350*time.Millisecond) // audio plus two gaps
	if s := probe(t, tools, out, "codec_name,sample_rate,channels,bit_rate"); s != "mp3,24000,1,64000" {
		t.Errorf("output stream = %q, want mp3,24000,1,64000", s)
	}
}

// Inputs in different formats are normalized, not silently corrupted (the
// concat demuxer would play the 22.05 kHz chunk too fast).
func TestConcatMixedFormats(t *testing.T) {
	t.Parallel()
	tools := needFFmpeg(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")
	writeWAVRate(t, a, time.Second, 24000)
	writeWAVRate(t, b, 2*time.Second, 22050)
	out := filepath.Join(dir, "audio.mp3")
	if err := tools.Concat(context.Background(), []string{a, b}, out); err != nil {
		t.Fatal(err)
	}
	got, err := tools.Duration(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	assertDuration(t, got, 3*time.Second+350*time.Millisecond)
}

// Relative paths that look like URLs ("name:rest") are still files.
func TestConcatRelativeColonPaths(t *testing.T) {
	tools := needFFmpeg(t)
	t.Chdir(t.TempDir())
	writeWAV(t, "x:y.wav", time.Second)
	if err := tools.Concat(context.Background(), []string{"x:y.wav"}, "out:1.mp3"); err != nil {
		t.Fatal(err)
	}
	got, err := tools.Duration(context.Background(), "out:1.mp3")
	if err != nil {
		t.Fatal(err)
	}
	assertDuration(t, got, time.Second)
}

// Cancelling a running ffmpeg kills it and removes its partial output.
func TestConcatCancelledMidRunRemovesOutput(t *testing.T) {
	t.Parallel()
	tools := needFFmpeg(t)
	dir := t.TempDir()
	long := filepath.Join(dir, "long.wav")
	writeWAV(t, long, 20*time.Minute)
	out := filepath.Join(dir, "audio.mp3")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := tools.Concat(ctx, []string{long}, out); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial output left behind (%v)", err)
	}
}

func TestConcatErrors(t *testing.T) {
	t.Parallel()
	tools := needFFmpeg(t)
	dir := t.TempDir()
	if err := tools.Concat(context.Background(), nil, filepath.Join(dir, "a.mp3")); err == nil {
		t.Error("empty input list accepted")
	}
	bad := filepath.Join(dir, "bad.wav")
	if err := os.WriteFile(bad, []byte("not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := tools.Concat(context.Background(), []string{bad}, filepath.Join(dir, "a.mp3"))
	if err == nil || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("err = %v, want an ffmpeg error with its output", err)
	}
	if _, err := tools.Duration(context.Background(), filepath.Join(dir, "missing.mp3")); err == nil {
		t.Error("Duration of a missing file succeeded")
	}
}

func TestConcatHonorsCancellation(t *testing.T) {
	t.Parallel()
	tools := needFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "a.wav")
	writeWAV(t, in, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tools.Concat(ctx, []string{in}, filepath.Join(dir, "a.mp3")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestCheckReportsMissingTools(t *testing.T) {
	t.Parallel()
	if err := (Tools{FFmpeg: "phonetic-no-such-ffmpeg"}).Check(); err == nil {
		t.Error("Check passed with a missing ffmpeg")
	}
}

func TestLimitedBuffer(t *testing.T) {
	t.Parallel()
	b := &limitedBuffer{max: 5}
	n, err := b.Write([]byte("abc"))
	if n != 3 || err != nil {
		t.Fatal(n, err)
	}
	b.Write([]byte("defgh"))
	if b.String() != "abcde" {
		t.Errorf("buffer = %q, want first 5 bytes", b.String())
	}
}

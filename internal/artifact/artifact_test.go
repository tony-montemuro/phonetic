package artifact

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func assertNoTempFiles(t *testing.T, d Dir) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(d.path, ".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

func TestForPath(t *testing.T) {
	t.Parallel()
	d := For("/var/lib/phonetic", 42)
	if got, want := d.Path(RawHTML), "/var/lib/phonetic/articles/42/raw.html"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestWriteAndReadFile(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 7)
	if _, err := d.ReadFile(RawHTML); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile before write: %v, want not-exist", err)
	}
	// Long then short: an overwrite that didn't truncate would be caught.
	for _, content := range []string{"first, longer content", "second"} {
		if err := d.WriteFile(RawHTML, []byte(content)); err != nil {
			t.Fatal(err)
		}
		got, err := d.ReadFile(RawHTML)
		if err != nil || string(got) != content {
			t.Fatalf("ReadFile = %q, %v; want %q", got, err, content)
		}
	}
	assertNoTempFiles(t, d)
}

func TestWriteJSONRoundTrip(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 1)
	type doc struct {
		Title string         `json:"title"`
		Words int            `json:"words"`
		Tags  map[string]int `json:"tags"`
	}
	in := doc{Title: "Hello", Words: 3, Tags: map[string]int{"b": 2, "a": 1}}
	if err := d.WriteJSON(Extracted, in); err != nil {
		t.Fatal(err)
	}
	raw, err := d.ReadFile(Extracted)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"title\": \"Hello\",\n  \"words\": 3,\n  \"tags\": {\n    \"a\": 1,\n    \"b\": 2\n  }\n}\n"
	if string(raw) != want {
		t.Errorf("WriteJSON output not indented/deterministic:\n%s", raw)
	}
	var out doc
	if err := d.ReadJSON(Extracted, &out); err != nil {
		t.Fatal(err)
	}
	if out.Title != in.Title || out.Words != in.Words || out.Tags["a"] != 1 {
		t.Errorf("round trip = %+v", out)
	}
}

func TestReadJSONReportsBadContent(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 1)
	if err := d.WriteFile(FetchMeta, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := d.ReadJSON(FetchMeta, &v); err == nil {
		t.Error("expected decode error")
	}
}

func TestWriteFailureLeavesNoTempFile(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 1)
	// Make the rename fail: the destination is a non-empty directory.
	if err := os.MkdirAll(filepath.Join(d.Path(RawHTML), "child"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFile(RawHTML, []byte("x")); err == nil {
		t.Fatal("expected rename to fail")
	}
	assertNoTempFiles(t, d)
}

// Concurrent writers to the same file never produce a torn result: the file
// always holds exactly one writer's content.
func TestConcurrentWritesAreAtomic(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 1)
	contents := map[string]bool{}
	var wg sync.WaitGroup
	for i := range 8 {
		c := string(rune('a'+i)) + "-" + string(make([]byte, 64<<10)) // 64KB each
		contents[c] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.WriteFile(RawHTML, []byte(c)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := d.ReadFile(RawHTML)
	if err != nil {
		t.Fatal(err)
	}
	if !contents[string(got)] {
		t.Errorf("file holds a mix of writes (%d bytes)", len(got))
	}
	assertNoTempFiles(t, d)
}

func TestBadNamesAreRejected(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 1)
	for _, name := range []string{"", ".", "..", "../phonetic.db", "a/b", "/etc/passwd"} {
		if err := d.WriteFile(name, []byte("x")); !errors.Is(err, ErrBadName) {
			t.Errorf("WriteFile(%q): %v, want ErrBadName", name, err)
		}
		if _, err := d.ReadFile(name); !errors.Is(err, ErrBadName) {
			t.Errorf("ReadFile(%q): %v, want ErrBadName", name, err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Path(%q) should panic", name)
				}
			}()
			d.Path(name)
		}()
	}
}

// WriteJSON is strict about UTF-8 (callers clean scraped text); a failed
// encode writes nothing.
func TestWriteJSONFailureWritesNothing(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 1)
	for name, v := range map[string]any{
		"invalid UTF-8": map[string]string{"title": "bad \xff"},
		"unencodable":   map[string]any{"c": make(chan int)},
	} {
		if err := d.WriteJSON(Extracted, v); err == nil {
			t.Errorf("%s: expected error", name)
		}
		if _, err := d.ReadFile(Extracted); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: file exists after failed write (%v)", name, err)
		}
	}
	assertNoTempFiles(t, d)
}

func TestWriteCreatesNestedDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	d := For(filepath.Join(root, "fresh-data-dir"), 9)
	if err := d.WriteFile(RawHTML, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh-data-dir", "articles", "9", RawHTML)); err != nil {
		t.Error(err)
	}
}

func TestSubAndRemoveAll(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 3)
	chunks := d.Sub(Chunks)
	if err := chunks.WriteFile("0001.wav", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if got := chunks.Path("0001.wav"); got != filepath.Join(d.path, "chunks", "0001.wav") {
		t.Errorf("Sub path = %q", got)
	}
	if err := chunks.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chunks.path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("chunks dir still exists (%v)", err)
	}
	if err := chunks.RemoveAll(); err != nil {
		t.Errorf("RemoveAll on a missing dir: %v", err)
	}
}

// WriteVia hands an external producer a temp path, then renames it into
// place; a failed producer leaves nothing behind.
func TestWriteVia(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 4)
	err := d.WriteVia(Audio, func(tmp string) error {
		if filepath.Dir(tmp) != d.path || filepath.Base(tmp) == Audio || filepath.Ext(tmp) != ".mp3" {
			t.Errorf("temp path %q should be a different .mp3 file in the same directory", tmp)
		}
		// Like ffmpeg: open the path itself (truncating) and write.
		return os.WriteFile(tmp, []byte("mp3 data"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.ReadFile(Audio); err != nil || string(got) != "mp3 data" {
		t.Errorf("Audio = %q, %v", got, err)
	}

	boom := errors.New("ffmpeg failed")
	if err := d.WriteVia("other.mp3", func(tmp string) error {
		_ = os.WriteFile(tmp, []byte("partial"), 0o644)
		return boom
	}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the producer's error", err)
	}
	if _, err := d.ReadFile("other.mp3"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("failed producer's output was renamed into place")
	}
	assertNoTempFiles(t, d)
}

// A producer that writes elsewhere and renames over the temp path (as some
// tools do) gets its file published; one that leaves no regular file fails.
func TestWriteViaProducerVariants(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 6)
	err := d.WriteVia("replaced.mp3", func(tmp string) error {
		other := tmp + ".new"
		if err := os.WriteFile(other, []byte("new data"), 0o644); err != nil {
			return err
		}
		return os.Rename(other, tmp)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := d.ReadFile("replaced.mp3"); string(got) != "new data" {
		t.Errorf("replaced content = %q", got)
	}

	for name, produce := range map[string]func(string) error{
		"removed": os.Remove,
		"directory": func(tmp string) error {
			if err := os.Remove(tmp); err != nil {
				return err
			}
			if err := os.Mkdir(tmp, 0o750); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(tmp, "x"), nil, 0o644)
		},
	} {
		if err := d.WriteVia(name+".mp3", produce); err == nil {
			t.Errorf("%s: WriteVia succeeded", name)
		}
		if _, err := os.Lstat(d.Path(name + ".mp3")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: something was published: %v", name, err)
		}
	}
	assertNoTempFiles(t, d)
}

// Writing into a subdirectory of a fresh data dir creates every level.
func TestSubOnFreshDataDir(t *testing.T) {
	t.Parallel()
	chunks := For(filepath.Join(t.TempDir(), "data"), 7).Sub(Chunks)
	if err := chunks.WriteFile("0000.wav", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got, _ := chunks.ReadFile("0000.wav"); string(got) != "x" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteStream(t *testing.T) {
	t.Parallel()
	d := For(t.TempDir(), 5)
	if err := d.WriteStream("a.wav", func(w io.Writer) error {
		_, err := io.WriteString(w, "RIFF....")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.ReadFile("a.wav"); string(got) != "RIFF...." {
		t.Errorf("content = %q", got)
	}
	boom := errors.New("connection reset")
	if err := d.WriteStream("b.wav", func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		return boom
	}); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if _, err := d.ReadFile("b.wav"); !errors.Is(err, fs.ErrNotExist) {
		t.Error("failed stream was renamed into place")
	}
	assertNoTempFiles(t, d)
}

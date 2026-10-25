package artifact

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func assertNoTempFiles(t *testing.T, d Dir) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(d.path, ".*.tmp-*"))
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

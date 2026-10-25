// Package artifact manages the per-article files the pipeline stages produce
// under <data_dir>/articles/<id>/.
//
// Files are written atomically and durably: temp file in the same directory,
// fsync, rename, then fsync of the directory (and of parents it had to
// create). Readers never see a half-written file, and once a write returns,
// it survives a crash, so a stage that records completion in the database
// after writing can rely on its outputs being there.
//
// Atomic writes prevent torn files, not stale ones: a worker that lost its
// lease can still rename its (complete) output over the new owner's. That's
// harmless because stages are idempotent and re-runnable. A crash between
// creating a temp file and renaming it leaves a hidden ".tmp-*-<name>" file
// behind; it is never read and goes away when the article's directory is
// removed.
package artifact

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Names of the files stages read and write.
const (
	RawHTML   = "raw.html"       // fetch output, converted to UTF-8
	FetchMeta = "fetch.json"     // fetch metadata (final URL, content type, time)
	Extracted = "extracted.json" // extract output
	Script    = "script.txt"     // text to speak (script stage output)
	Audio     = "audio.mp3"      // the episode (synthesize stage output)
	Chunks    = "chunks"         // subdirectory of per-chunk TTS audio (intermediate)
)

// ErrBadName is returned for file names that aren't a single plain path
// element.
var ErrBadName = errors.New("artifact: name must be a single file name")

// Dir is one article's artifact directory.
type Dir struct {
	path string
}

// For returns the artifact directory for article id under dataDir. It does
// not touch the filesystem.
func For(dataDir string, id int64) Dir {
	return Dir{path: filepath.Join(dataDir, "articles", strconv.FormatInt(id, 10))}
}

// Path returns the full path of name inside the directory. It panics if name
// isn't a single file name; names are compile-time constants, so that's a
// programming error.
func (d Dir) Path(name string) string {
	if err := checkName(name); err != nil {
		panic(err)
	}
	return filepath.Join(d.path, name)
}

// Sub returns the subdirectory name of d (not created until written to). It
// panics if name isn't a single file name, like Path.
func (d Dir) Sub(name string) Dir {
	return Dir{path: d.Path(name)}
}

// WriteFile atomically and durably writes data to name, creating the
// directory if needed.
func (d Dir) WriteFile(name string, data []byte) error {
	return d.writeAtomic(name, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// WriteStream atomically and durably writes name from a function that
// streams into it (e.g. an HTTP response body). If write fails, nothing is
// renamed into place.
func (d Dir) WriteStream(name string, write func(io.Writer) error) error {
	return d.writeAtomic(name, func(f *os.File) error { return write(f) })
}

// WriteVia atomically and durably produces name with an external tool:
// produce is given a temporary path in the same directory to write to (e.g.
// as ffmpeg's output file). The path already exists (empty), so tools must be
// told to overwrite it; it keeps name's extension. Whatever regular file is
// at the path when produce returns is synced and renamed into place, so
// produce may also replace it. If produce fails, the path is removed.
func (d Dir) WriteVia(name string, produce func(tmpPath string) error) error {
	return d.writeAtomic(name, func(f *os.File) error {
		// Only a name reservation: the tool writes through its own handle.
		if err := f.Close(); err != nil {
			return err
		}
		return produce(f.Name())
	})
}

// RemoveAll removes the directory and everything in it. A missing directory
// is not an error. Removal isn't made durable (no directory sync): it's for
// intermediates, which a crash may bring back.
func (d Dir) RemoveAll() error {
	return os.RemoveAll(d.path)
}

// ReadFile reads name.
func (d Dir) ReadFile(name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(d.path, name))
}

// WriteJSON atomically writes v as indented JSON to name. Strings must be
// valid UTF-8 (encoding/json/v2's default); callers storing scraped text must
// clean it first.
func (d Dir) WriteJSON(name string, v any) error {
	data, err := json.Marshal(v, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}
	return d.WriteFile(name, append(data, '\n'))
}

// ReadJSON decodes name into v.
func (d Dir) ReadJSON(name string, v any) error {
	data, err := d.ReadFile(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decoding %s: %w", name, err)
	}
	return nil
}

// writeAtomic creates a temp file next to name, lets fill write it (and
// possibly close it), then syncs whatever regular file is at the temp path,
// renames it into place and syncs the directory. It's the shared core for
// every writer.
func (d Dir) writeAtomic(name string, fill func(*os.File) error) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := d.ensure(); err != nil {
		return err
	}
	// The random part goes first so the temp file keeps name's extension
	// (tools such as ffmpeg pick formats by extension).
	tmp, err := os.CreateTemp(d.path, ".tmp-*-"+name)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.RemoveAll(tmp.Name()) // a tool may have left a directory
		}
	}()
	if err := fill(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil && !errors.Is(err, os.ErrClosed) { // WriteVia closed it
		return err
	}
	// Sync through a fresh handle: fill may have replaced the file.
	if err := syncFile(tmp.Name()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(d.path, name)); err != nil {
		return err
	}
	ok = true
	return syncDir(d.path)
}

// ensure creates the directory if needed. When it creates directories, it
// syncs their parents so the new entries are durable too.
func (d Dir) ensure() error {
	if _, err := os.Stat(d.path); err == nil {
		return nil
	}
	// The deepest existing ancestor is the last directory to sync.
	existing := filepath.Dir(d.path)
	for {
		if _, err := os.Stat(existing); err == nil || existing == filepath.Dir(existing) {
			break
		}
		existing = filepath.Dir(existing)
	}
	if err := os.MkdirAll(d.path, 0o750); err != nil {
		return err
	}
	for p := d.path; p != existing; p = filepath.Dir(p) {
		if err := syncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}

func checkName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return nil
}

// syncFile fsyncs the regular file at path.
func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact: %s is not a regular file", filepath.Base(path))
	}
	return f.Sync()
}

// syncDir fsyncs a directory so entries created or renamed in it are durable.
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

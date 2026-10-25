// Package extracttest stores and loads extraction fixtures: a captured page
// plus the extraction it is expected to produce (a golden file). The capture
// tool (cmd/phonetic-extract -save) writes them; the extract tests replay
// them.
package extracttest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tony-montemuro/phonetic/internal/extract"
)

// File names inside a fixture directory.
const (
	PageFile     = "page.html"     // the fetched page, as UTF-8
	MetaFile     = "meta.json"     // Meta
	ExpectedFile = "expected.json" // the golden extract.Article
)

// Meta describes where a fixture came from.
type Meta struct {
	SourceURL  string    `json:"source_url"`
	FinalURL   string    `json:"final_url"` // after redirects; extraction resolves links against it
	License    string    `json:"license,omitzero"`
	CapturedAt time.Time `json:"captured_at"`
}

// Fixture is one captured page.
type Fixture struct {
	Name string // directory name
	Dir  string
	Meta Meta
	Page []byte
}

// Load reads every fixture directory directly under root, in name order,
// skipping names starting with "." or "_". A missing root yields no fixtures
// (local fixture directories are optional).
func Load(root string) ([]Fixture, error) {
	entries, err := os.ReadDir(root) // sorted by name
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Fixture
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		f := Fixture{Name: e.Name(), Dir: dir}
		if f.Page, err = os.ReadFile(filepath.Join(dir, PageFile)); err != nil {
			return nil, fmt.Errorf("fixture %s: %w", dir, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, MetaFile))
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", dir, err)
		}
		if err := json.Unmarshal(data, &f.Meta); err != nil {
			return nil, fmt.Errorf("fixture %s: %s: %w", dir, MetaFile, err)
		}
		out = append(out, f)
	}
	return out, nil
}

// Save writes a fixture into dir, creating it.
func Save(dir string, page []byte, meta Meta, expected extract.Article) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, PageFile), page, 0o644); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, MetaFile), meta); err != nil {
		return err
	}
	return WriteExpected(dir, expected)
}

// WriteExpected (re)writes a fixture's golden file.
func WriteExpected(dir string, a extract.Article) error {
	return writeJSON(filepath.Join(dir, ExpectedFile), a)
}

// ReadExpected reads a fixture's golden file.
func ReadExpected(dir string) (extract.Article, error) {
	var a extract.Article
	data, err := os.ReadFile(filepath.Join(dir, ExpectedFile))
	if err != nil {
		return a, err
	}
	// Reject unknown members: a field removed from Article must not linger
	// unnoticed in golden files.
	err = json.Unmarshal(data, &a, json.RejectUnknownMembers(true))
	return a, err
}

// Marshal renders v the way fixture files store it.
func Marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v, jsontext.WithIndent("  "), json.Deterministic(true))
	return append(data, '\n'), err
}

func writeJSON(path string, v any) error {
	data, err := Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

package extracttest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tony-montemuro/phonetic/internal/extract"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	meta := Meta{SourceURL: "https://example.com/a", FinalURL: "https://example.com/a/", License: "CC0",
		CapturedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	art := extract.Article{Title: "T", Blocks: []extract.Block{{Kind: extract.KindParagraph, Text: "hello"}}, Images: []extract.Image{}}
	if err := Save(filepath.Join(root, "b"), []byte("<p>b</p>"), meta, art); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(root, "a"), []byte("<p>a</p>"), meta, art); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("not a fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".scratch"), 0o755); err != nil { // skipped
		t.Fatal(err)
	}

	fixtures, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 2 || fixtures[0].Name != "a" || string(fixtures[1].Page) != "<p>b</p>" {
		t.Fatalf("fixtures = %+v", fixtures)
	}
	if fixtures[0].Meta != meta {
		t.Errorf("meta = %+v, want %+v", fixtures[0].Meta, meta)
	}
	got, err := ReadExpected(fixtures[0].Dir)
	if err != nil || got.Title != "T" || len(got.Blocks) != 1 {
		t.Errorf("ReadExpected = %+v, %v", got, err)
	}
}

func TestLoadMissingRootIsEmpty(t *testing.T) {
	fixtures, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || fixtures != nil {
		t.Errorf("Load(missing) = %v, %v; want nil, nil", fixtures, err)
	}
}

func TestLoadRejectsIncompleteFixture(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Error("expected an error for a fixture without page.html")
	}
}

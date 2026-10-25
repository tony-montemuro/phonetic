package extract_test

import (
	"flag"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/tony-montemuro/phonetic/internal/extract"
	"github.com/tony-montemuro/phonetic/internal/extract/extracttest"
)

// Run as: go test ./internal/extract -run TestFixtures -update
// (only this package defines the flag, so don't pass it to ./...).
var update = flag.Bool("update", false, "rewrite the expected.json golden files from current output")

// Fixture roots: committed fixtures from openly licensed sources, and an
// optional gitignored directory for pages from sites you actually read.
const (
	committedRoot = "testdata/articles"
	localRoot     = "testdata/articles-local"
)

// firstLineDiff reports the first differing line of two renderings, which
// pinpoints any changed field (including a long paragraph's tail or a Source).
func firstLineDiff(got, want []byte) string {
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := range min(len(g), len(w)) {
		if g[i] != w[i] {
			return "line " + strconv.Itoa(i+1) + " of " + extracttest.ExpectedFile + ":\n  got:  " + g[i] + "\n  want: " + w[i]
		}
	}
	return "length differs: got " + strconv.Itoa(len(g)) + " lines, want " + strconv.Itoa(len(w))
}

// checkInvariants asserts properties every real extraction must have.
func checkInvariants(t *testing.T, a extract.Article) {
	t.Helper()
	if a.Title == "" || len(a.Blocks) == 0 {
		t.Errorf("implausible extraction: title %q, %d blocks", a.Title, len(a.Blocks))
	}
	for i, b := range a.Blocks {
		if strings.TrimSpace(b.Text) == "" {
			t.Errorf("block %d (%s) is empty", i, b.Kind)
		}
	}
	for i, img := range a.Images {
		u, err := url.Parse(img.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			t.Errorf("image %d URL %q is not absolute http(s)", i, img.URL)
		}
		if img.Block < 0 || img.Block > len(a.Blocks) {
			t.Errorf("image %d Block %d out of range [0, %d]", i, img.Block, len(a.Blocks))
		}
	}
}

// TestFixtures replays captured pages through Extract and compares against
// their golden files. Comparison is structural (immune to JSON formatting
// changes in the toolchain); the diff message points at the first changed
// line of the rendered golden file.
func TestFixtures(t *testing.T) {
	ran := 0
	for _, root := range []string{committedRoot, localRoot} {
		fixtures, err := extracttest.Load(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fixtures {
			ran++
			t.Run(filepath.Base(root)+"/"+f.Name, func(t *testing.T) {
				t.Parallel()
				if root == committedRoot && f.Meta.License == "" {
					t.Errorf("committed fixture has no license in %s (capture with -license)", extracttest.MetaFile)
				}
				pageURL, err := url.Parse(f.Meta.FinalURL)
				if err != nil {
					t.Fatal(err)
				}
				got, err := extract.Extract(f.Page, pageURL)
				if err != nil {
					t.Fatalf("Extract: %v", err)
				}
				checkInvariants(t, got)

				if *update {
					if err := extracttest.WriteExpected(f.Dir, got); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := extracttest.ReadExpected(f.Dir)
				if err != nil {
					t.Fatalf("%v (run with -update to create it)", err)
				}
				if !reflect.DeepEqual(got, want) {
					gotJSON, err1 := extracttest.Marshal(got)
					wantJSON, err2 := extracttest.Marshal(want)
					if err1 != nil || err2 != nil {
						t.Fatalf("rendering for diff: %v / %v", err1, err2)
					}
					t.Errorf("extraction changed; %s", firstLineDiff(gotJSON, wantJSON))
				}
			})
		}
	}
	if ran == 0 {
		t.Fatal("no fixtures found")
	}
}

package feed

import (
	"bytes"
	"encoding/xml"
	"flag"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testChannel = Channel{
	Title:       "phonetic",
	Description: "Articles, read aloud.",
	Link:        "https://pc.example.ts.net",
	FeedURL:     "https://pc.example.ts.net/feed.xml",
	ImageURL:    "https://pc.example.ts.net/artwork.png",
	Author:      "phonetic",
	Language:    "en",
}

var testEpisodes = []Episode{
	{
		GUID:        "phonetic-article-2",
		Title:       "Q&A: <Generics> in Go",
		Author:      "Jane Doe",
		Link:        "https://example.com/generics?a=1&b=2",
		Description: `<p>From <a href="https://example.com/generics?a=1&amp;b=2">Example</a> · 1,234 words</p>`,
		PubDate:     time.Date(2026, 11, 1, 9, 30, 0, 0, time.FixedZone("CST", -6*3600)),
		AudioURL:    "https://pc.example.ts.net/audio/2.mp3",
		AudioBytes:  3133484,
		Duration:    391603 * time.Millisecond,
	},
	{
		GUID:       "phonetic-article-1",
		Title:      "Fixing For Loops",
		PubDate:    time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC),
		AudioURL:   "https://pc.example.ts.net/audio/1.mp3",
		AudioBytes: 1000,
		Duration:   1499 * time.Millisecond,
	},
}

// The feed matches the reviewed golden file (go test ./internal/feed -update
// rewrites it).
func TestRenderGolden(t *testing.T) {
	t.Parallel()
	got, err := Render(testChannel, testEpisodes)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "feed.xml")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("feed differs from %s (run with -update after reviewing):\n%s", path, got)
	}
}

// Parsing the feed back recovers the values, whatever characters they hold.
func TestRenderRoundTrip(t *testing.T) {
	t.Parallel()
	out, err := Render(testChannel, testEpisodes)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Channel struct {
			Title         string `xml:"title"`
			LastBuildDate string `xml:"lastBuildDate"`
			Items         []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				Description string `xml:"description"`
				GUID        string `xml:"guid"`
				PubDate     string `xml:"pubDate"`
				Duration    string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
				Enclosure   struct {
					URL    string `xml:"url,attr"`
					Length int64  `xml:"length,attr"`
					Type   string `xml:"type,attr"`
				} `xml:"enclosure"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("feed is not well-formed: %v", err)
	}
	items := doc.Channel.Items
	if len(items) != 2 {
		t.Fatalf("%d items, want 2", len(items))
	}
	first, src := items[0], testEpisodes[0]
	if first.Title != src.Title || first.Link != src.Link || first.Description != src.Description || first.GUID != src.GUID {
		t.Errorf("item = %+v, want the episode's values back", first)
	}
	if first.PubDate != "Sun, 01 Nov 2026 15:30:00 +0000" || doc.Channel.LastBuildDate != first.PubDate {
		t.Errorf("pubDate = %q, lastBuildDate = %q; want UTC RFC 822 of the newest", first.PubDate, doc.Channel.LastBuildDate)
	}
	if first.Duration != "392" || items[1].Duration != "1" {
		t.Errorf("durations = %q, %q; want whole seconds, rounded", first.Duration, items[1].Duration)
	}
	if first.Enclosure.URL != src.AudioURL || first.Enclosure.Length != src.AudioBytes || first.Enclosure.Type != "audio/mpeg" {
		t.Errorf("enclosure = %+v", first.Enclosure)
	}
}

// The newest episode sets lastBuildDate wherever it is in the list.
func TestRenderLastBuildDateIsNewest(t *testing.T) {
	t.Parallel()
	eps := []Episode{testEpisodes[1], testEpisodes[0]}
	out, err := Render(testChannel, eps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "<lastBuildDate>Sun, 01 Nov 2026 15:30:00 +0000</lastBuildDate>") {
		t.Errorf("lastBuildDate not the newest episode's date:\n%s", out)
	}
}

func TestRenderRequiredFields(t *testing.T) {
	t.Parallel()
	noLang := testChannel
	noLang.Language = ""
	if _, err := Render(noLang, nil); err == nil {
		t.Error("channel without a language accepted")
	}
	for name, mutate := range map[string]func(*Episode){
		"guid":  func(e *Episode) { e.GUID = "" },
		"audio": func(e *Episode) { e.AudioURL = "" },
		"date":  func(e *Episode) { e.PubDate = time.Time{} },
	} {
		e := testEpisodes[0]
		mutate(&e)
		if _, err := Render(testChannel, []Episode{e}); err == nil {
			t.Errorf("episode without %s accepted", name)
		}
	}
}

// Scraped text may hold characters XML can't: the feed stays well-formed.
func TestRenderInvalidCharacters(t *testing.T) {
	t.Parallel()
	e := testEpisodes[0]
	e.Title = "nul\x00 vt\x0b bad\xff end"
	e.Author = "\x1f"
	out, err := Render(testChannel, []Episode{e})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Title string `xml:"channel>item>title"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("feed not well-formed: %v", err)
	}
	if want := "nul\uFFFD vt\uFFFD bad\uFFFD end"; doc.Title != want {
		t.Errorf("title = %q, want %q", doc.Title, want)
	}
}

func TestRenderEmpty(t *testing.T) {
	t.Parallel()
	out, err := Render(testChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "<item>") || strings.Contains(string(out), "lastBuildDate") {
		t.Errorf("empty feed has items or a build date:\n%s", out)
	}
	if err := xml.Unmarshal(out, new(struct{})); err != nil {
		t.Errorf("empty feed is not well-formed: %v", err)
	}
}

func TestArtwork(t *testing.T) {
	t.Parallel()
	data := Artwork()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != ArtworkSize || b.Dy() != ArtworkSize {
		t.Errorf("artwork is %v, want %d square", b, ArtworkSize)
	}
	if !bytes.Equal(Artwork(), data) {
		t.Error("artwork not stable")
	}
}

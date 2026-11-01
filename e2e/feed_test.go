//go:build e2e

package e2e

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// rssFeed mirrors the parts of the feed the tests check.
type rssFeed struct {
	Channel struct {
		Title string `xml:"title"`
		Image struct {
			URL string `xml:"url"`
		} `xml:"image"`
		Items []struct {
			Title     string `xml:"title"`
			Link      string `xml:"link"`
			GUID      string `xml:"guid"`
			Duration  string `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
			Enclosure struct {
				URL    string `xml:"url,attr"`
				Length int64  `xml:"length,attr"`
				Type   string `xml:"type,attr"`
			} `xml:"enclosure"`
		} `xml:"item"`
	} `xml:"channel"`
}

// feedGet sends GET url with the feed's credentials (if auth) and extra
// headers, returning the response with its body read.
func feedGet(t testing.TB, inst *instance, url string, auth bool, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.SetBasicAuth(feedUser, feedPassword)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := inst.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// readFeed fetches and parses the feed.
func readFeed(t testing.TB, inst *instance) (rssFeed, *http.Response) {
	t.Helper()
	resp, body := feedGet(t, inst, inst.BaseURL+"/feed.xml", true, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("feed: status %d", resp.StatusCode)
	}
	var f rssFeed
	if err := xml.Unmarshal(body, &f); err != nil {
		t.Fatalf("feed: %v\n%s", err, body)
	}
	return f, resp
}

// The whole path a listener sees: a submitted article becomes an episode in
// the feed whose enclosure downloads as playable MP3 audio.
func TestEpisodeAppearsInFeedAndPlays(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	inst := start(t, options{})

	f, resp := readFeed(t, inst)
	if len(f.Channel.Items) != 0 || f.Channel.Title != "phonetic" {
		t.Errorf("empty feed = %+v", f.Channel)
	}
	emptyTag := resp.Header.Get("ETag")

	a := submit(t, inst, web.URL("/article/episode"))
	got := waitStatus(t, inst, a.ID, "ready", 10*time.Second)

	f, resp = readFeed(t, inst)
	if len(f.Channel.Items) != 1 {
		t.Fatalf("%d episodes, want 1", len(f.Channel.Items))
	}
	ep := f.Channel.Items[0]
	wantURL := fmt.Sprintf("%s/audio/%d.mp3", inst.BaseURL, a.ID)
	if ep.Title != articleTitle("episode") || ep.Link != web.URL("/article/episode") || ep.GUID != fmt.Sprintf("phonetic-article-%d", a.ID) {
		t.Errorf("episode = %+v", ep)
	}
	if ep.Enclosure.URL != wantURL || ep.Enclosure.Length != got.AudioBytes || ep.Enclosure.Type != "audio/mpeg" {
		t.Errorf("enclosure = %+v, want %s with %d bytes", ep.Enclosure, wantURL, got.AudioBytes)
	}
	if want := strconv.Itoa(max(1, int(math.Round(float64(got.DurationMS)/1000)))); ep.Duration != want {
		t.Errorf("itunes:duration = %q, want %s", ep.Duration, want)
	}
	if tag := resp.Header.Get("ETag"); tag == "" || tag == emptyTag {
		t.Errorf("ETag %q didn't change when an episode appeared", tag)
	}

	// Unchanged feed: revalidation says so.
	resp, _ = feedGet(t, inst, inst.BaseURL+"/feed.xml", true, map[string]string{"If-None-Match": resp.Header.Get("ETag")})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: status %d, want 304", resp.StatusCode)
	}

	// The enclosure is the real episode: MP3 of the recorded length.
	resp, audio := feedGet(t, inst, ep.Enclosure.URL, true, nil)
	if resp.StatusCode != http.StatusOK || int64(len(audio)) != ep.Enclosure.Length {
		t.Fatalf("download: status %d, %d bytes, want %d", resp.StatusCode, len(audio), ep.Enclosure.Length)
	}
	path := filepath.Join(t.TempDir(), "episode.mp3")
	if err := os.WriteFile(path, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name:format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != "mp3" {
		t.Fatalf("ffprobe: %q, want an mp3 stream and a duration", out)
	}
	secs, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || math.Abs(secs*1000-float64(got.DurationMS)) > 50 {
		t.Errorf("ffprobe duration %s, want %dms (as recorded)", fields[1], got.DurationMS)
	}

	// The artwork the feed points at is served (without credentials).
	if resp, _ := feedGet(t, inst, f.Channel.Image.URL, false, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("artwork: status %d", resp.StatusCode)
	}
}

func TestFeedAndAudioRequireCredentials(t *testing.T) {
	t.Parallel()
	inst := start(t, options{})
	for _, path := range []string{"/feed.xml", "/audio/1.mp3"} {
		resp, _ := feedGet(t, inst, inst.BaseURL+path, false, nil)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s without credentials: %d %q, want a 401 challenge", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
}

// Links are built from PHONETIC_PUBLIC_URL (e.g. a tailscale serve or
// WireGuard address), not from the address phonetic listens on.
func TestFeedLinksUsePublicURL(t *testing.T) {
	t.Parallel()
	inst := start(t, options{Env: map[string]string{"PHONETIC_PUBLIC_URL": "https://pc.example.ts.net/"}})
	f, _ := readFeed(t, inst)
	if f.Channel.Image.URL != "https://pc.example.ts.net/artwork.png" {
		t.Errorf("artwork URL = %q", f.Channel.Image.URL)
	}
}

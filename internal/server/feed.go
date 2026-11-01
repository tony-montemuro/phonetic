package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tony-montemuro/phonetic/internal/artifact"
	"github.com/tony-montemuro/phonetic/internal/feed"
	"github.com/tony-montemuro/phonetic/internal/store"
)

// FeedStore is the persistence the feed needs (*store.Store implements it).
type FeedStore interface {
	ListReady(ctx context.Context, limit int) ([]store.Article, error)
	GetArticle(ctx context.Context, id int64) (store.Article, error)
}

// maxEpisodes bounds the feed (until retention trims old episodes).
const maxEpisodes = 500

// handleFeed serves the podcast feed. Each response carries an ETag of its
// body, so apps polling with If-None-Match get a 304 until something changes.
func handleFeed(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		articles, err := d.Feed.ListReady(r.Context(), maxEpisodes)
		if err != nil {
			d.Logger.Error("listing episodes", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		var episodes []feed.Episode
		for _, a := range articles {
			// Skip articles without audio (ready before Phase 3 made it), so
			// one odd row can't break the whole feed.
			if a.AudioBytes > 0 && !a.ReadyAt.IsZero() {
				episodes = append(episodes, episodeOf(a, d.PublicURL))
			}
		}
		body, err := feed.Render(feed.Channel{
			Title:       "phonetic",
			Description: "Articles, read aloud.",
			Link:        d.PublicURL,
			FeedURL:     d.PublicURL + "/feed.xml",
			ImageURL:    d.PublicURL + "/artwork.png",
			Author:      "phonetic",
			Language:    "en",
		}, episodes)
		if err != nil {
			d.Logger.Error("rendering feed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(body)
		h := w.Header()
		h.Set("Content-Type", "application/rss+xml; charset=utf-8")
		h.Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
		h.Set("Cache-Control", "private, no-cache") // behind credentials; always revalidate
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}
}

// episodeOf describes a ready article as a feed episode.
func episodeOf(a store.Article, publicURL string) feed.Episode {
	title := a.Title
	if title == "" {
		title = a.URL
	}
	return feed.Episode{
		GUID:        fmt.Sprintf("phonetic-article-%d", a.ID),
		Title:       title,
		Author:      a.Author,
		Link:        a.URL,
		Description: episodeDescription(a),
		PubDate:     a.ReadyAt,
		AudioURL:    fmt.Sprintf("%s/audio/%d.mp3", publicURL, a.ID),
		AudioBytes:  a.AudioBytes,
		Duration:    a.Duration,
	}
}

// episodeDescription is the episode's show notes (HTML): where the article
// came from and how long it is.
func episodeDescription(a store.Article) string {
	source := a.SiteName
	if source == "" {
		if u, err := url.Parse(a.URL); err == nil {
			source = u.Hostname()
		}
	}
	notes := fmt.Sprintf(`From <a href="%s">%s</a>`, html.EscapeString(a.URL), html.EscapeString(source))
	if a.WordCount > 0 {
		notes += fmt.Sprintf(" · %s words", groupThousands(a.WordCount))
	}
	return "<p>" + notes + "</p>"
}

// groupThousands formats n with comma separators: 12345 → "12,345".
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// handleAudio serves an episode's MP3 (/audio/<id>.mp3), with range
// requests, which podcast apps use to resume and seek.
func handleAudio(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ServeMux wildcards span whole segments, so the suffix is checked here.
		raw, ok := strings.CutSuffix(r.PathValue("file"), ".mp3")
		id, okID := parseID(raw)
		if !ok || !okID {
			http.NotFound(w, r)
			return
		}
		a, err := d.Feed.GetArticle(r.Context(), id)
		// Only what the feed lists (see handleFeed) is served.
		if errors.Is(err, store.ErrNotFound) || (err == nil && (a.Status != store.StatusReady || a.AudioBytes <= 0)) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			d.Logger.Error("loading article", "article", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		f, err := os.Open(artifact.For(d.DataDir, id).Path(artifact.Audio))
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			d.Logger.Error("opening audio", "article", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			d.Logger.Error("stat audio", "article", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "audio/mpeg")
		h.Set("Cache-Control", "private")
		// A strong validator, so a resumed download (If-Range) can never
		// splice two versions of the file, even ones written in the same
		// second.
		h.Set("ETag", fmt.Sprintf(`"%d-%d-%d"`, id, info.Size(), info.ModTime().UnixNano()))
		http.ServeContent(w, r, "", info.ModTime(), f)
	}
}

// handleArtwork serves the channel artwork. It needs no auth: it's not
// private, and some apps fetch artwork without the feed's credentials.
func handleArtwork(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(feed.Artwork()))
}

// Package feed renders the podcast feed: RSS 2.0 with the iTunes tags Apple
// Podcasts and other apps read.
package feed

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"time"
)

// Channel describes the podcast itself.
type Channel struct {
	Title       string
	Description string
	Link        string // the site, e.g. the public base URL
	FeedURL     string // where this feed is served (atom:link rel="self")
	ImageURL    string // square artwork, 1400-3000 px
	Author      string
	Language    string // e.g. "en"
}

// Episode is one item of the feed.
type Episode struct {
	GUID        string // stable forever; apps use it to recognize episodes
	Title       string
	Author      string
	Link        string // the source article
	Description string // HTML
	PubDate     time.Time
	AudioURL    string
	AudioBytes  int64
	Duration    time.Duration
}

// Render returns the feed document for c and episodes (in the order given;
// newest first by convention). Fields apps need to show or tell apart
// episodes are required. Text that XML can't hold (control characters,
// invalid UTF-8) comes out as U+FFFD, so the document is always well-formed.
func Render(c Channel, episodes []Episode) ([]byte, error) {
	if c.Title == "" || c.Link == "" || c.ImageURL == "" || c.Language == "" {
		return nil, errors.New("feed: channel needs a title, link, image and language")
	}
	for i, e := range episodes {
		if e.GUID == "" || e.AudioURL == "" || e.PubDate.IsZero() {
			return nil, fmt.Errorf("feed: episode %d (%q) needs a GUID, audio URL and date", i, e.GUID)
		}
	}
	ch := channelXML{
		Title:       c.Title,
		Link:        c.Link,
		Description: c.Description,
		Language:    c.Language,
		AtomLink:    atomLinkXML{Href: c.FeedURL, Rel: "self", Type: "application/rss+xml"},
		Author:      c.Author,
		Explicit:    "false",
		Category:    categoryXML{Text: "News"},
		ITunesImage: imageRefXML{Href: c.ImageURL},
		Image:       imageXML{URL: c.ImageURL, Title: c.Title, Link: c.Link},
	}
	var newest time.Time
	for _, e := range episodes {
		if e.PubDate.After(newest) {
			newest = e.PubDate
		}
		ch.Items = append(ch.Items, itemXML{
			Title:       e.Title,
			Link:        e.Link,
			Description: e.Description,
			Author:      e.Author,
			GUID:        guidXML{IsPermaLink: "false", Value: e.GUID},
			PubDate:     rfc822(e.PubDate),
			Enclosure:   enclosureXML{URL: e.AudioURL, Length: e.AudioBytes, Type: "audio/mpeg"},
			Duration:    durationSeconds(e.Duration),
			EpisodeType: "full",
		})
	}
	if !newest.IsZero() {
		ch.LastBuildDate = rfc822(newest)
	}
	doc := rssXML{
		Version:  "2.0",
		ITunesNS: "http://www.itunes.com/dtds/podcast-1.0.dtd",
		AtomNS:   "http://www.w3.org/2005/Atom",
		Channel:  ch,
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	enc := xml.NewEncoder(&b)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// durationSeconds is d in whole seconds, rounded, but at least 1 for any
// audio (0 omits the tag).
func durationSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return max(1, int64(math.Round(d.Seconds())))
}

// rfc822 formats t as RSS dates are written (RFC 822 with a four-digit year).
func rfc822(t time.Time) string { return t.UTC().Format(time.RFC1123Z) }

// The XML shapes. encoding/xml has no prefix support for namespaced
// elements, so prefixed names are written literally and the namespaces are
// declared on <rss>. These structs are for encoding only: decoding needs the
// namespace URIs, and mixing the two forms here would break.
type rssXML struct {
	XMLName  xml.Name   `xml:"rss"`
	Version  string     `xml:"version,attr"`
	ITunesNS string     `xml:"xmlns:itunes,attr"`
	AtomNS   string     `xml:"xmlns:atom,attr"`
	Channel  channelXML `xml:"channel"`
}

type channelXML struct {
	Title         string      `xml:"title"`
	Link          string      `xml:"link"`
	Description   string      `xml:"description"`
	Language      string      `xml:"language,omitempty"`
	LastBuildDate string      `xml:"lastBuildDate,omitempty"`
	AtomLink      atomLinkXML `xml:"atom:link"`
	Author        string      `xml:"itunes:author,omitempty"`
	Explicit      string      `xml:"itunes:explicit"`
	Category      categoryXML `xml:"itunes:category"`
	ITunesImage   imageRefXML `xml:"itunes:image"`
	Image         imageXML    `xml:"image"`
	Items         []itemXML   `xml:"item"`
}

type atomLinkXML struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type categoryXML struct {
	Text string `xml:"text,attr"`
}

type imageRefXML struct {
	Href string `xml:"href,attr"`
}

type imageXML struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type itemXML struct {
	Title       string       `xml:"title"`
	Link        string       `xml:"link,omitempty"`
	Description string       `xml:"description"`
	Author      string       `xml:"itunes:author,omitempty"`
	GUID        guidXML      `xml:"guid"`
	PubDate     string       `xml:"pubDate"`
	Enclosure   enclosureXML `xml:"enclosure"`
	Duration    int64        `xml:"itunes:duration,omitempty"`
	EpisodeType string       `xml:"itunes:episodeType"`
}

type guidXML struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type enclosureXML struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

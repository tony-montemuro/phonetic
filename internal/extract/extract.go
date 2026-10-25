// Package extract pulls the readable article out of a web page: metadata,
// the body as typed blocks of plain text, and the images with whatever
// context helps judge how much they matter.
//
// It wraps codeberg.org/readeck/go-readability (a port of Mozilla's
// Readability.js) to find the article, then walks the cleaned article tree
// itself, since readability's own text rendering loses block structure.
//
// Extraction keeps content, not presentation decisions: code and data
// tables are kept as their own block kinds, and SpokenText decides how they
// sound. A later LLM step can do better with the originals.
package extract

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MinWords is the smallest spoken body that counts as an article. Below it
// the page is most likely an index, error, consent or paywall page.
const MinWords = 30

const (
	contextChars = 300 // each side of an image's surrounding-text context
	excerptChars = 300
)

// ErrNoContent means no readable article was found.
var ErrNoContent = errors.New("no readable article content")

// Placeholders SpokenText uses for content that doesn't work as audio.
const (
	CodeOmitted  = "(A code example is omitted here.)"
	TableOmitted = "(A table is omitted here.)"
)

// BlockKind classifies a block of the article body.
type BlockKind string

const (
	KindParagraph    BlockKind = "paragraph"
	KindHeading      BlockKind = "heading"
	KindListItem     BlockKind = "list_item"
	KindQuote        BlockKind = "quote"
	KindCaption      BlockKind = "caption"      // a figure caption with no image, e.g. a quote's attribution
	KindPreformatted BlockKind = "preformatted" // <pre> prose such as poetry or lyrics; lines preserved
	KindCode         BlockKind = "code"         // lines preserved
	KindTable        BlockKind = "table"        // rows on lines, cells separated by " | "
)

// Spoken reports whether blocks of this kind are read aloud as-is (and count
// toward the word count).
func (k BlockKind) Spoken() bool {
	return k != KindCode && k != KindTable
}

// Block is one unit of the article body. Text has whitespace collapsed,
// except that line breaks inside the block are kept as "\n" (and code and
// preformatted blocks keep their indentation and blank lines).
//
// Text is what gets read aloud. When that differs from the original (math
// spoken as "(formula)"), Source holds the original, e.g. with the TeX.
type Block struct {
	Kind   BlockKind `json:"kind"`
	Text   string    `json:"text"`
	Source string    `json:"source,omitzero"`
}

// Image is an image in the article body.
type Image struct {
	URL     string `json:"url"`
	Alt     string `json:"alt,omitzero"`
	Caption string `json:"caption,omitzero"`
	// Context is the text just before and after the image, to help judge
	// whether the article depends on it.
	Context string `json:"context,omitzero"`
	// Block is the index of the block containing the image (for an image
	// inline in a paragraph) or the first block after it (len(Blocks) if the
	// image is last).
	Block int `json:"block"`
}

// Article is the extraction result.
type Article struct {
	Title       string    `json:"title"`
	Byline      string    `json:"byline,omitzero"`
	SiteName    string    `json:"site_name,omitzero"`
	Language    string    `json:"language,omitzero"`
	Excerpt     string    `json:"excerpt,omitzero"`
	LeadImage   string    `json:"lead_image,omitzero"` // the page's preview image (og:image), if any
	PublishedAt time.Time `json:"published_at,omitzero"`
	ModifiedAt  time.Time `json:"modified_at,omitzero"`
	Blocks      []Block   `json:"blocks"`
	// WordCount counts spoken blocks only (approximated for CJK text).
	WordCount int     `json:"word_count"`
	Images    []Image `json:"images"`
}

// Extract finds the article in page, which must already be UTF-8 (as
// fetch.Result.Body is), fetched from pageURL.
//
// The page is parsed here as UTF-8, rather than by readability's FromReader,
// which guesses the encoding statistically and can guess wrong.
func Extract(page []byte, pageURL *url.URL) (Article, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return Article{}, fmt.Errorf("%w: parsing HTML: %v", ErrNoContent, err)
	}
	prepare(root)
	parser := readability.NewParser()
	// Keep the classes the walker needs; readability strips all others.
	parser.ClassesToPreserve = append(parser.ClassesToPreserve, preservedClasses...)
	parsed, err := parser.ParseAndMutate(root, pageURL)
	if err != nil {
		return Article{}, fmt.Errorf("%w: %v", ErrNoContent, err)
	}
	if parsed.Node == nil {
		return Article{}, ErrNoContent
	}

	w := &walker{base: pageURL, title: cleanLine(parsed.Title()), kinds: []BlockKind{KindParagraph}}
	w.walk(parsed.Node)
	w.flush()

	a := Article{
		Title:     cleanLine(parsed.Title()),
		Byline:    cleanLine(parsed.Byline()),
		SiteName:  cleanLine(parsed.SiteName()),
		Language:  cleanLine(parsed.Language()),
		Excerpt:   head(cleanLine(parsed.Excerpt()), excerptChars),
		LeadImage: absoluteHTTP(pageURL, parsed.ImageURL()),
		Blocks:    w.blocks,
		Images:    w.images(),
	}
	if a.Blocks == nil {
		a.Blocks = []Block{}
	}
	if t, err := parsed.PublishedTime(); err == nil && !t.IsZero() {
		a.PublishedAt = t.UTC()
	}
	if t, err := parsed.ModifiedTime(); err == nil && !t.IsZero() {
		a.ModifiedAt = t.UTC()
	}
	for _, b := range a.Blocks {
		if b.Kind.Spoken() {
			a.WordCount += countWords(b.Text)
		}
	}
	if a.WordCount < MinWords {
		return Article{}, fmt.Errorf("%w: only %d words", ErrNoContent, a.WordCount)
	}
	return a, nil
}

// SpokenText returns the body as it should be read aloud: one block per
// paragraph (blank-line separated), with code and data tables replaced by
// short placeholders.
func (a Article) SpokenText() string {
	parts := make([]string, 0, len(a.Blocks))
	for _, b := range a.Blocks {
		switch b.Kind {
		case KindCode:
			parts = append(parts, CodeOmitted)
		case KindTable:
			parts = append(parts, TableOmitted)
		default:
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Classes readability must keep for the walker to recognize these elements.
const (
	classCode          = "phonetic-code" // set by prepare on code <pre>s
	classEmoji         = "emoji"
	classMathInline    = "mwe-math-fallback-image-inline"
	classMathDisplay   = "mwe-math-fallback-image-display"
	classWPCaptionText = "wp-caption-text"
)

// minLayoutCellWords: a table cell this long means a layout table.
const minLayoutCellWords = 50

// formulaSpoken stands in for TeX-like math in spoken text.
const formulaSpoken = "(formula)"

var preservedClasses = []string{classCode, classEmoji, classMathInline, classMathDisplay, classWPCaptionText}

// Site-specific chrome prepare removes before readability runs. Each list
// applies only where its site's markup is recognized, so generic class names
// like "references" or "tagline" are never deleted on other sites.
var (
	// mediaWikiChrome applies on MediaWiki pages (<body class="mediawiki">).
	mediaWikiChrome = []string{
		"mw-editsection",     // "[edit]" links (their wrapper also hides headings from readability)
		"reflist",            // reference lists
		"mw-references-wrap", //
		"references",         //
		"shortdescription",   // hidden short description
		"tagline",            // mobile-skin page tagline
		"mw-redirectedfrom",  // "(Redirected from ...)"
	}
	mediaWikiChromeIDs = []string{"contentSub", "siteSub"}
	// codeExampleChrome applies inside MDN code examples (.code-example).
	codeExampleChrome = []string{"example-header", "language-name"}
)

// prepare adjusts the parsed page before readability sees it:
//   - puts React streaming content where it belongs (see
//     resolveStreamedContent), since readability skips hidden elements;
//   - removes known site chrome (see mediaWikiChrome, codeExampleChrome);
//   - marks code <pre>s (see isCodePre) with classCode, so code without a
//     <code> child isn't read aloud as prose;
//   - strips class and id attributes inside <pre> (except preservedClasses):
//     they're only syntax highlighting, and readability deletes elements
//     whose class looks unlikely (e.g. "comment"), which would remove code
//     comments.
func prepare(root *html.Node) {
	resolveStreamedContent(root)
	mediaWiki := false
	if body := find(root, atom.Body); body != nil {
		mediaWiki = hasClass(body, "mediawiki")
	}
	var remove []*html.Node
	var rec func(n *html.Node, inCodeExample bool)
	rec = func(n *html.Node, inCodeExample bool) {
		if n.Type == html.ElementNode {
			if (mediaWiki && isMediaWikiChrome(n)) || (inCodeExample && hasAnyClass(n, codeExampleChrome)) {
				remove = append(remove, n)
				return
			}
			inCodeExample = inCodeExample || hasClass(n, "code-example")
			if atom.Lookup([]byte(n.Data)) == atom.Pre {
				if isCodePre(n) {
					n.Attr = setOnlyClass(dropAttr(n.Attr, "id"), classCode)
				}
				stripHighlighting(n)
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c, inCodeExample)
		}
	}
	rec(root, false)
	for _, n := range remove {
		n.Parent.RemoveChild(n)
	}
}

// reactInstruction matches the inline-script calls React's streaming
// renderer uses to place late content: $RC/$RR("B:x","S:y") complete a
// Suspense boundary, $RS("S:y","P:x") a segment.
var reactInstruction = regexp.MustCompile(`\$(RC|RR|RS)\("([^"]+)","([^"]+)"`)

// resolveStreamedContent does what React's streaming server rendering does
// in the browser (React 18/19 "Fizz"): content arrives later in the page in a
// hidden container (usually <div hidden id="S:y">), and inline scripts move
// it into place:
//   - $RC/$RR("B:x","S:y"): the boundary between <!--$?--> and <!--/$-->
//     (template B:x plus fallback content such as a spinner) is replaced by
//     the content;
//   - $RS("S:y","P:x"): template P:x is replaced by the content.
//
// Without this, readability (which skips hidden elements) sees only
// placeholders and fallbacks. Pairing comes from the scripts themselves, so
// pages without them are never touched. Known gap: React's external runtime
// (data-bid/data-sid attributes instead of inline scripts) isn't handled.
func resolveStreamedContent(root *html.Node) {
	var calls [][]string
	byID := map[string]*html.Node{}
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if id := attr(n, "id"); id != "" {
				if _, dup := byID[id]; !dup {
					byID[id] = n
				}
			}
			if atom.Lookup([]byte(n.Data)) == atom.Script {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.TextNode {
						calls = append(calls, reactInstruction.FindAllStringSubmatch(c.Data, -1)...)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(root)
	for _, call := range calls { // document order, as the browser runs them
		kind, a, b := call[1], call[2], call[3]
		placeholder, chunk := byID[a], byID[b]
		if kind == "RS" {
			placeholder, chunk = byID[b], byID[a]
		}
		if placeholder == nil || chunk == nil || placeholder.Parent == nil || chunk.Parent == nil ||
			isAncestor(chunk, placeholder) {
			continue
		}
		if kind == "RS" {
			moveChildrenBefore(chunk, placeholder)
			placeholder.Parent.RemoveChild(placeholder)
		} else {
			completeBoundary(placeholder, chunk)
		}
		removeChunk(chunk)
	}
}

// completeBoundary replaces a Suspense boundary's fallback with its content.
// The boundary is marked by comments: <!--$?--><template id="B:x">fallback
// <!--/$-->. Without the markers, the content just replaces the template.
func completeBoundary(tmpl, chunk *html.Node) {
	start := tmpl.PrevSibling
	if start == nil || start.Type != html.CommentNode || start.Data != "$?" {
		moveChildrenBefore(chunk, tmpl)
		tmpl.Parent.RemoveChild(tmpl)
		return
	}
	// Remove everything up to the matching <!--/$-->, tracking nesting.
	depth := 0
	n := start.NextSibling
	for n != nil {
		next := n.NextSibling
		if n.Type == html.CommentNode {
			switch n.Data {
			case "/$":
				if depth == 0 {
					moveChildrenBefore(chunk, n)
					start.Data = "$"
					return
				}
				depth--
			case "$", "$?", "$!":
				depth++
			}
		}
		n.Parent.RemoveChild(n)
		n = next
	}
}

func moveChildrenBefore(from, before *html.Node) {
	for from.FirstChild != nil {
		c := from.FirstChild
		from.RemoveChild(c)
		before.Parent.InsertBefore(c, before)
	}
}

// removeChunk removes an emptied content container, and its hidden wrapper
// if it had one (React uses <table hidden><tbody id="S:y"> and similar for
// content that can't live in a div).
func removeChunk(chunk *html.Node) {
	parent := chunk.Parent
	parent.RemoveChild(chunk)
	if parent.Type == html.ElementNode && hasAttr(parent, "hidden") && parent.FirstChild == nil && parent.Parent != nil {
		parent.Parent.RemoveChild(parent)
	}
}

// isAncestor reports whether a is an ancestor of n.
func isAncestor(a, n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == a {
			return true
		}
	}
	return false
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return true
		}
	}
	return false
}

func isMediaWikiChrome(n *html.Node) bool {
	return hasAnyClass(n, mediaWikiChrome) || slices.Contains(mediaWikiChromeIDs, attr(n, "id"))
}

func hasAnyClass(n *html.Node, classes []string) bool {
	for _, c := range classes {
		if hasClass(n, c) {
			return true
		}
	}
	return false
}

// isCodePre decides whether a <pre> holds source code. Language markers
// (language-go, lang-py, brush:js) count only on the <pre> itself or its
// <code> child, since CMSs put lang-xx classes on whole pages. Highlighter
// containers count on the <pre> or up to two ancestors (never <html> or
// <body>).
func isCodePre(pre *html.Node) bool {
	own := []*html.Node{pre}
	if c := find(pre, atom.Code); c != nil {
		own = append(own, c)
	}
	for _, n := range own {
		if hasLanguageClass(n) || hasHighlighterClass(n) {
			return true
		}
	}
	p := pre.Parent
	for range 2 {
		if p == nil || p.Type != html.ElementNode {
			break
		}
		if t := atom.Lookup([]byte(p.Data)); t == atom.Body || t == atom.Html {
			break
		}
		if hasHighlighterClass(p) {
			return true
		}
		p = p.Parent
	}
	return false
}

func hasLanguageClass(n *html.Node) bool {
	for _, c := range strings.Fields(strings.ToLower(attr(n, "class"))) {
		if strings.HasPrefix(c, "language-") || strings.HasPrefix(c, "lang-") || strings.HasPrefix(c, "brush:") {
			return true
		}
	}
	return false
}

// hasHighlighterClass reports a class used by syntax highlighters and code
// blocks: Pygments/Sphinx, highlight.js, Prism, Chroma, WordPress, Markdown
// renderers and the like.
func hasHighlighterClass(n *html.Node) bool {
	for _, c := range strings.Fields(strings.ToLower(attr(n, "class"))) {
		switch {
		case c == "code", c == "highlight", c == "sourcecode", c == "prettyprint", c == "chroma",
			c == "hljs", c == "codehilite", c == "wp-block-code",
			strings.HasPrefix(c, "highlight-"):
			return true
		}
	}
	return false
}

// stripHighlighting removes ids and classes from everything inside n,
// keeping only the classes the walker relies on (preservedClasses).
func stripHighlighting(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			var keep []string
			for _, cl := range strings.Fields(attr(c, "class")) {
				if slices.Contains(preservedClasses, cl) {
					keep = append(keep, cl)
				}
			}
			c.Attr = dropAttr(dropAttr(c.Attr, "class"), "id")
			if len(keep) > 0 {
				c.Attr = append(c.Attr, html.Attribute{Key: "class", Val: strings.Join(keep, " ")})
			}
		}
		stripHighlighting(c)
	}
}

func dropAttr(attrs []html.Attribute, key string) []html.Attribute {
	return slices.DeleteFunc(attrs, func(a html.Attribute) bool { return a.Namespace == "" && a.Key == key })
}

// setOnlyClass replaces every class attribute with a single one holding class.
func setOnlyClass(attrs []html.Attribute, class string) []html.Attribute {
	return append(dropAttr(attrs, "class"), html.Attribute{Key: "class", Val: class})
}

// walker turns the cleaned article tree into blocks and images.
type walker struct {
	base   *url.URL
	title  string // images whose alt text is the title are banners, not content
	blocks []Block
	buf    strings.Builder // spoken text of the current block
	src    strings.Builder // the same, with originals (TeX) instead of stand-ins
	kinds  []BlockKind     // stack: the kind of the innermost enclosing block
	// pendingBR is set after a <br> with no text since; a second <br> then
	// ends the paragraph.
	pendingBR bool
	found     []Image
}

func (w *walker) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		// Source newlines are ordinary whitespace in HTML; only <br> makes
		// a line break inside a block.
		w.text(strings.ReplaceAll(n.Data, "\n", " "))
		return
	case html.ElementNode:
	default:
		w.walkChildren(n)
		return
	}

	// readability renames elements by changing only n.Data, and creates
	// nodes without a DataAtom, so the tag name is the source of truth.
	tag := atom.Lookup([]byte(n.Data))
	if n.Data == "mjx-container" { // MathJax 3: speak the formula once
		w.math(find(n, atom.Math))
		return
	}
	switch tag {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg,
		atom.Iframe, atom.Video, atom.Audio, atom.Object, atom.Embed, atom.Canvas:
		return // nothing here should be read aloud
	case atom.Math:
		w.math(n)
		return
	case atom.Img:
		w.image(n)
		return
	case atom.Br:
		if w.pendingBR {
			w.flush() // a run of <br>s separates paragraphs
		} else {
			w.buf.WriteByte('\n')
			w.src.WriteByte('\n')
			w.pendingBR = true
		}
		return
	case atom.Sup:
		if w.isFootnoteMarker(n) {
			return
		}
	case atom.A:
		if w.isPermalink(n) {
			return
		}
	case atom.Pre:
		w.pre(n)
		return
	case atom.Table:
		if isLayoutTable(n) {
			break // walk it as ordinary content; cells are blocks
		}
		w.flush()
		spoken, source := tableText(n)
		w.emitBlock(KindTable, spoken, source)
		w.collectImages(n)
		return
	case atom.Figcaption:
		if figureHasMedia(n) {
			return // attached to the image instead
		}
	}
	if hasClass(n, classWPCaptionText) && w.captionLastImage(cleanLine(spokenTextOf(n))) {
		return
	}

	if isInline(n, tag) {
		w.walkChildren(n)
		return
	}
	w.flush()
	w.kinds = append(w.kinds, w.kindFor(tag))
	w.walkChildren(n)
	w.flush()
	w.kinds = w.kinds[:len(w.kinds)-1]
}

func (w *walker) walkChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}
}

// kindFor decides the kind of a block element: headings and list items are
// their own kind; generic blocks inherit from an enclosing heading, quote,
// list item or caption.
func (w *walker) kindFor(tag atom.Atom) BlockKind {
	switch tag {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		return KindHeading
	case atom.Li, atom.Dt, atom.Dd:
		return KindListItem
	case atom.Blockquote:
		return KindQuote
	case atom.Figcaption:
		return KindCaption
	}
	switch parent := w.kinds[len(w.kinds)-1]; parent {
	case KindHeading, KindQuote, KindListItem, KindCaption:
		return parent
	}
	return KindParagraph
}

// text appends inline text to the current block (spoken and source alike).
func (w *walker) text(s string) {
	w.inline(s, s)
}

// inline appends text whose spoken form may differ from its original (math
// as "(formula)"). Emoji and math stand-ins get a leading space if needed
// but none trailing, so following punctuation isn't pushed away.
func (w *walker) inline(spoken, source string) {
	if strings.TrimFunc(spoken, isSpace) != "" {
		w.pendingBR = false
	}
	w.buf.WriteString(spoken)
	w.src.WriteString(source)
}

func (w *walker) inlineWord(spoken, source string) {
	if w.buf.Len() > 0 && !strings.HasSuffix(w.buf.String(), " ") {
		spoken, source = " "+spoken, " "+source
	}
	w.inline(spoken, source)
}

func (w *walker) math(n *html.Node) {
	w.inlineWord(mathText(n))
}

// flush ends the current block, if it has any text.
func (w *walker) flush() {
	w.emitBlock(w.kinds[len(w.kinds)-1], cleanLines(w.buf.String()), cleanLines(w.src.String()))
	w.buf.Reset()
	w.src.Reset()
	w.pendingBR = false
}

// emitBlock appends a block; source is kept only when it differs.
func (w *walker) emitBlock(kind BlockKind, text, source string) {
	if text == "" {
		return
	}
	b := Block{Kind: kind, Text: text}
	if source != text {
		b.Source = source
	}
	w.blocks = append(w.blocks, b)
}

// pre emits a <pre> as code (if it contains <code>) or as preformatted
// prose, keeping its lines.
func (w *walker) pre(n *html.Node) {
	w.flush()
	kind := KindPreformatted
	if hasClass(n, classCode) || find(n, atom.Code) != nil {
		kind = KindCode
	}
	spoken, source := rawText(n)
	w.emitBlock(kind, keepLines(spoken), keepLines(source))
	w.collectImages(n)
}

// image handles an <img>: emoji and math fallbacks are inline text; others
// are article images.
func (w *walker) image(n *html.Node) {
	if spoken, source, ok := inlineImageText(n); ok {
		w.inlineWord(spoken, source)
		return
	}
	w.addImage(n)
}

// collectImages records article images inside n without emitting text
// (inline emoji and math images are part of n's own text).
func (w *walker) collectImages(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && atom.Lookup([]byte(c.Data)) == atom.Img {
			if _, _, inline := inlineImageText(c); !inline {
				w.addImage(c)
			}
		}
		w.collectImages(c)
	}
}

func (w *walker) addImage(n *html.Node) {
	src := imageSource(n)
	alt := cleanLine(attr(n, "alt"))
	if isTracker(n) || (alt != "" && strings.EqualFold(alt, w.title)) {
		return // tracking pixel, or a banner image of the title
	}
	u := absoluteHTTP(w.base, src)
	if u == "" {
		return
	}
	w.found = append(w.found, Image{
		URL:     u,
		Alt:     alt,
		Caption: figureCaption(n),
		Block:   len(w.blocks),
	})
}

// captionLastImage attaches a WordPress-style caption (a sibling element,
// not a <figcaption>) to the image just before it. It reports whether the
// caption was used.
func (w *walker) captionLastImage(caption string) bool {
	if len(w.found) == 0 || caption == "" {
		return false
	}
	last := &w.found[len(w.found)-1]
	// The caption must directly follow the image, or the paragraph the image
	// sat inline in; otherwise it isn't this image's caption.
	if last.Block < len(w.blocks)-1 || last.Caption != "" {
		return false
	}
	last.Caption = caption
	return true
}

// images returns the found images (deduplicated by URL) with their context.
func (w *walker) images() []Image {
	out := []Image{}
	seen := map[string]bool{}
	for _, img := range w.found {
		if seen[img.URL] {
			continue
		}
		seen[img.URL] = true
		var parts []string
		if prev := w.spokenBlock(img.Block-1, -1); prev != "" {
			parts = append(parts, tail(prev, contextChars))
		}
		if next := w.spokenBlock(img.Block, +1); next != "" {
			parts = append(parts, head(next, contextChars))
		}
		img.Context = strings.Join(parts, " … ")
		out = append(out, img)
	}
	return out
}

// spokenBlock returns the text of the nearest spoken block starting at i and
// moving in direction dir, or "".
func (w *walker) spokenBlock(i, dir int) string {
	for ; i >= 0 && i < len(w.blocks); i += dir {
		if w.blocks[i].Kind.Spoken() {
			return strings.ReplaceAll(w.blocks[i].Text, "\n", " ")
		}
	}
	return ""
}

// inlineTags are phrasing elements that don't break a block. Everything
// else does (except custom elements, see isInline): an extra break is
// harmless, glued-together words are not.
var inlineTags = map[atom.Atom]bool{
	atom.A: true, atom.Abbr: true, atom.B: true, atom.Bdi: true, atom.Bdo: true, atom.Cite: true,
	atom.Code: true, atom.Data: true, atom.Del: true, atom.Dfn: true, atom.Em: true, atom.Font: true,
	atom.I: true, atom.Ins: true, atom.Kbd: true, atom.Label: true, atom.Mark: true, atom.Q: true,
	atom.Rp: true, atom.Rt: true, atom.Ruby: true, atom.S: true, atom.Samp: true, atom.Small: true,
	atom.Span: true, atom.Strike: true, atom.Strong: true, atom.Sub: true, atom.Sup: true,
	atom.Time: true, atom.Tt: true, atom.U: true, atom.Var: true, atom.Wbr: true, atom.Big: true,
	atom.Picture: true, atom.Source: true, atom.Nobr: true, atom.Acronym: true, atom.Output: true,
	atom.Slot: true,
}

// isInline reports whether an element continues the current block. Custom
// elements (web components, MathJax's mjx-*) count as inline: they mostly
// appear inside running text.
func isInline(n *html.Node, tag atom.Atom) bool {
	return inlineTags[tag] || (tag == 0 && strings.Contains(n.Data, "-"))
}

// isFootnoteMarker reports a <sup> holding only a link to a fragment of
// this same page, like "1" or "[3]" footnote references.
func (w *walker) isFootnoteMarker(n *html.Node) bool {
	var link *html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode && strings.TrimFunc(c.Data, isBracketOrSpace) == "":
		case c.Type == html.ElementNode && atom.Lookup([]byte(c.Data)) == atom.A && link == nil:
			link = c
		default:
			return false
		}
	}
	return link != nil && w.isSamePageFragment(attr(link, "href"))
}

// isSamePageFragment reports whether href points at a fragment of the page
// itself ("#x", or the page's own URL plus "#x").
func (w *walker) isSamePageFragment(href string) bool {
	target, err := w.base.Parse(href)
	if err != nil || target.Fragment == "" {
		return false
	}
	page := *w.base
	page.Fragment, page.RawFragment = "", ""
	target.Fragment, target.RawFragment = "", ""
	return target.String() == page.String()
}

func isBracketOrSpace(r rune) bool { return isSpace(r) || r == '[' || r == ']' }

// permalinkSymbols are the whole text of heading anchor links ("Section¶").
var permalinkSymbols = map[string]bool{"\u00b6": true, "#": true, "\u00a7": true, "\U0001F517": true}

// isPermalink reports a link to a fragment of this same page whose only
// text is a permalink symbol (a trailing variation selector is ignored).
func (w *walker) isPermalink(a *html.Node) bool {
	text := strings.TrimFunc(textOf(a), func(r rune) bool { return isSpace(r) || r == '\ufe0f' })
	return permalinkSymbols[text] && w.isSamePageFragment(attr(a, "href"))
}

// isLayoutTable guesses whether a table is page layout rather than data,
// following the signals readability itself uses: a nested table or a long
// cell means layout; header cells, a caption, a summary or role=grid mean
// data; role=presentation or a single column means layout; otherwise block
// content in cells means layout.
func isLayoutTable(n *html.Node) bool {
	switch attr(n, "role") {
	case "presentation", "none":
		return true
	case "grid", "treegrid":
		return false
	}
	var (
		nested, longCell, headers, blocks bool
		maxCols                           int
	)
	var rec func(*html.Node)
	rec = func(c *html.Node) {
		for ; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch atom.Lookup([]byte(c.Data)) {
			case atom.Table:
				nested = true
			case atom.Th, atom.Thead, atom.Caption:
				headers = true
			case atom.Tr:
				cols := 0
				for cell := c.FirstChild; cell != nil; cell = cell.NextSibling {
					if t := atom.Lookup([]byte(cell.Data)); cell.Type == html.ElementNode && (t == atom.Td || t == atom.Th) {
						cols++
					}
				}
				maxCols = max(maxCols, cols)
			case atom.Td:
				if countWords(textOf(c)) >= minLayoutCellWords {
					longCell = true
				}
			case atom.P, atom.Div, atom.Blockquote, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Ul, atom.Ol, atom.Pre:
				blocks = true
			}
			rec(c.FirstChild)
		}
	}
	rec(n.FirstChild)
	switch {
	case nested || longCell:
		return true
	case headers || attr(n, "summary") != "":
		return false
	case maxCols <= 1:
		return true
	}
	return blocks
}

// tableText renders a data table as rows of " | "-separated cells, in
// spoken and original form.
func tableText(n *html.Node) (spoken, source string) {
	var rows, srcRows []string
	var rec func(*html.Node)
	rec = func(c *html.Node) {
		for ; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && atom.Lookup([]byte(c.Data)) == atom.Tr {
				var cells, srcCells []string
				for cell := c.FirstChild; cell != nil; cell = cell.NextSibling {
					if t := atom.Lookup([]byte(cell.Data)); cell.Type == html.ElementNode && (t == atom.Td || t == atom.Th) {
						cells = append(cells, cleanLine(spokenTextOf(cell)))
						srcCells = append(srcCells, cleanLine(sourceTextOf(cell)))
					}
				}
				if row := strings.Join(cells, " | "); strings.Trim(row, " |") != "" {
					rows = append(rows, row)
					srcRows = append(srcRows, strings.Join(srcCells, " | "))
				}
				continue
			}
			rec(c.FirstChild)
		}
	}
	rec(n.FirstChild)
	return strings.Join(rows, "\n"), strings.Join(srcRows, "\n")
}

// figureHasMedia reports whether a figcaption's nearest enclosing figure
// holds an image or video (so the caption describes it rather than being
// body text).
func figureHasMedia(figcaption *html.Node) bool {
	for fig := figcaption.Parent; fig != nil; fig = fig.Parent {
		if fig.Type == html.ElementNode && atom.Lookup([]byte(fig.Data)) == atom.Figure {
			return find(fig, atom.Img) != nil || find(fig, atom.Picture) != nil || find(fig, atom.Video) != nil
		}
	}
	return false
}

// figureCaption returns the <figcaption> text of the image's nearest
// enclosing <figure>, if any.
func figureCaption(img *html.Node) string {
	for p := img.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && atom.Lookup([]byte(p.Data)) == atom.Figure {
			if fc := find(p, atom.Figcaption); fc != nil {
				return cleanLine(spokenTextOf(fc))
			}
			return ""
		}
	}
	return ""
}

// mathText returns a <math> element's spoken stand-in and its original (see
// formulaText). n may be nil, for math we couldn't find.
func mathText(n *html.Node) (spoken, source string) {
	if n == nil {
		return formulaSpoken, formulaSpoken
	}
	return formulaText(attr(n, "alttext"))
}

// formulaText turns formula alt text into its spoken form: as-is if it reads
// as plain text ("x"), "(formula)" if it looks like TeX. The source keeps the
// original, minus Wikipedia's displaystyle wrapper.
func formulaText(alt string) (spoken, source string) {
	source = unwrapDisplayStyle(alt)
	switch {
	case source == "":
		return formulaSpoken, formulaSpoken
	case strings.ContainsAny(source, "\\^_{}"):
		return formulaSpoken, source
	}
	return source, source
}

// inlineImageText returns the inline text for emoji and math images, which
// are part of the running text rather than article images.
func inlineImageText(n *html.Node) (spoken, source string, ok bool) {
	switch {
	case hasClass(n, classEmoji):
		alt := cleanLine(attr(n, "alt"))
		return alt, alt, true
	case hasClass(n, classMathInline) || hasClass(n, classMathDisplay):
		spoken, source = formulaText(attr(n, "alt"))
		return spoken, source, true
	}
	return "", "", false
}

// unwrapDisplayStyle strips the "{\displaystyle ...}" wrapper Wikipedia puts
// around formula alt text, leaving the formula itself.
func unwrapDisplayStyle(s string) string {
	s = strings.TrimSpace(s)
	if inner, ok := strings.CutPrefix(s, `{\displaystyle`); ok && strings.HasSuffix(inner, "}") {
		s = strings.TrimSpace(strings.TrimSuffix(inner, "}"))
	}
	return s
}

// imageSource picks the image URL: lazy-loading attributes first (src is
// often a placeholder), then the largest srcset candidate, then src.
func imageSource(n *html.Node) string {
	for _, key := range []string{"data-src", "data-original", "data-lazy-src"} {
		if v := strings.TrimSpace(attr(n, key)); v != "" && !strings.HasPrefix(v, "data:") {
			return v
		}
	}
	if best := largestSrcset(attr(n, "srcset")); best != "" {
		return best
	}
	return strings.TrimSpace(attr(n, "src"))
}

// largestSrcset parses a srcset (per the HTML spec, a URL runs to the next
// whitespace and may contain commas) and returns the widest candidate.
func largestSrcset(srcset string) string {
	type candidate struct {
		url   string
		width float64
	}
	var cands []candidate
	s := srcset
	for {
		s = strings.TrimLeft(s, " \t\n\r\f,")
		if s == "" {
			break
		}
		end := strings.IndexAny(s, " \t\n\r\f")
		if end < 0 {
			end = len(s)
		}
		u := strings.TrimRight(s[:end], ",")
		s = s[end:]
		desc := ""
		if i := strings.IndexByte(s, ','); i >= 0 {
			desc, s = strings.TrimSpace(s[:i]), s[i+1:]
		} else {
			desc, s = strings.TrimSpace(s), ""
		}
		c := candidate{url: u}
		for _, d := range strings.Fields(desc) {
			if v, err := strconv.ParseFloat(d[:len(d)-1], 64); err == nil && (strings.HasSuffix(d, "w") || strings.HasSuffix(d, "x")) {
				c.width = v
			}
		}
		if u != "" && !strings.HasPrefix(u, "data:") {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	return slices.MaxFunc(cands, func(a, b candidate) int {
		switch {
		case a.width < b.width:
			return -1
		case a.width > b.width:
			return 1
		}
		return 0
	}).url
}

// isTracker reports tiny images (tracking pixels, spacers).
func isTracker(n *html.Node) bool {
	for _, key := range []string{"width", "height"} {
		if v, err := strconv.Atoi(strings.TrimSuffix(attr(n, key), "px")); err == nil && v <= 2 {
			return true
		}
	}
	return false
}

// absoluteHTTP resolves ref against base and returns it if it's http(s).
func absoluteHTTP(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "data:") {
		return ""
	}
	u, err := base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.String()
}

func find(n *html.Node, a atom.Atom) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && atom.Lookup([]byte(c.Data)) == a {
			return c
		}
		if f := find(c, a); f != nil {
			return f
		}
	}
	return nil
}

// spokenTextOf and sourceTextOf flatten an element's text the way the
// walker would (inline emoji and math included), for captions and table
// cells.
func spokenTextOf(n *html.Node) string { return inlineTextOf(n, true) }
func sourceTextOf(n *html.Node) string { return inlineTextOf(n, false) }

func inlineTextOf(n *html.Node, spoken bool) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode {
			tag := atom.Lookup([]byte(n.Data))
			s, src, ok := "", "", false
			switch tag {
			case atom.Img:
				s, src, ok = inlineImageText(n)
			case atom.Math:
				s, src = mathText(n)
				ok = true
			}
			if ok {
				if !spoken {
					s = src
				}
				b.WriteString(" " + s)
				return
			}
			if !isInline(n, tag) {
				b.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

// textOf concatenates n's text with spaces between nodes.
func textOf(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

// rawText concatenates n's text exactly (for <pre>, where whitespace is
// meaningful), in spoken and original form; <br> counts as a newline and
// inline emoji or math images as their text.
func rawText(n *html.Node) (spoken, source string) {
	var b, src strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
			src.WriteString(n.Data)
			return
		case html.ElementNode:
			switch atom.Lookup([]byte(n.Data)) {
			case atom.Br:
				b.WriteByte('\n')
				src.WriteByte('\n')
			case atom.Img:
				if s, o, ok := inlineImageText(n); ok {
					b.WriteString(s)
					src.WriteString(o)
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String(), src.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), class)
}

// invisible runes are deleted outright: break opportunities and soft hyphens
// that sit inside words. (ZWJ/ZWNJ are kept; they carry meaning in some
// scripts and emoji.)
var invisible = strings.NewReplacer("\u200b", "", "\ufeff", "", "\u2060", "", "\u00ad", "")

// sanitize makes s valid UTF-8 and removes invisible in-word characters.
func sanitize(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return invisible.Replace(s)
}

// cleanLine collapses all whitespace (newlines included) to single spaces.
func cleanLine(s string) string {
	return strings.Join(strings.FieldsFunc(sanitize(s), isSpace), " ")
}

// keepLines tidies preformatted text without changing its meaning: lines
// keep their indentation (minus what all of them share) and inner blank
// lines are kept; trailing whitespace and leading/trailing blank lines go.
func keepLines(s string) string {
	lines := strings.Split(sanitize(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	common := -1
	for _, l := range lines {
		if l == "" {
			continue
		}
		if indent := len(l) - len(strings.TrimLeft(l, " \t")); common < 0 || indent < common {
			common = indent
		}
	}
	if common > 0 {
		for i, l := range lines {
			if l != "" {
				lines[i] = l[common:]
			}
		}
	}
	return strings.Join(lines, "\n")
}

// cleanLines keeps each non-empty line, with whitespace collapsed. (The
// walker writes "\n" only for <br>; <pre> text keeps its real lines.)
func cleanLines(s string) string {
	var lines []string
	for line := range strings.SplitSeq(sanitize(s), "\n") {
		if l := cleanLine(line); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

func isSpace(r rune) bool {
	return unicode.IsSpace(r) || r == '\u2007' || r == '\u202f'
}

// countWords counts whitespace-separated words, approximating CJK text
// (written without spaces) at one word per two characters.
func countWords(s string) int {
	n := 0
	for field := range strings.FieldsFuncSeq(s, isSpace) {
		cjk := 0
		for _, r := range field {
			if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
				cjk++
			}
		}
		if cjk > 0 {
			n += max(1, (cjk+1)/2)
		} else {
			n++
		}
	}
	return n
}

// head and tail return at most n bytes from the start/end of s, cut at a
// word boundary when possible and never inside a UTF-8 sequence.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if i := strings.LastIndexByte(s[:cut], ' '); i > n/2 {
		cut = i
	}
	return s[:cut] + "…"
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	if i := strings.IndexByte(s[cut:], ' '); i >= 0 && i < n/2 {
		cut += i + 1
	}
	return "…" + s[cut:]
}

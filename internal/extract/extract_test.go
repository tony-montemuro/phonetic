package extract

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

var pageURL, _ = url.Parse("https://example.com/posts/1")

// filler is enough prose for readability to treat a block as the article.
const filler = "This sentence exists to give the paragraph enough substance that the readability scorer treats it as article content rather than page chrome, and it keeps going a little longer for good measure."

// doc wraps body in a page with typical surrounding chrome.
func doc(head, body string) string {
	return `<!doctype html><html lang="en"><head><title>A Test Post</title>` + head +
		`</head><body><nav><a href="/">Home</a> <a href="/about">About</a></nav><article>` +
		body + `</article><footer>Copyright 2026</footer></body></html>`
}

func mustExtract(t *testing.T, page string) Article {
	t.Helper()
	a, err := Extract([]byte(page), pageURL)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return a
}

// texts returns the blocks' texts, for compact assertions.
func texts(a Article) []string {
	out := make([]string, len(a.Blocks))
	for i, b := range a.Blocks {
		out[i] = b.Text
	}
	return out
}

func hasBlock(a Article, kind BlockKind, text string) bool {
	for _, b := range a.Blocks {
		if b.Kind == kind && b.Text == text {
			return true
		}
	}
	return false
}

func TestExtractMetadataAndBlocks(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc(`
		<meta property="og:site_name" content="Example Site">
		<meta property="og:image" content="/lead.jpg">
		<meta name="author" content="Jane Doe">
		<meta property="article:published_time" content="2026-10-01T09:30:00-04:00">`,
		`<h1>A Test Post</h1>
		<p>First paragraph with <a href="/x">a link</a> and <em>emphasis</em>. `+filler+`</p>
		<h2>Section heading</h2>
		<p>Second   paragraph
		   spans source lines.&nbsp;`+filler+`</p>
		<ul><li>Item one</li><li>Item <b>two</b></li></ul>
		<blockquote><p>Quoted words of wisdom.</p></blockquote>`))

	if a.Title != "A Test Post" || a.Byline != "Jane Doe" || a.SiteName != "Example Site" || a.Language != "en" {
		t.Errorf("metadata = %q / %q / %q / %q", a.Title, a.Byline, a.SiteName, a.Language)
	}
	if a.LeadImage != "https://example.com/lead.jpg" {
		t.Errorf("LeadImage = %q", a.LeadImage)
	}
	if want := time.Date(2026, 10, 1, 13, 30, 0, 0, time.UTC); !a.PublishedAt.Equal(want) || a.PublishedAt.Location() != time.UTC {
		t.Errorf("PublishedAt = %v, want %v in UTC", a.PublishedAt, want)
	}
	want := []Block{
		{Kind: KindParagraph, Text: "First paragraph with a link and emphasis. " + filler},
		{Kind: KindHeading, Text: "Section heading"},
		{Kind: KindParagraph, Text: "Second paragraph spans source lines. " + filler},
		{Kind: KindListItem, Text: "Item one"},
		{Kind: KindListItem, Text: "Item two"},
		{Kind: KindQuote, Text: "Quoted words of wisdom."},
	}
	if len(a.Blocks) != len(want) {
		t.Fatalf("blocks:\n%q\nwant:\n%q", a.Blocks, want)
	}
	for i := range want {
		if a.Blocks[i] != want[i] {
			t.Errorf("block %d = %+v, want %+v", i, a.Blocks[i], want[i])
		}
	}
	if !strings.Contains(a.SpokenText(), "\n\nSection heading\n\n") {
		t.Errorf("SpokenText should separate blocks with blank lines: %q", a.SpokenText())
	}
}

// readability renames elements by setting only node.Data (e.g. a layout
// table becomes a div) and creates new nodes without a DataAtom; the walker
// must go by tag name. A Paul Graham-style page exercises both.
func TestExtractLayoutTableAndBrParagraphs(t *testing.T) {
	t.Parallel()
	page := `<html><head><title>Essay</title></head><body>
		<table><tr><td><img src="/nav.gif" width="69" height="357"></td>
		<td><font size="2">` + filler + `<br><br>Second paragraph after a double break. ` + filler +
		`<br><br>Third paragraph. ` + filler + `</font></td></tr></table></body></html>`
	a := mustExtract(t, page)
	got := texts(a)
	if len(got) < 3 || !strings.HasPrefix(got[len(got)-2], "Second paragraph") || !strings.HasPrefix(got[len(got)-1], "Third paragraph") {
		t.Errorf("blocks = %q, want separate paragraphs at <br><br>", got)
	}
	for _, b := range a.Blocks {
		if b.Kind == KindTable {
			t.Errorf("layout table treated as data: %q", b.Text)
		}
	}
}

func TestExtractSingleBrIsALineBreak(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><p>Roses are red,<br>violets are blue.</p><p>`+filler+`</p>`))
	if !hasBlock(a, KindParagraph, "Roses are red,\nviolets are blue.") {
		t.Errorf("blocks = %q", texts(a))
	}
}

func TestExtractInlineSpacing(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p><b>foo</b>bar and <i>x</i> <i>y</i>. `+filler+`</p>
		<center>Alpha</center><center>Beta</center><p>`+filler+`</p>`))
	got := strings.Join(texts(a), "|")
	if !strings.Contains(got, "foobar and x y.") {
		t.Errorf("inline text spacing wrong: %q", got)
	}
	if strings.Contains(got, "AlphaBeta") {
		t.Errorf("unknown block elements glued together: %q", got)
	}
}

func TestExtractCodeTablesAndPre(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p>
		<pre><code>func main() {
	fmt.Println("hi")
}</code></pre>
		<p>Middle with <code>inline()</code> code. `+filler+`</p>
		<table><tr><th>Name</th><th>Qty</th></tr><tr><td>Apples</td><td>3</td></tr></table>
		<pre>The woods are lovely,
dark and deep.</pre>
		<p>End. `+filler+`</p>`))

	if !hasBlock(a, KindCode, "func main() {\n\tfmt.Println(\"hi\")\n}") {
		t.Errorf("code block not kept: %q", a.Blocks)
	}
	if !hasBlock(a, KindTable, "Name | Qty\nApples | 3") {
		t.Errorf("data table not kept: %q", a.Blocks)
	}
	if !hasBlock(a, KindPreformatted, "The woods are lovely,\ndark and deep.") {
		t.Errorf("<pre> prose should be preformatted, not code: %q", a.Blocks)
	}
	spoken := a.SpokenText()
	for _, want := range []string{CodeOmitted, TableOmitted, "Middle with inline() code.", "The woods are lovely,"} {
		if !strings.Contains(spoken, want) {
			t.Errorf("SpokenText missing %q", want)
		}
	}
	if strings.Contains(spoken, "fmt.Println") || strings.Contains(spoken, "Apples") {
		t.Error("code or table content leaked into SpokenText")
	}
	// Code and tables don't count as words.
	for _, b := range a.Blocks {
		if b.Kind == KindCode || b.Kind == KindTable {
			if b.Kind.Spoken() {
				t.Errorf("%s should not be spoken", b.Kind)
			}
		}
	}
}

func TestExtractImages(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>Before the chart. `+filler+`</p>
		<figure><img src="/img/chart.png" alt="  Growth   chart "><figcaption>Figure 1:
		Growth over time.</figcaption></figure>
		<p>After the chart. `+filler+`</p>
		<img src="https://example.com/placeholder.png" data-src="https://cdn.example.com/lazy.jpg">
		<img srcset="https://res.cloudinary.com/demo/image/upload/w_480,c_fill/a.jpg 480w, https://res.cloudinary.com/demo/image/upload/w_1080,c_fill/a.jpg 1080w" alt="responsive">
		<img src="/pixel.gif" width="1" height="1">
		<img src="data:image/png;base64,AAAA">
		<img src="/img/chart.png" alt="duplicate">
		<img src="javascript:alert(1)">
		<img src="//cdn.example.com/proto.png">
		<p>Closing. `+filler+`</p>`))

	if len(a.Images) != 4 {
		t.Fatalf("images = %+v, want 4 (chart, lazy, responsive, protocol-relative)", a.Images)
	}
	chart := a.Images[0]
	if chart.URL != "https://example.com/img/chart.png" || chart.Alt != "Growth chart" ||
		chart.Caption != "Figure 1: Growth over time." {
		t.Errorf("chart = %+v", chart)
	}
	if !strings.HasPrefix(chart.Context, "Before the chart.") || !strings.Contains(chart.Context, " … After the chart.") {
		t.Errorf("chart context = %q", chart.Context)
	}
	if a.Blocks[chart.Block].Text[:16] != "After the chart." {
		t.Errorf("chart.Block = %d points at %q", chart.Block, a.Blocks[chart.Block].Text)
	}
	if a.Images[1].URL != "https://cdn.example.com/lazy.jpg" {
		t.Errorf("lazy image = %+v (data-src should win over a placeholder src)", a.Images[1])
	}
	if a.Images[2].URL != "https://res.cloudinary.com/demo/image/upload/w_1080,c_fill/a.jpg" {
		t.Errorf("srcset image = %+v (want the largest, commas intact)", a.Images[2])
	}
	if a.Images[3].URL != "https://cdn.example.com/proto.png" {
		t.Errorf("protocol-relative image = %+v", a.Images[3])
	}
	if strings.Contains(a.SpokenText(), "Figure 1") {
		t.Error("image caption leaked into the spoken text")
	}
}

func TestExtractContextAtEdges(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<img src="/first.png"><p>`+filler+`</p><p>`+filler+`</p><img src="/last.png">`))
	if len(a.Images) != 2 {
		t.Fatalf("images = %+v", a.Images)
	}
	if a.Images[0].Block != 0 || strings.Contains(a.Images[0].Context, "…") && !strings.HasPrefix(a.Images[0].Context, "This") {
		t.Errorf("first image = %+v", a.Images[0])
	}
	if a.Images[1].Block != len(a.Blocks) {
		t.Errorf("last image Block = %d, want %d", a.Images[1].Block, len(a.Blocks))
	}
}

func TestExtractContextIsBounded(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 200)
	a := mustExtract(t, doc("", `<p>`+long+`</p><img src="/a.png"><p>`+long+`</p>`))
	if len(a.Images) != 1 {
		t.Fatalf("images = %+v", a.Images)
	}
	if n := len(a.Images[0].Context); n > 2*contextChars+20 {
		t.Errorf("context is %d bytes, want about %d at most", n, 2*contextChars)
	}
}

// Emoji and Wikipedia math fallback images are inline text, not article
// images; <math> uses its alt text.
func TestExtractInlineImagesAndMath(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>Euler's identity <img class="mwe-math-fallback-image-inline" src="/m.svg" alt="e^{i\pi}+1=0"> is famous.
				Also <math alttext="{\displaystyle x^2}"><mi>x</mi></math> and <math alttext="y"><mi>y</mi></math> and a smile <img class="emoji" src="/e.png" alt="&#x1F600;">. `+filler+`</p>`))
	if len(a.Images) != 0 {
		t.Errorf("inline images counted as article images: %+v", a.Images)
	}
	b := a.Blocks[0]
	// TeX is spoken as a stand-in; plain-text math as itself. Source keeps
	// the originals for the LLM step.
	for _, want := range []string{"identity (formula) is famous", "Also (formula) and y and", "smile \U0001F600."} {
		if !strings.Contains(b.Text, want) {
			t.Errorf("spoken text missing %q in %q", want, b.Text)
		}
	}
	for _, want := range []string{`identity e^{i\pi}+1=0 is famous`, "Also x^2 and y and"} {
		if !strings.Contains(b.Source, want) {
			t.Errorf("source missing %q in %q", want, b.Source)
		}
	}
}

func TestExtractSkipsMediaAndFootnotes(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>A claim<sup><a href="#fn1">1</a></sup> and another<sup>[<a href="#cite-2">2</a>]</sup>. `+filler+`</p>
		<video src="/v.mp4">Your browser does not support the video tag.</video>
		<audio src="/a.mp3">No audio support.</audio>
		<p>E = mc<sup>2</sup> stays. `+filler+`</p>`))
	got := a.SpokenText()
	if strings.Contains(got, "claim1") || strings.Contains(got, "[2]") || strings.Contains(got, "another2") {
		t.Errorf("footnote markers read aloud: %q", got)
	}
	if strings.Contains(got, "does not support") || strings.Contains(got, "No audio") {
		t.Errorf("media fallback text read aloud: %q", got)
	}
	if !strings.Contains(got, "E = mc2 stays.") {
		t.Errorf("a real superscript was dropped: %q", got)
	}
}

func TestExtractCaptions(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p>
		<figure><blockquote><p>All the world's a stage.</p></blockquote><figcaption>William Shakespeare</figcaption></figure>
		<div class="wp-caption"><img src="/photo.jpg" alt="Protest"><p class="wp-caption-text">Photo: Jane Doe for Reuters</p></div>
		<p>`+filler+`</p>`))
	if !hasBlock(a, KindCaption, "William Shakespeare") {
		t.Errorf("quote attribution lost: %q", a.Blocks)
	}
	if len(a.Images) != 1 || a.Images[0].Caption != "Photo: Jane Doe for Reuters" {
		t.Errorf("WordPress caption not attached: %+v", a.Images)
	}
	if strings.Contains(a.SpokenText(), "Jane Doe for Reuters") {
		t.Error("WordPress caption read aloud")
	}
}

func TestExtractCleansText(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", "<p>Bad \xff byte, Super\u200bcalifragilistic, soft\u00adware, word\u2060joined. "+filler+"</p>"))
	got := texts(a)[0]
	for _, want := range []string{"Bad \uFFFD byte,", "Supercalifragilistic,", "software,", "wordjoined."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

// Input is treated as UTF-8 regardless of a (stale) <meta charset>.
func TestExtractTreatsInputAsUTF8(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc(`<meta charset="windows-1252">`, "<p>It’s café time. "+filler+"</p>"))
	if !strings.Contains(a.SpokenText(), "It’s café time.") {
		t.Errorf("text decoded twice: %q", a.SpokenText()[:40])
	}
}

func TestExtractExcerptIsCapped(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+strings.Repeat(filler+" ", 20)+`</p>`))
	if len(a.Excerpt) > excerptChars+10 {
		t.Errorf("excerpt is %d bytes", len(a.Excerpt))
	}
}

func TestExtractNoContent(t *testing.T) {
	t.Parallel()
	for name, page := range map[string]string{
		"empty":       "",
		"short":       doc("", "<p>Just a few words here.</p>"),
		"only chrome": `<html><body><nav>Home</nav><footer>Copyright</footer></body></html>`,
		"only code":   doc("", "<p>See:</p>"+strings.Repeat("<pre><code>x := 1\ny := 2\nz := 3\nw := 4</code></pre><p>and</p>", 6)),
	} {
		if _, err := Extract([]byte(page), pageURL); !errors.Is(err, ErrNoContent) {
			t.Errorf("%s: err = %v, want ErrNoContent", name, err)
		}
	}
}

func TestCountWords(t *testing.T) {
	t.Parallel()
	tests := map[string]int{
		"":               0,
		"one two  three": 3,
		"日本語の文章です":       4, // 8 CJK chars ~ 4 words
		"mixed 日本 text":  3,
	}
	for in, want := range tests {
		if got := countWords(in); got != want {
			t.Errorf("countWords(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLargestSrcset(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"":                                      "",
		"/a.jpg":                                "/a.jpg",
		"/a.jpg 1x, /b.jpg 2x":                  "/b.jpg",
		"/s.jpg 480w,/l.jpg 1080w, /m.jpg 800w": "/l.jpg",
		"https://x.com/w_40,h_40/a.jpg 40w, https://x.com/w_400,h_400/a.jpg 400w": "https://x.com/w_400,h_400/a.jpg",
		"data:image/gif;base64,AAA 1x, /real.jpg 2x":                              "/real.jpg",
	}
	for in, want := range tests {
		if got := largestSrcset(in); got != want {
			t.Errorf("largestSrcset(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeadTail(t *testing.T) {
	t.Parallel()
	if got := head("short", 10); got != "short" {
		t.Errorf("head short = %q", got)
	}
	if got := head("alpha beta gamma delta", 12); got != "alpha beta…" {
		t.Errorf("head = %q", got)
	}
	if got := tail("alpha beta gamma delta", 12); got != "…gamma delta" {
		t.Errorf("tail = %q", got)
	}
	s := strings.Repeat("é", 20) // 40 bytes; never split a rune
	for n := 1; n < 40; n++ {
		for _, out := range []string{head(s, n), tail(s, n)} {
			if strings.ContainsRune(out, '\uFFFD') {
				t.Errorf("n=%d split a rune: %q", n, out)
			}
		}
	}
}

func TestExtractCodeKeepsIndentation(t *testing.T) {
	t.Parallel()
	code := "\n    def f(x):\n        if x:\n            return 1\n\n        return 2   \n\n"
	a := mustExtract(t, doc("", "<p>"+filler+"</p><pre><code>"+code+"</code></pre><pre>First stanza line\nsecond line\n\nSecond stanza</pre><p>"+filler+"</p>"))
	if !hasBlock(a, KindCode, "def f(x):\n    if x:\n        return 1\n\n    return 2") {
		t.Errorf("code indentation/blank lines lost: %q", a.Blocks)
	}
	if !hasBlock(a, KindPreformatted, "First stanza line\nsecond line\n\nSecond stanza") {
		t.Errorf("stanza break lost: %q", a.Blocks)
	}
}

// Inline emoji/math images inside a table or <pre> belong to that block's
// text, not to the next paragraph.
func TestExtractInlineImagesStayInTables(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p>
		<table><tr><th>Name</th><th>Mood</th></tr><tr><td>Ann</td><td><img class="emoji" src="/e.png" alt="X"></td></tr>
		<tr><td>Bo</td><td><img class="mwe-math-fallback-image-inline" src="/m.svg" alt="{\displaystyle a^2}"></td></tr></table>
		<p>`+filler+`</p>`))
	var table Block
	for _, b := range a.Blocks {
		if b.Kind == KindTable {
			table = b
		}
	}
	if table.Text != "Name | Mood\nAnn | X\nBo | (formula)" || table.Source != "Name | Mood\nAnn | X\nBo | a^2" {
		t.Errorf("table = %+v", table)
	}
	for _, b := range a.Blocks {
		if b.Kind == KindParagraph && (b.Text == "X" || b.Text == "(formula)") {
			t.Errorf("inline image text leaked out of the table: %q", texts(a))
		}
	}
	if len(a.Images) != 0 {
		t.Errorf("inline images counted: %+v", a.Images)
	}
}

func TestExtractInlineCustomElements(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>Where <mjx-container class="MathJax"><mjx-math>junk glyphs</mjx-math><mjx-assistive-mml><math alttext="x"><mi>x</mi></math></mjx-assistive-mml></mjx-container> is the input and <nobr>no break</nobr> here, via <my-widget>a widget</my-widget>. `+filler+`</p>`))
	if got := a.Blocks[0].Text; !strings.HasPrefix(got, "Where x is the input and no break here, via a widget.") {
		t.Errorf("custom/inline elements split the paragraph: %q", texts(a))
	}
}

// Notion/Confluence-style exports wrap every cell in <p>; header cells still
// mark it as data.
func TestExtractDataTableWithParagraphCells(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p>
		<table><tr><th><p>Option</p></th><th><p>Cost</p></th></tr><tr><td><p>Basic</p></td><td><p>$5</p></td></tr></table>
		<p>`+filler+`</p>`))
	if !hasBlock(a, KindTable, "Option | Cost\nBasic | $5") {
		t.Errorf("data table with <p> cells lost: %q", a.Blocks)
	}
}

func TestExtractHeadingWrappingBlocks(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><h2><a href="#s"><div>Section title</div></a></h2><p>`+filler+`</p>`))
	if !hasBlock(a, KindHeading, "Section title") {
		t.Errorf("heading kind not inherited: %q", a.Blocks)
	}
}

func TestExtractAbsoluteFootnoteLinks(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>A claim<sup><a href="https://example.com/posts/1#fn1">1</a></sup> and a cite<sup>
		[<a href="/posts/1#fn2">2</a>]&nbsp;</sup>, but<sup><a href="https://other.example/x#y">3</a></sup> stays. `+filler+`</p>`))
	got := a.Blocks[0].Text
	if strings.Contains(got, "claim1") || strings.Contains(got, "[2]") || strings.Contains(got, "cite2") {
		t.Errorf("same-page footnote markers read aloud: %q", got)
	}
	if !strings.Contains(got, "but3 stays") {
		t.Errorf("a link to another page was dropped: %q", got)
	}
}

func TestExtractNestedFigcaptionNotSpoken(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><figure><img src="/f.jpg"><div><figcaption>Nested caption</figcaption></div></figure><p>`+filler+`</p>`))
	if len(a.Images) != 1 || a.Images[0].Caption != "Nested caption" {
		t.Errorf("images = %+v", a.Images)
	}
	if strings.Contains(a.SpokenText(), "Nested caption") {
		t.Error("nested figcaption read aloud as well as attached")
	}
}

func TestExtractWPCaptionAfterInlineImage(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><p>Look at this photo <img src="/photo.jpg"></p><p class="wp-caption-text">Photo: Jane Doe</p><p>`+filler+`</p>`))
	if len(a.Images) != 1 || a.Images[0].Caption != "Photo: Jane Doe" {
		t.Errorf("images = %+v", a.Images)
	}
	if strings.Contains(a.SpokenText(), "Jane Doe") {
		t.Error("caption read aloud")
	}
}

func TestExtractInlineAltTextIsTextForBr(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><p>Line one<br><img class="emoji" src="/e.png" alt="X"><br>Line two</p><p>`+filler+`</p>`))
	if !hasBlock(a, KindParagraph, "Line one\nX\nLine two") {
		t.Errorf("emoji between single <br>s caused a paragraph break: %q", texts(a))
	}
}

func TestExtractSkipsTitleBannerImage(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<img src="/title.gif" alt="A Test Post"><p>`+filler+`</p><img src="/real.jpg" alt="Real"><p>`+filler+`</p>`))
	if len(a.Images) != 1 || a.Images[0].Alt != "Real" {
		t.Errorf("images = %+v (title banner should be skipped)", a.Images)
	}
}

func TestExtractDropsPermalinkAnchors(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><h2>Basic Tutorial<a class="headerlink" href="#basic">`+"\u00b6"+`</a></h2><h3>Next <a href="#n">#</a></h3><p>`+filler+`</p>`))
	if !hasBlock(a, KindHeading, "Basic Tutorial") || !hasBlock(a, KindHeading, "Next") {
		t.Errorf("permalink symbols kept: %q", texts(a))
	}
}

// readability deletes elements whose class looks like page clutter, such as
// "comment"; syntax-highlighted code comments must survive.
func TestExtractKeepsHighlightedCodeComments(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><div class="code"><pre><span class="comment">// Set up the pipeline.</span>
c := gen(2, 3)<span class="comment"> // 4</span></pre></div><p>`+filler+`</p>`))
	if !hasBlock(a, KindCode, "// Set up the pipeline.\nc := gen(2, 3) // 4") {
		t.Errorf("code comments lost or block not code: %q", a.Blocks)
	}
}

func TestExtractClassifiesHighlightedPreAsCode(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p>
		<div class="highlight-python3 notranslate"><div class="highlight"><pre>import logging</pre></div></div>
		<pre class="language-go">x := 1</pre>
		<pre>A plain verse,
not code.</pre><p>`+filler+`</p>`))
	if !hasBlock(a, KindCode, "import logging") || !hasBlock(a, KindCode, "x := 1") {
		t.Errorf("highlighted <pre> not code: %q", a.Blocks)
	}
	if !hasBlock(a, KindPreformatted, "A plain verse,\nnot code.") {
		t.Errorf("plain <pre> should stay preformatted: %q", a.Blocks)
	}
}

func TestExtractRemovesKnownChrome(t *testing.T) {
	t.Parallel()
	page := `<html><head><title>A Test Post</title></head><body class="mediawiki skin-vector"><main>
		<div class="shortdescription">Hidden short description</div>
		<div id="contentSub"><span class="mw-redirectedfrom">(Redirected from X)</span></div>
		<p>` + filler + `</p>
		<div class="mw-heading mw-heading2"><h2 id="History">History</h2><span class="mw-editsection">[<a href="/edit">edit</a>]</span></div>
		<p>` + filler + `</p>
		<div class="reflist"><ol class="references"><li>Dunham, 1999, p. xxiv.</li></ol></div></main></body></html>`
	a := mustExtract(t, page)
	spoken := a.SpokenText()
	for _, junk := range []string{"Hidden short description", "Redirected from", "edit", "Dunham"} {
		if strings.Contains(spoken, junk) {
			t.Errorf("chrome %q read aloud: %q", junk, spoken)
		}
	}
	if !hasBlock(a, KindHeading, "History") {
		t.Errorf("Wikipedia heading lost: %q", texts(a))
	}

	mdn := mustExtract(t, doc("", `<p>`+filler+`</p><div class="code-example"><div class="example-header"><span class="language-name">http</span></div><pre class="brush: http notranslate">GET / HTTP/1.1</pre></div><p>`+filler+`</p>`))
	if strings.Contains(mdn.SpokenText(), "\n\nhttp\n\n") || !hasBlock(mdn, KindCode, "GET / HTTP/1.1") {
		t.Errorf("MDN code example: %q", mdn.Blocks)
	}
}

// The MediaWiki and MDN class lists apply only on those sites: elsewhere a
// "tagline" subtitle or a "references" wrapper is real content.
func TestExtractChromeClassesAreScoped(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p class="tagline">Why the budget fight matters for every resident of the city.</p><p>`+filler+`</p>
		<div class="references"><p>Smith argues the point at length in his book. `+filler+`</p></div><p>`+filler+`</p>`))
	spoken := a.SpokenText()
	for _, want := range []string{"Why the budget fight matters", "Smith argues"} {
		if !strings.Contains(spoken, want) {
			t.Errorf("non-Wikipedia/MDN content %q removed: %q", want, texts(a))
		}
	}
}

func TestExtractCodeDetectionIsScoped(t *testing.T) {
	t.Parallel()
	verse := "<pre>Roses are red,\nviolets are blue.</pre>"
	for name, page := range map[string]string{
		"lang class on body": `<html><head><title>T</title></head><body class="lang-en"><article><p>` + filler + `</p>` + verse + `<p>` + filler + `</p></article></body></html>`,
		"language wrapper":   doc("", `<div class="page language-fr"><p>`+filler+`</p>`+verse+`<p>`+filler+`</p></div>`),
		"highlights box":     doc("", `<p>`+filler+`</p><div class="post-highlights">`+verse+`</div><p>`+filler+`</p>`),
	} {
		a := mustExtract(t, page)
		if !hasBlock(a, KindPreformatted, "Roses are red,\nviolets are blue.") {
			t.Errorf("%s: verse misclassified: %q", name, a.Blocks)
		}
	}
	a := mustExtract(t, doc("", `<p>`+filler+`</p><pre class="hljs">a := 1</pre><div class="codehilite"><pre>b = 2</pre></div>
		<div class="wp-block-code"><pre>c = 3</pre></div><pre><code class="language-go">d := 4</code></pre><p>`+filler+`</p>`))
	for _, code := range []string{"a := 1", "b = 2", "c = 3", "d := 4"} {
		if !hasBlock(a, KindCode, code) {
			t.Errorf("%q not detected as code: %q", code, a.Blocks)
		}
	}
}

// Classes the walker needs survive the highlighting strip inside <pre>.
func TestExtractEmojiInPre(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><pre>I am happy <img class="emoji" src="/e.png" alt="X"> today</pre><p>`+filler+`</p>`))
	if !hasBlock(a, KindPreformatted, "I am happy X today") || len(a.Images) != 0 {
		t.Errorf("emoji in <pre>: blocks %q, images %+v", a.Blocks, a.Images)
	}
}

func TestExtractAbsoluteSamePagePermalink(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>`+filler+`</p><h2>Intro <a href="https://example.com/posts/1#intro">#</a></h2><p>`+filler+`</p>`))
	if !hasBlock(a, KindHeading, "Intro") {
		t.Errorf("absolute same-page permalink kept: %q", texts(a))
	}
}

func TestExtractPermalinkMustBeSamePageAnchor(t *testing.T) {
	t.Parallel()
	a := mustExtract(t, doc("", `<p>See <a href="/usc/230">`+"\u00a7"+`</a> 230 for details. `+filler+`</p>`))
	if !strings.Contains(texts(a)[0], "See \u00a7 230") {
		t.Errorf("a real section-sign link was dropped: %q", texts(a)[0])
	}
}

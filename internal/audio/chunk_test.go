package audio

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// checkChunks asserts the invariants every Chunk result must satisfy: within
// the limit (except an unsplittable single rune), trimmed, non-empty, valid
// UTF-8, and the input's words preserved in order, neither merged nor
// altered (a word may only be split across consecutive pieces).
func checkChunks(t *testing.T, text string, max int, chunks []string) {
	t.Helper()
	for i, c := range chunks {
		if len(c) > max && utf8.RuneCountInString(c) != 1 {
			t.Errorf("chunk %d is %d bytes, over %d", i, len(c), max)
		}
		if c == "" || c != strings.TrimSpace(c) {
			t.Errorf("chunk %d not trimmed or empty: %q", i, c)
		}
		if !utf8.ValidString(c) {
			t.Errorf("chunk %d splits a UTF-8 sequence: %q", i, c)
		}
	}
	out := strings.Fields(strings.Join(chunks, " "))
	j := 0
	for _, word := range strings.Fields(text) {
		acc := ""
		for acc != word {
			if j >= len(out) || !strings.HasPrefix(word, acc+out[j]) {
				t.Fatalf("word %q lost, merged or altered (output so far %q)", word, out[:min(j+1, len(out))])
			}
			// A word may only be split if it doesn't fit, or (CJK text
			// having no spaces) after a sentence end.
			if acc != "" && len(word) <= max && !endsCJKSentence(acc) {
				t.Fatalf("word %q split although it fits in %d bytes", word, max)
			}
			acc += out[j]
			j++
		}
	}
	if j != len(out) {
		t.Errorf("extra output words: %q", out[j:])
	}
}

func TestChunkPacksParagraphs(t *testing.T) {
	t.Parallel()
	text := "First paragraph.\n\nSecond paragraph.\n\nThird paragraph is a bit longer than the others."
	got := Chunk(text, 50)
	want := []string{"First paragraph.\n\nSecond paragraph.", "Third paragraph is a bit longer than the others."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Chunk = %q, want %q", got, want)
	}
	checkChunks(t, text, 50, got)
	if all := Chunk(text, 1000); len(all) != 1 || all[0] != text {
		t.Errorf("text under the limit should be one chunk: %q", all)
	}
}

func TestChunkSplitsLongParagraphAtSentences(t *testing.T) {
	t.Parallel()
	text := `Short intro.

One sentence here. Another one follows! A question? "A quoted end." Last bit without a period`
	got := Chunk(text, 40)
	checkChunks(t, text, 40, got)
	for _, c := range got[1:] {
		if !strings.HasSuffix(c, ".") && !strings.HasSuffix(c, "!") && !strings.HasSuffix(c, "?") &&
			!strings.HasSuffix(c, `."`) && c != "Last bit without a period" {
			t.Errorf("chunk %q doesn't end at a sentence boundary", c)
		}
	}
	if got[0] != "Short intro." {
		t.Errorf("a short paragraph before an oversized one should stand alone: %q", got)
	}
}

func TestChunkSplitsLongSentenceAtWords(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("word ", 30) + "end"
	got := Chunk(text, 23)
	checkChunks(t, text, 23, got)
	for _, c := range got {
		if strings.Contains(c, "wor ") || strings.HasPrefix(c, "d ") {
			t.Errorf("word split mid-word: %q", c)
		}
	}
}

func TestChunkHardSplitsOverlongWordsOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	text := "see https://example.com/" + strings.Repeat("é", 30) + " ok"
	for _, max := range []int{5, 7, 10} {
		checkChunks(t, text, max, Chunk(text, max))
	}
	// Even a limit below one rune's size makes progress.
	got := Chunk("éé", 1)
	if strings.Join(got, "") != "éé" {
		t.Errorf("tiny limit = %q", got)
	}
}

func TestChunkEmptyAndWhitespace(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"", "   ", "\n\n\n", " \n\n \t "} {
		if got := Chunk(text, 10); len(got) != 0 {
			t.Errorf("Chunk(%q) = %q, want none", text, got)
		}
	}
}

func TestChunkPanicsOnBadLimit(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("expected a panic for maxChars <= 0")
		}
	}()
	Chunk("x", 0)
}

// FuzzChunk checks the invariants on arbitrary input.
// Run: go test ./internal/audio -run '^$' -fuzz FuzzChunk -fuzztime 1000000x
func FuzzChunk(f *testing.F) {
	f.Add("Hello there. General Kenobi!\n\nYou are a bold one.", 12)
	f.Add(strings.Repeat("日本", 50), 7)
	f.Add("a\n\n\n\nb. c! d? e", 1)
	f.Add("Dr. X met J. Y in the U.S. e.g. here. \u3002\u3002", 9)
	f.Add("one\r\n\r\n two \n \n three", 4)
	f.Fuzz(func(t *testing.T, text string, max int) {
		if max <= 0 || max > 10000 || !utf8.ValidString(text) {
			return
		}
		checkChunks(t, text, max, Chunk(text, max))
	})
}

func TestSplitSentencesSkipsFalseEnds(t *testing.T) {
	t.Parallel()
	text := `Dr. Smith met Mr. Jones in the U.S. last year, e.g. in Washington. Prices rose 3.5 percent... then fell. He asked "why?" and left. J. R. R. Tolkien wrote it. "Done." Next one!` +
		" See p. 5, as Smith et al. (2019) agree. Item No. 5 is here. I said no. Then\u00a0a no-break space."
	got := splitSentences(text)
	want := []string{
		"Dr. Smith met Mr. Jones in the U.S. last year, e.g. in Washington.",
		"Prices rose 3.5 percent... then fell.",
		`He asked "why?" and left.`,
		"J. R. R. Tolkien wrote it.",
		`"Done."`,
		"Next one!",
		"See p. 5, as Smith et al. (2019) agree.",
		"Item No. 5 is here.",
		"I said no.",
		"Then\u00a0a no-break space.",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitSentences =\n%q\nwant\n%q", got, want)
	}
}

func TestSplitSentencesCJK(t *testing.T) {
	t.Parallel()
	text := "\u65e5\u672c\u8a9e\u306e\u6587\u3067\u3059\u3002\u300c\u3053\u3093\u306b\u3061\u306f\u3002\u300d\u7d42\u308f\u308a\uff01"
	got := splitSentences(text)
	want := []string{"\u65e5\u672c\u8a9e\u306e\u6587\u3067\u3059\u3002", "\u300c\u3053\u3093\u306b\u3061\u306f\u3002\u300d", "\u7d42\u308f\u308a\uff01"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitSentences = %q, want %q", got, want)
	}
	// Chunking a long CJK paragraph cuts at sentence ends, not mid-word.
	para := strings.Repeat(want[0], 10)
	for _, c := range Chunk(para, len(want[0])*3) {
		if !strings.HasSuffix(c, "\u3002") || strings.Contains(c, " ") {
			t.Errorf("CJK chunk %q doesn't end at a sentence or has a space added", c)
		}
	}
}

func TestParagraphsAcceptCRLFAndBlankLines(t *testing.T) {
	t.Parallel()
	got := paragraphs("one\r\n\r\ntwo\n \t\nthree\nstill three")
	want := []string{"one", "two", "three\nstill three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("paragraphs = %q, want %q", got, want)
	}
}

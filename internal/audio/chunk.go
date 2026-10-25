// Package audio prepares text for speech synthesis and assembles the
// resulting audio: it splits a script into chunks a TTS engine accepts, joins
// the synthesized chunks into one MP3 with ffmpeg, and measures durations
// with ffprobe.
package audio

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// sentenceEnd matches a candidate sentence end: Western terminal punctuation
// (plus closing quotes or brackets) followed by whitespace, or CJK terminal
// punctuation, which is followed directly by the next sentence. Whitespace
// includes Unicode spaces such as the no-break spaces common in extracted
// HTML (\s alone is ASCII-only).
var sentenceEnd = regexp.MustCompile(`[.!?\x{2026}]+["'\x{201D}\x{2019})\]\x{00BB}\x{300D}\x{300F}]*[\s\p{Zs}]+|[\x{3002}\x{FF01}\x{FF1F}\x{FF1B}]+[\x{300D}\x{300F}\x{FF09}"\x{201D}]*`)

// abbreviations end with a period that doesn't end a sentence (compared
// lowercased, without the final period).
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "st": true, "jr": true, "sr": true,
	"vs": true, "etc": true, "e.g": true, "i.e": true, "cf": true, "approx": true,
	"fig": true, "vol": true, "dept": true, "inc": true, "ltd": true, "co": true, "corp": true,
	"u.s": true, "u.k": true, "a.m": true, "p.m": true, "ph.d": true, "jan": true, "feb": true,
	"mar": true, "apr": true, "jun": true, "jul": true, "aug": true, "sep": true, "sept": true,
	"oct": true, "nov": true, "dec": true, "p": true, "pp": true, "al": true, "ch": true,
	"sec": true, "eq": true, "ed": true, "eds": true, "est": true, "gen": true, "gov": true,
	"sen": true, "rep": true, "rev": true, "mt": true, "ft": true, "ave": true,
}

// numberAbbreviations abbreviate only before a number: "No. 5", but "I said
// no." ends a sentence.
var numberAbbreviations = map[string]bool{"no": true, "nos": true, "nr": true}

// Chunk splits text into pieces of at most maxBytes bytes for synthesis,
// cutting at the most natural boundary that fits: between paragraphs (blank
// lines), then between sentences, then between words, and only as a last
// resort inside a word (never inside a UTF-8 sequence; a single rune larger
// than maxBytes becomes its own chunk). Paragraphs are packed together while
// they fit, so a chunk boundary is usually also a paragraph boundary. Chunks
// are trimmed and never empty.
//
// The limit is in bytes, which is at least the character count, so it is
// safe for APIs that limit characters.
func Chunk(text string, maxBytes int) []string {
	if maxBytes <= 0 {
		panic("audio.Chunk: maxBytes must be positive")
	}
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			chunks = append(chunks, s)
		}
		cur.Reset()
	}
	add := func(piece, sep string) {
		if cur.Len() > 0 && cur.Len()+len(sep)+len(piece) > maxBytes {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString(sep)
		}
		cur.WriteString(piece)
	}

	for _, para := range paragraphs(text) {
		if len(para) <= maxBytes {
			add(para, "\n\n")
			continue
		}
		// An oversized paragraph starts its own chunk sequence.
		flush()
		for _, sentence := range splitSentences(para) {
			if len(sentence) <= maxBytes {
				add(sentence, sentenceSep(cur.String()))
				continue
			}
			flush()
			for _, piece := range splitWords(sentence, maxBytes) {
				add(piece, " ")
			}
		}
		flush()
	}
	flush()
	return chunks
}

// paragraphs splits text at blank lines (lines that are empty after
// trimming), accepting CRLF line endings.
func paragraphs(text string) []string {
	var out []string
	var cur []string
	flush := func() {
		if p := strings.TrimSpace(strings.Join(cur, "\n")); p != "" {
			out = append(out, p)
		}
		cur = cur[:0]
	}
	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

// splitSentences splits a paragraph after each sentence end, skipping false
// ends: a period after an abbreviation or an initial ("Dr.", "J."), and any
// break followed by a lowercase letter ("e.g. in", "\"why?\" he asked").
func splitSentences(para string) []string {
	var out []string
	start := 0
	for _, m := range sentenceEnd.FindAllStringIndex(para, -1) {
		if !isSentenceEnd(para, start, m[0], m[1]) {
			continue
		}
		if s := strings.TrimSpace(para[start:m[1]]); s != "" {
			out = append(out, s)
		}
		start = m[1]
	}
	if s := strings.TrimSpace(para[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// isSentenceEnd decides whether the punctuation match para[at:end] really
// ends a sentence that began at start.
func isSentenceEnd(para string, start, at, end int) bool {
	r, _ := utf8.DecodeRuneInString(para[at:])
	if r != '.' && r != '!' && r != '?' && r != '\u2026' {
		return true // CJK terminator
	}
	if next, _ := utf8.DecodeRuneInString(para[end:]); unicode.IsLower(next) {
		return false
	}
	if r == '.' {
		word := lastWord(para[start:at])
		abbr := strings.ToLower(strings.TrimSuffix(word, "."))
		if abbreviations[abbr] {
			return false
		}
		if next, _ := utf8.DecodeRuneInString(strings.TrimLeft(para[end:], " ")); numberAbbreviations[abbr] && unicode.IsDigit(next) {
			return false
		}
		if n := utf8.RuneCountInString(word); n == 1 && unicode.IsUpper([]rune(word)[0]) {
			return false // an initial, as in "J. Smith"
		}
	}
	return true
}

// lastWord returns the final whitespace-separated word of s, without
// leading punctuation such as opening brackets or quotes.
func lastWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimLeft(fields[len(fields)-1], "(\"'[\u201C\u2018")
}

// sentenceSep is what joins a sentence to the text before it: nothing after
// a CJK sentence (CJK text has no spaces between sentences), else a space.
func sentenceSep(before string) string {
	if endsCJKSentence(before) {
		return ""
	}
	return " "
}

// endsCJKSentence reports whether s ends with CJK terminal punctuation,
// possibly followed by closing quotes or brackets.
func endsCJKSentence(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(strings.TrimRight(s, "\u300D\u300F\uFF09\"\u201D"))
	return r == '\u3002' || r == '\uFF01' || r == '\uFF1F' || r == '\uFF1B'
}

// splitWords splits an overlong sentence at spaces into pieces of at most
// maxBytes, hard-splitting single words that are longer still.
func splitWords(sentence string, maxBytes int) []string {
	var out []string
	var cur strings.Builder
	for _, word := range strings.Fields(sentence) {
		for len(word) > maxBytes {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			cut := maxBytes
			for cut > 0 && !utf8.RuneStart(word[cut]) {
				cut--
			}
			if cut == 0 { // maxBytes smaller than one rune: take the rune anyway
				_, cut = utf8.DecodeRuneInString(word)
			}
			out = append(out, word[:cut])
			word = word[cut:]
		}
		if cur.Len() > 0 && cur.Len()+1+len(word) > maxBytes {
			out = append(out, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(word)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

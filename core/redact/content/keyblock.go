package content

import (
	"regexp"
	"strings"
)

// A private key block: a BEGIN line, a body, an END line. PGP's "PRIVATE KEY BLOCK" is one too.
// The headers spell their spaces as "[ ]", so this source holds no key header.
var (
	keyHeader = regexp.MustCompile(`^-----BEGIN[ ](?:[A-Z0-9]+[ ]){0,3}PRIVATE[ ]KEY(?:[ ]BLOCK)?-----`)
	keyFooter = regexp.MustCompile(`^-----END[ ](?:[A-Z0-9]+[ ]){0,3}PRIVATE[ ]KEY(?:[ ]BLOCK)?-----`)
)

// keyBlockTrigger starts a BEGIN line (lowercase, as triggers are matched).
const keyBlockTrigger = "-----begin"

// isKeyBodyByte reports whether c can be part of a key block's body: base64, the armour
// headers' "Name: value, value (x)" punctuation, the dashes of the END line, and whitespace.
// A quote or a backslash cannot, so a body never runs out of the JSON string it is in.
func isKeyBodyByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("+/=:,.()- \t\r\n", c) >= 0
}

// keyBodyEnd returns where the body run starting at i ends: the first byte that cannot be in a
// body. In raw JSON text the escapes \n \r \t and \/ are body bytes too, since a key block
// inside a JSON string has its line breaks escaped.
//
// An armour header line ("Comment: Alice <alice@example.org>", "Comment: Bob's key #1") is free
// text, so the whole line is body, whatever its bytes, except a double quote or a backslash that
// is not one of those escapes: those can end the JSON string the block is in, and the run must
// not cross it.
//
// el is the END-line cache that findKeyBlocks keeps across all the blocks of the text.
func keyBodyEnd(text string, i int, el *endLineCache) int {
	lineStart := true
	for i < len(text) {
		if lineStart {
			lineStart = false
			if n := armorHeaderName(text[i:]); n > 0 {
				end, ok := armorHeaderValueEnd(text, i+n, el)
				if !ok {
					return end
				}
				i = end
				continue
			}
		}
		c := text[i]
		if c == '\\' && i+1 < len(text) && strings.IndexByte("nrt/", text[i+1]) >= 0 {
			lineStart = text[i+1] == 'n'
			i += 2
			continue
		}
		if !isKeyBodyByte(c) {
			break
		}
		lineStart = c == '\n'
		i++
	}
	return i
}

// armorHeaderName returns the length of an armour header's "Name: " at the start of s, or 0.
func armorHeaderName(s string) int {
	j := 0
	for j < len(s) && j < 64 && (isLetter(s[j]) || j > 0 && (s[j] >= '0' && s[j] <= '9' || s[j] == '-')) {
		j++
	}
	if j == 0 || j+1 >= len(s) || s[j] != ':' || s[j+1] != ' ' {
		return 0
	}
	return j + 2
}

// armorHeaderValueEnd returns where an armour header's value, starting at i, ends: at its line
// break (raw, or escaped in JSON), which is left for the caller. ok is false when the value holds
// a stray backslash, or a double quote (bare or escaped) that may end the JSON string the block
// is in: the run ends there.
//
// A quote can still be part of the header ("Comment: Alice "Al" Lovelace"):
//   - an escaped quote (raw JSON, where every quote inside a string is escaped) is, when the
//     block's END line comes before the next unescaped quote: the string closes after the END
//     line;
//   - a bare quote is, in decoded text only: when its header line ends with a real line break
//     (raw JSON has them escaped), and no line between it and the END line, other than another
//     header line, holds a quote. A bare quote that ends a JSON string is on a line with no real
//     break, and the run stops there.
//
// endLine caches where the next END line is, and what was found up to it. Where this header
// line ends is found once, at its first bare quote, so a line full of quotes is read once.
func armorHeaderValueEnd(text string, i int, endLine *endLineCache) (int, bool) {
	lineEnd := 0 // the line's real line break, once a bare quote asked; -1: none usable
	for i < len(text) {
		switch c := text[i]; {
		case c == '\n' || c == '\r':
			return i, true
		case c == '\\' && i+1 < len(text) && text[i+1] == '"':
			if !endLineBeforeQuote(text, i+2, endLine) {
				return i, false
			}
			i += 2
			continue
		case c == '"':
			if lineEnd == 0 {
				lineEnd = realLineEnd(text, i, endLine)
			}
			if lineEnd < 0 || !decodedHeaderQuoteOK(text, lineEnd, endLine) {
				return i, false
			}
			i++
			continue
		case c == '\\':
			if i+1 < len(text) && strings.IndexByte("nrt", text[i+1]) >= 0 {
				return i, true
			}
			if i+1 < len(text) && text[i+1] == '/' {
				i += 2
				continue
			}
			// A JSON \uXXXX escape (Go's encoder writes "<", ">" and "&" that way) cannot end
			// the string the block is in: it is part of the header.
			if i+6 <= len(text) && text[i+1] == 'u' && isHex4(text[i+2:i+6]) {
				i += 6
				continue
			}
			return i, false
		}
		i++
	}
	return i, true
}

// isHex4 reports whether s is four hex digits.
func isHex4(s string) bool {
	for i := 0; i < len(s); i++ {
		if !(s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'f' || s[i] >= 'A' && s[i] <= 'F') {
			return false
		}
	}
	return len(s) == 4
}

// endLineBeforeQuote reports whether a key block's END line comes at or after i, within 64 KiB,
// and before the next unescaped double quote. The answers are cached in el, so a header full of
// quotes is still read once.
func endLineBeforeQuote(text string, i int, el *endLineCache) bool {
	if !endLineAfter(text, i, el) {
		return false
	}
	if el.clearFrom >= 0 && i >= el.clearFrom {
		return true // no unescaped quote from clearFrom to the END line, so none from i either
	}
	if el.quoteAt >= i {
		return false // an unescaped quote at quoteAt, before the END line, is after i too
	}
	for j := i; j < el.at; j++ {
		if text[j] == '"' && !escapedAt(text, j) {
			el.quoteAt = j
			return false
		}
	}
	el.clearFrom = i
	return true
}

// realLineEnd returns where the line holding q ends, just after its real line break, or -1 when
// it has none or an escaped one ("\n", raw JSON) comes first. The line's break, and the last
// escaped one before it, are cached in el: many blocks on one long line ask about the same line.
func realLineEnd(text string, q int, el *endLineCache) int {
	if !(el.lineFrom >= 0 && q >= el.lineFrom && q < el.lineBreak) {
		el.lineFrom, el.lineBreak = q, len(text)
		if nl := strings.IndexByte(text[q:], '\n'); nl >= 0 {
			el.lineBreak = q + nl
		}
		el.lineEscaped = el.lineFrom + strings.LastIndex(text[el.lineFrom:el.lineBreak], `\n`) // lineFrom-1: none
	}
	if el.lineBreak >= len(text) || el.lineEscaped >= q {
		return -1
	}
	return el.lineBreak + 1
}

// decodedHeaderQuoteOK reports whether a bare quote on an armour header line that ends at
// lineEnd (realLineEnd) is part of the header: the END line follows within 64 KiB, and every line
// in between that holds a quote is a header line. What a walk finds is cached in el.
func decodedHeaderQuoteOK(text string, lineEnd int, el *endLineCache) bool {
	if !endLineAfter(text, lineEnd, el) {
		return false
	}
	if el.linesClearFrom >= 0 && lineEnd >= el.linesClearFrom {
		return true // the lines from linesClearFrom to the END line were checked
	}
	if el.badLineAt >= lineEnd {
		return false // a line with a quote, at badLineAt, is still between
	}
	for j := lineEnd; j < el.at; {
		k := strings.IndexByte(text[j:el.at], '\n')
		line := text[j:el.at]
		if k >= 0 {
			line = text[j : j+k]
		}
		if armorHeaderName(line) == 0 && strings.IndexByte(line, '"') >= 0 {
			el.badLineAt = j
			return false
		}
		if k < 0 {
			break
		}
		j += k + 1
	}
	el.linesClearFrom = lineEnd
	return true
}

// endLineWindow is how far an END line may be from where it is looked for: a key block is a few
// kilobytes, so one farther off is another block's.
const endLineWindow = 64 << 10

// endLineAfter finds the next END line at or after i into el, and reports whether there is one
// within endLineWindow. Each search reads no farther than the window, and no byte an earlier
// search read again: the cache is shared by every block of the text, so many blocks stay linear.
//
// The searched range is [lo, searched). A question about a point before lo (a BEGIN line inside
// an earlier block's header line) is about text never searched: everything is forgotten and the
// search starts again from there.
func endLineAfter(text string, i int, el *endLineCache) bool {
	if i < el.lo {
		el.reset()
		el.lo, el.searched = i, i
	}
	if el.at >= i {
		return el.at-i <= endLineWindow
	}
	el.reset()
	from := max(i, el.searched-len(endMark)+1)
	if i > el.searched {
		el.lo = i // a gap after the searched range: the range starts again here
	}
	limit := min(len(text), i+endLineWindow+len(endMark))
	if from < limit {
		if j := indexFrom(text[:limit], endMark, from); j >= 0 {
			el.at, el.searched = j, j
			return true
		}
		el.searched = limit
	}
	return false
}

const endMark = "-----END"

// endLineCache is what the key-block rule knows about the next END line: where it is (at, -1:
// none found), the range of text searched for one ([lo, searched)), and, up to it: from where
// there is no unescaped quote (clearFrom), where there is one (quoteAt), from where no line but
// header lines holds a quote (linesClearFrom), and where a line that is not a header holds one
// (badLineAt). -1: not known. Apart from the END line, it holds the last line realLineEnd read:
// from lineFrom, its real line break (lineBreak, len(text): none), and the last escaped "\n"
// before it (lineEscaped, lineFrom-1: none).
type endLineCache struct {
	at, lo, searched, clearFrom, quoteAt, linesClearFrom, badLineAt int
	lineFrom, lineBreak, lineEscaped                                int
}

func newEndLineCache() *endLineCache {
	return &endLineCache{at: -1, clearFrom: -1, quoteAt: -1, linesClearFrom: -1, badLineAt: -1, lineFrom: -1}
}

// reset forgets the END line and what was found up to it, but not how far the text was searched.
func (el *endLineCache) reset() {
	el.at, el.clearFrom, el.quoteAt, el.linesClearFrom, el.badLineAt = -1, -1, -1, -1, -1
}

// escapedAt reports whether the byte at j is escaped: an odd number of backslashes before it.
func escapedAt(text string, j int) bool {
	n := 0
	for k := j - 1; k >= 0 && text[k] == '\\'; k-- {
		n++
	}
	return n%2 == 1
}

// findKeyBlocks replaces each private key block whole, from its BEGIN line to its END line.
// A block whose END line is missing (a body cut off mid-key) is replaced to the end of its body
// run. Each step is linear: the body run's end and the next END line are each found once and
// reused while the BEGIN lines are inside them.
func findKeyBlocks(text string, at []int, out []Edit) []Edit {
	covered, runEnd, nextEnd := 0, -1, -1
	el := newEndLineCache() // shared by every block: END positions only move forward
	for _, p := range at {
		if p < covered {
			continue
		}
		h := keyHeader.FindStringIndex(text[p:min(len(text), p+128)])
		if h == nil {
			continue
		}
		bodyStart := p + h[1]
		if runEnd < bodyStart {
			runEnd = keyBodyEnd(text, bodyStart, el)
		}
		end := runEnd // the cut-off case
		for {
			if nextEnd < bodyStart {
				// None after bodyStart means none after any later start either: len(text)
				// stands for "none", so the search is not repeated.
				if nextEnd = indexFrom(text, "-----END", bodyStart); nextEnd < 0 {
					nextEnd = len(text)
				}
			}
			if nextEnd >= runEnd {
				break
			}
			if f := keyFooter.FindStringIndex(text[nextEnd:min(len(text), nextEnd+128)]); f != nil {
				end = nextEnd + f[1]
				break
			}
			bodyStart = nextEnd + 1 // an END line of something else: look past it
		}
		out = append(out, Edit{Start: p, End: end, Cat: catPrivateKey})
		covered = end
	}
	return out
}

// indexFrom is strings.Index from position i, or -1.
func indexFrom(text, sub string, i int) int {
	if i > len(text) {
		return -1
	}
	if j := strings.Index(text[i:], sub); j >= 0 {
		return i + j
	}
	return -1
}

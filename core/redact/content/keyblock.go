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
func keyBodyEnd(text string, i int) int {
	lineStart := true
	endLine := endLineCache{at: -1, clearFrom: -1, linesClearFrom: -1} // found once for every header quote that asks
	for i < len(text) {
		if lineStart {
			lineStart = false
			if n := armorHeaderName(text[i:]); n > 0 {
				end, ok := armorHeaderValueEnd(text, i+n, &endLine)
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
// endLine caches where the next END line is, and what was found up to it.
func armorHeaderValueEnd(text string, i int, endLine *endLineCache) (int, bool) {
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
			if !decodedHeaderQuoteOK(text, i, endLine) {
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
			return i, false
		}
		i++
	}
	return i, true
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
	for j := i; j < el.at; j++ {
		if text[j] == '"' && !escapedAt(text, j) {
			return false
		}
	}
	el.clearFrom = i
	return true
}

// decodedHeaderQuoteOK reports whether a bare quote at q, on an armour header line, is part of
// the header: its line ends with a real line break (no escaped one first), the END line follows
// within 64 KiB, and every line in between that holds a quote is a header line.
func decodedHeaderQuoteOK(text string, q int, el *endLineCache) bool {
	nl := strings.IndexByte(text[q:], '\n')
	if nl < 0 || strings.Contains(text[q:q+nl], `\n`) {
		return false
	}
	lineEnd := q + nl + 1
	if !endLineAfter(text, lineEnd, el) {
		return false
	}
	if el.linesClearFrom >= 0 && lineEnd >= el.linesClearFrom {
		return true // the lines from linesClearFrom to the END line were checked
	}
	for j := lineEnd; j < el.at; {
		k := strings.IndexByte(text[j:el.at], '\n')
		line := text[j:el.at]
		if k >= 0 {
			line = text[j : j+k]
		}
		if armorHeaderName(line) == 0 && strings.IndexByte(line, '"') >= 0 {
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

// endLineAfter finds the next END line at or after i into el, and reports whether there is one
// within 64 KiB.
func endLineAfter(text string, i int, el *endLineCache) bool {
	if el.at < i {
		el.at, el.clearFrom, el.linesClearFrom = len(text), -1, -1 // none
		if j := indexFrom(text, "-----END", i); j >= 0 {
			el.at = j
		}
	}
	return el.at < len(text) && el.at-i <= 64<<10
}

// endLineCache is where the next END line is, and from where up to it there is no unescaped
// quote (clearFrom), or no line but header lines holding one (linesClearFrom); -1: not known.
type endLineCache struct{ at, clearFrom, linesClearFrom int }

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
			runEnd = keyBodyEnd(text, bodyStart)
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

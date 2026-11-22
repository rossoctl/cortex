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
	for i < len(text) {
		if lineStart {
			lineStart = false
			if n := armorHeaderName(text[i:]); n > 0 {
				end, ok := armorHeaderValueEnd(text, i+n)
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
// a double quote or a stray backslash first: the run ends there.
func armorHeaderValueEnd(text string, i int) (int, bool) {
	for i < len(text) {
		switch c := text[i]; {
		case c == '\n' || c == '\r':
			return i, true
		case c == '"':
			return i, false
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

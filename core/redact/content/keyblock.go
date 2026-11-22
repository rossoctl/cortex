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
func keyBodyEnd(text string, i int) int {
	for i < len(text) {
		c := text[i]
		if c == '\\' && i+1 < len(text) && strings.IndexByte("nrt/", text[i+1]) >= 0 {
			i += 2
			continue
		}
		if !isKeyBodyByte(c) {
			break
		}
		i++
	}
	return i
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

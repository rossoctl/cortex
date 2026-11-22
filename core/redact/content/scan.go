package content

import (
	"encoding/base64"
	"strings"
)

// THE PREFILTER. One pass over the text finds every rule's trigger strings, case folded; a
// rule runs only at its triggers. Most text values hold none, and then no pattern runs at all.

type trigger struct {
	s    string // lowercase
	rule int    // an index into anchoredRules, then into specials
}

// special is a rule with a finder of its own, run at its triggers.
type special struct {
	triggers []string
	find     func(text string, at []int, out []Edit) []Edit
}

var specials = []special{
	{[]string{keyBlockTrigger}, findKeyBlocks},
	{[]string{"://"}, findURLPasswords},
	{keywordTriggers, findKeywords},
}

var (
	ruleCount = len(anchoredRules) + len(specials)

	triggers []trigger
	// pairGroup maps the first two bytes (folded) of a trigger to 1 + its index in
	// pairTriggers, the triggers that start with them; 0 means none does.
	pairGroup    [1 << 16]uint8
	pairTriggers [][]int
	lowerByte    [256]byte
)

func init() {
	for c := range lowerByte {
		lowerByte[c] = byte(c)
		if c >= 'A' && c <= 'Z' {
			lowerByte[c] = byte(c) + 'a' - 'A'
		}
	}
	for i, r := range anchoredRules {
		for _, s := range r.triggers {
			triggers = append(triggers, trigger{s, i})
		}
	}
	for i, sp := range specials {
		for _, s := range sp.triggers {
			triggers = append(triggers, trigger{s, len(anchoredRules) + i})
		}
	}
	for i, t := range triggers {
		k := pair(t.s[0], t.s[1])
		if pairGroup[k] == 0 {
			pairTriggers = append(pairTriggers, nil)
			pairGroup[k] = uint8(len(pairTriggers)) // fewer than 256 groups: init panics otherwise
		}
		g := pairGroup[k] - 1
		pairTriggers[g] = append(pairTriggers[g], i)
	}
	if len(pairTriggers) > 255 {
		panic("content: too many trigger pairs")
	}
}

func pair(a, b byte) uint16 { return uint16(lowerByte[a])<<8 | uint16(lowerByte[b]) }

// scanTriggers returns, per rule, the sorted positions where one of its triggers starts.
func scanTriggers(text string) [][]int {
	var hits [][]int
	for i := 0; i+1 < len(text); i++ {
		g := pairGroup[pair(text[i], text[i+1])]
		if g == 0 {
			continue
		}
		for _, ti := range pairTriggers[g-1] {
			t := triggers[ti]
			if hasPrefixFold(text[i:], t.s) {
				if hits == nil {
					hits = make([][]int, ruleCount)
				}
				hits[t.rule] = append(hits[t.rule], i)
			}
		}
	}
	return hits
}

// hasPrefixFold reports whether s starts with the lowercase prefix, ASCII case folded.
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lowerByte[s[i]] != prefix[i] {
			return false
		}
	}
	return true
}

// findPatterns runs the pattern rules on text. encoded is false for decoded base64, which runs
// every rule but the encoded one itself (a Kubernetes Secret's .dockerconfigjson, a base64
// .env file).
func findPatterns(text string, encoded bool) []Edit {
	var out []Edit
	if hits := scanTriggers(text); hits != nil {
		for i, r := range anchoredRules {
			if len(hits[i]) > 0 {
				out = r.find(text, hits[i], out)
			}
		}
		for i, sp := range specials {
			if h := hits[len(anchoredRules)+i]; len(h) > 0 {
				out = sp.find(text, h, out)
			}
		}
	}
	if encoded {
		out = findEncoded(text, out)
	}
	return out
}

// THE ENCODED RULE: a run of 64 or more base64 (or base64url) characters, padded or not, that
// decodes to text holding a secret is replaced whole. A run over maxEncodedRun is not decoded:
// it is binary data (an image, a file), which the callers skip by JSON path anyway, and decoding
// it would cost more than it finds. Decoded bytes that are mostly not text are binary too.
const (
	minEncodedRun = 64
	maxEncodedRun = 64 << 10
)

var isBase64Byte = func() (t [256]bool) {
	for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/-_" {
		t[c] = true
	}
	return t
}()

func findEncoded(text string, out []Edit) []Edit {
	for i := 0; i < len(text); {
		if !isBase64Byte[text[i]] {
			i++
			continue
		}
		s := i
		for i < len(text) && isBase64Byte[text[i]] {
			i++
		}
		e := i
		for i < len(text) && i-e < 2 && text[i] == '=' {
			i++
		}
		if n := e - s; n < minEncodedRun || n > maxEncodedRun {
			continue
		}
		dec, ok := decodeBase64(text[s:e], i > e)
		if ok && isMostlyText(dec) && len(findPatterns(dec, false)) > 0 {
			out = append(out, Edit{Start: s, End: i, Cat: catEncoded})
		}
	}
	return out
}

// decodeBase64 decodes a run in the one encoding its characters allow: standard, or URL-safe.
func decodeBase64(run string, padded bool) (string, bool) {
	std, url := strings.ContainsAny(run, "+/"), strings.ContainsAny(run, "-_")
	if std && url {
		return "", false
	}
	enc := base64.StdEncoding
	if url {
		enc = base64.URLEncoding
	}
	if !padded {
		enc = enc.WithPadding(base64.NoPadding)
	}
	b, err := enc.DecodeString(run + strings.Repeat("=", padLen(run, padded)))
	return string(b), err == nil
}

// padLen is how much padding a padded run needs to be whole: the run's own "=" were left out
// of it.
func padLen(run string, padded bool) int {
	if !padded {
		return 0
	}
	return (4 - len(run)%4) % 4
}

// isMostlyText reports whether at least 90% of b's bytes are printable ASCII, whitespace or
// UTF-8 (0x80 and up).
func isMostlyText(b string) bool {
	n := 0
	for i := 0; i < len(b); i++ {
		if c := b[i]; c >= 0x20 && c < 0x7f || c == '\n' || c == '\r' || c == '\t' || c >= 0x80 {
			n++
		}
	}
	return n*10 >= len(b)*9
}

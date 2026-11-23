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

// WRAPPED BASE64 ("base64 .env" wraps at 76 columns; MIME and PEM bodies at 64 or 76) is decoded
// as one run, so a secret that crosses a line break is found, and the whole block is replaced.
// A block (wrappedBlock) is two or more lines of one width, each followed by one line break (raw,
// or escaped in raw JSON), then perhaps a last line that completes the base64. Whatever does not
// make a block that decodes to a secret is decoded run by run, as an unwrapped run is: a
// block's lines, or one run on its own. Each run is decoded a bounded number of times (in the
// block, in the block without its last line, on its own, without an escape's letter), so the rule
// stays linear.
func findEncoded(text string, out []Edit) []Edit {
	for i := 0; i < len(text); {
		if !isBase64Byte[text[i]] {
			i++
			continue
		}
		// In raw JSON, a line's run starts after an escaped "\n": its "n" is a base64 letter.
		// The block is looked for after it first.
		var block []b64Run
		if escapeLetterAt(text, i) {
			if b := wrappedBlock(text, i+1); len(b) >= 2 {
				block = b
			}
		}
		if block == nil {
			block = wrappedBlock(text, i)
		}
		i = block[len(block)-1].end
		covered := -1
		if e, ok := blockEdit(text, block); ok {
			out = append(out, notOnEscape(text, e))
			covered = e.End
		}
		// The runs the block's edit does not cover (all of them, or a last line it was found
		// without) are decoded on their own: a last line can hold a secret of its own.
		for _, r := range block {
			if r.s >= covered {
				if e, ok := runEdit(text, r); ok {
					out = append(out, notOnEscape(text, e))
				}
			}
		}
	}
	return out
}

// b64Run is a run of base64 characters, text[s:e], and its padding, up to end.
type b64Run struct{ s, e, end int }

// encodedRunAt returns the base64 run starting at i, with up to two "=" of padding.
func encodedRunAt(text string, i int) b64Run {
	r := b64Run{s: i}
	for i < len(text) && isBase64Byte[text[i]] {
		i++
	}
	r.e = i
	for i < len(text) && i-r.e < 2 && text[i] == '=' {
		i++
	}
	r.end = i
	return r
}

// wrappedBlock returns the runs of the wrapped block that starts at i: full lines of one width
// (a multiple of 4, at least 16, unpadded), each followed by one line break, and perhaps a last
// line (lastLineOK). A run that starts no block is returned alone.
func wrappedBlock(text string, i int) []b64Run {
	first := encodedRunAt(text, i)
	block := []b64Run{first}
	width := first.e - first.s
	if first.end != first.e || width < 16 || width%4 != 0 {
		return block
	}
	for pos := first.end; ; {
		b := lineBreakAt(text, pos)
		if b == 0 || pos+b >= len(text) || !isBase64Byte[text[pos+b]] {
			return block
		}
		r := encodedRunAt(text, pos+b)
		switch n := r.e - r.s; {
		case n == width && r.end == r.e:
			block = append(block, r)
			pos = r.end
		case n <= width && lastLineOK(text, block, r):
			return append(block, r)
		default:
			return block
		}
	}
}

// lastLineOK reports whether r, a line shorter than block's or padded, is the block's last line:
// it ends the line (a line break, a quote, a backslash, a space, the end), it completes the
// base64 (a multiple of 4 with its padding), and it is padded or follows two full lines. "\nok"
// or "\nPORT=8080" after a run is the text that follows it, not its last line.
func lastLineOK(text string, block []b64Run, r b64Run) bool {
	if r.end < len(text) && strings.IndexByte("\n\r\"\\ \t", text[r.end]) < 0 {
		return false
	}
	total := r.end - r.s
	for _, l := range block {
		total += l.e - l.s
	}
	return total%4 == 0 && (r.end > r.e || len(block) >= 2)
}

// blockEdit decodes a wrapped block of two or more runs as one, and returns the edit for it when
// it decodes to text with a secret. The whole block is decoded first, so a secret that crosses
// into the last line is replaced with it. When every secret ends before the last line, and that
// line is shorter or padded (it may be the text that follows, "APP_KEY=" on the next line), the
// edit stops before it, and the caller decodes it on its own. When the whole block does not
// decode (a last line that only looked like one), it is tried without its last line.
func blockEdit(text string, block []b64Run) (Edit, bool) {
	if len(block) < 2 {
		return Edit{}, false
	}
	last := block[len(block)-1]
	tail := last.e-last.s != block[0].e-block[0].s || last.end > last.e
	for _, rs := range [][]b64Run{block, block[:len(block)-1]} {
		if len(rs) < len(block) && !tail {
			break
		}
		var b strings.Builder
		for _, r := range rs {
			b.WriteString(text[r.s:r.e])
		}
		end := rs[len(rs)-1]
		if n := b.Len(); n < minEncodedRun || n > maxEncodedRun {
			continue
		}
		dec, ok := decodeBase64(b.String(), end.end > end.e)
		if !ok || !isMostlyText(dec) {
			continue
		}
		edits := findPatterns(dec, false)
		if len(edits) == 0 {
			continue
		}
		if len(rs) == len(block) && tail {
			// Where the last line's bytes start in the decoded text: every full line is a
			// multiple of 4 characters, so 3 bytes per 4.
			lastAt := (b.Len() - (last.e - last.s)) / 4 * 3
			inLast := false
			for _, e := range edits {
				inLast = inLast || e.End > lastAt
			}
			if !inLast {
				return Edit{Start: rs[0].s, End: block[len(block)-2].end, Cat: catEncoded}, true
			}
		}
		return Edit{Start: rs[0].s, End: end.end, Cat: catEncoded}, true
	}
	return Edit{}, false
}

// runEdit decodes one run on its own, and returns the edit for it when it decodes to text with a
// secret. A run that starts with an escape's letter (raw JSON's "\n" before it) is tried
// without that letter too.
func runEdit(text string, r b64Run) (Edit, bool) {
	for _, s := range []int{r.s, r.s + 1} {
		if s > r.s && !escapeLetterAt(text, r.s) {
			break
		}
		if n := r.e - s; n < minEncodedRun || n > maxEncodedRun {
			continue
		}
		dec, ok := decodeBase64(text[s:r.e], r.end > r.e)
		if ok && isMostlyText(dec) && len(findPatterns(dec, false)) > 0 {
			return Edit{Start: s, End: r.end, Cat: catEncoded}, true
		}
	}
	return Edit{}, false
}

// notOnEscape moves an edit that starts on the letter of a JSON escape ("\n" before a run) past
// that letter: the marker must not split an escape, or raw JSON would no longer be valid.
func notOnEscape(text string, e Edit) Edit {
	if escapeLetterAt(text, e.Start) && e.Start+1 < e.End {
		e.Start++
	}
	return e
}

// escapeLetterAt reports whether text[i] is the letter of a JSON escape ("\n", "\r", "\t", "\b",
// "\f", "\/"): an odd number of backslashes before it.
func escapeLetterAt(text string, i int) bool {
	if i == 0 || strings.IndexByte("nrtbf/", text[i]) < 0 {
		return false
	}
	n := 0
	for j := i - 1; j >= 0 && text[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// lineBreakAt returns the length of the one line break at text[i]: "\n", "\r\n", or their
// escapes in raw JSON; 0 for none.
func lineBreakAt(text string, i int) int {
	for _, br := range []string{"\r\n", "\n", `\r\n`, `\n`} {
		if strings.HasPrefix(text[i:], br) {
			return len(br)
		}
	}
	return 0
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

// Package content finds secrets in content: the text of request and response bodies, SSE
// streams, and the text fields of session events (#1921).
//
// It is the content counterpart of core/redact, which strips credential-named KEYS from config,
// headers and query strings. This package finds secret-shaped VALUES in text.
//
// THE RULES SEE ONE DECODED TEXT VALUE AT A TIME: a JSON string's content, a block of streamed
// text, an error message. They know nothing of JSON quoting or SSE framing; the layers that do
// (a JSON scanner, SSE reassembly) call Find on each value and splice the result back. Find
// returns edits, byte ranges in the text it was given, so a caller can map a redaction back onto
// the bytes it came from (an SSE delta boundary, say) and build its output once.
//
// Even so, every rule is written to be safe on raw bytes too: no character class admits a quote,
// a backslash or whitespace where the secret could not hold one, and every candidate is checked
// at its boundaries. Text that is not JSON (a plain-text error, a malformed body) is redacted as
// a whole, through String or Bytes, and stays well formed.
//
// THIS IS THE SECRET RULES ONLY (#1921, part A1): provider API keys, private key blocks, JWTs,
// Authorization and bearer values, the password in a URL, a secret-named keyword with a secret-
// shaped value, and those secrets inside base64. Identity rules (names, emails, hosts, account
// ids) come later, as do the callers: nothing in Cortex calls this package yet.
//
// Every pattern is RE2, so a match is linear in its input, and each runs only where one of its
// trigger strings occurs: one pass over the text finds every trigger, and a rule's pattern is
// then tried, anchored, at each of its triggers. A check that needs code (entropy, the safe-id
// list, a boundary, decoding base64) runs on the candidate a pattern found.
package content

import (
	"sort"
	"strings"
)

// Edit is one secret found in a text value: the bytes text[Start:End] are to be replaced by
// the marker of category Cat, "[REDACTED:<Cat>]".
type Edit struct {
	Start, End int
	Cat        string
}

// Context is what a caller knows about where a text value sits.
type Context struct {
	// Key is the JSON object key the value is the value of, or "". A secret-named key
	// ("password", "client_secret", "x-api-key") makes the whole value a candidate, checked by
	// the same rules as a keyword assignment in text.
	Key string
}

// Stats counts the redactions by category. A category absent from the map had none.
type Stats map[string]int

// Redactor finds secrets in text. It holds no state between calls and is safe for concurrent
// use.
type Redactor struct{}

// New returns a Redactor with the secret rules.
func New() *Redactor { return &Redactor{} }

// minLen is the shortest text the pattern rules look at: nothing shorter holds a secret they
// match. A keyed value (Context.Key) is checked whatever its length.
const minLen = 8

// Find returns the secrets in one text value, as edits sorted by Start and not overlapping.
// It returns nil when there are none.
func (r *Redactor) Find(text string, ctx Context) []Edit {
	var edits []Edit
	if ctx.Key != "" {
		if e, ok := keyedValue(text, ctx.Key); ok {
			edits = append(edits, e)
		}
	}
	if len(text) >= minLen {
		edits = append(edits, findPatterns(text, true)...)
	}
	return merge(edits)
}

// Apply returns text with each edit replaced by its marker. The edits must be sorted and not
// overlap, as Find returns them. With no edits it returns text itself.
func Apply(text string, edits []Edit) string {
	if len(edits) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, e := range edits {
		b.WriteString(text[last:e.Start])
		b.WriteString(marker(e.Cat))
		last = e.End
	}
	b.WriteString(text[last:])
	return b.String()
}

// Count returns the edits counted by category.
func Count(edits []Edit) Stats {
	st := Stats{}
	for _, e := range edits {
		st[e.Cat]++
	}
	return st
}

// String returns s with every secret replaced, and what was replaced. s is redacted as one
// text value with no key. When nothing matched it returns s itself.
func (r *Redactor) String(s string) (string, Stats) {
	edits := r.Find(s, Context{})
	return Apply(s, edits), Count(edits)
}

// Bytes is String for a byte slice. When nothing matched it returns b itself, so an unchanged
// body is kept byte for byte without a copy.
func (r *Redactor) Bytes(b []byte) ([]byte, Stats) {
	s := string(b)
	edits := r.Find(s, Context{})
	if len(edits) == 0 {
		return b, Stats{}
	}
	return []byte(Apply(s, edits)), Count(edits)
}

// marker is the replacement for a secret of category cat. It is plain ASCII with no quote or
// backslash, so it can stand inside a JSON string or an SSE data line as it is.
func marker(cat string) string { return "[REDACTED:" + cat + "]" }

// priority orders the categories for overlapping edits: the merged edit takes the category
// of the highest. A key block or an encoded secret is named for the whole it covers.
var priority = map[string]int{
	catPrivateKey: 7, catEncoded: 6, catJWT: 5, catAPIKey: 4, catCredential: 3, catPassword: 2, catSecretValue: 1,
}

// merge sorts the edits and joins any that overlap, so none of a secret survives between two
// rules' matches.
func merge(edits []Edit) []Edit {
	if len(edits) < 2 {
		return edits
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].Start != edits[j].Start {
			return edits[i].Start < edits[j].Start
		}
		return edits[i].End > edits[j].End
	})
	out := edits[:1]
	for _, e := range edits[1:] {
		cur := &out[len(out)-1]
		if e.Start >= cur.End {
			out = append(out, e)
			continue
		}
		cur.End = max(cur.End, e.End)
		if priority[e.Cat] > priority[cur.Cat] {
			cur.Cat = e.Cat
		}
	}
	return out
}

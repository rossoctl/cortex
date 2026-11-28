package content

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// THE JSON LAYER (#1921, part A1b). A body that is JSON is not decoded and re-encoded: a Go map
// loses the key order, and Marshal re-escapes "<", ">" and "&", rewrites numbers and drops the
// whitespace. Instead a scanner walks the raw bytes and finds each string literal. Each string
// value is decoded on its own and given to Find, with the key it is the value of as context;
// only a string with a secret in it is encoded again. Every other byte (whitespace, numbers, key
// order, the strings left as they were, with their own escapes) is copied as it is. So an
// unchanged body comes back byte for byte, and a changed one differs only inside the strings
// that held a secret.
//
//   - Keys are not redacted: the secret rules run on values only. A number under a secret name
//     ("password": 12345678) is checked as text would be, and becomes a string when redacted.
//   - JSON in JSON (tool arguments and tool results hold JSON in a string): a decoded string
//     that is itself a JSON object or array is scanned the same way, up to maxJSONDepth, with
//     the string's key for its top-level values, and the outer string is encoded again when the
//     inner document changed.
//   - Images, audio and documents are not read. A string is skipped when it sits where binary
//     data does (skipBinary, a binary data URI) or is 64 KiB or more, AND it is all base64 of a
//     known binary format (isBinaryBase64): where a string sits never skips text. The skips are
//     counted, so an export can say what was not looked at. A secret inside an attached PDF or a
//     screenshot is not found.
//   - A body that is not valid JSON (an HTML error page, text, a truncated body) is redacted as
//     text, as Bytes does.

// maxJSONDepth is how many levels of JSON in a JSON string are scanned as JSON. A deeper one is
// redacted as text.
const maxJSONDepth = 4

// minBinaryGuard is the length from which a string that is all base64 of a known binary format
// (an image, audio, a PDF) is skipped wherever it sits.
const minBinaryGuard = 64 << 10

// JSONStats is what JSON found: the redactions by category, the binary strings it skipped
// unread (a count and their bytes), and whether the body was not JSON and was redacted as text.
type JSONStats struct {
	Redacted                          Stats
	SkippedBinary, SkippedBinaryBytes int
	Text                              bool
}

// JSON returns the JSON document b with every secret in its string values replaced, and what
// was replaced and skipped. When nothing changed it returns b itself. A body that is not valid
// JSON is redacted as text (Bytes).
func (r *Redactor) JSON(b []byte) ([]byte, JSONStats) {
	st := JSONStats{Redacted: Stats{}}
	if !json.Valid(b) {
		out, s := r.Bytes(b)
		st.Redacted, st.Text = s, true
		return out, st
	}
	j := jsonScan{r: r, st: &st}
	out, changed := j.document(string(b), 0, "")
	if !changed {
		return b, st
	}
	return []byte(out), st
}

// jsonScan holds what one JSON call shares across the documents it scans (JSON in JSON).
type jsonScan struct {
	r  *Redactor
	st *JSONStats
	// keys caches what each key means for its values. A body repeats a few keys ("role",
	// "content", "type") thousands of times, and the name rule is not free.
	keys map[string]keyInfo
}

// keyInfo is what a key means for its values: whether Find uses it as context (a secret or
// cookie name), and the shortest number under it that can be a secret (0: none can).
type keyInfo struct {
	named  bool
	minNum int
}

// key returns what key means for its values.
func (j *jsonScan) key(key string) keyInfo {
	if key == "" {
		return keyInfo{}
	}
	k, ok := j.keys[key]
	if !ok {
		switch t := nameTier(key); {
		case isCookieName(key):
			k = keyInfo{named: true}
		case t == tierPassword:
			k = keyInfo{named: true, minNum: 6}
		case t != tierNone:
			k = keyInfo{named: true, minNum: 16}
		}
		if j.keys == nil {
			j.keys = map[string]keyInfo{}
		}
		if len(j.keys) < 4096 { // a body with endless distinct keys is not cached whole
			j.keys[key] = k
		}
	}
	return k
}

// context is the Context for a value under key: the key, when it is one Find uses, else none,
// which Find treats the same.
func (j *jsonScan) context(key string) Context {
	if !j.key(key).named {
		return Context{}
	}
	return Context{Key: key}
}

// replacement is a string literal of a document, doc[start:end] (quotes included), to be
// replaced by text (an encoded string, quotes included).
type replacement struct {
	start, end int
	text       string
}

// container is an object or array the scanner is inside.
type container struct {
	obj bool
	// key is the object key this container is the value of. An array's elements take it as
	// theirs: in "tokens": ["...", "..."] each value is a token.
	key string
	// typ is the object's "type" value, when it is a short string.
	typ string
	// deferred are the string values whose skip depends on typ, which may come after them:
	// they are looked at when the object closes.
	deferred []literal
}

// literal is a string value in a document: doc[start:end], quotes included, and its key.
type literal struct {
	start, end int
	key        string
}

// document scans one valid JSON document and returns it with its secrets replaced, and whether
// anything changed. outerKey is the key the document sits under, for JSON in a JSON string
// ({"password":"[\"...\"]"}): its top-level values take it, as an array's elements take theirs.
func (j *jsonScan) document(doc string, depth int, outerKey string) (string, bool) {
	var (
		repl      []replacement
		stack     []*container
		key       string // the key of the value that comes next, in an object
		expectKey bool
	)
	valueKey := func() string {
		if len(stack) == 0 {
			return outerKey
		}
		if top := stack[len(stack)-1]; top.obj {
			return key
		}
		return stack[len(stack)-1].key
	}
	for i := 0; i < len(doc); {
		switch c := doc[i]; c {
		case ' ', '\t', '\n', '\r', ':':
			i++
		case ',':
			if len(stack) > 0 && stack[len(stack)-1].obj {
				expectKey = true
			}
			i++
		case '{', '[':
			stack = append(stack, &container{obj: c == '{', key: valueKey()})
			expectKey = c == '{'
			i++
		case '}', ']':
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, l := range top.deferred {
				if top.typ == "base64" && isBinaryBase64(doc[l.start+1:l.end-1]) {
					j.skipped(l.end - l.start - 2)
					continue
				}
				repl = j.value(doc, l, depth, repl)
			}
			expectKey = false
			i++
		case '"':
			end := stringEnd(doc, i)
			if expectKey {
				key = decodeJSONString(doc[i+1 : end-1])
				expectKey = false
				i = end
				continue
			}
			l := literal{start: i, end: end, key: valueKey()}
			i = end
			if len(stack) == 0 {
				repl = j.value(doc, l, depth, repl)
				continue
			}
			top := stack[len(stack)-1]
			if top.obj && l.key == "type" && l.end-l.start < 64 {
				top.typ = decodeJSONString(doc[l.start+1 : l.end-1])
			}
			switch skipBinary(top, l.key) {
			case skipNow:
				if isBinaryBase64(doc[l.start+1 : l.end-1]) {
					j.skipped(l.end - l.start - 2)
				} else {
					repl = j.value(doc, l, depth, repl)
				}
			case skipIfBase64:
				top.deferred = append(top.deferred, l)
			default:
				repl = j.value(doc, l, depth, repl)
			}
		default: // a number, true, false or null
			start := i
			for i < len(doc) && strings.IndexByte(",}] \t\n\r", doc[i]) < 0 {
				i++
			}
			if c == '-' || c >= '0' && c <= '9' {
				// A number under a secret-named key ("password": 12345678) is checked as text would
				// be. A redacted one becomes a string: the marker is not a number.
				repl = j.number(doc[start:i], start, valueKey(), repl)
			}
		}
	}
	if len(repl) == 0 {
		return doc, false
	}
	sort.Slice(repl, func(a, b int) bool { return repl[a].start < repl[b].start })
	var b strings.Builder
	b.Grow(len(doc))
	last := 0
	for _, r := range repl {
		b.WriteString(doc[last:r.start])
		b.WriteString(r.text)
		last = r.end
	}
	b.WriteString(doc[last:])
	return b.String(), true
}

func (j *jsonScan) skipped(n int) {
	j.st.SkippedBinary++
	j.st.SkippedBinaryBytes += n
}

// value redacts one string value, adding its replacement to repl when it changed.
func (j *jsonScan) value(doc string, l literal, depth int, repl []replacement) []replacement {
	raw := doc[l.start+1 : l.end-1]
	if isBinaryDataURI(raw) || len(raw) >= minBinaryGuard && isBinaryBase64(raw) {
		j.skipped(len(raw))
		return repl
	}
	text := decodeJSONString(raw)
	if depth < maxJSONDepth && looksLikeJSONContainer(text) && json.Valid([]byte(text)) {
		inner, changed := j.document(text, depth+1, l.key)
		if changed {
			repl = append(repl, replacement{l.start, l.end, encodeJSONString(inner)})
		}
		return repl
	}
	edits := j.r.Find(text, j.context(l.key))
	if len(edits) == 0 {
		return repl
	}
	for _, e := range edits {
		j.st.Redacted[e.Cat]++
	}
	return append(repl, replacement{l.start, l.end, encodeJSONString(Apply(text, edits))})
}

// number redacts a number literal num at doc[start:] under key, when the key names a secret.
// A number shorter than any secret under that key can be ("prompt_token_ids": [31337, ...]:
// "token" is a secret name, but a 5-digit number under it is not one) is not looked at.
func (j *jsonScan) number(num string, start int, key string, repl []replacement) []replacement {
	k := j.key(key)
	if k.minNum == 0 || len(num) < k.minNum {
		return repl
	}
	edits := j.r.Find(num, Context{Key: key})
	if len(edits) == 0 {
		return repl
	}
	for _, e := range edits {
		j.st.Redacted[e.Cat]++
	}
	return append(repl, replacement{start, start + len(num), encodeJSONString(Apply(num, edits))})
}

// skipKind is what the scanner does with a string value because of where it sits.
type skipKind int

const (
	skipNo       skipKind = iota
	skipNow               // binary data: not read
	skipIfBase64          // binary data when its object's "type" is "base64"
)

// skipBinary says whether a string value under key, directly in container top, is an image,
// audio or a document by where it sits:
//   - Anthropic's {"type":"image"|"document","source":{"type":"base64","data":...}}: the
//     source's "data", when the source's type is "base64" (a "text" document source holds
//     plain text, which is read);
//   - OpenAI's {"input_audio":{"data":...}}.
//
// Data URIs (OpenAI's image_url.url, the Responses API's image_url and file_data) are skipped by
// their content instead, wherever they sit: isBinaryDataURI.
func skipBinary(top *container, key string) skipKind {
	if !top.obj || key != "data" {
		return skipNo
	}
	switch top.key {
	case "source":
		return skipIfBase64
	case "input_audio":
		return skipNow
	}
	return skipNo
}

// isBinaryDataURI reports whether the raw JSON string s is, whole, a base64 data URI of an image,
// audio, video, a PDF or other binary data: a binary media type, and after ";base64," nothing but
// base64 of a known binary format (isBinaryBase64). A text data URI ("data:text/plain;base64,"),
// or a string that only starts like one (a tool result: a data URI, then text), is read.
func isBinaryDataURI(s string) bool {
	if !strings.HasPrefix(s, "data:") {
		return false
	}
	head := s[5:min(len(s), 5+128)]
	semi := strings.Index(head, ";base64,")
	if semi < 0 {
		return false
	}
	mt := strings.ToLower(head[:semi])
	if strings.ContainsAny(mt, " \t\\") {
		return false
	}
	for _, p := range []string{"image/", "audio/", "video/", "application/pdf", "application/octet-stream"} {
		if strings.HasPrefix(mt, p) {
			return isBinaryBase64(s[5+semi+len(";base64,"):])
		}
	}
	return false
}

// binaryMagic are the first bytes of the binary formats the guard knows: PNG, JPEG, GIF, WebP and
// WAV (RIFF), PDF, MP3 (an ID3 tag or a frame sync).
var binaryMagic = []string{"\x89PNG", "\xff\xd8\xff", "GIF8", "RIFF", "%PDF", "ID3", "\xff\xfb", "\xff\xf3", "\xff\xf2"}

// isBinaryBase64 reports whether the raw JSON string s is all standard base64 (a "\n" escape
// between lines allowed) and decodes to a known binary format. Every skip checks this: where a
// string sits only says it should be binary, so text there ("source": {"type": "base64",
// "data": "my key is ..."} in a tool's arguments) is still read.
//
// Every line must be base64 ("=" only as a line's final padding), the whole string. Only a
// prefix is decoded, binaryPrefix bytes, and what follows the format's header there must not
// read as text (readsAsText): 10% or more control bytes, or else neither valid UTF-8 nor 90%
// printable ASCII. A real image is compressed data, which is neither; a header put in front of
// base64 text ("iVBORw0KGgo..." then a config
// file) is read. The prefix is enough: an image with text appended after it is already caught by
// its compressed head failing readsAsText, so the whole-string check only ever caught "header,
// then text", and the prefix catches that too, without decoding 30 MB for a 40 MB image. A
// string built to get past it (a header, then 64 KiB of non-text, then text) is skipped: that is
// no real image, and the cost of reading every image whole.
func isBinaryBase64(s string) bool {
	if len(s) < 16 {
		return false
	}
	var buf []byte
	for line := range strings.SplitSeq(strings.ReplaceAll(s, `\r`, `\n`), `\n`) {
		if line == "" {
			continue
		}
		if !base64Line(line) {
			return false
		}
		if len(buf) < binaryPrefix {
			// Only the prefix's share of the line: an image in JSON is one long line.
			b, err := base64.StdEncoding.DecodeString(line[:min(len(line), binaryPrefix/3*4)])
			if err != nil {
				return false
			}
			buf = append(buf, b...)
		}
	}
	buf = buf[:min(len(buf), binaryPrefix)]
	for _, m := range binaryMagic {
		if strings.HasPrefix(string(buf[:min(len(buf), len(m))]), m) {
			return !readsAsText(buf[len(m):])
		}
	}
	return false
}

// binaryPrefix is how much of a base64 string isBinaryBase64 decodes.
const binaryPrefix = 64 << 10

// base64Line reports whether line is standard base64 with its padding: a multiple of 4
// characters of the alphabet, "=" only as its last one or two. No decoding.
func base64Line(line string) bool {
	if len(line)%4 != 0 {
		return false
	}
	pad := len(line) - len(strings.TrimRight(line, "="))
	if pad > 2 {
		return false
	}
	for i := 0; i < len(line)-pad; i++ {
		c := line[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/') {
			return false
		}
	}
	return true
}

// readsAsText reports whether b is text in any language, not compressed, random or zeroed data:
// under 10% control bytes, and valid UTF-8 or at least 90% printable ASCII and whitespace.
func readsAsText(b []byte) bool {
	printable, control := 0, 0
	for _, c := range b {
		switch {
		case c >= 0x20 && c < 0x7f || c == '\n' || c == '\r' || c == '\t':
			printable++
		case c < 0x20 || c == 0x7f:
			control++
		}
	}
	if control*10 >= len(b) {
		return false
	}
	return printable*10 >= len(b)*9 || utf8.Valid(b)
}

// looksLikeJSONContainer reports whether s, after leading whitespace, starts an object or array.
func looksLikeJSONContainer(s string) bool {
	s = strings.TrimLeft(s, " \t\r\n")
	return s != "" && (s[0] == '{' || s[0] == '[')
}

// stringEnd returns the index just after the closing quote of the JSON string that opens at
// doc[i]. The document is valid, so there is one.
func stringEnd(doc string, i int) int {
	for i++; i < len(doc); i++ {
		switch doc[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(doc)
}

// decodeJSONString decodes the content of a valid JSON string literal (without its quotes), as
// encoding/json does: a lone surrogate becomes U+FFFD.
func decodeJSONString(s string) string {
	if strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if j := strings.IndexByte(s[i:], '\\'); j != 0 {
			if j < 0 {
				b.WriteString(s[i:])
				break
			}
			b.WriteString(s[i : i+j]) // the run up to the next escape, at once
			i += j
		}
		if i+1 >= len(s) {
			b.WriteByte('\\')
			break
		}
		i++
		switch s[i] {
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r := hex4(s[i+1:])
			i += 4
			if utf16.IsSurrogate(r) {
				r2 := rune(-1)
				if i+6 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
					r2 = hex4(s[i+3:])
				}
				if d := utf16.DecodeRune(r, r2); d != utf8.RuneError {
					b.WriteRune(d)
					i += 6
					continue
				}
				r = utf8.RuneError
			}
			b.WriteRune(r)
		default: // " \ /
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// hex4 is the value of the four hex digits at the start of s (valid JSON has them).
func hex4(s string) rune {
	var r rune
	for i := 0; i < 4 && i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			c = c - 'A' + 10
		}
		r = r<<4 | rune(c)
	}
	return r
}

// encodeJSONString encodes s as a JSON string literal, quotes included: a quote, a backslash and
// the control characters are escaped, and nothing else. "<", ">", "&" and non-ASCII stay as they
// are.
func encodeJSONString(s string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		j := i
		for j < len(s) && s[j] >= 0x20 && s[j] != '"' && s[j] != '\\' {
			j++
		}
		b.WriteString(s[i:j]) // the run that needs no escape, at once
		if i = j; i == len(s) {
			break
		}
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/tidwall/gjson"
)

// LOSSLESS CAPTURE (#1494). A request event can carry the request exactly as it crossed the
// proxy, so that a later replay sends the same bytes. A body is stored as a skeleton — its bytes
// with each large block replaced by a reference — and the blocks themselves, each written once
// per segment as a block record ("b"). Every request re-sends the whole conversation, so after
// the first request of a segment almost every block is a reference to one already written.
//
// THE CUTS ARE AT BYTE OFFSETS OF THE ORIGINAL, and nothing is re-encoded: a rebuild is the
// skeleton's pieces and the blocks concatenated in order, which is the original by
// construction. The SHA-256 of the original is stored too, and a rebuild that does not match it
// is refused. Exact bytes matter: the prompt cache and thinking signatures are sensitive to them.

// captureRec is a request capture on its event record: the skeletons of the original and the
// forwarded body, and the request's headers and query. Old readers ignore the key.
//
// Headers and the query are not hashed, so they must be exact by construction. JSON strings
// cannot hold invalid UTF-8, which Go's server accepts in a header value, so a header set or
// query that holds any is written as bytes instead: HX and QX, base64 on disk.
type captureRec struct {
	O  *bodyRec            `json:"o,omitempty"`
	W  *bodyRec            `json:"w,omitempty"`
	H  http.Header         `json:"h,omitempty"`
	HX map[string][][]byte `json:"hx,omitempty"`
	Q  string              `json:"q,omitempty"`
	QX []byte              `json:"qx,omitempty"`
	// R is the response, on a response event. Its body is one block record, whole: a response
	// is not re-sent from one request to the next, so splitting it would save nothing. As a
	// block it is skipped unread by an event read, as every block is.
	R *responseRec `json:"r,omitempty"`
}

// responseRec is a ResponseCapture on disk. B is the key of the block holding the body, empty for
// an empty body; S is the hex SHA-256 of the body, checked on read.
type responseRec struct {
	C  int         `json:"c"`
	H  http.Header `json:"h,omitempty"`
	B  string      `json:"b,omitempty"`
	S  string      `json:"s"`
	T  bool        `json:"t,omitempty"`
	IC bool        `json:"ic,omitempty"`
	D  bool        `json:"d,omitempty"`
}

// bodyRec is one body's skeleton. P alternates literal bytes and block references, starting and
// ending with a literal: literal, ref, literal, ..., literal. S is the hex SHA-256 of the body,
// the form a body_rewritten invocation records, so the two can be matched.
type bodyRec struct {
	S string   `json:"s"`
	P []string `json:"p"`
}

// splitBody cuts body into a skeleton and blocks, and returns the blocks not yet in seen, which
// it adds to seen. seen is the segment's set of written blocks, kept apart from its strings so a
// block and a string with the same bytes are each written under their own kind.
//
// What is a block, for any JSON object body: each top-level string, each element of each
// top-level array, and — for an element that is an object with a "content" array — each element
// of that array and each other field of the element instead. That is the system prompt and its
// blocks, each tool definition, and each message or message content block, in Anthropic
// Messages, OpenAI Chat Completions and Responses alike. Cutting a message into its content
// blocks keeps a moving cache_control marker from making the whole message new on every
// request. A body that is not a JSON object, or not valid UTF-8, is one block: still exact,
// only not deduplicated within itself.
func splitBody(body []byte, seen map[string]struct{}) (*bodyRec, []stringRec) {
	var cuts [][2]int
	cut := func(v gjson.Result) {
		start, end := v.Index, v.Index+len(v.Raw)
		// Index is zero when gjson could not place the value, so check the bytes are there.
		if len(v.Raw) < session.InternMinLen || end > len(body) || string(body[start:end]) != v.Raw {
			return
		}
		if n := len(cuts); n > 0 && start < cuts[n-1][1] {
			return
		}
		cuts = append(cuts, [2]int{start, end})
	}
	if root := gjson.ParseBytes(body); utf8.Valid(body) && root.IsObject() && json.Valid(body) {
		root.ForEach(func(_, v gjson.Result) bool {
			switch {
			case v.IsArray():
				v.ForEach(func(_, el gjson.Result) bool {
					if c := el.Get("content"); el.IsObject() && c.IsArray() {
						el.ForEach(func(k, f gjson.Result) bool {
							if k.Str == "content" && f.IsArray() {
								f.ForEach(func(_, b gjson.Result) bool { cut(b); return true })
							} else {
								cut(f)
							}
							return true
						})
					} else {
						cut(el)
					}
					return true
				})
			case v.Type == gjson.String:
				cut(v)
			}
			return true
		})
	} else if len(body) > 0 {
		cuts = [][2]int{{0, len(body)}}
	}

	sum := sha256.Sum256(body)
	b := &bodyRec{S: hex.EncodeToString(sum[:]), P: make([]string, 0, 2*len(cuts)+1)}
	var fresh []stringRec
	at := 0
	for _, c := range cuts {
		v := string(body[c[0]:c[1]])
		h := hashOf(v)
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			fresh = append(fresh, stringRec{h: h, v: v})
		}
		b.P = append(b.P, string(body[at:c[0]]), h)
		at = c[1]
	}
	b.P = append(b.P, string(body[at:]))
	return b, fresh
}

// rebuild is the body b is the skeleton of, from the segment's blocks. It fails when a block is
// missing or the result does not hash to what was captured: corruption, never a near miss.
func (b *bodyRec) rebuild(blocks map[string]string) ([]byte, error) {
	if len(b.P)%2 != 1 {
		return nil, fmt.Errorf("archive: capture skeleton has %d pieces, want an odd number", len(b.P))
	}
	var out bytes.Buffer
	for i, p := range b.P {
		if i%2 == 0 {
			out.WriteString(p)
			continue
		}
		v, ok := blocks[p]
		if !ok {
			return nil, fmt.Errorf("archive: capture block %s is not in the segment", p)
		}
		out.WriteString(v)
	}
	sum := sha256.Sum256(out.Bytes())
	if hex.EncodeToString(sum[:]) != b.S {
		return nil, errors.New("archive: rebuilt body does not match the captured hash")
	}
	// Never nil: a forwarded body rewritten to nothing is not "no rewrite".
	return append([]byte{}, out.Bytes()...), nil
}

// refs is every block b refers to.
func (b *bodyRec) refs() []string {
	var out []string
	for i := 1; i < len(b.P); i += 2 {
		out = append(out, b.P[i])
	}
	return out
}

// splitCapture is c as an event record carries it, and the blocks it adds to seen.
func splitCapture(c *pipeline.RequestCapture, seen map[string]struct{}) (*captureRec, []stringRec) {
	if c == nil {
		return nil, nil
	}
	if r := c.Response; r != nil {
		sum := sha256.Sum256(r.Body)
		rec := &captureRec{R: &responseRec{C: r.Status, H: r.Header, S: hex.EncodeToString(sum[:]),
			T: r.Truncated, IC: r.Incomplete, D: r.Decoded}}
		var fresh []stringRec
		if len(r.Body) > 0 {
			v := string(r.Body)
			rec.R.B = hashOf(v)
			if _, ok := seen[rec.R.B]; !ok {
				seen[rec.R.B] = struct{}{}
				fresh = append(fresh, stringRec{h: rec.R.B, v: v})
			}
		}
		return rec, fresh
	}
	o, fresh := splitBody(c.Original, seen)
	f := &captureRec{O: o, H: c.Header, Q: c.Query}
	if !utf8.ValidString(c.Query) {
		f.Q, f.QX = "", []byte(c.Query)
	}
	for _, vs := range c.Header {
		if slices.ContainsFunc(vs, func(v string) bool { return !utf8.ValidString(v) }) {
			f.H, f.HX = nil, make(map[string][][]byte, len(c.Header))
			for k, vs := range c.Header {
				var bs [][]byte
				if vs != nil {
					bs = make([][]byte, 0, len(vs))
				}
				for _, v := range vs {
					bs = append(bs, []byte(v))
				}
				f.HX[k] = bs
			}
			break
		}
	}
	if c.Forwarded != nil {
		var more []stringRec
		f.W, more = splitBody(c.Forwarded, seen)
		fresh = append(fresh, more...)
	}
	return f, fresh
}

// blockLine is a block record. JSON strings cannot hold invalid UTF-8, so such a block is
// written as base64 instead.
func blockLine(s stringRec) ([]byte, error) {
	if utf8.ValidString(s.v) {
		return appendLine(record{K: kindBlock, H: s.h, V: s.v})
	}
	return appendLine(record{K: kindBlock, H: s.h, X: []byte(s.v)})
}

// readCapture returns the capture of the event numbered seq in the segment at path, rebuilt and
// verified. It is nil, with no error, when the segment does not hold that event or the event
// carries no capture: an event written before lossless capture, or not a request.
//
// TWO PASSES, so a read holds only the blocks of the one request: the first finds the event's
// skeletons, the second keeps just the blocks they name. Blocks precede the event that first
// uses them, so the second pass stops at the event.
func readCapture(path string, seq uint64) (*pipeline.RequestCapture, error) {
	var raw json.RawMessage
	if _, err := readRecords(path, nil, nil, func(e *pipeline.SessionEvent, c json.RawMessage) bool {
		if e.Seq == seq {
			raw = c
		}
		return e.Seq < seq
	}); err != nil || raw == nil {
		return nil, err
	}
	var f captureRec
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("archive: capture record: %w", err)
	}
	if f.O == nil && f.R == nil {
		return nil, errors.New("archive: capture record has no body")
	}
	want := map[string]struct{}{}
	if f.R != nil && f.R.B != "" {
		want[f.R.B] = struct{}{}
	}
	if f.O != nil {
		for _, h := range f.O.refs() {
			want[h] = struct{}{}
		}
	}
	if f.W != nil {
		for _, h := range f.W.refs() {
			want[h] = struct{}{}
		}
	}
	blocks := map[string]string{}
	if len(want) > 0 {
		if _, err := readRecords(path, want, blocks, func(e *pipeline.SessionEvent, _ json.RawMessage) bool {
			return e.Seq < seq
		}); err != nil {
			return nil, err
		}
	}
	if r := f.R; r != nil {
		body := []byte(blocks[r.B])
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != r.S {
			return nil, errors.New("archive: response body does not match the captured hash")
		}
		return &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: r.C, Header: r.H,
			Body: body, Truncated: r.T, Incomplete: r.IC, Decoded: r.D}}, nil
	}
	c := &pipeline.RequestCapture{Header: f.H, Query: f.Q}
	if f.QX != nil {
		c.Query = string(f.QX)
	}
	if f.HX != nil {
		c.Header = make(http.Header, len(f.HX))
		for k, vs := range f.HX {
			var ss []string
			if vs != nil {
				ss = make([]string, 0, len(vs))
			}
			for _, v := range vs {
				ss = append(ss, string(v))
			}
			c.Header[k] = ss
		}
	}
	var err error
	if c.Original, err = f.O.rebuild(blocks); err != nil {
		return nil, err
	}
	if f.W != nil {
		if c.Forwarded, err = f.W.rebuild(blocks); err != nil {
			return nil, err
		}
	}
	return c, nil
}

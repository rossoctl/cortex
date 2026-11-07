package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/core/model"
)

// Find returns, ascending, the seqs of id's archived events that match, and the newest seq it
// read. An event matches when event reports it, or when text reports a piece of its capture: a
// body block, a key or a value between blocks, a header name or value, the query, or a response's
// status. ok is false when the archive holds nothing under id.
//
// ONE READ OF EACH SEGMENT. Every request re-sends the conversation, so its body is mostly
// blocks that an earlier request wrote. text sees each block one time for each segment, and an
// event matches through the blocks it uses. No body is rebuilt.
//
// A block that the detail view shows as a label is not checked: an image or a document content
// block, and a block stored as bytes (a body that is not UTF-8). Their text is base64 or binary,
// which the operator never sees.
//
// segment, when not nil, runs before each segment is read. A segment's strings are not shared
// with the next segment, so a caller that keeps a result for each string drops it there, and the
// memory of one segment is not held for the rest of the search.
//
// Like Page, it runs on the caller's goroutine. It stops, with ctx's error, when ctx ends.
func (a *Archive) Find(ctx context.Context, id string, event func(*model.SessionEvent) bool,
	segment func(), text func(string) bool) (seqs []uint64, last uint64, ok bool, err error) {
	e, ok := a.entry(id)
	if !ok || len(e.segments) == 0 {
		return nil, 0, false, nil
	}
	for _, seg := range e.segments {
		if segment != nil {
			segment()
		}
		hit := map[string]bool{}
		_, rerr := readRecordsWith(filepath.Join(e.dir, seg.File), nil, nil, func(rec *record) {
			hit[rec.H] = rec.X == nil && !mediaBlock(rec.V) && MatchJSON(rec.V, text)
		}, func(ev *model.SessionEvent, c json.RawMessage) bool {
			if ctx.Err() != nil {
				return false
			}
			last = max(last, ev.Seq)
			if event(ev) || captureMatches(c, hit, text) {
				seqs = append(seqs, ev.Seq)
			}
			return true
		})
		if errors.Is(rerr, fs.ErrNotExist) {
			continue // pruned or renamed since the snapshot, as in Page
		}
		if rerr != nil {
			return seqs, last, true, rerr
		}
		if cerr := ctx.Err(); cerr != nil {
			return seqs, last, true, cerr
		}
	}
	return seqs, last, true, nil
}

// mediaBlock reports whether v is an image or a document content block. The detail view shows one
// as a label, at any depth: Claude Code returns a read image inside a tool_result.
func mediaBlock(v string) bool {
	t := gjson.Get(v, "type").Str
	return (t == "image" || t == "document") && gjson.Get(v, "source").IsObject()
}

// MatchJSON reports whether text matches v as the detail view shows it: each key and value of v
// when v is JSON, v itself when it is not. A capture block is a value cut out of a body, so most
// are JSON. The search route also uses it for a tool's parameters.
func MatchJSON(v string, text func(string) bool) bool {
	if !gjson.Valid(v) {
		return text(v)
	}
	return jsonMatches(gjson.Parse(v), text)
}

func jsonMatches(r gjson.Result, text func(string) bool) bool {
	switch {
	case r.IsObject() && mediaBlock(r.Raw):
		return false
	case r.IsObject() || r.IsArray():
		found, obj := false, r.IsObject()
		r.ForEach(func(k, v gjson.Result) bool {
			found = (obj && text(k.Str)) || jsonMatches(v, text)
			return !found
		})
		return found
	case r.Type == gjson.String:
		return text(r.Str)
	}
	return text(r.Raw) // a number, true, false or null, as the tree writes it
}

// captureMatches reports whether text matches c, an event's capture record: a block it uses, a key
// or a value between its blocks, a header, the query, or a response's status.
func captureMatches(c json.RawMessage, hit map[string]bool, text func(string) bool) bool {
	if c == nil {
		return false
	}
	var f captureRec
	if json.Unmarshal(c, &f) != nil {
		return false
	}
	for _, b := range []*bodyRec{f.O, f.W} {
		if b != nil && bodyMatches(b, hit, text) {
			return true
		}
	}
	if headersMatch(f.H, text) || (f.Q != "" && text(f.Q)) {
		return true
	}
	if r := f.R; r != nil {
		return text(strconv.Itoa(r.C)) || headersMatch(r.H, text) || hit[r.B]
	}
	return false
}

// bodyMatches reports whether a body's skeleton matches: a block it names, or a key or a value
// between them.
func bodyMatches(b *bodyRec, hit map[string]bool, text func(string) bool) bool {
	for i, p := range b.P {
		if i%2 == 1 {
			if hit[p] {
				return true
			}
		} else if skeletonMatches(p, text) {
			return true
		}
	}
	return false
}

// skeletonMatches reports whether text matches a key or a value in p, the JSON of a body between
// two blocks: a string decoded, any other value as its JSON text. As in a block, the quotes and
// the punctuation around them are not checked. A block is a whole value, so p cuts no string in
// two.
func skeletonMatches(p string, text func(string) bool) bool {
	const punct = "{}[]:, \t\r\n"
	for i := 0; i < len(p); {
		switch {
		case p[i] == '"':
			j := i + 1
			for ; j < len(p) && p[j] != '"'; j++ {
				if p[j] == '\\' {
					j++
				}
			}
			if j < len(p) {
				s := p[i+1 : j]
				if strings.IndexByte(s, '\\') >= 0 {
					_ = json.Unmarshal([]byte(p[i:j+1]), &s) // only an escape needs the decoder
				}
				if text(s) {
					return true
				}
			}
			i = j + 1
		case strings.IndexByte(punct, p[i]) >= 0:
			i++
		default:
			j := i
			for j < len(p) && p[j] != '"' && strings.IndexByte(punct, p[j]) < 0 {
				j++
			}
			if text(p[i:j]) {
				return true
			}
			i = j
		}
	}
	return false
}

// headersMatch reports whether a header name, or its values as the detail view joins them,
// matches.
func headersMatch(h http.Header, text func(string) bool) bool {
	for k, vs := range h {
		if text(k) || text(strings.Join(vs, ", ")) {
			return true
		}
	}
	return false
}

package archive

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

// The record kinds a segment line can carry. A reader skips any other kind, so a later version
// can add one without breaking an older reader. kindBlock is the newest: a block of a captured
// request body, see capture.go.
const (
	kindString = "s"
	kindEvent  = "e"
	kindBlock  = "b"
)

// record is one line of a segment.
//
// A string record carries H and V: a string the segment defines once, under its hash. A block
// record is the same for a block of a captured body, with X in place of V for a block that is
// not valid UTF-8. An event record carries E, the event with its referenced fields blanked, R,
// which of them were referenced, and F, the request's capture when it has one. One struct for
// every kind so a reader decodes a line without knowing its kind first.
type record struct {
	K string              `json:"k"`
	H string              `json:"h,omitempty"`
	V string              `json:"v,omitempty"`
	X []byte              `json:"x,omitempty"`
	E *model.SessionEvent `json:"e,omitempty"`
	R *refs               `json:"r,omitempty"`
	// F stays undecoded until a capture is asked for: an event read has no use for it.
	F json.RawMessage `json:"f,omitempty"`
}

// refs names, BY POSITION, which of an event's deduplicated strings were written as
// references. A nil entry means the value is inline in the event; an all-nil array is
// omitted.
//
// POSITIONAL RATHER THAN A SENTINEL STRING in the blanked field, because any sentinel is a
// string some prompt can contain. A position cannot collide with content.
//
// The fields are exactly the seven core/session.Interner shares in memory — message content,
// the completion, tool descriptions, tool schemas, tool results, history tool-call arguments,
// A2A part content — and nothing else, so the two dedups are of the same thing. MCP params and results stay inline for the
// interner's reason: they are maps, unmeasured on this workload.
//
// TA is the newest, and TR the one before it. A reader that predates a key ignores it, so a
// value long enough to have been written as a reference reads back empty there; every other
// field is intact.
type refs struct {
	M  []*string `json:"m,omitempty"`  // Inference.Messages[i].Content
	C  *string   `json:"c,omitempty"`  // Inference.Completion
	TD []*string `json:"td,omitempty"` // Inference.Tools[i].Description
	TP []*string `json:"tp,omitempty"` // Inference.Tools[i].Parameters
	TR []*string `json:"tr,omitempty"` // Inference.ToolResults[i].Content
	TA []*string `json:"ta,omitempty"` // Inference.HistoryToolCalls[i].Arguments
	AP []*string `json:"ap,omitempty"` // A2A.Parts[i].Content
}

// stringRec is a string a segment has not defined yet, keyed by its hash.
type stringRec struct{ h, v string }

// hashOf is a string's key in a segment: the first 16 bytes of its SHA-256, base64url without
// padding — 22 characters. 128 bits make an accidental collision across a session's strings
// negligible, and the strings are the user's own traffic, so there is no adversary to defend
// a shorter key against.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// dirName is the directory a session's segments live in: the hex of the first 16 bytes of the
// SHA-256 of its id. A session id is up to session.MaxSessionIDLen bytes of anything a client
// sent, so it cannot be a path element itself; its hash always can, at a fixed 32 characters.
func dirName(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:16])
}

// split returns e with every deduplicated field of at least session.InternMinLen bytes blanked,
// the references to them, and the strings among them not yet in seen, which it adds to seen.
//
// IT NEVER MUTATES e. The event is the store's — it is held there and served from there — and
// shares its slices with the store's copy, so split clones each extension and slice before
// blanking anything, as core/session.Interner.InternEvent does for the same reason.
//
// refs is nil when nothing was referenced, so a contentless event (a tunnel-open, a denial)
// costs no "r" key at all.
func split(e model.SessionEvent, seen map[string]struct{}) (model.SessionEvent, *refs, []stringRec) {
	var r refs
	var fresh []stringRec
	hit := false
	ref := func(s *string) *string {
		if len(*s) < session.InternMinLen {
			return nil
		}
		h := hashOf(*s)
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			fresh = append(fresh, stringRec{h: h, v: *s})
		}
		*s = ""
		hit = true
		return &h
	}

	if e.Inference != nil {
		cp := *e.Inference
		cp.Messages = slices.Clone(cp.Messages)
		r.M = make([]*string, len(cp.Messages))
		for i := range cp.Messages {
			r.M[i] = ref(&cp.Messages[i].Content)
		}
		r.C = ref(&cp.Completion)
		cp.Tools = slices.Clone(cp.Tools)
		r.TD = make([]*string, len(cp.Tools))
		r.TP = make([]*string, len(cp.Tools))
		for i := range cp.Tools {
			r.TD[i] = ref(&cp.Tools[i].Description)
			p := string(cp.Tools[i].Parameters)
			r.TP[i] = ref(&p)
			cp.Tools[i].Parameters = model.RawJSON(p)
		}
		cp.ToolResults = slices.Clone(cp.ToolResults)
		r.TR = make([]*string, len(cp.ToolResults))
		for i := range cp.ToolResults {
			r.TR[i] = ref(&cp.ToolResults[i].Content)
		}
		cp.HistoryToolCalls = slices.Clone(cp.HistoryToolCalls)
		r.TA = make([]*string, len(cp.HistoryToolCalls))
		for i := range cp.HistoryToolCalls {
			r.TA[i] = ref(&cp.HistoryToolCalls[i].Arguments)
		}
		e.Inference = &cp
	}
	if e.A2A != nil {
		cp := *e.A2A
		cp.Parts = slices.Clone(cp.Parts)
		r.AP = make([]*string, len(cp.Parts))
		for i := range cp.Parts {
			r.AP[i] = ref(&cp.Parts[i].Content)
		}
		e.A2A = &cp
	}
	if !hit {
		return e, nil, fresh
	}
	r.M, r.TD, r.TP, r.TR, r.TA, r.AP = nilIfAllNil(r.M), nilIfAllNil(r.TD), nilIfAllNil(r.TP), nilIfAllNil(r.TR),
		nilIfAllNil(r.TA), nilIfAllNil(r.AP)
	return e, &r, fresh
}

func nilIfAllNil(xs []*string) []*string {
	for _, x := range xs {
		if x != nil {
			return xs
		}
	}
	return nil
}

// join restores the fields r references from table, in place. It reports false when a
// reference names a string the table does not hold, or a position the event does not have —
// both corruption, which a reader treats like a torn tail rather than serving a blanked field
// as if it were the original.
func join(e *model.SessionEvent, r *refs, table map[string]string) bool {
	if r == nil {
		return true
	}
	set := func(dst *string, h *string) bool {
		if h == nil {
			return true
		}
		v, ok := table[*h]
		if ok {
			*dst = v
		}
		return ok
	}
	if e.Inference == nil {
		if r.M != nil || r.C != nil || r.TD != nil || r.TP != nil || r.TR != nil || r.TA != nil {
			return false
		}
	} else {
		inf := e.Inference
		if len(r.M) > len(inf.Messages) || len(r.TD) > len(inf.Tools) || len(r.TP) > len(inf.Tools) ||
			len(r.TR) > len(inf.ToolResults) || len(r.TA) > len(inf.HistoryToolCalls) {
			return false
		}
		for i, h := range r.M {
			if !set(&inf.Messages[i].Content, h) {
				return false
			}
		}
		if !set(&inf.Completion, r.C) {
			return false
		}
		for i, h := range r.TD {
			if !set(&inf.Tools[i].Description, h) {
				return false
			}
		}
		for i, h := range r.TP {
			p := string(inf.Tools[i].Parameters)
			if !set(&p, h) {
				return false
			}
			inf.Tools[i].Parameters = model.RawJSON(p)
		}
		for i, h := range r.TR {
			if !set(&inf.ToolResults[i].Content, h) {
				return false
			}
		}
		for i, h := range r.TA {
			if !set(&inf.HistoryToolCalls[i].Arguments, h) {
				return false
			}
		}
	}
	if e.A2A == nil {
		return r.AP == nil
	}
	if len(r.AP) > len(e.A2A.Parts) {
		return false
	}
	for i, h := range r.AP {
		if !set(&e.A2A.Parts[i].Content, h) {
			return false
		}
	}
	return true
}

// stringLine and eventLine are the lines a segment writes for an event, each with its trailing
// newline; blockLine, in capture.go, is the third.
func stringLine(s stringRec) ([]byte, error) {
	return appendLine(record{K: kindString, H: s.h, V: s.v})
}

func eventLine(e model.SessionEvent, r *refs, f *captureRec) ([]byte, error) {
	rec := record{K: kindEvent, E: &e, R: r}
	if f != nil {
		b, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		rec.F = b
	}
	return appendLine(rec)
}

func appendLine(rec record) ([]byte, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

package cpex

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/internal/parsercommon"
)

// Violation codes for requests the plugin refuses to present to CPEX.
const (
	// codeUnnamedEntity: an MCP tool call, prompt or resource read that
	// names no tool, prompt or URI, so no route can be chosen for it.
	codeUnnamedEntity = "unnamed_entity"
	// codeAmbiguousRequest: a parsed JSON body with keys that collide under
	// case folding or repeat. The parsers match keys case-insensitively and
	// keep the last duplicate; a server may not, so policy could judge a
	// different request from the one the server runs.
	codeAmbiguousRequest = "ambiguous_request"
	// codeUnparseableRequest: a body no parser claimed, sent to a
	// strict_hosts destination, that is a JSON object the parsers cannot
	// read (UTF-16 or UTF-32, a byte order mark, NaN, Infinity, a number
	// out of float64 range, a truncated object, colliding keys, a method
	// that is not one string). The parsers decline such a
	// body, so it would reach CPEX as global traffic, while a lenient
	// server (Python's json, for one) may still read it as the tool call
	// no route judged.
	codeUnparseableRequest = "unparseable_request"
)

// unpresentableRequest returns the violation code for a request that must
// be denied before CPEX sees it, or "". Call it on the request phase only.
// strict is whether the request goes to a strict_hosts destination, the
// only requests whose unclaimed bodies are checked by unparseableObject.
func unpresentableRequest(pctx *pipeline.Context, strict bool) string {
	if m := pctx.Extensions.MCP; m != nil {
		switch m.Method {
		case "tools/call", "prompts/get":
			if mcpParamName(m.Params) == "" {
				return codeUnnamedEntity
			}
		case "resources/read":
			if mcpParamURI(m.Params) == "" {
				return codeUnnamedEntity
			}
		}
	}
	parsed := pctx.Extensions.MCP != nil || pctx.Extensions.A2A != nil || pctx.Extensions.Inference != nil
	// Check the bytes the parsers parsed: each decodes a zstd body first
	// with the same DecodeRequestBody, so the raw bytes would not be JSON.
	// A body that does not decode stays as sent, and so is refused.
	if len(pctx.Body) == 0 {
		return ""
	}
	body := parsercommon.DecodeRequestBody(pctx.Headers, pctx.Body)
	if parsed && ambiguousJSON(body) {
		return codeAmbiguousRequest
	}
	if strict && !parsed && unparseableObject(body) {
		return codeUnparseableRequest
	}
	return ""
}

// unparseableObject reports whether body opens with a JSON object that the
// parsers could not read as a lenient tool server might: one in UTF-16 or
// UTF-32 or after a byte order mark (Python's json.loads(bytes) reads all
// three; no parser here does), or whose first value encoding/json cannot
// decode (NaN, Infinity, a number out of float64 range, a truncated
// object; invalid UTF-8 it replaces with U+FFFD rather than rejects), or
// whose keys collide (see ambiguousJSON), or whose top-level "method"
// key, in any case, does not decode as a string. A parser declines such
// a body, so it would reach CPEX as global traffic while the server may
// still run it as the tool call no route judged.
// Nothing about the body's own shape decides this, so a caller cannot opt
// out by dropping or renaming a key; the caller limits it to strict_hosts
// instead. Only the first value is read: what follows it (NDJSON, a stream
// of values) does not make the body unparseable.
func unparseableObject(body []byte) bool {
	if wide := wideJSON(body); wide != nil {
		trimmed := bytes.TrimLeft(wide, " \t\r\n")
		return len(trimmed) > 0 && trimmed[0] == '{'
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	withoutBOM := bytes.TrimPrefix(trimmed, []byte("\xef\xbb\xbf"))
	if len(withoutBOM) != len(trimmed) {
		withoutBOM = bytes.TrimLeft(withoutBOM, " \t\r\n")
		return len(withoutBOM) > 0 && withoutBOM[0] == '{'
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	if undecodableJSON(trimmed) {
		return true
	}
	var first json.RawMessage
	if json.NewDecoder(bytes.NewReader(trimmed)).Decode(&first) != nil {
		return true
	}
	if !utf8.Valid(first) || ambiguousJSON(first) {
		return true
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(first, &keys) != nil {
		return true
	}
	for k := range keys {
		if strings.EqualFold(k, "method") {
			var rpc struct {
				Method string `json:"method"`
			}
			return json.Unmarshal(first, &rpc) != nil
		}
	}
	return false
}

// wideJSON returns body transcoded to UTF-8 when its first bytes mark it
// as UTF-16 or UTF-32 the way Python's json.detect_encoding reads them (a
// byte order mark, or the NUL pattern ASCII leaves), and nil otherwise.
// Bytes that do not transcode become U+FFFD; only the leading structure
// matters to the caller.
func wideJSON(b []byte) []byte {
	var unit int
	var big bool
	switch {
	case bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}):
		unit, big, b = 4, true, b[4:]
	case bytes.HasPrefix(b, []byte{0xff, 0xfe, 0, 0}):
		unit, big, b = 4, false, b[4:]
	case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		unit, big, b = 2, true, b[2:]
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
		unit, big, b = 2, false, b[2:]
	case len(b) >= 4 && b[0] == 0 && b[1] == 0:
		unit, big = 4, true
	case len(b) >= 4 && b[0] == 0:
		unit, big = 2, true
	case len(b) >= 4 && b[1] == 0 && b[2] == 0 && b[3] == 0:
		unit, big = 4, false
	case len(b) >= 2 && b[1] == 0:
		unit, big = 2, false
	default:
		return nil
	}
	units := make([]uint32, 0, len(b)/unit)
	for i := 0; i+unit <= len(b); i += unit {
		var u uint32
		for j := 0; j < unit; j++ {
			k := j
			if !big {
				k = unit - 1 - j
			}
			u = u<<8 | uint32(b[i+k])
		}
		units = append(units, u)
	}
	var out []byte
	for i := 0; i < len(units); i++ {
		r := rune(units[i])
		if unit == 2 && utf16.IsSurrogate(r) && i+1 < len(units) {
			if d := utf16.DecodeRune(r, rune(units[i+1])); d != utf8.RuneError {
				r = d
				i++
			}
		}
		out = utf8.AppendRune(out, r)
	}
	return out
}

// undecodableJSON reports whether encoding/json's tokenizer rejects the
// first value in body or finds it incomplete. What follows that value is
// not read.
func undecodableJSON(body []byte) bool {
	// Tokens, not a decoded tree: a number decodes to float64 here as it
	// does in the parsers, so one out of range is an error too.
	dec := json.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return true
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return false
		}
	}
}

// ambiguousJSON reports whether body is not valid JSON, or has an object
// with a repeated key, or two keys equal under Unicode case folding in an
// object the parsers decode into Go structs or a server commonly binds to
// one (MCP tool arguments, at any depth; see shapeOf). Everywhere else
// (tool schemas, A2A data parts, metadata, response formats) keys are data,
// which every side decodes as a map, so only exact repeats count there.
func ambiguousJSON(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		keys      map[string]struct{} // nil for an array
		expectKey bool
		lastKey   string
		// shape is the object's own shape, or for an array the shape of
		// the objects it holds.
		shape jsonShape
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return len(stack) != 0
		}
		if err != nil {
			return true
		}
		var top *frame
		if n := len(stack); n > 0 {
			top = stack[n-1]
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				f := &frame{expectKey: d == '{', shape: shapeEnvelope}
				if d == '{' {
					f.keys = map[string]struct{}{}
				}
				switch {
				case top == nil:
				case top.keys != nil:
					f.shape = shapeOf(top.shape, top.lastKey, d == '[')
					top.expectKey = false
				case d == '{':
					f.shape = top.shape
				case top.shape == shapeArguments:
					f.shape = shapeArguments
				default:
					f.shape = shapeData // an array in an array
				}
				stack = append(stack, f)
			default:
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].keys != nil {
					stack[n-1].expectKey = true
				}
			}
			continue
		}
		if top == nil || top.keys == nil {
			continue
		}
		if !top.expectKey {
			top.expectKey = true
			continue
		}
		key, _ := tok.(string)
		k := key
		if top.shape != shapeData {
			k = foldKey(key)
		}
		if _, dup := top.keys[k]; dup {
			return true
		}
		top.keys[k] = struct{}{}
		top.lastKey = key
		top.expectKey = false
	}
}

// jsonShape names an object the parsers decode into a Go struct, whose
// fields encoding/json matches case-insensitively. shapeData is any other
// object.
type jsonShape int

const (
	shapeData          jsonShape = iota
	shapeEnvelope                // the body: JSON-RPC or an inference request
	shapeParams                  // JSON-RPC params (MCP, A2A)
	shapeA2AMessage              // A2A params.message
	shapeA2APart                 // A2A message part
	shapeMessage                 // inference messages[] item
	shapeBlock                   // inference content block, Responses content part
	shapeItem                    // Responses input[] item
	shapeToolNamespace           // Responses input[].tools[] namespace
	shapeTool                    // inference tools[] item, Responses tool def
	shapeFunction                // OpenAI tools[].function
	// shapeArguments is MCP params.arguments and everything under it. A
	// server commonly binds arguments into a struct with encoding/json, so
	// keys fold at every depth.
	shapeArguments
)

// member is the shape of the value under one key of a struct-decoded
// object: an object of that shape, or (list) an array of such objects.
type member struct {
	shape jsonShape
	list  bool
}

// shapeTransitions maps a struct-decoded object's shape and the folded key
// of one of its members to the shape of the value under that key. Members
// not listed, and values of the other kind (an object where an array is
// decoded, or the reverse, which the parsers do not decode as structs), are
// data.
var shapeTransitions = func() map[jsonShape]map[string]member {
	obj := func(s jsonShape) member { return member{shape: s} }
	list := func(s jsonShape) member { return member{shape: s, list: true} }
	t := map[jsonShape]map[string]member{
		shapeEnvelope: {
			"params":   obj(shapeParams),
			"messages": list(shapeMessage),
			"input":    list(shapeItem),
			"tools":    list(shapeTool),
			"system":   list(shapeBlock),
		},
		shapeParams:        {"message": obj(shapeA2AMessage), "arguments": obj(shapeArguments)},
		shapeA2AMessage:    {"parts": list(shapeA2APart)},
		shapeMessage:       {"content": list(shapeBlock)},
		shapeBlock:         {"content": list(shapeBlock)},
		shapeItem:          {"content": list(shapeBlock), "output": list(shapeBlock), "tools": list(shapeToolNamespace)},
		shapeToolNamespace: {"tools": list(shapeTool)},
		shapeTool:          {"function": obj(shapeFunction)},
	}
	folded := make(map[jsonShape]map[string]member, len(t))
	for shape, members := range t {
		folded[shape] = make(map[string]member, len(members))
		for k, v := range members {
			folded[shape][foldKey(k)] = v
		}
	}
	return folded
}()

// shapeOf is the shape of the object, or with list of the objects in the
// array, under key in an object of shape parent.
func shapeOf(parent jsonShape, key string, list bool) jsonShape {
	if parent == shapeArguments {
		return shapeArguments
	}
	m, ok := shapeTransitions[parent][foldKey(key)]
	if !ok || m.list != list {
		return shapeData
	}
	return m.shape
}

// foldKey maps s to a key equal for exactly the strings strings.EqualFold
// treats as equal: each rune becomes the smallest rune of its case-folding
// orbit.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}

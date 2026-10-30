package cpex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/internal/parsercommon"
	"github.com/rossoctl/cortex/core/session"
)

// isStreamingResponseGap reports whether a response-phase invocation has a
// non-empty body the policy engine can't inspect: an MCP or A2A response
// that yielded zero content parts (a stream), or an inference response that
// could not be read as its dialect. An inference response read cleanly may
// carry no parts (an empty completion, an error body). Callers fail closed.
func isStreamingResponseGap(pctx *pipeline.Context, isResponse bool, cmfPartCount int) bool {
	if !isResponse || len(pctx.ResponseBody) == 0 {
		return false
	}
	if pctx.Extensions.MCP == nil && pctx.Extensions.Inference != nil {
		return !readInferenceResponse(pctx).decoded
	}
	if cmfPartCount > 0 {
		return false
	}
	return pctx.Extensions.MCP != nil || pctx.Extensions.A2A != nil
}

// Values of the session_source config key.
const (
	sessionSourceHeader   = "header"
	sessionSourceResolved = "resolved"
)

// cpexSessionID returns the session id CPEX keys session state (taint
// labels) on, read by CPEX's session resolver from Agent.SessionID, and
// whether it replaces the A2A context id. Tag-free so it is unit-tested
// without cgo.
//
// Under "header" it is the X-Session-Id request header, which an upstream
// sets so session-scoped state persists across the separate requests of
// one logical session; the A2A context id takes precedence. Under
// "resolved" it is trustedSessionID, which replaces any A2A context id:
// both that and X-Session-Id are chosen by the caller.
func cpexSessionID(pctx *pipeline.Context, source string) (id string, replaces bool) {
	if source == sessionSourceResolved {
		return trustedSessionID(pctx), true
	}
	return pctx.Headers.Get(session.SessionIDHeader), false
}

// trustedSessionID returns the listener's session id when a dedicated
// agent session header or the process chain named it, else "". A guess
// (the most recent session, User-Agent affinity, a pending or the default
// bucket) is not an identity a caller can be held to, and the generic
// X-Session-Id and Session-Id headers let any caller name a fresh session.
// The session a parent agent named (ChainSession) wins over one the sender
// named itself, so a child process cannot name a fresh session to leave
// its parent's taint. ChainSession is set only where the listener has
// process attribution (cortex bound to loopback, as the laptop install is);
// in the authbridge-cpex sidecar it is always empty, so a dedicated header,
// which the sender sets, is trusted as named.
func trustedSessionID(pctx *pipeline.Context) string {
	if id := pctx.ChainSession; session.IsConversation(id) {
		return id
	}
	if pctx.Session == nil {
		return ""
	}
	id := pctx.Session.ID
	if !session.IsConversation(id) {
		return ""
	}
	switch pctx.SessionSource {
	case pipeline.SessionSourceProcess:
		return id
	case pipeline.SessionSourceHeader:
		switch http.CanonicalHeaderKey(pctx.SessionHeader) {
		case "", session.SessionIDHeader, http.CanonicalHeaderKey(session.CodexSessionHeader):
			return ""
		}
		return id
	}
	return ""
}

// cmfPartKind enumerates the structured CMF content part buildCMF emits
// for a given MCP message. Kept protocol-neutral (no rcpex import) so the
// MCP→CMF decision is unit-testable without cgo/FFI; the cgo adapter
// (manager_cpex.go) converts a cmfPart into the matching rcpex ContentPart.
type cmfPartKind int

const (
	// cmfPartText is the opaque text fallback. Used for protocol
	// mechanics (tools/list, initialize, …) and when no MCP extension
	// was parsed — CPEX gets the raw body as a single text part, no
	// per-tool route matches, and the request flows unchanged. This
	// preserves pre-structured-mapping behavior for non-action traffic.
	cmfPartText cmfPartKind = iota
	cmfPartToolCall
	cmfPartPromptRequest
	cmfPartResourceRef
	cmfPartToolResult
)

// cmfPart is a protocol-neutral description of one CMF content part. The
// cgo adapter maps it onto an rcpex constructor (NewToolCallPart, …). Only
// the fields relevant to Kind are populated.
type cmfPart struct {
	Kind cmfPartKind

	// Name is the tool/prompt name. It is the routing key CPEX matches
	// against per-tool routes (`- tool: get_compensation`), so it MUST be
	// set for ToolCall / PromptRequest / ToolResult parts.
	Name string

	// Arguments is the tool_call / prompt_request argument object that
	// APL `args.*` predicates (and Cedar `${args.*}`) evaluate against.
	Arguments map[string]any

	// URI is the resources/read target (ResourceRef parts).
	URI string

	// CorrelationID threads the JSON-RPC request id through as the
	// part's *_id field so request and response parts correlate.
	CorrelationID string

	// Content is the tool_result payload (ToolResult parts) — the parsed
	// inner object, not the MCP result envelope.
	Content any

	// IsError flags a tool_result built from a JSON-RPC error response.
	IsError bool

	// Text is the opaque-fallback body (cmfPartText only).
	Text string
}

// mcpToCMFPart decides the structured CMF content part for an MCP message.
// `isResponse` selects the phase; requestBody / responseBody supply the
// payload (and the opaque text fallback) for that phase.
//
// On the response phase the tool result is parsed from responseBody, NOT
// from mcp.Result: response hooks run in reverse pipeline order, so the
// cpex plugin executes BEFORE mcp-parser and mcp.Result isn't populated
// yet. The tool name still persists in mcp.Params from the request phase.
func mcpToCMFPart(mcp *pipeline.MCPExtension, isResponse bool, requestBody, responseBody []byte) cmfPart {
	if mcp == nil {
		body := requestBody
		if isResponse {
			body = responseBody
		}
		return cmfPart{Kind: cmfPartText, Text: string(body)}
	}
	corrID := stringifyRPCID(mcp.RPCID)

	if isResponse {
		// Response phase. Only tools/call yields a structured result
		// today; other methods fall back to opaque text.
		if mcp.Method == "tools/call" {
			return cmfPart{
				Kind:          cmfPartToolResult,
				Name:          mcpParamName(mcp.Params),
				CorrelationID: corrID,
				Content:       extractToolResultFromBody(responseBody),
				IsError:       mcp.Err != nil,
			}
		}
		return cmfPart{Kind: cmfPartText, Text: string(responseBody)}
	}

	// Request phase.
	switch mcp.Method {
	case "tools/call":
		return cmfPart{
			Kind:          cmfPartToolCall,
			Name:          mcpParamName(mcp.Params),
			Arguments:     mcpParamArgs(mcp.Params),
			CorrelationID: corrID,
		}
	case "prompts/get":
		return cmfPart{
			Kind:          cmfPartPromptRequest,
			Name:          mcpParamName(mcp.Params),
			Arguments:     mcpParamArgs(mcp.Params),
			CorrelationID: corrID,
		}
	case "resources/read":
		return cmfPart{
			Kind:          cmfPartResourceRef,
			URI:           mcpParamURI(mcp.Params),
			CorrelationID: corrID,
		}
	default:
		// Protocol mechanics (tools/list, initialize, …) — no per-tool
		// route to match. Pass the raw body through as text.
		return cmfPart{Kind: cmfPartText, Text: string(requestBody)}
	}
}

// cmfEntity maps a cmfPart to the (entity_type, entity_name) pair CPEX's
// route resolver keys on. Parts that name no entity (protocol mechanics,
// opaque text) return ("", "") and are identified by identification.
func cmfEntity(part cmfPart) (entityType, entityName string) {
	switch part.Kind {
	case cmfPartToolCall, cmfPartToolResult:
		return "tool", part.Name
	case cmfPartPromptRequest:
		return "prompt", part.Name
	case cmfPartResourceRef:
		return "resource", part.URI
	default:
		return "", ""
	}
}

// Entity coordinates for requests that name no tool, prompt, resource or
// model. PPE reserves http/* for an entity-less request: it matches `http:`
// routes by path when the config declares them and otherwise falls back to
// the global policy. Without any meta, CPEX 0.3 denies the request as
// unidentified_request whenever the config declares a policy.
const (
	entityTypeLLM    = "llm"
	globalEntityType = "http"
	globalEntityName = "*"
)

// Values of the fallback_identification config key.
const (
	fallbackGlobal = "global"
	fallbackNone   = "none"
)

// identification returns the meta coordinates for a request: the parsed
// entity when there is one, otherwise the global identification unless the
// fallback is "none", in which case it returns ("", "") and the request
// carries no meta.
func identification(entityType, entityName, fallback string) (string, string) {
	if entityType != "" && entityName != "" {
		return entityType, entityName
	}
	if fallback == fallbackNone {
		return "", ""
	}
	return globalEntityType, globalEntityName
}

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

// requestEntity is the entity a request names on the request phase: an MCP
// tool, prompt or resource, or an inference model. ("", "") when it names
// none.
func requestEntity(pctx *pipeline.Context) (entityType, entityName string) {
	switch {
	case pctx.Extensions.MCP != nil:
		return cmfEntity(mcpToCMFPart(pctx.Extensions.MCP, false, nil, nil))
	case pctx.Extensions.Inference != nil:
		return entityTypeLLM, pctx.Extensions.Inference.Model
	}
	return "", ""
}

// presentsAsEgress reports whether a request is presented as a call to
// http_egress: with synthetic egress on, an outbound request on the
// request phase that names no entity, whichever parser claimed it. A body
// shaped as an MCP protocol method or an A2A message is egress like any
// other, so it cannot route around the egress policy.
func presentsAsEgress(pctx *pipeline.Context, isResponse, egress bool) bool {
	if !egress || isResponse || pctx.Direction != pipeline.Outbound {
		return false
	}
	et, en := requestEntity(pctx)
	return et == "" || en == ""
}

// pctxToCMFParts is the top-level protocol selector: it turns whichever
// protocol extension is populated into ordered CMF content parts plus the
// (entity_type, entity_name) routing coordinates. Precedence MCP →
// inference → A2A mirrors the priority order parsers populate in, and any
// unpopulated case falls back to a single opaque-text part carrying the
// raw body.
//
// Only MCP tool/prompt/resource calls and inference name an entity; A2A,
// MCP protocol mechanics and unparsed traffic return ("", "") and take the
// fallback identification (see identification). With egress set, an
// outbound request that names no entity is presented as a call to the tool
// http_egress instead (see presentsAsEgress, egressPart); its response
// keeps the protocol mapping.
//
// Splitting this from the cgo adapter keeps the MCP/inference/A2A → CMF
// decision unit-testable under CGO_ENABLED=0; manager_cpex.go converts the
// returned cmfParts into rcpex ContentParts and stamps the entity coords
// onto Extensions.Meta.
func pctxToCMFParts(pctx *pipeline.Context, isResponse, egress bool) (parts []cmfPart, entityType, entityName string) {
	if presentsAsEgress(pctx, isResponse, egress) {
		part := egressPart(pctx)
		return []cmfPart{part}, "tool", part.Name
	}
	switch {
	case pctx.Extensions.MCP != nil:
		part := mcpToCMFPart(pctx.Extensions.MCP, isResponse, pctx.Body, pctx.ResponseBody)
		et, en := cmfEntity(part)
		return []cmfPart{part}, et, en

	case pctx.Extensions.Inference != nil:
		// Inference routes by model (`- llm: <model>`). Only structured
		// text/tool parts cross — no opaque body part. Model persists from
		// the request phase, so entity routing works on responses too.
		if isResponse {
			return readInferenceResponse(pctx).parts, entityTypeLLM, pctx.Extensions.Inference.Model
		}
		return inferenceToCMFParts(pctx.Extensions.Inference), entityTypeLLM, pctx.Extensions.Inference.Model

	case pctx.Extensions.A2A != nil:
		return a2aToCMFParts(pctx.Extensions.A2A, isResponse, pctx.ResponseBody), "", ""

	default:
		body := pctx.Body
		if isResponse {
			body = pctx.ResponseBody
		}
		return []cmfPart{{Kind: cmfPartText, Text: string(body)}}, "", ""
	}
}

// inferenceToCMFParts maps an inference request's InferenceExtension onto
// ordered CMF content parts: one text part per request message with
// non-empty string content, in message order (inference-parser populated
// the extension before cpex on the forward request pass). Flattening the
// whole prompt to text parts makes every message redactable via
// cmf.llm_input, and the strict "non-empty string content, in order" rule
// is the contract applyInferenceRequestBodyMod relies on to map redacted
// text back onto the right body message positionally. Tool output becomes
// tool-result parts instead: an OpenAI chat message with role "tool", and
// each Anthropic tool_result block or Responses-API function_call_output
// item the parser kept in inf.ToolResults. A result with no text (an image
// block only) yields no part.
//
// The response is read from the body instead (readInferenceResponse).
func inferenceToCMFParts(inf *pipeline.InferenceExtension) []cmfPart {
	if inf == nil {
		return nil
	}
	var parts []cmfPart
	for _, m := range inf.Messages {
		switch {
		case m.Content == "":
		case m.Role == roleTool && m.ToolUseID != "":
			// A chat-completions result the parser also recorded in ToolResults; the
			// loop below emits the id-carrying copy, so this one would count twice.
		case m.Role == roleTool:
			parts = append(parts, cmfPart{Kind: cmfPartToolResult, Content: m.Content})
		default:
			parts = append(parts, cmfPart{Kind: cmfPartText, Text: m.Content})
		}
	}
	for _, r := range inf.ToolResults {
		if r.Content == "" {
			continue
		}
		parts = append(parts, cmfPart{
			Kind:          cmfPartToolResult,
			CorrelationID: r.ToolUseID,
			Content:       r.Content,
			IsError:       r.IsError,
		})
	}
	return parts
}

// roleTool is the OpenAI chat role of a message carrying tool output.
const roleTool = "tool"

// a2aToCMFParts maps an A2AExtension onto ordered CMF content parts.
//
// Request phase: one text part per text-kind message part with non-empty
// content, in order (from the extension). Only kind=="text" parts
// participate — data/file parts carry JSON blobs / URIs that a text
// redactor would corrupt, so they're excluded from BOTH the read mapping
// here and the write-back in applyA2ARequestBodyMod, keeping positional
// alignment exact.
//
// Response phase: parsed from responseBody (not a2a.Artifact, which
// a2a-parser hasn't populated yet on the reverse-order response pass). See
// a2aResponseParts.
func a2aToCMFParts(a2a *pipeline.A2AExtension, isResponse bool, responseBody []byte) []cmfPart {
	if a2a == nil {
		return nil
	}
	if isResponse {
		return a2aResponseParts(responseBody)
	}
	var parts []cmfPart
	for _, p := range a2a.Parts {
		if p.Kind != "text" || p.Content == "" {
			continue
		}
		parts = append(parts, cmfPart{Kind: cmfPartText, Text: p.Content})
	}
	return parts
}

// parseToolArgs decodes an LLM tool call's raw argument string (which the
// model emits as a JSON object string) into the map APL `args.*`
// predicates evaluate against. Malformed model output is preserved rather
// than dropped: on a parse miss the raw string is wrapped as
// {"_raw": <string>} so a policy can still see (and a redactor still
// reach) the content. Empty input yields nil.
func parseToolArgs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(raw), &args) == nil {
		return args
	}
	return map[string]any{"_raw": raw}
}

// inferProvider derives a coarse provider label from a model id for
// LLMExtension.Provider. Best-effort and conservative — it returns ""
// (provider unset) rather than guess for unrecognized model families, so
// a policy keying on provider only ever sees a value we're confident in.
func inferProvider(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"),
		strings.HasPrefix(m, "text-"), strings.Contains(m, "davinci"):
		return "openai"
	case strings.HasPrefix(m, "claude"):
		return "anthropic"
	case strings.HasPrefix(m, "gemini"):
		return "google"
	default:
		return ""
	}
}

// inferenceHistory renders the request conversation as role-tagged
// entries for CPEX's AgentExtension.Conversation.History, giving policies
// that need turn/role context (not just flat redactable text) a structured
// view. Tag-free + []map[string]any so it's unit-testable and assigns
// cleanly into the rcpex []any history field. Empty-content messages are
// dropped to match the text-part flattening. Returns nil when empty.
func inferenceHistory(inf *pipeline.InferenceExtension) []map[string]any {
	if inf == nil {
		return nil
	}
	var out []map[string]any
	for _, m := range inf.Messages {
		if m.Content == "" {
			continue
		}
		out = append(out, map[string]any{"role": m.Role, "content": m.Content})
	}
	return out
}

// extractToolResultFromBody parses a JSON-RPC tools/call response body and
// returns the inner tool result payload (via extractToolResultContent on
// the `result` object). Used on the response phase, where the cpex plugin
// runs before mcp-parser and so must parse the body itself rather than
// read the not-yet-populated mcp.Result.
func extractToolResultFromBody(body []byte) any {
	if len(body) == 0 {
		return nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		// Benign and common (e.g. a non-JSON tool result), so debug only.
		// Log the size, never the body — it can carry secrets/PII.
		slog.Debug("cpex: tools/call response body is not valid JSON; result.* predicates will not resolve",
			"bytes", len(body))
		return nil
	}
	result, _ := envelope["result"].(map[string]any)
	content := extractToolResultContent(result)
	if content == nil {
		// Valid JSON, but not a tools/call result shape we can project into
		// CMF (wrong types / missing fields). Debug only; no body content.
		slog.Debug("cpex: could not extract a tool result from the response (unexpected shape); result.* predicates will not resolve",
			"bytes", len(body))
	}
	return content
}

// extractToolResultContent pulls the inner tool payload out of an MCP
// tools/call result envelope so APL `result.*` predicates resolve against
// the real data (e.g. `result.ssn`), not the MCP content wrapper. Prefers
// the typed structuredContent (MCP 2025-06-18); falls back to parsing the
// first text block as JSON; on parse-miss wraps the raw text as
// {"text": <raw>}.
func extractToolResultContent(result map[string]any) any {
	if result == nil {
		return nil
	}
	if sc, ok := result["structuredContent"]; ok {
		return sc
	}
	content, ok := result["content"].([]any)
	if !ok {
		return nil
	}
	for _, b := range content {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := block["type"].(string); t != "text" {
			continue
		}
		s, ok := block["text"].(string)
		if !ok {
			continue
		}
		var inner any
		if json.Unmarshal([]byte(s), &inner) == nil {
			return inner
		}
		return map[string]any{"text": s}
	}
	return nil
}

// mcpParamName extracts params.name (tool / prompt name) as a string.
func mcpParamName(params map[string]any) string {
	if params == nil {
		return ""
	}
	if n, ok := params["name"].(string); ok {
		return n
	}
	return ""
}

// mcpParamArgs extracts params.arguments as an object, or nil.
func mcpParamArgs(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	if a, ok := params["arguments"].(map[string]any); ok {
		return a
	}
	return nil
}

// mcpParamURI extracts params.uri (resources/read target) as a string.
func mcpParamURI(params map[string]any) string {
	if params == nil {
		return ""
	}
	if u, ok := params["uri"].(string); ok {
		return u
	}
	return ""
}

// stringifyRPCID renders a JSON-RPC id (string | number | null) as the
// stable string CPEX uses for the part's correlation id. JSON numbers
// decode to float64; format them without a spurious decimal so id 1 stays
// "1", not "1.000000" or "1e+00".
func stringifyRPCID(id any) string {
	switch v := id.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// MCPRequestBodyMod describes what changes to apply to an MCP JSON-RPC
// request body. The cgo adapter extracts these values from a CPEX-
// modified CMF Message and hands them to applyMCPRequestBodyMod.
//
// Exactly one field is consumed per method:
//
//	tools/call     → NewArguments  → params.arguments
//	prompts/get    → NewArguments  → params.arguments
//	resources/read → NewURI        → params.uri
//
// ArgsSet / URISet are the explicit "this field carries a real
// modification" signals. We do NOT infer intent from emptiness: a
// policy whose redaction is "strip all arguments" legitimately returns
// an empty (or nil) arguments map, and a "blank the URI" redaction
// returns an empty URI. Treating empty-as-no-op would silently forward
// the ORIGINAL body (secret intact) while the policy believed it had
// redacted. The cgo adapter sets these flags when the corresponding
// CMF content part is present, regardless of value.
//
// Other methods are no-ops — the operator's APL policy can't rewrite
// protocol mechanics (initialize, tools/list, etc.) because there's
// no semantically meaningful target for the rewrite.
type MCPRequestBodyMod struct {
	NewArguments map[string]any
	ArgsSet      bool
	NewURI       string
	URISet       bool
}

// applyMCPRequestBodyMod rewrites pctx.Body, which is expected to be
// an MCP JSON-RPC request. Returns mutated=true when the body was
// changed and SetBody was called; returns mutated=false (with no
// error) when the original body didn't match the expected shape
// (e.g., parsed JSON has no `params` object, method is unsupported,
// or the mod struct's relevant field is empty for the method), or the new
// value equals the one already there.
func applyMCPRequestBodyMod(pctx *pipeline.Context, method string, mod MCPRequestBodyMod) (mutated bool, err error) {
	if len(pctx.Body) == 0 {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.Body, &envelope); err != nil {
		return false, fmt.Errorf("decode request body as JSON: %w", err)
	}
	params, _ := envelope["params"].(map[string]any)
	if params == nil {
		return false, nil
	}

	switch method {
	case "tools/call", "prompts/get":
		// Gate on the explicit ArgsSet flag, NOT len()==0: a policy that
		// stripped all arguments returns an empty map and still means to
		// rewrite. Inferring no-op from emptiness would forward the
		// original (un-redacted) arguments.
		if !mod.ArgsSet || jsonEqual(params["arguments"], mod.NewArguments) {
			return false, nil
		}
		params["arguments"] = mod.NewArguments
	case "resources/read":
		if !mod.URISet || params["uri"] == mod.NewURI {
			return false, nil
		}
		params["uri"] = mod.NewURI
	default:
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize request body: %w", err)
	}
	pctx.SetBody(newBody)
	return true, nil
}

// applyMCPResponseBodyMod rewrites pctx.ResponseBody for a tools/call
// JSON-RPC response. The new content replaces:
//
//   - result.content[].text — the canonical MCP text block, stringified
//     as JSON (pretty-printed when possible).
//   - result.structuredContent — only when the original response had it.
//     Don't introduce structuredContent on a response that didn't carry
//     it; clients sniffing for the new shape would be surprised.
//
// Fail-closed on multiple text blocks: the read side
// (extractToolResultContent) surfaces only the FIRST text block to the
// policy, and we hold a single redacted value. A result.content[] with
// >1 text block is therefore an ambiguous rewrite — blocks[1..] were
// never shown to the policy, so rewriting only block[0] would forward
// the others (potentially carrying the same secret) verbatim while
// reporting success. We return an error so the caller fails closed
// rather than leaking the un-inspected blocks (mirrors the A2A and
// inference response guards). Rewrite all blocks would be wrong too —
// we'd stamp one value over distinct blocks.
//
// Returns mutated=true when SetResponseBody was called; mutated=false
// when the body wasn't a tools/call response, had no result.content,
// the response had no replaceable text block, or newContent equals the
// result the policy was shown.
func applyMCPResponseBodyMod(pctx *pipeline.Context, method string, newContent any) (mutated bool, err error) {
	if method != "tools/call" {
		return false, nil
	}
	if len(pctx.ResponseBody) == 0 || newContent == nil {
		return false, nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(pctx.ResponseBody, &envelope); err != nil {
		return false, fmt.Errorf("decode response body as JSON: %w", err)
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok || jsonEqual(extractToolResultContent(result), newContent) {
		return false, nil
	}

	replaced := false

	// Primary path: rewrite the text block in result.content[]. Collect
	// all text blocks first so >1 fails closed rather than silently
	// leaving blocks[1..] unredacted (the read side only inspected the
	// first; we only hold one redacted value).
	if contentArr, ok := result["content"].([]any); ok {
		textBlocks := make([]map[string]any, 0, len(contentArr))
		for _, block := range contentArr {
			blockObj, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := blockObj["type"].(string); ok && t == "text" {
				textBlocks = append(textBlocks, blockObj)
			}
		}
		if len(textBlocks) > 1 {
			return false, fmt.Errorf(
				"MCP tools/call response has %d text blocks; single-value redaction rewrite is ambiguous",
				len(textBlocks))
		}
		if len(textBlocks) == 1 {
			textBlocks[0]["text"] = stringifyForTextBlock(newContent)
			replaced = true
		}
	}

	// Secondary path: update structuredContent when it was already
	// present. Operators can opt clients into the newer
	// structuredContent shape by populating it in the upstream
	// response; CPEX rewrites it consistently. We never introduce
	// it on a response that lacked it.
	if _, has := result["structuredContent"]; has {
		result["structuredContent"] = newContent
		replaced = true
	}

	if !replaced {
		return false, nil
	}

	newBody, err := json.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("re-serialize response body: %w", err)
	}
	pctx.SetResponseBody(newBody)
	return true, nil
}

// stringifyForTextBlock turns the modified content value back into the
// string the MCP text block expects. We try pretty-printed JSON first
// (matches the upstream MCP server's typical output, makes diffs
// readable in session events); fall back to compact JSON on error;
// fall back to %v on impossible-to-marshal types.
func stringifyForTextBlock(v any) string {
	if s, ok := v.(string); ok {
		// Common case: the redactor handed back a string verbatim.
		return s
	}
	if b, err := json.MarshalIndent(v, "", "  "); err == nil {
		return string(b)
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

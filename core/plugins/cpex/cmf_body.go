package cpex

import (
	"net/http"

	"github.com/rossoctl/cortex/core/pipeline"
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
	id := pctx.SessionID
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

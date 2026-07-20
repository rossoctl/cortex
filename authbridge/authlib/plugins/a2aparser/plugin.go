package a2aparser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/internal/parsercommon"
)

// A2AParser parses A2A JSON-RPC 2.0 request bodies and populates
// pctx.Extensions.A2A with the parsed method, session ID, message parts,
// and role for downstream policy plugins (e.g., guardrails).
type A2AParser struct{}

func NewA2AParser() *A2AParser { return &A2AParser{} }

func init() {
	plugins.RegisterPlugin("a2a-parser", func() pipeline.Plugin { return NewA2AParser() })
}

func (p *A2AParser) Name() string { return "a2a-parser" }

func (p *A2AParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:   true,
		Description: "Parses A2A messages into pctx.Extensions.A2A for downstream plugins.",
	}
}

func (p *A2AParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// No Invocation recorded when the parser doesn't apply to this
	// message (empty body, non-JSON-RPC body) — otherwise every
	// unrelated HTTP call through the pipeline would show an "a2a-parser
	// skip" row in abctl, which is noise. Operators infer "a2a-parser
	// exists in this pipeline" from the pipeline config, not per-event.
	if len(pctx.Body) == 0 {
		slog.Debug("a2a-parser: no body, skipping")
		return pipeline.Action{Type: pipeline.Continue}
	}

	var rpc parsercommon.JSONRPCRequest
	if err := json.Unmarshal(pctx.Body, &rpc); err != nil {
		slog.Debug("a2a-parser: invalid JSON-RPC", "error", err, "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}

	ext := &pipeline.A2AExtension{
		Method:   rpc.Method,
		RPCID:    rpc.ID,
		IsAction: isA2AAction(rpc.Method),
	}

	// Extract message fields generically — any method with params.message
	// gets full extraction (forward-compatible with future A2A methods).
	// A2A spec uses "contextId" (current) or "sessionId" (older drafts).
	// Some A2A clients (notably the Python SDK used by the rossoctl backend)
	// place contextId INSIDE params.message rather than at the top-level
	// params, so fall through to params.message.contextId when neither
	// top-level slot is set. Without this fallback every inbound turn of
	// an established conversation lands in the "default" session bucket
	// instead of the real contextId — response-phase extraction later
	// populates A2A.SessionID, but that's after the listener has already
	// keyed the event, so abctl shows every turn under "default".
	ext.SessionID = rpc.StringParam("contextId")
	if ext.SessionID == "" {
		ext.SessionID = rpc.StringParam("sessionId")
	}
	ext.TaskID = rpc.StringParam("taskId")
	if msg := rpc.MapParam("message"); msg != nil {
		if ext.SessionID == "" {
			if cid, ok := msg["contextId"].(string); ok {
				ext.SessionID = cid
			}
		}
		if role, ok := msg["role"].(string); ok {
			ext.Role = role
		}
		if messageID, ok := msg["messageId"].(string); ok {
			ext.MessageID = messageID
		}
		if rawParts, ok := msg["parts"].([]any); ok {
			ext.Parts = parseA2AParts(rawParts)
		}
	}

	pctx.Extensions.A2A = ext

	slog.Info("a2a-parser", "method", rpc.Method)
	slog.Debug("a2a-parser: extracted",
		"method", rpc.Method,
		"sessionId", ext.SessionID,
		"role", ext.Role,
		"messageId", ext.MessageID,
		"parts", len(ext.Parts),
	)
	for i, part := range ext.Parts {
		slog.Debug("a2a-parser: part", "index", i, "kind", part.Kind, "content", parsercommon.Truncate(part.Content, parsercommon.DebugBodyMax))
	}
	pctx.Observe("matched_" + rpc.Method)
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse is the legacy buffered-path response hook. Because this
// plugin implements StreamingResponder, pipeline.RunResponse skips it
// and OnResponseFrame is the dispatch path under all listeners — this
// method is unreachable from a normal listener. Kept for tests and
// hypothetical pipelines that call OnResponse directly without going
// through RunResponse, with a defensive guard against re-recording if
// the streaming path has already populated state.
func (p *A2AParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if pctx.Extensions.A2A == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.A2A
	// If OnResponseFrame already finalized (any response field set),
	// don't re-record on the buffered path.
	if ext.FinalStatus != "" || ext.Artifact != "" || ext.ErrorMessage != "" {
		return pipeline.Action{Type: pipeline.Continue}
	}
	if len(pctx.ResponseBody) == 0 {
		pctx.Skip("no_response_body")
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Capture the server-assigned contextId — but only when the request
	// didn't already carry one. Overwriting would split the inbound
	// request and response events into different session buckets.
	if ext.SessionID == "" {
		if sid := extractSessionID(pctx.ResponseBody); sid != "" {
			ext.SessionID = sid
		}
	}

	extractResponseSummary(pctx.ResponseBody, ext)
	logA2AFinalized(ext)
	pctx.Observe("matched_" + ext.Method + "_response")
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponseFrame folds each message/stream SSE event into the
// running A2A response state. Final-status and artifact text are
// accumulated across frames; the public extension fields are
// finalized on last=true so the session event sees a single
// consistent snapshot.
//
// application/json (message/send) responses are delivered as a
// single last=true frame carrying the full envelope — the same path
// extracts the response summary from it.
func (p *A2AParser) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if pctx.Extensions.A2A == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	ext := pctx.Extensions.A2A

	// Per-frame fold. message/stream events arrive one per frame;
	// message/send arrives as a single last=true frame.
	if len(frame) > 0 {
		// Try the message/send envelope first — message/stream events
		// don't have the {result: {status: {state: "..."}}} top-level
		// shape so extractSendResponse will return false and we fall
		// through to the per-event extractor.
		if !extractSendResponse(frame, ext) {
			extractStreamEvent(frame, ext)
		}
		// Capture session id from any frame if the request didn't carry one.
		if ext.SessionID == "" {
			if sid := sessionIDFromJSON(frame); sid != "" {
				ext.SessionID = sid
			}
		}
	}

	if last {
		// Empty stream and we never recorded anything on the request
		// side — record a Skip so the response row is paired with the
		// request row in abctl.
		if ext.FinalStatus == "" && ext.Artifact == "" && ext.ErrorMessage == "" {
			pctx.Skip("no_response_body")
			return pipeline.Action{Type: pipeline.Continue}
		}
		logA2AFinalized(ext)
		pctx.Observe("matched_" + ext.Method + "_response")
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// logA2AFinalized emits the operator-facing debug log once a response
// is finalized — shared by OnResponse and OnResponseFrame so the two
// paths log identically.
// maxArtifactBytes caps the accumulated A2A artifact text recorded
// for observability. Long agent runs can stream many artifact-update
// events; without a cap, ext.Artifact would grow unbounded across
// frames and live on the response event for the life of the request.
// 64 KiB keeps the most recent text usable in abctl while bounding
// per-request memory.
const maxArtifactBytes = 64 * 1024

// appendCapped appends s to dst up to maxArtifactBytes; once the cap
// is reached, further appends are silently dropped (a single
// "…(truncated)" suffix is added the first time we hit the cap so the
// timeline shows truncation explicitly).
func appendCapped(dst, s string) string {
	const truncMarker = "…(truncated)"
	if len(dst) >= maxArtifactBytes {
		return dst
	}
	remaining := maxArtifactBytes - len(dst)
	if len(s) <= remaining {
		return dst + s
	}
	return dst + s[:remaining] + truncMarker
}

func logA2AFinalized(ext *pipeline.A2AExtension) {
	slog.Debug("a2a-parser: response parsed",
		"sessionId", ext.SessionID,
		"finalStatus", ext.FinalStatus,
		"artifactLen", len(ext.Artifact),
		"error", ext.ErrorMessage,
	)
}

// extractResponseSummary parses the response body for final status, artifact text,
// and error message. Supports both SSE streams (message/stream) and plain JSON-RPC
// responses (message/send).
func extractResponseSummary(body []byte, ext *pipeline.A2AExtension) {
	// Try plain JSON-RPC first (message/send response)
	if extractSendResponse(body, ext) {
		return
	}
	// SSE stream (message/stream): scan data: lines for status and artifact events
	extractStreamResponse(body, ext)
}

// extractSendResponse handles message/send responses (single JSON-RPC result).
func extractSendResponse(body []byte, ext *pipeline.A2AExtension) bool {
	var resp struct {
		Result struct {
			Status struct {
				State   string `json:"state"`
				Message struct {
					Parts []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
			Artifacts []struct {
				Parts []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"artifacts"`
			TaskID string `json:"taskId"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Result.Status.State == "" {
		return false
	}

	ext.FinalStatus = resp.Result.Status.State
	if ext.TaskID == "" && resp.Result.TaskID != "" {
		ext.TaskID = resp.Result.TaskID
	}

	// Extract artifact text
	for _, artifact := range resp.Result.Artifacts {
		for _, part := range artifact.Parts {
			if part.Kind == "text" && part.Text != "" {
				ext.Artifact = part.Text
			}
		}
	}

	// Extract error message from status message on failure
	if resp.Result.Status.State == "failed" {
		for _, part := range resp.Result.Status.Message.Parts {
			if part.Kind == "text" && part.Text != "" {
				ext.ErrorMessage = part.Text
				break
			}
		}
	}
	return true
}

// extractStreamResponse handles message/stream SSE responses (the
// buffered path). For each "data:" line it extracts a single event
// and folds it into the extension via extractStreamEvent.
func extractStreamResponse(body []byte, ext *pipeline.A2AExtension) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		extractStreamEvent(data, ext)
	}
}

// extractStreamEvent folds one A2A message/stream event payload (the
// JSON object after `data: `) into the extension's running response
// state. Used by both the buffered path (extractStreamResponse loops
// over data: lines) and the streaming path (OnResponseFrame already
// has the parsed payload as a frame).
func extractStreamEvent(data []byte, ext *pipeline.A2AExtension) {
	var event struct {
		Result struct {
			Kind   string `json:"kind"`
			Final  bool   `json:"final"`
			TaskID string `json:"taskId"`
			Status struct {
				State   string `json:"state"`
				Message struct {
					Parts []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
			Artifact struct {
				Parts []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"artifact"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}

	if ext.TaskID == "" && event.Result.TaskID != "" {
		ext.TaskID = event.Result.TaskID
	}

	switch event.Result.Kind {
	case "status-update":
		if event.Result.Final {
			ext.FinalStatus = event.Result.Status.State
			if event.Result.Status.State == "failed" {
				for _, part := range event.Result.Status.Message.Parts {
					if part.Kind == "text" && part.Text != "" {
						ext.ErrorMessage = part.Text
						break
					}
				}
			}
		}
	case "artifact-update", "artifact":
		for _, part := range event.Result.Artifact.Parts {
			if part.Kind == "text" && part.Text != "" {
				ext.Artifact = appendCapped(ext.Artifact, part.Text)
			}
		}
	}
}

// extractSessionID finds a contextId (preferred) or sessionId in the response.
// Supports plain JSON-RPC responses and SSE event streams (message/stream).
func extractSessionID(body []byte) string {
	if sid := sessionIDFromJSON(body); sid != "" {
		return sid
	}
	// SSE format: scan "data:" lines for the first event that carries a session ID.
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if sid := sessionIDFromJSON(data); sid != "" {
			return sid
		}
	}
	return ""
}

func sessionIDFromJSON(data []byte) string {
	var resp struct {
		Result struct {
			ContextID string `json:"contextId"`
			SessionID string `json:"sessionId"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &resp) != nil {
		return ""
	}
	if resp.Result.ContextID != "" {
		return resp.Result.ContextID
	}
	return resp.Result.SessionID
}

func parseA2AParts(rawParts []any) []pipeline.A2APart {
	parts := make([]pipeline.A2APart, 0, len(rawParts))
	for _, raw := range rawParts {
		partMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, ok := partMap["kind"].(string)
		if !ok || kind == "" {
			continue
		}
		var content string
		switch kind {
		case "text":
			content, _ = partMap["text"].(string)
		case "file":
			// TODO: update when A2A spec stabilizes — canonical Part uses mediaType + content field presence, not "kind".
			content, _ = partMap["data"].(string)
			if content == "" {
				content, _ = partMap["uri"].(string)
			}
		case "data":
			if dataVal, ok := partMap["data"]; ok && dataVal != nil {
				if b, err := json.Marshal(dataVal); err == nil {
					content = string(b)
				}
			}
		}
		parts = append(parts, pipeline.A2APart{Kind: kind, Content: content})
	}
	return parts
}

// isA2AAction reports whether an A2A JSON-RPC method name names a
// user-meaningful agent-to-agent call that guardrails should judge.
// Only methods that carry a user message into the agent (or out to
// another agent) qualify; protocol/discovery methods are bypass.
//
// On the inbound side, action methods are how IBAC's session intent
// gets seeded — a2a-parser's classification doesn't drive inbound
// IBAC behavior (IBAC is outbound-only) but the field is set on
// inbound for consistency and for any future inbound guardrail.
func isA2AAction(method string) bool {
	switch method {
	case "message/send", "message/stream":
		return true
	}
	return false
}

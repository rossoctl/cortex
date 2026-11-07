package a2aparser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
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
	if ext := parseA2ARequest(pctx.Path, pctx.Headers, pctx.Body); ext != nil {
		pctx.Extensions.A2A = ext
		pctx.Observe("matched_" + ext.Method)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// parseA2ARequest parses one A2A request from plain values. It returns nil
// when the parser does not claim the request.
func parseA2ARequest(path string, headers http.Header, rawBody []byte) *pipeline.A2AExtension {
	// Parse the bytes as sent after any zstd Content-Encoding, as
	// inference-parser does, so a compressed call is claimed rather than
	// passed through unparsed.
	body := parsercommon.DecodeRequestBody(headers, rawBody)
	// No Invocation recorded when the parser doesn't apply to this
	// message (empty body, non-JSON-RPC body) — otherwise every
	// unrelated HTTP call through the pipeline would show an "a2a-parser
	// skip" row in agentop, which is noise. Operators infer "a2a-parser
	// exists in this pipeline" from the pipeline config, not per-event.
	if len(body) == 0 {
		slog.Debug("a2a-parser: no body, skipping")
		return nil
	}

	// A top-level non-object body (array, string, number, null) is not
	// JSON-RPC; peek the first byte so it takes the calm skip path below
	// rather than logging as malformed.
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	isObject := len(trimmed) > 0 && trimmed[0] == '{'

	var rpc parsercommon.JSONRPCRequest
	if isObject {
		if err := json.Unmarshal(body, &rpc); err != nil {
			slog.Debug("a2a-parser: invalid JSON-RPC", "error", err, "bodyLen", len(body),
				"path", pipeline.SanitizeLogValue(path))
			return nil
		}
	}

	// Claim only A2A-namespace methods. Both A2A and MCP ride JSON-RPC 2.0
	// over HTTP, so "the method is non-empty" cannot tell them apart — the
	// method namespace is the only in-body signal that does. Gating here
	// keeps this parser from claiming traffic that is JSON-RPC but belongs
	// to another protocol — MCP (initialize, tools/list, notifications/*,
	// ...) — or a non-JSON-RPC body such as an inference /v1/messages call
	// (which unmarshals into JSONRPCRequest with an empty Method). Without
	// it, agentop showed a phantom a2a-parser match on both MCP and inference
	// traffic. mcp-parser has the mirror guard for its own namespace.
	if !isObject || !isA2AMethod(rpc.Method) {
		slog.Debug("a2a-parser: not an A2A-namespace method, skipping", "method", pipeline.SanitizeLogValue(rpc.Method), "path", pipeline.SanitizeLogValue(path),
			"bodyLen", len(body))
		return nil
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
	// keyed the event, so agentop shows every turn under "default".
	ext.SessionID = rpc.StringParam("contextId")
	if ext.SessionID == "" {
		ext.SessionID = rpc.StringParam("sessionId")
	}
	if msg := rpc.MapParam("message"); msg != nil {
		if taskID, ok := msg["taskId"].(string); ok {
			ext.TaskID = taskID
		}
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
	// Spec v0.3.0 puts the task id in params.message.taskId (above) or,
	// for a method with TaskIdParams, in params.id. Older clients send
	// params.taskId, so it stays the last fallback.
	if ext.TaskID == "" && taskIDParamIsID(rpc.Method) {
		ext.TaskID = rpc.StringParam("id")
	}
	if ext.TaskID == "" {
		ext.TaskID = rpc.StringParam("taskId")
	}

	slog.Info("a2a-parser", "method", pipeline.SanitizeLogValue(rpc.Method))
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
	return ext
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
	// message/send arrives as a single last=true frame. A buffered
	// message/stream (a body-writing plugin is in the chain) arrives as
	// one last=true frame that holds the whole SSE body.
	if len(frame) > 0 {
		extractResponseSummary(frame, ext)
		// Capture session id from any frame if the request didn't carry one.
		if ext.SessionID == "" {
			if sid := extractSessionID(frame); sid != "" {
				ext.SessionID = sid
			}
		}
	}

	if last {
		// Empty stream and we never recorded anything on the request
		// side — record a Skip so the response row is paired with the
		// request row in agentop.
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
// 64 KiB keeps the most recent text usable in agentop while bounding
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
	// Plain JSON-RPC (message/send response)
	if json.Valid(body) {
		foldResponse(body, ext)
		return
	}
	// SSE stream (message/stream): fold each data: line as one event
	extractStreamResponse(body, ext)
}

// foldResponse folds one JSON-RPC response payload into ext: a whole
// message/send body, or one message/stream event. Both response paths
// call it, so they agree. A result with a task state (a Task or a
// status-update) goes to extractSendResponse. The last state wins, final
// or not. Other results go to extractStreamEvent.
func foldResponse(data []byte, ext *pipeline.A2AExtension) {
	if !extractSendResponse(data, ext) {
		extractStreamEvent(data, ext)
	}
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
			// any, not string: a non-string kind or id must not fail the whole decode.
			Kind   any    `json:"kind"`
			ID     any    `json:"id"`
			TaskID string `json:"taskId"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Result.Status.State == "" {
		return false
	}

	ext.FinalStatus = resp.Result.Status.State
	if id := resultTaskID(resp.Result.Kind, resp.Result.ID, resp.Result.TaskID); ext.TaskID == "" && id != "" {
		ext.TaskID = id
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
// buffered path). For each "data:" line it folds a single event into
// the extension via foldResponse, as OnResponseFrame does for a frame.
//
// parsercommon.NormalizeSSE first: a leading BOM or CR-only line endings are
// wire-legal SSE, and a raw LF split reads such a body as one unparseable
// line — leaving the extension empty and the exchange unclassified (#1567).
func extractStreamResponse(body []byte, ext *pipeline.A2AExtension) {
	for _, line := range bytes.Split(parsercommon.NormalizeSSE(body), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		foldResponse(data, ext)
	}
}

// extractStreamEvent folds one A2A result that has no task state (an
// artifact-update event or a Message) into the extension's running
// response state. foldResponse calls it for both response paths.
func extractStreamEvent(data []byte, ext *pipeline.A2AExtension) {
	var event struct {
		Result struct {
			Kind     string `json:"kind"`
			ID       any    `json:"id"` // any: a non-string id must not fail the whole decode
			TaskID   string `json:"taskId"`
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

	if id := resultTaskID(event.Result.Kind, event.Result.ID, event.Result.TaskID); ext.TaskID == "" && id != "" {
		ext.TaskID = id
	}

	switch event.Result.Kind {
	case "artifact-update", "artifact":
		for _, part := range event.Result.Artifact.Parts {
			if part.Kind == "text" && part.Text != "" {
				ext.Artifact = appendCapped(ext.Artifact, part.Text)
			}
		}
	}
}

// taskIDParamIsID reports whether the params of an A2A method (spec v0.3.0)
// are TaskIdParams or extend them, so params.id is the task id.
// tasks/pushNotificationConfig/set is not in the list: its params carry taskId.
func taskIDParamIsID(method string) bool {
	switch method {
	case "tasks/get", "tasks/cancel", "tasks/resubscribe",
		"tasks/pushNotificationConfig/get", "tasks/pushNotificationConfig/list",
		"tasks/pushNotificationConfig/delete":
		return true
	}
	return false
}

// resultTaskID returns the task id of an A2A result (spec v0.3.0).
// A Task (kind "task") carries its id in id. A Message and the two
// task update events carry it in taskId. A kind or id that is not a
// string gives no task id.
func resultTaskID(kind, id any, taskID string) string {
	if kind == "task" {
		s, _ := id.(string)
		return s
	}
	return taskID
}

// extractSessionID finds a contextId (preferred) or sessionId in the response.
// Supports plain JSON-RPC responses and SSE event streams (message/stream).
func extractSessionID(body []byte) string {
	if sid := sessionIDFromJSON(body); sid != "" {
		return sid
	}
	// SSE format: scan "data:" lines for the first event that carries a session ID.
	// Normalized for the same reason as extractStreamResponse above.
	for _, line := range bytes.Split(parsercommon.NormalizeSSE(body), []byte("\n")) {
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

// isA2AMethod reports whether a JSON-RPC method name belongs to the A2A
// protocol namespace. A2A methods are slash-namespaced under message/,
// tasks/, and agent/ (per the A2A spec: message/send, message/stream,
// tasks/get, tasks/list, tasks/cancel, tasks/resubscribe,
// tasks/pushNotificationConfig/*, agent/getAuthenticatedExtendedCard).
// This positively identifies A2A traffic — declining MCP JSON-RPC and
// non-JSON-RPC bodies (empty method) that also unmarshal into a
// JSONRPCRequest. It stays forward-compatible within A2A's own namespace
// design: future message/*, tasks/*, and agent/* methods match without a
// code change.
func isA2AMethod(method string) bool {
	return strings.HasPrefix(method, "message/") ||
		strings.HasPrefix(method, "tasks/") ||
		strings.HasPrefix(method, "agent/")
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

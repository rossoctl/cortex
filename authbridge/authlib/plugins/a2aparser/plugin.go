package a2aparser

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/internal/parsercommon"
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
		Writes:    []string{"a2a"},
		ReadsBody: true,
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
		Method: rpc.Method,
		RPCID:  rpc.ID,
	}

	// Extract message fields generically — any method with params.message
	// gets full extraction (forward-compatible with future A2A methods).
	// A2A spec uses "contextId" (current) or "sessionId" (older drafts).
	ext.SessionID = rpc.StringParam("contextId")
	if ext.SessionID == "" {
		ext.SessionID = rpc.StringParam("sessionId")
	}
	ext.TaskID = rpc.StringParam("taskId")
	if msg := rpc.MapParam("message"); msg != nil {
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

// OnResponse extracts the server-assigned session/context ID and response summary
// from the response body. The summary includes final status, artifact text, and
// error message — enabling debugging of agent behavior without reading raw SSE.
//
// Handles both JSON-RPC responses (message/send) and SSE event streams (message/stream).
func (p *A2AParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// No Invocation on response when the parser doesn't apply: either
	// there's no response body or the matching request wasn't an A2A
	// JSON-RPC call. Keeps the response event clean for non-A2A traffic.
	if len(pctx.ResponseBody) == 0 || pctx.Extensions.A2A == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Capture the server-assigned contextId — but only when the request
	// didn't already carry one. Overwriting would split the inbound
	// request and response events into different session buckets (the
	// agent's A2A SDK may mint a fresh contextId in its response even
	// when the client supplied one, which is legal per A2A but breaks
	// telemetry bucketing). The request-side contextId is authoritative
	// for session attribution.
	if pctx.Extensions.A2A.SessionID == "" {
		if sid := extractSessionID(pctx.ResponseBody); sid != "" {
			pctx.Extensions.A2A.SessionID = sid
		}
	}

	// Extract response summary (final status + artifact + error)
	extractResponseSummary(pctx.ResponseBody, pctx.Extensions.A2A)

	slog.Debug("a2a-parser: response parsed",
		"sessionId", pctx.Extensions.A2A.SessionID,
		"finalStatus", pctx.Extensions.A2A.FinalStatus,
		"artifactLen", len(pctx.Extensions.A2A.Artifact),
		"error", pctx.Extensions.A2A.ErrorMessage,
	)
	pctx.Observe("matched_" + pctx.Extensions.A2A.Method + "_response")
	return pipeline.Action{Type: pipeline.Continue}
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

// extractStreamResponse handles message/stream SSE responses.
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
			continue
		}

		// Capture taskId from any event
		if ext.TaskID == "" && event.Result.TaskID != "" {
			ext.TaskID = event.Result.TaskID
		}

		switch event.Result.Kind {
		case "status-update":
			if event.Result.Final {
				ext.FinalStatus = event.Result.Status.State
				// Extract error message on failure
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
			// A2A SDKs emit kind="artifact-update" on the stream; older
			// samples use "artifact". Accept both. Concatenate text parts
			// from the frame; repeated frames for the same artifact carry
			// appended text so we accumulate across frames.
			for _, part := range event.Result.Artifact.Parts {
				if part.Kind == "text" && part.Text != "" {
					ext.Artifact += part.Text
				}
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

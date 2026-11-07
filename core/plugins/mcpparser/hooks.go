package mcpparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/rossoctl/cortex/core/bypass"
	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/parse/mcp"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// mcpConfig is the plugin's local config schema. The MCP-endpoint
// `paths` list scopes body-less transport-layer detection (SSE GET,
// session-terminate DELETE) to known MCP endpoints — without it,
// every body-less GET in the cluster would risk being mis-classified
// as an MCP transport call.
type mcpConfig struct {
	// Paths is the set of URL path globs that should be treated as
	// MCP endpoints for body-less-request detection. Defaults to
	// ["/mcp"] which matches the standard MCP Streamable HTTP setup
	// used by the MCP Python SDK and most server templates.
	//
	// Path-shape detection only fires on body-less requests; body-
	// having JSON-RPC requests are parsed regardless of path (the
	// JSON-RPC body itself is the protocol signal).
	Paths []string `json:"paths"`
}

func (c *mcpConfig) applyDefaults() {
	if len(c.Paths) == 0 {
		c.Paths = []string{"/mcp"}
	}
}

// MCPParser parses MCP JSON-RPC 2.0 request bodies and populates
// pctx.Extensions.MCP with the method, RPC ID, raw params, and the
// IsAction classification verdict for downstream guardrails.
//
// Recognizes three shapes:
//
//  1. JSON-RPC body (POST /mcp with a valid {jsonrpc, method, ...}
//     payload): populates Method/RPCID/Params. IsAction=true for
//     known action methods (tools/call, prompts/get, resources/read);
//     all other methods leave IsAction at the zero-value false.
//
//  2. Body-less DELETE on a configured path with the Mcp-Session-Id
//     header: MCP Streamable HTTP session termination per spec.
//     Populates Method=$transport/terminate, IsAction=false.
//
//  3. Body-less GET on a configured path: MCP Streamable HTTP server-
//     to-client SSE channel-open. Populates Method=$transport/stream,
//     IsAction=false.
//
// Body-less requests on non-configured paths leave Extensions.MCP
// nil — the parser can't reliably tell whether they're MCP traffic.
type MCPParser struct {
	paths *bypass.Matcher
}

func NewMCPParser() *MCPParser { return &MCPParser{} }

func init() {
	plugins.RegisterPlugin("mcp-parser", func() pipeline.Plugin { return NewMCPParser() })
}

func (p *MCPParser) Name() string { return "mcp-parser" }

func (p *MCPParser) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:   true,
		Description: "Parses MCP tool calls/results into pctx.Extensions.MCP.",
	}
}

// Configure decodes the optional `paths` list and compiles a path
// matcher used by body-less transport-layer detection. Always
// initializes the matcher (default paths are applied when omitted)
// so OnRequest never has to nil-check.
func (p *MCPParser) Configure(raw json.RawMessage) error {
	var c mcpConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("mcp-parser config: %w", err)
		}
	}
	c.applyDefaults()
	matcher, err := bypass.NewMatcher(c.Paths)
	if err != nil {
		return fmt.Errorf("mcp-parser paths: %w", err)
	}
	p.paths = matcher
	return nil
}

func (p *MCPParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if ext := mcp.ParseRequest(pctx.Method, pctx.Path, pctx.Headers, pctx.Body, p.paths); ext != nil {
		pctx.Extensions.MCP = ext
		pctx.Observe("matched_" + ext.Method)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// recordEmptyResponse records the terminal invocation for a response that
// carried no body, distinguishing an expected ack from an anomaly:
//
//   - A JSON-RPC notification has no request id, so the transport acks it
//     with an empty HTTP 202 and there is no response object to parse. That
//     is the expected, complete end of the exchange — record an Observe so
//     agentop credits mcp-parser (otherwise the paired response row renders as
//     "—", which reads as if nothing handled it). Synthetic transport events
//     ($transport/*) likewise carry no id and are handled MCP protocol events.
//   - A request that DID carry an id but came back empty is anomalous (a
//     truncated or failed upstream response). Keep it a Skip so it is not
//     mistaken for a clean handling — a skip never credits the plugin in
//     agentop, which is the right signal for a broken response.
//
// Either branch records an invocation, so agentop still pairs the response row
// with the request row (pairing keys on plugin+method+direction).
func recordEmptyResponse(pctx *pipeline.Context) {
	if pctx.Extensions.MCP.RPCID == nil {
		pctx.Observe("matched_" + pctx.Extensions.MCP.Method + "_ack")
		return
	}
	pctx.Skip("no_response_body")
}

// OnResponse is the legacy buffered-path response hook. Because this
// plugin implements StreamingResponder, pipeline.RunResponse skips it
// and OnResponseFrame is the dispatch path under all listeners — this
// method is unreachable from a normal listener. Kept for tests and
// hypothetical pipelines that call OnResponse directly without going
// through RunResponse, with a defensive guard against re-recording if
// the streaming path has already populated state.
func (p *MCPParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// Stay silent when the request side never participated — the parser
	// recorded nothing on request, so recording on response would orphan
	// the row.
	if pctx.Extensions.MCP == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	// If OnResponseFrame already populated the result/error or recorded
	// a skip on this pass, don't re-record. This keeps the buffered and
	// streaming paths from emitting duplicate rows when the listener
	// runs both (it shouldn't, today — the streaming path skips
	// RunResponse entirely — but the guard is cheap and protects
	// future listener variants).
	if pctx.Extensions.MCP.Result != nil || pctx.Extensions.MCP.Err != nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	// We DID process the request but the response has no body — typical
	// for JSON-RPC notifications that ack with HTTP 202. recordEmptyResponse
	// credits mcp-parser with an Observe for an expected notification ack
	// (no request id) and keeps a Skip for an anomalous empty response to a
	// request that carried an id. Either way an invocation is recorded so
	// agentop pairs the response row with the request row.
	if len(pctx.ResponseBody) == 0 {
		recordEmptyResponse(pctx)
		return pipeline.Action{Type: pipeline.Continue}
	}

	if !mcp.ParseResponse(pctx.ResponseBody, pctx.Extensions.MCP) {
		slog.Debug("mcp-parser: response is not valid JSON-RPC or SSE", "bodyLen", len(pctx.ResponseBody))
		pctx.Skip("unparseable_response")
		return pipeline.Action{Type: pipeline.Continue}
	}

	recordResponse(pctx)
	slog.Debug("mcp-parser: response detail", "method", pipeline.SanitizeLogValue(pctx.Extensions.MCP.Method),
		"body", pipeline.SanitizeLogValue(parsercommon.Truncate(string(pctx.ResponseBody), parsercommon.DebugBodyMax)))
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponseFrame is the streaming-aware response hook. Listeners
// invoke it once per SSE frame (text/event-stream) and once with
// last=true at end-of-stream. application/json responses arrive as
// a single last=true frame — so the same code handles both shapes.
//
// One response per request: JSON-RPC 2.0 gives a request one response,
// with a result or an error, not both. The first frame that carries a
// response wins, as in the buffered path (mcp.ParseResponse). The parser
// ignores a later response frame, so Result and Err are never both set.
func (p *MCPParser) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	// Stay silent when the request side never participated.
	if pctx.Extensions.MCP == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}

	// End-of-stream call with no payload. If we never saw a frame with a
	// result/error and the response body was empty, record the terminal
	// invocation via recordEmptyResponse (matches the buffered path: an
	// Observe for an expected notification ack, a Skip for an anomalous empty
	// response to a request with an id) so agentop pairs request and response
	// rows uniformly across shapes.
	if len(frame) == 0 {
		if last && pctx.Extensions.MCP.Result == nil && pctx.Extensions.MCP.Err == nil {
			recordEmptyResponse(pctx)
		}
		return pipeline.Action{Type: pipeline.Continue}
	}
	// The request has its response. A later response frame is not a second answer.
	if pctx.Extensions.MCP.Result != nil || pctx.Extensions.MCP.Err != nil {
		slog.Debug("mcp-parser: ignoring a later response frame", "method", pipeline.SanitizeLogValue(pctx.Extensions.MCP.Method))
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Parse SSE-aware: a frame is normally one SSE event's pre-stripped
	// JSON-RPC payload (streaming path), but the buffered dispatch hands the
	// WHOLE response body as a single last=true frame — and for a Streamable
	// HTTP server that body is a raw `data: {...}` SSE blob. A bare
	// json.Unmarshal fails on that blob and silently drops the result (the
	// tools/list-response-not-recorded bug). mcp.ParseResponse tries a direct
	// decode first (clean frame), then scans `data:` lines (raw SSE blob), so
	// both shapes record correctly.
	if !mcp.ParseResponse(frame, pctx.Extensions.MCP) {
		// Notifications, heartbeats, malformed frames, or other shapes with
		// no JSON-RPC result/error envelope — nothing to record per-frame.
		// The end-of-stream last=true call records a Skip if no envelope was
		// ever seen, so the response row still pairs with its request.
		slog.Debug("mcp-parser: response frame carried no JSON-RPC result/error",
			"frameLen", len(frame), "frame", pipeline.SanitizeLogValue(parsercommon.Truncate(string(frame), parsercommon.DebugBodyMax)))
		return pipeline.Action{Type: pipeline.Continue}
	}

	recordResponse(pctx)
	slog.Debug("mcp-parser: streaming frame", "method", pipeline.SanitizeLogValue(pctx.Extensions.MCP.Method),
		"body", pipeline.SanitizeLogValue(parsercommon.Truncate(string(frame), parsercommon.DebugBodyMax)))
	return pipeline.Action{Type: pipeline.Continue}
}

// recordResponse emits the operator-facing log and Observe row for the
// response that mcp.ParseResponse wrote into pctx.Extensions.MCP.
// Shared by OnResponse (buffered) and OnResponseFrame (streaming) so
// the two paths agree on what gets recorded for one envelope.
func recordResponse(pctx *pipeline.Context) {
	if e := pctx.Extensions.MCP.Err; e != nil {
		slog.Info("mcp-parser: response error", "method", pipeline.SanitizeLogValue(pctx.Extensions.MCP.Method),
			"code", e.Code, "message", pipeline.SanitizeLogValue(e.Message))
		pctx.Observe("response_error")
		return
	}
	if r := pctx.Extensions.MCP.Result; r != nil {
		slog.Info("mcp-parser: response", "method", pipeline.SanitizeLogValue(pctx.Extensions.MCP.Method), "resultKeys", resultKeys(r))
	}
	pctx.Observe("matched_" + pctx.Extensions.MCP.Method + "_response")
}

func resultKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

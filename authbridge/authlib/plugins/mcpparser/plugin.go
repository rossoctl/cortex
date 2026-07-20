package mcpparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/rossoctl/cortex/authbridge/authlib/bypass"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/internal/parsercommon"
)

// Synthetic method names emitted on body-less MCP transport-layer
// requests where there's no JSON-RPC method on the wire to report.
//
// The "$" prefix is non-standard — neither MCP nor JSON-RPC 2.0
// formally reserves it (JSON-RPC 2.0 §6 only reserves "rpc.*").
// We chose it because no current MCP spec method uses "$" and
// because it's visually distinct from real category/action method
// names; operators reading abctl can tell at a glance that these
// aren't methods that appeared in the request body. If a future MCP
// revision starts using "$" prefixes, switch this scheme to a less
// likely sentinel (e.g. "_transport/stream") at that time.
const (
	syntheticTransportStream    = "$transport/stream"
	syntheticTransportTerminate = "$transport/terminate"
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

// isMCPAction reports whether a JSON-RPC method name names a user-
// meaningful side-effect operation that guardrails should judge.
// The list is small and grows only when MCP introduces a new method
// that carries user intent on the wire. Everything not in this list
// — protocol setup, capability discovery, subscription management,
// notifications, etc. — is treated as protocol mechanics with
// IsAction=false (the zero value).
//
// Aligned with MCP spec revision 2025-03-26 (Streamable HTTP). When
// MCP adds a new action-shaped method (a hypothetical
// "tools/execute_remote", a "prompts/render_with_data", etc.), update
// this list. The audit anchor is the spec-revision string so future
// maintainers have a date to compare against rather than re-deriving
// the action set from scratch.
func isMCPAction(method string) bool {
	switch method {
	case "tools/call", "prompts/get", "resources/read":
		return true
	}
	return false
}

func (p *MCPParser) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// Body-less transport-layer detection. Scoped to the configured
	// MCP-endpoint paths because there's no protocol payload to
	// confirm; without the path narrow, every body-less GET in the
	// cluster would risk being mis-classified as an MCP SSE channel.
	if len(pctx.Body) == 0 {
		if p.paths != nil && p.paths.Match(pctx.Path) {
			switch {
			case pctx.Method == "DELETE" && pctx.Headers.Get("Mcp-Session-Id") != "":
				// MCP Streamable HTTP session termination per spec —
				// the Mcp-Session-Id header is set by the MCP client
				// SDK, not user input, so it's a precise distinguisher
				// from a real "DELETE /api/users/42" action call.
				pctx.Extensions.MCP = &pipeline.MCPExtension{
					Method: syntheticTransportTerminate,
					// IsAction defaults to false — protocol mechanics.
				}
				slog.Info("mcp-parser: session terminate", "path", pctx.Path)
				pctx.Observe("matched_" + syntheticTransportTerminate)
				return pipeline.Action{Type: pipeline.Continue}

			case pctx.Method == "GET":
				// MCP Streamable HTTP server-to-client SSE channel-open.
				// Heuristic recognition: any body-less GET on a
				// configured MCP path. If the request turns out not to
				// be MCP, the worst-case effect is that guardrails
				// downstream see a "transport/stream" extension and skip
				// it — same effect as the pre-classification behavior
				// of letting body-less GETs through.
				pctx.Extensions.MCP = &pipeline.MCPExtension{
					Method: syntheticTransportStream,
					// IsAction defaults to false — protocol mechanics.
				}
				slog.Info("mcp-parser: transport stream", "path", pctx.Path)
				pctx.Observe("matched_" + syntheticTransportStream)
				return pipeline.Action{Type: pipeline.Continue}
			}
		}
		// Empty body, no MCP-shaped transport pattern matched. Don't
		// attach an extension — the parser doesn't claim this request.
		// Operators infer "mcp-parser exists in this pipeline" from
		// config, not per-event rows.
		slog.Debug("mcp-parser: no body, skipping")
		return pipeline.Action{Type: pipeline.Continue}
	}

	var rpc parsercommon.JSONRPCRequest
	if err := json.Unmarshal(pctx.Body, &rpc); err != nil {
		slog.Debug("mcp-parser: body is not valid JSON-RPC", "error", err, "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}
	// Empty method → body parses as JSON but isn't a JSON-RPC request
	// (e.g. an OpenAI chat/completions body also unmarshals into
	// JSONRPCRequest with zero-value fields). Don't attach a useless
	// MCPExtension to non-MCP traffic — downstream consumers shouldn't
	// see a phantom "mcp: {}" on every inference event.
	if rpc.Method == "" {
		slog.Debug("mcp-parser: body is JSON but not JSON-RPC, skipping", "bodyLen", len(pctx.Body))
		return pipeline.Action{Type: pipeline.Continue}
	}

	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method:   rpc.Method,
		RPCID:    rpc.ID,
		Params:   rpc.Params,
		IsAction: isMCPAction(rpc.Method),
	}

	slog.Info("mcp-parser: request", "method", rpc.Method, "isAction", pctx.Extensions.MCP.IsAction)
	slog.Debug("mcp-parser: payload", "method", rpc.Method, "body", parsercommon.Truncate(string(pctx.Body), parsercommon.DebugBodyMax))

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
	// for JSON-RPC notifications that ack with HTTP 202. Record a Skip
	// so abctl can pair the response row with the request row in the
	// timeline (pairing keys on plugin+method+direction; an empty
	// invocation slot orphans both ends).
	if len(pctx.ResponseBody) == 0 {
		pctx.Skip("no_response_body")
		return pipeline.Action{Type: pipeline.Continue}
	}

	rpc, ok := parseMCPResponse(pctx.ResponseBody)
	if !ok {
		slog.Debug("mcp-parser: response is not valid JSON-RPC or SSE", "bodyLen", len(pctx.ResponseBody))
		pctx.Skip("unparseable_response")
		return pipeline.Action{Type: pipeline.Continue}
	}

	applyMCPResponseRPC(pctx, rpc)
	slog.Debug("mcp-parser: response detail", "method", pctx.Extensions.MCP.Method, "body", parsercommon.Truncate(string(pctx.ResponseBody), parsercommon.DebugBodyMax))
	return pipeline.Action{Type: pipeline.Continue}
}

// maxStreamObserves caps the number of per-frame Observe rows a single
// streaming response may emit. Long tools/call streams can produce
// dozens of result envelopes; without a bound, every envelope appends
// an Invocation row that lives on pctx.Extensions.Invocations for the
// life of the request — both noisy in the session timeline and a
// memory growth point. After the cap, additional frames update
// pctx.Extensions.MCP (so the latest result is still observable
// off-stream) but no Invocation row is appended; one final
// "_truncated" Observe at last=true tells operators what happened.
const maxStreamObserves = 50

// OnResponseFrame is the streaming-aware response hook. Listeners
// invoke it once per SSE frame (text/event-stream) and once with
// last=true at end-of-stream. application/json responses arrive as
// a single last=true frame — so the same code handles both shapes.
//
// Per-message recording: for MCP, each frame's payload is one
// complete JSON-RPC response message. We parse + record per message
// rather than waiting for end-of-stream, so a long-running tools/call
// surfaces partial results in the session timeline as they arrive —
// up to maxStreamObserves rows; beyond that the per-frame Observe is
// suppressed and a single _truncated row is emitted at end-of-stream.
func (p *MCPParser) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	// Stay silent when the request side never participated.
	if pctx.Extensions.MCP == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}

	state := getOrCreateMCPStreamState(pctx)

	// End-of-stream call with no payload. If we never saw a frame with
	// a result/error and the response body was empty, record a Skip
	// (matches the buffered path's "no_response_body" semantics so
	// abctl pairs request and response rows uniformly across shapes).
	// If we observed too many frames, emit a single truncation row so
	// operators see that records were dropped.
	if len(frame) == 0 {
		if last {
			if pctx.Extensions.MCP.Result == nil && pctx.Extensions.MCP.Err == nil && state.observed == 0 {
				pctx.Skip("no_response_body")
			}
			if state.truncated {
				pctx.Observe("matched_" + pctx.Extensions.MCP.Method + "_response_truncated")
			}
		}
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Parse SSE-aware: a frame is normally one SSE event's pre-stripped
	// JSON-RPC payload (streaming path), but the buffered dispatch hands the
	// WHOLE response body as a single last=true frame — and for a Streamable
	// HTTP server that body is a raw `data: {...}` SSE blob. A bare
	// json.Unmarshal fails on that blob and silently drops the result (the
	// tools/list-response-not-recorded bug). parseMCPResponse tries a direct
	// decode first (clean frame), then scans `data:` lines (raw SSE blob), so
	// both shapes record correctly.
	rpc, ok := parseMCPResponse(frame)
	if !ok {
		// Notifications, heartbeats, malformed frames, or other shapes with
		// no JSON-RPC result/error envelope — nothing to record per-frame.
		// The end-of-stream last=true call records a Skip if no envelope was
		// ever seen, so the response row still pairs with its request.
		slog.Debug("mcp-parser: response frame carried no JSON-RPC result/error",
			"frameLen", len(frame), "frame", parsercommon.Truncate(string(frame), parsercommon.DebugBodyMax))
		return pipeline.Action{Type: pipeline.Continue}
	}

	if state.observed >= maxStreamObserves {
		// Past the cap: keep updating ext.MCP so the latest result is
		// reflected, but don't append another Invocation row.
		state.truncated = true
		applyMCPResponseRPCNoObserve(pctx, rpc)
		slog.Debug("mcp-parser: streaming frame (suppressed observe)", "method", pctx.Extensions.MCP.Method, "observed", state.observed)
		return pipeline.Action{Type: pipeline.Continue}
	}

	applyMCPResponseRPC(pctx, rpc)
	state.observed++
	slog.Debug("mcp-parser: streaming frame", "method", pctx.Extensions.MCP.Method, "body", parsercommon.Truncate(string(frame), parsercommon.DebugBodyMax))
	return pipeline.Action{Type: pipeline.Continue}
}

// mcpStreamState holds per-stream scratch on pctx.Extensions.Custom so
// the per-frame Observe cap can be enforced across calls without
// adding fields to the public MCPExtension shape.
type mcpStreamState struct {
	observed  int
	truncated bool
}

const mcpStreamStateKey = "mcp-parser/stream-state"

func getOrCreateMCPStreamState(pctx *pipeline.Context) *mcpStreamState {
	if s := pipeline.GetState[mcpStreamState](pctx, mcpStreamStateKey); s != nil {
		return s
	}
	s := &mcpStreamState{}
	pipeline.SetState(pctx, mcpStreamStateKey, s)
	return s
}

// applyMCPResponseRPCNoObserve mutates pctx.Extensions.MCP without
// emitting an Observe row. Used past the per-stream Observe cap.
func applyMCPResponseRPCNoObserve(pctx *pipeline.Context, rpc jsonRPCResponse) {
	if rpc.Error != nil {
		pctx.Extensions.MCP.Err = &pipeline.MCPError{
			Code:    rpc.Error.Code,
			Message: rpc.Error.Message,
			Data:    rpc.Error.Data,
		}
		return
	}
	if rpc.Result != nil {
		pctx.Extensions.MCP.Result = rpc.Result
	}
}

// applyMCPResponseRPC mutates pctx.Extensions.MCP from a parsed
// JSON-RPC response and emits the operator-facing log + Observe row.
// Shared by OnResponse (buffered) and OnResponseFrame (streaming) so
// the two paths agree on what gets recorded for one envelope.
func applyMCPResponseRPC(pctx *pipeline.Context, rpc jsonRPCResponse) {
	if rpc.Error != nil {
		pctx.Extensions.MCP.Err = &pipeline.MCPError{
			Code:    rpc.Error.Code,
			Message: rpc.Error.Message,
			Data:    rpc.Error.Data,
		}
		slog.Info("mcp-parser: response error", "method", pctx.Extensions.MCP.Method, "code", rpc.Error.Code, "message", rpc.Error.Message)
		pctx.Observe("response_error")
		return
	}
	if rpc.Result != nil {
		pctx.Extensions.MCP.Result = rpc.Result
		slog.Info("mcp-parser: response", "method", pctx.Extensions.MCP.Method, "resultKeys", resultKeys(rpc.Result))
	}
	pctx.Observe("matched_" + pctx.Extensions.MCP.Method + "_response")
}

// parseMCPResponse handles both plain JSON-RPC responses and SSE event streams
// (used by MCP's Streamable HTTP transport). For SSE, the first data: line
// carrying a result or error wins. Malformed SSE data frames are logged at
// DEBUG so a broken upstream is observable rather than silently skipped.
func parseMCPResponse(body []byte) (jsonRPCResponse, bool) {
	var rpc jsonRPCResponse
	if json.Unmarshal(body, &rpc) == nil && (rpc.Result != nil || rpc.Error != nil) {
		return rpc, true
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		var r jsonRPCResponse
		if err := json.Unmarshal(data, &r); err != nil {
			slog.Debug("mcp-parser: skipping malformed SSE data frame", "error", err, "data", parsercommon.Truncate(string(data), 128))
			continue
		}
		if r.Result != nil || r.Error != nil {
			return r, true
		}
	}
	return jsonRPCResponse{}, false
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type jsonRPCResponse struct {
	ID     any            `json:"id"`
	Result map[string]any `json:"result"`
	Error  *jsonRPCError  `json:"error"`
}

func resultKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

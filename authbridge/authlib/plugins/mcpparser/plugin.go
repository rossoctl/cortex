package mcpparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins/internal/parsercommon"
)

// Synthetic method names emitted on body-less MCP transport-layer
// requests where there's no JSON-RPC method on the wire to report.
// The "$" prefix is reserved (real MCP methods follow a category/
// action naming convention with no "$"), so operators reading abctl
// can tell at a glance these aren't methods that appeared in the
// request body.
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
		Writes:      []string{"mcp"},
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

func (p *MCPParser) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	// Stay silent when the request side never participated — the parser
	// recorded nothing on request, so recording on response would orphan
	// the row.
	if pctx.Extensions.MCP == nil {
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

	if rpc.Error != nil {
		pctx.Extensions.MCP.Err = &pipeline.MCPError{
			Code:    rpc.Error.Code,
			Message: rpc.Error.Message,
			Data:    rpc.Error.Data,
		}
		slog.Info("mcp-parser: response error", "method", pctx.Extensions.MCP.Method, "code", rpc.Error.Code, "message", rpc.Error.Message)
		pctx.Observe("response_error")
		return pipeline.Action{Type: pipeline.Continue}
	}

	if rpc.Result != nil {
		pctx.Extensions.MCP.Result = rpc.Result
		slog.Info("mcp-parser: response", "method", pctx.Extensions.MCP.Method, "resultKeys", resultKeys(rpc.Result))
		slog.Debug("mcp-parser: response detail", "method", pctx.Extensions.MCP.Method, "body", parsercommon.Truncate(string(pctx.ResponseBody), parsercommon.DebugBodyMax))
	}

	pctx.Observe("matched_" + pctx.Extensions.MCP.Method + "_response")
	return pipeline.Action{Type: pipeline.Continue}
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

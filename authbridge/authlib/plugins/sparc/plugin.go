// Package sparc implements a pre-tool reflection plugin for authbridge.
//
// Before an agent's proposed tool call executes, the plugin asks an external
// SPARC reflection service (the agent-lifecycle-toolkit / ALTK SPARC component,
// served over HTTP) whether the call is *grounded* in the conversation and the
// available tool specifications. SPARC catches hallucinated / ungrounded
// arguments (e.g. an invented transaction id) and inappropriate tool selection.
//
// Generic by design — works for ANY kagenti agent. SPARC's three inputs are
// collected from exactly what the agent produces, with no bespoke per-agent
// wiring:
//
//   - conversation history (INCLUDING the system prompt) — from the agent's
//     LLM call captured by inference-parser (pctx.Extensions.Inference.Messages,
//     which preserves every role verbatim);
//   - tool specifications in OpenAI function-calling format — from the same
//     inference call (pctx.Extensions.Inference.Tools);
//   - the proposed tool call in OpenAI format — from the outbound MCP
//     tools/call (mcp mode) or from the LLM response (inference mode).
//
// Enforcement is format-aware (the verdict is returned to the agent in the
// shape it expects):
//
//   - enforcement: "mcp" (default; the kagenti norm) — gate the outbound MCP
//     tools/call. On a reflected reject, return SPARC's clarification as a
//     JSON-RPC MCP tool *result*; the agent's MCP client consumes it like any
//     tool output and asks the user for the missing detail. Robust to LLM
//     streaming (reads the actual tool call, not the streamed response).
//   - enforcement: "inference" — gate the agent's LLM response (where all three
//     inputs are co-located). On a reflected reject, rewrite the completion so
//     the assistant turn carries SPARC's clarification (tool_calls dropped).
//     For agents that don't route tools through MCP.
//
// All enforcement policy lives here; the service only returns SPARC's verdict.
package sparc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/bypass"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

// enforcement modes.
const (
	EnforcementMCP       = "mcp"       // gate the outbound MCP tools/call (default)
	EnforcementInference = "inference" // gate the agent's LLM response (non-MCP agents)
)

// on_reject_action values: what to do when SPARC returns "reject".
const (
	// OnRejectObserve records the reject and lets the call through (shadow mode).
	OnRejectObserve = "observe"
	// OnRejectReflect returns SPARC's reflection to the agent (MCP tool result
	// in mcp mode, or a rewritten LLM response in inference mode) so the agent
	// transparently re-asks the user. This is "reflection mode".
	OnRejectReflect = "reflect"
	// OnRejectDeny hard-blocks the call.
	OnRejectDeny = "deny"
)

// fail_policy values: what to do when SPARC is unavailable or returns "error".
const (
	FailOpen   = "open"   // allow + record (default — SPARC is a quality gate, not auth)
	FailClosed = "closed" // block
)

// sparcConfig is the plugin's local config schema. Field tags drive both
// runtime decoding (json) and operator-facing schema introspection
// (description / required / default / enum), matching ibac and the auth plugins.
// See authbridge/docs/sparc-plugin.md for prose.
type sparcConfig struct {
	ReflectorEndpoint string `json:"reflector_endpoint" required:"true" description:"Base URL of the SPARC reflection service. The plugin POSTs to {endpoint}/reflect."`

	ReflectorBearer string `json:"reflector_bearer" description:"Optional bearer token for the reflection service. Empty for in-cluster unauthenticated calls."`

	Enforcement string `json:"enforcement" description:"Where to gate and how to return the verdict: mcp=gate the MCP tools/call, return an MCP tool result; inference=gate the LLM response, rewrite the completion." default:"mcp" enum:"mcp,inference"`

	Track string `json:"track" description:"SPARC reflection track." default:"fast_track" enum:"fast_track,slow_track,syntax,spec_free,transformations_only"`

	TimeoutMs int `json:"timeout_ms" description:"Per-call reflection timeout. SPARC LLM calls take seconds. Validation rejects values below 100." default:"30000"`

	OnRejectAction string `json:"on_reject_action" description:"Behavior on a SPARC reject: observe=log only; reflect=return SPARC's clarification to the agent; deny=hard block." default:"reflect" enum:"observe,reflect,deny"`

	DenyScoreThreshold float64 `json:"deny_score_threshold" description:"When SPARC's grounding score (0=worst..1=best) is <= this, escalate any reject to a hard deny regardless of on_reject_action. 0 disables escalation." default:"0"`

	FailPolicy string `json:"fail_policy" description:"On SPARC unavailable or decision=error: open=allow and record; closed=block." default:"open" enum:"open,closed"`

	SkipTools []string `json:"skip_tools" description:"Tool-name globs (path.Match) to NOT reflect on, e.g. trivial read-only tools. Evaluated before reflect_tools."`

	ReflectTools []string `json:"reflect_tools" description:"If non-empty, ONLY reflect on tools whose name matches one of these globs; all others are skipped."`

	BypassHosts []string `json:"bypass_hosts" description:"Host globs (path.Match) skipped without reflecting. Defaults include keycloak / spire / otel."`

	BypassPaths []string `json:"bypass_paths" description:"URL path globs skipped without reflecting. Defaults: /.well-known/* /healthz /readyz /livez."`
}

// defaultBypassHosts mirrors ibac's conservative starting set (kept local so a
// future ibac tweak can't silently change sparc behavior).
var defaultBypassHosts = []string{
	"keycloak.*", "keycloak", "spire-server.*", "spire-agent.*",
	"otel-collector.*", "jaeger.*", "prometheus.*",
}

var defaultBypassPaths = []string{"/healthz", "/readyz", "/livez", "/.well-known/*"}

func (c *sparcConfig) applyDefaults() {
	if c.Enforcement == "" {
		c.Enforcement = EnforcementMCP
	}
	if c.Track == "" {
		c.Track = "fast_track"
	}
	if c.TimeoutMs == 0 {
		c.TimeoutMs = 30000
	}
	if c.OnRejectAction == "" {
		c.OnRejectAction = OnRejectReflect
	}
	if c.FailPolicy == "" {
		c.FailPolicy = FailOpen
	}
	if len(c.BypassHosts) == 0 {
		c.BypassHosts = defaultBypassHosts
	}
	if len(c.BypassPaths) == 0 {
		c.BypassPaths = defaultBypassPaths
	}
}

func (c *sparcConfig) validate() error {
	if c.ReflectorEndpoint == "" {
		return errors.New("reflector_endpoint is required")
	}
	if c.TimeoutMs < 100 {
		return fmt.Errorf("timeout_ms must be at least 100, got %d", c.TimeoutMs)
	}
	switch c.Enforcement {
	case EnforcementMCP, EnforcementInference:
	default:
		return fmt.Errorf("enforcement must be mcp or inference, got %q", c.Enforcement)
	}
	switch c.OnRejectAction {
	case OnRejectObserve, OnRejectReflect, OnRejectDeny:
	default:
		return fmt.Errorf("on_reject_action must be observe|reflect|deny, got %q", c.OnRejectAction)
	}
	switch c.FailPolicy {
	case FailOpen, FailClosed:
	default:
		return fmt.Errorf("fail_policy must be open or closed, got %q", c.FailPolicy)
	}
	if c.DenyScoreThreshold < 0 || c.DenyScoreThreshold > 1 {
		return fmt.Errorf("deny_score_threshold must be in [0,1], got %v", c.DenyScoreThreshold)
	}
	for _, g := range []struct {
		label string
		pats  []string
	}{{"bypass_hosts", c.BypassHosts}, {"bypass_paths", c.BypassPaths}, {"skip_tools", c.SkipTools}, {"reflect_tools", c.ReflectTools}} {
		for _, p := range g.pats {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("invalid %s pattern %q: %w", g.label, p, err)
			}
		}
	}
	for _, p := range c.BypassHosts {
		if t := strings.TrimSpace(p); t == "" || t == "*" {
			return fmt.Errorf("bypass_hosts pattern %q matches everything; remove sparc from the pipeline instead", p)
		}
	}
	for _, p := range c.BypassPaths {
		if t := strings.TrimSpace(p); t == "" || t == "*" || t == "/*" {
			return fmt.Errorf("bypass_paths pattern %q matches everything; remove sparc from the pipeline instead", p)
		}
	}
	return nil
}

// SPARC reflects on proposed tool calls and enforces the configured policy.
type SPARC struct {
	cfg         sparcConfig
	reflector   Reflector
	bypassPaths *bypass.Matcher
	bypassHosts []string
	timeout     time.Duration
}

// NewSPARC constructs an unconfigured plugin. Configure must be called first.
func NewSPARC() *SPARC { return &SPARC{} }

func init() {
	plugins.RegisterPlugin("sparc", func() pipeline.Plugin { return NewSPARC() })
}

func (p *SPARC) Name() string { return "sparc" }

func (p *SPARC) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		// Needs a parser to supply SPARC's inputs. inference-parser provides the
		// conversation + tool specs (both modes); mcp-parser provides the tool
		// call (mcp mode). RequiresAny is a static "at least one" check; the
		// per-mode runtime requirements are validated/handled below.
		RequiresAny: []string{"inference-parser", "mcp-parser"},
		ReadsBody:   true,
		WritesBody:  true, // MCP result (mcp mode) / completion rewrite (inference mode)
		Description: "SPARC pre-tool reflection: blocks ungrounded/hallucinated tool calls.",
	}
}

func (p *SPARC) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(sparcConfig{})
}

func (p *SPARC) Configure(raw json.RawMessage) error {
	var c sparcConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("sparc config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("sparc config: %w", err)
	}
	p.cfg = c

	matcher, err := bypass.NewMatcher(c.BypassPaths)
	if err != nil {
		return fmt.Errorf("sparc bypass_paths: %w", err)
	}
	p.bypassPaths = matcher
	p.bypassHosts = c.BypassHosts
	p.timeout = time.Duration(c.TimeoutMs) * time.Millisecond
	p.reflector = newHTTPReflector(c.ReflectorEndpoint, c.ReflectorBearer, p.timeout)
	return nil
}

// OnRequest handles mcp mode (gate the outbound MCP tools/call).
func (p *SPARC) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.reflector == nil {
		return pipeline.DenyStatus(503, "sparc.unconfigured", "sparc plugin not configured")
	}
	if p.cfg.Enforcement != EnforcementMCP {
		return cont()
	}
	if pctx.Headers.Get(sentinelHeader) == "1" {
		return cont()
	}
	if p.bypassPaths.Match(pctx.Path) {
		pctx.Skip("path_bypass")
		return cont()
	}
	if matchesAnyHost(p.bypassHosts, pctx.Host) {
		pctx.Skip("host_bypass")
		return cont()
	}
	anyAction, anyBypass := pctx.Classification()
	if anyBypass {
		pctx.Skip("protocol_mechanics")
		return cont()
	}
	if !anyAction {
		return cont()
	}

	toolName := extractMCPToolName(pctx.Extensions.MCP)
	if toolName == "" {
		pctx.Skip("not_tool_call")
		return cont()
	}
	if p.toolSkipped(toolName) {
		pctx.Skip("tool_skipped")
		return cont()
	}

	messages, toolSpecs, ok := correlateInferenceContext(pctx)
	if !ok {
		// No conversation/tool context to ground against. This is a missing
		// prerequisite (no inference-parser event / session tracking off), not
		// a verdict — apply fail_policy so a misconfigured pipeline doesn't
		// silently run unprotected.
		return p.handleNoContext(pctx, toolName, EnforcementMCP)
	}
	in := ReflectInput{
		Messages:  messages,
		ToolSpecs: toolSpecs,
		ToolCalls: []map[string]any{buildToolCall(pctx.Extensions.MCP, toolName)},
		SessionID: sessionID(pctx),
		Track:     p.cfg.Track,
	}

	verdict, err := p.reflector.Reflect(ctx, in)
	if err != nil {
		return p.handleUnavailable(pctx, toolName, err, EnforcementMCP)
	}
	p.emitEvent(pctx, toolName, verdict)

	switch verdict.Decision {
	case DecisionApprove:
		p.recordAllow(pctx, toolName, verdict, EnforcementMCP)
		return cont()
	case DecisionReject:
		return p.handleReject(pctx, toolName, verdict, EnforcementMCP)
	default:
		return p.handleUnavailable(pctx, toolName,
			fmt.Errorf("%w: sparc decision=%q", ErrReflectorUnavailable, verdict.Decision), EnforcementMCP)
	}
}

// OnResponse handles inference mode (gate the agent's LLM response).
func (p *SPARC) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.cfg.Enforcement != EnforcementInference {
		return cont()
	}
	inf := pctx.Extensions.Inference
	if inf == nil || len(inf.ToolCalls) == 0 {
		return cont() // not an LLM tool-call response (e.g. streamed or plain text)
	}
	tc := inf.ToolCalls[0]
	if tc.Name == "" {
		return cont()
	}
	if p.toolSkipped(tc.Name) {
		pctx.Skip("tool_skipped")
		return cont()
	}

	messages, toolSpecs, ok := inferenceInputs(inf)
	if !ok {
		return p.handleNoContext(pctx, tc.Name, EnforcementInference)
	}
	in := ReflectInput{
		Messages:  messages,
		ToolSpecs: toolSpecs,
		ToolCalls: []map[string]any{openAIToolCall(tc.ID, tc.Name, tc.Arguments)},
		SessionID: sessionID(pctx),
		Track:     p.cfg.Track,
	}

	verdict, err := p.reflector.Reflect(ctx, in)
	if err != nil {
		return p.handleUnavailable(pctx, tc.Name, err, EnforcementInference)
	}
	p.emitEvent(pctx, tc.Name, verdict)

	switch verdict.Decision {
	case DecisionApprove:
		p.recordAllow(pctx, tc.Name, verdict, EnforcementInference)
		return cont()
	case DecisionReject:
		return p.handleReject(pctx, tc.Name, verdict, EnforcementInference)
	default:
		return p.handleUnavailable(pctx, tc.Name,
			fmt.Errorf("%w: sparc decision=%q", ErrReflectorUnavailable, verdict.Decision), EnforcementInference)
	}
}

// handleReject applies on_reject_action with score-driven escalation. The
// returned action is format-aware per enforcement mode.
func (p *SPARC) handleReject(pctx *pipeline.Context, toolName string, verdict ReflectVerdict, mode string) pipeline.Action {
	action := p.cfg.OnRejectAction
	escalated := false
	if p.cfg.DenyScoreThreshold > 0 && verdict.OverallAvgScore != nil &&
		*verdict.OverallAvgScore <= p.cfg.DenyScoreThreshold {
		action = OnRejectDeny
		escalated = true
	}
	details := map[string]string{
		"tool": toolName, "score": scoreString(verdict.OverallAvgScore),
		"sparc": firstExplanation(verdict), "action": action,
		"escalated": fmt.Sprintf("%t", escalated),
	}

	switch action {
	case OnRejectObserve:
		details["mode"] = "observe"
		pctx.Record(inv(pipeline.ActionObserve, mode, "reject_observed", details))
		return cont()
	case OnRejectReflect:
		pctx.Record(inv(pipeline.ActionModify, mode, "reflected", details))
		if mode == EnforcementInference {
			rewriteInferenceResponse(pctx, buildClarificationText(verdict))
			return cont()
		}
		return mcpResultAction(mcpRPCID(pctx.Extensions.MCP), buildClarificationText(verdict))
	default: // OnRejectDeny
		pctx.Record(inv(pipeline.ActionDeny, mode, "blocked", details))
		reason := firstExplanation(verdict)
		if mode == EnforcementInference {
			rewriteInferenceResponse(pctx, "Tool call blocked by policy: "+reason)
			return cont()
		}
		return pipeline.DenyStatus(403, "sparc.blocked", reason)
	}
}

// handleUnavailable applies fail_policy when SPARC can't be reached or errored.
func (p *SPARC) handleUnavailable(pctx *pipeline.Context, toolName string, err error, mode string) pipeline.Action {
	errPreview := preview(err.Error(), 240)
	details := map[string]string{"tool": toolName, "error": errPreview}
	if p.cfg.FailPolicy == FailOpen {
		details["mode"] = "fail_open"
		pctx.Record(inv(pipeline.ActionObserve, mode, "reflector_unavailable", details))
		slog.Warn("sparc: reflector unavailable, failing open", "host", pctx.Host, "error", errPreview)
		return cont()
	}
	pctx.Record(inv(pipeline.ActionDeny, mode, "reflector_unavailable", details))
	slog.Warn("sparc: reflector unavailable, failing closed", "host", pctx.Host, "error", errPreview)
	if mode == EnforcementInference {
		rewriteInferenceResponse(pctx, "Tool call blocked: reflection service unavailable.")
		return cont()
	}
	return pipeline.DenyStatus(503, "sparc.reflector_unavailable", errPreview)
}

// handleNoContext applies fail_policy when the conversation/tool context needed
// to ground a tool call is missing (no inference-parser event, or session
// tracking off). fail_open skips and lets the call through — recorded so the gap
// is visible in the session timeline; fail_closed blocks rather than letting an
// unverifiable call past. Default-open behavior is unchanged from a plain skip.
func (p *SPARC) handleNoContext(pctx *pipeline.Context, toolName, mode string) pipeline.Action {
	if p.cfg.FailPolicy == FailOpen {
		pctx.Skip("no_inference_context")
		return cont()
	}
	pctx.Record(inv(pipeline.ActionDeny, mode, "no_inference_context", map[string]string{"tool": toolName}))
	slog.Warn("sparc: no inference context to ground against, failing closed", "host", pctx.Host, "tool", toolName)
	if mode == EnforcementInference {
		rewriteInferenceResponse(pctx, "Tool call blocked: no conversation context available to verify it.")
		return cont()
	}
	return pipeline.DenyStatus(503, "sparc.no_inference_context", "no conversation context to verify the tool call")
}

func (p *SPARC) recordAllow(pctx *pipeline.Context, toolName string, verdict ReflectVerdict, mode string) {
	pctx.Record(inv(pipeline.ActionAllow, mode, "grounded", map[string]string{
		"tool": toolName, "score": scoreString(verdict.OverallAvgScore),
	}))
}

// emitEvent publishes the full SPARC verdict via the plugin-event escape-hatch
// so abctl / session consumers can render the structured reflection.
func (p *SPARC) emitEvent(pctx *pipeline.Context, toolName string, verdict ReflectVerdict) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom["sparc"+pipeline.PluginEventSuffix] = sparcEvent{
		Tool:        toolName,
		Decision:    verdict.Decision,
		Score:       verdict.OverallAvgScore,
		Track:       p.cfg.Track,
		Enforcement: p.cfg.Enforcement,
		Issues:      verdict.Issues,
		ExecutionMs: verdict.ExecutionTimeMs,
	}
}

// toolSkipped reports whether a tool is excluded from reflection by config.
func (p *SPARC) toolSkipped(name string) bool {
	if anyGlobMatch(p.cfg.SkipTools, name) {
		return true
	}
	if len(p.cfg.ReflectTools) > 0 && !anyGlobMatch(p.cfg.ReflectTools, name) {
		return true
	}
	return false
}

// sparcEvent is the structured reflection record surfaced to session consumers.
type sparcEvent struct {
	Tool        string         `json:"tool"`
	Decision    string         `json:"decision"`
	Score       *float64       `json:"score,omitempty"`
	Track       string         `json:"track"`
	Enforcement string         `json:"enforcement"`
	Issues      []ReflectIssue `json:"issues,omitempty"`
	ExecutionMs float64        `json:"executionMs,omitempty"`
}

func cont() pipeline.Action { return pipeline.Action{Type: pipeline.Continue} }

func inv(action pipeline.InvocationAction, mode, reason string, details map[string]string) pipeline.Invocation {
	// mcp enforcement runs in OnRequest (request phase); inference enforcement
	// runs in OnResponse (response phase). Tag the invocation accordingly so it
	// lands in the right session-event snapshot.
	phase := pipeline.InvocationPhaseRequest
	if mode == EnforcementInference {
		phase = pipeline.InvocationPhaseResponse
	}
	if mode != "" {
		details["enforcement"] = mode
	}
	return pipeline.Invocation{
		Action: action, Phase: phase, Reason: reason, Details: details,
	}
}

// Compile-time interface checks.
var (
	_ pipeline.Plugin       = (*SPARC)(nil)
	_ pipeline.Configurable = (*SPARC)(nil)
)

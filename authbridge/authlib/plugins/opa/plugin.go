package opa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/open-policy-agent/opa/sdk"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"
)

const (
	pathInboundRequest   = "authbridge/inbound/request"
	pathInboundResponse  = "authbridge/inbound/response"
	pathOutboundRequest  = "authbridge/outbound/request"
	pathOutboundResponse = "authbridge/outbound/response"
)

type opaConfig struct {
	BundleURL       string   `json:"bundle_url"`
	AgentIDFile     string   `json:"agent_id_file"`
	AgentID         string   `json:"agent_id"`
	PollingMinDelay int      `json:"polling_min_delay"`
	PollingMaxDelay int      `json:"polling_max_delay"`
	Include         []string `json:"include"`
}

// includeSet is built once at Configure time from the Include config list.
// It controls which optional field groups appear in the OPA input document.
type includeSet map[string]bool

func newIncludeSet(keys []string) includeSet {
	s := make(includeSet, len(keys)+2)
	// Default-on keys (always present)
	s["mcp.params.name"] = true
	s["mcp.params.uri"] = true
	for _, k := range keys {
		s[k] = true
	}
	return s
}

func (s includeSet) has(key string) bool {
	return s[key]
}

// hasParamKey returns true if the full mcp.params map is included OR the
// specific mcp.params.<key> is listed.
func (s includeSet) hasParamKey(key string) bool {
	if s["mcp.params"] {
		return true
	}
	return s["mcp.params."+key]
}

func (c *opaConfig) applyDefaults() {
	if c.AgentID == "" && c.AgentIDFile == "" {
		c.AgentIDFile = "/shared/client-id.txt"
	}
	if c.PollingMinDelay <= 0 {
		c.PollingMinDelay = 10
	}
	if c.PollingMaxDelay <= 0 {
		c.PollingMaxDelay = 120
	}
}

func (c *opaConfig) validate() error {
	if c.BundleURL == "" {
		return errors.New("bundle_url is required")
	}
	return nil
}

// decider abstracts OPA decision-making for testability.
type decider interface {
	Decision(ctx context.Context, options sdk.DecisionOptions) (*sdk.DecisionResult, error)
	Stop(ctx context.Context)
}

// OPA evaluates requests against OPA bundles downloaded from a Kagenti
// Bundle Server. The bundle resource path is derived from the agent's
// identity (/shared/client-id.txt).
type OPA struct {
	cfg      opaConfig
	inc      includeSet
	agentID  string
	decider  atomic.Pointer[decider]
	ready    atomic.Bool
	bgCancel atomic.Pointer[context.CancelFunc]
}

func init() {
	plugins.RegisterPlugin("opa", func() pipeline.Plugin { return &OPA{} })
}

func (p *OPA) Name() string { return "opa" }

func (p *OPA) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		Requires:    []string{"jwt-validation"},
		RequiresAny: []string{"a2a-parser", "mcp-parser", "inference-parser"},
		Description: "OPA policy enforcement for inbound and outbound requests.",
	}
}

func (p *OPA) Configure(raw json.RawMessage) error {
	var c opaConfig
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("opa config: %w", err)
		}
	}
	agentIDFileExplicit := c.AgentIDFile != ""
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("opa config: %w", err)
	}
	p.cfg = c
	p.inc = newIncludeSet(c.Include)

	if c.AgentID != "" {
		p.agentID = c.AgentID
	} else if c.AgentIDFile != "" {
		if v, err := config.ReadCredentialFile(c.AgentIDFile); err == nil {
			p.agentID = v
		} else {
			if agentIDFileExplicit {
				slog.Warn("opa: agent_id_file not yet readable; Init will poll in background",
					"path", c.AgentIDFile, "error", err)
			} else {
				slog.Warn("opa: agent_id_file defaulted to Kagenti convention and not yet readable; "+
					"Init will poll in background. Set agent_id or agent_id_file if not running under Kagenti.",
					"path", c.AgentIDFile, "error", err)
			}
		}
	}
	return nil
}

func (p *OPA) Init(_ context.Context) error {
	if p.agentID != "" {
		return p.startOPA()
	}
	if p.cfg.AgentIDFile == "" {
		return errors.New("opa: no agent_id or agent_id_file configured")
	}
	if p.bgCancel.Load() != nil {
		return nil
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	p.bgCancel.Store(&cancel)
	go func() {
		v, err := config.WaitForCredentialFile(bgCtx, p.cfg.AgentIDFile)
		if err != nil {
			slog.Debug("opa: agent_id_file wait stopped",
				"path", p.cfg.AgentIDFile, "error", err)
			return
		}
		p.agentID = v
		if err := p.startOPA(); err != nil {
			slog.Error("opa: failed to start OPA after agent_id_file became available",
				"error", err)
		}
	}()
	return nil
}

func (p *OPA) startOPA() error {
	cfgBytes, agentID, err := p.buildOPAConfig()
	if err != nil {
		return err
	}
	readyCh := make(chan struct{})
	opa, err := sdk.New(context.Background(), sdk.Options{
		Config: bytes.NewReader(cfgBytes),
		Ready:  readyCh,
	})
	if err != nil {
		return fmt.Errorf("opa sdk.New: %w", err)
	}
	var dec decider = opa
	p.decider.Store(&dec)
	go func() {
		<-readyCh
		p.ready.Store(true)
		slog.Info("opa: bundle loaded and policy activated", "agent_id", agentID)
	}()
	return nil
}

func (p *OPA) buildOPAConfig() ([]byte, string, error) {
	agentID := p.agentID

	if agentID == "" {
		return nil, "", errors.New("agentID is empty")
	}

	// Escape agentID to prevent path traversal and ensure safe URL path segment
	escapedAgentID := url.PathEscape(agentID)

	cfg := map[string]any{
		"services": map[string]any{
			"kagenti": map[string]any{
				"url": p.cfg.BundleURL,
			},
		},
		"bundles": map[string]any{
			"authz": map[string]any{
				"service":  "kagenti",
				"resource": fmt.Sprintf("bundles/%s.tar.gz", escapedAgentID),
				"polling": map[string]any{
					"min_delay_seconds": p.cfg.PollingMinDelay,
					"max_delay_seconds": p.cfg.PollingMaxDelay,
				},
			},
		},
	}
	data, _ := json.Marshal(cfg)
	return data, agentID, nil
}

func (p *OPA) Shutdown(ctx context.Context) error {
	if cancel := p.bgCancel.Swap(nil); cancel != nil {
		(*cancel)()
	}
	if dec := p.decider.Load(); dec != nil {
		(*dec).Stop(ctx)
	}
	return nil
}

func (p *OPA) Ready() bool {
	return p.ready.Load()
}

func (p *OPA) decisionPath(pctx *pipeline.Context, phase string) string {
	if pctx.Direction == pipeline.Outbound {
		if phase == "response" {
			return pathOutboundResponse
		}
		return pathOutboundRequest
	}
	if phase == "response" {
		return pathInboundResponse
	}
	return pathInboundRequest
}

func (p *OPA) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	dec := p.decider.Load()
	if dec == nil {
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: "opa_not_ready",
		})
		return pipeline.DenyStatus(503, "upstream.unreachable", "opa policy engine not initialized")
	}

	path := p.decisionPath(pctx, "request")
	input := buildInput(pctx, p.inc)
	result, err := (*dec).Decision(ctx, sdk.DecisionOptions{
		Path:  path,
		Input: input,
	})
	if err != nil {
		if sdk.IsUndefinedErr(err) {
			pctx.Skip("no_policy_rule")
			return pipeline.Action{Type: pipeline.Continue}
		}
		slog.Warn("opa: decision error on request", "error", err, "decision_path", path, "request_path", pctx.Path)
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: "decision_error",
			Details: map[string]string{
				"error": err.Error(),
			},
		})
		return pipeline.Deny("policy.forbidden", fmt.Sprintf("OPA decision error: %v", err))
	}

	allowed, reason := interpretDecision(result)
	if !allowed {
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: "policy_denied",
			Details: map[string]string{
				"opa_reason": reason,
			},
		})
		return pipeline.Deny("policy.forbidden", reason)
	}

	pctx.Allow("policy_allowed")
	return pipeline.Action{Type: pipeline.Continue}
}

func (p *OPA) OnResponse(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	dec := p.decider.Load()
	if dec == nil {
		pctx.Skip("opa_not_ready")
		return pipeline.Action{Type: pipeline.Continue}
	}

	path := p.decisionPath(pctx, "response")
	input := buildInput(pctx, p.inc)
	input["response"] = map[string]any{
		"status_code": pctx.StatusCode,
		"headers":     flattenHeaders(pctx.ResponseHeaders),
	}
	result, err := (*dec).Decision(ctx, sdk.DecisionOptions{
		Path:  path,
		Input: input,
	})
	if err != nil {
		if sdk.IsUndefinedErr(err) {
			pctx.Skip("no_policy_rule")
			return pipeline.Action{Type: pipeline.Continue}
		}
		slog.Warn("opa: decision error on response", "error", err, "decision_path", path, "request_path", pctx.Path)
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: "response_decision_error",
			Details: map[string]string{
				"error": err.Error(),
			},
		})
		return pipeline.Deny("policy.forbidden", fmt.Sprintf("OPA response decision error: %v", err))
	}

	allowed, reason := interpretDecision(result)
	if !allowed {
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: "response_policy_denied",
			Details: map[string]string{
				"opa_reason": reason,
			},
		})
		return pipeline.Deny("policy.forbidden", reason)
	}

	pctx.Allow("response_policy_allowed")
	return pipeline.Action{Type: pipeline.Continue}
}

func buildInput(pctx *pipeline.Context, inc includeSet) map[string]any {
	input := map[string]any{
		"direction": pctx.Direction.String(),
		"method":    pctx.Method,
		"path":      pctx.Path,
		"host":      pctx.Host,
		"headers":   flattenHeaders(pctx.Headers),
	}
	if pctx.Identity != nil {
		input["identity"] = map[string]any{
			"subject":   pctx.Identity.Subject(),
			"client_id": pctx.Identity.ClientID(),
			"scopes":    pctx.Identity.Scopes(),
		}
	}
	if pctx.Agent != nil {
		input["agent"] = map[string]any{
			"client_id": pctx.Agent.ClientID,
		}
	}

	if pctx.Extensions.A2A != nil {
		input["a2a"] = buildA2AInput(pctx.Extensions.A2A, inc)
	}
	if pctx.Extensions.MCP != nil {
		input["mcp"] = buildMCPInput(pctx.Extensions.MCP, inc)
	}
	if pctx.Extensions.Inference != nil {
		input["inference"] = buildInferenceInput(pctx.Extensions.Inference, inc)
	}

	return input
}

func buildA2AInput(ext *pipeline.A2AExtension, inc includeSet) map[string]any {
	a2a := map[string]any{
		"method":     ext.Method,
		"session_id": ext.SessionID,
		"task_id":    ext.TaskID,
		"role":       ext.Role,
	}
	if inc.has("a2a.content") {
		if len(ext.Parts) > 0 {
			parts := make([]map[string]any, len(ext.Parts))
			for i, part := range ext.Parts {
				parts[i] = map[string]any{
					"kind":    part.Kind,
					"content": part.Content,
				}
			}
			a2a["parts"] = parts
		}
		if ext.FinalStatus != "" {
			a2a["final_status"] = ext.FinalStatus
		}
		if ext.Artifact != "" {
			a2a["artifact"] = ext.Artifact
		}
		if ext.ErrorMessage != "" {
			a2a["error_message"] = ext.ErrorMessage
		}
	}
	return a2a
}

func buildMCPInput(ext *pipeline.MCPExtension, inc includeSet) map[string]any {
	mcp := map[string]any{
		"method": ext.Method,
	}
	if len(ext.Params) > 0 {
		if inc.has("mcp.params") {
			mcp["params"] = ext.Params
		} else {
			filtered := make(map[string]any)
			for k, v := range ext.Params {
				if inc.hasParamKey(k) {
					filtered[k] = v
				}
			}
			if len(filtered) > 0 {
				mcp["params"] = filtered
			}
		}
	}
	if inc.has("mcp.result") && len(ext.Result) > 0 {
		mcp["result"] = ext.Result
	}
	if inc.has("mcp.error") && ext.Err != nil {
		errMap := map[string]any{
			"code":    ext.Err.Code,
			"message": ext.Err.Message,
		}
		if ext.Err.Data != nil {
			errMap["data"] = ext.Err.Data
		}
		mcp["error"] = errMap
	}
	return mcp
}

func buildInferenceInput(ext *pipeline.InferenceExtension, inc includeSet) map[string]any {
	inf := map[string]any{
		"model":  ext.Model,
		"stream": ext.Stream,
	}
	if ext.MaxTokens != nil {
		inf["max_tokens"] = *ext.MaxTokens
	}
	if len(ext.Tools) > 0 {
		if inc.has("inference.tools.detail") {
			tools := make([]map[string]any, len(ext.Tools))
			for i, tool := range ext.Tools {
				tools[i] = map[string]any{
					"name":        tool.Name,
					"description": tool.Description,
				}
				if len(tool.Parameters) > 0 {
					tools[i]["parameters"] = tool.Parameters
				}
			}
			inf["tools"] = tools
		} else {
			names := make([]string, len(ext.Tools))
			for i, tool := range ext.Tools {
				names[i] = tool.Name
			}
			inf["tools"] = names
		}
	}
	if inc.has("inference.messages") && len(ext.Messages) > 0 {
		messages := make([]map[string]any, len(ext.Messages))
		for i, msg := range ext.Messages {
			messages[i] = map[string]any{
				"role":    msg.Role,
				"content": msg.Content,
			}
		}
		inf["messages"] = messages
	}
	if inc.has("inference.completion") && ext.Completion != "" {
		inf["completion"] = ext.Completion
	}
	if inc.has("inference.tool_calls") && len(ext.ToolCalls) > 0 {
		toolCalls := make([]map[string]any, len(ext.ToolCalls))
		for i, tc := range ext.ToolCalls {
			toolCalls[i] = map[string]any{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			if tc.ID != "" {
				toolCalls[i]["id"] = tc.ID
			}
		}
		inf["tool_calls"] = toolCalls
	}
	return inf
}

func flattenHeaders(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	flat := make(map[string]string, len(h))
	for k, vals := range h {
		flat[strings.ToLower(k)] = strings.Join(vals, ",")
	}
	return flat
}

// interpretDecision extracts an allow/deny decision from the OPA result.
// Deny-by-default: unrecognized shapes are treated as denials.
func interpretDecision(result *sdk.DecisionResult) (allowed bool, reason string) {
	if result == nil || result.Result == nil {
		return false, "no decision result"
	}
	switch v := result.Result.(type) {
	case bool:
		if v {
			return true, ""
		}
		return false, "policy denied"
	case map[string]any:
		if allow, ok := v["allow"].(bool); ok {
			if allow {
				return true, ""
			}
			if r, ok := v["reason"].(string); ok {
				return false, r
			}
			return false, "policy denied"
		}
		return false, "unrecognized policy result shape"
	default:
		return false, fmt.Sprintf("unexpected decision type %T", result.Result)
	}
}

var (
	_ pipeline.Configurable = (*OPA)(nil)
	_ pipeline.Initializer  = (*OPA)(nil)
	_ pipeline.Shutdowner   = (*OPA)(nil)
	_ pipeline.Readier      = (*OPA)(nil)
)

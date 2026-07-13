// Package contextguru is an outbound AuthBridge plugin that compacts an agent's
// LLM request context before it is forwarded upstream, using the embedded
// github.com/kagenti/context-guru engine. It runs in the pre-LLM hook
// (OnRequest): it hands the outbound request body to apply.BodyWithModel, and if
// the engine rewrote it (dropped/reduced tool outputs, injected cache_control,
// etc.) it replaces the body via pctx.SetBody. OnResponse is a pass-through in
// v1 — model-driven restoration/expand is a later integration.
//
// It is the single outbound WritesBody plugin, so it is mutually exclusive with
// SPARC on the outbound chain (the pipeline refuses to build with two). It
// declares RequiresAny: [inference-parser] so a parser establishes the request
// is an inference call before it runs.
//
// LLM-backed engine components (summarize, extract:code) are optional: configure
// a `model:` block to enable a static cheap model, and the plugin also
// reconstructs the agent's own "incoming" model from the live request so a
// component with model.source: incoming can reuse it. With no model available
// those components degrade to deterministic/no-op.
package contextguru

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/llmclient"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/plugins"

	"github.com/kagenti/context-guru/apply"
	cgcomponents "github.com/kagenti/context-guru/components"
	_ "github.com/kagenti/context-guru/components/offload"  // register offload components
	_ "github.com/kagenti/context-guru/components/reformat" // register reformat components
	cgconfig "github.com/kagenti/context-guru/config"
	cgstore "github.com/kagenti/context-guru/store"
	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// sentinelHeader is set on the plugin's own outbound LLM calls (via llmclient) so
// that if such a call ever transits this forward proxy, OnRequest short-circuits
// instead of recursing. Belt-and-suspenders alongside llmclient's standalone
// http.Client, which does not route back through the listener.
const sentinelHeader = "X-Context-Guru-LLM"

// defaultModelTimeout bounds a single engine LLM call (summarize/extract). It is
// generous because summarize compresses a whole trajectory in one call; the
// engine sets its own shorter context deadlines on top.
const defaultModelTimeout = 150 * time.Second

// modelConfig defines the optional static "cheap" model the LLM-backed engine
// components (extract:code, summarize) call. Absent → those components degrade
// to deterministic. The wire is always OpenAI-compatible chat-completions
// (llmclient POSTs base_url + "/v1/chat/completions"); point base_url at any
// OpenAI-compatible endpoint (ollama /v1, a litellm proxy, vLLM, OpenAI, …).
type modelConfig struct {
	BaseURL   string `json:"base_url"`   // e.g. http://cheap-llm.svc:8000  (/v1/chat/completions appended)
	Model     string `json:"model"`      // e.g. gpt-4o-mini
	APIKey    string `json:"api_key"`    // optional bearer
	MaxTokens int    `json:"max_tokens"` // completion cap; 0 → 4096
	TimeoutMs int    `json:"timeout_ms"` // per-call ceiling; 0 → defaultModelTimeout
}

// logEmitter is context-guru's telemetry sink: it logs what the engine did on
// each request so operators can observe the plugin from AuthBridge's own logs
// (per-request summary at INFO, per-component detail at DEBUG). Passed to
// cfg.Build so every pipeline run reports through it.
type logEmitter struct{}

func (logEmitter) Component(r cgcomponents.Report) {
	if r.Skipped {
		return
	}
	slog.Debug("context-guru component",
		"component", r.Component, "kind", r.Kind,
		"tokensBefore", r.TokensBefore, "tokensAfter", r.TokensAfter, "saved", r.Saved(),
		"reverted", r.Reverted, "irreversible", r.Irreversible, "err", r.Err)
}

func (logEmitter) Run(r cgcomponents.RunReport) {
	if r.TokensAfter >= r.TokensBefore {
		return // nothing saved — stay quiet
	}
	fired := make([]string, 0, len(r.Components))
	for _, c := range r.Components {
		if !c.Skipped && !c.Reverted {
			fired = append(fired, c.Component)
		}
	}
	slog.Info("context-guru compacted request",
		"session", r.Session, "tokensBefore", r.TokensBefore, "tokensAfter", r.TokensAfter,
		"tokensSaved", r.Saved(), "pctSaved", pct(r.TokensBefore, r.TokensAfter), "components", fired)
}

func pct(before, after int) string {
	if before <= 0 {
		return "0%"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(before-after)/float64(before))
}

// contextGuruConfig is the plugin's config subtree. Plugin-only keys (paths,
// model) sit alongside `engine`, the native context-guru config passed verbatim
// to config.LoadBytes (preset / pipeline / per-component blocks incl. marker_mode
// / store).
type contextGuruConfig struct {
	Paths  []string        `json:"paths"`
	Model  *modelConfig    `json:"model"`
	Engine json.RawMessage `json:"engine"`
}

// cgModel adapts an llmclient.Client to context-guru's components.Model interface
// (single prompt in, text out).
type cgModel struct {
	c         *llmclient.Client
	maxTokens int
}

func (m cgModel) Complete(ctx context.Context, prompt string) (string, error) {
	resp, err := m.c.CallRaw(ctx, &llmclient.ChatRequest{
		Temperature: 0,
		MaxTokens:   m.maxTokens,
		Messages:    []llmclient.ChatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("contextguru: model returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// ContextGuru is the plugin. pipe/store are built once at Configure and shared
// across requests (store is synchronized; the engine holds no mutable pipeline
// state). static is the configured cheap model (nil when none).
type ContextGuru struct {
	cfg      contextGuruConfig
	pipe     *cgcomponents.Pipeline
	store    cgstore.Store
	static   cgcomponents.Model
	modelTO  time.Duration
	modelMax int
}

// New returns an unconfigured plugin instance.
func New() *ContextGuru { return &ContextGuru{} }

func init() {
	plugins.RegisterPlugin("context-guru", func() pipeline.Plugin { return New() })
}

func (p *ContextGuru) Name() string { return "context-guru" }

func (p *ContextGuru) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:   true,
		WritesBody:  true, // single outbound body-writer slot (mutually exclusive with SPARC)
		RequiresAny: []string{"inference-parser"},
		Description: "Compacts the outbound LLM request context before forwarding (context-guru).",
	}
}

func (p *ContextGuru) Configure(raw json.RawMessage) error {
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p.cfg); err != nil {
			return fmt.Errorf("context-guru config: %w", err)
		}
	}
	if len(p.cfg.Paths) == 0 {
		p.cfg.Paths = []string{"/v1/chat/completions", "/v1/completions", "/v1/messages"}
	}

	// Engine config: default to the balanced preset when none is supplied.
	engine := p.cfg.Engine
	if len(engine) == 0 {
		engine = []byte("preset: balanced")
	}
	cfg, err := cgconfig.LoadBytes(engine)
	if err != nil {
		return fmt.Errorf("context-guru engine config: %w", err)
	}
	pipe, err := cfg.Build(logEmitter{}) // log what the engine does per request/component
	if err != nil {
		return fmt.Errorf("context-guru build pipeline: %w", err)
	}
	p.pipe = pipe
	p.store = cfg.NewStore()

	// Model timeouts / caps (shared by static + incoming clients).
	p.modelTO = defaultModelTimeout
	p.modelMax = 4096
	if m := p.cfg.Model; m != nil {
		if m.TimeoutMs > 0 {
			p.modelTO = time.Duration(m.TimeoutMs) * time.Millisecond
		}
		if m.MaxTokens > 0 {
			p.modelMax = m.MaxTokens
		}
		if m.BaseURL != "" && m.Model != "" {
			p.static = cgModel{
				c: llmclient.New(llmclient.Options{
					Endpoint:           m.BaseURL,
					Model:              m.Model,
					Bearer:             m.APIKey,
					Timeout:            p.modelTO,
					SentinelHeaderName: sentinelHeader,
				}),
				maxTokens: p.modelMax,
			}
		}
	}
	slog.Info("context-guru configured",
		"paths", p.cfg.Paths, "modelConfigured", p.static != nil,
		"engineBytes", len(engine))
	return nil
}

func (p *ContextGuru) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	cont := pipeline.Action{Type: pipeline.Continue}
	// The plugin's own LLM calls carry the sentinel; never recurse into them.
	if pctx.Headers != nil && pctx.Headers.Get(sentinelHeader) != "" {
		return cont
	}
	if p.pipe == nil || len(pctx.Body) == 0 || !p.gated(pctx.Path) {
		return cont
	}
	provider := providerFor(pctx.Path)
	models := cgcomponents.ModelSpec{
		Static:   p.static,
		Incoming: p.incomingModel(pctx, provider),
	}
	before := len(pctx.Body)
	out, changed := apply.BodyWithModel(ctx, p.pipe, p.store, provider, pctx.Body, sessionID(pctx), false, models)
	if changed && len(out) > 0 {
		pctx.SetBody(out)
		// Per-request byte-level view (the engine's token-level view is logged by
		// logEmitter.Run). The framework also emits a body-mutation session event.
		slog.Info("context-guru rewrote request body",
			"provider", provider, "path", pctx.Path, "session", sessionID(pctx),
			"bytesBefore", before, "bytesAfter", len(out), "pctSaved", pct(before, len(out)))
	}
	return cont
}

// OnResponse is a pass-through in v1. The model-driven expand loop (resolve
// markers from the Store + re-invoke upstream via llmclient) would live here in a
// later integration; the Store + session key are already threaded for it.
func (p *ContextGuru) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// gated reports whether the request path is an inference path the plugin acts on.
func (p *ContextGuru) gated(path string) bool {
	for _, s := range p.cfg.Paths {
		if path == s || strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}

// providerFor maps the request path to the bifrost provider dialect the engine
// parses the body as.
func providerFor(path string) bschemas.ModelProvider {
	if strings.HasSuffix(path, "/v1/messages") {
		return bschemas.Anthropic
	}
	return bschemas.OpenAI
}

// incomingModel reconstructs the agent's own model client from the live request
// (base URL + token-exchanged Authorization + the request's model), so an engine
// component with model.source: incoming reuses it. OpenAI-dialect only (llmclient
// speaks /v1/chat/completions); returns nil for anthropic or when the pieces
// aren't present, in which case the engine falls back to the static model or
// degrades to deterministic.
func (p *ContextGuru) incomingModel(pctx *pipeline.Context, provider bschemas.ModelProvider) cgcomponents.Model {
	if provider != bschemas.OpenAI || pctx.Host == "" {
		return nil
	}
	model := modelFromBody(pctx.Body)
	if model == "" {
		return nil
	}
	scheme := pctx.Scheme
	if scheme == "" {
		scheme = "http"
	}
	bearer := ""
	if pctx.Headers != nil {
		bearer = strings.TrimPrefix(pctx.Headers.Get("Authorization"), "Bearer ")
	}
	return cgModel{
		c: llmclient.New(llmclient.Options{
			Endpoint:           scheme + "://" + pctx.Host,
			Model:              model,
			Bearer:             bearer,
			Timeout:            p.modelTO,
			SentinelHeaderName: sentinelHeader,
		}),
		maxTokens: p.modelMax,
	}
}

// modelFromBody pulls the top-level "model" field from an OpenAI/Anthropic
// chat-completions request body.
func modelFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// sessionID keys per-session engine state to the conversation: the A2A contextId
// when present, else the caller subject for multi-tenant isolation.
func sessionID(pctx *pipeline.Context) string {
	if pctx.Session != nil && pctx.Session.ID != "" {
		return pctx.Session.ID
	}
	if pctx.Identity != nil {
		return pctx.Identity.Subject()
	}
	return ""
}

// ConfigSchema surfaces the operator-facing fields in abctl / /v1/plugins.
func (p *ContextGuru) ConfigSchema() []pipeline.FieldSchema {
	return []pipeline.FieldSchema{
		{Name: "paths", Type: "[]string", Description: "Inference request paths to compact (default: chat/completions, completions, messages)."},
		{Name: "model", Type: "object", Description: "Optional static cheap model for LLM-backed components (summarize, extract:code)."},
		{Name: "engine", Type: "object", Description: "Native context-guru config: preset / pipeline / per-component (marker_mode) / store."},
	}
}

var (
	_ pipeline.Plugin         = (*ContextGuru)(nil)
	_ pipeline.Configurable   = (*ContextGuru)(nil)
	_ pipeline.SchemaProvider = (*ContextGuru)(nil)
)

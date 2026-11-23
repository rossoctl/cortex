// Package contextguru is an outbound AuthBridge plugin that compacts an agent's
// LLM request context before it is forwarded upstream, using the embedded
// github.com/rossoctl/context-guru engine. It runs in the pre-LLM hook
// (OnRequest): it hands the outbound request body to apply.BodyWithModel, and if
// the engine rewrote it (dropped/reduced tool outputs, injected cache_control,
// etc.) it replaces the body via pctx.SetBody. OnResponse is a pass-through in
// v1 — model-driven restoration/expand is a later integration.
//
// It is a request-body writer, and request-body writers chain: it may share the
// outbound chain with tool-prune or any other, each seeing the body as the one
// before it left it. It declares RequiresAny: [inference-parser] so a parser
// establishes the request is an inference call before it runs.
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
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/llmclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	cgcomponents "github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/offload"  // register offload components
	_ "github.com/rossoctl/context-guru/components/reformat" // register reformat components
	cgconfig "github.com/rossoctl/context-guru/config"
	cgstore "github.com/rossoctl/context-guru/store"
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
// (single prompt in, text out). model/host identify the call for accounting
// (accountant.ModelCall.Model/Host); report, when set, is the request this call was made
// for, and Complete reports the call to it as a KindSpend context operation (W4.6) before
// returning. report is captured at call time because it is per-request while a cgModel
// wrapping the plugin's static model is shared across requests.
//
// The engine calls Complete concurrently (its extract stage runs six calls at once) and
// sometimes after OnRequest returned (summarize_async, cache_aware_async). So Complete reads
// nothing of pctx's dispatch state: report is a pipeline.OpReporter, captured on the request
// goroutine (via pctx.OpReporter(), while the plugin is dispatched) when the model was built, so
// Report is safe to call later from any goroutine and stamps the op with the plugin's own name
// and its on_error: observe state without Complete having to know either. A call that returns
// after the request was accounted has its op dropped by the pipeline and counted in lateDropped.
type cgModel struct {
	c           *llmclient.Client
	maxTokens   int
	model       string
	host        string
	report      pipeline.OpReporter
	lateDropped *atomic.Int64
}

// pluginName is the plugin's registered name, and the Operator of its model-call spend ops.
const pluginName = "context-guru"

// spend reports one model call as a KindSpend op. A zero usage means the cost is unknown.
func (m cgModel) spend(usage accountant.CallUsage) {
	accepted := m.report.Report(accountant.Operation{
		Kind: accountant.KindSpend,
		Call: &accountant.ModelCall{Host: m.host, Model: m.model, Usage: usage, Detached: false},
	})
	if !accepted && m.lateDropped != nil {
		m.lateDropped.Add(1)
	}
}

func (m cgModel) Complete(ctx context.Context, prompt string) (string, error) {
	resp, err := m.c.CallRaw(ctx, &llmclient.ChatRequest{
		Temperature: 0,
		MaxTokens:   m.maxTokens,
		Messages:    []llmclient.ChatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		// The server answered, so the call may be billed: report it with its cost unknown. Any
		// other error may come before the request was sent, and a call never sent costs nothing.
		if errors.Is(err, llmclient.ErrResponded) {
			m.spend(accountant.CallUsage{})
		}
		return "", err
	}
	var usage accountant.CallUsage
	if u := resp.Usage; u != nil {
		// prompt_tokens includes the cached tokens (the OpenAI convention ChatUsage states). A
		// gateway that counts them apart can report more cached tokens than prompt tokens, and
		// Input must not go negative.
		cached := min(u.CachedTokens(), u.PromptTokens)
		usage = accountant.CallUsage{
			Usage:  accountant.Usage{Input: u.PromptTokens - cached, CacheRead: cached},
			Output: u.CompletionTokens,
		}
	}
	// With no usage block, usage stays zero, which the accountant reads as "sent, cost unknown".
	m.spend(usage)
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("contextguru: model returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// hostOf extracts the host component of a base URL for accountant.ModelCall.Host (the
// upstream the call went to). Falls back to the raw string when it does not parse as a
// URL with a host (e.g. just "host:port" with no scheme).
func hostOf(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return baseURL
}

// ContextGuru is the plugin. pipe/store are built once at Configure and shared
// across requests (store is synchronized; the engine holds no mutable pipeline
// state). static is the configured cheap model's template (nil when none); it
// carries no pctx, since it is shared across requests — OnRequest copies it per
// request with staticModelFor so Complete can report its spend op to the right one.
type ContextGuru struct {
	cfg      contextGuruConfig
	pipe     *cgcomponents.Pipeline
	store    cgstore.Store
	sentinel string // unguessable value carried by our own LLM calls
	static   *cgModel
	modelTO  time.Duration
	modelMax int

	// localInstall is set by plugins.BuildWithDeps before Configure: on a local install the
	// model calls bypass HTTPS_PROXY (see localModelTransport).
	localInstall bool

	// lateModelCalls counts model calls whose spend op was dropped because the call returned
	// after its request was accounted (cgModel). Reported by Metrics.
	lateModelCalls atomic.Int64
}

// Metrics implements pipeline.MetricsProvider.
func (p *ContextGuru) Metrics() []pipeline.Metric {
	return []pipeline.Metric{{
		Name:  "late_model_calls_dropped",
		Value: float64(p.lateModelCalls.Load()),
		Unit:  "count",
		Note:  "model calls that returned after their request was accounted; their spend reaches no figure",
	}}
}

// New returns an unconfigured plugin instance.
func New() *ContextGuru { return &ContextGuru{} }

func init() {
	plugins.RegisterPlugin(pluginName, func() pipeline.Plugin { return New() })
}

func (p *ContextGuru) Name() string { return pluginName }

func (p *ContextGuru) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:         true,
		WritesRequestBody: true, // chains with any other request-body writer
		RequiresAny:       []string{"inference-parser"},
		Description:       "Compacts the outbound LLM request context before forwarding (context-guru).",
	}
}

// SetLocalInstall is how plugins.BuildWithDeps says whether this is a local install, which decides
// whether the plugin's model calls may use HTTPS_PROXY.
func (p *ContextGuru) SetLocalInstall(local bool) { p.localInstall = local }

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
	// Per-process random value, not a constant: outbound headers are the
	// agent's own request, and a caller who can set headers must not be able
	// to switch context-guru off by setting them.
	p.sentinel = llmclient.NewSentinelValue()

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
		switch {
		case m.BaseURL == "" && m.Model == "":
			// Empty model block → no static model; LLM components degrade to
			// deterministic. (Common when the config is present but the operator
			// hasn't wired an endpoint yet.)
		case m.BaseURL == "" || m.Model == "":
			// Exactly one set is almost always a typo/empty ${VAR} expansion that
			// would silently disable the cheap model. Fail loud instead.
			return fmt.Errorf("context-guru model config: base_url and model must both be set (got base_url=%q model=%q)", m.BaseURL, m.Model)
		default:
			p.static = &cgModel{
				c: llmclient.New(llmclient.Options{
					Endpoint:           m.BaseURL,
					Model:              m.Model,
					Bearer:             m.APIKey,
					Timeout:            p.modelTO,
					SentinelHeaderName: sentinelHeader,
					SentinelValue:      p.sentinel,
					HTTPClient:         modelHTTPClient(p.modelTO, p.localInstall),
				}),
				maxTokens: p.modelMax,
				model:     m.Model,
				host:      hostOf(m.BaseURL),
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
	// Equality with the per-process secret, not mere presence: a caller
	// setting the header to any guessed value skips nothing.
	if pctx.Headers != nil && pctx.Headers.Get(sentinelHeader) == p.sentinel {
		pctx.Skip("self_call")
		return cont
	}
	if p.pipe == nil || len(pctx.Body) == 0 || !p.gated(pctx.Path) {
		return cont
	}
	// The engine Store is keyed by session for per-caller isolation. With no
	// session identity every such request would share one empty key and bleed
	// compaction state across unrelated callers, so skip compaction entirely.
	sid := sessionID(pctx)
	if sid == "" {
		slog.Debug("context-guru skipped request with no session identity", "path", pipeline.SanitizeLogValue(pctx.Path))
		return cont
	}
	provider := providerFor(pctx.Path)
	models := p.modelsFor(pctx, provider)
	before := len(pctx.Body)
	res := apply.BodyOpts(ctx, p.pipe, p.store, apply.Opts{
		Provider: provider,
		Body:     pctx.Body,
		Session:  sid,
		Models:   models,
	})
	out, changed, ops := reportResult(pctx, res)
	if changed && len(out) > 0 {
		// SetBody decides whether this write actually applies (true unless a non-observe
		// writer ahead of this plugin already enforced its own bytes — pipeline/plugin-reference.md's
		// shadow-write case). Ops are stamped and reported AFTER this call, never before: reporting
		// first would price a shadow write as a real saving, which can fail the library's
		// client-vs-forwarded sum check and zero the whole request's figure, tool-prune's savings on
		// it included (review finding on plugin.go:485). ReportOps's own DryRun stamp (on_error:
		// observe) is a different, narrower case — see pipeline.Context.ReportOps's doc comment —
		// and stampDryRun's OR leaves it untouched when it already applies.
		applied := pctx.SetBody(out)
		stampDryRun(ops, !applied)
		pctx.ReportOps(ops...)
		// Per-request byte-level view (the engine's token-level view is logged by
		// logEmitter.Run). The framework also emits a body-mutation session event.
		msg := "context-guru rewrote request body"
		if !applied {
			msg = "context-guru would rewrite request body (on_error: observe, not applied)"
		}
		slog.Info(msg,
			"provider", provider, "path", pipeline.SanitizeLogValue(pctx.Path), "session", sid,
			"bytesBefore", before, "bytesAfter", len(out), "pctSaved", pct(before, len(out)))
	}
	return cont
}

// stampDryRun sets DryRun on every op whose DryRun is not already true. Called with dryRun=true
// when the caller's SetBody did not apply the rewrite these ops describe, so a shadow write is
// never priced as if it reached the wire.
func stampDryRun(ops []accountant.Operation, dryRun bool) {
	if !dryRun {
		return
	}
	for i := range ops {
		ops[i].DryRun = true
	}
}

// reportResult turns one apply.BodyOpts result into the body to forward, whether it changed, and
// the ops that describe it — WITHOUT reporting them. Split out of OnRequest so the accounting
// side (what would be reported) is testable directly against a hand-built apply.Result, without
// re-running the engine — a second apply.BodyOpts call against the same session/store would
// observe state the first call already mutated, not reproduce it.
//
// Thread: names the request's accounting thread from the engine's own thread split
// (design §5.4/§5.5). res.Trace.Thread.ID is empty with no Opts.ThreadOf wired (unchanged
// in this task), and SetAccountingThread's empty-id no-op then leaves
// core/accounting.ThreadOf's default rule ("main" for the primary agent) in place.
//
// Ops: the engine's Trace.Ops plus the envelope ops (envelopeOps), built only when the engine
// produced a rewrite (changed && non-empty out). The caller must call pctx.SetBody(out) BEFORE
// reporting these — mirroring tool-prune's applied := pctx.SetBody(out); ReportOps(...!applied)
// ordering — and stamp DryRun from whether that write actually applied, not from whether the
// engine merely computed it: reporting here, before SetBody, would price a shadow write (an
// earlier enforced writer's bytes already in effect) as a real saving.
func reportResult(pctx *pipeline.Context, res apply.Result) (out []byte, changed bool, ops []accountant.Operation) {
	threadParent, _ := res.Trace.Thread.InheritsFrom()
	pctx.SetAccountingThread(res.Trace.Thread.ID, threadParent)
	out, changed = res.Body, res.Changed
	if changed && len(out) > 0 {
		ops = append(ops, res.Trace.Ops...)
		// The tools and system changes apply makes report no op of their own; they are found
		// by diffing this plugin's input against its output (envelope.go). pctx.Body is still
		// the input here: the caller sets the output after this returns.
		ops = append(ops, envelopeOps(pctx.Body, out)...)
	}
	return out, changed, ops
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

// staticModelFor returns the configured cheap model for one request, carrying pctx so
// cgModel.Complete can report that model call's spend op to the right request. p.static
// is a template shared across requests (built once at Configure) and holds no pctx of
// its own; nil when no static model is configured.
func (p *ContextGuru) staticModelFor(pctx *pipeline.Context) cgcomponents.Model {
	if p.static == nil {
		return nil
	}
	m := *p.static
	m.report = pctx.OpReporter()
	m.lateDropped = &p.lateModelCalls
	return m
}

// providerFor maps the request path to the bifrost provider dialect the engine
// parses the body as.
func providerFor(path string) bschemas.ModelProvider {
	if strings.HasSuffix(path, "/v1/messages") {
		return bschemas.Anthropic
	}
	return bschemas.OpenAI
}

// modelsFor is the engine's model spec for one request: the static model, and the request's own
// model as the incoming one (incomingModel; OpenAI only). An absent client stays a nil
// interface, never a typed nil: the engine checks the interface.
//
// modelsFor builds the static and incoming models for this request, UNLESS the plugin runs
// under on_error: observe, when it builds neither.
//
// Observing's own doc says a shadow pass changes nothing: "nothing it does this pass takes
// effect". The keep-alive keeper already holds this line (OnRequest: "UNDER on_error: observe
// NOTHING IS HELD... that ping is real, billed spend — the opposite of a shadow run"). Before
// this, the engine's own model-calling components (cache_aware_summarizer, extract_llm,
// extract_llm_sweep) did not: cgModel.Complete always sends the real HTTP
// call, and dryRun only changes how the resulting op is REPORTED (Projected, left out of every
// total) — the call itself, and its cost on the caller's own credential, already happened
// (review finding). Returning an empty ModelSpec here makes every model.source: incoming or
// model: component degrade to deterministic/no-op under observe, the same way the keeper
// degrades to no ping: observe measures what the DETERMINISTIC reducers would do, at the cost
// of not previewing what the model-backed ones would add.
func (p *ContextGuru) modelsFor(pctx *pipeline.Context, provider bschemas.ModelProvider) cgcomponents.ModelSpec {
	if pctx.Observing() {
		return cgcomponents.ModelSpec{}
	}
	spec := cgcomponents.ModelSpec{Static: p.staticModelFor(pctx)}
	if m := p.incomingModel(pctx, provider); m != nil {
		spec.Incoming = m
	}
	return spec
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
			SentinelValue:      p.sentinel,
			HTTPClient:         modelHTTPClient(p.modelTO, p.localInstall),
		}),
		maxTokens:   p.modelMax,
		model:       model,
		host:        pctx.Host,
		report:      pctx.OpReporter(),
		lateDropped: &p.lateModelCalls,
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
// when present, else the caller subject for multi-tenant isolation. Returns ""
// when neither is available; OnRequest treats that as "skip compaction" rather
// than sharing one store key across unrelated callers.
func sessionID(pctx *pipeline.Context) string {
	if pctx.SessionID != "" {
		return pctx.SessionID
	}
	if pctx.Identity != nil {
		return pctx.Identity.Subject()
	}
	return ""
}

// ConfigSchema surfaces the operator-facing fields in agentop / /v1/plugins.
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

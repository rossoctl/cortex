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
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/llmclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	cgcomponents "github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/offload"  // register offload components
	_ "github.com/rossoctl/context-guru/components/reformat" // register reformat components
	cgconfig "github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/proxyapp"
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

// KeepAlivePing logs each ping. It carries no header and no body. pingEmitter adds the
// accountant's record (ping.go); with no accountant host wired this line is the only record.
func (logEmitter) KeepAlivePing(r cgcomponents.KeepAliveReport) {
	slog.Info("context-guru keep-alive ping", "session", r.Session, "model", r.Model,
		"ping", r.Pings, "cacheRead", r.CacheRead, "cacheWrite", r.CacheWrite,
		"output", r.Output, "costUSD", r.CostUSD, "status", r.Status, "ms", r.DurationMs)
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
	// KeepAlive is the opt-in prompt-cache keep-alive. Off unless enabled: true.
	KeepAlive keepAliveConfig `json:"keep_alive"`
	// ContextWindow says where model windows come from, for the engine's fraction triggers.
	ContextWindow windowsConfig `json:"context_window"`
}

// cgModel adapts an llmclient.Client to context-guru's components.Model interface
// (single prompt in, text out). model/host identify the call for accounting
// (accountant.ModelCall.Model/Host); pctx, when set, is the request this call was made
// for, and Complete reports the call to it as a KindSpend context operation (W4.6)
// before returning. pctx is captured at call time because it is per-request while a
// cgModel wrapping the plugin's static model is shared across requests.
//
// The engine calls Complete concurrently (its extract stage runs six calls at once) and
// sometimes after OnRequest returned (summarize_async, cache_aware_async). So Complete reads
// nothing of pctx's dispatch state: dryRun is the plugin's on_error: observe state, captured
// on the request goroutine when the model was built, and the op is reported with
// pctx.ReportOpsAs under the plugin's own name. A call that returns after the request was
// accounted has its op dropped by the pipeline and counted in lateDropped.
type cgModel struct {
	c           *llmclient.Client
	maxTokens   int
	model       string
	host        string
	pctx        *pipeline.Context
	dryRun      bool
	lateDropped *atomic.Int64
}

// pluginName is the plugin's registered name, and the Operator of its model-call spend ops.
const pluginName = "context-guru"

func (m cgModel) Complete(ctx context.Context, prompt string) (string, error) {
	resp, err := m.c.CallRaw(ctx, &llmclient.ChatRequest{
		Temperature: 0,
		MaxTokens:   m.maxTokens,
		Messages:    []llmclient.ChatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	if resp.Usage != nil && m.pctx != nil {
		cached := resp.Usage.CachedTokens()
		accepted := m.pctx.ReportOpsAs(pluginName, m.dryRun, accountant.Operation{
			Kind: accountant.KindSpend,
			Call: &accountant.ModelCall{
				Host:  m.host,
				Model: m.model,
				Usage: accountant.CallUsage{
					Usage: accountant.Usage{
						Input:     resp.Usage.PromptTokens - cached,
						CacheRead: cached,
					},
					Output: resp.Usage.CompletionTokens,
				},
				Detached: false,
			},
		})
		if !accepted && m.lateDropped != nil {
			m.lateDropped.Add(1)
		}
	}
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
	rates    pricing.Resolver
	// acct is the process's accountant host, nil when accounting is off. Keep-alive pings are
	// accounted through it (ping.go).
	acct *accounting.Host

	// windows resolves a model's context window; see windows.go.
	windows proxyapp.Resolver
	// cgSessions maps a Cortex session id to the session key the engine derived for it
	// (apply.Trace.Session), so OnFinish records billed input under the key OnRequest reads.
	// One entry per session, overwritten on each request, bounded by sessionKeysCap.
	cgSessions sessionKeys

	// Keep-alive, all nil/zero unless keep_alive.enabled. The keeper decides when to ping; disp
	// only sends and holds the credential.
	keeper *keepalive.Keeper
	disp   *cortexDispatcher
	kaPol  keepalive.Policy
	// stopped is set at the start of Shutdown. After a hot-reload the old pipeline is shut down
	// once its drain window ends, and a long streaming request still on it reaches OnFinish
	// later. keepalive.Keeper.Record has no stopped check of its own, so without this flag that
	// request would hold its credential again on a keeper whose sweep is gone.
	stopped atomic.Bool
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

// SetPricingResolver receives Cortex's rate table (plugins.Deps, before Configure). The
// keep-alive dispatcher prices its pre-flight ceiling from it.
func (p *ContextGuru) SetPricingResolver(r pricing.Resolver) { p.rates = r }

// SetAccounting receives the process's accountant host (plugins.Deps, before Configure).
// Keep-alive pings are accounted through it; with none, a ping is only logged.
func (p *ContextGuru) SetAccounting(h *accounting.Host) { p.acct = h }

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
	p.windows = buildWindows(p.cfg.ContextWindow)
	// Per-process random value, not a constant: outbound headers are the
	// agent's own request, and a caller who can set headers must not be able
	// to switch context-guru off by setting them.
	p.sentinel = llmclient.NewSentinelValue()

	// A keeper built here stays unstarted until Init runs. The normal lifecycle is Configure, then
	// Init, on a fresh instance; a Configure after Init does not happen there, and a keeper it
	// built would not sweep until Init is called again.
	if p.keeper != nil {
		// A second Configure on one instance must not orphan the first keeper's holds.
		p.keeper.Shutdown()
		p.disp.Close()
		p.keeper, p.disp, p.kaPol = nil, nil, keepalive.Policy{}
	}
	if p.cfg.KeepAlive.Enabled {
		p.kaPol = p.cfg.KeepAlive.policy()
		p.disp = newCortexDispatcher(newPingClient())
		p.disp.sentinel = p.sentinel
		p.disp.rates = func() pricing.Resolver { return p.rates }
		p.keeper = keepalive.New(keepalive.Options{Dispatcher: p.disp})
		args := []any{"idle", p.kaPol.Idle, "maxPings", p.kaPol.MaxPings, "maxUSDPerPing", p.kaPol.MaxUSDPerPing,
			"minPrefixTokens", p.kaPol.MinPrefixTokens}
		if p.acct == nil {
			// A WARN, not a refusal: refusing would turn keep-alive off wherever accounting is off.
			slog.Warn("context-guru keep-alive enabled: idle sessions are pinged on the caller's own "+
				"credential, and with no accountant host wired that spend is logged but does not reach "+
				"Cortex's cost ledger", args...)
		} else {
			slog.Info("context-guru keep-alive enabled: idle sessions are pinged on the caller's own "+
				"credential, and that spend is accounted through the accountant host", args...)
		}
	}

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
					HTTPClient:         modelHTTPClient(p.modelTO),
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
	if p.selfCall(pctx) {
		pctx.Skip("self_call")
		return cont
	}
	if !p.gated(pctx.Path) {
		return cont
	}
	sid := sessionID(pctx)
	// A real request on this session ends its idle span: the keeper drops the entry (and the
	// credential it held) before this request goes upstream, so no ping races it. A subagent's
	// request says nothing about whether the main thread is idle, and the main thread is the only
	// one recorded, so it leaves the main thread's entry alone.
	//
	// isSubagent is checked with whatever Extensions.Inference holds right now — nil when, as
	// here, this plugin runs before inference-parser's own OnRequest. A request that only turns
	// out to be a subagent's once inference-parser has run still opens an arrival here, and
	// OnFinish's own isSubagent check (via recordFor) can then disagree with this one. The
	// kaArrived marker records what THIS call decided, so OnFinish departs exactly the arrivals
	// this call made — never fewer, never (on a session a concurrent request shares) one too many.
	if p.keeper != nil && !p.stopped.Load() && sid != "" && !isSubagent(pctx) {
		p.keeper.Arrive(keepalive.Key{Session: sid})
		pipeline.SetState(pctx, kaArrivedStateKey, &kaArrived{})
	}
	if p.pipe == nil || len(pctx.Body) == 0 {
		return cont
	}
	// The engine Store is keyed by session for per-caller isolation. With no
	// session identity every such request would share one empty key and bleed
	// compaction state across unrelated callers, so skip compaction entirely.
	if sid == "" {
		slog.Debug("context-guru skipped request with no session identity", "path", pipeline.SanitizeLogValue(pctx.Path))
		return cont
	}
	provider := providerFor(pctx.Path)
	models := cgcomponents.ModelSpec{
		Static:   p.staticModelFor(pctx),
		Incoming: p.incomingModel(pctx, provider),
	}
	before := len(pctx.Body)
	window, exact := p.windowFor(ctx, pctx)
	res := apply.BodyOpts(ctx, p.pipe, p.store, apply.Opts{
		Provider:    provider,
		Body:        pctx.Body,
		Session:     sid,
		Models:      models,
		Window:      window,
		WindowExact: exact,
	})
	if res.Trace.Session != "" {
		p.cgSessions.set(sid, res.Trace.Session)
	}
	out, changed := reportResult(pctx, res)
	if changed && len(out) > 0 {
		// Per-request byte-level view (the engine's token-level view is logged by
		// logEmitter.Run). The framework also emits a body-mutation session event.
		// The message follows this plugin's own SetBody result, not BodyMutated: under
		// on_error: observe the write is a shadow and the client's body goes upstream.
		msg := "context-guru rewrote request body"
		if !pctx.SetBody(out) {
			msg = "context-guru would rewrite request body (on_error: observe, not applied)"
		}
		slog.Info(msg,
			"provider", provider, "path", pipeline.SanitizeLogValue(pctx.Path), "session", sid,
			"bytesBefore", before, "bytesAfter", len(out), "pctSaved", pct(before, len(out)))
	}
	return cont
}

// reportResult reports one apply.BodyOpts result to the accountant and returns the body
// to forward and whether it changed. Split out of OnRequest so the accounting side
// (what gets reported, and when) is testable directly against a hand-built apply.Result,
// without re-running the engine — a second apply.BodyOpts call against the same
// session/store would observe state the first call already mutated, not reproduce it.
//
// Thread: names the request's accounting thread from the engine's own thread split
// (design §5.4/§5.5). res.Trace.Thread.ID is empty with no Opts.ThreadOf wired (unchanged
// in this task), and SetAccountingThread's empty-id no-op then leaves
// core/accounting.ThreadOf's default rule ("main" for the primary agent) in place.
//
// Ops: the engine's Trace.Ops plus the envelope ops (envelopeOps), reported only when the
// engine produced a rewrite (changed && non-empty out), and
// regardless of whether the caller's subsequent SetBody actually applies it — ReportOps
// itself stamps DryRun when the plugin runs under on_error: observe, so the accountant
// still sees what the engine WOULD have sent (W5.1/W4.6).
func reportResult(pctx *pipeline.Context, res apply.Result) (out []byte, changed bool) {
	threadParent, _ := res.Trace.Thread.InheritsFrom()
	pctx.SetAccountingThread(res.Trace.Thread.ID, threadParent)
	out, changed = res.Body, res.Changed
	if changed && len(out) > 0 {
		pctx.ReportOps(res.Trace.Ops...)
		// The tools and system changes apply makes report no op of their own; they are found
		// by diffing this plugin's input against its output (envelope.go). pctx.Body is still
		// the input here: the caller sets the output after this returns.
		pctx.ReportOps(envelopeOps(pctx.Body, out)...)
	}
	return out, changed
}

// OnResponse is a pass-through in v1. The model-driven expand loop (resolve
// markers from the Store + re-invoke upstream via llmclient) would live here in a
// later integration; the Store + session key are already threaded for it.
func (p *ContextGuru) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// selfCall reports whether pctx is one of the plugin's own calls (an engine model call or a
// keep-alive ping), by equality with the per-process sentinel: a caller setting the header to
// any guessed value skips nothing.
func (p *ContextGuru) selfCall(pctx *pipeline.Context) bool {
	return pctx.Headers != nil && p.sentinel != "" && pctx.Headers.Get(sentinelHeader) == p.sentinel
}

// Init starts the keep-alive sweep. Nothing to start when keep_alive is off.
func (p *ContextGuru) Init(context.Context) error {
	if p.keeper != nil {
		p.keeper.Start()
	}
	return nil
}

// Shutdown stops the sweep and releases every held credential before returning: on a hot-reload
// this instance is the old one, and its holds must not outlive it.
func (p *ContextGuru) Shutdown(context.Context) error {
	p.stopped.Store(true)
	if p.keeper != nil {
		p.keeper.Shutdown()
	}
	if p.disp != nil {
		p.disp.Close()
	}
	return nil
}

// kaArrived marks, on pctx, that THIS request's OnRequest opened a keep-alive arrival — set only
// when the Arrive gate there passed (see OnRequest). Record itself departs on every path, via its
// own deferred Keeper.Depart; what it does not cover is a path that never reaches Record at all.
// OnFinish checks this marker in a deferred call and, when set and Record was not reached, calls
// Depart with the same key Arrive used — otherwise that arrival stays open on its thread's key
// forever (until Forget or a restart), which the keeper's sweep reads as a request still in
// flight and never pings again.
type kaArrived struct{}

// kaArrivedStateKey is this plugin's own key into pctx.Extensions.Custom (see pipeline.SetState) —
// per-request, so it cannot be confused with another request sharing the same session.
const kaArrivedStateKey = "context-guru-keepalive-arrived"

// OnFinish hands the finished request to the keeper. Here and not in OnResponse: inference-parser
// settles token counts after this plugin's response hook, and by OnFinish they are final and
// pctx.Body still holds the compacted bytes that went upstream. A mapping and one Record call —
// no I/O — so it stays far inside the finish timeout and cannot change the outcome.
func (p *ContextGuru) OnFinish(_ context.Context, pctx *pipeline.Context) {
	sid := sessionID(pctx)
	arrived := pipeline.GetState[kaArrived](pctx, kaArrivedStateKey) != nil
	// Depart this request's arrival on every path that does not reach Record — the early returns
	// below, and a panic in recordBilled or recordFor, which the pipeline's dispatchFinish
	// recovers (without this the arrival would stay open until a restart). Not unconditional:
	// Record departs for itself, and a second Depart would release a CONCURRENT request's count
	// on the same thread. `recorded` is set immediately before Record.
	//
	// That covers a request that finishes after Shutdown (one still draining on the old pipeline
	// after a hot-reload), which must hold nothing: no sweep would ever ping it, and Shutdown's
	// sweep over the live entries does not touch the inflight count. It also covers what
	// recordFor skips — a subagent request and one with no inference parse.
	recorded := false
	defer func() {
		if arrived && !recorded {
			p.keeper.Depart(keepalive.Key{Session: sid})
		}
	}()
	if p.stopped.Load() {
		return
	}
	if p.selfCall(pctx) || !p.gated(pctx.Path) {
		return
	}
	if sid != "" {
		p.recordBilled(pctx, sid)
	}
	if p.keeper == nil {
		return
	}
	hdr := pctx.Headers
	scheme, host := pctx.Scheme, pctx.Host
	disp := p.disp
	r, ok := recordFor(pctx, sid, p.kaPol, func() dispatch.Ref { return disp.hold(scheme, host, hdr) },
		p.pingEmitter(pctx, sid))
	if !ok {
		return
	}
	recorded = true
	p.keeper.Record(r)
}

func isSubagent(pctx *pipeline.Context) bool {
	inf := pctx.Extensions.Inference
	return inf != nil && inf.AgentRole == pipeline.AgentRoleSubagent
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
	m.pctx = pctx
	m.dryRun = pctx.Observing()
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
			HTTPClient:         modelHTTPClient(p.modelTO),
		}),
		maxTokens:   p.modelMax,
		model:       model,
		host:        pctx.Host,
		pctx:        pctx,
		dryRun:      pctx.Observing(),
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
		{Name: "context_window", Type: "object", Description: "Where model context windows come from, for fraction-of-window triggers (cache_aware_summarizer's 0.9): model_info (litellm | off, default litellm), model_info_url, windows (model id -> tokens, exact)."},
		{Name: "keep_alive", Type: "object", Description: "Opt-in prompt-cache keep-alive: enabled, idle_seconds (270), max_pings (2), max_usd_per_ping (0.50), min_prefix_tokens (80000). Pings bill the caller's own credential; each is accounted through the accountant host (its spend reaches the cost ledger as Source keep-alive) where accounting is on, and otherwise only logged."},
	}
}

var (
	_ pipeline.Plugin               = (*ContextGuru)(nil)
	_ pipeline.Configurable         = (*ContextGuru)(nil)
	_ pipeline.SchemaProvider       = (*ContextGuru)(nil)
	_ pricing.ResolverConsumer      = (*ContextGuru)(nil)
	_ plugins.AccountingConsumer    = (*ContextGuru)(nil)
	_ pipeline.Initializer          = (*ContextGuru)(nil)
	_ pipeline.Shutdowner           = (*ContextGuru)(nil)
	_ pipeline.Finisher             = (*ContextGuru)(nil)
	_ cgcomponents.KeepAliveEmitter = logEmitter{}
	_ cgcomponents.KeepAliveEmitter = pingEmitter{}
)

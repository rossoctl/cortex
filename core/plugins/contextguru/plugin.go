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
// a `cheap_model:` block to enable a static cheap model, and the plugin also
// reconstructs the agent's own "incoming" model from the live request so a
// component with model.source: incoming can reuse it. With no model available
// those components degrade to deterministic/no-op.
package contextguru

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
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
	"github.com/rossoctl/context-guru/models"
	cgstore "github.com/rossoctl/context-guru/store"
	yaml "gopkg.in/yaml.v3"
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
	// MaxConcurrent bounds concurrent calls to this static model, process-wide, across every
	// request. 0 → defaultModelMaxConcurrent (4) -- the same default as context-guru-proxy's
	// --cheap-model-concurrent flag, which this mirrors (cortex has no equivalent today
	// otherwise: an unbounded number of concurrent summarize/extract calls could fire).
	MaxConcurrent int `json:"max_concurrent"`
}

// defaultModelMaxConcurrent mirrors context-guru-proxy's --cheap-model-concurrent default.
const defaultModelMaxConcurrent = 4

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
	// CheapModel is the optional static cheap model for LLM-backed components. Named
	// cheap_model, not model: context-guru-proxy's own name for the same concept is
	// CHEAP_MODEL*, and the "cheap" is load-bearing -- it is never the agent's own model
	// (that one is model.source: incoming, reconstructed from the live request, not configured
	// here at all).
	CheapModel *modelConfig    `json:"cheap_model"`
	Engine     json.RawMessage `json:"engine"`
	// ContextWindow says where model windows come from, for the engine's fraction triggers.
	ContextWindow windowsConfig `json:"context_window"`
	// CacheMode overrides whether the engine places prompt-cache breakpoints on the outbound
	// request at all: "auto" (default), "on" or "off". It is apply.Opts.CacheMode, threaded
	// through unchanged -- the SAME field and the SAME three values as context-guru-proxy's
	// CACHE_MODE environment variable, so a value means the same thing in both. "" behaves as
	// "auto", exactly as an unset CacheMode always has. Validated at Configure.
	CacheMode string `json:"cache_mode"`
}

// inferencePaths are the request paths this plugin acts on. Hardcoded, not configurable:
// nothing in this repo's tests, docs or deploy manifests has ever overridden it, and
// context-guru-proxy has no equivalent knob either -- it hardcodes its own routes
// (/openai, /anthropic, /healthz, /stats, /expand) the same way, because the whole point of
// that binary is to always run the engine on them. See gated.
var inferencePaths = []string{"/v1/chat/completions", "/v1/completions", "/v1/messages"}

// validCacheModes are apply.Opts.CacheMode's only legal values ("" counts as "auto" and is not
// listed here, since Configure treats an absent key as unset, not as the literal string).
var validCacheModes = map[string]bool{"auto": true, "on": true, "off": true}

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
	// sem bounds concurrent calls to this model, process-wide. A channel, not a pointer: every
	// per-request copy of cgModel (staticModelFor) shares the one built in Configure. nil only on
	// a zero cgModel{} (tests that build one directly without going through Configure); Complete
	// treats a nil sem as unbounded.
	sem chan struct{}
	// onAcquire, for tests only (nil otherwise): called the instant this call's semaphore slot
	// is taken, before the network call. A broken cap lets every call acquire with no blocking
	// at all, in-process -- so a test observing concurrency here, rather than at the HTTP
	// server on the other end of a real round trip, sees the true count with no network-timing
	// race to hide a bound that no longer holds.
	onAcquire func()
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
	if m.sem != nil {
		select {
		case m.sem <- struct{}{}:
			defer func() { <-m.sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if m.onAcquire != nil {
		m.onAcquire()
	}
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
	cfg   contextGuruConfig
	pipe  *cgcomponents.Pipeline
	store cgstore.Store
	// stateDB is the session-state database Configure opened (stateDBFor), nil when the state
	// is in memory only; statePath is its file. The handle is shared and never closed.
	stateDB   *sql.DB
	statePath string
	// stateDisk is the store's disk copy on stateDB, started in Init (attachStateDB) and closed,
	// which flushes its pending writes, in Shutdown. Nil until Init, and when there is no copy.
	stateDisk atomic.Pointer[cgstore.Disk]
	sentinel  string // unguessable value carried by our own LLM calls
	static    *cgModel
	modelTO   time.Duration
	modelMax  int
	// localInstall is plugins.Deps.LocalInstall (SetLocalInstall), set before Configure. It
	// decides where the session state is kept (attachStateDB) and whether the model calls bypass
	// HTTPS_PROXY (localModelTransport).
	localInstall bool
	rates        pricing.Resolver
	// acct is the process's accountant host, nil when accounting is off. Keep-alive pings are
	// accounted through it (ping.go).
	acct *accounting.Host

	// windows resolves a model's context window; see windows.go.
	windows models.Resolver
	// cgSessions maps a Cortex session id to the session key the engine derived for it
	// (apply.Trace.Session), so OnFinish records billed input under the key OnRequest reads.
	// One entry per session, overwritten on each request, bounded by sessionKeysCap.
	cgSessions sessionKeys

	// Keep-alive, all nil/zero when keep_alive.enabled is false. The keeper decides when to ping; disp
	// only sends and holds the credential.
	keeper *keepalive.Keeper
	disp   *cortexDispatcher
	kaPol  keepalive.Policy
	// stopped is set at the start of Shutdown. After a hot-reload the old pipeline is shut down
	// once its drain window ends, and a long streaming request still on it reaches OnFinish
	// later. keepalive.Keeper.Record has no stopped check of its own, so without this flag that
	// request would hold its credential again on a keeper whose sweep is gone.
	stopped atomic.Bool
	// stopMu closes the window between OnFinish's stopped check and its Record call: OnFinish holds
	// it for reading across both, and Shutdown sets stopped under the write lock. So once Shutdown
	// has set stopped, no OnFinish that passed the check is still on its way to Record, and the
	// keeper's Shutdown sweep releases every hold there will be.
	stopMu sync.RWMutex
	// lateModelCalls counts model calls whose spend op was dropped because the call returned
	// after its request was accounted (cgModel). Reported by Metrics.
	lateModelCalls atomic.Int64
}

// Metrics implements pipeline.MetricsProvider.
func (p *ContextGuru) Metrics() []pipeline.Metric {
	var diskErrors int64
	if d := p.stateDisk.Load(); d != nil {
		diskErrors = d.Errors()
	}
	return []pipeline.Metric{{
		Name:  "late_model_calls_dropped",
		Value: float64(p.lateModelCalls.Load()),
		Unit:  "count",
		Note:  "model calls that returned after their request was accounted; their spend reaches no figure",
	}, {
		Name:  "state_db_errors",
		Value: float64(diskErrors),
		Unit:  "count",
		Note:  "session-state database operations that failed since start; the state they carried stayed in memory only",
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

// SetLocalInstall receives whether this is a laptop install (plugins.Deps, before Configure). On a
// local install the plugin keeps its session state at ~/.cortex/context-guru-state.db, and in the
// user cache directory otherwise (attachStateDB); its model calls and keep-alive pings bypass
// HTTPS_PROXY (see localModelTransport), which otherwise points at Cortex itself; and
// context_window.model_info defaults to the LiteLLM map rather than off (buildWindows).
func (p *ContextGuru) SetLocalInstall(local bool) { p.localInstall = local }

func (p *ContextGuru) Configure(raw json.RawMessage) error {
	var decoded contextGuruConfig
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return fmt.Errorf("context-guru config: %w", err)
		}
		warnOnRetiredTopLevelKeys(raw)
	}
	if decoded.CacheMode != "" && !validCacheModes[decoded.CacheMode] {
		return fmt.Errorf("context-guru config: cache_mode %q is not one of: auto, on, off", decoded.CacheMode)
	}
	p.cfg = decoded

	// Engine config: default to the balanced preset when none is supplied. injectDefaultCacheStrategy
	// then fills `cache: {strategy: auto}` ONLY when the operator's own engine: YAML has no cache:
	// key at all -- keep-alive stays on by default on every install, same as before keep_alive:
	// was retired, but through the engine's own strategy table rather than a second copy of it.
	//
	// engine: with no value, or every child commented out, decodes to the JSON literal "null"
	// (len 4), not an empty json.RawMessage -- treated the same as an absent engine: key here,
	// so the two cannot silently diverge (review finding: without this, a null engine turned
	// keep-alive off while an absent one turned it on, the one difference between "I wrote
	// engine: with nothing under it" and "I never wrote engine: at all" an operator could not
	// see and would not expect).
	engine := p.cfg.Engine
	if len(bytes.TrimSpace(engine)) == 0 || bytes.Equal(bytes.TrimSpace(engine), []byte("null")) {
		engine = []byte("preset: balanced")
	}
	engine = injectDefaultCacheStrategy(engine)
	cfg, err := cgconfig.LoadBytes(engine)
	if err != nil {
		return fmt.Errorf("context-guru engine config: %w", err)
	}
	pipe, err := cfg.Build(logEmitter{}) // log what the engine does per request/component
	if err != nil {
		return fmt.Errorf("context-guru build pipeline: %w", err)
	}
	p.pipe = pipe
	// The database is opened here, before the store is built, so the state keys live a day in
	// memory only when a database backs them, as in context-guru's own proxy: a session that
	// returns later is loaded from the database. An explicit store.state_ttl_seconds wins.
	// Opening starts nothing; Init attaches the disk copy.
	p.stateDB, p.statePath = nil, ""
	if cfg.Store.Enabled == nil || *cfg.Store.Enabled {
		p.stateDB, p.statePath = stateDBFor(p.localInstall)
		if p.stateDB != nil && cfg.Store.StateTTLSeconds == 0 {
			cfg.Store.StateTTLSeconds = int(stateTTLDefault / time.Second)
		}
	}
	p.store = cfg.NewStore()
	p.windows = buildWindows(p.cfg.ContextWindow, p.localInstall)
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
	// p.kaPol is stored regardless of pol.KeepAlive: cache.strategy's other effects at the
	// request's own cache breakpoints (HeadTTL1h/HeadTTLMinBytes, OnRequest's apply.Opts) are
	// independent of whether the keep-alive keeper itself is built, and a strategy such as
	// 1-hour-head, which asks for the one but not the other, used to leave p.kaPol at its zero
	// value — resolving a policy nothing read (review finding).
	pol := cachePolicy(cfg.Cache)
	p.kaPol = pol
	if pol.KeepAlive {
		p.disp = newCortexDispatcher(newPingClient(p.localInstall))
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
	if m := p.cfg.CheapModel; m != nil {
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
		case m.MaxConcurrent < 0:
			return fmt.Errorf("context-guru model config: max_concurrent must not be negative (got %d)", m.MaxConcurrent)
		default:
			maxConcurrent := m.MaxConcurrent
			if maxConcurrent == 0 {
				maxConcurrent = defaultModelMaxConcurrent
			}
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
				sem:       make(chan struct{}, maxConcurrent),
			}
		}
	}
	slog.Info("context-guru configured",
		"paths", inferencePaths, "modelConfigured", p.static != nil,
		"engineBytes", len(engine), "stateDB", p.statePath)
	return nil
}

// warnOnRetiredTopLevelKeys logs one WARN per retired top-level key raw still carries -- naming
// it and where its settings live now -- the same pattern context-guru's own
// decodeWithModelHint uses for a retired key (drop it, never refuse the document, always say
// so). json.Unmarshal into contextGuruConfig silently ignores an unknown field by default, which
// would otherwise let `keep_alive: {enabled: true}`, `model: {...}` or `paths: [...]` look
// respected when none of them does anything any more (review finding for the latter two: hardcoded
// paths and cheap_model's rename from model were both silent).
func warnOnRetiredTopLevelKeys(raw json.RawMessage) {
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil {
		return
	}
	if _, present := keys["keep_alive"]; present {
		slog.Warn("context-guru config: keep_alive is retired and is not applied; " +
			"set engine.cache.strategy (or cache.keepalive_* directly) instead -- see " +
			"docs/context-guru-plugin.md")
	}
	if _, present := keys["model"]; present {
		slog.Warn("context-guru config: model is retired and is not applied; " +
			"set cheap_model instead -- see docs/context-guru-plugin.md")
	}
	if _, present := keys["paths"]; present {
		slog.Warn("context-guru config: paths is retired and is not applied; " +
			"the paths this plugin acts on are hardcoded -- see docs/context-guru-plugin.md")
	}
}

// injectDefaultCacheStrategy adds `cache: {strategy: auto}` to engine's own config, but only
// when the operator wrote no top-level cache: key there at all. This is what keeps keep-alive on
// by default on every install now that keep_alive: (which used to carry that default) is
// retired. It never touches engine bytes that already name a cache: key, however they wrote it
// -- an operator who names a strategy themselves, or hand-tunes cache.keepalive_* directly with
// no strategy named, is left alone either way; only "no cache: key at all" gets this default.
//
// RE-MARSHALS THE WHOLE DOCUMENT rather than appending text, because p.cfg.Engine decodes from
// JSON (Configure's own json.RawMessage field), and `{"preset":"xhigh"}\ncache:\n  strategy:
// auto\n` is not one YAML document: a flow mapping that is the whole first line cannot be
// followed by more top-level block-mapping keys. yaml.Unmarshal into doc below still succeeds on
// that combined text (it reads the first document and stops), which is exactly why this went
// unnoticed -- LoadBytes got no error, just a cache.strategy of "" (review finding: keep-alive
// off for every config that sets engine at all, including the plain preset case).
func injectDefaultCacheStrategy(engine []byte) []byte {
	var doc map[string]yaml.Node
	if err := yaml.Unmarshal(engine, &doc); err != nil {
		return engine // malformed YAML: let cgconfig.LoadBytes produce the real error
	}
	if doc == nil {
		// An engine: key with every child commented out reaches here as the JSON/YAML text
		// "null" (an engine: with no value decodes the same way): yaml.Unmarshal leaves doc nil
		// with no error, and doc["cache"] = cache below would panic -- assignment to entry in
		// nil map (review finding, reproduced). Treat it as an absent engine: cgconfig.LoadBytes
		// already gives a config with no error for "null", so this stays a no-op rather than
		// also setting cache.strategy on an engine that names nothing else.
		return engine
	}
	if _, present := doc["cache"]; present {
		return engine
	}
	var cache yaml.Node
	if err := cache.Encode(map[string]string{"strategy": cgconfig.DefaultStrategy}); err != nil {
		return engine // cgconfig.DefaultStrategy is a constant string; this does not fail in practice
	}
	doc["cache"] = cache
	out, err := yaml.Marshal(doc)
	if err != nil {
		return engine // same: a map of yaml.Node values this package built itself
	}
	return out
}

// Init attaches the store's disk copy and starts its flusher. It runs in Init and not in
// Configure so the flusher is under the pipeline's Start/unwind/Stop contract: a pipeline that a
// failed build or reload drops is never started, and so holds no flusher. Init also starts the
// keep-alive sweep, when the resolved cache policy has it on. It FAILS OPEN and never returns an error.
func (p *ContextGuru) Init(context.Context) error {
	if old := p.stateDisk.Swap(attachStateDB(p.store, p.stateDB, p.statePath)); old != nil {
		_ = old.Close()
	}
	if p.keeper != nil {
		p.keeper.Start()
	}
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
	// A real request on a thread ends that thread's idle span: the keeper drops the entry (and the
	// credential it held) before this request goes upstream, so no ping races it. Only THIS
	// request's thread (keeperKey): a subagent's request says nothing about whether the main
	// thread is idle, and the reverse.
	//
	// keeperKey reads the agent id from headers, so it gives OnFinish the same key. Its one
	// fallback reads Extensions.Inference — nil when, as here, this plugin runs before
	// inference-parser's own OnRequest — so a subagent that names no agent can still open an
	// arrival on the main thread here and be skipped by recordFor. The kaArrived marker holds the
	// key THIS call arrived on, so OnFinish departs exactly the arrivals this call made — never
	// fewer, never (on a thread a concurrent request shares) one too many.
	//
	// UNDER on_error: observe NOTHING IS HELD: a held turn is pinged upstream with the caller's own
	// credential, and that ping is real, billed spend — the opposite of a shadow run, where
	// nothing the plugin does takes effect. The marker tells OnFinish, which
	// runs with no policy of its own, to record nothing for this request.
	if p.keeper != nil && pctx.Observing() {
		pipeline.SetState(pctx, kaShadowStateKey, &kaShadow{})
	} else if p.keeper != nil && !p.stopped.Load() && sid != "" {
		if key, ok := keeperKey(pctx, sid); ok {
			p.keeper.Arrive(key)
			pipeline.SetState(pctx, kaArrivedStateKey, &kaArrived{key: key})
		}
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
	models := p.modelsFor(pctx, provider)
	before := len(pctx.Body)
	window, exact := p.windowFor(ctx, pctx)
	// HeadTTL1h/HeadTTLMinBytes are cache.strategy: 1-hour-head's whole effect (cachePolicy):
	// without them here, that strategy resolved a policy nothing read, and OnRequest asked for
	// the plain 5-minute tier regardless (review finding).
	res := apply.BodyOpts(ctx, p.pipe, p.store, apply.Opts{
		Provider:        provider,
		Body:            pctx.Body,
		Session:         sid,
		Models:          models,
		Window:          window,
		WindowExact:     exact,
		CacheMode:       p.cfg.CacheMode,
		HeadTTL1h:       p.kaPol.HeadTTL1h,
		HeadTTLMinBytes: p.kaPol.HeadTTLMinBytes,
		OneHourFirst:    p.oneHourFirstFor(pctx, provider, pctx.Host),
	})
	if res.Trace.Session != "" {
		p.cgSessions.set(sid, res.Trace.Session)
	}
	out, changed, ops := reportResult(pctx, res)
	if changed && len(out) > 0 {
		// SetBody decides whether this write actually applies (true unless a non-observe
		// writer ahead of this plugin already enforced its own bytes — pipeline/plugin-reference.md's
		// shadow-write case). Ops are stamped and reported AFTER this call, never before: reporting
		// first would price a shadow write as a real saving, which can fail the library's
		// client-vs-forwarded sum check and zero the whole request's figure, tool-prune's savings on
		// it included. ReportOps's own DryRun stamp (on_error:
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
	// res.Changed itself misses a head-TTL relabel (cache.strategy: 1-hour-head, or a
	// 1-hour-always/-on-wait OneHourFirst relabel) on a request that has a messages array:
	// apply.BodyOpts folds the relabel into Changed on its two early returns (no messages, or no
	// restore plan to splice), but the full-pipeline path at the bottom overwrites Changed from
	// the message rewrite alone, dropping the tools/system edit res.Body already carries (review
	// finding, reproduced — Trace.HeadTTL1h true, Changed false, with the relabelled bytes
	// sitting unread in res.Body). Trace.HeadTTL1h is authoritative here regardless of which path
	// set Changed, so ORing it in recovers exactly the one case this library gap drops — guarded
	// by a byte comparison, because a body already at ttl "1h" sets Trace.HeadTTL1h too (apply's
	// own doc: "count it as head all the same") with nothing actually rewritten (review
	// suggestion), and reporting that as a change would call SetBody on identical bytes.
	out, changed = res.Body, res.Changed || (res.Trace.HeadTTL1h && !bytes.Equal(res.Body, pctx.Body))
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

// selfCall reports whether pctx is one of the plugin's own calls (an engine model call or a
// keep-alive ping), by equality with the per-process sentinel: a caller setting the header to
// any guessed value skips nothing.
func (p *ContextGuru) selfCall(pctx *pipeline.Context) bool {
	return pctx.Headers != nil && p.sentinel != "" && pctx.Headers.Get(sentinelHeader) == p.sentinel
}

// Shutdown stops the sweep and releases every held credential before returning: on a hot-reload
// this instance is the old one, and its holds must not outlive it.
func (p *ContextGuru) Shutdown(context.Context) error {
	p.stopMu.Lock()
	p.stopped.Store(true)
	p.stopMu.Unlock()
	if p.keeper != nil {
		p.keeper.Shutdown()
	}
	if p.disp != nil {
		p.disp.Close()
	}
	if d := p.stateDisk.Swap(nil); d != nil {
		_ = d.Close()
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
type kaArrived struct{ key keepalive.Key }

// kaArrivedStateKey is this plugin's own key into pctx.Extensions.Custom (see pipeline.SetState) —
// per-request, so it cannot be confused with another request sharing the same session.
const kaArrivedStateKey = "context-guru-keepalive-arrived"

// kaShadow marks a request this plugin saw under on_error: observe: the keeper holds nothing
// for it, so no keep-alive ping is ever sent on the caller's credential in shadow mode.
type kaShadow struct{}

// kaShadowStateKey is this plugin's key for kaShadow.
const kaShadowStateKey = "context-guru-keepalive-shadow"

// OnFinish hands the finished request to the keeper. Here and not in OnResponse: inference-parser
// settles token counts after this plugin's response hook, and by OnFinish they are final and
// pctx.Body still holds the compacted bytes that went upstream. A mapping and one Record call —
// no I/O — so it stays far inside the finish timeout and cannot change the outcome.
func (p *ContextGuru) OnFinish(_ context.Context, pctx *pipeline.Context) {
	sid := sessionID(pctx)
	arrived := pipeline.GetState[kaArrived](pctx, kaArrivedStateKey)
	// Depart this request's arrival on every path that does not reach Record — the early returns
	// below, and a panic in recordBilled or recordFor, which the pipeline's
	// dispatchFinish recovers (without this the arrival would stay open until a restart). Not unconditional:
	// Record departs for itself, and a second Depart would release a CONCURRENT request's count
	// on the same thread. `recorded` is set immediately before Record.
	//
	// That covers a request that finishes after Shutdown (one still draining on the old pipeline
	// after a hot-reload), which must hold nothing: no sweep would ever ping it, and Shutdown's
	// sweep over the live entries does not touch the inflight count. It also covers what
	// recordFor skips — a subagent request that names no agent, and one with no inference parse.
	recorded := false
	defer func() {
		if arrived != nil && !recorded {
			p.keeper.Depart(arrived.key)
		}
	}()
	p.stopMu.RLock()
	defer p.stopMu.RUnlock()
	if p.stopped.Load() {
		return
	}
	if p.selfCall(pctx) || !p.gated(pctx.Path) {
		return
	}
	if sid != "" {
		p.recordBilled(pctx, sid)
	}
	if p.keeper == nil || pipeline.GetState[kaShadow](pctx, kaShadowStateKey) != nil {
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
	for _, s := range inferencePaths {
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

// oneHourFirstFor is apply.Opts.OneHourFirst, the 1-hour-always/-on-wait strategies' own effect
// (p.kaPol.OneHourAlways/OneHourOnWait; 1-hour-head is HeadTTL1h above, a different strategy).
// nil when there is no keeper: apply.go's own doc says nil never relabels, the same contract
// context-guru-proxy's keepalive.Handler.oneHourFirstFor relies on.
//
// keepalive.Keeper.OneHourFirst takes a tenant id this single-process plugin has no equivalent
// of (one process, one deployment, not the proxy's multi-tenant registry), passed "". route is
// pctx.Path, the same value recordFor gives keepalive.Record.Route (keepalive.go), so
// oneHourRoute keys this call's st.off/st.costly lookups the same way the keeper's own
// bookkeeping does.
//
// threadID is captured from pctx here, at the same call that reads it for keeperKey
// (keepalive.go), rather than taken from the library's own apply.Opts.ThreadOf/res.Trace.Thread,
// which this plugin does not wire (review finding: that left threadID always "", so a named
// subagent's request was asked -- and relabelled -- as if it were the main thread). The two
// agree on one convention, agentThreadPrefix + pctx.Subagent(), so this call's Key{Thread:...}
// and the keeper's own Arrive/Depart/Record calls for the same request key identically.
func (p *ContextGuru) oneHourFirstFor(pctx *pipeline.Context, provider bschemas.ModelProvider, endpoint string) func(session, threadID, model string, toolsDeclared bool) bool {
	if p.keeper == nil || (!p.kaPol.OneHourOnWait && !p.kaPol.OneHourAlways) {
		return nil
	}
	mode := keepalive.OneHourMode{Always: p.kaPol.OneHourAlways, RebuildTokens: int64(p.kaPol.OneHourRebuildTokens)}
	threadID := ""
	if agent := pctx.Subagent(); agent != "" {
		threadID = agentThreadPrefix + agent
	}
	route := pctx.Path
	return func(session, _ /* unused: the library never resolves this without Opts.ThreadOf */, model string, toolsDeclared bool) bool {
		return p.keeper.OneHourFirst("", session, threadID, provider, endpoint, route, model, toolsDeclared, mode)
	}
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
// total) — the call itself, and its cost on the caller's own credential, already happened.
// Returning an empty ModelSpec here makes every model.source: incoming or
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
		{Name: "cheap_model", Type: "object", Description: "Optional static cheap model for LLM-backed components (summarize, extract:code); context-guru-proxy's own name for the same concept is CHEAP_MODEL*. Its calls honour HTTPS_PROXY except on a local install, where it points at Cortex itself. max_concurrent bounds concurrent calls to it, process-wide; default 4, the same default as context-guru-proxy's --cheap-model-concurrent flag."},
		{Name: "engine", Type: "object", Description: "Native context-guru config: preset / pipeline / per-component (marker_mode) / store / cache. " +
			"cache.strategy picks a named prompt-cache policy -- none, auto, 1-ping, 2-ping, 1-hour-head, 1-hour-on-wait, 1-hour-always, 30-min-ping -- " +
			"resolved by context-guru itself (config.LookupStrategy), not reimplemented here; see context-guru's docs/how-to/cache-strategies.md. " +
			"Unset: this plugin fills cache.strategy with \"auto\" itself (only when no cache: key is present at all), so keep-alive stays on by default on every install."},
		{Name: "context_window", Type: "object", Description: "Where model context windows come from, for fraction-of-window triggers (cache_aware_summarizer's 0.9): model_info (litellm | off; default litellm on a local install, off elsewhere, and litellm wherever model_info_url is set), model_info_url, windows (model id -> tokens, exact)."},
		{Name: "cache_mode", Type: "string", Description: "Override whether the engine places prompt-cache breakpoints on the outbound request at all: \"auto\" (default), \"on\" or \"off\". The same field and the same three values as context-guru-proxy's CACHE_MODE environment variable."},
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

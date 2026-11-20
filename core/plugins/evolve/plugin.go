// Package evolve connects Cortex to altk-evolve, a service that learns
// guidelines from agent trajectories (https://github.com/AgentToolkit/altk-evolve),
// using only the tools a stock `evolve-mcp --transport sse` server exposes.
//
// The plugin does two things, both optional:
//
//   - Inject. On a session's first main-agent inference request it asks evolve
//     (get_guidelines_with_attribution) for the guidelines relevant to the
//     session's task in the serve namespace, caches them per session, and
//     appends them to the system instructions of every main-agent request in
//     that session as a delimited <evolve-guidelines> block. The block is
//     byte-stable across turns so prompt caching keeps working.
//   - Learn. It records each completed main-agent turn as OpenAI chat messages
//     and, when the session has been idle for idle_after_seconds, sends the new
//     turns to evolve (process_trajectory) as one identified batch. Evolve
//     generates guidelines from them into the learn namespace.
//
// By default the two namespaces are a review gate. Session content includes
// tool output the agent read, so a guideline distilled from it can carry text an
// attacker wrote. Learned guidelines therefore land in learn_namespace, which is
// not injected from; a person copies the ones worth keeping into
// serve_namespace. Setting the two to the same namespace skips the review:
// whatever evolve learns is served to the next session.
//
// Every failure fails open. Evolve unreachable, slow, or returning nonsense
// means the request goes upstream untouched; the guidelines are an
// improvement, never a dependency.
package evolve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

const name = "evolve"

// defaultPrompt is the evolve guideline prompt the plugin learns with unless
// configured otherwise: coding agents work where hooks, checks and error
// messages state the project's rules, which is what environment-grounded learns.
const defaultPrompt = "environment-grounded"

// promptName is evolve's rule for a guideline prompt name.
var promptName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type config struct {
	Endpoint       string   `json:"endpoint" required:"true" description:"SSE URL of the evolve MCP server (evolve-mcp --transport sse), e.g. http://127.0.0.1:8201/sse."`
	ServeNamespace string   `json:"serve_namespace" description:"Evolve namespace guidelines are injected from: the reviewed set, unless it is also learn_namespace." default:"cortex"`
	LearnNamespace string   `json:"learn_namespace" description:"Evolve namespace learned guidelines are written to, for review. Set it to serve_namespace to serve learned guidelines without review." default:"cortex-candidates"`
	Profile        string   `json:"processing_profile" description:"Evolve processing profile the plugin creates and learns with." default:"cortex"`
	Prompt         *string  `json:"guidelines_prompt" description:"Evolve guideline prompt the processing profile learns with. environment-grounded turns a requirement a hook, check or error message states into a step to take first. Empty: the evolve server's own default. An evolve that can't take it learns with its default, with a warning." default:"environment-grounded"`
	Inject         *bool    `json:"inject" description:"Append guidelines to main-agent requests." default:"true"`
	Placement      string   `json:"placement" description:"Where guidelines go: system (after the system instructions) or user (a system-reminder on the first user message)." default:"system" enum:"system,user"`
	Learn          *bool    `json:"learn" description:"Send idle sessions' new turns to evolve to learn from." default:"true"`
	Agents         []string `json:"agents" description:"Client names to act on (claude-code, codex, bob, opencode). Empty = all recognised agents."`
	TopK           int      `json:"top_k" description:"Most guidelines to inject per session." default:"8"`
	TimeoutMs      int      `json:"timeout_ms" description:"Timeout for fetching a session's guidelines. The first request of a session waits up to this long." default:"3000"`
	LearnTimeout   int      `json:"learn_timeout_seconds" description:"Timeout for one learning call; evolve generates guidelines with an LLM before answering." default:"300"`
	CacheTTL       int      `json:"cache_ttl_seconds" description:"How long a session's guidelines are reused before refetching (how soon a removed guideline stops)." default:"1800"`
	RetryAfter     int      `json:"retry_after_seconds" description:"After a failed fetch, how long to inject nothing before trying again." default:"60"`
	IdleAfter      int      `json:"idle_after_seconds" description:"Quiet time after which a session's new turns are sent to evolve." default:"900"`
	MinTurns       int      `json:"min_turns" description:"Fewest new main-agent turns worth learning from." default:"2"`
	MaxCached      int      `json:"max_sessions" description:"Most sessions tracked for caching and learning." default:"1024"`
}

func (c *config) applyDefaults() {
	t := true
	if c.Inject == nil {
		c.Inject = &t
	}
	if c.Learn == nil {
		c.Learn = &t
	}
	defaults := []struct {
		field *int
		value int
	}{
		{&c.TopK, 8}, {&c.TimeoutMs, 3000}, {&c.LearnTimeout, 300}, {&c.CacheTTL, 1800},
		{&c.RetryAfter, 60}, {&c.IdleAfter, 900}, {&c.MinTurns, 2}, {&c.MaxCached, 1024},
	}
	for _, d := range defaults {
		if *d.field == 0 {
			*d.field = d.value
		}
	}
	if c.ServeNamespace == "" {
		c.ServeNamespace = "cortex"
	}
	if c.LearnNamespace == "" {
		c.LearnNamespace = "cortex-candidates"
	}
	if c.Profile == "" {
		c.Profile = "cortex"
	}
	if c.Prompt == nil {
		prompt := defaultPrompt
		c.Prompt = &prompt
	}
	if c.Placement == "" {
		c.Placement = placementSystem
	}
}

func (c *config) validate() error {
	if !strings.HasPrefix(c.Endpoint, "http://") && !strings.HasPrefix(c.Endpoint, "https://") {
		return fmt.Errorf("endpoint must be an http(s) URL, got %q", c.Endpoint)
	}
	if *c.Prompt != "" && !promptName.MatchString(*c.Prompt) {
		return fmt.Errorf("guidelines_prompt must be an evolve prompt name (letters, digits, '-' and '_'), or empty for evolve's default, got %q", *c.Prompt)
	}
	if c.Placement != placementSystem && c.Placement != placementUser {
		return fmt.Errorf("placement must be %q or %q, got %q", placementSystem, placementUser, c.Placement)
	}
	if c.TopK < 1 || c.TopK > 50 {
		return fmt.Errorf("top_k must be between 1 and 50")
	}
	if c.TimeoutMs < 50 {
		return fmt.Errorf("timeout_ms must be at least 50")
	}
	for _, v := range []int{c.LearnTimeout, c.CacheTTL, c.RetryAfter, c.IdleAfter, c.MinTurns, c.MaxCached} {
		if v < 1 {
			return fmt.Errorf("learn_timeout_seconds, cache_ttl_seconds, retry_after_seconds, idle_after_seconds, min_turns and max_sessions must be positive")
		}
	}
	return nil
}

// cached is one session's guidelines. block is pre-rendered so every request
// in the session appends identical bytes.
type cached struct {
	guidelines []Guideline
	block      string
	fetchedAt  time.Time // when evolve last answered for this session
	expires    time.Time
	failed     bool
	// stale marks guidelines kept from an earlier fetch because the refresh
	// failed: still injected, but not what evolve serves now.
	stale bool
}

// flight is a session's guideline fetch in progress. Requests of the session
// that arrive meanwhile wait for its entry instead of calling evolve again.
type flight struct {
	done  chan struct{}
	entry *cached // set before done is closed; nil if the fetch panicked
}

// session is one session's learning state: its unsent turns and when it was
// last active.
type session struct {
	rec     *recorder
	last    time.Time
	sending bool
}

// Plugin is the evolve plugin.
type Plugin struct {
	cfg    config
	raw    json.RawMessage
	client Evolve
	agents map[string]struct{}
	now    func() time.Time
	epoch  int64
	// recorders counts the recorders created, for their batch ids: a session
	// evicted and resumed gets a new recorder, whose batches must not reuse the
	// ids evolve already accepted from the old one.
	recorders int

	mu       sync.Mutex
	cache    map[string]*cached
	order    []string // cache insertion order, for eviction
	inflight map[string]*flight
	sessions map[string]*session

	tick   time.Duration
	cancel context.CancelFunc
	done   chan struct{}

	m metrics
}

func New() *Plugin { return &Plugin{now: time.Now, tick: 30 * time.Second} }

func init() {
	plugins.RegisterPlugin(name, func() pipeline.Plugin { return New() })
}

func (p *Plugin) Name() string { return name }

func (p *Plugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		ReadsBody:         true,
		WritesRequestBody: true,
		RequiresAny:       []string{"inference-parser"},
		Description:       "Injects reviewed altk-evolve guidelines; sends idle sessions to learn from.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider.
func (p *Plugin) ConfigSchema() []pipeline.FieldSchema { return pipeline.SchemaOf(config{}) }

// RawConfig implements pipeline.RawConfigProvider.
func (p *Plugin) RawConfig() json.RawMessage { return p.raw }

func (p *Plugin) Configure(raw json.RawMessage) error {
	var c config
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return fmt.Errorf("evolve config: %w", err)
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return fmt.Errorf("evolve config: %w", err)
	}
	p.cfg = c
	p.raw = raw
	p.agents = make(map[string]struct{}, len(c.Agents))
	for _, a := range c.Agents {
		p.agents[a] = struct{}{}
	}
	p.cache = map[string]*cached{}
	p.inflight = map[string]*flight{}
	p.sessions = map[string]*session{}
	p.epoch = p.now().Unix()
	if p.client == nil {
		p.client = newMCPEvolve(c.Endpoint, c.Profile, *c.Prompt, time.Duration(c.TimeoutMs)*time.Millisecond)
	}
	return nil
}

// Init starts the idle-session sender.
func (p *Plugin) Init(context.Context) error {
	if !*p.cfg.Learn {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.learnLoop(ctx)
	return nil
}

// Shutdown stops the sender. Unsent turns are dropped: learning is
// best-effort, and a restart is not worth delaying for an LLM call.
func (p *Plugin) Shutdown(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
		select {
		case <-p.done:
		case <-ctx.Done():
		}
	}
	if c, ok := p.client.(interface{ Close() }); ok {
		c.Close()
	}
	return nil
}

// eligible reports why a request is out of scope, or "" when it is a
// main-agent inference request of a known session and allowed agent.
func (p *Plugin) eligible(pctx *pipeline.Context) (reason, sessionID, agent string) {
	if pctx.Headers != nil && pctx.Headers.Get(sentinelHeader) != "" {
		return "own_call", "", ""
	}
	inf := pctx.Extensions.Inference
	if inf == nil {
		return "no_inference_extension", "", ""
	}
	if pctx.Subagent() != "" || inf.AgentRole == pipeline.AgentRoleSubagent {
		return "subagent", "", ""
	}
	if len(inf.Tools) == 0 {
		// Claude Code's tool-less side calls (titles, summaries) are not the task.
		return "no_tools", "", ""
	}
	if pctx.Session == nil {
		return "no_session", "", ""
	}
	sessionID = pctx.Session.ID
	if sessionID == "" || sessionID == "default" || strings.HasPrefix(sessionID, "pending:") {
		return "no_session", "", ""
	}
	client := pctx.ClientInfo()
	if client == nil || client.Name == "" {
		return "unknown_agent", "", ""
	}
	if len(p.agents) > 0 {
		if _, ok := p.agents[client.Name]; !ok {
			return "agent_not_selected", "", ""
		}
	}
	return "", sessionID, client.Name
}

// event is published as SessionEvent.Plugins["evolve"]: which guidelines
// shaped this request, so agentop can show it.
type event struct {
	Injected  bool      `json:"injected"`
	IDs       []string  `json:"ids,omitempty"`
	Revisions []*int    `json:"revisions,omitempty"`
	Sources   []string  `json:"sources,omitempty"`
	Cached    bool      `json:"cached"`
	Error     string    `json:"error,omitempty"`
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
}

func (p *Plugin) OnRequest(ctx context.Context, pctx *pipeline.Context) (action pipeline.Action) {
	action = pipeline.Action{Type: pipeline.Continue}
	if !*p.cfg.Inject {
		return action
	}
	defer func() {
		if r := recover(); r != nil {
			p.m.inc(&p.m.recovered)
			slog.Warn("evolve: recovered, forwarding original body", "panic", r)
			action = pipeline.Action{Type: pipeline.Continue}
		}
	}()
	d := dialectOf(pathOnly(pctx.Path))
	if d == dialectNone {
		pctx.Skip("path_not_inference")
		return action
	}
	reason, sessionID, _ := p.eligible(pctx)
	if reason != "" {
		pctx.Skip(reason)
		return action
	}
	if len(pctx.Body) == 0 || !gjson.ValidBytes(pctx.Body) {
		pctx.Skip("invalid_body")
		return action
	}

	entry, hit := p.guidelinesFor(ctx, sessionID, pctx.Extensions.Inference.Messages)
	ev := event{Cached: hit, FetchedAt: entry.fetchedAt}
	if entry.stale {
		ev.Error = "refresh_failed"
	}
	if len(entry.guidelines) == 0 {
		reason := "no_guidelines"
		if entry.failed {
			reason = "evolve_unavailable"
			ev.Error = reason
		}
		p.publish(pctx, ev)
		pctx.Skip(reason)
		return action
	}
	out, err := injectBlock(pctx.Body, d, p.cfg.Placement, entry.block)
	if err != nil || !gjson.ValidBytes(out) {
		p.m.inc(&p.m.injectErrors)
		ev.Error = "unsupported_request_shape"
		p.publish(pctx, ev)
		pctx.Skip("unsupported_request_shape")
		return action
	}
	applied := pctx.SetBody(out)
	ev.Injected = applied
	for _, g := range entry.guidelines {
		ev.IDs = append(ev.IDs, g.ID)
		ev.Revisions = append(ev.Revisions, g.Revision)
		ev.Sources = append(ev.Sources, g.Source)
	}
	p.publish(pctx, ev)
	if applied {
		p.m.inc(&p.m.injected)
	} else {
		p.m.inc(&p.m.projected)
	}
	return action
}

// guidelinesFor returns the session's cached guidelines, fetching them when
// absent or expired. One fetch per session at a time: a request that arrives
// while one is in progress waits for it rather than calling evolve again, but
// only until its own timeout_ms or cancellation, and then goes on with the
// session's earlier guidelines, or none.
func (p *Plugin) guidelinesFor(ctx context.Context, sessionID string, messages []pipeline.InferenceMessage) (*cached, bool) {
	p.mu.Lock()
	if c, ok := p.cache[sessionID]; ok && p.now().Before(c.expires) {
		p.mu.Unlock()
		p.m.inc(&p.m.cacheHits)
		return c, true
	}
	previous := p.cache[sessionID]
	if f, ok := p.inflight[sessionID]; ok {
		p.mu.Unlock()
		return p.await(ctx, f, previous)
	}
	f := &flight{done: make(chan struct{})}
	p.inflight[sessionID] = f
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.inflight[sessionID] == f {
			delete(p.inflight, sessionID)
		}
		p.mu.Unlock()
		close(f.done)
	}()
	f.entry = p.fetch(ctx, sessionID, messages, previous)
	return f.entry, false
}

// await waits for another request's fetch of the same session, for as long as
// this request would have waited for its own.
func (p *Plugin) await(ctx context.Context, f *flight, previous *cached) (*cached, bool) {
	timer := time.NewTimer(time.Duration(p.cfg.TimeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-f.done:
		if f.entry != nil {
			p.m.inc(&p.m.cacheHits)
			return f.entry, true
		}
	case <-timer.C:
	case <-ctx.Done():
	}
	if previous != nil && len(previous.guidelines) > 0 {
		return previous, true
	}
	return &cached{failed: true}, false
}

// fetch asks evolve for the session's guidelines and caches the result.
func (p *Plugin) fetch(ctx context.Context, sessionID string, messages []pipeline.InferenceMessage, previous *cached) *cached {

	texts := make([]messageText, len(messages))
	for i, m := range messages {
		texts[i] = messageText{role: m.Role, content: m.Content}
	}
	entry := &cached{fetchedAt: p.now()}
	if task := taskText(texts); task == "" {
		entry.expires = p.now().Add(time.Duration(p.cfg.RetryAfter) * time.Second)
	} else {
		// Not cancelled with the request that started it: other requests of the
		// session may be waiting for the answer. timeout_ms still bounds it.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(p.cfg.TimeoutMs)*time.Millisecond)
		gs, err := p.client.Guidelines(callCtx, p.cfg.ServeNamespace, task, sessionID)
		cancel()
		p.m.inc(&p.m.fetches)
		switch {
		case err != nil:
			p.m.inc(&p.m.fetchErrors)
			// No session id: the failure is evolve's, not the session's, and the session's own
			// event already records the skip. The id comes from a request header, which
			// CodeQL's go/clear-text-logging treats as sensitive.
			slog.Warn("evolve: guideline fetch failed; forwarding requests unchanged", "err", err)
			entry.failed = true
			entry.expires = p.now().Add(time.Duration(p.cfg.RetryAfter) * time.Second)
		default:
			if len(gs) > p.cfg.TopK {
				gs = gs[:p.cfg.TopK]
			}
			entry.guidelines = gs
			entry.expires = p.now().Add(time.Duration(p.cfg.CacheTTL) * time.Second)
			if len(gs) > 0 {
				entry.block = formatBlock(gs)
			}
		}
	}
	// Keep serving what the session already had through a transient failure,
	// rather than dropping guidelines mid-session and breaking the prompt cache.
	// The entry still says the refresh failed, and when evolve last answered: a
	// guideline withdrawn meanwhile is still being served.
	if entry.failed && previous != nil && len(previous.guidelines) > 0 {
		entry.guidelines, entry.block, entry.failed = previous.guidelines, previous.block, false
		entry.fetchedAt, entry.stale = previous.fetchedAt, true
	}
	p.store(sessionID, entry)
	return entry
}

func (p *Plugin) store(sessionID string, entry *cached) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.cache[sessionID]; !ok {
		p.order = append(p.order, sessionID)
	}
	p.cache[sessionID] = entry
	for len(p.order) > p.cfg.MaxCached {
		evict := p.order[0]
		p.order = p.order[1:]
		// A fetch in progress for the evicted session is left alone: the requests
		// waiting for it still get its answer, and later ones join it.
		delete(p.cache, evict)
	}
}

func (p *Plugin) publish(pctx *pipeline.Context, ev event) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[name+pipeline.PluginEventSuffix] = ev
}

func (p *Plugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// OnFinish records the completed turn for learning. It runs after the
// response is on the wire, and only appends to the session's recorder.
func (p *Plugin) OnFinish(_ context.Context, pctx *pipeline.Context) {
	if !*p.cfg.Learn {
		return
	}
	if !answered(pctx) {
		return
	}
	reason, sessionID, agent := p.eligible(pctx)
	if reason != "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	if !ok {
		if len(p.sessions) >= p.cfg.MaxCached && !p.evictOldestSession() {
			// Every tracked session is being sent. Leave this one out rather than
			// go past max_sessions; its turns aren't learned from.
			p.m.inc(&p.m.untracked)
			return
		}
		p.recorders++
		s = &session{rec: newRecorder(agent, fmt.Sprintf("%d.%d", p.epoch, p.recorders))}
		p.sessions[sessionID] = s
	}
	s.rec.observe(pctx.Extensions.Inference)
	s.last = p.now()
}

// answered reports whether the request got a whole response worth learning
// from: a 2xx, or a response the client hung up on only after it had read the
// end of it. Codex closes each stream as soon as it reads response.completed,
// so the listener marks most of its turns client_canceled and resets the
// status to 0, though the response arrived whole; without this most of every
// Codex session would be dropped. A turn the parser never saw finish (no
// finish reason, or a failed one) and a denied request are still skipped.
func answered(pctx *pipeline.Context) bool {
	out := pctx.Outcome()
	if out == nil {
		return false
	}
	if out.StatusCode >= 200 && out.StatusCode < 300 {
		return true
	}
	if out.StatusCode != 0 || out.FinalAction != pipeline.OutcomeError {
		return false
	}
	inf := pctx.Extensions.Inference
	return inf != nil && inf.FinishReason != "" && inf.FinishReason != "failed" &&
		(inf.Completion != "" || len(inf.ToolCalls) > 0)
}

// evictOldestSession stops tracking the least recently active session that
// isn't being sent, and reports whether there was one.
func (p *Plugin) evictOldestSession() bool {
	var oldest string
	var at time.Time
	for id, s := range p.sessions {
		if !s.sending && (oldest == "" || s.last.Before(at)) {
			oldest, at = id, s.last
		}
	}
	if oldest == "" {
		return false
	}
	delete(p.sessions, oldest)
	return true
}

func (p *Plugin) learnLoop(ctx context.Context) {
	defer close(p.done)
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.learnIdle(ctx)
		}
	}
}

// learnIdle sends each idle session's new turns to evolve, and failed batches
// whose retry is due, one session at a time. A failed batch is sent again as it
// was, under the same batch id: evolve skips a batch it already processed, so a
// send that succeeded but reported failure is not learned twice.
func (p *Plugin) learnIdle(ctx context.Context) {
	p.mu.Lock()
	ids := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		p.learnOne(ctx, id)
	}
}

// learnOne sends one session's due batch, if it has one. The batch is taken
// just before it is sent, so a session that resumed while earlier sessions were
// being sent is no longer idle, and waits for a later tick.
func (p *Plugin) learnOne(ctx context.Context, id string) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if !ok || s.sending {
		p.mu.Unlock()
		return
	}
	now := p.now()
	idle := !s.last.After(now.Add(-time.Duration(p.cfg.IdleAfter) * time.Second))
	// A session stays tracked after it is learned from, so a resumed session
	// continues its batch sequence; max_sessions bounds how many are kept.
	t, due := s.rec.next(id, p.cfg.MinTurns, idle, now)
	if !due {
		p.mu.Unlock()
		return
	}
	s.sending = true
	p.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, time.Duration(p.cfg.LearnTimeout)*time.Second)
	err := p.client.Learn(callCtx, p.cfg.LearnNamespace, t)
	cancel()
	p.mu.Lock()
	s.sending = false
	dropped := s.rec.sent(t, err, p.now(), p.tick)
	p.mu.Unlock()
	switch {
	case dropped:
		p.m.inc(&p.m.learnDropped)
		slog.Warn("evolve: giving up sending a session batch to learn from", "session", id,
			"batch", t.Batch.BatchID, "attempts", maxLearnAttempts, "err", err)
	case err != nil:
		p.m.inc(&p.m.learnErrors)
		slog.Warn("evolve: sending session to learn from failed; will retry", "session", id, "err", err)
	default:
		p.m.inc(&p.m.learned)
		slog.Info("evolve: sent idle session to learn from", "session", id, "messages", len(t.Messages), "batch", t.Batch.BatchID)
	}
}

// Metrics implements pipeline.MetricsProvider.
func (p *Plugin) Metrics() []pipeline.Metric { return p.m.snapshot() }

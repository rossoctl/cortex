// Package inferencerouter is the inference-router plugin: it sends a coding agent's
// new sessions to the inference server chosen for that agent, and keeps every
// session on the server it started on.
//
// The agent needs no setting of its own for that beyond going through the proxy. A
// request addressed to a server's host is routed, and so is a routed agent's
// inference request addressed anywhere else — its provider, Anthropic, OpenCode Zen,
// Bob's gateway — which is captured: sent to the agent's server at the server's path
// for its API format (see capturedPaths), by how the path the agent named ends.
// Only a POST inference-parser read as inference is captured, since an agent's own
// tools can POST to an API whose path happens to end the same way.
//
// The choice is plugin config — servers by name, and agents by name to a server —
// changed by editing it and letting the proxy hot-reload. Sessions are pinned in the
// process's durable store (storage.StoreConsumer) — on a local install a file under
// ~/.cortex — which neither a reload nor a restart loses, so changing an agent's
// server moves only the sessions it has not started yet. A process with no durable
// store pins in its process-scoped store (pctx.Shared), which a reload keeps and a
// restart empties.
//
// Place it last in the outbound chain, which is where agentop puts it. A redirect
// moves pctx.Host, so the plugins before it decide on the host the client asked for
// and any plugin after it would see the server's host instead. Nothing should follow
// it that keys on the host.
//
// A routed request is always redirected to its server's own scheme and host, even
// when it already names that host. pctx.Host is the request's Host header, the
// client's word, and on a TLS-bridged request the forward proxy dials the CONNECT
// authority, which need not be that host. A router that skipped the redirect because
// the Host header already named the server would hand the server's key to whatever
// the client CONNECTed to. With the redirect the listener dials RedirectTarget, the
// server, whatever the Host header or the CONNECT said.
//
// The server's key replaces the client's only when pctx.Redirected() reports that the
// redirect took effect. Under on_error: observe, Redirect returns nil and moves
// nothing, so a key set on the strength of that nil would go to the host the client
// named.
//
// A session is pinned by where the request that decides it went, not by the choice
// made for it: to the server when the request was redirected there; to "not routed"
// when it stayed where the client sent it, because the agent is not routed or
// because the router runs under on_error: observe; and not at all when the redirect
// failed or the request was refused for its model, so the session's next request
// decides again. A pin by choice would hold a session started under observe to a
// server it never used, and turning enforce on would then move it there
// mid-conversation, which is the switch the pin exists to prevent.
//
// The first request the router sees is not always a session's first. A session
// already running when routing is first configured, quiet while it was, has no pin,
// and neither has one whose pin lapsed. The session's history tells such a session
// from a new one: on a pin miss for a routed agent, the latest earlier inference
// request, not a side request, that agent sent in the session decides. One to a
// server's host keeps the session on that server. One the router would capture,
// recorded where the agent sent it, was made while the router was not routing the
// agent — before it was routed, or under observe — so the session ran unrouted and
// is not routed now: switching an agent onto a server must not move a conversation
// already running on its provider. Any other request is no evidence. Only a session
// with no evidence is new and goes to its agent's current server. Only a non-empty
// history is evidence: a view is empty for a session the store has recorded nothing
// of yet.
//
// With no pin and no history, a Claude Code inference request says itself whether
// the router is seeing its conversation begin. Its system prompt states the caller,
// and the main conversation carries its tool manifest: an opening turn has no
// assistant message yet and goes to the agent's current server, and a continuation
// has some, so the router did not see it begin — it started before routing was set
// up, or lost its pin to a restart of a process that keeps pins only in memory — and
// it is not routed and pinned so, rather than taken for new and moved
// mid-conversation. The one-shots Claude Code interleaves with a conversation carry
// no tools, and a subagent's requests are its own conversation's; neither says
// whether the session began, so neither decides: each leaves the session unpinned.
// A one-shot still goes to the agent's current server. It is no part of the
// conversation, so answering it there moves nothing, and a new session's first
// request can be one — Claude Code's title request reaches the router before the
// opening turn — which, left where Claude Code sent it, put a new session's title on
// the provider the agent was routed away from. A subagent's request is a
// conversation of its own, which may be running already — a /clear moves background
// subagents onto the new session id — so it stays where Claude Code sent it unless
// the router captured it. Only Claude Code is read this way, because no
// other agent states its role, and one that switches providers, as OpenCode does,
// sends earlier turns that went elsewhere. A session routed to a server keeps it by
// its pin and its history, so a restart that keeps neither sends such a conversation
// back to where Claude Code sends it. So does a move to a new session id — Claude
// Code's continued-in hand-off, or a background job forked from a conversation —
// which neither follows.
//
// A pin is its agent's. The session id is the listener's answer, and another
// agent's request can be filed under it — by process attribution, which files a
// command an agent runs under that agent's session and is on by default on a
// laptop; by the ActiveSession fallback with client affinity off; or by a header id
// two clients share. Such a request is decided as if the session were unpinned, from
// its own agent's history and choice, and leaves the pin alone. Honouring the pin for
// it would hand it the session's server and key.
//
// The listener's synthetic sessions are never pinned: the default bucket, and the
// pending:<agent> buckets an agent's calls collect in before its session is known.
// Each holds many conversations rather than one, so a pin would hold every later
// conversation filed there to the first one's server. Their requests follow the
// agent's current server, unpinned, as a request with no session does.
//
// The model is the agent's to choose and the server's to answer, and the router does
// not read a model list to judge it: a server serves aliases it does not list. A
// routed request goes with the model the agent asked for. When the server refuses
// that model, the refusal is recorded for
// an hour and the request is sent again, once, before the agent sees anything, with
// the server's substitute: its main model for a request with tools, its helper for one
// without, each the newest model on the server's list whose name contains the word
// configured for it (routerconfig.Resolve). While the refusal is recorded, requests
// for that model are sent with the substitute from the start. A session keeps the
// substitute it was given, so a conversation never changes model part-way when a
// refusal lapses or a newer model is listed. A request already sent with a
// substitute is never sent again: a refused substitute reaches the agent.
//
// A model the router sends to can have a smaller output limit or context window than
// the one the agent was built for. A refusal that states the limit — Bedrock's,
// Anthropic's and vLLM's wording, behind LiteLLM — is learned for the server and model,
// kept in the durable store for 30 days, and the request is fitted and sent again
// before the agent sees the refusal. Later requests are fitted before they are sent:
// the output field is lowered, never raised, nor to or below the body's
// thinking.budget_tokens; and once an Anthropic messages or OpenAI chat-completions
// request passes 85% of the window its oldest turns after the first user message are
// dropped to under 60%, with a note saying how many. A responses-format request only
// has its output field lowered, as does one with a single turn larger than the window. Only requests the router switched — to another host or another model — are fitted, and each kind of
// refusal is resent at most once per request.
package inferencerouter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/storage"
)

// Name is the plugin's registered name.
const Name = "inference-router"

const (
	// pinPrefix namespaces the plugin's keys in the shared store.
	pinPrefix = Name + "/pin/"

	// pinTTL is how long a pin outlives a session's last request. It slides: every
	// request renews it, so only a session idle this long forgets its server.
	pinTTL = 30 * 24 * time.Hour

	// historyLimit is how many of a session's archived events wentTo reads, newest
	// first, before it stops looking: the archive is read from disk on the request
	// path.
	historyLimit = 2000

	// codeUnavailable maps to 503 in pipeline's code table. A session whose server
	// cannot be reached through the router is the server being unavailable to it,
	// not the client's mistake.
	codeUnavailable = "upstream.unreachable"
)

// The pin detail on every record that resolved a server.
const (
	pinNew      = "new"      // this request pinned the session
	pinExisting = "existing" // the session was already pinned
	pinNone     = "none"     // nothing was pinned: no session, a synthetic one, no store, a failed redirect, another agent's pin, a side request, or an unparsed request
	pinMoved    = "moved"    // a sessions: override moved the session with this request
)

// The turn detail on a not-routed record, where the request's own turn decided an
// unpinned Claude Code session.
const (
	turnContinuation = "continuation" // a conversation the router did not see begin
	turnAside        = "aside"        // a one-shot or a subagent's request, which decides nothing
)

// route is one configured server, ready to redirect to.
type route struct {
	endpoint routerconfig.Endpoint
	key      string
	// main and helper are the words naming the server's substitute models, resolved
	// against its model list when one is needed (see substitute).
	main, helper string
}

// target is the server's own scheme and host, where every request routed to it is
// redirected.
func (r route) target() *url.URL {
	return &url.URL{Scheme: r.endpoint.Scheme, Host: r.endpoint.Host}
}

// Router is the plugin. Built by Configure; the zero value routes nothing.
type Router struct {
	servers map[string]route  // by server name
	byHost  map[string]string // every server's Endpoint.Key, to its name
	agents  map[string]string // agent name to server name
	// sessions is the config's sessions: map, a session id to the server the user
	// moved it to ("" to stop routing it). It beats the pin and every unpinned rule.
	sessions map[string]string

	// store is the process's durable store (storage.StoreConsumer), where pins are
	// kept when there is one; nil keeps them in pctx.Shared.
	store storage.Store
	// history is the session archive (session.HistoryConsumer), nil in a process with
	// none.
	history session.History
	// now is the clock a stored pin's renewal is judged by.
	now func() time.Time

	// catalogs is each server's model list, as last fetched; client fetches them, the
	// models client when nil, and stopCatalogs ends the fetching Init started.
	catalogs     catalogs
	client       *http.Client
	stopCatalogs context.CancelFunc

	// noStore makes the "pins are off" warning once per instance rather than once
	// per request.
	noStore sync.Once
	// reasoningMu serializes a moved session's reasoning record: two of its requests
	// in flight read, change and save the same record.
	reasoningMu sync.Mutex
}

// New constructs an unconfigured plugin.
func New() *Router { return &Router{now: time.Now} }

// SetStore implements storage.StoreConsumer: the router keeps its pins in st, so a
// restart forgets none.
func (p *Router) SetStore(st storage.Store) { p.store = st }

// SetHistory implements session.HistoryConsumer: the router reads a session's archived
// events for where it went before this process (see wentTo).
func (p *Router) SetHistory(h session.History) { p.history = h }

func init() {
	plugins.RegisterPlugin(Name, func() pipeline.Plugin { return New() })
}

func (p *Router) Name() string { return Name }

func (p *Router) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		WritesDestination: true,
		WritesRequestBody: true, // SetRequestModel, and SetBody to drop another server's reasoning
		Requires:          []string{"inference-parser"},
		Description:       "Sends each agent's new sessions to its chosen server, wherever addressed.",
	}
}

// ConfigSchema implements pipeline.SchemaProvider. servers and agents are maps, which
// pipeline.SchemaOf renders as type "unknown".
func (p *Router) ConfigSchema() []pipeline.FieldSchema {
	return pipeline.SchemaOf(routerconfig.Config{})
}

// Configure decodes and validates the config, then builds the routing tables. A
// failed Configure leaves the previous tables in place; BuildWithDeps discards the
// instance anyway.
func (p *Router) Configure(raw json.RawMessage) error {
	c, err := routerconfig.Decode(raw)
	if err != nil {
		return fmt.Errorf("inference-router config: %w", err)
	}
	servers := make(map[string]route, len(c.Servers))
	byHost := make(map[string]string, len(c.Servers)) // one name per host: Decode refuses two servers on one
	for name, s := range c.Servers {
		ep, err := routerconfig.ParseURL(s.URL)
		if err != nil { // Decode already accepted it; kept so a drift between the two fails here
			return fmt.Errorf("inference-router config: servers.%s.url: %w", name, err)
		}
		if ep.PlaintextRemote() {
			slog.Warn("inference-router: server is plain http on another machine, so every request routed to it "+
				"crosses the network decrypted, its key and prompt included; use https unless the network is trusted",
				"server", name, "url", ep.URL())
		}
		servers[name] = route{endpoint: ep, key: s.Key, main: s.Main, helper: s.Helper}
		byHost[ep.Key()] = name
	}
	// p.agents is keyed by CANONICAL agent name, folded from whatever spelling the
	// config named: pipeline.CanonicalAgent maps retired spellings ("bob-shell",
	// "ibm-bob") to the name requests now report ("bob"), so a config written before a
	// rename keeps routing instead of silently matching nothing. pipeline.FoldAgentNames
	// is the one implementation of that fold, shared with agentop's own reads and
	// writes of the same agents: map so the two cannot disagree.
	p.servers, p.byHost, p.agents, p.sessions = servers, byHost, pipeline.FoldAgentNames(c.Agents), c.Sessions
	return nil
}

// OnRequest routes one request. See the package doc for what it decides and why.
func (p *Router) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	cont := pipeline.Action{Type: pipeline.Continue}

	// A CONNECT or a transparent connection is dialed where the client chose,
	// whatever a plugin does. Pinning on one would pin a session on a request that
	// cannot be routed, so it is left before anything is decided.
	if !pctx.Redirectable() {
		pctx.Skip("not_redirectable")
		return cont
	}
	// Every path on a server's host is handled — /v1/messages, count_tokens,
	// /v1/models — because they all belong to the server the session is on. A request
	// to any other host is captured when it is a routed agent's inference request,
	// and sent to the server's path for its API format.
	captured := ""
	addressed := p.byHost[routerconfig.Key(pctx.Host)] // the server the request named, "" for none
	if _, ok := p.byHost[routerconfig.Key(pctx.Host)]; !ok {
		path, ok := capture(pctx)
		// A sessions: override admits the capture too: the move is the user's
		// instruction, and it must reach a session whose agent is not routed and
		// holds no pin, which no other rule would route.
		if !ok || (p.agents[agentOf(pctx)] == "" && !p.pinnedToServer(pctx) && !p.overrideRoutes(pctx)) {
			if ok {
				p.keepOwnReasoning(pctx, "", false, "")
			}
			pctx.Skip("not_an_inference_server")
			return cont
		}
		captured = path
	}

	name, pin := p.serverFor(pctx, captured != "")
	if name == "" {
		// Not routed: the request, its key included, goes where the client sent it.
		// An own ("") override that cannot take effect — the router is observing, so
		// nothing it does applies — moves no pin: the pin the move would replace
		// stands until enforce is back. This branch returns before any redirect, so
		// it reads the policy itself.
		if pin.state == pinMoved && pctx.Observing() {
			pin.forgo()
		} else {
			pin.settle("")
		}
		p.keepOwnReasoning(pctx, addressed, pin.moving(), pin.movedFrom(addressed))
		details := map[string]string{"pin": pin.state}
		if pin.turn != "" {
			details["turn"] = pin.turn
		}
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "not_routed", Details: details})
		return cont
	}
	// Built where each record is made, since a failed redirect changes the pin.
	details := func() map[string]string { return map[string]string{"server": name, "pin": pin.state} }
	srv, ok := p.servers[name]
	if !ok {
		// Removed since the session was pinned, by agentop server remove or a hand edit.
		// Moving the conversation to whatever the agent uses now is the switch this
		// plugin exists to prevent.
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "pinned_server_removed", Details: details()})
		return pipeline.Deny(codeUnavailable, fmt.Sprintf(
			"this session is pinned to inference server %q, which is no longer configured; add it back, or start a new session", name))
	}

	// Redirected even when the Host header already names the server: the listener
	// dials the redirect target, and without one it dials what the client chose,
	// which on a bridged request is the CONNECT authority and not the Host header.
	if err := pctx.Redirect(srv.target()); err != nil {
		// The request went nowhere, so a first request pins nothing: pinning the server
		// would hold the session to one it never reached, and the next request can
		// decide again.
		pin.forgo()
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "redirect_failed", Details: details()})
		return pipeline.Deny(codeUnavailable, fmt.Sprintf("inference-router could not send this request to %q: %v", name, err))
	}
	// Redirect returns nil under on_error: observe without moving anything. The
	// server's key must then stay off the request, which still goes where the client
	// sent it. Redirected is the signal that it moved, and it is this router's
	// redirect: a pipeline admits one WritesDestination plugin.
	if !pctx.Redirected() {
		// The request stayed put, so that is what a first request pins: a session that
		// started under observe stays put once enforce is on. A session an override
		// would have moved keeps its pin instead: nothing moved — this is observe —
		// and the pin is the state the move, once enforce is on, will replace.
		if pin.state == pinMoved {
			pin.forgo()
		} else {
			pin.settle("")
		}
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionObserve, Reason: "would_route", Details: details()})
		return cont
	}
	if captured != "" {
		// The table's paths are absolute with no query, so this is refused only by a
		// drift between the table and SetRedirectPath's rule.
		if err := pctx.SetRedirectPath(captured); err != nil {
			pin.forgo()
			pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "redirect_failed", Details: details()})
			return pipeline.Deny(codeUnavailable, fmt.Sprintf("inference-router could not send this request to %q: %v", name, err))
		}
	}
	// The model is chosen only once the request has moved, so an observed router
	// changes nothing.
	p.chooseModel(pctx, name)
	// Before applyLimits, so limits are fitted to the body as sent.
	p.keepOwnReasoning(pctx, name, pin.moving(), pin.movedFrom(addressed))
	p.applyLimits(pctx, name, true)
	pin.settle(name)
	if addressed != name {
		dropProviderCredentials(pctx.Headers)
	}
	setKey(pctx, srv.key)
	d := details()
	if captured != "" {
		d["captured"] = captured
	}
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "routed", Details: d})
	return cont
}

func (p *Router) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// pin is a session's pin: the agent whose session it is, and the server the session
// is on, "" for not routed. See pins for where it is kept.
type pin struct {
	agent  string
	server string
}

// belongsTo reports whether this pin is agent's. The stored spelling is folded
// through pipeline.CanonicalAgent, so a pin recorded under a retired agent name —
// "bob-shell", before that name converged into "bob" — still matches the requests it
// was pinning. Without the fold, a rename reads every existing pin as another
// agent's, unpins the session, and re-decides it mid-conversation: the exact
// mid-conversation switch the pin exists to prevent.
func (pn pin) belongsTo(agent string) bool {
	return pipeline.CanonicalAgent(pn.agent) == agent
}

// pinning is one request's session pin: how it was decided, and for a
// session's first request where to store the outcome once OnRequest knows it.
type pinning struct {
	state string // pinNew, pinExisting or pinNone
	store pins
	key   string
	agent string
	// turn is turnContinuation or turnAside when the request's own turn left an
	// unpinned session unrouted, "" otherwise.
	turn string
	// override is whether a sessions: entry decided the server, and from the
	// server the session's pin named before it, "" for none or not routed.
	override bool
	from     string
}

// moving reports whether this request is the one an override moves its session
// with: from another pin (pinMoved), or from none (pinNew).
func (pn pinning) moving() bool {
	return pn.state == pinMoved || pn.state == pinNew && pn.override
}

// movedFrom is where the session ran before this request moved it: its pin's
// server, or, unrouted, wherever its requests are addressed.
func (pn pinning) movedFrom(addressed string) string {
	if pn.from != "" {
		return pn.from
	}
	return addressed
}

// settle pins a session on its first request to where that request went: server
// when it was redirected there, "" when it stayed where the client sent it. A
// session already pinned keeps its pin, which serverFor renewed.
func (pn *pinning) settle(server string) {
	if pn.state == pinNew || pn.state == pinMoved {
		pn.store.save(pn.key, pin{agent: pn.agent, server: server})
	}
}

// forgo leaves a session unpinned after a first request that went nowhere, so its
// next request decides again. A session already pinned keeps its pin.
func (pn *pinning) forgo() {
	if pn.state == pinNew || pn.state == pinMoved {
		pn.state = pinNone
	}
}

// serverFor is the server the request's session uses, "" for not routed, and the
// session's pin.
//
// A session its agent already pinned uses its pin, "not routed" included, so routing
// an agent later does not move the sessions it already has running; every request
// renews the pin. Otherwise the session is decided by unpinned, and the pin comes
// back pinNew for OnRequest to settle by where the request went (see the package
// doc) — except when the pin is another agent's, which this request must neither
// follow nor overwrite, or when the request decides nothing, and then it comes back
// pinNone. Without a session to pin, whether
// none, a synthetic one or no store, the agent's choice applies and nothing is
// pinned: a synthetic session's history is many conversations', not this one's.
//
// Two first requests of one session racing can both miss and both store; they store
// the same outcome unless a reload lands between them, which is the case the pin
// cannot rule out and does not need to.
func (p *Router) serverFor(pctx *pipeline.Context, captured bool) (string, pinning) {
	agent := agentOf(pctx)
	choice := p.agents[agent]
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return choice, pinning{state: pinNone}
	}
	store := p.pinsFor(pctx)
	if store == nil {
		p.noStore.Do(func() {
			slog.Warn("inference-router: this binary wires no process store, so sessions are not pinned: " +
				"each request follows its agent's current server, and changing it moves running sessions")
		})
		return choice, pinning{state: pinNone}
	}
	key := pinPrefix + pctx.Session.ID
	// A sessions: override is the user's explicit move: it beats the pin and every
	// unpinned rule, and the pin follows it, so removing the entry later does not
	// move the session back. pinMoved tells the record this request moved a session
	// that already held a pin; pinNew is a session that held none.
	if over, ok := p.overrideFor(pctx); ok {
		state, from := pinNew, ""
		if pn, had := store.load(key); had && pn.belongsTo(agent) {
			switch {
			case pn.server == over:
				state = pinExisting
				p.keepPin(pctx, store, key, pn)
			case pn.server != over:
				state, from = pinMoved, pn.server
			}
		}
		return over, pinning{state: state, store: store, key: key, agent: agent, override: true, from: from}
	}
	if pn, ok := store.load(key); ok {
		if !pn.belongsTo(agent) {
			server, _, _ := p.unpinned(pctx, agent, choice, captured)
			return server, pinning{state: pinNone}
		}
		p.keepPin(pctx, store, key, pn)
		return pn.server, pinning{state: pinExisting}
	}
	server, turn, decides := p.unpinned(pctx, agent, choice, captured)
	if !decides {
		return server, pinning{state: pinNone, turn: turn}
	}
	return server, pinning{state: pinNew, store: store, key: key, agent: agent, turn: turn}
}

// keepPin renews the session's pin and, with it, its reasoning record, which must
// not expire under a pin its session keeps alive.
func (p *Router) keepPin(pctx *pipeline.Context, store pins, key string, pn pin) {
	if store.keep(key, pn) {
		p.renewReasoning(pctx)
	}
}

// overrideFor is the sessions: entry for pctx's session, when one names it. A
// synthetic session is never overridden, whatever a hand-fed config says: its id
// holds many conversations, not one to move.
func (p *Router) overrideFor(pctx *pipeline.Context) (string, bool) {
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return "", false
	}
	over, ok := p.sessions[pctx.Session.ID]
	return over, ok
}

// overrideRoutes reports whether a sessions: entry moves pctx's session to a
// server. An entry to "" stops routing the session, which no capture needs: the
// request is already going where the client sent it.
func (p *Router) overrideRoutes(pctx *pipeline.Context) bool {
	over, ok := p.overrideFor(pctx)
	return ok && over != ""
}

// pinnedToServer reports whether pctx's session holds its agent's pin to a server.
func (p *Router) pinnedToServer(pctx *pipeline.Context) bool {
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return false
	}
	store := p.pinsFor(pctx)
	if store == nil {
		return false
	}
	pn, ok := store.load(pinPrefix + pctx.Session.ID)
	return ok && pn.belongsTo(agentOf(pctx)) && pn.server != ""
}

// unpinned is the server for a request in a session its agent holds no pin on, and
// whether the request decides the session's pin. In order:
//
//  1. An agent that is not routed is left alone, whatever its history.
//  2. The server the session's history shows the agent already uses.
//  3. A request no parser read as inference goes to the agent's current choice and
//     decides nothing.
//  4. A Claude Code request's own turn (see turnOf): a conversation's opening turn
//     goes to the agent's current choice; a continuation, a conversation the router
//     did not see begin, is not routed; a side request decides nothing, and goes to
//     the agent's current choice when it is a one-shot or was captured, and is not
//     routed when it is a subagent's to a server's host. turn names the last two,
//     for the record.
//  5. The agent's current choice.
func (p *Router) unpinned(pctx *pipeline.Context, agent, choice string, captured bool) (server, turn string, decides bool) {
	if choice == "" {
		return "", "", true
	}
	if server, ok := p.wentTo(pctx.Session, agent); ok {
		return server, "", true
	}
	if pctx.Extensions.Inference == nil {
		return choice, "", false
	}
	switch turnOf(pctx.Extensions.Inference) {
	case turnContinuation:
		return "", turnContinuation, true
	case turnAside:
		// A one-shot is sent beside the main conversation and is no part of it, so the
		// agent's server answering it moves nothing. A subagent's request is a
		// conversation of its own, which may be running already, so it is left where
		// Claude Code sent it unless captured: then leaving it would send the user's
		// credential to the provider the agent was routed away from.
		if captured || pctx.Extensions.Inference.AgentRole == pipeline.AgentRoleMain {
			return choice, turnAside, false
		}
		return "", turnAside, false
	}
	return choice, "", true
}

// turnOf is a Claude Code request's place in its session's conversation, read from
// inference-parser's parse: turnContinuation for the main conversation with earlier
// assistant turns, turnAside for any other request, and "" for the main
// conversation's opening turn or a request that states no role.
//
// The role is stated only by Claude Code's system prompt (see
// pipeline.InferenceExtension.AgentRole), and only Claude Code is read this way:
// another agent's earlier turns may have gone to another provider — OpenCode starts
// on Zen and addresses a server later — so its turns say nothing about whether its
// use of the servers began before this request. The main conversation carries its
// tool manifest; the one-shots Claude Code interleaves with it, its title request,
// auto mode's classifier and the permission monitor, carry none, and neither they
// nor a subagent's requests say whether the session began now.
func turnOf(ext *pipeline.InferenceExtension) string {
	if ext == nil || ext.AgentRole == "" {
		return ""
	}
	if ext.AgentRole != pipeline.AgentRoleMain || len(ext.Tools) == 0 {
		return turnAside
	}
	for _, m := range ext.Messages {
		if m.Role == "assistant" {
			return turnContinuation
		}
	}
	return ""
}

// wentTo is the server the latest earlier inference request agent sent in the
// session to a server's host went to; ok is false when there is no such request,
// which is what makes a session new. The session's events in memory are read first,
// then, where the process has a session archive, its archived events below the
// oldest one memory holds, up to historyLimit of them; an archive read that fails is
// no evidence.
//
// Only an outbound request row a parser read as inference counts, and only one that
// reached a server. A tunnel row, a count_tokens or a /v1/models request no parser
// claimed, a side request (see turnOf), which decides nothing, and a denied request,
// which went nowhere, say nothing about where the conversation is; nor does a
// request to any other host, which the router never routes (see the package doc).
// Another agent's row says nothing about this agent's conversation, and following it
// would hand this request that agent's server and key. A row's Host is where the
// bytes went, the server's host for a routed request, because the listener records
// it after the redirect; RequestedHost, where the client asked to go, is not where
// the session is.
func (p *Router) wentTo(view *pipeline.SessionView, agent string) (server string, ok bool) {
	events := view.Events
	for i := len(events) - 1; i >= 0; i-- {
		if server, ok := p.serverOf(&events[i], agent); ok {
			return server, true
		}
	}
	if p.history == nil {
		return "", false
	}
	var before uint64
	if len(events) > 0 {
		before = events[0].Seq
	}
	read := 0
	err := p.history.Earlier(view.ID, before, func(e *pipeline.SessionEvent) bool {
		read++
		server, ok = p.serverOf(e, agent)
		return !ok && read < historyLimit
	})
	if ok {
		return server, true
	}
	if err != nil {
		slog.Warn("inference-router: could not read the session's archived events; deciding it without them",
			"session", view.ID, "error", err)
	}
	return "", false
}

// serverOf is the server e shows agent's session on, by the rule wentTo gives; ok is
// false when e is no evidence.
func (p *Router) serverOf(e *pipeline.SessionEvent, agent string) (server string, ok bool) {
	if e.Direction != pipeline.Outbound || e.Phase != pipeline.SessionRequest || e.Inference == nil ||
		turnOf(e.Inference) == turnAside || pipeline.AgentName(e.Client.Label()) != agent {
		return "", false
	}
	if server, ok = p.byHost[routerconfig.Key(e.Host)]; ok {
		return server, true
	}
	// A request the router captures from a routed agent is recorded under its
	// server's host. One recorded where the agent sent it was made while the router
	// was not routing the agent — before it was routed, or under observe — so the
	// session ran unrouted, and moving it now would switch a running conversation.
	if _, captured := capturePath(e.HTTPMethod, e.HTTPPath); captured && e.RequestedHost == "" {
		return "", true
	}
	return "", false
}

// synthetic reports a session id the listener files traffic under when it knows no
// one conversation: the default bucket, or an agent's pending bucket. See the
// package doc for why those are never pinned.
func synthetic(id string) bool {
	return !session.IsConversation(id)
}

// agentOf is the request's agent as the session store and agentop name it, or ""
// when the request carried no User-Agent.
func agentOf(pctx *pipeline.Context) string {
	agent := pipeline.AgentName(pctx.ClientInfo().Label())
	if agent == pipeline.UnknownClientLabel {
		return ""
	}
	return agent
}

// credentialWords are the words of a header name that make the header a credential.
var credentialWords = []string{"key", "apikey", "token", "auth", "authorization", "cookie",
	"secret", "signature", "password", "credential", "credentials"}

// dropProviderCredentials removes from h each header whose name, split at "-" and
// "_" with case ignored, has one of credentialWords. Authorization and X-Api-Key stay
// for setKey, which puts the server's key in whichever of them the agent used. The
// router calls it on a request it sends to a server the request did not name.
func dropProviderCredentials(h http.Header) {
	for name := range h {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "X-Api-Key":
			continue
		}
		for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' }) {
			if slices.Contains(credentialWords, w) {
				h.Del(name)
				break
			}
		}
	}
}

// setKey puts key in the header the client authenticated with: X-Api-Key when it
// sent one, Authorization otherwise, both when it sent both. That keeps the header
// Claude Code uses whichever of ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN set it up.
func setKey(pctx *pipeline.Context, key string) {
	if pctx.Headers == nil {
		pctx.Headers = http.Header{}
	}
	h := pctx.Headers
	apiKey := len(h.Values("X-Api-Key")) > 0
	if apiKey {
		h.Set("X-Api-Key", key)
	}
	if !apiKey || len(h.Values("Authorization")) > 0 {
		h.Set("Authorization", "Bearer "+key)
	}
}

var (
	_ pipeline.Plugin         = (*Router)(nil)
	_ pipeline.Configurable   = (*Router)(nil)
	_ pipeline.SchemaProvider = (*Router)(nil)
	_ pipeline.Resender       = (*Router)(nil)
	_ pipeline.Initializer    = (*Router)(nil)
	_ pipeline.Shutdowner     = (*Router)(nil)
	_ storage.StoreConsumer   = (*Router)(nil)
	_ session.HistoryConsumer = (*Router)(nil)
)

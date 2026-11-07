package forwardproxy

import (
	"log/slog"
	"net/http"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/listener/internal/sessionevent"
	"github.com/rossoctl/cortex/core/pipeline"
)

// recordingSessionID turns the identity resolved at hydration into the bucket an
// event is recorded under. resolved is the listener's own answer, carried in a
// local so no plugin can rewrite it. It wins, and then the client is not resolved
// again. "" means hydration found no identity (or ran on a path that has none),
// and recording — unlike a plugin — must still file the event somewhere, so the
// client is resolved and attribution.RecordSession applies, in descending order
// of trustworthiness:
//
//  1. A session id the client put on the request (SessionIDHeaders). This is
//     the only source that can tell two concurrent agent sessions apart:
//     ActiveSession() is a single global "most recently updated" id, so with
//     two Claude Code windows open, whichever spoke last would swallow the
//     other's events.
//  2. ActiveSession(), which correlates an agent's outbound calls with the
//     inbound A2A turn that caused them — the in-cluster case, where the
//     agent is not the one holding the session id.
//  3. The default bucket, for traffic belonging to neither.
//
// clientHeaders must be the headers as RECEIVED (r.Header), not pctx.Headers.
// The two diverge: pctx.Headers starts as a clone of r.Header but the outbound
// pipeline has already run by the time events are recorded, so it holds the
// upstream-facing set including any plugin rewrite (staticinject writes a
// configurable target header, so this is reachable by configuration, not just
// in theory). A bucket key means "what the client claimed about its session", so
// a plugin rewriting a header for the gateway's benefit must not silently re-file
// telemetry — and it would fail invisibly, since "" means "fall back", never an
// error. Pass nil where there is no request to read (opaque tunnels); resolution
// then falls through to the two fallbacks below.
//
// Safe to call with s.Sessions == nil: ActiveSession() is skipped and the default
// bucket is returned, so a future caller that forgets the nil check on Sessions
// gets a usable answer rather than a panic.
func (s *Server) recordingSessionID(resolved string, clientHeaders http.Header, chain []attribution.Proc) string {
	if resolved != "" {
		return resolved
	}
	return s.attribute(clientHeaders, chain).RecordSession("", s.activeSession)
}

// trustedSessionOf is the listener's own answer to "which session is this request
// on", from how the connection arrived (vmcontrol installs the hook; the socket
// that accepted the connection names the VM). Empty when no hook is installed, or
// when it does not recognise this request — a transparently redirected connection
// has no request at all.
//
// Separate from hydrateSession so the pre-pipeline refusal paths can ask the same
// question: those never reach hydration, and a refusal filed under a header- or
// process-derived id defeats the point of deriving identity from the connection.
func (s *Server) trustedSessionOf(r *http.Request) string {
	if s.TrustedSessionID == nil || r == nil {
		return ""
	}
	return s.TrustedSessionID(r)
}

// attributeForRecording resolves a request that never reached hydration — refused before the
// pipeline, or unreadable — pins its attribution on pctx so its rows carry the process, and
// returns the bucket they go in: the trusted sid, else the attribution's, else
// ActiveSession(), else default.
//
// A trusted sid SHORT-CIRCUITS, and pins attribution EMPTY rather than skipping the
// pin: SetAttribution is write-once and AttributedProc() is recorded straight into
// SessionEvent.Proc, so claiming the slot with nothing is what stops a VM's refusal
// row from being stamped with the host process that merely happens to own the
// control socket — the same reasoning hydrateSession's trusted branch gives, applied
// to the paths that never get there (#1569 review).
func (s *Server) attributeForRecording(pctx *pipeline.Context, trusted string, clientHeaders http.Header, chain []attribution.Proc) string {
	var at attribution.Attribution
	if trusted == "" {
		at = s.attribute(clientHeaders, chain)
	}
	pctx.SetAttribution(at.Proc, at.ClientProc, at.Header)
	return at.RecordSession(trusted, s.activeSession)
}

// tunnelSessionID is recordingSessionID for a tunnel's own row. Under client affinity
// resolved is "" only for affinity's ambiguous answer or for no identity at all, and
// neither sends a tunnel row to default: a CONNECT rarely carries a User-Agent, so the
// ambiguous answer would file nearly every tunnel away from the request it carries.
// Those rows keep ActiveSession() at recording time, as without affinity.
// With the client's process known, an empty answer means a process of no agent, and the
// row goes to default like that process's requests.
func (s *Server) tunnelSessionID(resolved string, clientHeaders http.Header, chain []attribution.Proc) string {
	if s.ClientAffinity && resolved == "" && chain == nil {
		return attribution.FiledSession("", s.activeSession)
	}
	return s.recordingSessionID(resolved, clientHeaders, chain)
}

// hydrateSession resolves the plugin session for a request and, when there is one,
// sets pctx.Session and how it was resolved. It returns the id for the caller's
// local, which recording reads instead of pctx.
// trusted is the connection's own answer (trustedSessionOf), resolved by the
// caller — which also keeps the r.Header read at the CALL SITE rather than in
// here. The headers arrive as a parameter for the same reason trusted does:
// the caller resolves both once per request, so the pre-hydration refusal
// paths (attributeForRecording and its call sites) share the caller's values
// instead of re-deriving them. CodeQL note, so nobody re-litigates the
// placement: moving the r.Header read inside this function made CodeQL's
// diff-informed analysis report 28 pre-existing go/clear-text-logging and
// go/log-injection alerts in untouched plugins (sessionbudget, contextguru,
// inferencerouter) as new, every one sourced to the moved line. They are
// false positives — a header id passes validHeaderSessionID before anything
// logs it, and a session id is not a secret — and taking the read back out
// did NOT clear them; they need dismissing on the PR (#1569 review).
//
// The plugin session is attribution.PluginSession: recordingSessionID's order
// without the default-bucket fallback. It is "" when nothing names a session,
// and for a process of no agent.
//
// The missing fallback is the point. Recording must file an event somewhere, so
// "default" is the right last resort there. A plugin asking "which session is
// this?" must be able to hear "nothing known" — handing it ID "default" would
// silently satisfy sessionbudget's DefaultSessionFallback, an opt-in config, and
// start enforcing budgets against a shared bucket where today it deliberately
// skips (see its no_session_id path). Same precedence otherwise, so the two
// answers cannot diverge on any request that has an identity at all.
//
// TRUST — the decision this closes, recorded because it was previously deferred:
// the id may be client-asserted, and handing it to plugins means a client can
// name which session's state it is handed. Session is documented as an
// observability and correlation key rather than an authorization subject (see
// pipeline.Context.Session), and this resolution assumes that. IBAC is the plugin
// that tests the assumption, because it reads Session.LastIntent() to judge an
// outbound call and can deny on it — so where header bucketing is enabled, a
// client naming another session's id can have its action judged against that
// session's recorded intent. Before this, that required winning an
// ActiveSession() race; naming a header is deterministic. Session ids are also
// enumerable through the unauthenticated session API.
//
// Accepted for the case this is built for: a laptop, where the user owns every
// session and the goal is telling their own concurrent sessions apart. NOT
// acceptable where session attribution is a trust boundary — a shared or
// standalone forward proxy, or any deployment running an authorization plugin on
// session state. Those must set session.id_headers to an empty list, which
// returns this to ActiveSession() only.
//
// The open alternative is to narrow what plugins receive for a client-asserted
// id — the ID, which is all that sessionbudget, contextguru and sparc key on, but
// not the recorded events, so no client can adopt another session's intent. Its
// cost is sparc's InferenceRequests() correlation for header-identified sessions.
// Stated in full under "Decision recorded: client-asserted ids as plugin input"
// in #984, which is where to argue with it.
//
// With the client's process known (chain non-nil), a header claims the session for that
// process as well as for its agent, and a request with no header is filed by process first;
// client affinity and ActiveSession() answer only what it leaves open. See core/attribution.
func (s *Server) hydrateSession(pctx *pipeline.Context, trusted string, clientHeaders http.Header, chain []attribution.Proc) string {
	// The listener's own answer first, and it ends the search: a session
	// resolved from how the connection arrived is not something the client has
	// any say in, and letting a header or a process chain override it would
	// defeat the point of deriving identity from the connection (vmcontrol).
	// Hydration still happens — plugins get their pctx.Session view — but the
	// claims and affinity work below is skipped: a VM's traffic must not be
	// filed by a process it never ran as. A transparently redirected connection
	// has no request at all (r == nil), so there is nothing to claim either.
	//
	// Attribution is pinned EMPTY rather than left unset. SetAttribution is
	// write-once and AttributedProc() is recorded straight into
	// SessionEvent.Proc, so claiming the slot here is what stops a later
	// attributeForRecording from filling a VM's row in with a host process
	// that merely happens to own the control socket. The VM's identity is the
	// session id itself, which the connection already settled.
	var at attribution.Attribution
	if trusted == "" {
		at = s.attribute(clientHeaders, chain)
	}
	pctx.SetAttribution(at.Proc, at.ClientProc, at.Header)
	sid, source := at.PluginSession(trusted, s.activeSession)
	if sid != "" {
		pctx.Session = s.sessionViewFor(sid)
		pctx.SessionID = sid
		pctx.SessionSource = source
		pctx.SessionHeader = at.Header
	}
	if trusted == "" && chain != nil && s.processesOn() {
		pctx.ChainSession = at.ChainSession
		if pctx.ChainSession != "" && sid != pctx.ChainSession && source == pipeline.SessionSourceHeader {
			if _, seen := s.chainOverrideLogged.LoadOrStore(at.Header, struct{}{}); !seen {
				slog.Info("forwardproxy: a process named a session other than its parent agent's; "+
					"policy keyed on the process chain uses the parent's", "header", at.Header)
			}
		}
	}
	return sid
}

// sink is Sessions as the listener records through it. See sessionevent.SinkOf.
func (s *Server) sink() sessionevent.Sink { return sessionevent.SinkOf(s.Sessions) }

// activeSession is the store's ActiveSession(), "" without a store.
func (s *Server) activeSession() string {
	if s.Sessions == nil {
		return ""
	}
	return s.sink().ActiveSession()
}

// attribute is the attributor's answer for a request, or what an attributor with nothing
// configured gives — no session — when there is none (sessions off, or a test Server built
// without one).
func (s *Server) attribute(clientHeaders http.Header, chain []attribution.Proc) attribution.Attribution {
	a := s.attributor()
	if a == nil {
		return attribution.Attribution{Agent: attribution.AgentOf(clientHeaders)}
	}
	if !s.processesOn() {
		chain = nil
	}
	return a.Resolve(clientHeaders, chain)
}

// attributor is the Server's Attributor, built on first use from SessionIDHeaders and
// ClientAffinity — fields the caller sets after NewServer — and registered with the store,
// which tells it which sessions exist. nil without a store.
func (s *Server) attributor() *attribution.Attributor {
	if s.Sessions == nil {
		return nil
	}
	s.attributorOnce.Do(func() {
		s.attr = attribution.New(attribution.Config{
			SessionIDHeaders: s.SessionIDHeaders,
			ClientAffinity:   s.ClientAffinity,
		})
		s.Sessions.AddRecorder(s.attr)
	})
	return s.attr
}

// touch is attribution's Touch, a no-op without process attribution.
func (s *Server) touch(sid string, chain []attribution.Proc) {
	if a := s.attributor(); a != nil && chain != nil {
		a.Touch(sid, chain)
	}
}

// affinityClient names the coding agent behind a request for session attribution; see
// attribution.AgentOf.
func affinityClient(clientHeaders http.Header) string { return attribution.AgentOf(clientHeaders) }

// sessionViewFor returns the recorded view for sid, or an empty view carrying
// just the id when nothing has been recorded under it yet.
//
// Hydration runs BEFORE the request event is appended, so on a session's first
// request Store.View returns nil even though the id is known. Passing that nil
// through would tell every plugin "no session" on the first call of every
// session, and sessionbudget skips enforcement entirely on an empty id — so a
// client rotating session ids would never be metered. An empty Events slice is
// also the truthful answer: the session exists, and nothing has been recorded
// under it yet.
//
// Synthesizing rather than creating the bucket keeps this a read: hydration must
// not have the side effect of minting store entries for traffic that may still
// be rejected before anything is recorded.
func (s *Server) sessionViewFor(sid string) *pipeline.SessionView {
	if s.Sessions != nil {
		if v := s.sink().View(sid); v != nil {
			return v
		}
	}
	return &pipeline.SessionView{ID: sid}
}

// Package attribution decides which agent, process and session a request belongs to.
//
// It owns no events and never calls the session store: everything it knows it was told,
// through Resolve, Touch, and the session.Recorder hooks the store calls on every append.
// That is what lets the same code run outside the proxy — a local relay that resolves at
// accept time and stamps the answer on the request as headers (wire.go) — while the store
// only records. The answer is final: nothing renames a row after it is written. A process's
// calls before it names a session are linked to that session when the list is read
// (session.Link), not moved into it.
package attribution

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// Proc is one process in a request's chain, nearest first.
type Proc struct {
	PID int32
	// Start is the process's start time in Unix nanoseconds. PIDs are reused; PID and
	// Start together name one process for its lifetime.
	Start int64
	// Exe is the process's executable path, "" when unknown.
	Exe string
}

// Key is the process as events and bucket ids carry it: "<pid>.<start>".
func (p Proc) Key() string {
	return strconv.Itoa(int(p.PID)) + "." + strconv.FormatInt(p.Start, 10)
}

func (p Proc) key() procKey { return procKey{pid: p.PID, start: p.Start} }

type procKey struct {
	pid   int32
	start int64
}

// Attribution is one request's answer.
type Attribution struct {
	// Agent is the coding agent the request is attributed to, by canonical name: the one its
	// User-Agent names, or — for a pending answer — the agent whose process the calls wait
	// for. "" for none.
	Agent string
	// Proc is the agent process the request belongs to, as Proc.Key: the agent root of the
	// client's chain when the client is an agent's, the pending bucket's process for a
	// pending answer, else the client itself. "" when the client could not be looked up.
	Proc string
	// ClientProc is the client process itself, when it differs from Proc — a worker below its
	// launcher, Proc being the launcher. Its own pending bucket links through it. "" otherwise.
	ClientProc string
	// Session is a session the request belongs to, DefaultSessionID for a process of no
	// agent while an agent is active, or "": not named yet (Source pending) or nothing known
	// (fall back to the store's ActiveSession).
	Session string
	// Header is the session-id header that named Session when Source is header.
	Header string
	// Source says how Session was decided. pipeline.SessionSourcePending means "an agent
	// process that has named no session yet".
	Source pipeline.SessionSource
	// ChainSession is the live session the topmost process above the client named; see
	// pipeline.Context.ChainSession.
	ChainSession string
}

// PluginID is the session handed to plugins: a named session, the pending bucket for a
// pending answer, else "". The default bucket is never a plugin's session.
func (a Attribution) PluginID() string {
	switch {
	case a.Session != "" && a.Session != session.DefaultSessionID:
		return a.Session
	case a.Source == pipeline.SessionSourcePending:
		return session.PendingID(a.Agent, a.Proc)
	}
	return ""
}

// RecordID is the bucket a row is recorded under, or "" when the caller must fall back to
// the store's ActiveSession: the default bucket included, which plugins never hear.
func (a Attribution) RecordID() string {
	if a.Session != "" {
		return a.Session
	}
	if a.Source == pipeline.SessionSourcePending {
		return session.PendingID(a.Agent, a.Proc)
	}
	return ""
}

// Config is what an Attributor is built with.
type Config struct {
	// SessionIDHeaders are the headers that name a session, in order. Empty turns attribution
	// off: no session is any client's, so every answer falls back.
	SessionIDHeaders []string
	// ClientAffinity files a header-less request by its coding agent when its process is not
	// known.
	ClientAffinity bool
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Attributor resolves requests. Safe for concurrent use. Its lock is a leaf: it calls
// nothing that locks, so the store may call its Recorder hooks under the store's own lock.
type Attributor struct {
	cfg Config

	mu sync.Mutex
	// procs and lastProcClaim serve process attribution; see process.go.
	procs         map[procKey]*procState
	lastProcClaim time.Time
	// owners is client affinity's: the agent whose header first named each session.
	owners map[string]string
	// seen is when each session id was last claimed or recorded. It is attribution's own
	// answer to "does this session exist", which it used to ask the store.
	seen map[string]time.Time
}

// New returns an Attributor.
func New(cfg Config) *Attributor { return &Attributor{cfg: cfg} }

func (a *Attributor) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}
	return time.Now()
}

// AgentOf is the coding agent a request's User-Agent names, by canonical name, else "".
func AgentOf(h http.Header) string {
	if h == nil {
		return ""
	}
	return pipeline.ParseUserAgent(h.Get("User-Agent")).AffinityName()
}

// Resolve attributes one request. h is the request's headers as the client sent them, nil
// where there are none (a transparent connection); chain is the client's process chain,
// nil when it is not known, and then process attribution does not apply.
//
// Not a pure query: a session header claims the session for the client's process and agent,
// which is what later header-less requests are filed by. It writes only the attributor's
// own state.
func (a *Attributor) Resolve(h http.Header, chain []Proc) Attribution {
	agent := AgentOf(h)
	if len(a.cfg.SessionIDHeaders) == 0 {
		out := Attribution{Agent: agent}
		if len(chain) > 0 {
			out.Proc = chain[0].Key()
		}
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out := a.resolveLocked(h, agent, chain, now)
	if len(chain) > 0 {
		out.ChainSession = a.chainSessionLocked(chain, now)
	}
	return out
}

func (a *Attributor) resolveLocked(h http.Header, agent string, chain []Proc, now time.Time) Attribution {
	out := Attribution{Agent: agent}
	if len(chain) > 0 {
		out.Proc = chain[0].Key()
	}
	if sid, header := session.IDHeaderFrom(h, a.cfg.SessionIDHeaders); sid != "" {
		out.Session, out.Header, out.Source = sid, header, pipeline.SessionSourceHeader
		if len(chain) > 0 {
			out.Proc = a.claimProcessLocked(sid, agent, chain, now)
			if self := chain[0].Key(); self != out.Proc {
				out.ClientProc = self
			}
		}
		if a.cfg.ClientAffinity {
			a.claimLocked(sid, agent)
		}
		return out
	}
	if len(chain) > 0 {
		switch sid, pending := a.sessionForProcessLocked(chain, agent, now); {
		case pending != nil:
			out.Agent, out.Proc, out.Source = pending.agent, pending.proc, pipeline.SessionSourcePending
			return out
		case sid == session.DefaultSessionID:
			out.Session = sid
			return out
		case sid != "":
			out.Session, out.Source = sid, pipeline.SessionSourceProcess
			return out
		}
	}
	if a.cfg.ClientAffinity {
		switch sid := a.sessionForClientLocked(agent, now); {
		case sid == pendingAnswer:
			out.Proc, out.Source = "", pipeline.SessionSourcePending
		case sid == session.DefaultSessionID:
			out.Session = sid
		case sid != "":
			out.Session, out.Source = sid, pipeline.SessionSourceAffinity
		}
	}
	return out
}

// Touch records that a request chain[0] sent under sessionID has finished: a claim the
// client or its agent root holds on sessionID counts as made now, so a process serving
// several sessions answers with the one whose request finished last.
func (a *Attributor) Touch(sessionID string, chain []Proc) {
	if len(chain) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.touchLocked(sessionID, chain, a.now())
}

// Record implements session.Recorder: a session the store appended to exists.
func (a *Attributor) Record(sessionID string, _ *pipeline.SessionEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seeLocked(sessionID, a.now())
}

// Cleared implements session.Clearer: every session is gone, and with them every claim.
func (a *Attributor) Cleared() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.procs, a.lastProcClaim, a.owners, a.seen = nil, time.Time{}, nil, nil
}

// Deleted implements session.Deleter.
func (a *Attributor) Deleted(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.seen, sessionID)
	delete(a.owners, sessionID)
}

// Evicted implements session.Evicter: the store dropped id for max_sessions or session.ttl.
// A tool bound to it is rebound to its agent's live session on its next request.
func (a *Attributor) Evicted(sessionID string) { a.Deleted(sessionID) }

// Rekeyed implements session.Rekeyer, for the in-cluster default → A2A contextId rename.
func (a *Attributor) Rekeyed(oldID, newID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.seen[oldID]; ok {
		delete(a.seen, oldID)
		a.seen[newID] = t
	}
	if o, ok := a.owners[oldID]; ok {
		delete(a.owners, oldID)
		a.owners[newID] = o
	}
}

// maxSeen bounds the seen table. Past it, ids not seen for procIdleTTL go first, then the
// oldest quarter.
const maxSeen = 4096

func (a *Attributor) seeLocked(id string, now time.Time) {
	if a.seen == nil {
		a.seen = make(map[string]time.Time)
	}
	if _, ok := a.seen[id]; !ok && len(a.seen) >= maxSeen {
		a.pruneSeenLocked(now)
	}
	a.seen[id] = now
}

func (a *Attributor) pruneSeenLocked(now time.Time) {
	for id, t := range a.seen {
		if now.Sub(t) > procIdleTTL {
			delete(a.seen, id)
		}
	}
	if len(a.seen) < maxSeen {
		return
	}
	ids := make([]string, 0, len(a.seen))
	for id := range a.seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return a.seen[ids[i]].Before(a.seen[ids[j]]) })
	for _, id := range ids[:len(ids)/4] {
		delete(a.seen, id)
	}
}

// liveLocked reports whether id exists for attribution: claimed or recorded, and not since
// deleted, evicted or expired from the store.
func (a *Attributor) liveLocked(id string, _ time.Time) bool {
	_, ok := a.seen[id]
	return ok
}

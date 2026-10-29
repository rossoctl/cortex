package attribution

import (
	"sort"
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// Process attribution files a request that carries no session header under the session of
// the PROCESS that sent it. The rules are the ones session.Store applied before this package
// existed, less adoption: a process that has named no session yet gets a pending answer, and
// its calls stay in their bucket. Walking the chain from the client up, at each process it
// knows:
//
//  1. Named a session through its own session header: its newest live one. A process that
//     never named one — a tool an agent's shell ran — is bound to that answer the first
//     time it is seen, and keeps it for its life.
//  2. An agent's (its requests carry a known coding agent's User-Agent) that has named no
//     session yet: a pending answer for that agent's root. Stopping here is what keeps a
//     nested agent — a claude -p run from another session's shell — out of its parent's
//     session.
//  3. Named sessions, none of them live now: "", so resolution falls back.
//  4. Bound earlier: that session.
//
// No agent anywhere in the chain is the default bucket while some process has named a
// session within ambiguityWindow, and "" otherwise.

// procState is what the attributor knows about one process.
type procState struct {
	// claims are the sessions this process named through its own session header — or a
	// process of the same agent directly below it named (see agentRootIndexLocked) — each
	// with when it was last active.
	claims map[string]time.Time
	// bound is the session a process that never named one was filed under the first time
	// it was seen; "" until then.
	bound string
	// agent is the coding agent this process's requests identify as by User-Agent.
	agent string
	// claimed is set when the process — or a process of the same agent below it — names a
	// session, and stays set for as long as the table keeps the process.
	claimed bool
	seen    time.Time
}

// maxProcs caps the process table; procIdleTTL is how long a quiet process is kept once it
// is full. A live agent re-names its session on every headered request, so only a process
// that has gone quiet, almost always one that has exited, is that old.
const (
	maxProcs    = 4096
	procIdleTTL = 24 * time.Hour
)

// ambiguityWindow is how recently a known agent must have sent traffic to count toward the
// ambiguous answers. Expiry cannot serve: session.ttl defaults to never.
const ambiguityWindow = 5 * time.Minute

// pendingRef is a pending answer: the agent whose process has named no session, and that
// process as Proc.Key.
type pendingRef struct{ agent, proc string }

// claimProcessLocked records that chain[0] named sessionID, on chain[0] and on its agent
// root, so a launcher and its worker answer with one session. It returns the root's key,
// the process the request is attributed to.
func (a *Attributor) claimProcessLocked(sessionID, agent string, chain []Proc, now time.Time) string {
	if len(sessionID) > session.MaxSessionIDLen {
		sessionID = sessionID[:session.MaxSessionIDLen]
	}
	a.lastProcClaim = now
	self := a.procLocked(chain[0], now)
	// A process that names a session is never a tool bound to another's.
	self.bound = ""
	if agent != "" {
		self.agent = agent
	}
	root := a.agentRootIndexLocked(chain, self.agent)
	for _, i := range []int{0, root} {
		st := a.procLocked(chain[i], now)
		if st.claims == nil {
			st.claims = make(map[string]time.Time, 1)
		}
		st.claims[sessionID] = now
		st.claimed = true
	}
	// A claim makes the session exist for attribution before the store has recorded the
	// request that carried it, which is what claimGrace used to cover.
	a.seeLocked(sessionID, now)
	return chain[root].Key()
}

func (a *Attributor) touchLocked(sessionID string, chain []Proc, now time.Time) {
	self := a.procs[chain[0].key()]
	if self == nil {
		return
	}
	for _, i := range []int{0, a.agentRootIndexLocked(chain, self.agent)} {
		if st := a.procs[chain[i].key()]; st != nil {
			if _, ok := st.claims[sessionID]; ok {
				st.claims[sessionID] = now
			}
		}
	}
}

// sessionForProcessLocked is where a header-less request from chain[0] goes, by the rules at
// the top of this file: a session, DefaultSessionID, a pending answer, or "" to fall back.
func (a *Attributor) sessionForProcessLocked(chain []Proc, agent string, now time.Time) (string, *pendingRef) {
	self := a.procLocked(chain[0], now)
	if agent != "" {
		// An agent's process is never a tool bound to another session. Its first request
		// may have been a CONNECT, which carries no User-Agent, and been bound before it
		// said whose it was.
		self.agent, self.bound = agent, ""
	}
	for i, p := range chain {
		st := self
		if i > 0 {
			if st = a.procs[p.key()]; st == nil {
				continue
			}
		}
		if sid := a.newestClaimLocked(st, now); sid != "" {
			if i > 0 && self.agent == "" && !self.claimed {
				self.bound = sid
			}
			return sid, nil
		}
		if st.agent != "" {
			root := i + a.agentRootIndexLocked(chain[i:], st.agent)
			if root != i {
				if rs := a.procs[chain[root].key()]; rs != nil {
					if sid := a.newestClaimLocked(rs, now); sid != "" {
						if i > 0 && self.agent == "" && !self.claimed {
							self.bound = sid
						}
						return sid, nil
					}
				}
			}
			return "", &pendingRef{agent: st.agent, proc: chain[root].Key()}
		}
		if st.claimed {
			// A process that names its own sessions is no other session's, so with none
			// of them live the walk stops here and resolution falls back.
			return "", nil
		}
		if st.bound != "" {
			if a.liveLocked(st.bound, now) {
				if i > 0 && self.agent == "" && !self.claimed {
					self.bound = st.bound
				}
				return st.bound, nil
			}
			st.bound = ""
		}
	}
	if !a.lastProcClaim.IsZero() && now.Sub(a.lastProcClaim) <= ambiguityWindow {
		return session.DefaultSessionID, nil
	}
	return "", nil
}

// chainSessionLocked is the live session the topmost process above chain[0] named, "" when
// none above it holds one. chain[0]'s agent root and the processes of that agent below it
// are chain[0]'s own and are not consulted.
func (a *Attributor) chainSessionLocked(chain []Proc, now time.Time) string {
	if len(chain) < 2 {
		return ""
	}
	agent := ""
	if self := a.procs[chain[0].key()]; self != nil {
		agent = self.agent
	}
	for i := len(chain) - 1; i > a.agentRootIndexLocked(chain, agent); i-- {
		if st := a.procs[chain[i].key()]; st != nil {
			if sid := a.newestClaimLocked(st, now); sid != "" {
				return sid
			}
		}
	}
	return ""
}

// ProcessHints are the PIDs of processes that have named a session, most recently seen
// first and at most limit of them: where a lookup for a new connection looks first.
func (a *Attributor) ProcessHints(limit int) []int32 {
	if limit <= 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	type seenProc struct {
		pid  int32
		seen time.Time
	}
	var claimers []seenProc
	for k, st := range a.procs {
		if st.claimed {
			claimers = append(claimers, seenProc{pid: k.pid, seen: st.seen})
		}
	}
	sort.Slice(claimers, func(i, j int) bool { return claimers[i].seen.After(claimers[j].seen) })
	if len(claimers) > limit {
		claimers = claimers[:limit]
	}
	out := make([]int32, len(claimers))
	for i, c := range claimers {
		out[i] = c.pid
	}
	return out
}

// IsAgentProcess reports whether p — or a process of the same agent it runs — has named a
// session through its own header.
func (a *Attributor) IsAgentProcess(p Proc) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.procs[p.key()]
	return st != nil && st.claimed
}

// procLocked is p's state, created on first sight and stamped as seen now.
func (a *Attributor) procLocked(p Proc, now time.Time) *procState {
	if a.procs == nil {
		a.procs = make(map[procKey]*procState)
	}
	k := p.key()
	st, ok := a.procs[k]
	if !ok {
		if len(a.procs) >= maxProcs {
			a.pruneProcsLocked(now)
		}
		st = &procState{}
		a.procs[k] = st
	}
	st.seen = now
	return st
}

// pruneProcsLocked makes room in the process table: every process not heard from in
// procIdleTTL, then, if that freed nothing below the cap, the least recently seen quarter,
// processes that never named a session first.
func (a *Attributor) pruneProcsLocked(now time.Time) {
	for k, st := range a.procs {
		if now.Sub(st.seen) > procIdleTTL {
			delete(a.procs, k)
		}
	}
	if len(a.procs) < maxProcs {
		return
	}
	type aged struct {
		k       procKey
		claimed bool
		seen    time.Time
	}
	all := make([]aged, 0, len(a.procs))
	for k, st := range a.procs {
		all = append(all, aged{k: k, claimed: st.claimed, seen: st.seen})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].claimed != all[j].claimed {
			return !all[i].claimed
		}
		return all[i].seen.Before(all[j].seen)
	})
	for _, x := range all[:len(all)/4] {
		delete(a.procs, x.k)
	}
}

// agentRootIndexLocked is the index in chain of the topmost process of agent reached from
// chain[0] through processes of that same agent only. A claude -p started from a shell is
// its own root, because the shell between it and the outer claude is no agent's.
func (a *Attributor) agentRootIndexLocked(chain []Proc, agent string) int {
	if agent == "" {
		return 0
	}
	i := 0
	for i+1 < len(chain) {
		st := a.procs[chain[i+1].key()]
		if st == nil || st.agent != agent {
			break
		}
		i++
	}
	return i
}

// newestClaimLocked is the live session st has named most recently, "" when none is.
func (a *Attributor) newestClaimLocked(st *procState, now time.Time) string {
	var best string
	var at time.Time
	for id, claimedAt := range st.claims {
		if !a.liveLocked(id, now) {
			delete(st.claims, id)
			continue
		}
		if best == "" || claimedAt.After(at) {
			best, at = id, claimedAt
		}
	}
	return best
}

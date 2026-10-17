package session

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Process attribution files a request that carries no session header under the session of
// the PROCESS that sent it, rather than by User-Agent (client affinity) or by timing
// (ActiveSession). A listener that can name a connection's process — core/peerproc, on a
// laptop install — passes the client's chain: the client first, then its parents.
// Everything here is inert until a listener calls ClaimProcess or SessionForProcess, which
// only one with process attribution on does.
//
// SessionForProcess walks the chain from the client up and, at each process it knows:
//
//	1. Named a session through its own session header (ClaimProcess): its live session.
//	   A process that never named one — a tool an agent's shell ran —
//	   is bound to that answer the first time it is seen, and keeps it for its life.
//	2. An agent's (its requests carry a known coding agent's User-Agent) that has named no
//	   session yet: that agent's pending bucket, which the first session it names adopts.
//	   Stopping here is what keeps a nested agent — a claude -p run from another session's
//	   shell — out of its parent's session.
//	3. Named sessions, none of them live now: "", so resolution falls back — it is no other session's.
//	4. Bound earlier: that session.
//
// No agent anywhere in the chain is the default bucket while some process has named a
// session within ambiguityWindow, and "" otherwise, so ActiveSession() answers as before.

// Proc is one process in a request's chain.
type Proc struct {
	PID int32
	// Start is the process's start time in Unix nanoseconds. PIDs are reused; PID and
	// Start together name one process for its lifetime.
	Start int64
	// Exe is the process's executable path, "" when unknown.
	Exe string
}

type procKey struct {
	pid   int32
	start int64
}

func (p Proc) key() procKey { return procKey{pid: p.PID, start: p.Start} }

// procState is what the store knows about one process.
type procState struct {
	// claims are the sessions this process named through its own session header — or a
	// process of the same agent directly below it named (see agentRootIndexLocked).
	claims map[string]time.Time
	// bound is the session a process that never named one was filed under the first time
	// it was seen; "" until then.
	bound string
	// agent is the coding agent this process's requests identify as by User-Agent.
	agent string
	// claimed is set when the process — or a process of the same agent below it — names a
	// session, and stays set for as long as the store keeps the process: it is what makes a
	// process an agent's own (IsAgentProcess) even after the sessions it named have been evicted.
	claimed bool
	seen    time.Time
}

// maxProcs caps the process table. Once it is full, a process the store has not heard from
// for procIdleTTL is dropped first (pruneProcsLocked). A live agent re-names its session on
// every headered request, so only a process that has gone quiet, almost always one that
// has exited, is that old.
const (
	maxProcs    = 4096
	procIdleTTL = 24 * time.Hour
)

// claimGrace is how long a claim on a session that has recorded nothing is kept. A claim
// is made at hydration, before the request that carried the header is appended, so a
// concurrent request can see the claim before the session exists.
const claimGrace = time.Minute

// PendingProcessID is the pending bucket of an agent process that has named no session
// yet: the agent's pending bucket, per process — pid and start time, since pids are reused
// and an adopted bucket's id keeps redirecting into its session — so two windows of one
// agent starting at once do not share one.
func PendingProcessID(agent string, p Proc) string {
	return PendingSessionID(agent) + "@" + strconv.Itoa(int(p.PID)) + "." + strconv.FormatInt(p.Start, 10)
}

// pendingOwner is the agent a pending bucket belongs to — claude-code for both
// pending:claude-code and pending:claude-code@4242 — and false for any other id.
func pendingOwner(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, PendingPrefix)
	if !ok {
		return "", false
	}
	agent, _, _ := strings.Cut(rest, "@")
	return agent, true
}

// ClaimProcess records that chain[0] named sessionID through its own session header, as
// Claim does for an agent. The claim is also recorded on the chain's agent root, so an
// agent that runs as a launcher and a worker answers with one session from both, and the
// root's pending bucket — and chain[0]'s own, if different — is adopted into sessionID
// when sessionID holds nothing yet. Adoption is attempted on a process's first claim of a
// session only, so a bucket that cannot be adopted is not retried — and logged — on every
// later request; one created between that claim and the session's first event stays a row
// of its own.
func (s *Store) ClaimProcess(sessionID, agent string, chain []Proc) {
	if sessionID == "" || len(chain) == 0 || strings.HasPrefix(sessionID, PendingPrefix) {
		return
	}
	if len(sessionID) > MaxSessionIDLen {
		sessionID = sessionID[:MaxSessionIDLen]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	s.lastProcClaim = now
	self := s.procLocked(chain[0], now)
	// A process that names a session is never a tool bound to another's.
	self.bound = ""
	if agent != "" {
		self.agent = agent
	}
	_, already := self.claims[sessionID]
	root := s.agentRootIndexLocked(chain, self.agent)
	for _, i := range []int{0, root} {
		st := s.procLocked(chain[i], now)
		if st.claims == nil {
			st.claims = make(map[string]time.Time, 1)
		}
		st.claims[sessionID] = now
		st.claimed = true
	}
	if self.agent != "" && !already {
		s.adoptLocked(PendingProcessID(self.agent, chain[root]), sessionID)
		if root != 0 {
			s.adoptLocked(PendingProcessID(self.agent, chain[0]), sessionID)
		}
	}
}

// TouchProcess records that a request chain[0] sent under sessionID has finished. A claim
// chain[0] or its agent root holds on sessionID counts as made now, so a process serving
// several sessions answers with the one whose request finished last. A process that holds
// no claim on sessionID, such as a tool bound to it, changes nothing.
func (s *Store) TouchProcess(sessionID string, chain []Proc) {
	if len(chain) == 0 {
		return
	}
	if len(sessionID) > MaxSessionIDLen {
		sessionID = sessionID[:MaxSessionIDLen]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	self := s.procs[chain[0].key()]
	if self == nil {
		return
	}
	now := s.clock()
	for _, i := range []int{0, s.agentRootIndexLocked(chain, self.agent)} {
		if st := s.procs[chain[i].key()]; st != nil {
			if _, ok := st.claims[sessionID]; ok {
				st.claims[sessionID] = now
			}
		}
	}
}

// SessionForProcess is where a request with no session header from chain[0] goes, in the
// order the comment at the top of this file gives, or "" to fall back to client affinity
// and then ActiveSession(). agent is the coding agent its User-Agent names, "" for none;
// it is remembered for the process, which is how an agent is told from the shell it runs.
func (s *Store) SessionForProcess(chain []Proc, agent string) string {
	if len(chain) == 0 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	self := s.procLocked(chain[0], now)
	if agent != "" {
		// An agent's process is never a tool bound to another session. Its first request
		// may have been a CONNECT, which carries no User-Agent, and been bound before it
		// said whose it was.
		self.agent, self.bound = agent, ""
	}
	for i, p := range chain {
		st := self
		if i > 0 {
			if st = s.procs[p.key()]; st == nil {
				continue
			}
		}
		if sid := s.newestClaimLocked(st, now); sid != "" {
			if i > 0 && self.agent == "" && !self.claimed {
				self.bound = sid
			}
			return sid
		}
		if st.agent != "" {
			root := i + s.agentRootIndexLocked(chain[i:], st.agent)
			// A claim is recorded on the agent's root as well as on the claimer, so a worker
			// whose launcher has named a session answers with it.
			if root != i {
				if rs := s.procs[chain[root].key()]; rs != nil {
					if sid := s.newestClaimLocked(rs, now); sid != "" {
						if i > 0 && self.agent == "" && !self.claimed {
							self.bound = sid
						}
						return sid
					}
				}
			}
			return PendingProcessID(st.agent, chain[root])
		}
		if st.claimed {
			// A process that names its own sessions is no other session's, so with none
			// of them live the walk stops here and resolution falls back.
			return ""
		}
		if st.bound != "" {
			if s.liveLocked(st.bound, now) {
				if i > 0 && self.agent == "" && !self.claimed {
					self.bound = st.bound
				}
				return st.bound
			}
			st.bound = ""
		}
	}
	if !s.lastProcClaim.IsZero() && now.Sub(s.lastProcClaim) <= ambiguityWindow {
		return DefaultSessionID
	}
	return ""
}

// ChainSession is the live session the topmost process above chain[0] named, "" when no
// process above it holds one. chain[0]'s agent root and the processes of that agent below
// it are chain[0]'s own and are not consulted. Unlike SessionForProcess it ignores what
// chain[0] named itself, so a process cannot leave the session of the agent that started
// it by naming one of its own: a policy that keys security state on the session asks this.
func (s *Store) ChainSession(chain []Proc) string {
	if len(chain) < 2 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := ""
	if self := s.procs[chain[0].key()]; self != nil {
		agent = self.agent
	}
	now := s.clock()
	for i := len(chain) - 1; i > s.agentRootIndexLocked(chain, agent); i-- {
		if st := s.procs[chain[i].key()]; st != nil {
			if sid := s.newestClaimLocked(st, now); sid != "" {
				return sid
			}
		}
	}
	return ""
}

// ProcessHints are the PIDs of processes that have named a session, most recently seen
// first and at most limit of them: where a lookup for a new connection looks first.
func (s *Store) ProcessHints(limit int) []int32 {
	if limit <= 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	type seenProc struct {
		pid  int32
		seen time.Time
	}
	var claimers []seenProc
	for k, st := range s.procs {
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

// IsAgentProcess reports whether p — or a process of the same agent it runs (see
// agentRootIndexLocked) — has named a session through its own header.
func (s *Store) IsAgentProcess(p Proc) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.procs[p.key()]
	return st != nil && st.claimed
}

// procLocked is p's state, created on first sight and stamped as seen now.
func (s *Store) procLocked(p Proc, now time.Time) *procState {
	if s.procs == nil {
		s.procs = make(map[procKey]*procState)
	}
	k := p.key()
	st, ok := s.procs[k]
	if !ok {
		if len(s.procs) >= maxProcs {
			s.pruneProcsLocked(now)
		}
		st = &procState{}
		s.procs[k] = st
	}
	st.seen = now
	return st
}

// pruneProcsLocked makes room in the process table: every process not heard from in
// procIdleTTL, then, if that freed nothing below the cap, the least recently seen quarter,
// processes that never named a session first.
func (s *Store) pruneProcsLocked(now time.Time) {
	for k, st := range s.procs {
		if now.Sub(st.seen) > procIdleTTL {
			delete(s.procs, k)
		}
	}
	if len(s.procs) < maxProcs {
		return
	}
	type aged struct {
		k       procKey
		claimed bool
		seen    time.Time
	}
	all := make([]aged, 0, len(s.procs))
	for k, st := range s.procs {
		all = append(all, aged{k: k, claimed: st.claimed, seen: st.seen})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].claimed != all[j].claimed {
			return !all[i].claimed
		}
		return all[i].seen.Before(all[j].seen)
	})
	for _, a := range all[:len(all)/4] {
		delete(s.procs, a.k)
	}
}

// agentRootIndexLocked is the index in chain of the topmost process of agent reached from
// chain[0] through processes of that same agent only. An agent that runs as a launcher
// and a worker (node bob above node bob) has one root; a claude -p started from a shell is
// its own, because the shell between it and the outer claude is no agent's. A nested agent
// its parent agent runs directly, with no shell between, cannot be told from a worker and
// joins its parent's root; Claude Code runs its tools through a shell.
func (s *Store) agentRootIndexLocked(chain []Proc, agent string) int {
	if agent == "" {
		return 0
	}
	i := 0
	for i+1 < len(chain) {
		st := s.procs[chain[i+1].key()]
		if st == nil || st.agent != agent {
			break
		}
		i++
	}
	return i
}

// newestClaimLocked is the live session st has named, "" when none is. A claim on a session
// that is gone is forgotten once claimGrace has passed.
func (s *Store) newestClaimLocked(st *procState, now time.Time) string {
	var best string
	var at time.Time
	for id, claimedAt := range st.claims {
		sess, ok := s.sessions[id]
		if !ok || s.isExpired(sess, now) {
			if now.Sub(claimedAt) > claimGrace {
				delete(st.claims, id)
			}
			continue
		}
		if best == "" || claimedAt.After(at) {
			best, at = id, claimedAt
		}
	}
	return best
}

func (s *Store) liveLocked(id string, now time.Time) bool {
	sess, ok := s.sessions[id]
	return ok && !s.isExpired(sess, now)
}

// forgetAdoptedLocked drops the adoption records that name id, as the pending bucket or
// as its adopter, once id has left the store. followAdoptedLocked already ignores a record
// whose adopter is gone; this stops them accumulating for the life of the process.
func (s *Store) forgetAdoptedLocked(id string) {
	for pending, to := range s.adopted {
		if pending == id || to == id {
			delete(s.adopted, pending)
		}
	}
}

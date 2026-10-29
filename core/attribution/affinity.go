package attribution

import (
	"time"

	"github.com/rossoctl/cortex/core/session"
)

// Client affinity files a header-less request whose process is not known under a session
// of the SAME coding agent, by User-Agent, rather than under the store's ActiveSession().

// pendingAnswer is sessionForClientLocked's "this agent has named nothing yet".
const pendingAnswer = "\x00pending"

// claimLocked records that client named sessionID through its own header. First claim
// wins: a second agent quoting the same id does not take it over.
func (a *Attributor) claimLocked(sessionID, client string) {
	if sessionID == "" || client == "" {
		return
	}
	if len(sessionID) > session.MaxSessionIDLen {
		sessionID = sessionID[:session.MaxSessionIDLen]
	}
	if a.owners == nil {
		a.owners = make(map[string]string)
	}
	if _, owned := a.owners[sessionID]; !owned {
		if len(a.owners) >= maxSeen {
			for id := range a.owners {
				if _, ok := a.seen[id]; !ok {
					delete(a.owners, id)
				}
			}
		}
		a.owners[sessionID] = client
	}
	a.seeLocked(sessionID, a.now())
}

// sessionForClientLocked is where a request with no session header and no known process
// goes under client affinity:
//
//  1. A known client: its newest live session, else pendingAnswer.
//  2. An unknown client while two or more known clients have each sent traffic within
//     ambiguityWindow: the default bucket.
//  3. Anything else: "", so ActiveSession() answers. This is the in-cluster case.
func (a *Attributor) sessionForClientLocked(client string, now time.Time) string {
	if client != "" {
		var newest string
		var at time.Time
		for id, owner := range a.owners {
			if owner != client || !a.liveLocked(id, now) {
				continue
			}
			if t := a.seen[id]; newest == "" || t.After(at) {
				newest, at = id, t
			}
		}
		if newest != "" {
			return newest
		}
		return pendingAnswer
	}
	seen := make(map[string]struct{}, 2)
	for id, t := range a.seen {
		if now.Sub(t) > ambiguityWindow {
			continue
		}
		owner := a.owners[id]
		if owner == "" {
			var ok bool
			if owner, _, ok = session.ParsePending(id); !ok {
				continue
			}
		}
		seen[owner] = struct{}{}
		if len(seen) >= 2 {
			return session.DefaultSessionID
		}
	}
	return ""
}

package session

import "strings"

// PendingPrefix marks a pending bucket: the calls an agent process makes before it names a
// session, recorded under PendingID. Nothing renames one; see Link.
const PendingPrefix = "pending:"

// PendingSessionID is the agent-wide pending bucket for client, used when the client's
// process could not be looked up.
func PendingSessionID(client string) string { return PendingPrefix + client }

// Rekeyer is optionally implemented by a Recorder that keeps per-session state, so that
// state follows a rename. Called under the store's write lock, once for every rename Rekey —
// the in-cluster A2A default → contextId merge — makes, and never for one it refused.
type Rekeyer interface {
	Rekeyed(oldID, newID string)
}

// Evicter is optionally implemented by a Recorder that keeps per-session state: the store
// calls Evicted under its write lock when it drops a session for max_sessions or ttl, as it
// calls Deleted for an explicit delete. The attributor forgets the session, so a tool bound
// to it is rebound to its agent's live one.
type Evicter interface {
	Evicted(sessionID string)
}

// PendingID is the pending bucket for agent's calls from one process, proc being that
// process as "<pid>.<start>", or the agent-wide bucket when proc is "" — the process
// could not be looked up. It is a recording id only: nothing renames a pending bucket,
// and its calls are linked to the session the process names when the list is read (see
// Link).
func PendingID(agent, proc string) string {
	if proc == "" {
		return PendingSessionID(agent)
	}
	return PendingSessionID(agent) + "@" + proc
}

// IsConversation reports whether id is one conversation's session: false for "", the
// default bucket, and any pending bucket, each of which holds many conversations' calls or
// none. Plugins that key state on a session (the inference-router's pins, cpex's taint)
// skip the others.
func IsConversation(id string) bool {
	return id != "" && id != DefaultSessionID && !strings.HasPrefix(id, PendingPrefix)
}

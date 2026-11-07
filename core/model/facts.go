package model

// FactState tells if a session fact is present and, if it is not, if the request has a session.
// The zero value is FactNone. FactNoSession comes only from Context.Session being nil, so no
// stored or zero SessionFacts reads as "no session".
type FactState uint8

const (
	// FactNone: the request has a session, and the store has no event of this kind for it.
	// The reasons are:
	//   - the store has no entry for the session yet, or it dropped the entry (max_sessions,
	//     ttl, Delete, Clear, a restart);
	//   - no event of this kind was appended to the session;
	//   - a session.max_events trim removed the event.
	FactNone FactState = iota
	// FactNoSession: the request has no session. Context.Session is nil.
	FactNoSession
	// FactPresent: Event is set.
	FactPresent
)

// SessionFact is one session fact. Event is not nil only when State is FactPresent.
type SessionFact struct {
	Event *SessionEvent
	State FactState
}

// SessionFacts are the facts that the session store keeps for one session.
type SessionFacts struct {
	LastIntent    SessionFact // the newest event that IsIntent accepts
	LastInference SessionFact // the newest event that IsInferenceRequest accepts

	// InferenceHosts is the session's table: the store entry's events and the archive's history.
	InferenceHosts InferenceHosts
	// HostsKnown is false when no store answered, or when the archive holds history that it has
	// no table for. So the zero value is "unknown", and a reader must not use the table.
	HostsKnown bool
}

// IsIntent reports an inbound A2A request: a user intent. SessionView.LastIntent uses the
// same rule.
func IsIntent(e *SessionEvent) bool {
	return e.Direction == Inbound && e.Phase == SessionRequest && e.A2A != nil
}

// IsInferenceRequest reports an outbound inference request. SessionView.InferenceRequests
// uses the same rule.
func IsInferenceRequest(e *SessionEvent) bool {
	return e.Direction == Outbound && e.Phase == SessionRequest && e.Inference != nil
}

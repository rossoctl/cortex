// Package session provides an in-memory session store for correlating
// inbound user intents with outbound tool calls across request boundaries.
// The store is per-pod (AuthBridge sidecar) and does not persist across restarts.
package session

import (
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// DefaultSessionID is used when no explicit A2A SessionID is present and no
// active session exists. This collapses all such requests into one shared session,
// which is correct for single-agent pods but may cause cross-user correlation in
// multi-tenant deployments. Future work: derive session ID from JWT claims.
const DefaultSessionID = "default"

// entry holds the events for one conversation.
type entry struct {
	ID        string
	Events    []pipeline.SessionEvent
	CreatedAt time.Time
	UpdatedAt time.Time
}

const maxSessionIDLen = 256

// Store is an in-memory, per-pod session store. It is safe for concurrent use.
type Store struct {
	mu          sync.RWMutex
	sessions    map[string]*entry
	ttl         time.Duration
	maxEvents   int
	maxSessions int
	activeID    string
	stop        chan struct{}
	closeOnce   sync.Once

	// subscribers is the fan-out list for Subscribe(). Protected by mu.
	subscribers []*subscriber
}

// subscriberChanBuf caps each subscriber's channel depth. 64 absorbs short
// bursts without unbounded memory; slow consumers drop rather than blocking
// Append.
const subscriberChanBuf = 64

// subscriber wires one live consumer of session events. Writes on ch are
// non-blocking — a full channel increments drops and discards the event.
type subscriber struct {
	ch        chan pipeline.SessionEvent
	drops     uint64 // atomic; reads via LoadDrops
	closeOnce sync.Once
}

// closeCh closes ch at most once. Called by the cancel func returned from
// Subscribe and by Store.Close during shutdown; whichever runs first wins.
func (s *subscriber) closeCh() {
	s.closeOnce.Do(func() { close(s.ch) })
}

// LoadDrops returns the total events dropped for this subscriber since Subscribe.
func (s *subscriber) LoadDrops() uint64 {
	return atomic.LoadUint64(&s.drops)
}

// Subscribe registers a listener for session events written after the call.
// The returned channel is buffered (64). Events are sent non-blockingly; if
// the consumer falls behind, events are dropped — the caller can observe the
// drop count via the returned Subscription's Drops method. Always call the
// cancel func to remove the subscriber and close the channel.
func (s *Store) Subscribe() (*Subscription, func()) {
	sub := &subscriber{ch: make(chan pipeline.SessionEvent, subscriberChanBuf)}

	s.mu.Lock()
	s.subscribers = append(s.subscribers, sub)
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		for i, existing := range s.subscribers {
			if existing == sub {
				s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		sub.closeCh()
	}
	return &Subscription{sub: sub}, cancel
}

// Subscription is the consumer-facing handle returned by Store.Subscribe.
type Subscription struct {
	sub *subscriber
}

// Events returns the receive-only channel of session events.
func (s *Subscription) Events() <-chan pipeline.SessionEvent {
	return s.sub.ch
}

// Drops returns the cumulative event drops due to a full channel.
func (s *Subscription) Drops() uint64 {
	return s.sub.LoadDrops()
}

// New creates a Store with the given TTL, per-session event limit, and max
// concurrent sessions. A background goroutine runs cleanup every TTL/2.
// Call Close() during graceful shutdown to stop the background goroutine.
func New(ttl time.Duration, maxEvents int, maxSessions int) *Store {
	s := &Store{
		sessions:    make(map[string]*entry),
		ttl:         ttl,
		maxEvents:   maxEvents,
		maxSessions: maxSessions,
		stop:        make(chan struct{}),
	}
	go s.backgroundCleanup()
	return s
}

// Close stops the background cleanup goroutine and closes every subscriber's
// channel so consumers unblock cleanly on shutdown. Safe to call multiple times.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)

		s.mu.Lock()
		subs := s.subscribers
		s.subscribers = nil
		s.mu.Unlock()

		for _, sub := range subs {
			sub.closeCh()
		}
	})
}

func (s *Store) backgroundCleanup() {
	interval := s.ttl / 2
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.Cleanup()
		}
	}
}

// Append adds an event to the named session. Creates the session if it
// doesn't exist. Updates activeID to this session. Evicts the oldest event
// if the session exceeds maxEvents.
func (s *Store) Append(sessionID string, event pipeline.SessionEvent) {
	if len(sessionID) > maxSessionIDLen {
		sessionID = sessionID[:maxSessionIDLen]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	sess, ok := s.sessions[sessionID]
	if !ok {
		sess = &entry{
			ID:        sessionID,
			CreatedAt: now,
		}
		s.sessions[sessionID] = sess
	}

	// Stamp the bucket ID so downstream consumers can attribute the event
	// without needing to know which session it was appended to — critical for
	// outbound events that have no protocol-native session field.
	event.SessionID = sessionID

	sess.Events = append(sess.Events, event)
	sess.UpdatedAt = now
	s.activeID = sessionID

	logAppended(sessionID, &event)
	s.publishLocked(event)

	if s.maxEvents > 0 && len(sess.Events) > s.maxEvents {
		sess.Events = trimEventsPinIntent(sess.Events, s.maxEvents)
	}

	// Evict oldest session if cap is exceeded.
	if s.maxSessions > 0 && len(s.sessions) > s.maxSessions {
		s.evictOldestLocked()
	}
}

// isIntentEvent matches the SessionView.LastIntent predicate: an
// inbound A2A request event. IBAC and any future intent-aware
// guardrail call LastIntent and need the most recent such event to
// survive FIFO eviction. Defined here so the eviction policy and
// the consumer view can never drift apart.
func isIntentEvent(e pipeline.SessionEvent) bool {
	return e.Direction == pipeline.Inbound &&
		e.Phase == pipeline.SessionRequest &&
		e.A2A != nil
}

// trimEventsPinIntent reduces events to len <= maxEvents while
// preserving the most-recent intent event, even when that intent
// sits in the prefix that FIFO would normally evict. All other
// events evict in chronological order; the protected intent stays
// at its original index, leaving a temporal gap between it and the
// first non-evicted event after it. That gap is visible in
// /v1/sessions and abctl as a discontinuity in the timeline, which
// is the right shape: the intent's append time is preserved
// (consumers can correlate against the inbound request's wall-clock
// timestamp) and chronological order across surviving events stays
// monotonic.
//
// Why pin only the MOST-RECENT intent: a session can carry several
// user turns ("solve this", then "now do that") and only the latest
// is what IBAC aligns subsequent tool calls against. Pinning all
// intents would let stale ones pile up under pathological loads
// (slow turns + huge fan-out) and starve the buffer.
//
// Pathological case — the buffer is mostly intents, exceeds
// maxEvents, and only the latest is pinned: older intents evict via
// normal FIFO. There is no scenario where this returns more than
// maxEvents events.
//
// Caller guarantees len(events) > maxEvents and maxEvents > 0;
// otherwise this is a no-op shape (returns events unchanged).
func trimEventsPinIntent(events []pipeline.SessionEvent, maxEvents int) []pipeline.SessionEvent {
	if maxEvents <= 0 || len(events) <= maxEvents {
		return events
	}
	excess := len(events) - maxEvents

	// Locate the most-recent intent. If it sits in the keep window
	// (index >= excess) it survives a plain FIFO trim — fast path.
	intentIdx := -1
	for i := len(events) - 1; i >= 0; i-- {
		if isIntentEvent(events[i]) {
			intentIdx = i
			break
		}
	}
	if intentIdx == -1 || intentIdx >= excess {
		// No protected event in the eviction prefix; FIFO trim.
		trimmed := make([]pipeline.SessionEvent, maxEvents)
		copy(trimmed, events[excess:])
		return trimmed
	}

	// Protected intent is in the eviction prefix. Build the result
	// as: [pinned intent] ++ [last maxEvents-1 non-intent-prefix
	// events]. We keep the intent at the front so its original
	// timestamp is preserved AND the resulting slice stays
	// chronologically ordered (intent.At < kept-tail.At by construction —
	// it's the oldest surviving event).
	out := make([]pipeline.SessionEvent, 0, maxEvents)
	out = append(out, events[intentIdx])
	tailStart := len(events) - (maxEvents - 1)
	out = append(out, events[tailStart:]...)
	return out
}

// View returns a read-only snapshot of the session's events.
// Returns nil if the session doesn't exist or is expired.
func (s *Store) View(sessionID string) *pipeline.SessionView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}
	if s.isExpired(sess, time.Now()) {
		return nil
	}

	events := make([]pipeline.SessionEvent, len(sess.Events))
	copy(events, sess.Events)
	return &pipeline.SessionView{ID: sessionID, Events: events}
}

// SessionSummary is a metadata-only view of a session, suitable for list
// endpoints that shouldn't copy the full event backlog.
type SessionSummary struct {
	ID          string    `json:"id"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	EventCount  int       `json:"eventCount"`
	TotalTokens int       `json:"totalTokens,omitempty"` // sum of Inference.TotalTokens across response events
	Active      bool      `json:"active"`                // true if this is the most recently updated session
}

// ListSessions returns summaries for every non-expired session. Order is
// UpdatedAt descending (most recent first). Safe for concurrent use.
func (s *Store) ListSessions() []SessionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	out := make([]SessionSummary, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) {
			continue
		}
		out = append(out, SessionSummary{
			ID:          id,
			CreatedAt:   sess.CreatedAt,
			UpdatedAt:   sess.UpdatedAt,
			EventCount:  len(sess.Events),
			TotalTokens: sumTokens(sess.Events),
			Active:      id == s.activeID,
		})
	}
	// Most recently updated first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// sumTokens aggregates Inference.TotalTokens across all response events
// in a session. Used by ListSessions so the summary endpoint can report a
// per-session cost without the caller loading every event first.
func sumTokens(events []pipeline.SessionEvent) int {
	var total int
	for i := range events {
		if events[i].Phase != pipeline.SessionResponse {
			continue
		}
		if events[i].Inference == nil {
			continue
		}
		total += events[i].Inference.TotalTokens
	}
	return total
}

// ActiveSession returns the most recently updated session ID.
// Used for outbound correlation when no explicit session ID is available.
func (s *Store) ActiveSession() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.activeID == "" {
		return ""
	}
	sess, ok := s.sessions[s.activeID]
	if !ok || s.isExpired(sess, time.Now()) {
		return ""
	}
	return s.activeID
}

// Rekey renames a session from oldID to newID, preserving all events.
// Used to merge the bootstrap "default" session into the server-assigned
// contextId after the backend response reveals it, so events recorded
// during the request phase (under "default") and subsequent turns (under
// the real contextId) land in the same bucket.
//
// Safe to call when oldID does not exist (no-op) or newID already exists
// (no-op — preserves the existing newID entry). If oldID was the active
// session, activeID is updated to newID.
//
// Assumes single-tenant, no concurrent conversations per pod. In a
// multi-tenant deployment two in-flight first-turn requests could both
// land under "default"; rekeying the first to arrive would strand the
// second's events. Call sites are expected to guard against that.
func (s *Store) Rekey(oldID, newID string) {
	if oldID == newID || oldID == "" || newID == "" {
		return
	}
	if len(newID) > maxSessionIDLen {
		// Two long IDs sharing a prefix would collide on the same truncated key,
		// silently turning the second Rekey into a no-op. Log so that's diagnosable.
		slog.Warn("session: newID truncated for rekey", "origLen", len(newID), "maxLen", maxSessionIDLen)
		newID = newID[:maxSessionIDLen]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[oldID]
	if !ok {
		return
	}
	if _, exists := s.sessions[newID]; exists {
		return
	}

	sess.ID = newID
	s.sessions[newID] = sess
	delete(s.sessions, oldID)
	if s.activeID == oldID {
		s.activeID = newID
	}
	// Retrofit already-recorded events with the new session ID so snapshot
	// reads stay consistent with live-streamed events after a rekey.
	for i := range sess.Events {
		sess.Events[i].SessionID = newID
	}
}

// Cleanup removes expired sessions. Safe for concurrent use.
func (s *Store) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
}

func (s *Store) cleanupLocked(now time.Time) {
	for id, sess := range s.sessions {
		if s.isExpired(sess, now) {
			delete(s.sessions, id)
			if s.activeID == id {
				s.activeID = ""
			}
		}
	}
}

func (s *Store) evictOldestLocked() {
	var oldestID string
	var oldestTime time.Time
	for id, sess := range s.sessions {
		if id == s.activeID {
			continue
		}
		if oldestID == "" || sess.UpdatedAt.Before(oldestTime) {
			oldestID = id
			oldestTime = sess.UpdatedAt
		}
	}
	if oldestID == "" {
		// All sessions are the active session — evict it as last resort.
		oldestID = s.activeID
		s.activeID = ""
	}
	if oldestID != "" {
		delete(s.sessions, oldestID)
	}
}

func (s *Store) isExpired(sess *entry, now time.Time) bool {
	return now.Sub(sess.UpdatedAt) > s.ttl
}

// publishLocked fans out event to every current subscriber. Must be called
// with s.mu held. Sends are non-blocking — a full channel increments the
// subscriber's drop counter and discards the event.
//
// Performance note: fan-out runs under s.mu, which serializes Append on the
// subscriber list length. This is fine for the debug API's expected load
// (O(1) live consumers: one abctl instance + maybe a curl tail). If the
// store ever needs to support many concurrent subscribers, refactor this
// to an unlocked broadcast (snapshot the slice under the lock, send outside).
func (s *Store) publishLocked(event pipeline.SessionEvent) {
	for _, sub := range s.subscribers {
		select {
		case sub.ch <- event:
		default:
			atomic.AddUint64(&sub.drops, 1)
		}
	}
}

// logAppended emits a structured DEBUG line so operators can observe session
// state evolution. Fields are chosen to cover the data captured by all four
// record helpers — extension payloads themselves are intentionally omitted
// since the parsers already log them.
func logAppended(sessionID string, e *pipeline.SessionEvent) {
	attrs := []any{
		"sessionId", sessionID,
		"direction", e.Direction.String(),
		"phase", e.Phase.String(),
	}
	if e.Host != "" {
		attrs = append(attrs, "host", e.Host)
	}
	if e.StatusCode != 0 {
		attrs = append(attrs, "status", e.StatusCode)
	}
	if e.Duration != 0 {
		attrs = append(attrs, "durationMs", e.Duration.Milliseconds())
	}
	if e.Identity != nil {
		if e.Identity.Subject != "" {
			attrs = append(attrs, "subject", e.Identity.Subject)
		}
		if e.Identity.ClientID != "" {
			attrs = append(attrs, "clientID", e.Identity.ClientID)
		}
		if e.Identity.AgentID != "" {
			attrs = append(attrs, "agent", e.Identity.AgentID)
		}
		if n := len(e.Identity.Scopes); n > 0 {
			attrs = append(attrs, "scopes", n)
		}
	}
	switch {
	case e.A2A != nil:
		attrs = append(attrs, "proto", "a2a", "method", e.A2A.Method)
	case e.MCP != nil:
		attrs = append(attrs, "proto", "mcp", "method", e.MCP.Method)
	case e.Inference != nil:
		attrs = append(attrs, "proto", "inference", "model", e.Inference.Model)
	}
	if e.Error != nil {
		attrs = append(attrs, "errorKind", e.Error.Kind)
	}
	slog.Debug("session: event appended", attrs...)
}


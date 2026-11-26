package contextguru

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	cgstore "github.com/rossoctl/context-guru/store"
)

// liveInstances is every plugin instance between Init and Shutdown. A config reload builds a new
// instance, and the old one serves its in-flight requests until it drains: its memory holds the
// same sessions, and its Shutdown flush would write them back to the shared database. So an erase
// reaches every live instance, not only the one in the pipeline now.
var (
	liveMu        sync.Mutex
	liveInstances = map[*ContextGuru]struct{}{}
)

func registerLive(p *ContextGuru) {
	liveMu.Lock()
	liveInstances[p] = struct{}{}
	liveMu.Unlock()
}

func unregisterLive(p *ContextGuru) {
	liveMu.Lock()
	delete(liveInstances, p)
	liveMu.Unlock()
}

// eraseTargets is every live instance, and p itself.
func eraseTargets(p *ContextGuru) []*ContextGuru {
	liveMu.Lock()
	defer liveMu.Unlock()
	out := []*ContextGuru{p}
	for q := range liveInstances {
		if q != p {
			out = append(out, q)
		}
	}
	return out
}

// EraseSession removes what the plugin holds for one Cortex session (pipeline.SessionEraser), in
// every live instance (liveInstances). It removes:
//   - the session state, every thread: in memory, in the writes not yet flushed, and in
//     context-guru-state.db (store.Memory.EraseSession), with the session's rewind payloads that
//     no other session owns;
//   - the last forwarded body (sentStash);
//   - the keep-alive entries, which hold the masked body a ping resends (Keeper.RetireSession).
//
// It leaves the thread tracker (hashes and counts, no text), the incoming-model credential holds
// (shared by every session on one credential) and the engine's content-keyed results, which name
// no session. DELETE /v1/sessions (EraseAllSessions) removes the results too.
func (p *ContextGuru) EraseSession(_ context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	var errs []error
	for _, q := range eraseTargets(p) {
		if mem, ok := q.store.(*cgstore.Memory); ok {
			if _, err := mem.EraseSession(sessionID); err != nil {
				errs = append(errs, fmt.Errorf("session state: %w", err))
			}
		}
		q.stash.forget(sessionID)
		if q.keeper != nil {
			q.keeper.RetireSession("", sessionID)
		}
	}
	return errors.Join(errs...)
}

// EraseAllSessions is EraseSession for every session: it empties the session state (memory and
// context-guru-state.db, the content-keyed results included), the forwarded bodies and the
// keep-alive entries, in every live instance.
func (p *ContextGuru) EraseAllSessions(context.Context) error {
	var errs []error
	for _, q := range eraseTargets(p) {
		if mem, ok := q.store.(*cgstore.Memory); ok {
			if _, err := mem.EraseAll(); err != nil {
				errs = append(errs, fmt.Errorf("session state: %w", err))
			}
		}
		q.stash.forgetAll()
		if q.keeper != nil {
			q.keeper.Forget("") // tenant "": one process, one deployment (keeperKey)
		}
	}
	return errors.Join(errs...)
}

// forget drops the session's entries: every thread key, thread.Key(session, thread), is the
// session id itself or the id, NUL, and the thread.
func (s *sentStash) forget(session string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.m {
		if k == session || strings.HasPrefix(k, session+"\x00") {
			s.bytes -= len(e.forwarded) + len(e.preBody)
			delete(s.m, k)
		}
	}
}

// forgetAll drops every entry.
func (s *sentStash) forgetAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.m)
	s.bytes = 0
}

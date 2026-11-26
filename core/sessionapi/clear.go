package sessionapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/pipeline"
)

// clearWait bounds how long an erase — DELETE /v1/sessions or DELETE /v1/sessions/{id} — waits
// for the archive (AwaitClear or AwaitDelete). The memory half is done before the wait starts, so
// a timeout costs only the report.
const clearWait = 10 * time.Second

// clearResponse is DELETE /v1/sessions' answer: what went from memory, and what went from disk.
type clearResponse struct {
	Sessions         int    `json:"sessions"`
	ArchivedSessions int    `json:"archivedSessions"`
	Bytes            int64  `json:"bytes"`
	ArchiveError     string `json:"archiveError,omitempty"`
	// PluginErrors lists, as "<plugin>: <error>", each plugin that failed to erase its own data
	// (pipeline.SessionEraser). The rest of the erase stands.
	PluginErrors []string `json:"pluginErrors,omitempty"`
}

// erasePlugins calls erase on every plugin of the live pipelines that keeps session data of its
// own (pipeline.SessionEraser), after the store and the archive are cleared, and returns the
// failures. A plugin in both pipelines is called twice: an erase is idempotent. Every plugin is
// called, whatever an earlier one returned.
func (s *Server) erasePlugins(r *http.Request, erase func(context.Context, pipeline.SessionEraser) error) []string {
	ctx, cancel := context.WithTimeout(r.Context(), clearWait)
	defer cancel()
	var failed []string
	for _, h := range []*pipeline.Holder{s.inbound, s.outbound} {
		if h == nil {
			continue
		}
		for _, p := range h.Plugins() {
			e, ok := p.(pipeline.SessionEraser)
			if !ok {
				continue
			}
			if err := erase(ctx, e); err != nil {
				failed = append(failed, p.Name()+": "+err.Error())
			}
		}
	}
	return failed
}

// WithClearAllowed lets DELETE /v1/sessions clear every session and DELETE /v1/sessions/{id}
// erase one. The binary passes listener.bind_loopback_only, which a laptop install sets: a proxy
// reachable only from its own machine has one user, and history is theirs to erase. Without it
// both endpoints refuse.
func WithClearAllowed(allowed bool) Option {
	return func(s *Server) { s.allowClear = allowed }
}

// handleClear answers DELETE /v1/sessions: every session goes from memory and, where the archive
// runs, from disk, and each plugin that keeps session data of its own erases all of it
// (pipeline.SessionEraser). The cost ledger stays — it holds spend totals and no content.
//
// THE FIRST REQUEST ON THIS API THAT CHANGES ANYTHING, and the API is unauthenticated, so what it
// guards against is a browser: a page the user visits that targets the loopback port, directly
// or through a DNS name rebound to 127.0.0.1. Four guards, each closing one route:
//
//  1. DELETE only, by the route pattern. A cross-site DELETE needs a CORS preflight, which this
//     server never approves; a simple cross-site POST needs none, which is why this is not POST.
//  2. The Host must be a loopback name. A rebound name reaches the port with its own name in Host.
//  3. No Origin header. Browsers send one, on every cross-origin request and every DELETE;
//     agentop and curl do not.
//  4. Only when allowed, which is only on a loopback-only laptop install. No new setting: a
//     cluster sidecar's API is reached through port-forwards and shared by whoever can reach it.
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if reason := s.clearRefusal(r); reason != "" {
		s.refuse(w, reason, r.RemoteAddr)
		return
	}
	body := clearResponse{Sessions: s.store.Clear()}
	status := http.StatusOK
	if s.archive != nil {
		res, err := s.archive.AwaitClear(clearWait)
		body.ArchivedSessions, body.Bytes = res.Sessions, res.Bytes
		if err != nil {
			body.ArchiveError = err.Error()
			status = http.StatusInternalServerError
		}
	}
	s.timelines.clear()
	body.PluginErrors = s.erasePlugins(r, func(ctx context.Context, e pipeline.SessionEraser) error {
		return e.EraseAllSessions(ctx)
	})
	if len(body.PluginErrors) > 0 {
		status = http.StatusInternalServerError
	}
	slog.Warn("sessionapi: cleared every session", "sessions", body.Sessions,
		"archivedSessions", body.ArchivedSessions, "bytes", body.Bytes, "archiveError", body.ArchiveError,
		"pluginErrors", body.PluginErrors, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("sessionapi: clear encode failed", "error", err)
	}
}

// refuse answers a refused erase, of everything or of one session: a 403 carrying why, logged at
// most once per clearRefusalLogEvery.
func (s *Server) refuse(w http.ResponseWriter, reason, remote string) {
	s.logClearRefusal(reason, remote)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: reason})
}

// handleDelete answers DELETE /v1/sessions/{id}: one session goes from memory and, where the
// archive runs, from disk — a session only the archive holds too — and each plugin that keeps
// session data of its own erases that session's (pipeline.SessionEraser). Guarded exactly as
// handleClear is, for the reasons given there, and the cost ledger stays here as well. 404 when
// neither the store nor the archive held the id; the plugins erase it even then.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if reason := s.clearRefusal(r); reason != "" {
		s.refuse(w, reason, r.RemoteAddr)
		return
	}
	id := r.PathValue("id")
	var body clearResponse
	if s.store.Delete(id) {
		body.Sessions = 1
	}
	status := http.StatusOK
	if s.archive != nil {
		res, err := s.archive.AwaitDelete(id, clearWait)
		body.ArchivedSessions, body.Bytes = res.Sessions, res.Bytes
		if err != nil {
			body.ArchiveError = err.Error()
			status = http.StatusInternalServerError
		}
	}
	s.timelines.forget(id)
	// The plugins are asked on every request, before the 404 check: a plugin's own file can hold a
	// session that neither the store nor the archive holds (after a restart, with the archive off).
	body.PluginErrors = s.erasePlugins(r, func(ctx context.Context, e pipeline.SessionEraser) error {
		return e.EraseSession(ctx, id)
	})
	if len(body.PluginErrors) > 0 {
		status = http.StatusInternalServerError
	}
	if status == http.StatusOK && body.Sessions == 0 && body.ArchivedSessions == 0 {
		http.NotFound(w, r)
		return
	}
	// INFO, where a clear of everything is a WARN: erasing one agent's sessions is one request
	// per session, and a WARN each would bury the log under one action.
	slog.Info("sessionapi: erased a session", "session", id, "inMemory", body.Sessions == 1,
		"archivedSessions", body.ArchivedSessions, "bytes", body.Bytes, "archiveError", body.ArchiveError,
		"pluginErrors", body.PluginErrors, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("sessionapi: delete encode failed", "error", err)
	}
}

// clearRefusalLogEvery bounds how often a refused clear is logged.
const clearRefusalLogEvery = time.Minute

// logClearRefusal logs a refused clear at most once per clearRefusalLogEvery, with the running
// count. Every refusal is worth knowing about — something on this machine tried — but not one line
// each: a page doing DNS rebinding reaches this endpoint same-origin, so no preflight stops it from
// looping, and a WARN per request would let any web page grow ~/.cortex/proxy.log without bound.
func (s *Server) logClearRefusal(reason, remote string) {
	n := s.clearRefusals.Add(1)
	now := time.Now().UnixNano()
	last := s.lastRefusalLog.Load()
	if (last != 0 && now-last < int64(clearRefusalLogEvery)) || !s.lastRefusalLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("sessionapi: refused to erase sessions", "reason", reason, "remote", remote, "refusedTotal", n)
}

// clearRefusal is why r may not clear, or "" when it may. The method is already DELETE: the
// route pattern answers anything else with 405.
func (s *Server) clearRefusal(r *http.Request) string {
	switch {
	case !httpx.LoopbackHost(r.Host):
		return "erasing is accepted only for a loopback Host (localhost, 127.0.0.1 or [::1])"
	case r.Header.Get("Origin") != "":
		return "erasing is refused from a browser: the request carries an Origin header"
	case !s.allowClear:
		return "this Cortex does not accept an erase: it is not bound to loopback only (listener.bind_loopback_only)"
	}
	return ""
}

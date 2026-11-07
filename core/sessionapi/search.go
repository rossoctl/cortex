package sessionapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unsafe"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// GET /v1/sessions/{id}/search?q=<text> (#1680) answers the seqs of the session's events whose
// detail view shows q in a part that view=summary drops: the bodies, and the capture where the
// archive holds one. agentop holds the summaries and checks them itself. So between the two, a
// row matches when its detail view shows q. See docs/session-api.md.

// searchResponse is the route's answer. Seqs is ascending, and LastSeq is the newest seq the
// pass read, so a client can tell when the session has grown since.
type searchResponse struct {
	Seqs          []uint64 `json:"seqs"`
	LastSeq       uint64   `json:"lastSeq"`
	SchemaVersion int      `json:"schemaVersion"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	id, q := r.PathValue("id"), r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, `{"error":"q is required"}`, http.StatusBadRequest)
		return
	}
	ctx, m := r.Context(), newMatcher(q)
	var seqs []uint64
	var last uint64
	found := false
	// View copies the events under the lock and releases it, so the check runs outside it.
	if view := s.store.View(id); view != nil {
		found = true
		for i := range view.Events {
			if ctx.Err() != nil {
				return
			}
			e := &view.Events[i]
			last = max(last, e.Seq)
			if m.event(e) {
				seqs = append(seqs, e.Seq)
			}
		}
	}
	if s.archive != nil {
		disk, diskLast, ok, err := s.archive.Find(ctx, id, m.event, m.text)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Part of the history was not read. An answer without it could say "no match"
			// where there is one.
			slog.Debug("sessionapi: archive search failed", "error", err, "sessionID", id)
			http.Error(w, `{"error":"archive read failed"}`, http.StatusInternalServerError)
			return
		}
		found = found || ok
		seqs, last = append(seqs, disk...), max(last, diskLast)
	}
	if !found {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	if seqs == nil {
		seqs = []uint64{}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(searchResponse{Seqs: seqs, LastSeq: last, SchemaVersion: SchemaVersion}); err != nil {
		slog.Debug("sessionapi: search encode failed", "error", err, "sessionID", id)
	}
}

// matcher is one search: q folded, and what each long string gave. The store and the archive
// reader share one copy of a long string between the events that repeat it, so the matcher folds
// each copy one time, keyed by its address and length. A string is not hashed for the key,
// because hashing it costs as much as the check. A key holds its string in memory until the
// search ends.
type matcher struct {
	q    string
	seen map[strKey]bool
	json map[strKey]bool
	keys map[string]bool
}

type strKey struct {
	p *byte
	n int
}

func newMatcher(q string) *matcher {
	return &matcher{q: strings.Map(unicode.ToLower, q),
		seen: map[strKey]bool{}, json: map[strKey]bool{}, keys: map[string]bool{}}
}

// text reports whether s holds the query, folded as agentop's find.Contains folds: rune by rune,
// with unicode.ToLower.
func (m *matcher) text(s string) bool {
	if len(s) < session.InternMinLen {
		return strings.Contains(strings.Map(unicode.ToLower, s), m.q)
	}
	k := strKey{unsafe.StringData(s), len(s)}
	r, ok := m.seen[k]
	if !ok {
		r = strings.Contains(strings.Map(unicode.ToLower, s), m.q)
		m.seen[k] = r
	}
	return r
}

// key reports whether a key name holds the query. The same few names repeat in every event.
func (m *matcher) key(k string) bool {
	r, ok := m.keys[k]
	if !ok {
		r = m.text(k)
		m.keys[k] = r
	}
	return r
}

// field reports whether a string field, as the detail view shows it, holds the query in its key
// or its value. An empty value is omitted from the JSON, so the view shows neither.
func (m *matcher) field(key, v string) bool { return v != "" && (m.key(key) || m.text(v)) }

// event reports whether a field that summarizeEvent drops holds the query, where the detail view
// shows that field for e's phase. agentop checks the rest of the event in the summary it holds.
//
// The key names are the ones the detail view shows: a list's own name, and the names inside each
// entry. The view nests each history tool call under its message as toolCalls, so the name
// historyToolCalls is not matched.
func (m *matcher) event(e *model.SessionEvent) bool {
	resp := e.Phase == model.SessionResponse
	if inf := e.Inference; inf != nil {
		switch {
		case detailShows("inference", resp, "messages") && m.messages(inf.Messages),
			detailShows("inference", resp, "tools") && m.tools(inf.Tools),
			detailShows("inference", resp, "toolResults") && m.results(inf.ToolResults),
			detailShows("inference", resp, "historyToolCalls") && m.calls(inf.HistoryToolCalls),
			detailShows("inference", resp, "toolCalls") && m.calls(inf.ToolCalls):
			return true
		}
	}
	if p := e.MCP; p != nil {
		if detailShows("mcp", resp, "params") && len(p.Params) > 0 && (m.key("params") || m.value(p.Params)) {
			return true
		}
		if detailShows("mcp", resp, "result") && len(p.Result) > 0 && (m.key("result") || m.value(p.Result)) {
			return true
		}
	}
	a := e.A2A
	return a != nil && detailShows("a2a", resp, "artifact") && m.field("artifact", a.Artifact)
}

func (m *matcher) messages(ms []model.InferenceMessage) bool {
	if len(ms) == 0 {
		return false
	}
	if m.key("messages") {
		return true
	}
	for i := range ms {
		x := &ms[i]
		if m.field("role", x.Role) || m.field("content", x.Content) || m.field("toolUseId", x.ToolUseID) ||
			m.field("toolCallNames", x.ToolCallNames) || m.field("toolUseIds", x.ToolUseIDs) ||
			(x.ContentBytes != 0 && m.field("contentBytes", strconv.Itoa(x.ContentBytes))) {
			return true
		}
	}
	return false
}

func (m *matcher) tools(ts []model.InferenceTool) bool {
	if len(ts) == 0 {
		return false
	}
	if m.key("tools") {
		return true
	}
	for i := range ts {
		x := &ts[i]
		if m.field("name", x.Name) || m.field("description", x.Description) ||
			(x.Parameters != "" && (m.key("parameters") || m.schema(string(x.Parameters)))) {
			return true
		}
	}
	return false
}

// calls is a list of tool calls: the response's own, or a request's history, which the detail
// view shows under each message as toolCalls.
func (m *matcher) calls(cs []model.InferenceToolCall) bool {
	if len(cs) == 0 {
		return false
	}
	if m.key("toolCalls") {
		return true
	}
	for i := range cs {
		x := &cs[i]
		if m.field("id", x.ID) || m.field("name", x.Name) || m.field("arguments", x.Arguments) {
			return true
		}
	}
	return false
}

func (m *matcher) results(rs []model.InferenceToolResult) bool {
	if len(rs) == 0 {
		return false
	}
	if m.key("toolResults") {
		return true
	}
	for i := range rs {
		x := &rs[i]
		if m.field("toolUseId", x.ToolUseID) || m.field("content", x.Content) ||
			(x.IsError && (m.key("isError") || m.key("true"))) {
			return true
		}
	}
	return false
}

// schema is a tool's parameters: JSON, which the detail tree shows as keys and values. Each
// distinct schema is walked one time.
func (m *matcher) schema(raw string) bool {
	k := strKey{unsafe.StringData(raw), len(raw)}
	r, ok := m.json[k]
	if !ok {
		r = archive.MatchJSON(raw, m.text)
		m.json[k] = r
	}
	return r
}

// value is a decoded JSON value (MCP params and results): its keys and values, as the detail
// tree writes them.
func (m *matcher) value(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if m.key(k) || m.value(e) {
				return true
			}
		}
		return false
	case []any:
		for _, e := range x {
			if m.value(e) {
				return true
			}
		}
		return false
	case string:
		return m.text(x)
	}
	b, err := json.Marshal(v) // a number, a bool or null, as JSON writes it
	return err == nil && m.text(string(b))
}

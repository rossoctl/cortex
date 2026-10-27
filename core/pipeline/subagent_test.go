package pipeline

import (
	"net/http"
	"strings"
	"testing"
)

// The header as Claude Code sends it, and the value it carries.
func TestSubagent_ReadsTheHeader(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	c.Headers.Set(ClaudeCodeAgentIDHeader, "a1a2e36cb5946f31c")
	if got := c.Subagent(); got != "a1a2e36cb5946f31c" {
		t.Errorf("Subagent() = %q, want a1a2e36cb5946f31c", got)
	}
}

// Claude Code sends the name lowercased — "x-claude-code-agent-id" — and every listener
// stores it canonicalized: net/http's parser on the proxies, http.Header.Set in
// ext_proc's headerMapToHTTP. This pins that the read side agrees with both.
func TestSubagent_HeaderNameIsCaseInsensitive(t *testing.T) {
	h := http.Header{}
	h.Set("x-claude-code-agent-id", "a1a2e36cb5946f31c")
	c := &Context{Headers: h}
	if got := c.Subagent(); got != "a1a2e36cb5946f31c" {
		t.Errorf("Subagent() = %q, want a1a2e36cb5946f31c", got)
	}
}

// The main conversation, every other client, and a context with no headers at all.
// Pinned because the field is omitempty: a non-empty answer here would put a key on
// nearly every row.
func TestSubagent_AbsentHeaderIsEmpty(t *testing.T) {
	for name, h := range map[string]http.Header{
		"nil headers": nil,
		"no header":   {},
		"empty value": {ClaudeCodeAgentIDHeader: []string{""}},
		"only spaces": {ClaudeCodeAgentIDHeader: []string{"   "}},
	} {
		c := &Context{Headers: h}
		if got := c.Subagent(); got != "" {
			t.Errorf("%s: Subagent() = %q, want empty", name, got)
		}
	}
}

// HTTP's optional whitespace around a value — SP and HTAB, RFC 9110 §5.5 — is not part
// of the id; net/http trims it on the forward proxy, but ext_proc hands bytes over as
// sent. Both kinds, at both ends, so a trim narrowed to spaces alone fails here.
func TestSubagent_TrimsSurroundingSpace(t *testing.T) {
	for name, v := range map[string]string{
		"spaces": " a1a2e36cb5946f31c ",
		"tabs":   "\ta1a2e36cb5946f31c\t",
		"mixed":  " \t a1a2e36cb5946f31c \t ",
	} {
		c := &Context{Headers: http.Header{ClaudeCodeAgentIDHeader: []string{v}}}
		if got := c.Subagent(); got != "a1a2e36cb5946f31c" {
			t.Errorf("%s: Subagent() = %q, want the id without its padding", name, got)
		}
	}
}

// A value that cannot be recorded is REFUSED, not cleaned up: the id is a join key
// against agent-<id>.meta.json, and a sanitised one would match nothing while looking
// like an answer. Each case is one of hasControlRunes' clauses or the length bound.
func TestSubagent_RefusesUnusableValues(t *testing.T) {
	for name, v := range map[string]string{
		"C0 escape":       "a1a2\x1b[2Je36cb",
		"newline":         "a1a2\ne36cb",
		"DEL":             "a1a2\x7fe36cb",
		"C1 CSI":          "a1a2\u009b2Je36cb",
		"bidi override":   "a1a2\u202ee36cb",
		"zero-width join": "a1a2\u200de36cb",
		"invalid UTF-8":   "a1a2\xffe36cb",
		"over the cap":    strings.Repeat("a", maxClientLen+1),
		// AT THE EDGES, where a trim runs first. strings.TrimSpace would strip every one of
		// these and hand validSubagentID a clean id, so the value would be recorded cleaned
		// up — the opposite of this function's rule. Only HTTP's optional whitespace (SP,
		// HTAB) is padding; anything else at either end is part of the value and refuses it.
		"trailing newline": "a1a2e36cb5946f31c\n",
		"leading newline":  "\na1a2e36cb5946f31c",
		"trailing CR":      "a1a2e36cb5946f31c\r",
		"trailing CRLF":    "a1a2e36cb5946f31c\r\n",
		"leading VT":       "\va1a2e36cb5946f31c",
		"trailing FF":      "a1a2e36cb5946f31c\f",
		"trailing NEL":     "a1a2e36cb5946f31c\u0085",
		"leading NEL":      "\u0085a1a2e36cb5946f31c",
	} {
		c := &Context{Headers: http.Header{ClaudeCodeAgentIDHeader: []string{v}}}
		if got := c.Subagent(); got != "" {
			t.Errorf("%s: Subagent() = %q, want empty", name, got)
		}
	}
}

// The cap is inclusive: an id of exactly maxClientLen bytes is kept whole.
func TestSubagent_KeepsAnIDAtTheCap(t *testing.T) {
	v := strings.Repeat("a", maxClientLen)
	c := &Context{Headers: http.Header{ClaudeCodeAgentIDHeader: []string{v}}}
	if got := c.Subagent(); got != v {
		t.Errorf("Subagent() = %d bytes, want the %d-byte id kept", len(got), len(v))
	}
}

// ResolveClient pins the answer to the header AS THE CLIENT SENT IT, so a plugin that
// rewrites Headers afterwards cannot re-file the row under another subagent. This is
// the same guarantee ClientInfo makes, and the reason ResolveClient resolves both.
func TestSubagent_ResolveClientPinsTheAnswer(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	c.Headers.Set(ClaudeCodeAgentIDHeader, "a1a2e36cb5946f31c")
	c.ResolveClient()

	c.Headers.Set(ClaudeCodeAgentIDHeader, "a-plugin-wrote-this")
	if got := c.Subagent(); got != "a1a2e36cb5946f31c" {
		t.Errorf("after a plugin rewrote the header, Subagent() = %q, want the pinned a1a2e36cb5946f31c", got)
	}

	c.Headers.Del(ClaudeCodeAgentIDHeader)
	if got := c.Subagent(); got != "a1a2e36cb5946f31c" {
		t.Errorf("after a plugin deleted the header, Subagent() = %q, want the pinned a1a2e36cb5946f31c", got)
	}
}

// An absent header pins too: "" is a real answer, so a header a plugin adds later must
// not turn a main-conversation row into a subagent's.
func TestSubagent_ResolveClientPinsAnEmptyAnswer(t *testing.T) {
	c := &Context{Headers: http.Header{}}
	c.ResolveClient()

	c.Headers.Set(ClaudeCodeAgentIDHeader, "a-plugin-wrote-this")
	if got := c.Subagent(); got != "" {
		t.Errorf("Subagent() = %q, want the pinned empty answer", got)
	}
}

// The round trip a consumer depends on: the id reaches the wire under its own key and
// decodes back, and an event that names none carries no key.
func TestSessionEvent_SubagentRoundTrips(t *testing.T) {
	raw, err := (SessionEvent{Phase: SessionRequest, Subagent: "a1a2e36cb5946f31c"}).MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"subagent":"a1a2e36cb5946f31c"`) {
		t.Errorf("wire form = %s, want a subagent key", raw)
	}
	var out SessionEvent
	if err := out.UnmarshalJSON(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Subagent != "a1a2e36cb5946f31c" {
		t.Errorf("Subagent = %q after the round trip, want a1a2e36cb5946f31c", out.Subagent)
	}
}

// A nested subagent sends both headers: its own id and the agent that started it. Each
// lands in its own field — a swap would invert every edge of the tree.
func TestParentSubagent_ReadsTheHeader(t *testing.T) {
	h := http.Header{}
	h.Set("x-claude-code-agent-id", "a21728bb34d27cb12")
	h.Set("x-claude-code-parent-agent-id", "a0974b0b8591dc76b")
	c := &Context{Headers: h}
	if got := c.Subagent(); got != "a21728bb34d27cb12" {
		t.Errorf("Subagent() = %q, want a21728bb34d27cb12", got)
	}
	if got := c.ParentSubagent(); got != "a0974b0b8591dc76b" {
		t.Errorf("ParentSubagent() = %q, want a0974b0b8591dc76b", got)
	}
}

// A subagent the main conversation started — the common case — sends no parent header,
// and that absence is the answer: its parent is the conversation.
func TestParentSubagent_TopLevelSubagentHasNoParent(t *testing.T) {
	c := &Context{Headers: http.Header{ClaudeCodeAgentIDHeader: []string{"a1a2e36cb5946f31c"}}}
	if got := c.ParentSubagent(); got != "" {
		t.Errorf("ParentSubagent() = %q, want empty for a subagent the conversation started", got)
	}
	if got := c.Subagent(); got != "a1a2e36cb5946f31c" {
		t.Errorf("Subagent() = %q, want a1a2e36cb5946f31c", got)
	}
}

// The parent goes through the same refusal the child does, and a refused parent does
// not take the child with it: the two values are checked independently.
func TestParentSubagent_RefusesUnusableValues(t *testing.T) {
	for name, v := range map[string]string{
		"C0 escape":     "a0974\x1b[2Jb0b8",
		"C1 CSI":        "a0974\u009b2Jb0b8",
		"bidi override": "a0974\u202eb0b8",
		"invalid UTF-8": "a0974\xffb0b8",
		"over the cap":  strings.Repeat("a", maxClientLen+1),
		// The parent goes through the same trim; see the child's edge cases.
		"trailing newline": "a0974b0b8591dc76b\n",
		"leading NEL":      "\u0085a0974b0b8591dc76b",
	} {
		c := &Context{Headers: http.Header{
			ClaudeCodeAgentIDHeader:       []string{"a21728bb34d27cb12"},
			ClaudeCodeParentAgentIDHeader: []string{v},
		}}
		if got := c.ParentSubagent(); got != "" {
			t.Errorf("%s: ParentSubagent() = %q, want empty", name, got)
		}
		if got := c.Subagent(); got != "a21728bb34d27cb12" {
			t.Errorf("%s: Subagent() = %q, want the valid child kept beside a refused parent", name, got)
		}
	}
}

// ResolveClient pins the parent with the child, so a plugin cannot re-parent a row.
// Asked through ParentSubagent FIRST on purpose: one memo serves both, and this is what
// fails if ResolveClient ever pins only the child's.
func TestParentSubagent_ResolveClientPinsTheAnswer(t *testing.T) {
	h := http.Header{}
	h.Set(ClaudeCodeAgentIDHeader, "a21728bb34d27cb12")
	h.Set(ClaudeCodeParentAgentIDHeader, "a0974b0b8591dc76b")
	c := &Context{Headers: h}
	c.ResolveClient()

	c.Headers.Set(ClaudeCodeParentAgentIDHeader, "a-plugin-wrote-this")
	if got := c.ParentSubagent(); got != "a0974b0b8591dc76b" {
		t.Errorf("after a plugin rewrote the header, ParentSubagent() = %q, want the pinned a0974b0b8591dc76b", got)
	}
}

func TestSessionEvent_ParentSubagentRoundTrips(t *testing.T) {
	raw, err := (SessionEvent{Phase: SessionRequest, Subagent: "a21728bb34d27cb12", ParentSubagent: "a0974b0b8591dc76b"}).MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"parentSubagent":"a0974b0b8591dc76b"`) {
		t.Errorf("wire form = %s, want a parentSubagent key", raw)
	}
	var out SessionEvent
	if err := out.UnmarshalJSON(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ParentSubagent != "a0974b0b8591dc76b" || out.Subagent != "a21728bb34d27cb12" {
		t.Errorf("after the round trip Subagent/ParentSubagent = %q/%q, want a21728bb34d27cb12/a0974b0b8591dc76b",
			out.Subagent, out.ParentSubagent)
	}
}

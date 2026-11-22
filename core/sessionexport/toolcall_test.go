package sessionexport

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/toolpair"
)

// tcBase is the fixture clock for this file.
var tcBase = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

// toolSession builds one session's events in ASCENDING SEQ ORDER: a main-agent request, a
// response asking for the named tool calls, then a later request carrying their results.
//
// Shaped as the real traffic is — the result arrives on a LATER request, which is the whole
// reason the pairing cannot run inside the streaming walk's callback.
func toolSession(calls []model.InferenceToolCall, results []model.InferenceToolResult) []model.SessionEvent {
	evs := []model.SessionEvent{
		{
			Seq: 1, At: tcBase, Direction: model.Outbound, Phase: model.SessionRequest,
			RequestID: "r1", Client: &model.EventClient{Name: "claude-code", Version: "2.1.270"},
			Inference: &model.InferenceExtension{
				AgentRole: model.AgentRoleMain,
				Messages:  []model.InferenceMessage{{Role: "user", Content: "do it"}},
				Tools:     []model.InferenceTool{{Name: "Bash"}},
			},
		},
		{
			Seq: 2, At: tcBase, Direction: model.Outbound, Phase: model.SessionResponse,
			RequestID: "r1", Duration: 2 * time.Second,
			Client:    &model.EventClient{Name: "claude-code", Version: "2.1.270"},
			Inference: &model.InferenceExtension{AgentRole: model.AgentRoleMain, ToolCalls: calls},
		},
	}
	if results != nil {
		evs = append(evs, model.SessionEvent{
			Seq: 3, At: tcBase.Add(3 * time.Second), Direction: model.Outbound,
			Phase: model.SessionRequest, RequestID: "r2",
			Client: &model.EventClient{Name: "claude-code", Version: "2.1.270"},
			Inference: &model.InferenceExtension{
				AgentRole:   model.AgentRoleMain,
				Messages:    []model.InferenceMessage{{Role: "user", Content: "next"}},
				Tools:       []model.InferenceTool{{Name: "Bash"}},
				ToolResults: results,
			},
		})
	}
	return evs
}

// testKey is a fixed key, so a hashed label is stable across a test run.
func testKey(t *testing.T) ToolNameKey {
	t.Helper()
	k, err := LoadToolNameKey(t.TempDir())
	if err != nil {
		t.Fatalf("LoadToolNameKey: %v", err)
	}
	return k
}

func projectOne(t *testing.T, evs []model.SessionEvent) []ToolCallRecord {
	t.Helper()
	recs, dropped := projectToolCalls(evs, "sess-1", testKey(t))
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 on a small fixture", dropped)
	}
	return recs
}

// --- T8: the tool-use id never reaches a record ---

// TestToolCall_NeverCarriesToolUseID is the join-back guard. The tool-use id is the pairing's
// internal key and the one field that would make a toolCall record joinable to a conversation
// (T8), so it is asserted on the ENCODED BYTES rather than on the struct: a field added later
// that happened to carry it fails here without anyone remembering this test exists.
func TestToolCall_NeverCarriesToolUseID(t *testing.T) {
	const id = "toolu_SECRET_USE_ID_0123456789"
	evs := toolSession(
		[]model.InferenceToolCall{{ID: id, Name: "Bash", Arguments: `{"command":"ls"}`}},
		[]model.InferenceToolResult{{ToolUseID: id, Content: "ok"}},
	)
	recs := projectOne(t, evs)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	var buf bytes.Buffer
	if err := NewToolCallWriter(&buf).WriteToolCall(recs[0]); err != nil {
		t.Fatalf("WriteToolCall: %v", err)
	}
	if strings.Contains(buf.String(), id) {
		t.Errorf("record carries the tool-use id — it is joinable back to a conversation:\n%s", buf.String())
	}
	// The join a consumer gets instead (T34).
	if recs[0].CallSeq == 0 {
		t.Error("callSeq is 0: without it there is no join at all")
	}
}

// --- T6/T7/T9: no content ---

// TestToolCall_CarriesNoContent extends the privacy contract to the new record, planting a secret
// in every content field the PAIRING reads — not only in tool arguments.
//
// THE PAIRING READS MORE CONTENT THAN THE RECORD CARRIES, which is exactly why this must cover
// message text and result text: two of its closing cases are only knowable from message content
// (a <task-notification> says a run ended) and from result content ("Command running in
// background" says the call merely started a job). So a test that planted only in arguments would
// pass while a closing case leaked the text it matched on (spec T9).
func TestToolCall_CarriesNoContent(t *testing.T) {
	const (
		secretArgs   = "SECRET-TC-ARGUMENTS"
		secretResult = "SECRET-TC-RESULT-BODY"
		secretMsg    = "SECRET-TC-MESSAGE-TEXT"
	)
	evs := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash", Arguments: `{"command":"` + secretArgs + `"}`}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: secretResult}},
	)
	// Plant in the message text the returns/notification scan reads.
	for i := range evs {
		if inf := evs[i].Inference; inf != nil {
			for j := range inf.Messages {
				inf.Messages[j].Content += " " + secretMsg
			}
		}
	}
	recs := projectOne(t, evs)
	var buf bytes.Buffer
	w := NewToolCallWriter(&buf)
	for _, r := range recs {
		if err := w.WriteToolCall(r); err != nil {
			t.Fatalf("WriteToolCall: %v", err)
		}
	}
	for _, secret := range []string{secretArgs, secretResult, secretMsg} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("record leaked %q:\n%s", secret, buf.String())
		}
	}
}

// --- T10/T10a/T11: the name rules ---

// TestToolCall_MCPNameNeverVerbatim is the T11 gate. An MCP name is operator-defined and
// namespaced, so it can carry a vendor relationship, an internal system name or a project
// codename — the spec's own worked example is the one used here.
func TestToolCall_MCPNameNeverVerbatim(t *testing.T) {
	const mcpName = "mcp__acme-billing-prod__refund"
	evs := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: mcpName}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	)
	recs := projectOne(t, evs)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.ToolSource != ToolMCP {
		t.Errorf("toolSource = %q, want %q", r.ToolSource, ToolMCP)
	}
	if strings.Contains(r.Tool, "acme") || strings.Contains(r.Tool, "billing") || r.Tool == mcpName {
		t.Errorf("tool = %q: an MCP name must never export verbatim", r.Tool)
	}
	if !strings.HasPrefix(r.Tool, hashPrefix) {
		t.Errorf("tool = %q, want the %q prefix so a consumer can tell a hash from a name",
			r.Tool, hashPrefix)
	}
	// Keyed, not a plain digest: the namespace makes an unkeyed digest reversible by trying
	// candidate vendor names, which is why T11 requires a key.
	other, err := LoadToolNameKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other.label(mcpName, ToolMCP) == r.Tool {
		t.Error("two installs produced the same label: the hash is not keyed")
	}
}

// TestClassifyTool covers the three-way branch, including the one that is easy to get backwards.
func TestClassifyTool(t *testing.T) {
	for _, tc := range []struct {
		name, tool, client string
		want               ToolSource
	}{
		{"builtin claude-code", "Bash", "claude-code", ToolBuiltin},
		{"builtin codex lowercase", "bash", "codex", ToolBuiltin},
		{"mcp prefix", "mcp__x__y", "claude-code", ToolMCP},
		// The cross-client case T10 names: one agent's Bash is another's bash, and neither
		// spelling is normalised into the other.
		{"claude-code spelling under codex", "Bash", "codex", ToolUnknown},
		{"codex spelling under claude-code", "bash", "claude-code", ToolUnknown},
		// T10a: an unknown source hashes, so an unlisted name must NOT read as builtin.
		{"unlisted name", "SomeNewTool", "claude-code", ToolUnknown},
		{"unknown client", "Bash", "", ToolUnknown},
		{"empty name", "", "claude-code", ToolUnknown},
		// A built-in named like an MCP tool is still treated as MCP: mislabelling the
		// operator-defined namespace is the error with a privacy cost.
		{"mcp prefix wins", mcpPrefix + "Bash", "claude-code", ToolMCP},
	} {
		if got := classifyTool(tc.tool, tc.client); got != tc.want {
			t.Errorf("%s: classifyTool(%q, %q) = %q, want %q", tc.name, tc.tool, tc.client, got, tc.want)
		}
	}
}

// TestToolCall_BuiltinNameVerbatim is T10's other half: a built-in name is NOT rewritten — no
// case folding, no canonical spelling — because that would lose which spelling the client sent.
func TestToolCall_BuiltinNameVerbatim(t *testing.T) {
	evs := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	)
	recs := projectOne(t, evs)
	if got := recs[0].Tool; got != "Bash" {
		t.Errorf("tool = %q, want %q verbatim", got, "Bash")
	}
	if recs[0].ToolSource != ToolBuiltin {
		t.Errorf("toolSource = %q, want %q", recs[0].ToolSource, ToolBuiltin)
	}
}

// TestToolNameKey_KeylessHashesToNothing: a Writer with no key must not fall back to exporting
// the name, and must not emit an unkeyed digest either. An absent label is a real answer; a
// cleartext MCP name is not.
func TestToolNameKey_KeylessHashesToNothing(t *testing.T) {
	var none ToolNameKey
	if got := none.label("mcp__acme__refund", ToolMCP); got != "" {
		t.Errorf("keyless label = %q, want empty", got)
	}
	// A built-in still exports, since it needs no key.
	if got := none.label("Bash", ToolBuiltin); got != "Bash" {
		t.Errorf("keyless builtin label = %q, want %q", got, "Bash")
	}
}

// TestToolNameKey_StableAndReused: T11a's "generated once and kept". A key that changed per
// export would make T1 and T4 fail for MCP traffic — the same tool would carry a different label
// in every export and no consumer could total its time across two of them.
func TestToolNameKey_StableAndReused(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadToolNameKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the process cache so the second load genuinely re-reads the file.
	keyOnce.Lock()
	delete(keyCached, dir)
	keyOnce.Unlock()
	k2, err := LoadToolNameKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	const name = "mcp__vendor__tool"
	if k1.label(name, ToolMCP) != k2.label(name, ToolMCP) {
		t.Error("the key changed across loads: one tool's history would split into two series")
	}
}

// TestToolNameKey_ConcurrentFirstUse is the T11a race. Two concurrent first-use exports must end
// up under ONE key: the spec's borrowed read-or-create ran once at boot, where two exports could
// each mint a key and the later write would win, re-labelling an export that had already
// streamed. O_CREATE|O_EXCL makes only one creator possible.
func TestToolNameKey_ConcurrentFirstUse(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	labels := make(chan string, n)
	for range n {
		go func() {
			keyOnce.Lock()
			delete(keyCached, dir)
			keyOnce.Unlock()
			k, err := LoadToolNameKey(dir)
			if err != nil {
				labels <- "ERR:" + err.Error()
				return
			}
			labels <- k.label("mcp__vendor__tool", ToolMCP)
		}()
	}
	first := <-labels
	if strings.HasPrefix(first, "ERR:") {
		t.Fatalf("concurrent load failed: %s", first)
	}
	for range n - 1 {
		if got := <-labels; got != first {
			t.Errorf("concurrent loads produced different labels (%q vs %q): "+
				"two keys were minted and one export was re-labelled", first, got)
		}
	}
}

// --- T16/T17/T31: ends and reasons ---

// TestToolCall_DurationPresentExactlyWhenHasEnd is T16's rule, stated as the spec states it:
// "no end, no duration", asked via HasEnd rather than by listing the reasons.
//
// A zero would pass a consumer's `durationMs > 0` filter as a 0 ms call and drag a p50 toward
// zero, which is why absence rather than zero is the answer for a call that never ended.
func TestToolCall_DurationPresentExactlyWhenHasEnd(t *testing.T) {
	// A result arrived: ended, so a duration.
	withResult := projectOne(t, toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	))
	if len(withResult) != 1 {
		t.Fatalf("got %d records, want 1", len(withResult))
	}
	r := withResult[0]
	if r.EndReason != toolpair.ReasonResult {
		t.Fatalf("endReason = %q, want %q", r.EndReason, toolpair.ReasonResult)
	}
	if !r.EndReason.HasEnd() {
		t.Fatal("ReasonResult must have an end")
	}
	if r.DurationMs == nil {
		t.Error("durationMs absent on a reason that HasEnd")
	}

	// A call that ends its run by design never gets a result: an end reason, but no end.
	endedRun := projectOne(t, toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "StructuredOutput"}},
		nil,
	))
	for _, r := range endedRun {
		if r.EndReason.HasEnd() {
			continue
		}
		if r.DurationMs != nil {
			t.Errorf("endReason %q has no end, yet durationMs = %d", r.EndReason, *r.DurationMs)
		}
	}
}

// TestToolCall_OpenSpanNotExported is T17. A span still open when the walk reads it has no
// duration to report, and a record that appeared with one in a later export would make the export
// non-reproducible.
func TestToolCall_OpenSpanNotExported(t *testing.T) {
	// A lone call with no result and no later turn of the same agent stays OPEN: toolpair's zero
	// reason. It must not be exported at all.
	evs := toolSession([]model.InferenceToolCall{{ID: "t1", Name: "Bash"}}, nil)
	res := toolpair.Calls(evs, toolpair.Options{})
	if len(res.Calls) != 1 || res.Calls[0].EndReason != "" {
		t.Fatalf("fixture does not produce one open call: %+v", res.Calls)
	}
	recs, _ := projectToolCalls(evs, "sess-1", testKey(t))
	if len(recs) != 0 {
		t.Errorf("exported %d records for an open call, want 0: %+v", len(recs), recs)
	}
}

// TestToolCall_EndReasonAlwaysWritten is T35's row for endReason: every exported call has one, so
// the key cannot be conditional. Checked on the wire, since that is where omitempty would bite.
func TestToolCall_EndReasonAlwaysWritten(t *testing.T) {
	recs := projectOne(t, toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	))
	var m map[string]any
	line, err := json.Marshal(recs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kind", "sessionId", "startedAt", "callSeq", "callIndex", "endReason"} {
		if _, ok := m[key]; !ok {
			t.Errorf("%q absent from the encoding: T35 requires it even at zero\n%s", key, line)
		}
	}
}

// --- T35: a measured zero is not an absent field ---

// TestToolCall_ZeroesSurviveMarshalling is the T35 regression, and the one most likely to be
// broken by a well-meaning `omitempty`. Three zeros here are REAL values:
//
//   - callIndex 0 is the FIRST call of every batch, and T34 makes it a join key
//   - durationMs 0 is a result clamped to its call's start (a real 0 ms call)
//   - resultBytes 0 is an empty result, where T14 reserves ABSENCE for "not measured"
func TestToolCall_ZeroesSurviveMarshalling(t *testing.T) {
	// A result that lands before the asking response finished: toolpair clamps End up to Start,
	// so the honest answer is a zero-length call rather than a negative one.
	evs := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: ""}},
	)
	// Put the result-carrying request BEFORE the asking response's end.
	evs[2].At = tcBase
	recs := projectOne(t, evs)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.CallIndex != 0 {
		t.Fatalf("fixture should produce callIndex 0, got %d", r.CallIndex)
	}
	if r.DurationMs == nil || *r.DurationMs != 0 {
		t.Errorf("durationMs = %v, want a present 0 for a clamped call", r.DurationMs)
	}
	if r.ResultBytes == nil || *r.ResultBytes != 0 {
		t.Errorf("resultBytes = %v, want a present 0 for an empty result", r.ResultBytes)
	}

	line, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"callIndex", "durationMs", "resultBytes"} {
		v, ok := m[key]
		if !ok {
			t.Errorf("%q absent at zero — omitempty turned a real 0 into \"not measured\"\n%s", key, line)
			continue
		}
		if f, isNum := v.(float64); isNum && f != 0 {
			t.Errorf("%q = %v, want 0", key, v)
		}
	}
}

// --- T15: the batch marker ---

// TestToolCall_BatchMarked is T15. A batch's calls share one window by construction, so the
// marker says the window is SHARED — never that the calls ran concurrently.
func TestToolCall_BatchMarked(t *testing.T) {
	evs := toolSession(
		[]model.InferenceToolCall{
			{ID: "t1", Name: "Bash"},
			{ID: "t2", Name: "Read"},
			{ID: "t3", Name: "Grep"},
		},
		[]model.InferenceToolResult{
			{ToolUseID: "t1", Content: "a"},
			{ToolUseID: "t2", Content: "b"},
			{ToolUseID: "t3", Content: "c"},
		},
	)
	recs := projectOne(t, evs)
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	seen := map[int]bool{}
	for _, r := range recs {
		if r.ParallelN != 3 {
			t.Errorf("parallelN = %d, want 3", r.ParallelN)
		}
		if !r.DurationShared {
			t.Error("durationShared false on a batch whose duration comes from a shared window")
		}
		if r.CallSeq != recs[0].CallSeq {
			t.Error("a batch's records must share callSeq — it is the batch's identity (T15)")
		}
		seen[r.CallIndex] = true
	}
	// T30: a batch is otherwise unorderable, so every index must be present exactly once.
	for i := range 3 {
		if !seen[i] {
			t.Errorf("callIndex %d missing: a batch cannot be put back in order", i)
		}
	}
}

// TestToolCall_LoneCallNotMarked: SiblingN is 1 for a lone call, so > 1 is the batch predicate
// and a lone call must carry neither marker.
func TestToolCall_LoneCallNotMarked(t *testing.T) {
	recs := projectOne(t, toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	))
	if recs[0].ParallelN != 0 || recs[0].DurationShared {
		t.Errorf("lone call marked as a batch: parallelN=%d shared=%v",
			recs[0].ParallelN, recs[0].DurationShared)
	}
}

// --- T5: context ---

// TestToolCall_CarriesSessionRoleAndClient is T5: a fleet view splits by any of the three.
func TestToolCall_CarriesSessionRoleAndClient(t *testing.T) {
	recs := projectOne(t, toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	))
	r := recs[0]
	if r.SessionID != "sess-1" {
		t.Errorf("sessionId = %q", r.SessionID)
	}
	if r.AgentRole != string(model.AgentRoleMain) {
		t.Errorf("agentRole = %q, want %q", r.AgentRole, model.AgentRoleMain)
	}
	if r.Client != "claude-code/2.1.270" {
		t.Errorf("client = %q", r.Client)
	}
	if r.Kind != ToolCallKind {
		t.Errorf("kind = %q, want %q", r.Kind, ToolCallKind)
	}
	if r.Schema != SchemaVersion {
		t.Errorf("schema = %d, want %d", r.Schema, SchemaVersion)
	}
}

// TestToolCall_AgentRoleIsAClosedVocabulary: the same rule and reason as Record.AgentRole's
// switch. This package's input is a DECODED archive event, so the field is whatever the JSON
// said — a 144-byte role carrying an escape sequence round-tripped verbatim before that switch.
func TestToolCall_AgentRoleIsAClosedVocabulary(t *testing.T) {
	evs := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	)
	const junk = "main\u0000\u001b[31mSECRET-ROLE"
	for i := range evs {
		if inf := evs[i].Inference; inf != nil && inf.AgentRole != "" {
			inf.AgentRole = model.AgentRole(junk)
		}
	}
	recs, _ := projectToolCalls(evs, "sess-1", testKey(t))
	for _, r := range recs {
		if r.AgentRole != "" {
			t.Errorf("agentRole = %q, want empty for an unrecognised value", r.AgentRole)
		}
	}
}

// --- §6.4: the cap and its marker ---

// TestToolCall_CapEmitsMarkerAndKeepsTheRest is §6.4. The cap bounds what one session
// contributes, and the marker says so IN BAND — a consumer must not read a capped session as a
// complete one, which is the same degrade-loudly posture as handleExport's ErrAbortHandler.
func TestToolCall_CapEmitsMarkerAndKeepsTheRest(t *testing.T) {
	// One response asking for maxCallsPerSession+10 calls, all closed by one later request.
	n := maxCallsPerSession + 10
	calls := make([]model.InferenceToolCall, 0, n)
	results := make([]model.InferenceToolResult, 0, n)
	for i := range n {
		id := "t" + itoa(i)
		calls = append(calls, model.InferenceToolCall{ID: id, Name: "Bash"})
		results = append(results, model.InferenceToolResult{ToolUseID: id, Content: "ok"})
	}
	recs, dropped := projectToolCalls(toolSession(calls, results), "sess-1", testKey(t))
	if len(recs) != maxCallsPerSession {
		t.Errorf("kept %d records, want the cap %d", len(recs), maxCallsPerSession)
	}
	if dropped != 10 {
		t.Errorf("dropped = %d, want 10", dropped)
	}
	m := markerRecord("sess-1", dropped, 0)
	if m.Kind != MarkerKind || m.SessionID != "sess-1" || m.Dropped != 10 || m.Cap != maxCallsPerSession {
		t.Errorf("marker = %+v", m)
	}
	// The marker must be legible on its own: a consumer reading an old file needs the cap that
	// produced it, not just the count.
	line, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kind", "sessionId", "dropped", "cap"} {
		if !strings.Contains(string(line), `"`+key+`"`) {
			t.Errorf("marker is missing %q: %s", key, line)
		}
	}
}

// TestToolNameKey_PublishesCompleteOrNotAtAll: the authoritative key file never exists in a
// partial state, and no temporary file survives.
//
// WHY THIS MATTERS MORE THAN THE RACE IT REPLACED. Creating the real path with O_EXCL and filling
// it afterwards closes the two-key race but publishes an EMPTY file first. A concurrent reader
// then finds a key shorter than toolKeyLen and refuses — and if the write never completes, that
// short file PERSISTS, so every later opted-in export fails until an operator deletes it. A
// permanent self-inflicted outage is worse than the transient mislabelling it was fixing.
func TestToolNameKey_PublishesCompleteOrNotAtAll(t *testing.T) {
	dir := t.TempDir()
	k, err := LoadToolNameKey(dir)
	if err != nil {
		t.Fatalf("LoadToolNameKey: %v", err)
	}
	if k.label("mcp__v__t", ToolMCP) == "" {
		t.Fatal("key loaded but labels nothing")
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temporary file survived publication: %q", e.Name())
		}
	}
	if len(names) != 1 || names[0] != toolKeyFile {
		t.Errorf("state dir holds %v, want only %q", names, toolKeyFile)
	}

	// The published file is complete and private.
	fi, err := os.Stat(filepath.Join(dir, toolKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != toolKeyLen {
		t.Errorf("key file is %d bytes, want %d — a reader would refuse a short key",
			fi.Size(), toolKeyLen)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %04o, want 0600", perm)
	}
}

// TestToolNameKey_ExistingKeyIsNeverReplaced: publication must not overwrite a key already in
// force, or an export streaming under the first key would be re-labelled mid-flight.
func TestToolNameKey_ExistingKeyIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadToolNameKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, toolKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	// Force a fresh load that takes the create path again.
	keyOnce.Lock()
	delete(keyCached, dir)
	keyOnce.Unlock()
	second, err := LoadToolNameKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, toolKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the key file changed on a second load: an in-flight export would be re-labelled")
	}
	const name = "mcp__vendor__tool"
	if first.label(name, ToolMCP) != second.label(name, ToolMCP) {
		t.Error("two loads produced different labels")
	}
}

// TestMarkerRecord_ReportsBothBounds: the two losses are different and a consumer must be able to
// tell them apart. Dropped is calls the projection paired and would not write; DroppedEvents is
// events it never held, whose calls therefore produce no record and are NOT counted in Dropped.
//
// The event fields are omitempty, so a session capped only on calls carries the marker it always
// carried — a consumer of an older file is unaffected.
func TestMarkerRecord_ReportsBothBounds(t *testing.T) {
	callsOnly := markerRecord("s", 7, 0)
	if callsOnly.DroppedEvents != 0 || callsOnly.EventCap != 0 {
		t.Errorf("event fields set with no events dropped: %+v", callsOnly)
	}
	line, err := json.Marshal(callsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "droppedEvents") || strings.Contains(string(line), "eventCap") {
		t.Errorf("event fields present at zero: %s", line)
	}

	both := markerRecord("s", 7, 1200)
	if both.DroppedEvents != 1200 || both.EventCap != maxPairedEvents {
		t.Errorf("marker = %+v, want 1200 events against the %d cap", both, maxPairedEvents)
	}
	if line, err = json.Marshal(both); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dropped", "cap", "droppedEvents", "eventCap"} {
		if !strings.Contains(string(line), `"`+key+`"`) {
			t.Errorf("marker is missing %q: %s", key, line)
		}
	}
}

// TestWalk_EventBoundIsBelowTheCallCap documents the relationship the two bounds must keep: the
// event bound has to admit at least as many events as the call cap could ever need, or the call
// cap becomes unreachable and its own test is measuring nothing.
//
// A batch of N calls rides on ONE response event, so the call cap can in principle be reached
// from far fewer events than itself — but a session of one call per turn needs two events per
// call, which is the shape this compares against.
func TestWalk_EventBoundIsBelowTheCallCap(t *testing.T) {
	if maxPairedEvents < 2*maxCallsPerSession {
		t.Errorf("maxPairedEvents (%d) is below two events per capped call (%d): a session of "+
			"one call per turn hits the event bound before the call cap, so the call cap is "+
			"unreachable on ordinary traffic", maxPairedEvents, 2*maxCallsPerSession)
	}
}

// itoa avoids a strconv import for one call site in a test.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// --- the seq-order precondition ---

// TestToolCall_DescendingEventsPairNothing is why Walk reverses, asserted rather than assumed.
//
// Earlier walks NEWEST FIRST and toolpair.Calls requires ascending seq. Feeding it the archive's
// order pairs nothing — every result precedes the call it closes, so each call closes as
// still-open and T17 then drops every record. It fails SILENTLY and COMPLETELY, which is the
// worst failure shape available: an export with no toolCall lines reads exactly like a session
// that made no tool calls.
func TestToolCall_DescendingEventsPairNothing(t *testing.T) {
	asc := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	)
	if got, _ := projectToolCalls(asc, "s", testKey(t)); len(got) != 1 {
		t.Fatalf("ascending order produced %d records, want 1", len(got))
	}
	desc := make([]model.SessionEvent, len(asc))
	for i, e := range asc {
		desc[len(asc)-1-i] = e
	}
	got, _ := projectToolCalls(desc, "s", testKey(t))
	if len(got) != 0 {
		t.Fatalf("descending order produced %d records: the fixture no longer demonstrates "+
			"the hazard, so Walk's reversal is untested", len(got))
	}
}

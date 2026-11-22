package sessionexport

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

func timeUTC() *time.Location { return time.UTC }

// toolArchive writes a session whose events pair into tool calls, then reopens the archive so a
// walk reads what a later process would — the same shape fixtureArchive uses, with inference
// payloads the pairing can actually work on.
//
// It holds TWO agents, so turnTables' per-agent gap bookkeeping is exercised rather than the
// single-agent path alone.
func toolArchive(t *testing.T) *archive.Archive {
	t.Helper()
	root := t.TempDir()
	write, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := tcBase
	store := session.New(0, 0, 0, session.WithClock(func() time.Time { return now }))
	store.AddRecorder(write)

	client := &model.EventClient{Name: "claude-code", Version: "2.1.270"}
	add := func(e model.SessionEvent) {
		now = e.At
		store.Append("tc-1", e)
	}
	// Three turns per agent: each closes the previous turn's call and asks for a new one, so the
	// middle calls close by result and the last stays open (and is dropped by T17).
	for turn := range 3 {
		for _, agent := range []string{"", "sub-1"} {
			role := model.AgentRoleMain
			if agent != "" {
				role = model.AgentRoleSubagent
			}
			rid := "req-" + agent + itoa(turn)
			at := tcBase.Add(time.Duration(turn*10) * time.Second)
			var results []model.InferenceToolResult
			if turn > 0 {
				results = []model.InferenceToolResult{
					{ToolUseID: "tu-" + agent + itoa(turn-1), Content: "done"},
				}
			}
			add(model.SessionEvent{
				At: at, Direction: model.Outbound, Phase: model.SessionRequest,
				RequestID: rid, Subagent: agent, Client: client, HTTPMethod: "POST",
				Inference: &model.InferenceExtension{
					Model: "claude-opus-5", AgentRole: role,
					Messages:    []model.InferenceMessage{{Role: "user", Content: "turn " + itoa(turn)}},
					Tools:       []model.InferenceTool{{Name: "Bash"}},
					ToolResults: results,
				},
			})
			add(model.SessionEvent{
				At: at.Add(time.Second), Direction: model.Outbound, Phase: model.SessionResponse,
				RequestID: rid, Subagent: agent, Client: client, Duration: 2 * time.Second,
				Inference: &model.InferenceExtension{
					Model: "claude-opus-5", AgentRole: role,
					ToolCalls: []model.InferenceToolCall{
						{ID: "tu-" + agent + itoa(turn), Name: "Bash", Arguments: `{"command":"ls"}`},
					},
				},
			})
		}
	}
	store.Close()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := archive.Open(root, archive.WithClock(func() time.Time { return tcBase }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// --- T20: a plain stream is byte-identical ---

// TestWriter_PlainStreamUnchanged is T20's hard constraint, and the reason Record.Kind is
// omitempty and set by the Writer rather than by FromEvent.
//
// NOT "SIMILAR" — IDENTICAL. A plain GET /v1/export must produce exactly the bytes it produced
// before toolCall records existed, because #1810 R49 makes Tier-2 content opt-in and every
// shipped consumer parses the old shape. Adding a key to every record is precisely what T28
// forbids on this path.
//
// Asserted against a literal rather than against a second Writer: comparing two code paths that
// share a bug passes while both are wrong, where a literal is the format a consumer already
// depends on.
func TestWriter_PlainStreamUnchanged(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteEvent(loadedEvent(), "sess-1"); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	line := buf.String()
	if strings.Contains(line, `"kind"`) {
		t.Errorf("a plain stream carries a kind key — T20's byte-identity is broken:\n%s", line)
	}
	// And the field order and content are the pre-toolCall ones: schema first, no kind between
	// it and sessionId.
	if !strings.HasPrefix(line, `{"schema":1,"sessionId":"sess-1"`) {
		t.Errorf("the record's leading fields changed shape:\n%s", line)
	}
}

// TestWriter_ToolCallStreamStampsEventKind is T28's other half: on an opted-in stream an event
// record DOES carry kind, so a consumer can select on it in both directions rather than on the
// absence of a key.
func TestWriter_ToolCallStreamStampsEventKind(t *testing.T) {
	var buf bytes.Buffer
	if err := NewToolCallWriter(&buf).WriteEvent(loadedEvent(), "sess-1"); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not a record: %v", err)
	}
	if m["kind"] != EventKind {
		t.Errorf("kind = %v, want %q on an opted-in stream", m["kind"], EventKind)
	}
}

// TestWriter_BothModesStayReadable is the compatibility claim T28 makes: a file from either mode
// is readable by one reader, because an absent kind still means an event record.
func TestWriter_BothModesStayReadable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		w        func(io.Writer) *Writer
		wantKind string
	}{
		{"plain", NewWriter, ""},
		{"opted in", NewToolCallWriter, EventKind},
	} {
		var buf bytes.Buffer
		if err := tc.w(&buf).WriteEvent(loadedEvent(), "s"); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var r Record
		if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
			t.Fatalf("%s: a record from this mode does not decode: %v", tc.name, err)
		}
		if r.Kind != tc.wantKind {
			t.Errorf("%s: Kind = %q, want %q", tc.name, r.Kind, tc.wantKind)
		}
		// Either way the event's own fields survive, which is what makes one reader enough.
		if r.SessionID != "sess-1" || r.Seq == 0 {
			t.Errorf("%s: event fields lost: %+v", tc.name, r)
		}
	}
}

// --- the walk, end to end ---

// TestWalk_ToolCallsOffByDefault: the pairing is not paid for unless it was asked for, and no
// toolCall line appears on a plain walk.
func TestWalk_ToolCallsOffByDefault(t *testing.T) {
	var buf bytes.Buffer
	stats, err := Walk(fixtureArchive(t), NewWriter(&buf), Filter{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if stats.ToolCalls != 0 || stats.Truncated != 0 {
		t.Errorf("stats = %+v, want no toolCall activity on a plain walk", stats)
	}
	if strings.Contains(buf.String(), ToolCallKind) {
		t.Error("a plain walk emitted a toolCall record")
	}
	if strings.Contains(buf.String(), `"kind"`) {
		t.Error("a plain walk stamped kind on its event records")
	}
}

// TestWalk_ToolCallStreamCarriesBothKinds is the end-to-end shape of an opted-in export: event
// records and toolCall records interleaved on one stream, each labelled.
//
// It runs over the real archive fixture, so it exercises the accumulate-reverse-pair path in Walk
// rather than projectToolCalls alone — which is where the newest-first/seq-order hazard lives.
func TestWalk_ToolCallStreamCarriesBothKinds(t *testing.T) {
	a := toolArchive(t)
	var buf bytes.Buffer
	stats, err := Walk(a, NewToolCallWriter(&buf), Filter{ToolNameKey: testKey(t)})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if stats.ToolCalls == 0 {
		t.Fatal("no toolCall records: the walk did not pair, which is what reversing protects")
	}
	var events, calls int
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not JSON: %q", line)
		}
		switch m["kind"] {
		case EventKind:
			events++
		case ToolCallKind:
			calls++
			// T8, on every line of a real walk rather than one projected record.
			if strings.Contains(line, "toolu_") {
				t.Errorf("a toolCall line carries a tool-use id: %s", line)
			}
		default:
			t.Errorf("a line carries no recognised kind: %s", line)
		}
	}
	if events == 0 {
		t.Error("no event records: the opt-in replaced the stream instead of adding to it")
	}
	if calls != stats.ToolCalls {
		t.Errorf("counted %d toolCall lines, stats say %d", calls, stats.ToolCalls)
	}
	if int(stats.Records) != events+calls {
		t.Errorf("stats.Records = %d, counted %d lines", stats.Records, events+calls)
	}
}

// TestWalk_ToolCallRecordsAreWellFormed checks the projected records off a real archive read
// carry the placement keys T29/T30/T34 promise, which is what a consumer joins on.
func TestWalk_ToolCallRecordsAreWellFormed(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Walk(toolArchive(t), NewToolCallWriter(&buf), Filter{ToolNameKey: testKey(t)}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var n int
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if !strings.Contains(line, `"kind":"`+ToolCallKind+`"`) {
			continue
		}
		var r ToolCallRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("toolCall line does not decode: %v\n%s", err, line)
		}
		n++
		if r.SessionID == "" {
			t.Error("sessionId empty: the walk did not stamp the session")
		}
		if r.StartedAt.IsZero() {
			t.Error("startedAt zero: T29 requires every record be placeable")
		}
		if r.StartedAt.Location() != timeUTC() {
			t.Errorf("startedAt is not UTC (%s): the stream's timestamps must parse as one",
				r.StartedAt.Location())
		}
		if r.CallSeq == 0 {
			t.Error("callSeq zero: T34's join key is missing")
		}
		if r.EndReason == "" {
			t.Error("endReason empty: T17 should have dropped an open call")
		}
	}
	if n == 0 {
		t.Fatal("no toolCall records to check")
	}
}

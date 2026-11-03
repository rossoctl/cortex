package archive

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// throughLines takes events through exactly what a segment does — split, one JSON line per
// record, decode, join — and returns what a reader would hand back.
func throughLines(t *testing.T, evs []pipeline.SessionEvent) []pipeline.SessionEvent {
	t.Helper()
	seen := map[string]struct{}{}
	table := map[string]string{}
	var out []pipeline.SessionEvent
	for _, e := range evs {
		stripped, r, fresh := split(e, seen)
		var lines [][]byte
		for _, s := range fresh {
			b, err := stringLine(s)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, b)
		}
		b, err := eventLine(stripped, r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, b)
		for _, l := range lines {
			var rec record
			if err := json.Unmarshal(l, &rec); err != nil {
				t.Fatalf("decode %q: %v", l, err)
			}
			switch rec.K {
			case kindString:
				table[rec.H] = rec.V
			case kindEvent:
				if !join(rec.E, rec.R, table) {
					t.Fatalf("join failed for seq %d", rec.E.Seq)
				}
				out = append(out, *rec.E)
			}
		}
	}
	return out
}

func mustJSON(t *testing.T, e pipeline.SessionEvent) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// onceThroughJSON is e as a client of the session API holds it: encoded, then decoded.
func onceThroughJSON(t *testing.T, e pipeline.SessionEvent) string {
	t.Helper()
	var back pipeline.SessionEvent
	if err := json.Unmarshal([]byte(mustJSON(t, e)), &back); err != nil {
		t.Fatal(err)
	}
	return mustJSON(t, back)
}

// What a segment hands back must encode exactly as what went in, once that has itself been
// through JSON — byte for byte, which for valid UTF-8 is plain byte-identity.
//
// THE QUALIFIER IS FOR INVALID UTF-8 ONLY, and is encoding/json's, not the archive's: marshaling
// a string holding \xff writes the escape �, and once that is decoded the string holds a
// real U+FFFD, which marshals as its raw bytes. Both decode to the identical string, so no
// client can tell. edgeEvents keeps such a string so the rule stays pinned rather than assumed.
//
// The fixture interleaves contentless tunnel-opens and a denial between turns, because a clean
// run of turns is not what live traffic looks like (gotcha #14).
func TestSplitJoin_RoundTripsByteIdentical(t *testing.T) {
	evs := append(synthSession(1, 20, 4096), edgeEvents()...)
	got := throughLines(t, evs)
	if len(got) != len(evs) {
		t.Fatalf("%d events back, want %d", len(got), len(evs))
	}
	for i := range evs {
		if w, g := onceThroughJSON(t, evs[i]), mustJSON(t, got[i]); w != g {
			t.Fatalf("event %d differs:\nwant %s\ngot  %s", i, w, g)
		}
	}
}

// The on-disk dedup and the store's interner agree on what is worth sharing.
func TestSplit_ThresholdIsInternMinLen(t *testing.T) {
	e := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Completion: strings.Repeat("c", session.InternMinLen-1),
		Messages:   []pipeline.InferenceMessage{{Content: strings.Repeat("m", session.InternMinLen)}},
	}}
	stripped, r, _ := split(e, map[string]struct{}{})
	if r == nil || r.C != nil || stripped.Inference.Completion != e.Inference.Completion {
		t.Fatalf("a %d-byte completion must stay inline: refs %+v", session.InternMinLen-1, r)
	}
	if len(r.M) != 1 || r.M[0] == nil || stripped.Inference.Messages[0].Content != "" {
		t.Fatalf("a %d-byte message must be a reference: refs %+v", session.InternMinLen, r)
	}
}

// Tool results dedup on disk as they do in memory — every request re-sends all of them —
// under the same threshold, and come back intact.
func TestSplit_ToolResultIsAReference(t *testing.T) {
	e := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{ToolResults: []pipeline.InferenceToolResult{
		{ToolUseID: "toolu_1", Content: strings.Repeat("r", session.InternMinLen)},
		{ToolUseID: "toolu_2", Content: "short", IsError: true},
	}}}
	stripped, r, _ := split(e, map[string]struct{}{})
	if r == nil || len(r.TR) != 2 || r.TR[0] == nil || stripped.Inference.ToolResults[0].Content != "" {
		t.Fatalf("a %d-byte tool result must be a reference: refs %+v", session.InternMinLen, r)
	}
	if r.TR[1] != nil || stripped.Inference.ToolResults[1].Content != "short" {
		t.Fatalf("a short tool result must stay inline: refs %+v", r)
	}
	got := throughLines(t, []pipeline.SessionEvent{e})
	if w, g := onceThroughJSON(t, e), mustJSON(t, got[0]); w != g {
		t.Fatalf("tool results differ after the round trip:\nwant %s\ngot  %s", w, g)
	}
}

// History tool-call arguments dedup on disk under the same threshold, and come back intact.
func TestSplit_HistoryToolCallArgumentsAreAReference(t *testing.T) {
	e := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{HistoryToolCalls: []pipeline.InferenceToolCall{
		{ID: "toolu_1", Name: "Bash", Arguments: `{"command":"` + strings.Repeat("a", session.InternMinLen) + `"}`},
		{ID: "toolu_2", Name: "Read", Arguments: `{}`},
	}}}
	stripped, r, _ := split(e, map[string]struct{}{})
	if r == nil || len(r.TA) != 2 || r.TA[0] == nil || stripped.Inference.HistoryToolCalls[0].Arguments != "" {
		t.Fatalf("long arguments must be a reference: refs %+v", r)
	}
	if r.TA[1] != nil || stripped.Inference.HistoryToolCalls[1].Arguments != `{}` {
		t.Fatalf("short arguments must stay inline: refs %+v", r)
	}
	got := throughLines(t, []pipeline.SessionEvent{e})
	if w, g := onceThroughJSON(t, e), mustJSON(t, got[0]); w != g {
		t.Fatalf("history tool calls differ after the round trip:\nwant %s\ngot  %s", w, g)
	}
}

// split runs on events the store also holds and serves; it must clone what it blanks.
func TestSplit_NeverMutatesItsInput(t *testing.T) {
	for _, e := range append(synthSession(2, 3, 2048), edgeEvents()...) {
		before := mustJSON(t, e)
		split(e, map[string]struct{}{})
		if after := mustJSON(t, e); after != before {
			t.Fatalf("split changed its input:\nbefore %s\nafter  %s", before, after)
		}
	}
}

func TestSplit_EmptyParametersRoundTrip(t *testing.T) {
	e := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Tools: []pipeline.InferenceTool{{Name: "none", Description: strings.Repeat("d", 80)}}}}
	got := throughLines(t, []pipeline.SessionEvent{e})
	if p := got[0].Inference.Tools[0].Parameters; p != "" {
		t.Fatalf("Parameters = %q, want empty", p)
	}
}

// A string is written once per seen set — once per segment — and again in a new one.
func TestSplit_EmitsAStringOncePerSeenSet(t *testing.T) {
	shared := strings.Repeat("the conversation so far ", 5)
	e := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{
		Messages: []pipeline.InferenceMessage{{Content: shared}}}}

	seen := map[string]struct{}{}
	if _, _, fresh := split(e, seen); len(fresh) != 1 {
		t.Fatalf("first event emitted %d strings, want 1", len(fresh))
	}
	if _, _, fresh := split(e, seen); len(fresh) != 0 {
		t.Fatalf("second event emitted %d strings, want 0", len(fresh))
	}
	if _, _, fresh := split(e, map[string]struct{}{}); len(fresh) != 1 {
		t.Fatalf("a new seen set emitted %d strings, want 1", len(fresh))
	}
}

// A reference to a string the segment never defined is corruption, and must say so.
func TestJoin_ReportsAMissingReference(t *testing.T) {
	e := synthSession(3, 1, 2048)[0]
	stripped, r, _ := split(e, map[string]struct{}{})
	if join(&stripped, r, map[string]string{}) {
		t.Fatal("join succeeded with an empty table")
	}
}

// A reference to a position the event does not have is corruption too, not a panic.
func TestJoin_ReportsAnOutOfRangeReference(t *testing.T) {
	h := "x"
	e := pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{}}
	if join(&e, &refs{M: []*string{&h}}, map[string]string{"x": "v"}) {
		t.Fatal("join succeeded for a message index the event does not have")
	}
	if join(&pipeline.SessionEvent{}, &refs{C: &h}, map[string]string{"x": "v"}) {
		t.Fatal("join succeeded for a completion on an event with no inference")
	}
}

func TestHashOf_IsStableAndShort(t *testing.T) {
	a, b := hashOf("same"), hashOf("same")
	if a != b || len(a) != 22 || hashOf("other") == a {
		t.Fatalf("hashOf: %q %q %q", a, b, hashOf("other"))
	}
}

// Any session id the store can hold — 256 bytes, a pending process bucket — names a
// fixed-length, filesystem-safe directory.
func TestDirName_IsFixedLengthHexForAnyID(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	ids := []string{"a", strings.Repeat("x", session.MaxSessionIDLen), "pending:claude-code@123.456", "../../etc"}
	names := map[string]bool{}
	for _, id := range ids {
		n := dirName(id)
		if !hex32.MatchString(n) {
			t.Errorf("dirName(%q) = %q, want 32 hex", id, n)
		}
		names[n] = true
	}
	if len(names) != len(ids) {
		t.Errorf("dirName collided: %v", names)
	}
}

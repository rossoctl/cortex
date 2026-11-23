package sessionexport

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// TestWalk_SinceDropsCallsBelowTheFloor is T33, and it is a consequence of Option A rather than
// code written for it — which is why it needs its own test.
//
// Walk STOPS at the first event below a since floor (see Walk's note on why stopping beats
// filtering), so the slice the pairing receives is the session's TAIL. A call whose asking
// response fell below the floor therefore has no start in that slice, and T29 requires every
// record carry one. The pairing's own forward walk handles this correctly without being told:
// a result with no matching call in the window closes nothing, so no record is emitted.
//
// WHAT THIS GUARDS AGAINST is the opposite outcome — a record built from a result alone, carrying
// a zero startedAt. That would place a call at year 1 on a timeline, which is worse than its
// absence, and it is exactly what a projection written to "emit a record per result" would do.
func TestWalk_SinceDropsCallsBelowTheFloor(t *testing.T) {
	a := toolArchive(t)
	// The fixture's turns run at tcBase, +10s and +20s. A floor inside the last turn leaves that
	// turn's result-carrying request in the window while its asking response sits below.
	floor := tcBase.Add(15 * time.Second)

	var buf strings.Builder
	stats, err := Walk(a, NewToolCallWriter(&buf), Filter{Since: floor, ToolNameKey: testKey(t)})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if stats.Records == 0 {
		t.Fatal("the floor dropped the whole session; this test needs events above it")
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if !strings.Contains(line, `"kind":"`+ToolCallKind+`"`) {
			continue
		}
		var r ToolCallRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("toolCall line does not decode: %v", err)
		}
		// T29: every exported record is placeable. A record whose start fell below the floor
		// must not appear at all rather than appear with a zero or pre-floor start.
		if r.StartedAt.IsZero() {
			t.Errorf("a record carries a zero startedAt — built from a result whose call was "+
				"below the floor:\n%s", line)
		}
		if r.StartedAt.Before(floor) {
			t.Errorf("a record starts at %s, below the since floor %s: its asking response was "+
				"not in the window, so it should not have been exported\n%s",
				r.StartedAt, floor, line)
		}
	}
}

// TestWalk_ToolCallWriteErrorSurfaces: a write failure on a toolCall line must reach the caller,
// the way an event line's does. Walk's contract is that a failed walk returns an error and its
// stats report what was carried up to it — which is what lets handleExport tell a failure before
// the first record from one after it.
func TestWalk_ToolCallWriteErrorSurfaces(t *testing.T) {
	a := toolArchive(t)
	// Past the event records (the fixture writes 12) and into the toolCall ones, so the failure
	// lands on a line this PR added rather than on one the existing tests already cover.
	w := NewToolCallWriter(&failAfter{w: io.Discard, remain: 13})
	_, err := Walk(a, w, Filter{ToolNameKey: testKey(t)})
	if err == nil {
		t.Fatal("Walk returned nil on a failing writer: a truncated export would read as whole")
	}
}

// TestWalk_SinceNeverLabelsAMidSessionTurnAsFirst is T24's meaning, and the bug it guards is
// one this PR shipped twice.
//
// `gapKind: "first"` means THE AGENT'S FIRST TURN, and T24 reserves an absent `gapMs` for
// exactly that case. But turnTables only sees the events the walk handed it, and that slice
// starts mid-session in three ways: a `--since` floor stops the read, maxPairedEvents drops the
// oldest events, and a pruned archive simply has no older segments. In each one an agent's
// first IN-WINDOW turn looked like its first turn ever, so a call that had been waiting on a
// tool for eight seconds exported as the start of the session with no gap at all.
//
// That breaks two things a consumer reasonably does: counting agents per session from `first`,
// and building a duty cycle from a `--since` export — which is the common case, not the corner.
// It also makes the export non-reproducible, since the same call answers differently depending
// on the window.
func TestWalk_SinceNeverLabelsAMidSessionTurnAsFirst(t *testing.T) {
	a := toolArchive(t)
	floor := tcBase.Add(5 * time.Second)

	var buf strings.Builder
	if _, err := Walk(a, NewToolCallWriter(&buf), Filter{Since: floor, ToolNameKey: testKey(t)}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var seen int
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if !strings.Contains(line, `"kind":"`+ToolCallKind+`"`) {
			continue
		}
		var r ToolCallRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("toolCall line does not decode: %v", err)
		}
		seen++
		if r.GapKind == GapFirst {
			t.Errorf("a floored export labelled callSeq %d as the agent's first turn: the "+
				"slice starts mid-session, so this agent may have been working for hours\n%s",
				r.CallSeq, line)
		}
	}
	if seen == 0 {
		t.Fatal("no toolCall records above the floor; this test needs some")
	}
}

// TestProjectToolCalls_FirstOnlyWhenTheSliceStartsAtSeqOne pins the rule directly, both ways:
// a slice that starts at the session's start still reports `first`, and one that does not
// reports neither field.
func TestProjectToolCalls_FirstOnlyWhenTheSliceStartsAtSeqOne(t *testing.T) {
	full := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	)
	if full[0].Seq != 1 {
		t.Fatalf("fixture starts at seq %d, want 1", full[0].Seq)
	}
	recs, _ := projectToolCalls(full, "s", testKey(t))
	if len(recs) != 1 || recs[0].GapKind != GapFirst {
		t.Fatalf("a slice from seq 1 must still report first: %+v", recs)
	}

	// The same events renumbered so the slice starts above 1, as a floored or pruned read does.
	mid := toolSession(
		[]model.InferenceToolCall{{ID: "t1", Name: "Bash"}},
		[]model.InferenceToolResult{{ToolUseID: "t1", Content: "ok"}},
	)
	for i := range mid {
		mid[i].Seq += 100
	}
	recs, _ = projectToolCalls(mid, "s", testKey(t))
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].GapKind != "" {
		t.Errorf("gapKind = %q for a slice starting at seq %d, want absent",
			recs[0].GapKind, mid[0].Seq)
	}
	if recs[0].GapMs != nil {
		t.Errorf("gapMs = %v for a mid-session slice, want absent", *recs[0].GapMs)
	}
}

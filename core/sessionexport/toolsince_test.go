package sessionexport

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
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

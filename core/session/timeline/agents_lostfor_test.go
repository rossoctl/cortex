package timeline

import (
	"testing"

	"github.com/rossoctl/cortex/core/session/toolpair"
)

// TestLostForFailsClosedOnAnUnknownReason is the guard on the mapping's DIRECTION.
//
// lostFor used to name the no-result reasons, so a reason added to toolpair later matched none
// of them and read as "not lost" — and a call that ended without a result then rendered as
// still running, forever, on a pane whose job is showing what finished. Deriving it from HasEnd
// inverts that: an unknown reason is lost until someone says otherwise. A future reason must
// not need an edit here, and this test fails if the derivation goes back to a list.
func TestLostForFailsClosedOnAnUnknownReason(t *testing.T) {
	if !lostFor(toolpair.EndReason("some_reason_added_later")) {
		t.Error("lostFor(unknown) = false, want true: an unrecognised reason must read as lost, not as open")
	}
	// The zero reason is the one exception: still open is not a kind of loss.
	if lostFor("") {
		t.Error(`lostFor("") = true, want false: an open call has no end at all`)
	}
	for _, r := range []toolpair.EndReason{toolpair.ReasonResult, toolpair.ReasonBackground} {
		if lostFor(r) {
			t.Errorf("lostFor(%q) = true, want false: it carries an end", r)
		}
	}
	for _, r := range []toolpair.EndReason{toolpair.ReasonEndedRun, toolpair.ReasonStopped, toolpair.ReasonAbandoned} {
		if !lostFor(r) {
			t.Errorf("lostFor(%q) = false, want true", r)
		}
	}
}

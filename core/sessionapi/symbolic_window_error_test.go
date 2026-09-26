package sessionapi

import (
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/usage"
)

// THE REFUSAL MUST NAME THE WINDOW THE CALLER USED, for every symbolic window there is.
//
// "month" joined the symbolic set without joining this message, so window=month&session= came back
// refused for "a symbolic window (today, 7d)" — a 400 describing a request the caller did not make,
// which reads as a bug in the server rather than as an answer.
//
// DERIVED FROM usage's OWN CONSTANTS rather than from a second literal list here: a renamed window
// fails this test, where a hand-copied list would simply go on agreeing with itself. It does not
// catch a FOURTH window added to ParseWindowSpec and to nothing else — the package exports no
// enumerable set to check against — so this holds the three that exist and no more than that.
func TestUsageErrorNamesEverySymbolicWindow(t *testing.T) {
	symbolic := []string{usage.WindowToday, usage.Window7d, usage.WindowMonth}
	msg := errSessionWithSymbolicWindow.Error()

	for _, w := range symbolic {
		if !strings.Contains(msg, w) {
			t.Errorf("the refusal %q does not name %q, so a caller who asked for that window "+
				"reads a message about a request they did not make", msg, w)
		}
	}

	// AND EACH IS REALLY SYMBOLIC, or the list above is the wrong list. A duration window named
	// here would advertise a refusal that does not apply to it: the ledger is not involved in
	// serving one, which is the whole reason session= is compatible with it.
	for _, w := range symbolic {
		spec, err := usage.ParseWindowSpec(w, time.Now())
		if err != nil {
			t.Fatalf("ParseWindowSpec(%q): %v", w, err)
		}
		if !spec.Symbolic() {
			t.Errorf("%q does not parse as a symbolic window, so naming it in this refusal is "+
				"wrong in the other direction", w)
		}
	}
}

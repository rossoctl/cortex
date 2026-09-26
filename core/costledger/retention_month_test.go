package costledger

import (
	"testing"

	"github.com/rossoctl/cortex/core/usage"
)

// THE MONTH'S PIN IS GONE, AND THAT IS THE FIX RATHER THAN A GAP. It compared
// defaultRetentionDays against usage.WindowMonthLocalDays in both directions — "they must be
// equal" — and the constant IS that expression now (see store.go), so the test had become 31 == 31.
//
// What it was really guarding lives in two better places: usage's own
// TestWindowMonthLocalDays_IsTheLongestMonthsDateCount walks twenty-one years of calendars in
// eight zones to confirm the number means what it says, and the 7d check below keeps a default
// chosen for the month from dropping under the other shipped window.
//
// It existed because the two DID disagree once: the default was 30 while its own doc claimed to be
// "long enough to answer what did last month cost", and 30 is one day short — prune retains
// exactly retainDays distinct dates, so a month-to-date window on the 31st of a 31-day month had
// its first day file already deleted, invisibly, because a pruned day produces no Caveats entry.

// TestDefaultRetention_StillCoversTheSevenDayWindow guards the other window against a change
// made for the month's sake. 7d needs nine dates and the month needs thirty-one, so the month
// dominates today — but a future default chosen only against the month could drop below nine
// and break a window nothing in this test file mentions.
func TestDefaultRetention_StillCoversTheSevenDayWindow(t *testing.T) {
	if defaultRetentionDays < usage.Window7dLocalDays {
		t.Errorf("defaultRetentionDays = %d, below the %d dates a 7d window can touch",
			defaultRetentionDays, usage.Window7dLocalDays)
	}
}

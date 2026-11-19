package ledger

import (
	"testing"
	"time"
)

// A session that asked not to be recorded (SessionEvent.NoRecord, #1923) is kept out of the
// session archive only. Its spend still counts here: ledger rows carry no content, and the cost
// views must stay complete.
func TestWriter_CountsAWithheldSession(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	for i := 0; i < 2; i++ {
		ev := costedEvent(t, "gw", "m", 0.25, 100, 50)
		ev.NoRecord = true
		w.Record("s1", ev)
	}
	now = at.Add(time.Minute)
	later := costedEvent(t, "gw", "m", 0.10, 10, 5)
	later.At, later.NoRecord = now, true
	w.Record("s1", later)
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 || rows[0].Requests != 2 || rows[0].CostMicros != 500_000 {
		t.Fatalf("rows = %+v, want one closed minute of 2 requests and 500000 micros", rows)
	}
}

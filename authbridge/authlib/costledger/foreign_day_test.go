package costledger

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// THE DURABLE SURFACE MUST NOT BE MORE PERMISSIVE THAN THE VOLATILE ONE, and it was.
//
// usage.plausibleTokenReport refuses a whole report where any counter exceeds
// pricing.MaxPlausibleTokens and counts it in Counts.RefusedTokenRequests. tokenCount bounded
// only the NEGATIVE side, so the same response was refused by the six-hour ring and written
// to the thirty-day file — and abctl renders both, so one endpoint reported two different
// token totals for identical traffic with nothing to say which was which.
//
// It is the inverse of the argument tokenCount's own comment makes about negatives: the
// reason a negative is refused is that a durable figure has no repair path, and the ceiling
// has exactly the same property.
func TestRecord_AnImplausibleTokenCountIsRefusedByTheLedgerToo(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	// One field past the bound. The others are ordinary, which is what makes the per-field
	// zeroing visible: an all-or-nothing refusal would take the output count with it.
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, pricing.MaxPlausibleTokens+1, 40))

	rows, _, err := w.Window(context.Background(), at.Add(-time.Minute), at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 — an impossible count reached a file kept for 30 "+
			"days while the ring refused the same report", r.InputTokens)
	}
	// The rest of the row survives, which is the point of bounding per FIELD here rather
	// than refusing the report as the ring does: a ledger row has no RefusedTokenRequests
	// column to explain an all-or-nothing drop with.
	if r.OutputTokens != 40 {
		t.Errorf("OutputTokens = %d, want 40 — the plausible counters must survive", r.OutputTokens)
	}
	if r.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want 250000 — the dollars are bounded separately, by "+
			"pricing.MaxPlausibleRequestCostMicros, and are what the row is for", r.CostMicros)
	}
}

// A LABEL IS SANITISED ON THE WAY OUT OF A FILE, not only on the way in.
//
// rowLabel caps and sanitises what the WRITER produces, and readDay trusted that as the only
// way a line could come to exist. A day file this process did not write — or wrote before the
// cap existed — therefore handed a multi-KB series key carrying a C1 CSI escape through Fold
// to the session API and into any terminal rendering it.
//
// writeDay is used deliberately: it puts bytes on disk without going through the Writer,
// which is the whole shape of the gap. The files are 0600 under $HOME, so the author is the
// user rather than a remote attacker — the point is that the CONTENT is not this process's
// own output and nothing downstream re-checks it.
func TestReadDay_SanitisesAndCapsLabelsFromAForeignFile(t *testing.T) {
	dir := t.TempDir()
	// U+009B is the C1 CSI: ONE code point that opens an escape sequence, which is why the
	// sanitiser is a rune scan and not a byte scan. Written as an escape so the fixture
	// cannot be mangled by a tool that touches this file.
	hostile := "claude-code\u009b31m"
	long := strings.Repeat("m", 4096)
	writeDay(t, dir, at, fmt.Sprintf(
		`{"at":%q,"endpoint":"gw","model":%q,"agent":%q,"requests":1,"costMicros":100,`+
			`"pricedRequests":1,"priceableRequests":1}`,
		at.Format(time.RFC3339Nano), long, hostile))
	w := newTestWriter(t, dir, func() time.Time { return at })

	rows, _, err := w.Query(context.Background(), at.Add(-time.Minute), at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if strings.ContainsRune(rows[0].Agent, '\u009b') {
		t.Errorf("Agent = %q still carries U+009B: it reaches /v1/usage and then a terminal",
			rows[0].Agent)
	}
	if n := len(rows[0].Model); n > maxLabelLen {
		t.Errorf("Model is %d bytes, want at most %d — an uncapped key from a file is an "+
			"uncapped series key in the response", n, maxLabelLen)
	}
	// And the row is otherwise intact: sanitising a label must not cost the money on it.
	if rows[0].CostMicros != 100 {
		t.Errorf("CostMicros = %d, want 100", rows[0].CostMicros)
	}
}

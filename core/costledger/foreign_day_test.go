package costledger

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pricing"
	"github.com/rossoctl/cortex/core/usage"
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
	// EVERY CAPPED FIELD CARRIES A HOSTILE VALUE, not just one. provenance was the field this
	// test could not see: the reader cleaned three of the four, and provenance is the one
	// Row.PricedBy is reconstructed from, so it reached a series key uncleaned. A fixture that
	// exercises one field per defect cannot catch the field nobody thought of.
	writeDay(t, dir, at, fmt.Sprintf(
		`{"at":%q,"endpoint":%q,"model":%q,"agent":%q,"provenance":%q,"requests":1,"costMicros":100,`+
			`"pricedRequests":1,"priceableRequests":1}`,
		at.Format(time.RFC3339Nano), hostile, long, hostile, long+hostile))
	w := newTestWriter(t, dir, func() time.Time { return at })

	rows, _, err := w.Query(context.Background(), at.Add(-time.Minute), at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	for _, f := range []struct {
		name  string
		value string
	}{
		{"Endpoint", rows[0].Endpoint},
		{"Model", rows[0].Model},
		{"Agent", rows[0].Agent},
		{"Provenance", rows[0].Provenance},
	} {
		if strings.ContainsRune(f.value, '\u009b') {
			t.Errorf("%s = %q still carries U+009B: it reaches /v1/usage and then a terminal",
				f.name, f.value)
		}
		if n := len(f.value); n > maxLabelLen {
			t.Errorf("%s is %d bytes, want at most %d — an uncapped key from a file is an "+
				"uncapped series key in the response", f.name, n, maxLabelLen)
		}
	}
	// And the row is otherwise intact: sanitising a label must not cost the money on it.
	if rows[0].CostMicros != 100 {
		t.Errorf("CostMicros = %d, want 100", rows[0].CostMicros)
	}
}

// A RETENTION SO LARGE IT DELETES EVERYTHING is the failure this bound exists for, and it is
// the reason a ceiling is not a disk-space preference.
//
// prune counts back with ref.AddDate(0, 0, -(retainDays-1)), and AddDate NORMALISES instead of
// saturating. At retainDays = 1<<62-1 against 2026-09-15 the cutoff comes out as 2026-09-17 —
// two days in the FUTURE — so every day file including today is older than the cutoff and the
// first prune takes the whole ledger. The largest number an operator can type, meaning "keep
// everything", kept nothing.
//
// Driven through newStore rather than by calling prune with a hand-built store, because the
// clamp is what is under test and newStore is where it lives.
func TestPrune_AnAbsurdRetentionKeepsHistoryRatherThanDeletingIt(t *testing.T) {
	dir := t.TempDir()
	day := at
	writeDay(t, dir, day, line(day, "gw", "m", 1, 10, 5, 100))
	// And a genuinely old file, so the test can tell "clamped to the maximum" from "pruned
	// nothing at all" — a clamp that disabled retention entirely would also keep today.
	old := day.AddDate(0, 0, -(maxRetentionDays + 5))
	writeDay(t, dir, old, line(old, "gw", "m", 1, 10, 5, 100))

	s, err := newStore(dir, math.MaxInt64/2, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	if s.retainDays != maxRetentionDays {
		t.Fatalf("retainDays = %d, want the clamp at %d", s.retainDays, maxRetentionDays)
	}
	if err := s.prune(day); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, day.Format(dayLayout)+".jsonl")); err != nil {
		t.Errorf("today's day file is gone after a prune with an absurd retention: %v — the "+
			"cutoff wrapped into the future and took the ledger with it", err)
	}
	if _, err := os.Stat(filepath.Join(dir, old.Format(dayLayout)+".jsonl")); err == nil {
		t.Errorf("the %d-day-old file survived, so retention did not run at all — the clamp must "+
			"bound the window, not disable it", maxRetentionDays+5)
	}
}

// The two copies of the ceiling must agree, on the same reasoning as the floor's own
// agreement test: config owns the derivation and cannot be imported here, so the guarantee is
// a test rather than a shared constant.
func TestMaxRetentionDays_MatchesTheConfigCeiling(t *testing.T) {
	// WHAT THIS CAN AND CANNOT CHECK, stated because the earlier version of this test claimed the
	// stronger thing and delivered the weaker one. It compared maxRetentionDays against a third
	// copy of the literal written here, so a change to config.maxCostLedgerRetentionDays left it
	// green — the two were never pinned at all.
	//
	// A test in this package cannot reach an unexported constant in config, so the pin belongs on
	// the config side and asserts against the now-exported MaxRetentionDays. What is checkable
	// here is that the exported name and the internal spelling are the same number, which is what
	// makes that pin bind this package's clamp.
	if maxRetentionDays != MaxRetentionDays {
		t.Errorf("maxRetentionDays = %d but MaxRetentionDays = %d: config's pin asserts against the exported one, so a drift here would leave the clamp unpinned",
			maxRetentionDays, MaxRetentionDays)
	}
	if MaxRetentionDays != 3650 {
		t.Errorf("MaxRetentionDays = %d, want 3650: config.maxCostLedgerRetentionDays is derived from this, and its own test is where the two are held equal", MaxRetentionDays)
	}
}

// TestQuery_AnAbsurdSpanIsBoundedWithoutLosingRows pins the day walk's bound and its losslessness at
// once, because either one alone is the wrong fix.
//
// The walk is one os.Open per day and was bounded only by the caller's span: measured at 106,751
// opens and 1.67s for Query(time.Time{}, now) against a tmpdir. Clamping is only correct because no
// day file can exist outside maxRetentionDays either side of the clock's day, so the rows a zero-from
// query returns must be IDENTICAL to a tight one's.
func TestQuery_AnAbsurdSpanIsBoundedWithoutLosingRows(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })
	writeDay(t, dir, at, fmt.Sprintf(
		`{"at":%q,"endpoint":"gw","model":"m","requests":1,"costMicros":100,`+
			`"pricedRequests":1,"priceableRequests":1}`, at.Format(time.RFC3339Nano)))

	for _, tc := range []struct {
		name     string
		from, to time.Time
		wantDays int
		wantRows int
	}{
		{"a tight window", at.Add(-time.Minute), at.Add(time.Minute), 1, 1},
		// The zero time is what a caller means by "everything", and it is 106,751 days back.
		{"from the zero time", time.Time{}, at.Add(time.Minute), maxRetentionDays + 1, 1},
		// The far future, the same problem at the other end.
		{"to the year 9999", at.Add(-time.Minute), at.AddDate(8000, 0, 0), maxRetentionDays + 1, 1},
		// Wholly outside the band: no file can exist there, so no day is walked at all.
		{"a span before any file could exist", at.AddDate(-50, 0, 0), at.AddDate(-40, 0, 0), 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromMin, toMin := span(tc.from, tc.to)
			first, last := w.dayWalk(fromMin, toMin)
			days := 0
			for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
				days++
			}
			if days != tc.wantDays {
				t.Errorf("the walk covers %d days, want %d: this is one os.Open each against a path an operator chose",
					days, tc.wantDays)
			}

			rows, _, err := w.Query(context.Background(), tc.from, tc.to)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(rows) != tc.wantRows {
				t.Errorf("got %d rows, want %d: the clamp is only correct if it cannot shorten an answer",
					len(rows), tc.wantRows)
			}
		})
	}
}

// TestMaxLabelLen_MatchesTheRingItMirrors makes row.go's "matched deliberately rather than chosen
// again" a checkable claim.
//
// Both copies were unexported, so nothing could compare them: every cap assertion in this package is
// written in terms of maxLabelLen itself, which means raising it to 4096 kept the whole suite green
// while the ring kept truncating at 96 — one series key spelled two ways depending on which half of
// the system answered. This is the defect MaxRetentionDays was exported to fix, in the other
// constant.
func TestMaxLabelLen_MatchesTheRingItMirrors(t *testing.T) {
	if maxLabelLen != usage.MaxLabelLen {
		t.Errorf("costledger maxLabelLen = %d, usage.MaxLabelLen = %d: a label capped differently on the two halves is one series key spelled two ways",
			maxLabelLen, usage.MaxLabelLen)
	}
	// The literal too, so a change that moved BOTH constants together still has to be deliberate:
	// the number is a judgement about real model ids, not an implementation detail.
	if maxLabelLen != 96 {
		t.Errorf("maxLabelLen = %d, want 96: see the derivation on usage.MaxLabelLen", maxLabelLen)
	}
}

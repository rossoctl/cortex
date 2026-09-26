package costledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/usage"
)

// writeDay writes lines straight to a day file, bypassing Writer, so a query test
// controls exactly what is on disk without driving the accumulation path.
func writeDay(t *testing.T, dir string, day time.Time, lines ...string) {
	t.Helper()
	name := filepath.Join(dir, day.Format(dayLayout)+".jsonl")
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(name, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// line renders one ledger row as JSON, so a test can state exactly what is on disk.
func line(min time.Time, endpoint, model string, requests, in, out, micros int64) string {
	return fmt.Sprintf(
		`{"at":%q,"endpoint":%q,"model":%q,"requests":%d,"inputTokens":%d,`+
			`"outputTokens":%d,"costMicros":%d,"pricedRequests":%d,"priceableRequests":%d}`,
		min.Format(time.RFC3339Nano), endpoint, model, requests, in, out, micros, requests, requests)
}

func TestQuery_SpanInsideOneDay(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		line(base.Add(time.Minute), "gw", "m", 1, 20, 5, 200),
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, _, err := w.Query(context.Background(), base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Inclusive of both endpoints at minute granularity, so the third row is out.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
}

func TestQuery_SpanCrossingLocalMidnight(t *testing.T) {
	// The case a UTC-vs-local bug shows up in, and the reason "today" is local
	// midnight: a laptop crossing a timezone must not have its day reset
	// mid-afternoon.
	//
	// A FIXED NON-UTC ZONE, not time.Local. Building both the input and the
	// expectation in time.Local made this vacuous wherever time.Local is UTC — the
	// default in most CI containers — because a dayOf reading UTC would then agree with
	// the test by construction. At UTC-7 the two rows below straddle local midnight but
	// fall in the SAME UTC day, so a UTC day walk visits one date and misses a file.
	// Verified by mutation: with dayOf on a UTC day, the old test passed under TZ=UTC
	// and failed under TZ=America/New_York; this one fails under both.
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, testZone)
	before := midnight.Add(-30 * time.Minute) // 23:30 on the 13th, local
	after := midnight.Add(30 * time.Minute)   // 00:30 on the 14th, local
	writeDay(t, dir, before, line(before, "gw", "m", 1, 10, 5, 100))
	writeDay(t, dir, after, line(after, "gw", "m", 1, 20, 5, 200))
	w := newTestWriter(t, dir, func() time.Time { return after })

	// Two files, named for the two LOCAL days. Asserted rather than assumed, because
	// this is the property a UTC boundary breaks.
	for _, want := range []string{"2026-09-13.jsonl", "2026-09-14.jsonl"} {
		if _, serr := os.Stat(filepath.Join(dir, want)); serr != nil {
			t.Fatalf("missing %s: %v", want, serr)
		}
	}

	got, _, err := w.Query(context.Background(), before, after)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 across two day files: %+v", len(got), got)
	}
}

func TestQuery_MissingDayFileIsNotAnError(t *testing.T) {
	// An idle day writes no file. That is the normal case on a laptop, not a
	// fault — erroring would make "this week" fail for anyone who took a day off.
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, _, err := w.Query(context.Background(), base.AddDate(0, 0, -3), base)
	if err != nil {
		t.Fatalf("Query over an empty range: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows from an empty ledger, want 0", len(got))
	}
}

func TestQuery_TruncatedFinalLineIsSkippedAndTheRestSurvives(t *testing.T) {
	// A truncated tail is the expected outcome of a crash mid-append. Losing the
	// whole day because its last line is half-written would turn a 60-second gap
	// into a 24-hour one.
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		line(base.Add(time.Minute), "gw", "m", 1, 20, 5, 200),
		`{"at":"2026-09-13T09:02:00`, // truncated mid-write
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, _, err := w.Query(context.Background(), base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the 2 intact ones: %+v", len(got), got)
	}
}

// The case a single json.Decoder over the whole file CANNOT survive: a bad line in
// the middle. A Decoder has no way to resync, so it stopped there and silently
// dropped every later row for that day — permanently, and the shortened figure was
// still labelled "today". Only the final-line variant above passed under that
// behaviour, which is why this test exists.
func TestQuery_CorruptLineMidFileSkipsOnlyThatLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"2026-09-13T09:01:00Z","endpoint":"gw"`, // no closing brace: the damage
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300),
		line(base.Add(3*time.Minute), "gw", "m", 1, 40, 5, 400),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, caveats, err := w.Query(context.Background(), base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want the 3 intact ones — a mid-file corruption must not "+
			"truncate the rest of the day: %+v", len(got), got)
	}
	var total int64
	for _, r := range got {
		total += r.CostMicros
	}
	if total != 800 {
		t.Errorf("CostMicros total = %d, want 800 (100 + 300 + 400)", total)
	}
	// J4: the skip has to be VISIBLE. It was counted into a local and logged at
	// slog.Debug, below the default level, so in production this figure was
	// indistinguishable from a complete one. Read off THIS read's Caveats, not off the
	// writer: see Caveats for the two readers that swapped them.
	if caveats.SkippedLines != 1 {
		t.Errorf("SkippedLines = %d, want 1; a caller has no other way to tell this "+
			"800 from a day that really only cost 800", caveats.SkippedLines)
	}
	if caveats.TruncatedDays != 0 {
		t.Errorf("TruncatedDays = %d, want 0 — the read stepped over the damage and finished",
			caveats.TruncatedDays)
	}
}

// A skip and an abandoned tail are different sizes of loss, so they are reported
// separately: a skip costs the lines it names, and a truncation costs the rest of the
// file by an amount the file cannot state.
func TestQuery_AnAbandonedDayIsReportedSeparatelyFromSkippedLines(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"2026-09-13T09:01:00Z"`,                     // one skippable line
		`{"at":"`+strings.Repeat("x", maxLineBytes+1)+`"}`, // and then the wall
		line(base.Add(3*time.Minute), "gw", "m", 1, 40, 5, 400),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	_, caveats, err := w.Query(context.Background(), base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// AT LEAST ONE, not exactly one, because SkippedLines is documented as a FLOOR on rows
	// lost rather than a count of them: one undecodable line can hold two rows where a
	// crash fragment was concatenated with the next append (measured — see
	// TestQuery_FragmentConcatenatedWithTheNextAppendCostsOneLine), and this file loses a
	// second row to the wall below regardless. Pinning the exact 1 asserted a stronger
	// claim than the field makes, and would fail a reader that counted the truth more
	// completely.
	if caveats.SkippedLines < 1 {
		t.Errorf("SkippedLines = %d, want at least 1: the read stepped over a line that held "+
			"spend, and a caller has no other way to know the figure beside it is short",
			caveats.SkippedLines)
	}
	if caveats.TruncatedDays != 1 {
		t.Errorf("TruncatedDays = %d, want 1; a day the reader gave up on must not be "+
			"served as a complete one", caveats.TruncatedDays)
	}
}

// PER READ, not cumulative. A day file with one corrupt line is re-read on every
// /v1/usage request, so a running count would climb forever over one piece of damage and
// read as a fault that is getting worse. Returning the counts to the read that produced
// them is what makes that true for every caller at once rather than for whichever one
// sampled the writer last; see Caveats.
func TestQuery_ReadIssuesDescribeTheReadThatReturnedThem(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"2026-09-13T09:01:00Z"`,
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	for i := 0; i < 3; i++ {
		_, caveats, err := w.Query(context.Background(), base, base.Add(time.Hour))
		if err != nil {
			t.Fatalf("Query %d: %v", i, err)
		}
		if caveats.SkippedLines != 1 {
			t.Fatalf("read %d returned SkippedLines = %d, want 1 — one line of damage must not "+
				"read as %d lines of damage because it was queried %d times",
				i+1, caveats.SkippedLines, caveats.SkippedLines, i+1)
		}
	}

	// And a read of a clean day carries nothing, or a caveat would outlive the file it
	// described.
	clean := base.AddDate(0, 0, -1)
	writeDay(t, dir, clean, line(clean, "gw", "m", 1, 10, 5, 100))
	_, caveats, err := w.Query(context.Background(), clean, clean.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !caveats.Clean() {
		t.Errorf("a read of a clean day returned %+v, want no caveats", caveats)
	}
}

// The exact byte pattern the old write path produced: a fragment with no trailing
// newline, then a later append concatenated onto it. One line is unreadable and
// everything after it survives.
func TestQuery_FragmentConcatenatedWithTheNextAppendCostsOneLine(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	name := filepath.Join(dir, base.Format(dayLayout)+".jsonl")
	body := line(base, "gw", "m", 1, 10, 5, 100) + "\n" +
		`{"at":"2026-09-13T09:01:00Z","endpo` + // short write, no newline
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300) + "\n" +
		line(base.Add(3*time.Minute), "gw", "m", 1, 40, 5, 400) + "\n"
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, _, err := w.Query(context.Background(), base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// The fragment swallows the row it was concatenated with — one line lost, not the
	// day. The 09:03 row is the one that proves the read did not stop.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (the intact first and last): %+v", len(got), got)
	}
	if got[len(got)-1].CostMicros != 400 {
		t.Errorf("last row = %d micros, want the 400 that follows the damage", got[len(got)-1].CostMicros)
	}
}

// A line past maxLineBytes ends that day's read, and this asserts the LAST-RESORT
// guard, not a tolerated outcome.
//
// "A line this long is damage no ledger write can produce" is a false premise: with Model
// going to disk uncapped, one request naming a megabyte-long model wrote a single valid line
// past this limit and permanently destroyed the rest of that day ($3.00 of a $3.25 day,
// measured). The write path caps every label, so a line this long can only come from a file
// something else corrupted — TestRecord_LabelsAreCappedSoALineCanNeverExceedTheReadLimit is
// the half that keeps this unreachable from a request, and this half only says the reader
// stays bounded and keeps what preceded the damage.
func TestQuery_LineBeyondTheBufferLimitEndsThatDay(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"`+strings.Repeat("x", maxLineBytes+1)+`"}`,
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, caveats, err := w.Query(context.Background(), base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query must not fail the whole request over one day file: %v", err)
	}
	if len(got) != 1 || got[0].CostMicros != 100 {
		t.Errorf("got %+v, want the one row that preceded the oversized line", got)
	}
	// And it must SAY SO. A truncated day served as a complete one is how the short
	// figure reaches a client as window:"today", priced:true with no caveat.
	if caveats.TruncatedDays != 1 {
		t.Errorf("TruncatedDays = %d, want 1 — a day abandoned part-way must be visible "+
			"to the caller, not only in a log line", caveats.TruncatedDays)
	}
}

// The C1 fix, stated as the arithmetic the read-side guard now rests on: whatever a
// request puts in the label fields, the line this package writes stays far below
// maxLineBytes, so readDay's unskippable-line path is not reachable from a request.
func TestRecord_LabelsAreCappedSoALineCanNeverExceedTheReadLimit(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	// Every label at maxLineBytes+64, the shape that destroyed a day: a model name off
	// the request body, a Host, a User-Agent and a provenance.
	huge := strings.Repeat("x", maxLineBytes+64)
	e := costedEvent(t, huge, huge, 0.25, 100, 50)
	e.Client = &pipeline.EventClient{Raw: huge}
	setProvenance(t, e, huge)
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	b := readAllBytes(t, dir)
	if len(b) >= maxLineBytes {
		t.Fatalf("one row serialized to %d bytes; readDay refuses to buffer %d and cannot "+
			"step over it, so this row would end every future read of that day", len(b), maxLineBytes)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	for name, got := range map[string]string{
		"Endpoint": rows[0].Endpoint, "Model": rows[0].Model,
		"Agent": rows[0].Agent, "Provenance": rows[0].Provenance,
	} {
		if len(got) != maxLabelLen {
			t.Errorf("%s is %d bytes on disk, want it capped at %d", name, len(got), maxLabelLen)
		}
	}
}

// Control characters in a caller-controlled label never reach the day file.
//
// Model is off the request body, Endpoint is the host the workload asked for and Agent is
// the User-Agent verbatim, so all three carry whatever bytes a caller chose — into a file
// that is retained for retentionDays, that an operator cats, and that cannot be edited
// afterwards. An escape sequence there rewrites the terminal of whoever reads it, on every
// read, for as long as the file exists. CWE-150.
//
// C1 IS IN HERE DELIBERATELY. The sanitiser filtered C0 and DEL only, and this test could
// not see that: U+009B is the single-character CSI, so "\u009b2J" clears the pane of
// whoever cats the file with no ESC byte for a C0 filter to catch, and the assertion that
// was meant to prove no control character survived was reading a predicate with the same
// blind spot.
func TestRecord_ControlCharactersNeverReachADayFile(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	// An SGR sequence that recolours the pane, a newline that breaks a table apart, a
	// carriage return that erases the line reporting it — and a C1 CSI, which is the same
	// attack as the first one with the ESC byte removed.
	e := costedEvent(t, "gw\x1b[31m", "opus\nnext-line", 0.25, 100, 50)
	e.Client = &pipeline.EventClient{Raw: "curl/8.4\r\x07"}
	setProvenance(t, e, "gateway\u009b2J")
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	for name, got := range map[string]string{
		"Endpoint": rows[0].Endpoint, "Model": rows[0].Model, "Agent": rows[0].Agent,
		"Provenance": rows[0].Provenance,
	} {
		if hasControlRunes(got) {
			t.Errorf("%s = %q on disk: a control character reached a durable row", name, got)
		}
		// The C1 half asserted on its own, because a byte-scanning predicate reports the row
		// clean while the CSI is still in it — two bytes, 0xC2 0x9B, which a terminal decoding
		// UTF-8 acts on exactly as it acts on ESC [.
		if strings.ContainsRune(got, '\u009b') {
			t.Errorf("%s = %q on disk: U+009B (CSI) survived; a C0-only filter does not "+
				"protect the operator who cats this file", name, got)
		}
		// REPLACED, not dropped: "gw[31m" would read as a plausible hostname and hide the
		// tampering, which is the whole reason sanitizeLabel substitutes rather than deletes.
		if !strings.Contains(got, "�") {
			t.Errorf("%s = %q: the removed bytes left no trace, so tampering is invisible",
				name, got)
		}
	}
	// The readable part survives — this is sanitisation, not rejection.
	if !strings.HasPrefix(rows[0].Model, "opus") {
		t.Errorf("Model = %q, want the label itself kept around the replacement", rows[0].Model)
	}
}

// SANITISE THEN CAP, in that order, because the substitution can TRIPLE a label: every
// replaced byte becomes three of U+FFFD. Capping first and substituting afterwards puts
// 3 x maxLabelLen bytes on the line for a label that is entirely control bytes, which is
// the length bound maxLabelLen exists to guarantee — and that bound is what keeps
// readDay's unskippable-line path out of reach of a request.
func TestRecord_ASanitisedLabelIsStillCappedInBYTES(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	// Every byte a control byte, comfortably past the cap.
	hostile := strings.Repeat("\x1b", maxLabelLen+32)
	e := costedEvent(t, hostile, hostile, 0.25, 100, 50)
	e.Client = &pipeline.EventClient{Raw: hostile}
	setProvenance(t, e, hostile)
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	for name, got := range map[string]string{
		"Endpoint": rows[0].Endpoint, "Model": rows[0].Model,
		"Agent": rows[0].Agent, "Provenance": rows[0].Provenance,
	} {
		if len(got) > maxLabelLen {
			t.Errorf("%s is %d BYTES on disk, want at most %d: the cap was applied before the "+
				"substitution, so each byte it kept grew to three", name, len(got), maxLabelLen)
		}
	}
}

// A label is cut on a RUNE boundary, and the cap is still counted in BYTES.
//
// A plain byte slice halves a multi-byte rune straddling byte 96 and puts an invalid UTF-8
// sequence into an append-only file that other tools parse and that cannot be corrected
// afterwards. Two ways to reach it, both here: a model name a workload chose (ordinary
// non-ASCII), and a label this package rewrote itself, where every replacement is a 3-byte
// U+FFFD and a byte cut has a two-in-three chance of splitting one.
//
// THE BYTE CAP STAYS. It is what bounds the line length maxLabelLen exists to guarantee,
// so the assertion is <= maxLabelLen bytes AND valid UTF-8 — not a rune count.
//
// AND THE BYTE CUT BREAKS THE BYTE CAP, which is the argument for this fix that the
// UTF-8 one obscures. Measured against the mutation: a 121-byte label cut at 96 leaves a
// 2-byte fragment, and encoding/json expands each invalid byte into a 3-byte U+FFFD, so
// the label lands on disk at 100 bytes — past the cap the line-length arithmetic rests on.
// Cutting cleanly is what makes maxLabelLen mean what it says.
//
// EVERY CASE CARRIES A ONE-BYTE PREFIX, and without it this test proves nothing:
// maxLabelLen is 96, which is a multiple of both 2 and 3, so a label made only of 2- or
// 3-byte runes has a rune boundary exactly at byte 96 and even a plain byte cut lands
// cleanly. The first version of this test had no prefix and passed against the byte cut it
// was written to catch. The prefix is what puts a rune across the cap.
func TestRecord_ALabelIsCutOnARuneBoundary(t *testing.T) {
	for _, tc := range []struct{ name, label string }{
		// 3-byte runes behind one ASCII byte, so byte 96 falls in the middle of one.
		{"multi-byte model name", "m" + strings.Repeat("模", maxLabelLen/3+8)},
		// Sanitised control bytes, which become 3-byte U+FFFD runes on the way through —
		// this package's own output, cut by this package's own cap.
		{"a label this package rewrote", "m" + strings.Repeat("\x1b", maxLabelLen+32)},
		// A 2-byte rune, which straddles a different byte offset.
		{"two-byte runes", "m" + strings.Repeat("é", maxLabelLen)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			now := at
			w := newTestWriter(t, dir, func() time.Time { return now })

			e := costedEvent(t, tc.label, tc.label, 0.25, 100, 50)
			e.Client = &pipeline.EventClient{Raw: tc.label}
			setProvenance(t, e, tc.label)
			w.Record("s1", e)

			now = at.Add(time.Minute)
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			rows := readAllRows(t, dir)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
			}
			for name, got := range map[string]string{
				"Endpoint": rows[0].Endpoint, "Model": rows[0].Model,
				"Agent": rows[0].Agent, "Provenance": rows[0].Provenance,
			} {
				if len(got) > maxLabelLen {
					t.Errorf("%s is %d BYTES on disk, want at most %d", name, len(got), maxLabelLen)
				}
				if !utf8.ValidString(got) {
					t.Errorf("%s = %q on disk is not valid UTF-8: the cut split a rune, and this "+
						"file is append-only", name, got)
				}
			}
			// The bytes on disk, not only the decoded row: json.Marshal substitutes U+FFFD for
			// invalid input, so a decoded row can read as valid while the file it came from
			// carried the fragment. Asserting the file is what makes this about the file.
			if !utf8.Valid(readAllBytes(t, dir)) {
				t.Error("the day file is not valid UTF-8")
			}
		})
	}
}

// The whole C1 measurement, end to end: one hostile label followed by real spend, and
// the day's total must survive. This is the assertion the old blessing test made
// impossible to write.
func TestQuery_AHostileLabelCannotDestroyTheRestOfTheDay(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	// Minute 0: the attempt.
	w.Record("s1", costedEvent(t, "gw", strings.Repeat("x", maxLineBytes+64), 0.25, 100, 50))
	// Minutes 1..5: ordinary priced traffic, $0.25 + 5 x $0.60 = $3.25 in all.
	for i := 1; i <= 5; i++ {
		now = at.Add(time.Duration(i) * time.Minute)
		e := costedEvent(t, "gw", "opus", 0.60, 100, 50)
		e.At = now
		w.Record("s1", e)
	}
	now = at.Add(6 * time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows, _, err := w.Query(context.Background(), at.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	totals, _, _, _ := Fold(rows, usage.GroupNone)
	if want := int64(3_250_000); totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d; 250000 is the measured loss — the oversized "+
			"line ended the day's read and took every later minute with it",
			totals.CostMicros, want)
	}
}

// Only CLOSED minutes reach disk, so the disk half of the ledger must not see the
// open one. Window is what adds it back; if both halves owned a minute, a reader
// composing them would double-count it.
func TestQuery_DoesNotSeeTheOpenMinute(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	got, _, err := w.Query(context.Background(), at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows while the minute was open, want 0: %+v", len(got), got)
	}
}

// The defect this whole seam exists to close: a turn whose spend all happened
// inside the current minute has NOTHING on disk, and a reader that saw only the day
// files would answer "no spend today" over real money.
func TestWindow_IncludesTheOpenMinuteWithNothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	if disk, _, err := w.Query(context.Background(), at.Add(-time.Hour), at); err != nil || len(disk) != 0 {
		t.Fatalf("disk half = %d rows (err %v), want 0 — the premise of this test", len(disk), err)
	}

	rows, _, err := w.Window(context.Background(), at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	totals, _, _, _ := Fold(rows, usage.GroupNone)
	if totals.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want 250000 from the open minute", totals.CostMicros)
	}
	if totals.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1 — priced:false here reads as $0.00 for a day with spend",
			totals.PricedRequests)
	}
}

// THE non-overlap proof. The same two events are counted once while the minute is
// open and once after it has been written, and the total must not move: if either
// half leaked the other's minute, this figure would double.
func TestWindow_CountsAMinuteExactlyOnceAcrossTheFlush(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	from, to := at.Add(-time.Hour), at.Add(time.Hour)
	before, _, err := w.Window(context.Background(), from, to)
	if err != nil {
		t.Fatalf("Window while open: %v", err)
	}
	openTotals, _, _, _ := Fold(before, usage.GroupNone)

	// Roll the minute: the same spend moves from memory to disk.
	now = at.Add(time.Minute)
	later := costedEvent(t, "gw", "m", 0.10, 10, 5)
	later.At = now
	w.Record("s1", later)
	// The closed minute reaches disk on the writer goroutine, so wait for it: the
	// question here is whether BOTH halves claim it, which needs it to be in one.
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	after, _, err := w.Window(context.Background(), from, to)
	if err != nil {
		t.Fatalf("Window after the roll: %v", err)
	}
	closedTotals, _, _, _ := Fold(after, usage.GroupNone)

	// The first minute's 500000 is now on disk and the second minute's 100000 is held.
	if openTotals.CostMicros != 500_000 {
		t.Errorf("open-minute total = %d, want 500000", openTotals.CostMicros)
	}
	if closedTotals.CostMicros != 600_000 {
		t.Errorf("total after the roll = %d, want 600000 (500000 on disk + 100000 held); "+
			"1100000 would mean the first minute was counted in both halves", closedTotals.CostMicros)
	}
	if closedTotals.Requests != 3 {
		t.Errorf("Requests = %d, want 3", closedTotals.Requests)
	}
}

// THE RESTART MEASUREMENT, driven through two Writers over one directory because that
// is what a restart is. Nothing exotic: a config reload, a crash loop or a rollout puts
// process 2 inside the same minute process 1 was recording, and every row process 1 had
// already committed was then dropped as a duplicate.
//
//	on disk after p1: 1000000 micros
//	Window() saw:      250000 micros
//	Dropped():              0
//
// The property is one-directional and stated that way on purpose: Window must report AT
// LEAST what the day files hold, whatever it does with its own memory. See Window for
// why no rule that reads the disk rows can tell this state from a racing flush, and
// TestWindow_AFlushRacingTheReadIsCountedExactlyOnce for the other side of the trade.
func TestWindow_ARestartInTheSameMinuteHidesNothingAlreadyCommitted(t *testing.T) {
	dir := t.TempDir()
	now := at
	clock := func() time.Time { return now }

	// Process 1: $1.00 recorded, flushed and stopped inside minute M.
	p1 := newTestWriter(t, dir, clock)
	p1.Record("s1", costedEvent(t, "gw", "m", 1.00, 100, 50))
	if err := p1.Close(); err != nil {
		t.Fatalf("p1.Close: %v", err)
	}
	var committed int64
	for _, r := range readAllRows(t, dir) {
		committed += r.CostMicros
	}
	if committed != 1_000_000 {
		t.Fatalf("on disk after p1 = %d micros, want 1000000 — the premise of this test", committed)
	}

	// Process 2: same directory, same minute, $0.25 of new spend held in memory.
	p2 := newTestWriter(t, dir, clock)
	p2.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	rows, caveats, err := p2.Window(context.Background(), at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	totals, _, _, _ := Fold(rows, usage.GroupNone)
	if totals.CostMicros < committed {
		t.Errorf("Window() = %d micros, BELOW the %d already on disk — a committed row is "+
			"hidden, and nothing reports it: Dropped() = %d, read caveats = %+v",
			totals.CostMicros, committed, p2.Dropped(), caveats)
	}
	if want := int64(1_250_000); totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d ($1.00 on disk + $0.25 held); 250000 is the "+
			"measured loss", totals.CostMicros, want)
	}
	if got := p2.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0 — nothing was dropped here; the point is that the "+
			"loss was invisible to every signal the ledger has", got)
	}
}

// The other side of that trade: a flush that lands in exactly the gap Window's
// reconciliation exists for must still be counted ONCE.
//
// DRIVEN, NOT HAND-SEEDED, and it has to stay that way. Writing a row for the held minute
// straight into the day file and asserting that Window dropped it is a fixture
// indistinguishable from the restart above, so it pins the behaviour that hid $1.00.
// betweenWindowReads lands a real flush between the memory read and the disk read instead —
// the only state the drop is justified by — so the assertion is about arithmetic rather than
// about a state nothing produced.
func TestWindow_AFlushRacingTheReadIsCountedExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Flush synchronously, so by the time the day files are read the held minute is
	// certainly on disk AND certainly still in the snapshot taken a moment earlier.
	var flushes int
	w.betweenWindowReads = func() {
		flushes++
		if err := w.Flush(); err != nil {
			t.Errorf("Flush during the read: %v", err)
		}
	}

	rows, _, err := w.Window(context.Background(), at.Add(-time.Hour), at)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if flushes != 1 {
		t.Fatalf("the seam fired %d times, want 1 — this test asserts nothing otherwise", flushes)
	}
	totals, _, _, _ := Fold(rows, usage.GroupNone)
	if want := int64(500_000); totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d counted exactly once; 1000000 means the minute "+
			"was counted from memory AND from disk, which is the failure the reconciliation "+
			"exists to prevent", totals.CostMicros, want)
	}
	if totals.Requests != 2 {
		t.Errorf("Requests = %d, want 2 — 4 is the same double count in the denominator",
			totals.Requests)
	}
}

// A cancelled caller stops the day walk instead of reading to the end for nobody. The
// ledger read is the only unbounded IO this package does on a request path: one
// os.Open-plus-scan per day in the window, against an operator-configured path.
func TestQuery_ACancelledContextStopsTheReadBeforeAnyIO(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base, line(base, "gw", "m", 1, 10, 5, 250_000))
	w := newTestWriter(t, dir, func() time.Time { return base })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rows, _, err := w.Query(ctx, base, base.Add(time.Minute))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Query on a cancelled context: err = %v, want context.Canceled", err)
	}
	// NO PARTIAL ANSWER. A short slice with a nil error would be a total missing rows
	// with nothing saying so, which is the failure SkippedLines exists to make visible.
	if len(rows) != 0 {
		t.Errorf("got %d rows, want none: an abandoned read must not return a partial day",
			len(rows))
	}

	// And Window, which is what a reader actually calls, propagates it rather than
	// answering from memory alone.
	if _, _, werr := w.Window(ctx, base, base.Add(time.Minute)); !errors.Is(werr, context.Canceled) {
		t.Errorf("Window on a cancelled context: err = %v, want context.Canceled", werr)
	}
}

// A disk row for a minute ABOVE the one held must survive the read.
//
// Dropping everything "at or after" the held minute rests on the claim that a concurrent
// flush can only produce rows the pending snapshot already had. False — a flush landing
// between pending() and Query() can advance the writer several minutes, and those newer
// minutes are on disk and NOT in a snapshot taken before them. Measured as $1.00 dropped from
// $1.25. Two processes sharing cost_ledger.dir reach the same state with no race at all,
// which the ~/.cortex/cost default makes plausible.
func TestWindow_KeepsADiskRowAboveTheHeldMinute(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	// Minute M, held in memory: $0.25.
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Minute M+1, on disk: $1.00. What a flush that advanced the writer between Window's
	// two reads leaves behind, or what another process writing the same directory does.
	next := at.Truncate(time.Minute).Add(time.Minute)
	writeDay(t, dir, next, line(next, "gw", "m", 1, 100, 50, 1_000_000))

	rows, _, err := w.Window(context.Background(), at.Add(-time.Hour), next.Add(time.Hour))
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	totals, _, _, _ := Fold(rows, usage.GroupNone)
	if want := int64(1_250_000); totals.CostMicros != want {
		t.Errorf("CostMicros = %d, want %d; 250000 is the measured loss — a minute above "+
			"the held one is not the held one and must not be dropped as an overlap",
			totals.CostMicros, want)
	}
}

// The held minute is not always in the window asked for. An idle proxy at 00:05
// still holds yesterday's last minute, and that spend is yesterday's.
func TestWindow_ExcludesAHeldMinuteOutsideTheRange(t *testing.T) {
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)
	yesterday := midnight.Add(-30 * time.Second) // 23:59:30
	now := yesterday
	w := newTestWriter(t, dir, func() time.Time { return now })
	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.At = yesterday
	w.Record("s1", e)

	// "today" as ParseWindowSpec builds it: local midnight to now.
	now = midnight.Add(5 * time.Minute)
	rows, _, err := w.Window(context.Background(), midnight, now)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows; yesterday's held minute must not land in today: %+v", len(rows), rows)
	}
}

// A reversed range is a caller mistake, not a reason to return nothing: swapping
// answers the question that was meant instead of an empty result a client would
// render as "no spend".
func TestQuery_ReversedRangeIsNormalised(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base, line(base, "gw", "m", 1, 10, 5, 100))
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, _, err := w.Query(context.Background(), base.Add(time.Hour), base)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d rows for a reversed range, want 1", len(got))
	}
}

func TestFold_ByModelSumsToTotals(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Endpoint: "gw", Model: "opus", Counts: usage.Counts{
			Requests: 2, InputTokens: 100, CostMicros: 300, PricedRequests: 2, PriceableRequests: 2}},
		{At: base, Endpoint: "gw", Model: "haiku", Counts: usage.Counts{
			Requests: 1, InputTokens: 20, CostMicros: 50, PricedRequests: 1, PriceableRequests: 1}},
	}

	// A row with no model, which is spend the breakdown CANNOT attribute. Without one in
	// the input this test compares a value to itself: Fold adds every row's Counts to
	// totals and the groupable ones to series, so with all rows groupable "sum == totals"
	// is the same accumulation twice and cannot fail whatever labelFor does. The
	// unattributable row is what makes the reconciliation an assertion.
	rows = append(rows, Row{At: base, Endpoint: "gw", Counts: usage.Counts{
		Requests: 1, InputTokens: 10, CostMicros: 25, PricedRequests: 1, PriceableRequests: 1}})

	totals, series, ungrouped, _ := Fold(rows, usage.GroupModel)

	if totals.CostMicros != 375 {
		t.Errorf("totals.CostMicros = %d, want 375 (300 + 50 + the 25 with no model)", totals.CostMicros)
	}
	if series["opus"].CostMicros != 300 || series["haiku"].CostMicros != 50 {
		t.Errorf("series = %+v, want opus 300 and haiku 50", series)
	}
	// The gap is DISCLOSED rather than hidden or double-counted. This is the half that can
	// actually break: Fold publishes a residual only when the group is one the ledger can
	// answer AND the ring calls reconcilable, and getting that wrong once made
	// group=status answer with a residual equal to its entire total.
	if ungrouped.Micros != 25 {
		t.Errorf("ungrouped = %d, want 25 — spend with no model must be disclosed as the "+
			"residual, not folded into a series key or dropped", ungrouped.Micros)
	}
	// The property that makes a breakdown table trustworthy, stated as the identity that
	// holds when the residual is right: the rows plus the residual account for the total
	// they sit under.
	var sum int64
	for _, c := range series {
		sum += c.CostMicros
	}
	if sum+ungrouped.Micros != totals.CostMicros {
		t.Errorf("series sums to %d, residual is %d, totals is %d; a client cannot reconcile "+
			"the table it was given with the figure above it", sum, ungrouped.Micros, totals.CostMicros)
	}
	// And the residual is not vacuously zero, which would make the identity above a
	// tautology.
	if ungrouped.Micros == 0 {
		t.Fatal("the residual is zero, so the reconciliation above proves nothing")
	}
}

func TestFold_ByEndpoint(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Endpoint: "gw-a", Model: "m", Counts: usage.Counts{Requests: 1, CostMicros: 10}},
		{At: base, Endpoint: "gw-b", Model: "m", Counts: usage.Counts{Requests: 1, CostMicros: 20}},
	}

	_, series, _, _ := Fold(rows, usage.GroupEndpoint)

	if series["gw-a"].CostMicros != 10 || series["gw-b"].CostMicros != 20 {
		t.Errorf("series = %+v, want gw-a 10 and gw-b 20", series)
	}
}

func TestFold_MethodIsAnAliasForModel(t *testing.T) {
	// Same equivalence the live path guarantees: two spellings, one series.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "opus", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}

	_, byModel, _, _ := Fold(rows, usage.GroupModel)
	_, byMethod, _, _ := Fold(rows, usage.GroupMethod)

	// THE EQUALITY IS VACUOUS ON TWO EMPTY MAPS, and Fold returning nothing for both spellings is
	// exactly what a broken alias would look like — so what the series CONTAINS is asserted first.
	if got := byModel["opus"]; got.Requests != 1 || got.CostMicros != 10 {
		t.Fatalf("byModel[\"opus\"] = %+v, want 1 request at 10 micros: with an empty series the comparison below holds for the wrong reason", got)
	}
	if len(byModel) != len(byMethod) || byModel["opus"] != byMethod["opus"] {
		t.Errorf("model series %+v and method series %+v differ", byModel, byMethod)
	}
}

func TestFold_UnpricedRowsCountButCostNothing(t *testing.T) {
	// The coverage gap must survive the round trip to disk, or a partial total
	// reads as a complete one.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "m", Counts: usage.Counts{Requests: 1, PriceableRequests: 1}}}

	totals, _, _, _ := Fold(rows, usage.GroupModel)

	if totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", totals.CostMicros)
	}
	if totals.PricedRequests != 0 || totals.PriceableRequests != 1 {
		t.Errorf("coverage = %d/%d, want 0/1", totals.PricedRequests, totals.PriceableRequests)
	}
}

// The inexactness caveat must survive the fold as well as the round trip: it is
// carried by the embedded usage.Counts, so Counts.Add is what makes it work, and a
// fold that dropped it would report a floor as an exact figure.
func TestFold_CarriesTheIncompleteCount(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Model: "m", Counts: usage.Counts{
			Requests: 1, CostMicros: 100, PricedRequests: 1, PriceableRequests: 1, IncompleteRequests: 1}},
		{At: base, Model: "m", Counts: usage.Counts{
			Requests: 1, CostMicros: 200, PricedRequests: 1, PriceableRequests: 1}},
	}

	totals, series, _, _ := Fold(rows, usage.GroupModel)

	if totals.IncompleteRequests != 1 {
		t.Errorf("totals.IncompleteRequests = %d, want 1", totals.IncompleteRequests)
	}
	if series["m"].IncompleteRequests != 1 {
		t.Errorf("series IncompleteRequests = %d, want 1", series["m"].IncompleteRequests)
	}
	// A subset, not a deduction: both requests stay priced and both figures stay in.
	if totals.PricedRequests != 2 || totals.CostMicros != 300 {
		t.Errorf("priced = %d, cost = %d; want 2 and 300", totals.PricedRequests, totals.CostMicros)
	}
}

// Avoided cost must survive the fold in its own column, on the same footing as the caveat
// above and for the same mechanical reason — Row embeds usage.Counts, so Counts.Add carries
// it — and it must not join the spend total on the way.
//
// The two are asserted TOGETHER because that is the only pairing that can fail informatively:
// a fold that added savings to spend reports 300 avoided and 900 spent, and a fold that
// dropped them reports 0 avoided and 600 spent. Either alone leaves one of those undetected.
//
// Also the mixed case on purpose — one row with a saving, one without — because a window is
// aggregated from minutes and most minutes have no saving in them at all.
func TestFold_CarriesAvoidedCostWithoutAddingItToSpend(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Model: "m", Counts: usage.Counts{
			Requests: 1, CostMicros: 400, PricedRequests: 1, PriceableRequests: 1, AvoidedMicros: 300}},
		{At: base, Model: "m", Counts: usage.Counts{
			Requests: 1, CostMicros: 200, PricedRequests: 1, PriceableRequests: 1}},
	}

	totals, series, _, _ := Fold(rows, usage.GroupModel)

	if totals.AvoidedMicros != 300 {
		t.Errorf("totals.AvoidedMicros = %d, want 300", totals.AvoidedMicros)
	}
	if series["m"].AvoidedMicros != 300 {
		t.Errorf("series AvoidedMicros = %d, want 300", series["m"].AvoidedMicros)
	}
	if totals.CostMicros != 600 {
		t.Errorf("CostMicros = %d, want 600 — the two priced figures and nothing else; 900 "+
			"means the saving was added to spend", totals.CostMicros)
	}
}

func TestFold_EmptyLabelIsNeverASeriesKey(t *testing.T) {
	// A blank row in a breakdown table reads as a bug rather than as missing
	// attribution — the same guard the live foldInto applies.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}

	totals, series, _, _ := Fold(rows, usage.GroupModel)

	if _, ok := series[""]; ok {
		t.Error(`series has an "" key`)
	}
	if totals.CostMicros != 10 {
		t.Errorf("CostMicros = %d, want the row still counted in totals", totals.CostMicros)
	}
}

func TestFold_GroupNoneReturnsNoSeries(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	totals, series, _, _ := Fold([]Row{{At: base, Counts: usage.Counts{Requests: 1, CostMicros: 10}}}, usage.GroupNone)
	if totals.CostMicros != 10 {
		t.Errorf("CostMicros = %d, want 10", totals.CostMicros)
	}
	if series != nil {
		t.Errorf("series = %+v, want nil for GroupNone", series)
	}
}

// A ledger row carries no session, status or plugin, so those groupings produce no
// series rather than a misleading one. The totals still stand.
func TestFold_GroupingsTheLedgerCannotAnswerReturnNoSeries(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "m", Endpoint: "gw", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}
	for _, g := range []usage.Group{usage.GroupSession, usage.GroupStatus, usage.GroupPlugin} {
		totals, series, ungrouped, _ := Fold(rows, g)
		if series != nil {
			t.Errorf("group=%s produced a series %+v; the ledger holds no such column", g, series)
		}
		if totals.CostMicros != 10 {
			t.Errorf("group=%s totals.CostMicros = %d, want 10", g, totals.CostMicros)
		}
		// NO RESIDUAL, and the opposite reading is the one that shipped: treating an axis a
		// persisted row cannot represent as a breakdown short by everything made
		// GET /v1/usage?window=today&group=status answer with series: null and
		// ungroupedCostMicros equal to Totals.CostMicros — a response asserting that none of
		// the money in it could be accounted for, over traffic where every dollar had an
		// endpoint, a model and an agent. usage.Group.Reconcilable refuses GroupNone for
		// exactly that reason ("a residual equal to the entire total ... would read as a
		// fault"), and these two axes reach the same state by a different door.
		//
		// A residual is a statement about a breakdown that ALMOST accounts for the total.
		// Where the source can produce no breakdown at all there is nothing for it to be a
		// residual of, and the honest response says which grouping was actually applied
		// instead — see Groupable and sessionapi's ledgerSnapshot.
		if ungrouped.Micros != 0 {
			t.Errorf("group=%s ungrouped cost = %d over a total of %d, want 0: the ledger cannot "+
				"group by this axis at all, which is not the same claim as a breakdown that fell "+
				"short by 100%%", g, ungrouped.Micros, totals.CostMicros)
		}
	}
}

// THE RESIDUAL A PRICED ROW WITH NO MODEL LEAVES IN group=model.
//
// The ledger keeps a gateway-priced response the inference parser could not read —
// /v1/embeddings, /v1/rerank — and such a row is stored with Model "" because there was
// no model on the wire. It counts toward the total, labelFor returns ok=false for it,
// and so a client summing the group=model series got less than Totals.CostMicros with
// nothing in the response to account for the difference. This is the LEDGER half of the
// claim usage.Snapshot.UngroupedCostMicros makes about both window kinds;
// TestSnapshot_GatewayPricedTrafficWithNoModelIsDisclosedAsUngrouped is the ring half.
func TestFold_GatewayPricedRowWithNoModelIsDisclosedAsUngrouped(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Endpoint: "gw", Model: "opus", Counts: usage.Counts{
			Requests: 1, CostMicros: 100_000, PricedRequests: 1, PriceableRequests: 1}},
		// The unparsed one: priced, no model, no tokens. See
		// TestRecord_APricedResponseWithNoInferenceExtensionIsStillRecorded for how it is
		// written.
		{At: base, Endpoint: "gw", Model: "", Counts: usage.Counts{
			Requests: 1, CostMicros: 250_000, PricedRequests: 1, PriceableRequests: 1}},
	}

	totals, series, ungrouped, _ := Fold(rows, usage.GroupModel)

	if totals.CostMicros != 350_000 {
		t.Fatalf("totals.CostMicros = %d, want 350000 — both rows are real spend", totals.CostMicros)
	}
	var sum int64
	for _, c := range series {
		sum += c.CostMicros
	}
	if sum != 100_000 {
		t.Errorf("series sums to %d, want 100000 (only the row that named a model)", sum)
	}
	if ungrouped.Micros != 250_000 {
		t.Errorf("ungrouped cost = %d, want 250000: without it a client summing group=model is "+
			"short of Totals.CostMicros by that much and the response says nothing about why",
			ungrouped.Micros)
	}
	if sum+ungrouped.Micros != totals.CostMicros {
		t.Errorf("series (%d) + ungrouped (%d) = %d, want totals %d — the reconciliation the "+
			"field exists to restore", sum, ungrouped.Micros, sum+ungrouped.Micros, totals.CostMicros)
	}
	// The same row is fully attributable on the axis it DOES carry, so that grouping has
	// nothing to disclose. A residual that showed up on every axis regardless would train a
	// client to ignore it.
	if _, _, byEndpoint, _ := Fold(rows, usage.GroupEndpoint); byEndpoint.Micros != 0 {
		t.Errorf("group=endpoint ungrouped cost = %d, want 0 — both rows carry an endpoint", byEndpoint.Micros)
	}
}

// A RESIDUAL THAT REACHES THE int64 CEILING IS A BOUND, NOT A FABRICATED DEFECT REPORT.
//
// Fold accumulated it with a bare `+=` while every field of the Counts beside it went
// through Counts.Add's saturating accumulate. Two rows near the ceiling therefore wrapped
// the residual NEGATIVE — and a negative residual is not merely a wrong number here.
// usage.Snapshot.SetUngroupedCost reads one as the series having overshot its own total and
// publishes SeriesOvershootMicros, a field whose doc tells the reader that this process is
// wrong about its own arithmetic and to file a bug. So the wrap turned correct-but-clamped
// data into a bug report about correct data, and lost the real residual while doing it.
//
// REACHABLE FROM FILE CONTENT, which is why this is pinned at Fold rather than left to the
// type's own unit test. Rows arrive from readDay, which json.Unmarshals each line with no
// bound on costMicros (store.go), so any int64 a day file holds reaches this loop. The
// WRITER cannot produce such a row — every event it sums is capped at
// pricing.MaxPlausibleRequestCostMicros, $10,000 — so the row this test builds is one a
// corrupted or hand-edited file yields, which is exactly the input the read path already
// has skippedLines and TruncatedDays to talk about.
//
// The assertions are ordered worst-first: the sign, then the magnitude, then the
// disclosure. Reverting the accumulate to `+=` fails on the sign, which is the one that
// says the arithmetic wrapped rather than clamped.
func TestFold_ASaturatedResidualIsABoundNotAFabricatedOvershoot(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	// Two of these sum to math.MaxInt64+1: the first value past the range, so the clamp is
	// exercised at its edge rather than deep inside it.
	half := int64(math.MaxInt64/2) + 1
	// No model, so both rows are residual rather than series entries.
	rows := []Row{
		{At: base, Endpoint: "gw", Model: "", Counts: usage.Counts{
			Requests: 1, CostMicros: half, PricedRequests: 1, PriceableRequests: 1}},
		{At: base, Endpoint: "gw", Model: "", Counts: usage.Counts{
			Requests: 1, CostMicros: half, PricedRequests: 1, PriceableRequests: 1}},
	}

	totals, _, ungrouped, _ := Fold(rows, usage.GroupModel)

	if ungrouped.Micros < 0 {
		t.Fatalf("residual = %d — negative, so the accumulate wrapped. A negative residual is "+
			"published as SeriesOvershootMicros, which tells an operator this process lost track "+
			"of its own arithmetic; the input here is merely large", ungrouped.Micros)
	}
	if ungrouped.Micros != math.MaxInt64 {
		t.Errorf("residual = %d, want math.MaxInt64: the clamp is the largest figure that can be "+
			"stated, and anything less understates spend the rows really carry", ungrouped.Micros)
	}
	if !ungrouped.Saturated {
		t.Error("residual clamped without saying so — the clamp is only honest while the flag " +
			"travels with it, which is the whole argument for usage.CostSum being a type")
	}

	// End to end through the setter, because "does not wrap" is not the claim that matters to
	// a client — "does not report a defect that did not happen" is.
	snap := usage.Snapshot{Totals: totals}
	snap.SetUngroupedCost(ungrouped)
	if snap.SeriesOvershootMicros != nil {
		t.Errorf("SeriesOvershootMicros = %d over rows whose breakdown is simply absent: the "+
			"field means this process double-counted, and publishing it here sends an operator "+
			"after a bug in the aggregator instead of at the day file", *snap.SeriesOvershootMicros)
	}
	if !snap.Totals.Saturated {
		t.Error("Totals.Saturated is false while the residual clamped — that field is the one " +
			"place a client is told to read every money figure here as a bound")
	}
}

// An unpriced row leaves no residual, because there are no dollars to be short OF. The
// coverage gap it does represent is answered by PricedRequests against
// PriceableRequests, which is a different question and must not be conflated: a client
// rendering this as a money band would invent spend that never happened.
func TestFold_AnUnpricedRowWithNoModelAddsNothingToTheResidual(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Endpoint: "gw", Model: "", Counts: usage.Counts{
		Requests: 1, PriceableRequests: 1}}}

	_, _, ungrouped, _ := Fold(rows, usage.GroupModel)

	if ungrouped.Micros != 0 {
		t.Errorf("ungrouped cost = %d over a row that cost nothing, want 0", ungrouped.Micros)
	}
}

// TestFold_GroupAgentBreaksDownByAgent covers labelFor's usage.GroupAgent arm, which
// the ledger shipped without because the constant did not exist yet.
func TestFold_GroupAgentBreaksDownByAgent(t *testing.T) {
	rows := []Row{
		{Endpoint: "gw", Model: "m", Agent: "claude-code/2.1.14",
			Counts: usage.Counts{Requests: 1, CostMicros: 100, InputTokens: 10}},
		{Endpoint: "gw", Model: "m", Agent: "opencode/0.4.2",
			Counts: usage.Counts{Requests: 1, CostMicros: 200, InputTokens: 20}},
	}

	totals, series, _, _ := Fold(rows, usage.GroupAgent)

	if totals.CostMicros != 300 {
		t.Errorf("totals.CostMicros = %d, want 300", totals.CostMicros)
	}
	if got := series["claude-code/2.1.14"].CostMicros; got != 100 {
		t.Errorf("claude-code CostMicros = %d, want 100", got)
	}
	if got := series["opencode/0.4.2"].CostMicros; got != 200 {
		t.Errorf("opencode CostMicros = %d, want 200", got)
	}
}

// TestFold_GroupAgentMapsAbsenceToUnknown is the consistency guarantee between the
// ledger and the live aggregator.
//
// The two differ in REPRESENTATION on purpose — the ledger stores "" so the durable
// file stays lossless, the aggregator's series keys are display strings — but they
// must not differ in what a client SEES for the same traffic. Mapping "" to the same
// reserved "unknown" bucket the aggregator uses is what makes group=agent answer
// identically whether it was served from the ring or from disk.
//
// This is a deliberate departure from how labelFor treats an absent endpoint or
// model, which produce no series entry at all. That is right for those axes: a row
// with no model is not inference, so it is not ABOUT that axis. An absent agent is
// different — the spend certainly happened and certainly belongs somewhere in a
// per-agent breakdown, which is also why the aggregator's byAgent has no guard where
// byEndpoint.Micros and byMethod do.
func TestFold_GroupAgentMapsAbsenceToUnknown(t *testing.T) {
	rows := []Row{
		{Endpoint: "gw", Model: "m", Agent: "claude-code/2.1.14",
			Counts: usage.Counts{Requests: 1, CostMicros: 100}},
		{Endpoint: "gw", Model: "m", Agent: "",
			Counts: usage.Counts{Requests: 1, CostMicros: 200}},
	}

	totals, series, _, _ := Fold(rows, usage.GroupAgent)

	if _, blank := series[""]; blank {
		t.Error(`series has an "" key; a blank row reads as a bug rather than as unattributed traffic`)
	}
	if got := series["unknown"].CostMicros; got != 200 {
		t.Errorf("unknown CostMicros = %d, want 200", got)
	}
	// The series sums to the total, which is the property the mapping buys: dropping
	// unattributed rows from the breakdown would leave a client unable to reconcile
	// a per-agent table against the figure beside it.
	var sum int64
	for _, c := range series {
		sum += c.CostMicros
	}
	if sum != totals.CostMicros {
		t.Errorf("series sums to %d but totals is %d", sum, totals.CostMicros)
	}
}

// TestLedgerAndRingAgreeOnASpoofedUnknownAgent pins the claim agentLabel's comment
// makes: a caller that sends literally "User-Agent: unknown" is bucketed the same way
// by the ledger and by the live aggregator.
//
// Reachable from off-host, so it is worth a test rather than a claim. The ring folds
// it under Label() == "unknown"; the ledger normalises it to "" on disk and labelFor
// maps that back to "unknown". Two spellings of unattributed traffic would show as two
// rows in any client that merged the two sources, which is the bug this prevents.
func TestLedgerAndRingAgreeOnASpoofedUnknownAgent(t *testing.T) {
	c := pipeline.ParseUserAgent("unknown")
	if c == nil {
		t.Fatal("ParseUserAgent(\"unknown\") = nil; a header WAS sent")
	}

	// The ring's key.
	ringKey := c.Label()
	// The ledger's: stored by agentLabel, read back by labelFor.
	stored := agentLabel(c)
	if stored != "" {
		t.Errorf("agentLabel = %q, want \"\": absence has one representation on disk", stored)
	}
	ledgerKey, ok := labelFor(Row{Agent: stored}, usage.GroupAgent)
	if !ok {
		t.Fatal("labelFor dropped the row; unattributed spend must still appear in the series")
	}
	if ledgerKey != ringKey {
		t.Errorf("ledger key %q != ring key %q; the two sources would render two rows for one thing",
			ledgerKey, ringKey)
	}
}

// TestQuery_TwoReadersDoNotSwapEachOthersCaveats is the reason Caveats is a return value
// rather than two counters on the Writer.
//
// As atomics set by whichever Query ran last and sampled by the caller in a separate call —
// which is exactly what sessionapi does, once per /v1/usage request, on an endpoint a chart
// polls — this fixture produced the following with the reads INTERLEAVED and no concurrency
// at all:
//
//	reader A read the day holding an undecodable line, then reported SkippedLines() = 0
//	reader B read the CLEAN day, then reported SkippedLines() = 1
//
// Both halves of the report in one sequence: a damaged day served as complete, and a
// clean day carrying a caveat about someone else's file. Nothing needs to race — any
// other read between the call and the sample is enough, and there is no ordering that
// makes the pair safe, which is why the fix is to attach the counts to the read.
func TestQuery_TwoReadersDoNotSwapEachOthersCaveats(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, testZone)
	clean := base.AddDate(0, 0, -1)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"2026-09-13T09:01:00Z","endpoint":"gw"`, // the damage
	)
	writeDay(t, dir, clean, line(clean, "gw", "m", 1, 10, 5, 100))
	w := newTestWriter(t, dir, func() time.Time { return base })

	// A starts on the corrupt day, B answers from the clean one in between, A finishes: the
	// interleaving that hands each reader the other's answer when the counts live on the Writer.
	_, aCaveats, aErr := w.Query(context.Background(), base, base.Add(time.Hour))
	if aErr != nil {
		t.Fatalf("A Query: %v", aErr)
	}
	_, bCaveats, bErr := w.Query(context.Background(), clean, clean.Add(time.Hour))
	if bErr != nil {
		t.Fatalf("B Query: %v", bErr)
	}
	if aCaveats.SkippedLines != 1 {
		t.Errorf("the read of the CORRUPT day returned SkippedLines = %d, want 1: a day that "+
			"lost a line must not report clean because another reader finished after it",
			aCaveats.SkippedLines)
	}
	if !bCaveats.Clean() {
		t.Errorf("the read of the CLEAN day returned %+v, want no caveats: a client would "+
			"render another reader's damage as its own, and an operator would go looking for "+
			"corruption in the wrong file", bCaveats)
	}
}

// TestQuery_ConcurrentReadersEachGetTheirOwnCaveats is the same property under real
// concurrency, which is how it reaches production: two /v1/usage requests, one asking
// about a day that lost a line and one about a day that did not.
//
// Worth having alongside the interleaved test above because -race says nothing about this
// on its own — the old counters were atomics, so the swap was a perfectly race-free wrong
// answer.
func TestQuery_ConcurrentReadersEachGetTheirOwnCaveats(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, testZone)
	clean := base.AddDate(0, 0, -1)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		`{"at":"2026-09-13T09:01:00Z","endpoint":"gw"`,
	)
	writeDay(t, dir, clean, line(clean, "gw", "m", 1, 10, 5, 100))
	w := newTestWriter(t, dir, func() time.Time { return base })

	const rounds = 200
	var wg sync.WaitGroup
	errs := make(chan string, 2*rounds)
	read := func(from time.Time, want int64) {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_, caveats, err := w.Query(context.Background(), from, from.Add(time.Hour))
			if err != nil {
				errs <- fmt.Sprintf("Query: %v", err)
				return
			}
			if caveats.SkippedLines != want {
				errs <- fmt.Sprintf("read of %s returned SkippedLines = %d, want %d",
					from.Format(dayLayout), caveats.SkippedLines, want)
				return
			}
		}
	}
	wg.Add(2)
	go read(base, 1)
	go read(clean, 0)
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg + " — each read has to carry the caveats for the file IT opened")
	}
}

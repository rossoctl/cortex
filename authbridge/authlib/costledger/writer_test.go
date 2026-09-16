package costledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// at is a fixed clock instant used across these tests. Deliberately not UTC
// midnight-adjacent, so a timezone bug does not accidentally pass.
var at = time.Date(2026, 9, 13, 9, 14, 30, 0, time.Local)

// testZone is a fixed non-UTC zone for the tests that turn on a day boundary.
//
// time.Local is not good enough for those. Building both the input and the
// expectation in time.Local makes the assertion vacuous wherever time.Local IS UTC —
// the default in most CI containers — because an implementation that read time.UTC
// would agree with the test by construction. At UTC-7, local midnight is 07:00 UTC,
// so the two readings land on different days and only the right one passes.
var testZone = time.FixedZone("test", -7*3600)

// newTestWriter opens a ledger over dir with a pinned clock, closed on cleanup.
func newTestWriter(t *testing.T, dir string, clock func() time.Time) *Writer {
	t.Helper()
	w, err := New(dir, WithClock(clock))
	if err != nil {
		t.Fatalf("New(%s): %v", dir, err)
	}
	// Error deliberately discarded: several of these tests point the writer at an
	// unwritable directory on purpose, and a cleanup that failed the test would
	// turn the assertion "IO failure does not propagate" into a failure.
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// costedEvent builds a response event carrying a settled cost record, the way
// inference-parser publishes it.
func costedEvent(t *testing.T, host, model string, costUSD float64, in, out int) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
		Inference: &pipeline.InferenceExtension{
			Model: model, InputTokens: in, OutputTokens: out,
			TotalTokens: in + out, PresentKinds: 0b1001,
		},
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	}
}

// setProvenance rewrites the provenance on an event's cost record.
func setProvenance(t *testing.T, e *pipeline.SessionEvent, prov string) {
	t.Helper()
	ev, ok := costevent.Record(e)
	if !ok {
		t.Fatal("event carries no cost record")
	}
	ev.Provenance = prov
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e.Plugins[costevent.Key] = raw
}

// markIncomplete flags an event's cost record as an inexact figure.
func markIncomplete(t *testing.T, e *pipeline.SessionEvent, reason string) {
	t.Helper()
	ev, ok := costevent.Record(e)
	if !ok {
		t.Fatal("event carries no cost record")
	}
	ev.Incomplete, ev.IncompleteReason = true, reason
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e.Plugins[costevent.Key] = raw
}

// readAllBytes concatenates every day file in dir.
func readAllBytes(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []byte
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		out = append(out, b...)
	}
	return out
}

// readAllRows decodes every row in every day file, in file order.
func readAllRows(t *testing.T, dir string) []Row {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(readAllBytes(t, dir)))
	var out []Row
	for {
		var r Row
		if err := dec.Decode(&r); err != nil {
			return out
		}
		out = append(out, r)
	}
}

func bytesContains(haystack []byte, needle string) bool {
	return bytes.Contains(haystack, []byte(needle))
}

func TestWriter_AccumulatesTheOpenMinuteWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	// Wait for the writer goroutine, so "no files" is a fact about behaviour rather
	// than about having asked before it got there.
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// The open minute stays in the writer's own accumulator. Writing it would mean the
	// same minute existed in two places, and a reader composing them would
	// double-count.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files while the minute was still open; want 0", len(entries))
	}
}

func TestWriter_FlushesOnMinuteRoll(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Advance past the minute boundary and record again: the previous minute closes.
	now = at.Add(time.Minute)
	later := costedEvent(t, "gw", "m", 0.10, 10, 5)
	later.At = now
	w.Record("s1", later)
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the one closed minute)", len(rows))
	}
	if rows[0].Requests != 2 {
		t.Errorf("Requests = %d, want 2 accumulated", rows[0].Requests)
	}
	if rows[0].CostMicros != 500_000 {
		t.Errorf("CostMicros = %d, want 500000", rows[0].CostMicros)
	}
	if rows[0].InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200", rows[0].InputTokens)
	}
	// The closed minute keeps its OWN timestamp, not the one that closed it.
	if want := at.Truncate(time.Minute); !rows[0].At.Equal(want) {
		t.Errorf("At = %v, want the closed minute %v", rows[0].At, want)
	}
}

func TestWriter_SeparateRowPerCompositeKey(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw-a", "opus", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw-b", "opus", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw-a", "haiku", 0.05, 10, 5))

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 distinct (endpoint, model) keys", len(rows))
	}
}

func TestWriter_ProvenanceIsPartOfTheKey(t *testing.T) {
	// A single minute can mix a gateway's own figures with modelled ones, and one
	// provenance per row would have to pick a winner. Keying on it keeps PricedBy
	// reconstructible from the ledger exactly as /v1/usage reports it.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	a := costedEvent(t, "gw", "m", 0.25, 100, 50)
	b := costedEvent(t, "gw", "m", 0.25, 100, 50)
	setProvenance(t, b, "authoritative")
	w.Record("s1", a)
	w.Record("s1", b)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — one per provenance", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Provenance] = true
	}
	if !seen["bundled"] || !seen["authoritative"] {
		t.Errorf("provenances = %v, want both bundled and authoritative", seen)
	}
}

func TestWriter_UnpricedTrafficIsRecordedWithoutCost(t *testing.T) {
	// An unpriced request still happened. It contributes no dollars and shows up as
	// the priced/priceable gap — dropping it would make the gap invisible and the
	// total look complete.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	delete(e.Plugins, costevent.Key) // no settled cost
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", rows[0].CostMicros)
	}
	if rows[0].PricedRequests != 0 {
		t.Errorf("PricedRequests = %d, want 0", rows[0].PricedRequests)
	}
	if rows[0].PriceableRequests != 1 {
		t.Errorf("PriceableRequests = %d, want 1 — it carried a model and tokens", rows[0].PriceableRequests)
	}
}

// A NEGATIVE token count never reaches a persisted row.
//
// These six numbers are decoded from the upstream response body, so their sign is chosen
// off-host. One negative folds into every later event for that minute through
// usage.Counts.Add and then persists: the minute, the day and every window containing it
// are wrong from then on, with nothing in the file to say a count was ever negative. The
// ring recovers on restart; an append-only file does not. See tokenCount for why the
// value is recorded as ABSENT rather than clamped, and why PresentKinds is left alone.
func TestRecord_ANegativeTokenCountIsRecordedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Inference.OutputTokens = -50
	e.Inference.CacheReadTokens = -1
	e.Inference.TotalTokens = -1_000_000
	w.Record("s1", e)
	// A second, ordinary event in the same minute, so the assertion covers what the fold
	// leaves behind rather than only what one row serialized to.
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 10, 5))

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 folded row: %+v", len(rows), rows)
	}
	got := rows[0]
	for name, v := range map[string]int64{
		"InputTokens": got.InputTokens, "CacheReadTokens": got.CacheReadTokens,
		"CacheWriteTokens": got.CacheWriteTokens, "OutputTokens": got.OutputTokens,
		"ReasoningTokens": got.ReasoningTokens, "Tokens": got.Tokens,
	} {
		if v < 0 {
			t.Errorf("%s = %d on disk: a negative count is now part of every total that "+
				"includes this minute, permanently", name, v)
		}
	}
	// ABSENT, not clamped, and not deducted from the counts that were reportable: the
	// good half of the second event's usage is still there.
	if got.InputTokens != 110 {
		t.Errorf("InputTokens = %d, want 110 (100 + 10) — the negative fields are dropped, "+
			"not the row", got.InputTokens)
	}
	if got.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5: -50 is recorded as no count at all, and the "+
			"second event's 5 still lands", got.OutputTokens)
	}
	// The request and its settled dollars still count. A nonsense usage block is not a
	// reason to lose money that was actually spent.
	if got.Requests != 2 || got.CostMicros != 500_000 {
		t.Errorf("Requests = %d, CostMicros = %d, want 2 and 500000", got.Requests, got.CostMicros)
	}
}

// The caveat a persisted total cannot afford to lose: a truncated stream's figure
// is a FLOOR, and once the process restarts this counter is the only thing left
// saying so. Without it the dollars on disk quietly gain a precision they never had.
func TestWriter_IncompleteFigureIsRecordedAsInexact(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 0)
	markIncomplete(t, e, "output-uncounted")
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].IncompleteRequests != 1 {
		t.Errorf("IncompleteRequests = %d, want 1", rows[0].IncompleteRequests)
	}
	// Disclosed, not deducted. The dollars and the priced count both stand; only the
	// claim of exactness is withdrawn. See usage.Counts.IncompleteRequests.
	if rows[0].CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want the figure kept at 250000", rows[0].CostMicros)
	}
	if rows[0].PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1 — incomplete is a subset, not a deduction", rows[0].PricedRequests)
	}
	// And it must round-trip: it is carried by the embedded usage.Counts, so a
	// consumer reading the file back sees the caveat too.
	if !bytesContains(readAllBytes(t, dir), "incompleteRequests") {
		t.Error("the serialized row does not carry incompleteRequests")
	}
}

// An exact figure must NOT carry the caveat: a permanent warning with nothing to
// act on is what teaches an operator to ignore the one signal that matters.
func TestWriter_ExactFigureCarriesNoIncompleteCount(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].IncompleteRequests != 0 {
		t.Errorf("IncompleteRequests = %d, want 0", rows[0].IncompleteRequests)
	}
	if bytesContains(readAllBytes(t, dir), "incompleteRequests") {
		t.Error("an exact row still serializes incompleteRequests")
	}
}

func TestWriter_NonInferenceTrafficIsIgnored(t *testing.T) {
	// MCP calls, health checks, tunnel opens. Recording them would put every
	// proxied response in the ledger and in the cost denominator.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", &pipeline.SessionEvent{At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw"})

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("got %d rows for non-inference traffic, want 0", len(rows))
	}
}

// A request event has no token counts and no cost. Folding it would double the
// request count for every turn, halving every coverage ratio the ledger reports.
func TestWriter_RequestPhaseIsIgnored(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Phase = pipeline.SessionRequest
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("got %d rows for a request event, want 0", len(rows))
	}
}

func TestWriter_HoldsNoPromptContent(t *testing.T) {
	// A user-facing promise in the docs. Assert on the serialized bytes, because
	// that is what lands on disk.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Inference.Messages = []pipeline.InferenceMessage{{Role: "user", Content: "SECRET-PROMPT-TEXT"}}
	e.Inference.Completion = "SECRET-COMPLETION-TEXT"
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	raw := readAllBytes(t, dir)
	for _, secret := range []string{"SECRET-PROMPT-TEXT", "SECRET-COMPLETION-TEXT", "Messages", "messages", "completion"} {
		if bytesContains(raw, secret) {
			t.Errorf("ledger bytes contain %q", secret)
		}
	}
}

// A failed append never reaches the request path, and is not silently forgotten either.
//
// The ledger is observability: a full disk or a read-only home must not turn into a failed
// request. Record has no error to return, so "does not propagate" is not a thing this test
// can observe directly — what it can observe is that the call returns, that the writer is
// still usable afterwards, and that the loss is counted somewhere an operator can see.
//
// IT USED TO ASSERT NOTHING AT ALL. It pointed the writer at a 0o500 directory, discarded
// Flush's error, called Record twice and ended — no assertion, so it passed against a
// Writer that propagated every error, and it passed on a disk where the write succeeded.
// Two changes make it real: the premise is asserted, and the failure is arranged with a
// DIRECTORY where the day file belongs (blockDayFile), which fails with EISDIR for root
// too. A permission bit does not: as root the old setup wrote the file successfully and
// the test reported success over a case it had not exercised.
func TestWriter_IOFailureDoesNotPropagate(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	blockDayFile(t, w, at)

	// The premise. Without this the rest of the test is about a working disk.
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err == nil {
		t.Fatal("Flush reported success appending to a path that is a directory; the premise " +
			"of this test is that the append fails")
	}

	// Record must return. Bounded, because the failure mode is a hang rather than an
	// error: a writer that wedged on its own IO would block the session-append path that
	// calls this, and a test that simply called Record would hang with it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Record did not return after a failed append; the request path is now waiting " +
			"on the cost ledger's disk")
	}

	// And the loss is visible. Dropped() is the only "is my cost history complete" signal
	// there is; the exact count belongs to TestWriter_AFailedAppendIsCountedAsADrop, so
	// this asserts only that swallowing the error did not also swallow the fact.
	now = at.Add(2 * time.Minute)
	_ = w.Flush()
	if w.Dropped() == 0 {
		t.Error("Dropped() = 0 after two failed appends: the error was hidden from the request " +
			"path AND from the operator, which is a ledger reporting itself complete over spend " +
			"it never wrote")
	}

	// The writer is still usable. An IO error must not close it — that would turn one
	// unwritable minute into a session with no cost history and no further attempts.
	if w.closed.Load() {
		t.Error("the writer closed itself over an IO error; the next day's rows would then be " +
			"dropped by a flag rather than by the disk")
	}
}

// blockDayFile puts a DIRECTORY where the day file for t belongs, so every append to
// it fails with EISDIR.
//
// A directory rather than a 0o500 parent because EISDIR applies to root too: a test
// that depends on a permission bit passes or fails depending on who runs it, and this
// one is asserting accounting, not permissions.
func blockDayFile(t *testing.T, w *Writer, when time.Time) {
	t.Helper()
	if err := os.Mkdir(w.store.path(when), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", w.store.path(when), err)
	}
}

// A failed append is a PERMANENT loss of that minute, because takeLocked advanced
// flushedThrough and emptied the map before the write ran — deliberately, since a
// re-held minute could be written twice. So the row exists nowhere afterwards, and
// Dropped(), the only exported "is my cost history complete" signal, answered 0 over
// it. TestWriter_IOFailureDoesNotPropagate asserts the error does not reach the
// request; this asserts the loss is not hidden from the operator.
func TestWriter_AFailedAppendIsCountedAsADrop(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	blockDayFile(t, w, at)

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err == nil {
		t.Fatal("Flush reported success writing to a path that is a directory; " +
			"the premise of this test is that the append fails")
	}

	if got := w.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d after an append that lost one row, want 1; 0 is a ledger "+
			"reporting itself complete over a minute of spend that no longer exists anywhere", got)
	}
	// And the row really is gone from memory too, which is what makes the loss
	// permanent rather than merely delayed: takeLocked emptied the accumulator before
	// the write was attempted.
	if held, open, _ := w.pending(); len(held) != 0 || !open.IsZero() {
		t.Errorf("the accumulator still holds %d rows (open %v); the row was not lost, so "+
			"this test is not measuring what it claims", len(held), open)
	}
}

// THE COUNT IS WHAT THE STORE COULD NOT WRITE, NOT THE SIZE OF THE BATCH.
//
// It used to be len(b.rows), which over-reports in both of the ways a partly-failed
// append can happen: a torn write leaves the rows before the tear durably on disk (see
// TestWriteLines_ATornAppendCountsOnlyTheRowsItLost), and store.append writes one file per
// day, so a batch spanning two of them can fail on one and land the other. Dropped() is
// the only exported "is my cost history complete" signal, and over-reporting trains an
// operator to disbelieve it exactly as thoroughly as under-reporting hides loss.
//
// write() is driven DIRECTLY here. Record cannot produce a two-day batch today — the
// accumulator holds one minute — so going through it would assert nothing about this
// arithmetic; the torn-append test covers the instance a real deployment reaches, and
// this one pins the accounting in write() that both instances flow through.
func TestWriter_AFailedAppendCountsOnlyWhatTheStoreCouldNotWrite(t *testing.T) {
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, testZone)
	before := midnight.Add(-time.Minute)
	w := newTestWriter(t, dir, func() time.Time { return midnight })
	// Only the SECOND day's file is unwritable. The first has to land, which is the whole
	// point: one row is lost and one is readable back.
	blockDayFile(t, w, midnight)

	w.write(batch{rows: []Row{
		{At: before, Endpoint: "gw", Model: "writable", Counts: usage.Counts{Requests: 1}},
		{At: midnight, Endpoint: "gw", Model: "blocked", Counts: usage.Counts{Requests: 1}},
	}})

	if got := w.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1: the batch carried two rows and only the one destined "+
			"for the unwritable day was lost — the other is on disk, and counting it reports a "+
			"row the ledger can read back as missing", got)
	}
	// And it really is on disk, or this test is asserting the wrong number.
	rows, issues, rerr := w.store.readDay(before)
	if rerr != nil {
		t.Fatalf("readDay: %v", rerr)
	}
	if len(rows) != 1 || issues.skippedLines != 0 {
		t.Errorf("the writable day holds %d rows with %d skipped lines, want 1 and 0",
			len(rows), issues.skippedLines)
	}
}

// The append can fail for a whole batch, not just one row, and the count is the row
// count rather than the batch count.
func TestWriter_AFailedAppendCountsEveryRowInTheBatch(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	blockDayFile(t, w, at)

	// Three distinct keys in one minute, so the batch carries three rows.
	for _, model := range []string{"opus", "sonnet", "haiku"} {
		w.Record("s1", costedEvent(t, "gw", model, 0.25, 100, 50))
	}
	now = at.Add(time.Minute)
	if err := w.Flush(); err == nil {
		t.Fatal("Flush reported success writing to a path that is a directory")
	}

	if got := w.Dropped(); got != 3 {
		t.Errorf("Dropped() = %d, want 3 — the count is rows lost, not batches failed", got)
	}
}

// The J6 bound. The model name is request-chosen, so without a cap the accumulator
// grows to whatever a caller sends — 50,000 keys held for one minute was measured,
// and every one of them is also copied by takeLocked under mu inside
// session.Store.Append's write lock.
func TestRecord_DistinctLabelsPerMinuteAreCapped(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	const sent = 5_000
	for i := 0; i < sent; i++ {
		w.Record("s1", costedEvent(t, "gw", fmt.Sprintf("model-%d", i), 0.25, 100, 50))
	}

	held, _, _ := w.pending()
	if len(held) > maxLabelsPerMinute {
		t.Errorf("the open minute holds %d rows after %d distinct models, want at most %d; "+
			"unbounded here is unbounded memory AND an unbounded walk on the request path",
			len(held), sent, maxLabelsPerMinute)
	}
	// FOLDED, NOT DROPPED. Coarse attribution is a worse answer than exact attribution
	// and a far better one than a total that is short by 4,936 requests.
	var total int64
	var requests int64
	var sawOverflow bool
	for _, r := range held {
		total += r.CostMicros
		requests += r.Requests
		if r.Model == overflowLabel {
			sawOverflow = true
		}
	}
	if want := int64(sent) * 250_000; total != want {
		t.Errorf("CostMicros across the capped minute = %d, want %d — the cap must cost "+
			"attribution detail, never dollars", total, want)
	}
	if requests != int64(sent) {
		t.Errorf("Requests = %d, want %d", requests, sent)
	}
	if !sawOverflow {
		t.Error("no (other) row: the excess was dropped or silently keyed under a real " +
			"model, either of which misattributes it")
	}
}

// The reserved slot has to be reserved BEFORE the map is full, or the overflow row
// itself becomes the (cap+1)th entry and the map settles one over its stated bound.
// usage.addLabel reserves it the same way and for the same reason.
func TestRecord_TheOverflowRowFitsInsideTheCap(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	for i := 0; i < maxLabelsPerMinute+10; i++ {
		w.Record("s1", costedEvent(t, "gw", fmt.Sprintf("model-%d", i), 0.25, 100, 50))
	}

	held, _, _ := w.pending()
	if len(held) != maxLabelsPerMinute {
		t.Errorf("held %d rows, want exactly %d — %d means the (other) row was added on "+
			"top of a full map instead of into the slot kept for it",
			len(held), maxLabelsPerMinute, maxLabelsPerMinute+1)
	}
}

// The cap is PER MINUTE, not for the life of the writer: a new minute starts from an
// empty accumulator, so a deployment with 40 real labels never reaches the bound and
// never sees an (other) band at all.
func TestRecord_TheCapResetsWithTheMinute(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	for i := 0; i < maxLabelsPerMinute*2; i++ {
		w.Record("s1", costedEvent(t, "gw", fmt.Sprintf("model-%d", i), 0.25, 100, 50))
	}
	now = at.Add(time.Minute)
	fresh := costedEvent(t, "gw", "opus", 0.25, 100, 50)
	fresh.At = now
	w.Record("s1", fresh)

	held, _, _ := w.pending()
	if len(held) != 1 || held[0].Model != "opus" {
		t.Errorf("the new minute holds %+v, want one row for opus — the cap must not carry "+
			"over and coarsen a minute that has no cardinality problem", held)
	}
}

// A day file records spend, so it must not be readable by other accounts on a
// shared machine. 0o644 is the default that would be wrong here.
func TestWriter_DayFileIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("readdir: %d entries, err %v", len(entries), err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s mode = %v, want no group or other access", entries[0].Name(), perm)
	}
}

// A flush that straddles local midnight must split across two day files. A cached
// handle would append tomorrow's minute to yesterday's file, silently giving that
// day 24 extra hours.
//
// In testZone rather than time.Local, for the reason recorded there: at UTC-7 these
// two minutes are in one UTC day and two local days, so a UTC filename would put both
// rows in one file and the assertion below would fail. Built in time.Local it passed
// on any UTC host whichever zone the implementation used.
func TestWriter_FlushStraddlingMidnightSplitsByDay(t *testing.T) {
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, testZone)
	before := midnight.Add(-time.Minute)
	now := before
	w := newTestWriter(t, dir, func() time.Time { return now })

	early := costedEvent(t, "gw", "m", 0.25, 100, 50)
	early.At = before
	w.Record("s1", early)
	// A late arrival for a closed minute appends immediately, so both days are
	// written by one writer without needing a roll.
	late := costedEvent(t, "gw", "m", 0.25, 100, 50)
	late.At = midnight
	now = midnight
	w.Record("s1", late)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	for _, want := range []string{"2026-09-13.jsonl", "2026-09-14.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
}

// stalledWriter is a Writer with NO writer goroutine and a queue of the given depth,
// which is how a unit test stands in for a filesystem that has stopped responding:
// nothing drains w.ops, so enqueue reaches its default arm for real.
//
// Built by hand because New always starts the goroutine. The store is real but nothing
// drains the queue, so no batch ever reaches it: the temp directory stays empty, and
// the test asserts that below.
func stalledWriter(t *testing.T, depth int, clock func() time.Time) *Writer {
	t.Helper()
	s, err := newStore(t.TempDir(), 0, clock().Location())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	return &Writer{
		store:      s,
		now:        clock,
		rows:       map[key]*Row{},
		ops:        make(chan batch, depth),
		quit:       make(chan struct{}),
		dropNotify: make(chan struct{}, dropNotifyDepth),
	}
}

// The property finding 3 is about: a filesystem that has stopped responding must not
// reach the request path. Record is called under session.Store's write lock, so a
// blocking hand-off would stall every other request in the proxy. A blocking
// implementation hangs here rather than failing.
//
// The count is asserted EXACTLY. It used to be "not zero", which passed against an
// implementation that lost 1,024 rows and counted 49.
func TestRecord_DropsRatherThanBlocksWhenTheWriterCannotKeepUp(t *testing.T) {
	const depth, events = 4, 20
	now := at
	w := stalledWriter(t, depth, func() time.Time { return now })

	// Each new minute closes the previous one, so this is one queued batch of one row
	// per event after the first, and the last minute stays in the accumulator.
	for i := 0; i < events; i++ {
		now = at.Add(time.Duration(i) * time.Minute)
		e := costedEvent(t, "gw", "m", 0.25, 100, 50)
		e.At = now
		w.Record("s1", e)
	}

	if got, want := w.Dropped(), int64(events-1-depth); got != want {
		t.Errorf("Dropped() = %d, want %d (%d rolled minutes, %d of them queued); "+
			"a drop that is not counted is a cost total short by an unknown amount",
			got, want, events-1, depth)
	}
	// Nothing is unaccounted for: queued + dropped + held == recorded.
	held, _, _ := w.pending()
	if got := int64(len(w.ops)) + w.Dropped() + int64(len(held)); got != events {
		t.Errorf("accounted for %d rows of %d recorded", got, events)
	}
	// And the premise: with nothing draining the queue, nothing reached disk. If it had,
	// Record would be doing IO on the request path.
	if entries, _ := os.ReadDir(w.store.dir); len(entries) != 0 {
		t.Errorf("%d files written with no writer goroutine running; Record touched disk", len(entries))
	}
}

// The J2 undercount, measured: after Close the writer goroutine is gone, so a
// non-blocking send lands in a buffer nobody will ever read again. 1,024 rows sat in
// w.ops for the life of the process while Dropped() answered 49 — a 21x undercount,
// and the shutdown warning quoted that same wrong number.
func TestRecord_AfterCloseIsCountedRatherThanParkedInTheQueue(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const events = opsBuffer + 50
	for i := 0; i < events; i++ {
		now = at.Add(time.Duration(i) * time.Minute)
		e := costedEvent(t, "gw", "m", 0.25, 100, 50)
		e.At = now
		w.Record("s1", e)
	}

	onDisk := int64(len(readAllRows(t, dir)))
	held, _, _ := w.pending()
	if got, want := w.Dropped()+onDisk+int64(len(held)), int64(events); got != want {
		t.Errorf("accounted for %d rows of %d recorded (dropped %d, on disk %d, held %d); "+
			"a row in none of the three is a loss Dropped() denies",
			got, want, w.Dropped(), onDisk, len(held))
	}
	if stuck := len(w.ops); stuck != 0 {
		t.Errorf("%d batches are parked in the queue with no goroutine to read them", stuck)
	}
}

// The other half of J2: a row a Record folds back into the accumulator AFTER Close's
// Flush has emptied it can never be written by anyone, so Close has to count it.
//
// THE RACE IS DRIVEN, not simulated: the clock is called on the writer goroutine, so a
// clock parked there until quit closes runs its Record at exactly the moment Close is
// past its Flush and not yet at its accounting. Asserting abandon()'s return value
// instead would not pin the thing that was broken — Close ignoring what abandon found —
// and a mutation proved it: dropping the Add left that assertion passing.
func TestClose_CountsARowThatRacedItsFlush(t *testing.T) {
	dir := t.TempDir()
	var wp atomic.Pointer[Writer]
	var armed, parked, raced atomic.Bool
	// Built here rather than on the writer goroutine, so no t.Fatalf can fire off-test.
	late := costedEvent(t, "gw", "m", 0.25, 100, 50)

	clock := func() time.Time {
		w := wp.Load()
		if w == nil || !armed.CompareAndSwap(false, true) {
			return at
		}
		parked.Store(true)
		select {
		case <-w.quit:
			// Close has flushed and asked the goroutine to stop. A Record landing now is one
			// that raced the shutdown: the accumulator was emptied a moment ago, so this row
			// exists only in a map nothing will ever write.
			w.Record("s1", late)
			raced.Store(true)
		case <-time.After(5 * time.Second):
		}
		return at
	}

	w, err := New(dir, WithClock(clock), withSettleInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wp.Store(w)
	for deadline := time.Now().Add(2 * time.Second); !parked.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("the writer goroutine never reached the clock; the settle tick is not running")
		}
		time.Sleep(time.Millisecond)
	}

	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if !raced.Load() {
		t.Fatal("the racing Record never ran; this test is not measuring what it claims")
	}
	if got := w.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1; the row is in no queue, no file and no map, and "+
			"Dropped() is the only signal that says so", got)
	}
	if held, _, _ := w.pending(); len(held) != 0 {
		t.Errorf("the accumulator still holds %d rows after Close; they would be counted twice "+
			"if a later Flush wrote them", len(held))
	}
}

// abandon's own arithmetic, over both places a row can be stranded. Separate from the
// test above because that one can only stage the accumulator half.
func TestAbandon_CountsTheQueueAndTheAccumulator(t *testing.T) {
	dir := t.TempDir()
	now := at
	w, err := New(dir, WithClock(func() time.Time { return now }), withSettleInterval(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	// One batch of two rows stranded in the queue, one row stranded in the accumulator.
	w.ops <- batch{rows: []Row{
		{At: at, Endpoint: "gw", Model: "haiku"},
		{At: at, Endpoint: "gw", Model: "sonnet"},
	}}
	w.mu.Lock()
	w.rows[key{endpoint: "gw", model: "opus"}] = &Row{At: at, Endpoint: "gw", Model: "opus"}
	w.mu.Unlock()

	if lost := w.abandon(); lost != 3 {
		t.Errorf("abandon() = %d, want 3 (two queued rows, one held row)", lost)
	}
	if held, _, _ := w.pending(); len(held) != 0 {
		t.Errorf("the accumulator still holds %d rows after abandon", len(held))
	}
}

// N3: closed must be set AFTER the writer goroutine has stopped. While it was set
// before close(quit), every Record racing the shutdown was counted as dropped although
// its rows could still have been written, and submit's inline-write branch was armed
// while the goroutine was still draining, so two writeLines could run on one day file.
// The second half no longer destroys rows — appendBytes never shortens a file, see
// TestWriteLines_ATornAppendDoesNotRollBackAnotherWritersRows — but the ordering is what
// keeps one writer doing this Writer's IO, which is the invariant run() rests on.
//
// THE ORDERING ITSELF IS ASSERTED, not a consequence of it, because the consequence
// needs a concurrent partial write to become damage and a unit test cannot arrange
// ENOSPC. The clock is the seam: the settle tick calls w.now() on the writer
// goroutine, so a clock that parks there and watches w.closed reports directly whether
// the flag flipped while the goroutine was alive. With the flag set before wg.Wait it
// flips immediately and the parked goroutine sees it; with it set after, the flag
// cannot move until this clock has returned and the goroutine has exited.
//
// THE OBSERVATION WINDOW STARTS AT close(quit), NOT AT AN ARBITRARY 250 ms. It used to
// park for a fixed 250 ms from whenever the settle tick happened to fire and then give up
// silently, so on a loaded runner — a CI box under -race with the rest of the suite in
// flight — Close could still be waiting to be scheduled when the window expired, and the
// test passed without ever having looked. That is the vacuous-pass shape this review keeps
// finding: green because nothing was checked.
//
// Close does Flush, close(quit), wg.Wait, drain, closed.Store(true). Waiting for quit to
// close is therefore a deterministic report that Close is INSIDE its ordering, and it can
// be waited on for as long as it takes: with the store after wg.Wait, the flag physically
// cannot move while this clock has not returned, so no amount of waiting can produce a
// false failure. What remains timing-dependent is only the reverse: a mutant that stores
// the flag between close(quit) and wg.Wait is caught by the short poll after that point.
// The window can therefore MISS a mutation on a pathologically slow machine and can never
// invent one — the safe direction, and the reason it is stated rather than tuned.
func TestClose_MarksClosedOnlyAfterTheGoroutineHasStopped(t *testing.T) {
	dir := t.TempDir()
	var wp atomic.Pointer[Writer]
	var parked, sawClosedWhileRunning, sawQuit atomic.Bool

	clock := func() time.Time {
		w := wp.Load()
		if w == nil {
			// New's own call, before the writer is published.
			return at
		}
		parked.Store(true)
		defer parked.Store(false)
		// Park until Close has entered its ordering, which close(quit) reports exactly.
		// Generous, and never a false failure: a correct Close cannot set closed until
		// this function returns, so waiting longer only ever helps.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if w.closed.Load() {
				// Before quit even closed, which is the flag set at the very top of Close.
				sawClosedWhileRunning.Store(true)
				return at
			}
			select {
			case <-w.quit:
				sawQuit.Store(true)
			default:
				time.Sleep(time.Millisecond)
				continue
			}
			break
		}
		if !sawQuit.Load() {
			// Not a pass. The test failed to arrange what it is about.
			return at
		}
		// From here Close is between close(quit) and its store. A correct implementation
		// is blocked in wg.Wait until this returns; a mutant that stores the flag in
		// between flips it now.
		for poll := time.Now().Add(200 * time.Millisecond); time.Now().Before(poll); {
			if w.closed.Load() {
				sawClosedWhileRunning.Store(true)
				return at
			}
			time.Sleep(time.Millisecond)
		}
		return at
	}

	// A fast settle so the goroutine reaches the clock promptly.
	w, err := New(dir, WithClock(clock), withSettleInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wp.Store(w)

	// Wait until the goroutine is parked in the clock, so Close runs its ordering while
	// the goroutine is demonstrably still alive.
	for deadline := time.Now().Add(2 * time.Second); !parked.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("the writer goroutine never reached the clock; the settle tick is not running")
		}
		time.Sleep(time.Millisecond)
	}

	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	// The real assertion first, because it can be true while the premise below is false: a
	// flag set at the very TOP of Close is observed before quit ever closes, and reporting
	// that as "this test proved nothing" would hide the defect behind the check that exists
	// to stop it hiding.
	if sawClosedWhileRunning.Load() {
		t.Error("closed was set while the writer goroutine was still running: rows recorded " +
			"during the shutdown are counted as dropped although they could still be written, " +
			"and submit's inline write and the draining goroutine can both be in writeLines " +
			"on one day file at once")
	} else if !sawQuit.Load() {
		// The premise, asserted rather than assumed: with no observation of close(quit) the
		// clock never watched Close from inside its ordering, so a green result above means
		// only that nothing was looked at. This is exactly what the old fixed 250 ms window
		// turned into a silent pass on a loaded runner.
		t.Fatal("the parked clock never observed close(quit), so Close was never watched while " +
			"the writer goroutine was alive; this test proved nothing about the ordering")
	}
	if !w.closed.Load() {
		t.Error("not closed after Close returned; submit would queue work to a goroutine that has gone")
	}
	select {
	case <-w.quit:
	default:
		t.Error("quit is still open after Close; the goroutine was never asked to stop")
	}
}

// A shutdown path may call Close twice — the second call must not report success over
// the first call's failure, or a retrying shutdown concludes the ledger was flushed.
func TestClose_RepeatsTheFirstError(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	blockDayFile(t, w, at)
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	first := w.Close()
	if first == nil {
		t.Fatal("Close over a blocked day file returned nil; the premise of this test is that the flush fails")
	}
	if second := w.Close(); second == nil {
		t.Error("the second Close returned nil over a shutdown that lost a minute; " +
			"a retrying caller would conclude the ledger was flushed")
	}
}

// The settle path: a minute that has ENDED is written even though no new event has
// arrived to roll it. Without this an idle proxy held its last minute in memory
// indefinitely, so a kill lost it and every reader had to reach into memory for it.
func TestSettleClosedMinute_WritesAnEndedMinuteWithNoNewTraffic(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Time moves on; no further traffic.
	now = at.Add(90 * time.Second)
	w.settleClosedMinute()

	rows := readAllRows(t, dir)
	if len(rows) != 1 || rows[0].CostMicros != 250_000 {
		t.Fatalf("got %+v, want the ended minute written", rows)
	}
	if _, open, _ := w.pending(); !open.IsZero() {
		t.Error("the minute is still held after being settled; it would be counted twice")
	}
}

// And it must NOT write the minute that is still accumulating — the one state that
// would put a minute on disk and in memory at once.
func TestSettleClosedMinute_LeavesTheCurrentMinuteAlone(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	now = at.Add(20 * time.Second) // same minute
	w.settleClosedMinute()

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files for a minute that has not ended yet", len(entries))
	}
}

// Retention still runs on a day roll — it just runs on the writer goroutine now
// rather than inside a request. The prune request rides the same queue as the rows,
// so a sync is what proves it arrived.
func TestPrune_OnADayRollRunsThroughTheWriter(t *testing.T) {
	dir := t.TempDir()
	now := at
	w, err := New(dir, WithClock(func() time.Time { return now }), WithRetentionDays(3))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	old := w.store.path(at.AddDate(0, 0, -10))
	if werr := os.WriteFile(old, []byte("{}\n"), 0o600); werr != nil {
		t.Fatalf("seed: %v", werr)
	}
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Cross local midnight, which is what arms the prune.
	now = dayOf(at).AddDate(0, 0, 1).Add(9 * time.Hour)
	next := costedEvent(t, "gw", "m", 0.25, 100, 50)
	next.At = now
	w.Record("s1", next)
	if serr := w.sync(); serr != nil {
		t.Fatalf("sync: %v", serr)
	}

	if _, serr := os.Stat(old); !os.IsNotExist(serr) {
		t.Errorf("the 10-day-old file survived a day roll under a 3-day retention: %v", serr)
	}
}

// N4: prunedDay used to advance when the batch carrying pruneAt was BUILT, on the
// request path, before the prune had run. A batch the queue dropped, or a prune that
// failed, therefore disarmed retention for the rest of that day — permanently, on a
// laptop proxy that may not restart for weeks, which is the exact case day-roll pruning
// exists for.
func TestPrune_StaysArmedUntilAPruneActuallyRuns(t *testing.T) {
	dir := t.TempDir()
	now := at
	w, err := New(dir, WithClock(func() time.Time { return now }),
		WithRetentionDays(3), withSettleInterval(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	next := dayOf(at).AddDate(0, 0, 1).Add(9 * time.Hour)
	w.mu.Lock()
	first := w.pruneDueLocked(next)
	w.mu.Unlock()
	if first.IsZero() {
		t.Fatal("no prune armed on a day roll; the premise of this test is that one is due")
	}

	// The batch carrying it never landed, so nothing was pruned. The next minute has to
	// ask again — this used to answer "already done" forever.
	w.mu.Lock()
	second := w.pruneDueLocked(next.Add(time.Minute))
	w.mu.Unlock()
	if second.IsZero() {
		t.Error("retention disarmed for the day by a prune that never ran; the files past " +
			"the window would stay until the next restart or the next midnight")
	}

	// And once one HAS run it stops asking, or every minute pays a directory listing.
	w.markPruned(next)
	w.mu.Lock()
	third := w.pruneDueLocked(next.Add(2 * time.Minute))
	w.mu.Unlock()
	if !third.IsZero() {
		t.Error("retention re-armed after a successful prune; that is a ReadDir every minute")
	}
}

// retainDays is what the option and the config field both call "how many day files
// survive", and prune kept retainDays + 1 of them: the cutoff was inclusive at both
// ends of the range.
func TestPrune_KeepsExactlyRetainDaysFiles(t *testing.T) {
	dir := t.TempDir()
	const retain = 3
	s, err := newStore(dir, retain, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	for i := 0; i < 7; i++ {
		if werr := os.WriteFile(s.path(at.AddDate(0, 0, -i)), []byte("{}\n"), 0o600); werr != nil {
			t.Fatalf("seed: %v", werr)
		}
	}

	if perr := s.prune(at); perr != nil {
		t.Fatalf("prune: %v", perr)
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	if len(entries) != retain {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%d files survived a %d-day retention (%v), want %d — today and the %d before it",
			len(entries), retain, names, retain, retain-1)
	}
}

// A HOST CLOCK THAT STEPS FORWARD USED TO DELETE THE WHOLE LEDGER, TODAY INCLUDED.
// The cutoff was today-minus-retention with no floor under it, so a clock that jumped
// — NTP correcting after a resume, a restored VM, a dead CMOS battery — put every
// existing file behind the cutoff and prune unlinked all of them. It runs
// synchronously in New, which is exactly when a laptop's clock is least trustworthy.
//
// Four years is not the interesting number; any step past the retention window has the
// same effect, and a year-scale one is what a dead battery actually produces.
func TestPrune_AForwardClockStepDoesNotDeleteTheLedger(t *testing.T) {
	dir := t.TempDir()
	const retain = 30
	s, err := newStore(dir, retain, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	seed := func(d time.Time) string {
		p := s.path(d)
		if werr := os.WriteFile(p, []byte("{}\n"), 0o600); werr != nil {
			t.Fatalf("seed: %v", werr)
		}
		return p
	}
	// Three days of real history. The newest is "today" by the clock that wrote it.
	live := []string{seed(at), seed(at.AddDate(0, 0, -1)), seed(at.AddDate(0, 0, -2))}
	// And one file that is past the window by the ledger's OWN newest day, so this also
	// pins that the floor is not "never prune again".
	stale := seed(at.AddDate(0, 0, -(retain + 10)))

	if perr := s.prune(at.AddDate(4, 0, 0)); perr != nil {
		t.Fatalf("prune: %v", perr)
	}

	for _, p := range live {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("%s was deleted by a prune run on a clock four years ahead of it: %v — "+
				"a clock step must never cost a user their cost history, today's least of all",
				filepath.Base(p), serr)
		}
	}
	if _, serr := os.Stat(stale); !os.IsNotExist(serr) {
		t.Errorf("%s survived: it is %d days behind the newest day file, so it is past the "+
			"window on any reading of the clock and retention still has to reclaim it",
			filepath.Base(stale), retain+10)
	}
}

// A FUTURE-DATED DAY FILE USED TO SURVIVE FOR EVER, so "at most retainDays files
// survive" stopped being true after a single clock-skewed write.
//
// The floor under the cutoff only ever LOWERS the reference day, and a file dated ahead
// of the clock is never Before any cutoff derived from it, so nothing could ever reclaim
// it: one write by a clock that was briefly years fast left that file in the directory
// for as long as the directory lived. Retention of ordinary days kept advancing, which
// is why this was a leak rather than a freeze — and why it is invisible until someone
// lists the directory.
func TestPrune_AFutureDatedDayFileIsNotKeptForever(t *testing.T) {
	dir := t.TempDir()
	const retain = 30
	s, err := newStore(dir, retain, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	seed := func(d time.Time) string {
		p := s.path(d)
		if werr := os.WriteFile(p, []byte("{}\n"), 0o600); werr != nil {
			t.Fatalf("seed: %v", werr)
		}
		return p
	}
	// Today and two days of real history, by a clock that is right.
	live := []string{seed(at), seed(at.AddDate(0, 0, -1)), seed(at.AddDate(0, 0, -2))}
	// And the artefact: one minute written while the clock read four years hence. Its
	// rows are dated then too, so no window this clock can express will ever read them.
	artefact := seed(at.AddDate(4, 0, 0))

	if perr := s.prune(at); perr != nil {
		t.Fatalf("prune: %v", perr)
	}

	if _, serr := os.Stat(artefact); !os.IsNotExist(serr) {
		t.Errorf("%s survived a prune run by a correct clock: a day file dated four years ahead "+
			"cannot be data this ledger will ever serve, and nothing else will ever reclaim it — "+
			"the retention cutoff only moves with the clock and a future date is never behind it",
			filepath.Base(artefact))
	}
	for _, p := range live {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("%s was deleted while reclaiming a future-dated file: %v — pruning the artefact "+
				"must not cost a user the history beside it", filepath.Base(p), serr)
		}
	}
}

// THE NEAR FUTURE IS NOT AN ARTEFACT, and this is the half that keeps the fix above from
// becoming the bug the floor exists to prevent.
//
// A clock a few seconds fast across midnight files a real minute of spend under
// tomorrow's date, and a clock that steps BACKWARD — a restored VM snapshot, an NTP
// correction after a resume — makes several days of genuine history look future-dated.
// Neither is reclaimed here: ordinary retention sweeps anything within a retention window
// of the clock's day as the clock advances into it, so those files age out on their own
// and no rule is needed. Only a file that would outlive the whole window is deleted.
func TestPrune_ADayFileWithinTheWindowAheadOfTheClockIsKept(t *testing.T) {
	dir := t.TempDir()
	const retain = 7
	s, err := newStore(dir, retain, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	seed := func(d time.Time) string {
		p := s.path(d)
		if werr := os.WriteFile(p, []byte("{}\n"), 0o600); werr != nil {
			t.Fatalf("seed: %v", werr)
		}
		return p
	}
	// Tomorrow: the midnight-skew case. And retain days ahead: the boundary, which is
	// still inside what ordinary retention will sweep.
	kept := []string{seed(at), seed(at.AddDate(0, 0, 1)), seed(at.AddDate(0, 0, retain))}

	if perr := s.prune(at); perr != nil {
		t.Fatalf("prune: %v", perr)
	}

	for _, p := range kept {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("%s was deleted: %v — a day within one retention window of the clock is "+
				"either a small skew or a clock that stepped back, and deleting a user's cost "+
				"history on that evidence is the failure the floor exists to prevent",
				filepath.Base(p), serr)
		}
	}
}

// N7: the day a row is FILED under and the day a reader LOOKS in have to be decided by
// one zone. path() named the file from the row's own zone while the day walk used the
// caller's, and they agreed only because nothing in the pipeline calls .UTC(). One that
// does writes a row near midnight to a file no query for that local day ever opens.
//
// In testZone (UTC-7), 23:30 local is 06:30 the NEXT day in UTC, so the two zones
// disagree about which day this instant belongs to — which is the point.
func TestRecord_ARowTimestampedInAnotherZoneIsStillFiledUnderTheLedgerDay(t *testing.T) {
	dir := t.TempDir()
	evening := time.Date(2026, 9, 13, 23, 30, 0, 0, testZone)
	now := evening
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.At = evening.UTC() // the same instant, from a producer that normalised to UTC
	w.Record("s1", e)
	now = evening.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "2026-09-13.jsonl")); err != nil {
		t.Errorf("the row was not filed under the ledger day: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-09-14.jsonl")); err == nil {
		t.Error("the row was filed under its own zone's date, which is a file no query " +
			"for that ledger day visits")
	}
	// The assertion that matters: a query for that ledger day finds it.
	rows, _, err := w.Query(context.Background(), dayOf(evening), evening.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows querying the ledger day the spend happened on, want 1", len(rows))
	}
	// And the stored timestamp itself carries the ledger zone, so every line in a day
	// file prints one offset. store.dayOf would find this row either way — it normalises
	// on the way to the filename — so without this the normalisation in Record is a line
	// no test pins.
	if body := readAllBytes(t, dir); !bytesContains(body, "-07:00") {
		t.Errorf("the row's timestamp does not carry the ledger zone's offset: %s", body)
	}
}

// The read half of the same rule: the same instants spelled in a different zone must
// read the same day files, or the answer depends on how the caller wrote the window
// down.
// The window STRADDLES the ledger midnight, which is the shape that catches it. A
// window inside one ledger day does not: store.path normalises the walk's dates on the
// way to a filename, so at UTC-7 a walk over UTC days lands on the right single file by
// arithmetic accident (UTC midnight maps back to the previous ledger day). What it
// cannot do is produce the right SET — a walk in UTC covers one date here where the
// ledger covers two, so one of the two files is never opened. An earlier version of
// this test used a single-day window and the mutation survived it.
func TestQuery_WindowBoundsInAnotherZoneReadTheSameDayFiles(t *testing.T) {
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, testZone)
	before := midnight.Add(-30 * time.Minute) // 23:30 on the 13th, ledger zone
	after := midnight.Add(30 * time.Minute)   // 00:30 on the 14th, ledger zone
	now := before
	w := newTestWriter(t, dir, func() time.Time { return now })

	for _, when := range []time.Time{before, after} {
		// costedEvent stamps its own At, so place each event explicitly.
		e := costedEvent(t, "gw", "m", 0.25, 100, 50)
		e.At = when
		now = when
		w.Record("s1", e)
	}
	now = after.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// Two ledger days, two files — the premise.
	for _, want := range []string{"2026-09-13.jsonl", "2026-09-14.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Fatalf("missing %s: %v", want, err)
		}
	}

	local, _, err := w.Query(context.Background(), before, after)
	if err != nil {
		t.Fatalf("Query in the ledger zone: %v", err)
	}
	utc, _, err := w.Query(context.Background(), before.UTC(), after.UTC())
	if err != nil {
		t.Fatalf("Query in UTC: %v", err)
	}
	if len(local) != 2 {
		t.Fatalf("got %d rows querying in the ledger zone, want 2 across the two day files", len(local))
	}
	if len(utc) != len(local) {
		t.Errorf("the ledger zone returned %d rows and UTC returned %d over the same "+
			"instants; the day walk must not depend on how the caller spelled the window",
			len(local), len(utc))
	}
}

// Close is called from a shutdown path that may already have failed once, so a second
// call must not panic on a closed channel or block on a goroutine that has gone.
func TestClose_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if rows := readAllRows(t, dir); len(rows) != 1 {
		t.Errorf("got %d rows, want the open minute flushed by Close", len(rows))
	}
}

// Flush after Close still writes: main flushes the ledger during shutdown, and a
// Writer whose goroutine has gone must do the work inline rather than queue it for
// nobody.
func TestFlush_AfterCloseStillWrites(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush after Close: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 1 {
		t.Errorf("got %d rows, want 1 written inline after Close", len(rows))
	}
}

// shortWriter accepts limit bytes and then fails, the way a file on a filesystem
// that has just run out of space does.
type shortWriter struct {
	limit   int
	written []byte
}

func (s *shortWriter) Write(b []byte) (int, error) {
	n := len(b)
	if n > s.limit {
		n = s.limit
	}
	s.written = append(s.written, b[:n]...)
	if n < len(b) {
		return n, io.ErrShortWrite
	}
	// Reached by the one-byte fence write, which is small enough to fit under the limit.
	// It must be allowed to land, or this fake cannot tell the two failures apart.
	return n, nil
}

// A torn append has to be REPORTED, whatever it did to the file: the writer counts the
// minute as lost and warns, and a caller reading Dropped() is how anyone finds out.
// It also has to report HOW MUCH landed, because that is what tells its caller which rows
// are on disk and therefore not lost. The count excludes the fence newline: that byte is
// damage control rather than row data, and counting it would make the row the tear landed
// in look complete.
func TestAppendBytes_ATornWriteIsReported(t *testing.T) {
	f := &shortWriter{limit: 7}

	n, err := appendBytes(f, []byte(`{"at":"2026-09-13T09:14:00Z"}`+"\n"))
	if err == nil {
		t.Fatal("appendBytes swallowed a torn write; the caller has to be able to log it")
	}
	if n != 7 {
		t.Errorf("appendBytes reported %d bytes stored, want 7 — its caller derives which rows "+
			"survived from this number, and the fence byte is not one of them", n)
	}
}

// Nothing landed, so there is no fragment to fence: a file we have just been told we
// cannot write to should not be written to again on the way out.
func TestAppendBytes_FailureBeforeAnyByteWritesNothingFurther(t *testing.T) {
	f := &shortWriter{limit: 0}

	n, err := appendBytes(f, []byte("{}\n"))
	if err == nil {
		t.Fatal("appendBytes reported success for a write that wrote nothing")
	}
	if n != 0 {
		t.Errorf("appendBytes reported %d bytes stored by a write that accepted none", n)
	}
	if len(f.written) != 0 {
		t.Errorf("wrote %q to a file that accepted no bytes", f.written)
	}
}

// tornFile is a day file whose first write lands only partly, the way a write to a
// filesystem that has just run out of space does. The bytes that "landed" go to a REAL
// file underneath, so what the test inspects afterwards is the state a real ENOSPC
// leaves on disk rather than a fake's idea of it.
//
// *os.File is EMBEDDED rather than wrapped method by method so this satisfies whatever
// interface writeLinesTo asks of a day file — including a Stat/Truncate pair, if an edit
// ever reintroduces the rollback these tests exist to forbid. The test then fails on
// behaviour instead of failing to compile.
type tornFile struct {
	*os.File
	limit int
	// beforeWrite runs once, immediately before the torn write. The seam for the other
	// writer: it is what lands rows in the window between everything this writer did on
	// the way in and the write that tears.
	beforeWrite func()
	// syncs counts Sync calls, so a test can assert that what landed was pushed to the
	// device rather than left in the page cache. See
	// TestWriteLines_ATornAppendStillSyncsWhatLanded.
	syncs int
}

func (t *tornFile) Write(b []byte) (int, error) {
	if t.beforeWrite != nil {
		hook := t.beforeWrite
		t.beforeWrite = nil
		hook()
	}
	if len(b) <= t.limit {
		// The one-byte fence. It must reach the real file.
		return t.File.Write(b)
	}
	n, err := t.File.Write(b[:t.limit])
	if err != nil {
		return n, err
	}
	return n, io.ErrShortWrite
}

// Sync counts and delegates. Declared explicitly rather than inherited from the embedded
// *os.File so the call is observable; a real fsync still happens, because these tests
// assert against the file on disk.
func (t *tornFile) Sync() error {
	t.syncs++
	return t.File.Sync()
}

// encodedLen is how many bytes one row occupies in a day file, newline included.
//
// Derived from the encoder rather than hard-coded: the tests below place a tear at an
// exact offset relative to a row boundary, and a literal would silently stop meaning
// "just inside the second row" the next time Row gains a field.
func encodedLen(t *testing.T, r Row) int {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(b) + 1 // json.Encoder terminates each object with a newline
}

// tornRows is a row per name, all in one minute of one day.
func tornRows(names ...string) []Row {
	out := make([]Row, 0, len(names))
	for _, n := range names {
		out = append(out, Row{At: at, Endpoint: "gw", Model: n})
	}
	return out
}

// modelsOnDisk is which rows a day file can still be read back as.
func modelsOnDisk(t *testing.T, s *store) (map[string]bool, dayIssues) {
	t.Helper()
	rows, issues, err := s.readDay(at)
	if err != nil {
		t.Fatalf("readDay: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Model] = true
	}
	return got, issues
}

// THE ROLLBACK USED TO DESTROY ROWS IT DID NOT WRITE. writeLines took the file's size
// before its write and truncated back to it if that write tore, so anything appended in
// between — by a second proxy sharing the default ~/.cortex/cost, or by this process's
// own Close draining while a Flush wrote inline — was inside the range being unlinked.
// The failure mode was "one bad minute wipes the day file", silently: no error names it
// and nothing is left in the file to show it happened.
//
// The interleaving is driven deterministically rather than with goroutines: the other
// writer commits its rows from inside this writer's Write call, which is exactly the
// window the old code left open.
func TestWriteLines_ATornAppendDoesNotRollBackAnotherWritersRows(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir, 30, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	path := s.path(at)

	open := func() (dayFile, error) {
		f, oerr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if oerr != nil {
			return nil, oerr
		}
		return &tornFile{
			File: f,
			// Well inside the first row, so the tear is mid-line — the damaging case.
			limit: 20,
			beforeWrite: func() {
				if _, werr := writeLines(path, tornRows("committed-1", "committed-2")); werr != nil {
					t.Fatalf("the other writer's append failed, so this test proves nothing: %v", werr)
				}
			},
		}, nil
	}

	if _, werr := writeLinesTo(tornRows("mine-1", "mine-2"), open); werr == nil {
		t.Fatal("a torn append reported success; the premise of this test is that it fails")
	}

	got, issues := modelsOnDisk(t, s)
	for _, want := range []string{"committed-1", "committed-2"} {
		if !got[want] {
			t.Errorf("row %q is gone: a torn append rolled the day file back over rows another "+
				"writer had already committed, which loses history no error reports", want)
		}
	}
	if issues.skippedLines != 1 {
		t.Errorf("skippedLines = %d, want 1 — the loss from a torn append has to be bounded to "+
			"its own fragment AND counted, since that count is the only thing that tells an "+
			"operator the total is short", issues.skippedLines)
	}
}

// The fence: a torn append ends mid-row, and with no terminator the next append
// concatenates onto the fragment and makes ITS first row unreadable too. One byte keeps
// the damage to the fragment.
func TestWriteLines_ATornAppendDoesNotSwallowTheNextRowAppended(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir, 30, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	path := s.path(at)

	open := func() (dayFile, error) {
		f, oerr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if oerr != nil {
			return nil, oerr
		}
		return &tornFile{File: f, limit: 20}, nil
	}
	if _, werr := writeLinesTo(tornRows("torn"), open); werr == nil {
		t.Fatal("a torn append reported success; the premise of this test is that it fails")
	}

	// The next minute, written normally by whoever gets there first.
	if _, werr := writeLines(path, tornRows("after-1", "after-2")); werr != nil {
		t.Fatalf("writeLines after a torn append: %v", werr)
	}

	got, issues := modelsOnDisk(t, s)
	for _, want := range []string{"after-1", "after-2"} {
		if !got[want] {
			t.Errorf("row %q is unreadable: it was appended onto an unterminated fragment, so a "+
				"failed minute cost a later one too", want)
		}
	}
	if issues.skippedLines != 1 {
		t.Errorf("skippedLines = %d, want 1 — only the fragment itself", issues.skippedLines)
	}
}

// A TORN APPEND LOSES THE ROW IT TORE AND THE ONES AFTER IT — NOT THE WHOLE BATCH, which
// is what the count used to say.
//
// Counting the batch was correct only while a failed append truncated itself away, and
// that rollback is gone: the bytes a short write stored are in the file and nothing
// shortens it afterwards, so every row that ended before the tear is on disk and
// readable. Writer.Dropped is the only exported "is my cost history complete" signal, and
// over-reporting it is not a safe direction to be wrong in — an operator who learns it
// cries wolf stops believing it when it is right.
func TestWriteLines_ATornAppendCountsOnlyTheRowsItLost(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir, 30, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	path := s.path(at)
	rows := tornRows("landed", "torn", "never-written")
	// A few bytes INTO the second row: the first is whole on disk, the second is a
	// fragment, the third never left the buffer.
	limit := encodedLen(t, rows[0]) + 5

	open := func() (dayFile, error) {
		f, oerr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if oerr != nil {
			return nil, oerr
		}
		return &tornFile{File: f, limit: limit}, nil
	}

	lost, werr := writeLinesTo(rows, open)
	if werr == nil {
		t.Fatal("a torn append reported success; the premise of this test is that it fails")
	}
	if lost != 2 {
		t.Errorf("writeLinesTo reported %d rows lost, want 2 — the first row is durably on disk, "+
			"and counting it makes Dropped() report a row it can read back as missing", lost)
	}
	got, issues := modelsOnDisk(t, s)
	if !got["landed"] {
		t.Error("row \"landed\" is not readable, so the count above is measuring the wrong thing: " +
			"the rows before a tear are the ones this reports as NOT lost")
	}
	for _, gone := range []string{"torn", "never-written"} {
		if got[gone] {
			t.Errorf("row %q is readable; it was reported lost", gone)
		}
	}
	// The fragment, fenced off and counted — the bound on what a tear costs.
	if issues.skippedLines != 1 {
		t.Errorf("skippedLines = %d, want 1", issues.skippedLines)
	}
}

// THE FENCE HAS TO BE FSYNCED, and on the torn path the sync used to be skipped
// altogether.
//
// Two claims rest on it. The rows before the tear are exactly the ones writeLinesTo now
// reports as not lost, and that is a claim about the device rather than about the page
// cache. And the newline appendBytes appends after a tear exists purely for the crash
// case — an unterminated fragment swallowing the next row appended — so leaving the one
// byte whose whole purpose is crash-durability unsynced defeats the purpose of writing it.
func TestWriteLines_ATornAppendStillSyncsWhatLanded(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir, 30, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	path := s.path(at)
	rows := tornRows("landed", "torn")

	var tf *tornFile
	open := func() (dayFile, error) {
		f, oerr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if oerr != nil {
			return nil, oerr
		}
		tf = &tornFile{File: f, limit: encodedLen(t, rows[0]) + 5}
		return tf, nil
	}

	if _, werr := writeLinesTo(rows, open); werr == nil {
		t.Fatal("a torn append reported success; the premise of this test is that it fails")
	}
	if tf == nil {
		t.Fatal("the day file was never opened")
	}
	if tf.syncs != 1 {
		t.Errorf("Sync was called %d times after a torn append, want 1: the surviving rows are "+
			"reported as durable and the fence exists for the crash case, so neither may be left "+
			"in the page cache", tf.syncs)
	}
}

// Two writers on one day file, which ~/.cortex/cost being a fixed default makes
// ordinary: a spare proxy on another port, or an overlapping restart. With nothing on
// the write path that shortens a file, they can only append — every row lands whole and
// nothing rolls anything back.
//
// A property test, not a bug reproduction: it is what would catch a return to per-row
// encoding into the file, a handle without O_APPEND, or a new rollback.
func TestWriteLines_ConcurrentWritersDoNotLoseEachOthersRows(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir, 30, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	path := s.path(at)

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if _, werr := writeLines(path, tornRows(fmt.Sprintf("w%d-r%d", w, j))); werr != nil {
					errs <- werr
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for werr := range errs {
		t.Fatalf("writeLines: %v", werr)
	}

	got, issues := modelsOnDisk(t, s)
	if issues.skippedLines != 0 || issues.truncated {
		t.Errorf("issues = %+v, want none: concurrent appends must not corrupt a line", issues)
	}
	for i := 0; i < writers; i++ {
		for j := 0; j < perWriter; j++ {
			if want := fmt.Sprintf("w%d-r%d", i, j); !got[want] {
				t.Errorf("row %q is missing; a concurrent writer destroyed it", want)
			}
		}
	}
}

// Every line a successful write produces has to be independently decodable, because
// that is the property readDay's per-line resync depends on. A row written without a
// terminating newline would make the NEXT row unreadable.
func TestWriteLines_EveryLineEndsWithANewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "day.jsonl")
	rows := []Row{
		{At: at, Endpoint: "gw", Model: "opus"},
		{At: at, Endpoint: "gw", Model: "haiku"},
	}
	if _, err := writeLines(path, rows); err != nil {
		t.Fatalf("writeLines: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Error("the file does not end with a newline; the next append would concatenate")
	}
	for i, l := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
		var r Row
		if err := json.Unmarshal(l, &r); err != nil {
			t.Errorf("line %d is not independently decodable: %v", i, err)
		}
	}
}

// assertOwnership checks the rule Window's arithmetic rests on: while a minute is
// held in memory, nothing on disk carries that minute or a later one.
func assertOwnership(t *testing.T, w *Writer, dir, step string) {
	t.Helper()
	// Settle the writer first: the rule is about what is ON DISK versus what is held,
	// and a batch still in flight would make the disk side look emptier than it is.
	if err := w.sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	held, open, _ := w.pending()
	if open.IsZero() {
		// NOT A PASS — a state to check in its own right. This helper returned here silently,
		// and step 3 of the caller below constructs exactly this state on purpose (a Flush,
		// then an event in the already-flushed minute), so the one step whose invariant is
		// most interesting was asserting nothing at all. With no minute held, the rule "disk
		// carries nothing at or above the held minute" is vacuous — but only if nothing is
		// held, so that is what gets checked instead. Rows retained with no open minute are
		// invisible to Window's reconciliation either way.
		if len(held) != 0 {
			t.Errorf("after %s: %d row(s) held in memory with no open minute — Window stitches "+
				"the open minute, so these rows reach no reader and no day file", step, len(held))
		}
		return
	}
	for _, r := range readAllRows(t, dir) {
		if !r.At.Truncate(time.Minute).Before(open) {
			t.Errorf("after %s: disk row at %v is not below the held minute %v — "+
				"Window would count it twice", step, r.At, open)
		}
	}
}

// Every path that writes a row has to keep the ownership rule, so this drives all
// three of them in one sequence rather than trusting the one that is obvious.
func TestPendingMinute_IsNeverAlsoOnDisk(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	// 1. The ordinary path: accumulate, then roll so the minute is flushed.
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	assertOwnership(t, w, dir, "the first open minute")
	now = at.Add(time.Minute)
	second := costedEvent(t, "gw", "m", 0.25, 100, 50)
	second.At = now
	w.Record("s1", second)
	assertOwnership(t, w, dir, "a minute roll")

	// 2. A late event for a minute below the held one goes straight to disk.
	late := costedEvent(t, "gw", "m", 0.25, 100, 50)
	late.At = at.Add(-5 * time.Minute)
	w.Record("s1", late)
	assertOwnership(t, w, dir, "a late event")

	// 3. An event arriving in the SAME minute after a Flush must not be re-held —
	// the case a periodic settle or a shutdown flush creates.
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	sameMinute := costedEvent(t, "gw", "m", 0.25, 100, 50)
	sameMinute.At = now
	w.Record("s1", sameMinute)
	assertOwnership(t, w, dir, "an event in an already-flushed minute")
	// The step's OWN claim, which assertOwnership cannot make for it: "must not be re-held".
	// The helper can only check the rule about a held minute, and this step's whole point is
	// that no minute is held afterwards — so without this the assertion above passes for both
	// the correct behaviour and the bug.
	if _, open, _ := w.pending(); !open.IsZero() {
		t.Errorf("after a flush, an event in the already-flushed minute re-opened %v; that "+
			"minute is already on disk, so Window would count it twice", open)
	}
	if n := len(readAllRows(t, dir)); n != 4 {
		t.Errorf("disk holds %d rows after the same-minute event, want 4 — the guard must send "+
			"it to disk rather than drop it or hold it", n)
	}

	// And that last event must still be recorded somewhere — the guard sends it to
	// disk rather than dropping it.
	var total int64
	for _, r := range readAllRows(t, dir) {
		total += r.CostMicros
	}
	pending, _, _ := w.pending()
	for _, r := range pending {
		total += r.CostMicros
	}
	if want := int64(4 * 250_000); total != want {
		t.Errorf("total across disk and memory = %d, want %d — every event exactly once", total, want)
	}
}

// Retention deletes day files past the window and leaves everything else alone,
// including names it cannot date — deleting an unrecognised file under an
// operator-configured path is the one unrecoverable mistake available here.
func TestWriter_PruneDropsOldDaysAndSparesUnknownNames(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir, 3, time.Local)
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	old := at.AddDate(0, 0, -10)
	keep := at.AddDate(0, 0, -1)
	for _, d := range []time.Time{old, keep} {
		if werr := os.WriteFile(s.path(d), []byte("{}\n"), 0o600); werr != nil {
			t.Fatalf("seed: %v", werr)
		}
	}
	stranger := filepath.Join(dir, "notes.txt")
	if werr := os.WriteFile(stranger, []byte("hi"), 0o600); werr != nil {
		t.Fatalf("seed: %v", werr)
	}

	if perr := s.prune(at); perr != nil {
		t.Fatalf("prune: %v", perr)
	}

	if _, serr := os.Stat(s.path(old)); !os.IsNotExist(serr) {
		t.Errorf("the 10-day-old file survived a 3-day retention: %v", serr)
	}
	if _, serr := os.Stat(s.path(keep)); serr != nil {
		t.Errorf("yesterday's file was deleted: %v", serr)
	}
	if _, serr := os.Stat(stranger); serr != nil {
		t.Errorf("an undatable file was deleted: %v", serr)
	}
}

func TestWriter_RecordsTheAgentLabel(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"}
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Agent != "claude-code/2.1.14" {
		t.Errorf("Agent = %q, want claude-code/2.1.14", rows[0].Agent)
	}
}

func TestWriter_TwoAgentsInOneMinuteAreTwoRows(t *testing.T) {
	// The agent is part of the composite key, so two agents on the same endpoint
	// and model must not be merged into one row.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	a := costedEvent(t, "gw", "m", 0.25, 100, 50)
	a.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.14"}
	b := costedEvent(t, "gw", "m", 0.25, 100, 50)
	b.Client = &pipeline.EventClient{Name: "opencode", Version: "0.4.2"}
	w.Record("s1", a)
	w.Record("s1", b)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — one per agent", len(rows))
	}
}

func TestWriter_NoClientStillWritesTheRow(t *testing.T) {
	// Dropping unattributed traffic would make the ledger's totals disagree with
	// /v1/usage's, which is worse than an empty column.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Client = nil
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want the cost still recorded", rows[0].CostMicros)
	}
}

// TestWriter_AbsentClientStoresTheEmptyString pins the STORAGE representation, which
// deliberately differs from the aggregator's display one.
//
// The ledger is a durable file: writing the literal "unknown" into it would destroy
// the distinction between "no agent was recorded" and "an agent reported itself as
// unknown", permanently and for every future reader. Storing "" keeps the row
// lossless, and omitempty keeps it out of the file entirely. labelFor is where "" is
// mapped to the display bucket, so the two sources still AGREE about what a client
// sees — see labelFor.
func TestWriter_AbsentClientStoresTheEmptyString(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Client = nil
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); rows[0].Agent != "" {
		t.Errorf("Agent = %q, want the empty string: the ledger stores absence losslessly", rows[0].Agent)
	}
	// omitempty: the key must not appear at all, so an absent agent costs no bytes.
	if bytesContains(readAllBytes(t, dir), `"agent"`) {
		t.Error(`an absent agent serialized an "agent" key`)
	}
}

// TestWriter_UnrecognisedAgentStoresItsRawLabel is the reason Raw exists. A coding
// agent this parser does not know must still be nameable in the durable history,
// otherwise the day someone runs a new one is a day of spend attributed to nothing.
func TestWriter_UnrecognisedAgentStoresItsRawLabel(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Client = pipeline.ParseUserAgent("SomeNewAgent/9.9")
	w.Record("s1", e)

	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if rows := readAllRows(t, dir); rows[0].Agent != "SomeNewAgent/9.9" {
		t.Errorf("Agent = %q, want the raw UA", rows[0].Agent)
	}
}

// TestRecord_PricedImpliesPriceable pins the subset relation the coverage arithmetic
// depends on.
//
// A body-less response carrying the gateway's own cost header is PRICED with no parsed
// token counts, so the model-and-tokens test that sets PriceableRequests does not fire.
// Before the fix that left PricedRequests=1 against PriceableRequests=0, which inverts
// the subset and makes every consumer's `priceable - priced` gap NEGATIVE — failing
// their `> 0` test, so the coverage warning disappeared exactly when there was
// something to warn about.
func TestRecord_PricedImpliesPriceable(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })
	// in=0, out=0 so TotalTokens is 0: a body-less response the gateway priced by header,
	// which is exactly the traffic "Charge a body-less response that carries a cost
	// header" started charging.
	w.Record("s1", costedEvent(t, "gw.example", "claude-sonnet-5", 0.25, 0, 0))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.PricedRequests != 1 {
		t.Fatalf("PricedRequests = %d, want 1 — the cost header is the whole point of this case", r.PricedRequests)
	}
	if r.PriceableRequests < r.PricedRequests {
		t.Errorf("PriceableRequests = %d against PricedRequests = %d: priced must be a SUBSET of "+
			"priceable, or every consumer's priceable-minus-priced gap goes NEGATIVE, fails its `> 0` "+
			"test, and suppresses the coverage warning exactly when there is something to warn about",
			r.PriceableRequests, r.PricedRequests)
	}
}

// unparsedCostedEvent is a response inference-parser could not parse — an
// /v1/embeddings call, say — that the gateway nonetheless priced by header. No
// Inference extension, therefore no model and no token counts, but a settled cost.
func unparsedCostedEvent(t *testing.T, host string, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceGatewayHeader, Provenance: "authoritative",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	}
}

// TestRecord_APricedResponseWithNoInferenceExtensionIsStillRecorded closes the half of
// the fourth-bodyless-path fix that lives here.
//
// inference-parser only parses six chat/completion paths plus Anthropic Messages, so
// /v1/embeddings, /v1/rerank and /v1/moderations leave Extensions.Inference nil — and it
// settles them from the gateway's cost header anyway. Keying admission on the extension
// dropped that spend from the ledger while the live ring counted it, so the same money
// appeared in a 1h window and vanished from window=today. Both money surfaces default to
// window=today, which made the default view the wrong one.
func TestRecord_APricedResponseWithNoInferenceExtensionIsStillRecorded(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })
	w.Record("s1", unparsedCostedEvent(t, "gw.example", 0.25))
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — a gateway-priced response the parser could not read "+
			"is real spend and belongs in the day file", len(rows))
	}
	r := rows[0]
	if r.CostMicros != 250_000 {
		t.Errorf("CostMicros = %d, want 250000", r.CostMicros)
	}
	if r.Model != "" {
		t.Errorf("Model = %q, want empty — there was no model on the wire to record", r.Model)
	}
	// The subset relation the coverage arithmetic depends on. Model is "" and Tokens is 0,
	// so the model-and-tokens test cannot set PriceableRequests; the priced branch must.
	if r.PricedRequests != 1 {
		t.Errorf("PricedRequests = %d, want 1", r.PricedRequests)
	}
	if r.PriceableRequests < r.PricedRequests {
		t.Errorf("PriceableRequests = %d against PricedRequests = %d: priced must remain a "+
			"SUBSET of priceable, or every consumer's coverage gap goes negative and "+
			"suppresses its own warning", r.PriceableRequests, r.PricedRequests)
	}
}

// TestRecord_AnUnpricedNonInferenceResponseIsStillIgnored is the negative half, and it is
// what keeps the fix above from becoming the denominator bug it was written to avoid.
//
// A health check, an MCP call, a tunnel: no Inference extension AND no priced record.
// Admitting those would put every proxied response in the cost denominator, which is the
// mistake that once made a correctly configured deployment read "1/10 priced" forever.
func TestRecord_AnUnpricedNonInferenceResponseIsStillIgnored(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })
	// No plugins at all: the shape of a health check.
	w.Record("s1", &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw.example",
	})
	// A record that EXISTS but priced nothing must not get in either: it adds no dollars,
	// so admitting it would inflate the request count without moving the money.
	//
	// Settled:false is what makes it unpriced. A settled ZERO is a different thing and IS
	// admitted deliberately — costevent.Priced is `CostUSD > 0 || Settled`, because a
	// producer writing a settled zero means "this call was free", which is a fact worth a
	// row. Only something that priced nothing at all is turned away, and a health check
	// cannot reach even this far: it carries no record for the guard to consult.
	unsettled, merr := json.Marshal(costevent.Event{Source: costevent.SourceGatewayHeader})
	if merr != nil {
		t.Fatalf("marshal: %v", merr)
	}
	w.Record("s1", &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw.example",
		Plugins: map[string]json.RawMessage{costevent.Key: unsettled},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("got %d rows, want 0 — non-inference traffic carrying no priced figure must "+
			"stay out of the cost denominator: %+v", len(rows), rows)
	}
}

package importer

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// fakeSource yields fixed sessions, and then err when it is set. After each session is
// handled, it calls yielded when that is set. parsed counts the sessions that skip let through.
type fakeSource struct {
	sessions []Session
	err      error
	yielded  func()
	parsed   int
}

func (f *fakeSource) Agent() string       { return "claude-code" }
func (f *fakeSource) PricingHost() string { return "api.anthropic.com" }
func (f *fakeSource) Sessions(_ time.Time, skip func(string, time.Time) bool) iter.Seq2[Session, error] {
	return func(yield func(Session, error) bool) {
		for _, s := range f.sessions {
			if skip(s.ID, s.Updated) {
				continue
			}
			f.parsed++
			if !yield(s, nil) {
				return
			}
			if f.yielded != nil {
				f.yielded()
			}
		}
		if f.err != nil {
			yield(Session{}, f.err)
		}
	}
}

// fakeSession is id with turns request/response pairs, one minute apart from start.
func fakeSession(id string, start time.Time, turns int) Session {
	s := Session{ID: id, SizeHint: 1024}
	for i := range turns {
		at := start.Add(time.Duration(i) * time.Minute)
		rid := fmt.Sprintf("%s-r%d", id, i)
		s.Events = append(s.Events,
			model.SessionEvent{At: at, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: rid,
				Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5"}},
			model.SessionEvent{At: at.Add(time.Second), Direction: model.Outbound, Phase: model.SessionResponse, RequestID: rid,
				Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5", InputTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}})
		s.Updated = at.Add(time.Second)
	}
	return s
}

func testRates(t *testing.T) pricing.Resolver {
	t.Helper()
	tab, err := pricing.NewTable(pricing.Bundled())
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

func openArchive(t *testing.T, opts ...archive.Option) *archive.Archive {
	t.Helper()
	return openArchiveAt(t, t.TempDir(), opts...)
}

func openArchiveAt(t *testing.T, dir string, opts ...archive.Option) *archive.Archive {
	t.Helper()
	a, err := archive.Open(dir, append([]archive.Option{archive.WithLedgerRetentionDays(31)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func openLedger(t *testing.T, opts ...ledger.Option) *ledger.Writer {
	t.Helper()
	w, err := ledger.New(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// ledgerRows is every row the ledger holds, after a flush.
func ledgerRows(t *testing.T, w *ledger.Writer) []ledger.Row {
	t.Helper()
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, _, err := w.Window(context.Background(), time.Now().AddDate(0, 0, -40), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func sumRows(rows []ledger.Row) (requests, tokens int64) {
	for _, r := range rows {
		requests += r.Requests
		tokens += r.Tokens
	}
	return requests, tokens
}

// archivedEvents is every archived event of id, newest first.
func archivedEvents(t *testing.T, a *archive.Archive, id string) []*model.SessionEvent {
	t.Helper()
	var out []*model.SessionEvent
	if _, _, err := a.Page(id, 0, 1000, func(e *model.SessionEvent) bool {
		out = append(out, e)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

var start = time.Now().Add(-3 * time.Hour).Truncate(time.Minute)

func TestRun_ImportsNewSession(t *testing.T) {
	a := openArchive(t)
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 2)}},
		Options{Archive: a, Rates: testRates(t)})
	if r.Agent != "claude-code" || r.Imported != 1 || r.Replaced != 0 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	sum, ok := a.Lookup("s1")
	if !ok || sum.Source != "imported:claude-code" || sum.EventCount != 4 {
		t.Fatalf("Lookup = %+v, %v; want 4 events marked imported", sum, ok)
	}
	for _, e := range archivedEvents(t, a, "s1") {
		if !e.Imported() {
			t.Fatalf("event %d has Source %q", e.Seq, e.Source)
		}
	}
}

func TestRun_SkipsLiveInStore(t *testing.T) {
	a := openArchive(t)
	st := session.New(0, 0, 100)
	defer st.Close()
	st.Append("s1", model.SessionEvent{At: start, Direction: model.Outbound, Phase: model.SessionRequest})
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 2)}},
		Options{Archive: a, Store: st, Rates: testRates(t)})
	if r.SkippedLive != 1 || r.Imported != 0 {
		t.Fatalf("report = %+v, want 1 skipped live", r)
	}
	if _, ok := a.Lookup("s1"); ok {
		t.Fatal("the archive holds the live session")
	}
}

func TestRun_SkipsLiveInArchive(t *testing.T) {
	a := openArchive(t)
	live := fakeSession("s1", start, 1).Events
	if err := a.WriteSession("s1", live, false); err != nil { // no imported mark: live
		t.Fatal(err)
	}
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 3)}},
		Options{Archive: a, Rates: testRates(t)})
	if r.SkippedLive != 1 || r.Imported+r.Replaced != 0 {
		t.Fatalf("report = %+v, want 1 skipped live", r)
	}
	if n := len(archivedEvents(t, a, "s1")); n != 2 {
		t.Fatalf("archive has %d events, want the 2 live ones", n)
	}
}

func TestRun_SkipsCurrent(t *testing.T) {
	a := openArchive(t)
	src := &fakeSource{sessions: []Session{fakeSession("s1", start, 2)}}
	o := Options{Archive: a, Rates: testRates(t)}
	Run(context.Background(), src, o)
	bytes := a.Stats().Bytes
	src.parsed = 0
	r := Run(context.Background(), src, o)
	if r.SkippedCurrent != 1 || r.Imported+r.Replaced != 0 {
		t.Fatalf("report = %+v, want 1 skipped current", r)
	}
	if src.parsed != 0 {
		t.Fatalf("the source parsed %d sessions, want 0: skip decides before the parse", src.parsed)
	}
	if got := a.Stats().Bytes; got != bytes {
		t.Fatalf("archive bytes %d, want %d", got, bytes)
	}
}

func TestRun_ReplacesNewer(t *testing.T) {
	a := openArchive(t)
	o := Options{Archive: a, Rates: testRates(t)}
	Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 2)}}, o)
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 3)}}, o)
	if r.Replaced != 1 || r.Imported != 0 {
		t.Fatalf("report = %+v, want 1 replaced", r)
	}
	if n := len(archivedEvents(t, a, "s1")); n != 6 {
		t.Fatalf("archive has %d events, want 6", n)
	}
}

func TestRun_LiveEventClearsMarkThenSkips(t *testing.T) {
	dir := t.TempDir()
	a, err := archive.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Archive: a, Rates: testRates(t)}
	Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 2)}}, o)
	live := model.SessionEvent{Seq: a.LastSeq("s1") + 1, SessionID: "s1", At: time.Now(),
		Direction: model.Outbound, Phase: model.SessionRequest, RequestID: "live"}
	a.Record("s1", &live)
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start.Add(time.Hour), 3)}}, o)
	if r.SkippedLive != 1 || r.Replaced != 0 {
		t.Fatalf("report = %+v, want 1 skipped live", r)
	}
	// Close and open again, so the live event in the open segment is on disk and readable.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = openArchiveAt(t, dir)
	if sum, _ := a.Lookup("s1"); sum.Source != "" {
		t.Fatalf("Source %q, want the mark cleared by the live event", sum.Source)
	}
	found := false
	for _, e := range archivedEvents(t, a, "s1") {
		found = found || e.RequestID == "live"
	}
	if !found {
		t.Fatal("the live event is gone")
	}
}

func TestRun_StopsAtSizeLimit(t *testing.T) {
	// A dry run predicts the stop of a real run from the size hints.
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry=%v", dry), func(t *testing.T) {
			const maxBytes = 64 << 10
			a := openArchive(t, archive.WithMaxBytes(maxBytes))
			var ss []Session
			for i := range 3 {
				s := fakeSession(fmt.Sprintf("s%d", i), start, 2)
				s.SizeHint = maxBytes*9/10 - 1 // fits only in an empty archive
				ss = append(ss, s)
			}
			r := Run(context.Background(), &fakeSource{sessions: ss}, Options{Archive: a, Rates: testRates(t), DryRun: dry})
			if r.Stopped != "archive full" || r.Imported != 1 || r.NotImported != 2 {
				t.Fatalf("report = %+v, want a stop with sessions not imported", r)
			}
			if st := a.Stats(); st.Bytes > st.MaxBytes {
				t.Fatalf("archive bytes %d over max %d", st.Bytes, st.MaxBytes)
			}
		})
	}
}

func TestRun_CancelStopsBetweenSessions(t *testing.T) {
	a := openArchive(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &fakeSource{sessions: []Session{fakeSession("s1", start, 1), fakeSession("s2", start, 1)}, yielded: cancel}
	r := Run(ctx, src, Options{Archive: a, Rates: testRates(t)})
	if r.Imported != 1 || r.Error != context.Canceled.Error() {
		t.Fatalf("report = %+v, want 1 imported and the cancel error", r)
	}
	if _, ok := a.Lookup("s2"); ok {
		t.Fatal("the run wrote a session after the cancel")
	}
}

func TestRun_WriteErrorStopsRun(t *testing.T) {
	a := openArchive(t)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{sessions: []Session{fakeSession("s1", start, 1), fakeSession("s2", start, 1)}}
	r := Run(context.Background(), src, Options{Archive: a, Rates: testRates(t)})
	if r.Error == "" || r.Imported+r.SkippedLive != 0 {
		t.Fatalf("report = %+v, want the write error and nothing counted", r)
	}
}

// A ledger that cannot write stops the run after the session that hit it, with the error named.
func TestRun_LedgerWriteErrorStopsRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := t.TempDir()
	w, err := ledger.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	src := &fakeSource{sessions: []Session{fakeSession("s1", start, 1), fakeSession("s2", start, 1)}}
	r := Run(context.Background(), src, Options{Archive: openArchive(t), Ledger: w, Rates: testRates(t)})
	if !strings.HasPrefix(r.Error, "cost ledger: ") || r.Imported != 1 {
		t.Fatalf("report = %+v, want a cost ledger error after the first session", r)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	src := &fakeSource{sessions: []Session{fakeSession("s1", start, 2), fakeSession("s2", start, 1)}}
	r := Run(context.Background(), src, Options{Archive: a, Ledger: w, Rates: testRates(t), DryRun: true})
	if r.Imported != 2 {
		t.Fatalf("report = %+v, want 2 imported", r)
	}
	if _, ok := a.Lookup("s1"); ok {
		t.Fatal("a dry run wrote the archive")
	}
	if rows := ledgerRows(t, w); len(rows) != 0 {
		t.Fatalf("a dry run wrote %d ledger rows", len(rows))
	}
}

func TestRun_LedgerGetsAllTurnsOnImport(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 3)}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)})
	rows := ledgerRows(t, w)
	if req, tok := sumRows(rows); req != 3 || tok != 3300 {
		t.Fatalf("ledger has %d requests, %d tokens; want 3, 3300", req, tok)
	}
	for _, r := range rows {
		if r.Source != "transcript" || r.CostMicros <= 0 {
			t.Fatalf("row = %+v, want a priced transcript row", r)
		}
	}
}

// More back-dated turns than the ledger's queue holds (1024): none may be dropped.
func TestRun_LedgerKeepsEveryTurnOfALargeImport(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	old := time.Now().AddDate(0, 0, -20).Truncate(time.Minute)
	var ss []Session
	for i := range 40 {
		ss = append(ss, fakeSession(fmt.Sprintf("big%d", i), old, 200))
	}
	rep := Run(context.Background(), &fakeSource{sessions: ss}, Options{Archive: a, Ledger: w, Rates: testRates(t)})
	if rep.Error != "" || rep.Imported != 40 {
		t.Fatalf("report = %+v, want 40 imported and no error", rep)
	}
	if req, _ := sumRows(ledgerRows(t, w)); req != 8000 {
		t.Fatalf("ledger has %d requests, want 8000", req)
	}
	if d := w.Dropped(); d != 0 {
		t.Fatalf("ledger dropped %d rows, want 0", d)
	}
}

func TestRun_LedgerGetsOnlyNewTurnsOnReplace(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 2)}}, o)
	if req, _ := sumRows(ledgerRows(t, w)); req != 2 {
		t.Fatalf("ledger has %d requests after the import, want 2", req)
	}
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 3)}}, o)
	if r.Replaced != 1 {
		t.Fatalf("report = %+v, want 1 replaced", r)
	}
	if req, tok := sumRows(ledgerRows(t, w)); req != 3 || tok != 3300 {
		t.Fatalf("ledger has %d requests, %d tokens; want 3, 3300 (the new turn only added)", req, tok)
	}
}

func TestRun_LedgerSkipsTurnsPastCutoff(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t, ledger.WithRetentionDays(3))
	old := fakeSession("s1", w.RetentionCutoff().Add(-time.Hour), 1)
	r := Run(context.Background(), &fakeSource{sessions: []Session{old, fakeSession("s2", start, 1)}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)})
	if r.Imported != 2 {
		t.Fatalf("report = %+v, want 2 imported", r)
	}
	if req, _ := sumRows(ledgerRows(t, w)); req != 1 {
		t.Fatalf("ledger has %d requests, want 1: the turn before the cutoff is skipped", req)
	}
}

func TestRun_EmptySessionNotWritten(t *testing.T) {
	a := openArchive(t)
	r := Run(context.Background(), &fakeSource{sessions: []Session{{ID: "s1", Skipped: 3}}},
		Options{Archive: a, Rates: testRates(t)})
	if r.Imported != 0 || r.SkippedLines != 3 {
		t.Fatalf("report = %+v, want nothing imported and 3 lines skipped", r)
	}
	if _, ok := a.Lookup("s1"); ok {
		t.Fatal("an empty session was written")
	}
}

func TestRun_SourceErrorFailsAgentOnly(t *testing.T) {
	a := openArchive(t)
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("s1", start, 1)}, err: errors.New("bad store")},
		Options{Archive: a, Rates: testRates(t)})
	if r.Error != "bad store" || r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported and the error", r)
	}
}

// A live session the archive pruned is new to the import, but the ledger still has its live
// rows. Only the turn with no live row near it goes to the ledger.
func TestRun_LedgerSkipsTurnsRecordedLive(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	s := fakeSession("s1", start, 3)
	for i := range s.Events {
		s.Events[i].Project = "/repo"
	}
	// The proxy recorded the first turn live; the transcript has it a few seconds later.
	liveTurn := s.Events[1]
	w.Record("s1", &liveTurn)
	// A live row in another project does not hide the third turn.
	other := s.Events[5]
	other.Project = "/other"
	w.Record("s2", &other)
	Run(context.Background(), &fakeSource{sessions: []Session{s}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)})
	var imported int64
	for _, r := range ledgerRows(t, w) {
		if r.Source == "transcript" {
			imported += r.Requests
		}
	}
	// Turn 1 is live; turn 2 is one minute from it, so it is skipped too; turn 3 is imported.
	if imported != 1 {
		t.Fatalf("ledger has %d imported requests, want 1", imported)
	}
}

// A session past the archive's retention is not written: the next prune would delete it, and the
// next run would write it again.
func TestRun_SkipsSessionPastRetention(t *testing.T) {
	a := openArchive(t, archive.WithRetentionDays(3))
	old := fakeSession("old", time.Now().Add(-4*24*time.Hour), 2)
	r := Run(context.Background(), &fakeSource{sessions: []Session{old, fakeSession("new", start, 1)}},
		Options{Archive: a, Rates: testRates(t)})
	if r.Imported != 1 || r.SkippedExpired != 1 {
		t.Fatalf("report = %+v, want 1 imported and 1 past retention", r)
	}
	if _, ok := a.Lookup("old"); ok {
		t.Fatal("the run wrote a session past retention")
	}
}

// A cancel stops the run after an archive-full stop too.
func TestRun_CancelStopsAfterArchiveFull(t *testing.T) {
	const maxBytes = 64 << 10
	a := openArchive(t, archive.WithMaxBytes(maxBytes))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ss []Session
	for i := range 3 {
		s := fakeSession(fmt.Sprintf("s%d", i), start, 1)
		s.SizeHint = maxBytes // never fits
		ss = append(ss, s)
	}
	r := Run(ctx, &fakeSource{sessions: ss, yielded: cancel}, Options{Archive: a, Rates: testRates(t)})
	if r.Stopped != StoppedArchiveFull || r.NotImported != 1 || r.Error != context.Canceled.Error() {
		t.Fatalf("report = %+v, want the stop, 1 not imported and the cancel error", r)
	}
}

// sumCost is the requests, tokens and cost of every ledger row, after a flush.
func sumCost(t *testing.T, w *ledger.Writer) (requests, tokens, micros int64) {
	t.Helper()
	rows := ledgerRows(t, w)
	requests, tokens = sumRows(rows)
	for _, r := range rows {
		micros += r.CostMicros
	}
	return requests, tokens, micros
}

// erase deletes id from the archive, as DELETE /v1/sessions/{id} does.
func erase(t *testing.T, a *archive.Archive, id string) {
	t.Helper()
	a.Deleted(id)
	if _, err := a.AwaitDelete(id, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Lookup(id); ok {
		t.Fatalf("the archive still holds %s after the erase", id)
	}
}

// The user erases an imported session and imports again: it comes back, and the forget record
// keeps its cost from being counted a second time.
func TestRun_ErasedImportedSessionComesBackThroughRecord(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	src := &fakeSource{sessions: []Session{fakeSession("A", start, 2)}}
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	if r := Run(context.Background(), src, o); r.Imported != 1 {
		t.Fatalf("first report = %+v, want 1 imported", r)
	}
	req, tok, micros := sumCost(t, w)
	if req != 2 || micros <= 0 {
		t.Fatalf("ledger has %d requests, %d micros after the import; want 2, priced", req, micros)
	}
	erase(t, a, "A")
	if _, _, ok := a.Forgotten("A"); !ok {
		t.Fatal("the erase left no forget record")
	}
	if r := Run(context.Background(), src, o); r.Imported != 1 {
		t.Fatalf("second report = %+v, want 1 imported", r)
	}
	if sum, ok := a.Lookup("A"); !ok || sum.Source != "imported:claude-code" {
		t.Fatalf("Lookup = %+v, %v; want the session back, marked imported", sum, ok)
	}
	if r2, t2, m2 := sumCost(t, w); r2 != req || t2 != tok || m2 != micros {
		t.Fatalf("ledger has %d requests, %d tokens, %d micros; want %d, %d, %d (unchanged)", r2, t2, m2, req, tok, micros)
	}
}

// An erased live session whose transcript has turns from before Cortex saw it: only the turns
// before the record's span, less one minute of slack, reach the ledger.
func TestRun_ErasedLiveSessionWithEarlierTurns(t *testing.T) {
	a := openArchive(t, archive.WithClock(func() time.Time { return start.Add(10 * time.Minute) }))
	w := openLedger(t)
	var seq uint64
	for _, at := range []time.Time{start.Add(10 * time.Minute), start.Add(11 * time.Minute)} {
		for _, e := range turnPair(fmt.Sprintf("live-%d", seq), "", at) {
			seq++
			e.Seq, e.SessionID = seq, "A"
			a.Record("A", &e)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.LastSeq("A") < seq && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.LastSeq("A") < seq {
		t.Fatal("the live events were not recorded")
	}
	erase(t, a, "A")
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("A", start, 12)}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)})
	if r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	if req, tok, _ := sumCost(t, w); req != 9 || tok != 9900 {
		t.Fatalf("ledger has %d requests, %d tokens; want 9, 9900 (the turns before start+9m)", req, tok)
	}
}

// An erased imported session that grew in the transcript: only the turns after the erase count.
func TestRun_ErasedThenGrownCountsOnlyNewTurns(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("A", start, 2)}}, o)
	erase(t, a, "A")
	if r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("A", start, 5)}}, o); r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	if req, tok, _ := sumCost(t, w); req != 5 || tok != 5500 {
		t.Fatalf("ledger has %d requests, %d tokens; want 5, 5500 (2 + 3 new)", req, tok)
	}
}

// The size limit removes an imported session at Open, and a later import brings it back with no
// second count.
func TestRun_SizeLimitPruneThenImportCountsOnce(t *testing.T) {
	dir := t.TempDir()
	w := openLedger(t)
	src := &fakeSource{sessions: []Session{fakeSession("A", start, 2)}}
	a := openArchiveAt(t, dir)
	if r := Run(context.Background(), src, Options{Archive: a, Ledger: w, Rates: testRates(t)}); r.Imported != 1 {
		t.Fatalf("first report = %+v, want 1 imported", r)
	}
	req, tok, micros := sumCost(t, w)
	a.Close()
	a = openArchiveAt(t, dir, archive.WithMaxBytes(1))
	if _, ok := a.Lookup("A"); ok {
		t.Fatal("the size limit did not remove the session")
	}
	a.Close()
	a = openArchiveAt(t, dir)
	if r := Run(context.Background(), src, Options{Archive: a, Ledger: w, Rates: testRates(t)}); r.Imported != 1 {
		t.Fatalf("second report = %+v, want 1 imported", r)
	}
	if r2, t2, m2 := sumCost(t, w); r2 != req || t2 != tok || m2 != micros {
		t.Fatalf("ledger has %d requests, %d tokens, %d micros; want %d, %d, %d (unchanged)", r2, t2, m2, req, tok, micros)
	}
}

// A record for a session that is still in the archive (a crash between the record and the
// remove): the import follows the archive, and the record does not hide a new turn.
func TestRun_RecordWithSessionOnDiskFollowsArchive(t *testing.T) {
	dir := t.TempDir()
	w := openLedger(t)
	a := openArchiveAt(t, dir)
	Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("A", start, 2)}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)})
	a.Close()
	line := fmt.Sprintf(`{"id":"A","from":%d,"to":%d}`+"\n", start.Add(-time.Hour).UnixMilli(), start.Add(time.Hour).UnixMilli())
	f, err := os.OpenFile(filepath.Join(dir, "forgotten.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	f.Close()
	a = openArchiveAt(t, dir)
	if _, _, ok := a.Forgotten("A"); !ok {
		t.Fatal("the hand-written record did not load")
	}
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("A", start, 3)}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)})
	if r.Replaced != 1 {
		t.Fatalf("report = %+v, want 1 replaced", r)
	}
	if req, tok, _ := sumCost(t, w); req != 3 || tok != 3300 {
		t.Fatalf("ledger has %d requests, %d tokens; want 3, 3300 (the new turn added)", req, tok)
	}
}

// An erased live session is new to the import, so the transcript writes it back as imported.
func TestRun_ErasedLiveSessionComesBack(t *testing.T) {
	a := openArchive(t)
	live := model.SessionEvent{Seq: 1, SessionID: "A", At: start, Direction: model.Outbound,
		Phase: model.SessionRequest, RequestID: "live"}
	a.Record("A", &live)
	deadline := time.Now().Add(5 * time.Second)
	for a.LastSeq("A") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.LastSeq("A") == 0 {
		t.Fatal("the live event was not recorded")
	}
	erase(t, a, "A")
	r := Run(context.Background(), &fakeSource{sessions: []Session{fakeSession("A", start, 2)}},
		Options{Archive: a, Rates: testRates(t)})
	if r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	if sum, ok := a.Lookup("A"); !ok || sum.Source != "imported:claude-code" {
		t.Fatalf("Lookup = %+v, %v; want the session marked imported", sum, ok)
	}
}

// Two new sessions in one project and one minute, in one run: the rows of the first do not hide
// the turns of the second.
func TestRun_TwoNewSessionsInOneMinuteBothCount(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	sa, sb := fakeSession("A", start, 2), fakeSession("B", start.Add(10*time.Second), 2)
	for _, s := range []Session{sa, sb} {
		for i := range s.Events {
			s.Events[i].Project = "/repo"
		}
	}
	r := Run(context.Background(), &fakeSource{sessions: []Session{sa, sb}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)})
	if r.Imported != 2 {
		t.Fatalf("report = %+v, want 2 imported", r)
	}
	if req, tok, _ := sumCost(t, w); req != 4 || tok != 4400 {
		t.Fatalf("ledger has %d requests, %d tokens; want 4, 4400", req, tok)
	}
}

// A replace does not read the rows of the earlier import as a reason to skip: a new turn in the
// same minute as the last old turn still reaches the ledger.
func TestRun_ReplaceIgnoresOwnTranscriptRows(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	old := fakeSession("A", start, 2)
	Run(context.Background(), &fakeSource{sessions: []Session{old}}, o)
	grown := fakeSession("A", start, 2)
	last := old.Events[len(old.Events)-2].At // the last old request
	at := last.Add(10 * time.Second)
	grown.Events = append(grown.Events,
		model.SessionEvent{At: at, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: "A-r2",
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5"}},
		model.SessionEvent{At: at.Add(time.Second), Direction: model.Outbound, Phase: model.SessionResponse, RequestID: "A-r2",
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5", InputTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}})
	grown.Updated = at.Add(time.Second)
	if r := Run(context.Background(), &fakeSource{sessions: []Session{grown}}, o); r.Replaced != 1 {
		t.Fatalf("report = %+v, want 1 replaced", r)
	}
	if req, tok, _ := sumCost(t, w); req != 3 || tok != 3300 {
		t.Fatalf("ledger has %d requests, %d tokens; want 3, 3300", req, tok)
	}
}

// cancelRates cancels the run when the import prices its first event, so the cancel lands
// between the price and the ledger read.
type cancelRates struct {
	pricing.Resolver
	cancel func()
}

func (c cancelRates) Resolve(endpoint, model string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	c.cancel()
	return c.Resolver.Resolve(endpoint, model, promptTotal)
}

// A cancel after the archive write does not make the ledger read fail open: the erased
// session's turns are not counted a second time.
func TestRun_CancelAfterTheArchiveWriteCountsOnce(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	src := &fakeSource{sessions: []Session{fakeSession("A", start, 2)}}
	Run(context.Background(), src, Options{Archive: a, Ledger: w, Rates: testRates(t)})
	req, tok, micros := sumCost(t, w)
	erase(t, a, "A")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := Run(ctx, src, Options{Archive: a, Ledger: w, Rates: cancelRates{testRates(t), cancel}})
	if r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	if r2, t2, m2 := sumCost(t, w); r2 != req || t2 != tok || m2 != micros {
		t.Fatalf("ledger has %d requests, %d tokens, %d micros; want %d, %d, %d (unchanged)", r2, t2, m2, req, tok, micros)
	}
}

// A replace that writes a row in the minute of an erased session's earlier row does not make
// that earlier row look like this run's: the erased session's turn is not counted again.
func TestRun_ReplaceDoesNotHideAnErasedSessionsRow(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	at := start.Add(5 * time.Minute)
	sa, sb := fakeSession("A", at, 1), fakeSession("B", start, 1)
	if r := Run(context.Background(), &fakeSource{sessions: []Session{sa, sb}}, o); r.Imported != 2 {
		t.Fatalf("first report = %+v, want 2 imported", r)
	}
	erase(t, a, "A")
	grown := fakeSession("B", start, 1)
	grown.Events = append(grown.Events,
		model.SessionEvent{At: at.Add(10 * time.Second), Direction: model.Outbound, Phase: model.SessionRequest, RequestID: "B-r1",
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5"}},
		model.SessionEvent{At: at.Add(11 * time.Second), Direction: model.Outbound, Phase: model.SessionResponse, RequestID: "B-r1",
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5", InputTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}})
	grown.Updated = at.Add(11 * time.Second)
	r := Run(context.Background(), &fakeSource{sessions: []Session{grown, sa}}, o)
	if r.Replaced != 1 || r.Imported != 1 {
		t.Fatalf("second report = %+v, want 1 replaced and 1 imported", r)
	}
	if req, tok, _ := sumCost(t, w); req != 3 || tok != 3300 {
		t.Fatalf("ledger has %d requests, %d tokens; want 3, 3300", req, tok)
	}
}

// turnPair is one request/response pair of id rid: the request at at, the response a second later.
func turnPair(rid, project string, at time.Time) []model.SessionEvent {
	return []model.SessionEvent{
		{At: at, Direction: model.Outbound, Phase: model.SessionRequest, RequestID: rid, Project: project,
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5"}},
		{At: at.Add(time.Second), Direction: model.Outbound, Phase: model.SessionResponse, RequestID: rid, Project: project,
			Inference: &model.InferenceExtension{Model: "claude-sonnet-4-5", InputTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}},
	}
}

// A fork (claude --resume --fork-session) copies its parent's turns with the same request ids.
// When one run imports both, the copied turn reaches the ledger one time, in either order.
func TestRun_ForkedCopyCountsOnceInOneRun(t *testing.T) {
	parent := Session{ID: "P", SizeHint: 1024, Events: turnPair("P-r0", "/repo", start)}
	fork := Session{ID: "F", SizeHint: 1024,
		Events: append(turnPair("P-r0", "/repo", start), turnPair("F-r0", "/repo", start.Add(70*time.Second))...)}
	parent.Updated, fork.Updated = parent.Events[1].At, fork.Events[3].At
	for _, order := range [][]Session{{parent, fork}, {fork, parent}} {
		a := openArchive(t)
		w := openLedger(t)
		r := Run(context.Background(), &fakeSource{sessions: order}, Options{Archive: a, Ledger: w, Rates: testRates(t)})
		if r.Imported != 2 {
			t.Fatalf("report = %+v, want 2 imported", r)
		}
		if req, tok, _ := sumCost(t, w); req != 2 || tok != 2200 {
			t.Fatalf("first %s: ledger has %d requests, %d tokens; want 2, 2200", order[0].ID, req, tok)
		}
	}
}

// A fork that Cortex saw live is erased and imported: its copy of the parent's turn is before the
// record's span, and the parent's live row keeps it from being counted again.
func TestRun_ErasedForkDoesNotCountParentTurnAgain(t *testing.T) {
	forkAt := start.Add(10 * time.Minute)
	a := openArchive(t, archive.WithClock(func() time.Time { return forkAt }))
	w := openLedger(t)
	parentTurn := turnPair("P-r0", "/repo", start)
	w.Record("P", &parentTurn[1])
	forkTurn := turnPair("F-r0", "/repo", forkAt)
	w.Record("F", &forkTurn[1])
	for i, e := range forkTurn {
		e.Seq, e.SessionID = uint64(i+1), "F"
		a.Record("F", &e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.LastSeq("F") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.LastSeq("F") < 2 {
		t.Fatal("the live events were not recorded")
	}
	erase(t, a, "F")
	fork := Session{ID: "F", SizeHint: 1024, Events: append(slices.Clone(parentTurn), forkTurn...)}
	fork.Updated = forkTurn[1].At
	if r := Run(context.Background(), &fakeSource{sessions: []Session{fork}},
		Options{Archive: a, Ledger: w, Rates: testRates(t)}); r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", r)
	}
	if req, tok, _ := sumCost(t, w); req != 2 || tok != 2200 {
		t.Fatalf("ledger has %d requests, %d tokens; want 2, 2200 (the two live rows)", req, tok)
	}
}

// A fork of an imported parent copies the parent's turn. A later run that imports the fork, or
// that brings back an erased parent with the fork, does not count that turn again.
func TestRun_ForkOfImportedParentCountsParentTurnOnce(t *testing.T) {
	parent := Session{ID: "P", SizeHint: 1024, Events: turnPair("P-r0", "/repo", start)}
	parent.Updated = parent.Events[1].At
	fork := Session{ID: "F", SizeHint: 1024,
		Events: append(turnPair("P-r0", "/repo", start), turnPair("F-r0", "/repo", start.Add(10*time.Minute))...)}
	fork.Updated = fork.Events[3].At
	for _, tc := range []struct {
		name  string
		erase bool
		order []Session
	}{
		{"fork in a later run", false, []Session{parent, fork}},
		{"erased parent then fork", true, []Session{parent, fork}},
		{"fork then erased parent", true, []Session{fork, parent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := openArchive(t)
			w := openLedger(t)
			o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
			Run(context.Background(), &fakeSource{sessions: []Session{parent}}, o)
			if tc.erase {
				erase(t, a, "P")
			}
			Run(context.Background(), &fakeSource{sessions: tc.order}, o)
			if req, tok, _ := sumCost(t, w); req != 2 || tok != 2200 {
				t.Fatalf("ledger has %d requests, %d tokens; want 2, 2200 (P-r0 and F-r0 once each)", req, tok)
			}
		})
	}
}

// A turn after a forget record's span lands in a minute where an earlier run wrote a row of
// another session. A fork of that session later in the same run does not count its copy of the
// row's turn again.
func TestRun_TurnAfterSpanKeepsEarlierRowsOfItsMinute(t *testing.T) {
	m := start.Add(10 * time.Minute)
	p1 := Session{ID: "P", SizeHint: 1024, Events: turnPair("P-r0", "/repo", start)}
	p1.Updated = p1.Events[1].At
	q := Session{ID: "Q", SizeHint: 1024, Events: turnPair("Q-r0", "/repo", m)}
	q.Updated = q.Events[1].At
	p2 := Session{ID: "P", SizeHint: 1024, Events: append(turnPair("P-r0", "/repo", start), turnPair("P-r1", "/repo", m)...)}
	p2.Updated = p2.Events[3].At
	f := Session{ID: "F", SizeHint: 1024,
		Events: append(turnPair("Q-r0", "/repo", m), turnPair("F-r0", "/repo", start.Add(20*time.Minute))...)}
	f.Updated = f.Events[3].At
	a := openArchive(t)
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	Run(context.Background(), &fakeSource{sessions: []Session{p1, q}}, o)
	erase(t, a, "P")
	Run(context.Background(), &fakeSource{sessions: []Session{p2, f}}, o)
	if req, tok, _ := sumCost(t, w); req != 4 || tok != 4400 {
		t.Fatalf("ledger has %d requests, %d tokens; want 4, 4400 (P-r0, Q-r0, P-r1, F-r0)", req, tok)
	}
}

// A fork of an imported parent runs live, is erased and is imported. Its copy of the parent's
// turn is before the record's span, and the parent's imported row keeps it from counting again.
func TestRun_ErasedLiveForkOfImportedParent(t *testing.T) {
	forkAt := start.Add(10 * time.Minute)
	a := openArchive(t, archive.WithClock(func() time.Time { return forkAt }))
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	parent := Session{ID: "P", SizeHint: 1024, Events: turnPair("P-r0", "/repo", start)}
	parent.Updated = parent.Events[1].At
	if r := Run(context.Background(), &fakeSource{sessions: []Session{parent}}, o); r.Imported != 1 {
		t.Fatalf("first report = %+v, want 1 imported", r)
	}
	forkTurn := turnPair("F-r0", "/repo", forkAt)
	w.Record("F", &forkTurn[1])
	for i, e := range forkTurn {
		e.Seq, e.SessionID = uint64(i+1), "F"
		a.Record("F", &e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.LastSeq("F") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.LastSeq("F") < 2 {
		t.Fatal("the live events were not recorded")
	}
	erase(t, a, "F")
	fork := Session{ID: "F", SizeHint: 1024, Events: append(turnPair("P-r0", "/repo", start), forkTurn...)}
	fork.Updated = forkTurn[1].At
	if r := Run(context.Background(), &fakeSource{sessions: []Session{fork}}, o); r.Imported != 1 {
		t.Fatalf("second report = %+v, want 1 imported", r)
	}
	if req, tok, _ := sumCost(t, w); req != 2 || tok != 2200 {
		t.Fatalf("ledger has %d requests, %d tokens; want 2, 2200 (P-r0 and F-r0 once each)", req, tok)
	}
}

// A record holds Unix milliseconds. A last request with a time below the millisecond is still in
// the span, so a re-import after the erase does not count it again.
func TestRun_SubMillisecondLastRequestStaysInSpan(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	s := Session{ID: "A", SizeHint: 1024,
		Events: append(turnPair("A-r0", "", start), turnPair("A-r1", "", start.Add(time.Minute+500*time.Microsecond))...)}
	s.Updated = s.Events[3].At
	Run(context.Background(), &fakeSource{sessions: []Session{s}}, o)
	erase(t, a, "A")
	if r := Run(context.Background(), &fakeSource{sessions: []Session{s}}, o); r.Imported != 1 {
		t.Fatalf("second report = %+v, want 1 imported", r)
	}
	if req, tok, _ := sumCost(t, w); req != 2 || tok != 2200 {
		t.Fatalf("ledger has %d requests, %d tokens; want 2, 2200 (unchanged)", req, tok)
	}
}

// A replace sends each turn that the earlier import did not have, also when its request is
// before the newest request of that import: a parallel subagent was mid-turn during it.
func TestRun_ReplaceKeepsAParallelTurnWithAnEarlierRequest(t *testing.T) {
	a := openArchive(t)
	w := openLedger(t)
	o := Options{Archive: a, Ledger: w, Rates: testRates(t)}
	old := Session{ID: "A", SizeHint: 1024,
		Events: append(turnPair("A-r0", "/repo", start), turnPair("A-r1", "/repo", start.Add(5*time.Minute))...)}
	old.Updated = old.Events[3].At
	Run(context.Background(), &fakeSource{sessions: []Session{old}}, o)

	grown := old
	grown.Events = append(slices.Clone(old.Events), turnPair("A-r3", "/repo", start.Add(7*time.Minute))...)
	// A-r2's request came before A-r1's, and its response after the earlier import.
	r2 := turnPair("A-r2", "/repo", start.Add(2*time.Minute))
	r2[1].At = start.Add(6 * time.Minute)
	grown.Events = append(grown.Events, r2...)
	grown.Updated = start.Add(7*time.Minute + time.Second)
	if r := Run(context.Background(), &fakeSource{sessions: []Session{grown}}, o); r.Replaced != 1 {
		t.Fatalf("report = %+v, want 1 replaced", r)
	}
	if req, tok, _ := sumCost(t, w); req != 4 || tok != 4400 {
		t.Fatalf("ledger has %d requests, %d tokens; want 4, 4400", req, tok)
	}
}

// A session whose transcript the source could not read is counted, and nothing is written.
func TestRun_UnreadableSessionIsCounted(t *testing.T) {
	a := openArchive(t)
	r := Run(context.Background(), &fakeSource{sessions: []Session{{ID: "x", Unreadable: true}, fakeSession("s1", start, 1)}},
		Options{Archive: a})
	if r.Unreadable != 1 || r.Imported != 1 {
		t.Fatalf("report = %+v, want 1 unreadable and 1 imported", r)
	}
	if _, ok := a.Lookup("x"); ok {
		t.Fatal("the unreadable session was archived")
	}
}

package sessionexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// base is the fixture's clock: s1's events run forward from it, old's sit a week behind it.
var base = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// fixtureArchive builds an archive holding s1 — six events a minute apart, the third an
// opaque tunnel open, each on its own host so a misattributed record names itself in a
// failure — and old, whose three events sit a week earlier. The store's clock follows each
// append, so a session's UpdatedAt is its newest event's time and a Since floor sees the
// sessions where it should.
//
// The archive is closed and reopened, as the session API's fixtures do, so a walk reads
// what a later process would: nothing still queued in memory.
func fixtureArchive(t *testing.T) *archive.Archive {
	t.Helper()
	root := t.TempDir()
	write, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := base
	store := session.New(0, 0, 0, session.WithClock(func() time.Time { return now }))
	store.AddRecorder(write)
	for i := range 6 {
		at := base.Add(time.Duration(i) * time.Minute)
		e := model.SessionEvent{At: at, Direction: model.Outbound, Phase: model.SessionRequest,
			Host: "h" + string(rune('a'+i)), HTTPMethod: "POST"}
		if i == 2 {
			e.Tunnel, e.TunnelReason, e.HTTPMethod = true, model.TunnelPassthroughHost, "CONNECT"
		}
		now = at
		store.Append("s1", e)
	}
	for i := range 3 {
		at := base.Add(-168 * time.Hour).Add(time.Duration(i) * time.Minute)
		now = at
		store.Append("old", model.SessionEvent{At: at, Direction: model.Outbound,
			Phase: model.SessionRequest, Host: "old" + string(rune('a'+i)), HTTPMethod: "POST"})
	}
	store.Close()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// walkLines runs a walk and returns its decoded records and stats.
func walkLines(t *testing.T, a *archive.Archive, f Filter) ([]Record, WalkStats) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	stats, err := Walk(a, w, f)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var out []Record
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line is not a record: %q: %v", line, err)
		}
		out = append(out, r)
	}
	if len(out) != stats.Records {
		t.Fatalf("decoded %d records, stats say %d", len(out), stats.Records)
	}
	return out, stats
}

// A walk with no filter carries every archived event — tunnel rows included, which is the
// whole difference from ReplaySince and a third of a real archive — newest first within a
// session, sessions most recently updated first.
func TestWalk_CarriesEveryEventIncludingTunnels(t *testing.T) {
	rs, stats := walkLines(t, fixtureArchive(t), Filter{})
	if stats.Sessions != 2 || stats.Records != 9 {
		t.Fatalf("stats = %+v, want 2 sessions 9 records", stats)
	}
	var s1 []uint64
	for _, r := range rs {
		if r.Schema != SchemaVersion {
			t.Errorf("record %v: schema %d, want %d", r, r.Schema, SchemaVersion)
		}
		if r.SessionID == "s1" {
			s1 = append(s1, r.Seq)
			if r.Host == "" {
				t.Errorf("s1 seq %d has no host", r.Seq)
			}
		}
	}
	if !slices.Equal(s1, []uint64{6, 5, 4, 3, 2, 1}) {
		t.Fatalf("s1 seqs %v, want 6..1 newest first", s1)
	}
	var tunnel int
	for _, r := range rs {
		if r.Tunnel {
			tunnel++
			if r.TunnelReason != string(model.TunnelPassthroughHost) {
				t.Errorf("tunnel record reason %q", r.TunnelReason)
			}
		}
	}
	if tunnel != 1 {
		t.Fatalf("%d tunnel records, want the one the fixture wrote", tunnel)
	}
}

// A Sessions filter carries only the named sessions; an id the archive does not hold is an
// error naming both what was asked for and what is there, so a typo cannot export nothing
// for it silently.
func TestWalk_SessionFilter(t *testing.T) {
	rs, stats := walkLines(t, fixtureArchive(t), Filter{Sessions: setOf("s1")})
	if stats.Sessions != 1 || stats.Records != 6 {
		t.Fatalf("stats = %+v, want s1's 6 records", stats)
	}
	for _, r := range rs {
		if r.SessionID != "s1" {
			t.Fatalf("record from %q under an s1-only filter", r.SessionID)
		}
	}

	a := fixtureArchive(t)
	var buf bytes.Buffer
	_, err := Walk(a, NewWriter(&buf), Filter{Sessions: setOf("s1", "nope")})
	var unknown *UnknownSessionsError
	if err == nil || !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want UnknownSessionsError", err)
	}
	if !slices.Equal(unknown.Requested, []string{"nope"}) || !slices.Equal(unknown.Known, []string{"old", "s1"}) {
		t.Fatalf("unknown = requested %v known %v", unknown.Requested, unknown.Known)
	}
	if buf.Len() != 0 {
		t.Fatalf("filter error wrote %d bytes; it must come before any record", buf.Len())
	}
}

// Since skips a session the archive last touched before the floor whole, and stops a
// session that straddles it at its first event below — carrying the tail, not re-decoding
// the whole session to filter it.
func TestWalk_SinceSkipsAndStops(t *testing.T) {
	floor := base.Add(2*time.Minute + 30*time.Second)
	rs, stats := walkLines(t, fixtureArchive(t), Filter{Since: floor})
	if stats.Sessions != 1 || stats.Records != 3 {
		t.Fatalf("stats = %+v, want s1's tail of 3 records", stats)
	}
	var seqs []uint64
	for _, r := range rs {
		if r.SessionID != "s1" {
			t.Fatalf("record from %q, want old skipped whole", r.SessionID)
		}
		if r.At.Before(floor) {
			t.Fatalf("seq %d at %s is below the floor %s", r.Seq, r.At, floor)
		}
		seqs = append(seqs, r.Seq)
	}
	if !slices.Equal(seqs, []uint64{6, 5, 4}) {
		t.Fatalf("seqs %v, want [6 5 4]", seqs)
	}

	// A floor above everything carries nothing and fails nothing.
	rs, stats = walkLines(t, fixtureArchive(t), Filter{Since: base.Add(24 * time.Hour)})
	if len(rs) != 0 || stats.Sessions != 0 || stats.Records != 0 {
		t.Fatalf("floor above all: %d records, stats %+v", len(rs), stats)
	}
}

// An empty archive is an empty export, not an error: a fresh install has nothing to say.
func TestWalk_EmptyArchiveIsEmpty(t *testing.T) {
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	rs, stats := walkLines(t, a, Filter{})
	if len(rs) != 0 || stats.Sessions != 0 || stats.Records != 0 {
		t.Fatalf("empty archive: %d records, stats %+v", len(rs), stats)
	}
}

// The privacy claim of this package, held one layer up from TestRecord_CarriesNoContent:
// an event round-tripped through the store and the archive — through the split/join
// string table, not just the projection — leaves none of the planted secrets in the walk's
// output bytes.
func TestWalk_CarriesNoContent(t *testing.T) {
	root := t.TempDir()
	write, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	e := *loadedEvent()
	store := session.New(0, 0, 0)
	store.AddRecorder(write)
	store.Append("sess-1", e)
	store.Close()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	var buf bytes.Buffer
	if _, err := Walk(a, NewWriter(&buf), Filter{}); err != nil {
		t.Fatal(err)
	}
	for _, secret := range allSecrets() {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("walk output contains %q", secret)
		}
	}
	if buf.Len() == 0 {
		t.Fatal("walk of a loaded event wrote nothing")
	}
}

func setOf(ids ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

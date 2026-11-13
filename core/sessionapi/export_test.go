package sessionapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/sessionexport"
)

// exportBase is the fixture's clock: s1's events run an hour apart from it, old's sit a
// week behind it, and the server's clock is pinned a day past both so a ?since floor
// lands where a test says rather than where the machine is.
var (
	exportBase   = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	exportNow    = exportBase.Add(24 * time.Hour)
	exportSecret = "SECRET-EXPORT-PROMPT"
)

// exportFixture builds a server whose archive holds s1 — six events an hour apart, the
// third an opaque tunnel open, the first carrying a prompt this file asserts never leaves
// — and old, whose three events sit a week earlier. The archive is closed and reopened so
// the endpoint reads what a later process would.
func exportFixture(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	write, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := exportBase
	store := session.New(0, 0, 0, session.WithClock(func() time.Time { return now }))
	store.AddRecorder(write)
	for i := range 6 {
		at := exportBase.Add(time.Duration(i) * time.Hour)
		e := model.SessionEvent{At: at, Direction: model.Outbound, Phase: model.SessionRequest,
			Host: "h" + string(rune('a'+i)), HTTPMethod: "POST"}
		if i == 0 {
			e.Inference = &model.InferenceExtension{Model: "m",
				Messages: []model.InferenceMessage{{Role: "user", Content: exportSecret}}}
		}
		if i == 2 {
			e.Tunnel, e.TunnelReason, e.HTTPMethod, e.Inference = true, model.TunnelPassthroughHost, "CONNECT", nil
		}
		now = at
		store.Append("s1", e)
	}
	for i := range 3 {
		at := exportBase.Add(-168 * time.Hour).Add(time.Duration(i) * time.Hour)
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
	srv := New(":0", session.New(0, 0, 0), WithArchive(a),
		WithClock(func() time.Time { return exportNow }))
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() {
		ts.Close()
		a.Close()
	})
	return ts
}

// exportRecords GETs path and decodes its body as records.
func exportRecords(t *testing.T, ts *httptest.Server, path string) (int, string, []sessionexport.Record) {
	t.Helper()
	resp, body := getPath(t, ts.URL, path)
	out := []sessionexport.Record{}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var r sessionexport.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("GET %s: line is not a record: %q: %v", path, line, err)
		}
		out = append(out, r)
	}
	return resp.StatusCode, body, out
}

// The export carries every archived event — tunnel rows included — as content-free
// records: content-type x-ndjson, one decodable record per line, schema 1, and none of the
// prompt text the fixture planted. That last assert is TestRecord_CarriesNoContent one
// layer up: the privacy claim held at the projection, held again at the HTTP surface.
func TestHandleExport_ServesEveryArchivedEvent(t *testing.T) {
	code, body, rs := exportRecords(t, exportFixture(t), "/v1/export")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/export = %d %s", code, body)
	}
	if len(rs) != 9 {
		t.Fatalf("%d records, want 9 (6 + 3, tunnel row included)", len(rs))
	}
	var tunnel int
	for _, r := range rs {
		if r.Schema != sessionexport.SchemaVersion {
			t.Errorf("record %v: schema %d", r, r.Schema)
		}
		if r.Tunnel {
			tunnel++
		}
	}
	if tunnel != 1 {
		t.Fatalf("%d tunnel records, want the fixture's one", tunnel)
	}
	if strings.Contains(body, exportSecret) {
		t.Fatal("export body contains the planted prompt text")
	}
}

// ?session= carries only the named sessions; an unknown id is a 400 naming it and the
// archived ones; an empty value is an absent one, as an empty since is.
func TestHandleExport_SessionParameter(t *testing.T) {
	ts := exportFixture(t)
	code, _, rs := exportRecords(t, ts, "/v1/export?session=s1")
	if code != http.StatusOK || len(rs) != 6 {
		t.Fatalf("?session=s1 = %d, %d records", code, len(rs))
	}
	for _, r := range rs {
		if r.SessionID != "s1" {
			t.Fatalf("record from %q under an s1-only filter", r.SessionID)
		}
	}

	code, body, _ := exportRecords(t, ts, "/v1/export?session=nope")
	if code != http.StatusBadRequest || !strings.Contains(body, "nope") ||
		!strings.Contains(body, "old") || !strings.Contains(body, "s1") {
		t.Fatalf("?session=nope = %d %s, want 400 naming nope and the archived ids", code, body)
	}

	if code, _, rs := exportRecords(t, ts, "/v1/export?session="); code != http.StatusOK || len(rs) != 9 {
		t.Fatalf("?session= = %d, %d records, want the empty value read as absent", code, len(rs))
	}
}

// A refusal naming a WIDE archive of LONG ids still fits what a client reads of a 400
// body. This is the case a count cap could not hold and the fixture above cannot reach:
// its two ids are "s1" and "old", so its refusal is 60-odd bytes however the message is
// rendered. Here 50 archived sessions at UUID length behind 5 unknown ids rendered 538
// bytes under the old cap — past agentop's 512-byte read, so the detail was dropped and
// the user saw a bare "apiclient: bad request" (#1830, #1831 review).
//
// The budget lives in core/sessionexport; what this test pins is the contract at the HTTP
// surface, where the message is wrapped in `{"error":...}` and the client's limit applies.
func TestHandleExport_UnknownSessionRefusalFitsAClientRead(t *testing.T) {
	root := t.TempDir()
	write, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := exportBase
	store := session.New(0, 0, 0, session.WithClock(func() time.Time { return now }))
	store.AddRecorder(write)
	for i := range 50 {
		id := fmt.Sprintf("3f2504e0-4f89-11d3-9a0c-0305e82c33%02d", i)
		at := exportBase.Add(time.Duration(i) * time.Minute)
		now = at
		store.Append(id, model.SessionEvent{At: at, Direction: model.Outbound,
			Phase: model.SessionRequest, Host: "h", HTTPMethod: "POST"})
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
	ts := httptest.NewServer(New(":0", session.New(0, 0, 0), WithArchive(a),
		WithClock(func() time.Time { return exportNow })).server.Handler)
	t.Cleanup(ts.Close)

	q := "/v1/export"
	for i := range 5 {
		q += fmt.Sprintf("&session=nope-%s-%d", strings.Repeat("f", 30), i)
	}
	code, body := getBody(t, ts.URL+strings.Replace(q, "&", "?", 1))
	if code != http.StatusBadRequest {
		t.Fatalf("GET %s = %d %s, want 400", q, code, body)
	}
	// agentop reads 512 bytes of a 400 body and parses the "error" field out of it; past
	// that the JSON is cut mid-string and the detail is lost entirely.
	if len(body) >= 512 {
		t.Errorf("400 body is %d bytes; a client reading 512 cannot parse it: %s", len(body), body)
	}
	// Bounded is not useful if it names nothing actionable.
	var refusal struct{ Error string }
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("400 body is not parseable JSON: %v: %s", err, body)
	}
	if !strings.Contains(refusal.Error, "nope-") {
		t.Errorf("refusal names none of the requested ids: %s", refusal.Error)
	}
	if !strings.Contains(refusal.Error, "3f2504e0") || !strings.Contains(refusal.Error, "more") {
		t.Errorf("refusal does not name the archived ids and count the rest: %s", refusal.Error)
	}
}

// ?since= carries only events newer than that far back: a session the archive last
// touched before the floor is skipped whole, one that straddles it stops at its first
// event below. 7d reads as a day count, which time.ParseDuration alone rejects.
func TestHandleExport_SinceParameter(t *testing.T) {
	ts := exportFixture(t)
	// exportNow is a day past s1's newest event and a week past old's, so 21h30m floors
	// between s1's third and fourth events.
	code, _, rs := exportRecords(t, ts, "/v1/export?since=21h30m")
	if code != http.StatusOK || len(rs) != 3 {
		t.Fatalf("?since=21h30m = %d, %d records, want s1's tail of 3", code, len(rs))
	}
	for _, r := range rs {
		if r.SessionID != "s1" {
			t.Fatalf("record from %q, want old skipped whole", r.SessionID)
		}
	}

	// 7d floors 22h short of old's newest event — old is skipped whole and s1's 6 carry —
	// and 8d carries all 9. Both boundaries are the point: the floor is event time, not
	// session age, and a day count reads where time.ParseDuration alone would not.
	if code, _, rs := exportRecords(t, ts, "/v1/export?since=7d"); code != http.StatusOK || len(rs) != 6 {
		t.Fatalf("?since=7d = %d, %d records, want s1's 6 with old outside the floor", code, len(rs))
	}
	if code, _, rs := exportRecords(t, ts, "/v1/export?since=8d"); code != http.StatusOK || len(rs) != 9 {
		t.Fatalf("?since=8d = %d, %d records, want all 9", code, len(rs))
	}
	// A single empty value reads as absent — ?since= alone is a trailing separator — but
	// a second value is a caller bug and a 400, however valid the first one looks: taking
	// the first silently would let ?since=&since=7d and ?since=1h&since=garbage mean
	// different things than they appear to.
	if code, _, rs := exportRecords(t, ts, "/v1/export?since="); code != http.StatusOK || len(rs) != 9 {
		t.Fatalf("?since= = %d, %d records, want the empty value read as absent", code, len(rs))
	}
	for _, raw := range []string{"garbage", "-5h", "0s", "7x", "d", "106752d", "200000d"} {
		code, body, _ := exportRecords(t, ts, "/v1/export?since="+raw)
		if code != http.StatusBadRequest {
			t.Fatalf("?since=%s = %d %s, want 400", raw, code, body)
		}
	}
	for _, q := range []string{"since=7d&since=garbage", "since=&since=7d", "since=1h&since=1h"} {
		code, body, _ := exportRecords(t, ts, "/v1/export?"+q)
		if code != http.StatusBadRequest {
			t.Fatalf("?%s = %d %s, want 400: a repeated since is a caller bug", q, code, body)
		}
	}
}

// A walk that fails before its first record answers 500, not 200 with an empty body: a
// segment that cannot be opened is a broken archive, and on the wire it was byte for byte
// an empty one — "nothing happened this month" on both sides (#1830 review). The fixture
// makes the segment unreadable behind the read-side archive's back, after Open has
// reconciled the metadata Summaries reads; a segment whose CONTENT is damaged is a
// different, tolerated case (readSegment serves the readable prefix and calls the rest
// truncated), which this test deliberately does not touch.
func TestHandleExport_BrokenSegmentIs500(t *testing.T) {
	root := t.TempDir()
	write, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := exportBase
	store := session.New(0, 0, 0, session.WithClock(func() time.Time { return now }))
	store.AddRecorder(write)
	for i := range 3 {
		at := exportBase.Add(time.Duration(i) * time.Hour)
		now = at
		store.Append("s1", model.SessionEvent{At: at, Direction: model.Outbound,
			Phase: model.SessionRequest, Host: "h", HTTPMethod: "POST"})
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
	var segs []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error { //nolint:errcheck // a missing file is the test's own failure to set up
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl.zst") {
			segs = append(segs, path)
		}
		return nil
	})
	if len(segs) == 0 {
		t.Fatal("fixture wrote no segment to make unreadable")
	}
	// A SELF-REFERENTIAL SYMLINK WHERE THE SEGMENT WAS, not chmod 0: root ignores the mode
	// bits, so a chmod fixture passes CI and fails for anyone running `go test` as root (a
	// devcontainer, an agent sandbox) with a 200 and the first record. ELOOP is refused for
	// every uid, root included, and it needs no chmod-back cleanup for t.TempDir to remove.
	//
	// A directory would NOT do: os.Open succeeds on one on macOS and the read fails later,
	// which readSegment reports as truncated rather than as an error — the tolerated case
	// above, and a 200. Only a path that cannot be OPENED reaches the handler's 500.
	for _, seg := range segs {
		if err := os.Remove(seg); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(seg, seg); err != nil {
			t.Fatal(err)
		}
	}

	ts := httptest.NewServer(New(":0", session.New(0, 0, 0), WithArchive(a),
		WithClock(func() time.Time { return exportNow })).server.Handler)
	t.Cleanup(ts.Close)
	code, body := getBody(t, ts.URL+"/v1/export")
	if code != http.StatusInternalServerError {
		t.Fatalf("GET /v1/export = %d %s, want 500 on an unreadable archive", code, body)
	}
	if !strings.Contains(string(body), "archive read failed") {
		t.Fatalf("body = %s, want search.go's archive-read-failed shape", body)
	}
}

// Without an archive the endpoint is a 404, the shape WithUsage and WithCatalog set: a
// client must not depend on it being there, and Kubernetes is where it is not.
func TestHandleExport_WithoutAnArchiveIs404(t *testing.T) {
	ts, _ := newTestServer(t)
	if code, body := getBody(t, ts.URL+"/v1/export"); code != http.StatusNotFound {
		t.Fatalf("GET /v1/export = %d %s, want 404 with no archive", code, body)
	}
}

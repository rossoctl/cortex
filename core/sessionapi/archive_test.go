package sessionapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// restarted builds what a proxy looks like after a restart: an archive holding s1's first ten
// events and old's three, written by a previous process, and a fresh store numbering from it.
func restarted(t *testing.T, opts ...Option) (*httptest.Server, *session.Store, *archive.Archive) {
	t.Helper()
	root := t.TempDir()
	prev, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prevStore := session.New(0, 0, 0)
	prevStore.AddRecorder(prev)
	for i := range 10 {
		prevStore.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Host: hostN(i)})
	}
	for range 3 {
		prevStore.Append("old", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	}
	prevStore.Close()
	if err := prev.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	srv := New(":0", store, append([]Option{WithArchive(a)}, opts...)...)
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
		a.Close()
	})
	return ts, store, a
}

func hostN(i int) string { return "h" + string(rune('a'+i)) }

func seqsOf(v *pipeline.SessionView) []uint64 {
	out := make([]uint64, len(v.Events))
	for i, e := range v.Events {
		out[i] = e.Seq
	}
	return out
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A session no longer in memory is served from disk, paged the same way a resident one is.
func TestHandleGet_ServesAnArchiveOnlySession(t *testing.T) {
	ts, _, _ := restarted(t)
	if v := getView(t, ts.URL, "/v1/sessions/s1"); !equalSeqs(seqsOf(v), []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) ||
		v.TotalEvents != 0 || v.OldestSeq != 0 {
		t.Fatalf("whole session: seqs %v total %d oldest %d", seqsOf(v), v.TotalEvents, v.OldestSeq)
	}
	v := getView(t, ts.URL, "/v1/sessions/s1?limit=4")
	if !equalSeqs(seqsOf(v), []uint64{7, 8, 9, 10}) || v.TotalEvents != 10 || v.OldestSeq != 1 {
		t.Fatalf("tail page: seqs %v total %d oldest %d", seqsOf(v), v.TotalEvents, v.OldestSeq)
	}
	if v := getView(t, ts.URL, "/v1/sessions/s1?limit=3&before=7"); !equalSeqs(seqsOf(v), []uint64{4, 5, 6}) {
		t.Fatalf("before=7: %v, want [4 5 6]", seqsOf(v))
	}
}

// The case the feature exists for: a session resumed after a restart is a few events in memory
// over a history on disk. One page spans both, oldest first, with no seq repeated.
func TestHandleGet_AResumedSessionPagesIntoItsHistory(t *testing.T) {
	ts, store, _ := restarted(t)
	for range 3 {
		store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	}
	v := getView(t, ts.URL, "/v1/sessions/s1?limit=5")
	if !equalSeqs(seqsOf(v), []uint64{9, 10, 11, 12, 13}) || v.OldestSeq != 1 || v.TotalEvents < 10 {
		t.Fatalf("seqs %v total %d oldest %d, want [9..13] from seq 1", seqsOf(v), v.TotalEvents, v.OldestSeq)
	}
	if v := getView(t, ts.URL, "/v1/sessions/s1?limit=3&before=11"); !equalSeqs(seqsOf(v), []uint64{8, 9, 10}) {
		t.Fatalf("before=11: %v, want [8 9 10] from disk", seqsOf(v))
	}
}

// The detail pane fetches one event by seq; one that is only on disk is still found, and one
// that exists nowhere is a 404, never a neighbour.
func TestHandleGetEvent_FindsAnEventOnDisk(t *testing.T) {
	ts, _, _ := restarted(t)
	code, body := getBody(t, ts.URL+"/v1/sessions/s1/events/3")
	var e pipeline.SessionEvent
	if code != http.StatusOK || json.Unmarshal(body, &e) != nil || e.Seq != 3 || e.SessionID != "s1" {
		t.Fatalf("GET events/3 = %d %s", code, body)
	}
	if code, _ := getBody(t, ts.URL+"/v1/sessions/s1/events/99"); code != http.StatusNotFound {
		t.Fatalf("GET events/99 = %d, want 404", code)
	}
}

type listBody struct {
	Sessions []session.SessionSummary `json:"sessions"`
	Archive  *pipeline.ArchiveUsage   `json:"archive"`
}

func getList(t *testing.T, url string) listBody {
	t.Helper()
	code, body := getBody(t, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", url, code)
	}
	var lb listBody
	if err := json.Unmarshal(body, &lb); err != nil {
		t.Fatal(err)
	}
	return lb
}

// The default list is unchanged — resident only, no archive object — so a client that does not
// ask sees exactly what it saw before.
func TestHandleList_ResidentOnlyByDefault(t *testing.T) {
	ts, store, _ := restarted(t)
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	lb := getList(t, ts.URL+"/v1/sessions")
	if len(lb.Sessions) != 1 || lb.Sessions[0].ID != "live" || lb.Archive != nil {
		t.Fatalf("default list = %+v", lb)
	}
}

// ?archived=true adds what is only on disk, marked resident: false, with the archive's figures
// and usage; a session in both appears once, as its resident row.
func TestHandleList_ArchivedTrueAddsDiskOnlyRows(t *testing.T) {
	ts, store, _ := restarted(t)
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	lb := getList(t, ts.URL+"/v1/sessions?archived=true")
	rows := map[string]session.SessionSummary{}
	for _, s := range lb.Sessions {
		if _, dup := rows[s.ID]; dup {
			t.Fatalf("%s listed twice", s.ID)
		}
		rows[s.ID] = s
	}
	if r, ok := rows["s1"]; !ok || r.Resident != nil {
		t.Fatalf("s1 should be its resident row: %+v", r)
	}
	old, ok := rows["old"]
	if !ok || old.Resident == nil || *old.Resident || old.EventCount != 3 {
		t.Fatalf("old should be a disk-only row with 3 events: %+v", old)
	}
	if lb.Archive == nil || lb.Archive.Bytes <= 0 || lb.Archive.MaxBytes <= 0 || lb.Archive.RetentionDays != 30 {
		t.Fatalf("archive usage = %+v", lb.Archive)
	}
}

// Without an archive the session API behaves exactly as before, ?archived=true included.
func TestHandleList_WithoutAnArchiveIgnoresArchived(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("live", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	lb := getList(t, ts.URL+"/v1/sessions?archived=true")
	if len(lb.Sessions) != 1 || lb.Archive != nil {
		t.Fatalf("list = %+v", lb)
	}
}

// pinned builds a session whose memory has a gap: max_events 5 keeps the inbound A2A intent
// (seq 1) pinned ahead of the newest four events, [1 18 19 20 21], while the archive recorded
// every event from seq from on. Closing the archive flushes it; the store keeps what it holds.
func pinned(t *testing.T, from uint64) *httptest.Server {
	t.Helper()
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 5, 0)
	intent := pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest,
		A2A: &pipeline.A2AExtension{Method: "message/send"}}
	for seq := uint64(1); seq <= 21; seq++ {
		if seq == from {
			store.AddRecorder(a)
		}
		e := pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest}
		if seq == 1 {
			e = intent
		}
		store.Append("s1", e)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if v := store.ViewTail("s1", 100); !equalSeqs(seqsOf(v), []uint64{1, 18, 19, 20, 21}) {
		t.Fatalf("fixture: memory holds %v, want the pinned intent ahead of a gap", seqsOf(v))
	}
	ts := httptest.NewServer(New(":0", store, WithArchive(a)).server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return ts
}

func seqRange(lo, hi uint64) []uint64 {
	var out []uint64
	for s := lo; s <= hi; s++ {
		out = append(out, s)
	}
	return out
}

// A page is the newest limit events below before across memory and disk together, each seq
// once — including where memory holds an event below a gap, as a pinned intent is. The whole-
// session marker is set only when nothing older is held on either side.
func TestHandleGet_FillsTheGapBehindAPinnedIntent(t *testing.T) {
	cases := []struct {
		path   string
		want   []uint64
		paged  bool // totalEvents/oldestSeq present: there is more than this response
		oldest uint64
	}{
		{"/v1/sessions/s1", seqRange(1, 21), false, 0},
		{"/v1/sessions/s1?limit=5", seqRange(17, 21), true, 1},
		{"/v1/sessions/s1?limit=3&before=20", seqRange(17, 19), true, 1},
		{"/v1/sessions/s1?limit=4&before=18", seqRange(14, 17), true, 1},
		{"/v1/sessions/s1?before=3", seqRange(1, 2), true, 1},
	}
	// from=2: the archive starts at seq 2, so seq 1 is held in memory only. totalEvents is at
	// least what either side holds: 21 on disk, or 20 when disk lacks the intent.
	for from, total := range map[uint64]int{1: 21, 2: 20} {
		ts := pinned(t, from)
		for _, c := range cases {
			v := getView(t, ts.URL, c.path)
			if !equalSeqs(seqsOf(v), c.want) {
				t.Errorf("from=%d %s: seqs %v, want %v", from, c.path, seqsOf(v), c.want)
			}
			if paged := v.TotalEvents != 0 || v.OldestSeq != 0; paged != c.paged || (c.paged && (v.OldestSeq != c.oldest || v.TotalEvents < total)) {
				t.Errorf("from=%d %s: total %d oldest %d, want paged=%v from seq %d", from, c.path, v.TotalEvents, v.OldestSeq, c.paged, c.oldest)
			}
		}
	}
}

// A session resumed after a restart lists its whole history — 10 events from before and 1 after —
// in the default list and with ?archived=true alike. The archive marks the entry's start on its
// own goroutine, so poll briefly for it.
func TestHandleList_AResumedSessionListsItsWholeHistory(t *testing.T) {
	ts, store, _ := restarted(t)
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	for _, url := range []string{ts.URL + "/v1/sessions", ts.URL + "/v1/sessions?archived=true"} {
		deadline := time.Now().Add(2 * time.Second)
		for {
			var n int
			for _, s := range getList(t, url).Sessions {
				if s.ID == "s1" {
					n = s.EventCount
				}
			}
			if n == 11 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: s1 lists %d events, want 11", url, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// ?raw=1 adds the event's exact capture from the archive: the request as the agent sent it and
// as it was forwarded, with its headers and query. Without the parameter, or for an event with
// no capture, the answer is the event alone.
func TestHandleGetEvent_RawAddsTheCapture(t *testing.T) {
	root := t.TempDir()
	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	want := &pipeline.RequestCapture{Original: []byte(`{"model":"m"}`), Forwarded: []byte(`{"model":"n"}`),
		Header: http.Header{"Anthropic-Beta": {"x"}}, Query: "beta=true"}
	store.AppendCapture("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest}, want)
	gz := []byte("\x1f\x8b\x08\x00\xff")
	store.AppendCapture("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse},
		&pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200,
			Header: http.Header{"Content-Encoding": {"gzip"}}, Body: gz, Incomplete: true}})
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest})
	if err := a.Close(); err != nil { // flushes, so the capture is readable
		t.Fatal(err)
	}
	if a, err = archive.Open(root); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := New(":0", store, WithArchive(a))
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	var got struct {
		Seq uint64               `json:"seq"`
		Raw *pipeline.RawCapture `json:"raw"`
	}
	code, body := getBody(t, ts.URL+"/v1/sessions/s1/events/1?raw=1")
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Seq != 1 || got.Raw == nil {
		t.Fatalf("GET events/1?raw=1 = %d %s", code, body)
	}
	if got.Raw.Request != `{"model":"m"}` || got.Raw.Forwarded == nil || *got.Raw.Forwarded != `{"model":"n"}` ||
		got.Raw.RequestHeaders.Get("Anthropic-Beta") != "x" || got.Raw.Query != "beta=true" {
		t.Fatalf("raw = %+v, want the capture", got.Raw)
	}
	// A response whose body is not UTF-8, as every compressed body is, comes back as base64.
	got.Raw = nil
	code, body = getBody(t, ts.URL+"/v1/sessions/s1/events/2?raw=1")
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Raw == nil || got.Raw.Response == nil {
		t.Fatalf("GET events/2?raw=1 = %d %s", code, body)
	}
	if r := got.Raw.Response; r.Status != 200 || r.Headers.Get("Content-Encoding") != "gzip" || r.Body != "" ||
		!bytes.Equal(r.BodyBase64, gz) || !r.Incomplete || r.Truncated || r.Decoded || got.Raw.Request != "" {
		t.Fatalf("raw = %+v, response %+v, want the gzip response as base64", got.Raw, r)
	}
	for _, path := range []string{"/v1/sessions/s1/events/1", "/v1/sessions/s1/events/3?raw=1"} {
		code, body := getBody(t, ts.URL+path)
		if code != http.StatusOK || strings.Contains(string(body), `"raw"`) {
			t.Fatalf("GET %s = %d %s, want the event without raw", path, code, body)
		}
	}
}

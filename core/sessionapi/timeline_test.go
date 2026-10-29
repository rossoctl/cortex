package sessionapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/session/timeline"
)

func getTimeline(t *testing.T, base, id, query string) (*timeline.Timeline, int) {
	t.Helper()
	resp, err := http.Get(base + "/v1/sessions/" + url.PathEscape(id) + "/timeline" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	var tl timeline.Timeline
	if err := json.NewDecoder(resp.Body).Decode(&tl); err != nil {
		t.Fatal(err)
	}
	return &tl, resp.StatusCode
}

func appendTurn(store *session.Store, id string, at time.Time, rid, toolID string) {
	store.Append(id, pipeline.SessionEvent{At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, RequestID: rid, Host: "llm.example",
		Inference: &pipeline.InferenceExtension{AgentRole: pipeline.AgentRoleMain, Tools: []pipeline.InferenceTool{{Name: "Bash"}}}})
	store.Append(id, pipeline.SessionEvent{At: at.Add(time.Second), Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, RequestID: rid, Host: "llm.example",
		StatusCode: 200, Duration: time.Second,
		Inference: &pipeline.InferenceExtension{FinishReason: "tool_use", ToolCalls: []pipeline.InferenceToolCall{{ID: toolID, Name: "Bash"}}}})
}

func TestTimelineServesSpansAndClips(t *testing.T) {
	ts, store := newTestServer(t)
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	appendTurn(store, "s1", base, "r1", "t1")
	appendTurn(store, "s1", base.Add(time.Hour), "r2", "t2")

	tl, code := getTimeline(t, ts.URL, "s1", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(tl.Turns) != 2 || len(tl.Tools) != 2 || len(tl.Hosts) != 2 || tl.LastSeq != 4 || tl.Truncated {
		t.Errorf("timeline = %+v", tl)
	}

	from := url.QueryEscape(base.Add(30 * time.Minute).Format(time.RFC3339))
	tl, _ = getTimeline(t, ts.URL, "s1", "?from="+from)
	// t1 was never answered but its agent moved on, so it is lost and ends where it started;
	// t2 is still in flight, which overlaps every later window.
	if len(tl.Turns) != 1 || len(tl.Tools) != 1 || tl.Tools[0].ID != "t2" || len(tl.Agents) != 1 {
		t.Errorf("clipped = %+v, want only the second turn and tool, and the agent kept", tl)
	}
}

func TestTimelineUnknownSessionAndBadTime(t *testing.T) {
	ts, store := newTestServer(t)
	if _, code := getTimeline(t, ts.URL, "nope", ""); code != http.StatusNotFound {
		t.Errorf("unknown session: status %d, want 404", code)
	}
	appendTurn(store, "s1", time.Now(), "r1", "t1")
	if _, code := getTimeline(t, ts.URL, "s1", "?to=yesterday"); code != http.StatusBadRequest {
		t.Errorf("bad time: status %d, want 400", code)
	}
}

// After a restart the timeline covers the whole session: the archive's history behind the
// few events the new process holds.
func TestTimelineReadsTheArchiveBehindMemory(t *testing.T) {
	ts, store, _ := restarted(t)
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, RequestID: "new", Host: "after.example"})

	tl, code := getTimeline(t, ts.URL, "s1", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if tl.LastSeq != 11 {
		t.Errorf("lastSeq = %d, want 11 (10 archived + 1 resident)", tl.LastSeq)
	}
	// The archived events in restarted carry no direction, so only the resident one is a host.
	if len(tl.Hosts) != 1 || tl.Hosts[0].Host != "after.example" {
		t.Errorf("hosts = %+v", tl.Hosts)
	}
	// A session only the archive holds is served, not 404'd.
	if _, code := getTimeline(t, ts.URL, "old", ""); code != http.StatusOK {
		t.Errorf("archived-only session: status %d, want 200", code)
	}
}

// A cached timeline is served until the session gains an event, and a caller's clip does
// not reach the cached copy.
func TestTimelineCacheFollowsTheNewestEvent(t *testing.T) {
	ts, store := newTestServer(t)
	base := time.Now().Add(-time.Hour)
	appendTurn(store, "s1", base, "r1", "t1")
	appendTurn(store, "s1", base.Add(30*time.Minute), "r2", "t2")

	from := url.QueryEscape(base.Add(20 * time.Minute).Format(time.RFC3339))
	getTimeline(t, ts.URL, "s1", "") // fill the cache, so the clipped read below is a hit
	if tl, _ := getTimeline(t, ts.URL, "s1", "?from="+from); len(tl.Turns) != 1 {
		t.Fatalf("clipped turns = %d, want 1", len(tl.Turns))
	}
	if tl, _ := getTimeline(t, ts.URL, "s1", ""); len(tl.Turns) != 2 {
		t.Errorf("after a clipped read, unclipped turns = %d, want 2: the clip reached the cache", len(tl.Turns))
	}
	appendTurn(store, "s1", base.Add(40*time.Minute), "r3", "t3")
	if tl, _ := getTimeline(t, ts.URL, "s1", ""); len(tl.Turns) != 3 || tl.LastSeq != 6 {
		t.Errorf("after a new event: turns = %d lastSeq = %d, want 3 and 6", len(tl.Turns), tl.LastSeq)
	}
}

// A session past the read bound says so.
func TestTimelineSaysWhenItIsTruncated(t *testing.T) {
	store := session.New(0, 0, 0) // uncapped: newTestServer's store holds only 100 events
	ts := httptest.NewServer(New(":0", store).server.Handler)
	t.Cleanup(func() { ts.Close(); store.Close() })
	at := time.Now()
	for i := 0; i < timelineMaxEvents+5; i++ {
		store.Append("big", pipeline.SessionEvent{At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h.example"})
	}
	store.Append("small", pipeline.SessionEvent{At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h.example"})
	if tl, _ := getTimeline(t, ts.URL, "big", ""); !tl.Truncated || tl.LastSeq != timelineMaxEvents+5 {
		t.Errorf("big: truncated = %v lastSeq = %d", tl.Truncated, tl.LastSeq)
	}
	if tl, _ := getTimeline(t, ts.URL, "small", ""); tl.Truncated {
		t.Errorf("small session reported truncated")
	}
}

// A session with nothing to return encodes its empty lists as [], served fresh and from the
// cache alike. Found on a real session: the cache's copy turned an empty list into null.
func TestTimelineListsAreNeverNull(t *testing.T) {
	ts, store := newTestServer(t)
	appendTurn(store, "s1", time.Now(), "r1", "t1")
	for _, pass := range []string{"fresh", "cached"} {
		resp, err := http.Get(ts.URL + "/v1/sessions/s1/timeline")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(b), "null") {
			t.Errorf("%s: %s", pass, b)
		}
	}
}

// An erased session's timeline is not served to the session that next takes its id, and the
// cache holds nothing of it afterwards.
func TestTimelineIsDroppedWhenItsSessionIsErased(t *testing.T) {
	for _, tc := range []struct {
		name  string
		erase func(t *testing.T, base string)
	}{
		{"one session", func(t *testing.T, base string) {
			if code, b := doDelete(t, base, "s1"); code != http.StatusOK {
				t.Fatalf("delete: %d %s", code, b)
			}
		}},
		{"every session", func(t *testing.T, base string) {
			if code, b := doClear(t, base, nil); code != http.StatusOK {
				t.Fatalf("clear: %d %s", code, b)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := session.New(0, 0, 0)
			srv := New(":0", store, WithClearAllowed(true))
			ts := httptest.NewServer(srv.server.Handler)
			t.Cleanup(func() { ts.Close(); store.Close() })
			at := time.Now()
			appendTurn(store, "s1", at, "r1", "erased")
			getTimeline(t, ts.URL, "s1", "")

			tc.erase(t, ts.URL)
			srv.timelines.mu.Lock()
			held := len(srv.timelines.entries)
			srv.timelines.mu.Unlock()
			if held != 0 {
				t.Errorf("cache holds %d timelines after the erase", held)
			}
			appendTurn(store, "s1", at, "r1", "fresh")
			if tl, _ := getTimeline(t, ts.URL, "s1", ""); len(tl.Tools) != 1 || tl.Tools[0].ID != "fresh" {
				t.Errorf("tools = %+v, want only the new session's", tl.Tools)
			}
		})
	}
}

// A session the store dropped without the API being told (ttl, max_sessions) restarts its
// numbering when it comes back. Reaching the cached timeline's seq again does not serve it.
func TestTimelineIsNotServedToTheNextSessionUnderItsID(t *testing.T) {
	ts, store := newTestServer(t)
	at := time.Now()
	appendTurn(store, "s1", at, "r1", "old")
	getTimeline(t, ts.URL, "s1", "")

	store.Delete("s1")
	appendTurn(store, "s1", at.Add(time.Second), "r1", "new")
	if tl, _ := getTimeline(t, ts.URL, "s1", ""); len(tl.Tools) != 1 || tl.Tools[0].ID != "new" {
		t.Errorf("tools = %+v, want the new session's", tl.Tools)
	}
}

// A session resumed after a restart reads its history from disk once: the events below what
// memory holds do not change, so later reads serve them from memory. Removing the files under
// the archive shows which: a read that went back to disk would lose the history.
func TestTimelineReadsAResumedSessionsHistoryOnce(t *testing.T) {
	root := t.TempDir()
	prev, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prevStore := session.New(0, 0, 0)
	prevStore.AddRecorder(prev)
	at := time.Now().Add(-time.Hour)
	for i := range 3 {
		prevStore.Append("s1", pipeline.SessionEvent{At: at.Add(time.Duration(i) * time.Second), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "before.example"})
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
	ts := httptest.NewServer(New(":0", store, WithArchive(a)).server.Handler)
	t.Cleanup(func() { ts.Close(); store.Close(); a.Close() })

	before := func(tl *timeline.Timeline) int {
		n := 0
		for _, h := range tl.Hosts {
			if h.Host == "before.example" {
				n++
			}
		}
		return n
	}
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "after.example"})
	if tl, _ := getTimeline(t, ts.URL, "s1", ""); before(tl) != 3 {
		t.Fatalf("first read: hosts = %+v, want the 3 archived requests", tl.Hosts)
	}

	segs, err := filepath.Glob(filepath.Join(root, "*", "*", "*.jsonl.zst"))
	if err != nil || len(segs) == 0 {
		t.Fatalf("no segments under %s: %v", root, err)
	}
	for _, f := range segs {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "after.example"})
	tl, _ := getTimeline(t, ts.URL, "s1", "")
	if tl.LastSeq != 5 || before(tl) != 3 {
		t.Errorf("after a new event: lastSeq = %d, hosts = %+v, want 5 and the 3 archived requests served from memory", tl.LastSeq, tl.Hosts)
	}
}

// Retention deleting a resumed session's oldest segment reaches the timeline, though its
// history is held in memory.
func TestTimelineFollowsRetentionPruningAResumedSession(t *testing.T) {
	root := t.TempDir()
	var clock atomic.Int64
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	record := func(at time.Time, host string) {
		clock.Store(at.UnixNano())
		a, err := archive.Open(root, archive.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		st := session.New(0, 0, 0)
		st.AddRecorder(a)
		st.Append("s1", pipeline.SessionEvent{At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: host})
		st.Close()
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Inside the default 30 days when the server starts, outside them two days later.
	record(time.Now().Add(-29*24*time.Hour), "old.example")
	record(time.Now().Add(-time.Hour), "recent.example")

	clock.Store(time.Now().UnixNano())
	a, err := archive.Open(root, archive.WithClock(now))
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	ts := httptest.NewServer(New(":0", store, WithArchive(a)).server.Handler)
	t.Cleanup(func() { ts.Close(); store.Close(); a.Close() })
	hosts := func() []string {
		tl, _ := getTimeline(t, ts.URL, "s1", "")
		var hs []string
		for _, h := range tl.Hosts {
			hs = append(hs, h.Host)
		}
		return hs
	}
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "after.example"})
	if hs := hosts(); len(hs) != 3 || hs[0] != "old.example" {
		t.Fatalf("before the prune: hosts = %v, want old, recent and after", hs)
	}

	clock.Add(int64(2 * 24 * time.Hour))
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, _, _ := a.Page("s1", 0, 0, func(*pipeline.SessionEvent) bool { return false })
		if info.OldestSeq > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retention did not prune the old segment")
		}
		time.Sleep(20 * time.Millisecond)
	}
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "after.example"})
	if hs := hosts(); slices.Contains(hs, "old.example") {
		t.Errorf("after the prune: hosts = %v, want the pruned request gone", hs)
	}
}

// A resumed session's history is held up to timelinePrefixEvents across sessions.
func TestTimelineHeldHistoryIsBounded(t *testing.T) {
	var c timelineCache
	held := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := 0
		for _, p := range c.prefixes {
			n += len(p.events)
		}
		return n
	}
	c.putPrefix("a", 0, &timelinePrefix{events: make([]pipeline.SessionEvent, timelinePrefixEvents-10)})
	c.putPrefix("b", 0, &timelinePrefix{events: make([]pipeline.SessionEvent, 20)})
	c.mu.Lock()
	_, a := c.prefixes["a"]
	_, b := c.prefixes["b"]
	c.mu.Unlock()
	if a || !b || held() != 20 {
		t.Errorf("held a=%v b=%v events=%d, want the least recently used history dropped", a, b, held())
	}
	c.putPrefix("a", 1, &timelinePrefix{events: make([]pipeline.SessionEvent, 5)})
	if held() != 20 {
		t.Errorf("a history read before an erase was kept")
	}
}

// A history replaced or dropped from the cache is not kept alive by it.
func TestTimelineDroppedHistoryIsCollectable(t *testing.T) {
	var c timelineCache
	collected := make(chan struct{})
	p := &timelinePrefix{events: make([]pipeline.SessionEvent, 1), readAt: time.Now()}
	runtime.AddCleanup(p, func(ch chan struct{}) { close(ch) }, collected)
	c.putPrefix("a", 0, p)
	c.putPrefix("a", 0, &timelinePrefix{events: make([]pipeline.SessionEvent, 1), readAt: time.Now()})
	p = nil
	for range 20 {
		runtime.GC()
		select {
		case <-collected:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Error("a replaced history is still reachable")
}

// A held history goes timelinePrefixTTL after it was read, though nothing else is stored.
func TestTimelineIdleHistoryExpires(t *testing.T) {
	defer func(d time.Duration) { timelinePrefixTTL = d }(timelinePrefixTTL)
	timelinePrefixTTL = 10 * time.Millisecond
	var c timelineCache
	c.putPrefix("a", 0, &timelinePrefix{events: make([]pipeline.SessionEvent, 1), readAt: time.Now()})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.prefixes)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("an idle history was kept past its expiry")
}

// A cached timeline outlives the old one-minute bound while its session records nothing, and an
// unanswered request still ages out on it as it would on a fresh compute.
func TestTimelineCacheAgesUnansweredRequestsOnAHit(t *testing.T) {
	var clock atomic.Int64
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	clock.Store(base.UnixNano())
	store := session.New(0, 0, 0)
	srv := New(":0", store, WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() }))
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() { ts.Close(); store.Close() })
	store.Append("s1", pipeline.SessionEvent{At: base, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, RequestID: "h", Host: "slow.example"})

	if tl, _ := getTimeline(t, ts.URL, "s1", ""); tl.Hosts[0].Lost {
		t.Fatal("lost at once; want in flight")
	}
	cached := srv.timelines.entries["s1"].tl
	clock.Add(int64(time.Hour))
	tl, _ := getTimeline(t, ts.URL, "s1", "")
	if srv.timelines.entries["s1"].tl != cached {
		t.Error("an hour on with no new event, the timeline was rebuilt rather than served from the cache")
	}
	if !tl.Hosts[0].Lost {
		t.Error("an hour on, the unanswered request is still in flight; want it aged out on the hit")
	}
	if cached.Hosts[0].Lost {
		t.Error("aging the served copy reached the cached timeline")
	}
}

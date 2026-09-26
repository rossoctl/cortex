package sessionapi

import (
	"fmt"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

func viewHosts(v *pipeline.SessionView) []string {
	out := make([]string, 0, len(v.Events))
	for i := range v.Events {
		out = append(out, v.Events[i].Host)
	}
	return out
}

// ?before=<seq> returns the page ending just before that event, which is what a client
// scrolling backward through a timeline asks for.
func TestHandleGet_BeforePagesBackward(t *testing.T) {
	ts, store := newUncappedServer(t)
	seed(t, store, "s1", 100)

	tail := getView(t, ts.URL, "/v1/sessions/s1?limit=10")
	if got, want := viewHosts(tail)[0], "h0090"; got != want {
		t.Fatalf("tail starts at %q, want %q", got, want)
	}

	older := getView(t, ts.URL,
		fmt.Sprintf("/v1/sessions/s1?limit=10&before=%d", tail.Events[0].Seq))
	if got, want := viewHosts(older)[9], "h0089"; got != want {
		t.Errorf("page ends at %q, want %q — it must stop before the cursor", got, want)
	}
	if got, want := viewHosts(older)[0], "h0080"; got != want {
		t.Errorf("page starts at %q, want %q", got, want)
	}
}

// The defect this endpoint had: a session longer than maxEventLimit had events NO request
// could reach, at any limit, because the only window was the most recent one. Paging
// reaches them, and this walks all the way to the session's first event to prove it.
func TestHandleGet_BeforeReachesEventsPastTheResponseCap(t *testing.T) {
	const total = maxEventLimit + 500
	ts, store := newUncappedServer(t)
	seed(t, store, "s1", total)

	// The largest single response the endpoint will produce still cannot see the start.
	widest := getView(t, ts.URL, fmt.Sprintf("/v1/sessions/s1?limit=%d", maxEventLimit+1000))
	if len(widest.Events) != maxEventLimit {
		t.Fatalf("widest response carried %d events, want the %d cap", len(widest.Events), maxEventLimit)
	}
	if viewHosts(widest)[0] == "h0000" {
		t.Fatal("test premise broken: the capped response already reaches the first event")
	}
	if widest.OldestSeq == 0 {
		t.Fatal("OldestSeq unset on a capped response; a client cannot tell more exists")
	}

	// Page backward until the oldest held event is in hand.
	cursor := widest.Events[0].Seq
	var first *pipeline.SessionView
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not reach the start of the session")
		}
		page := getView(t, ts.URL, fmt.Sprintf("/v1/sessions/s1?limit=%d&before=%d", maxEventLimit, cursor))
		if len(page.Events) == 0 {
			t.Fatal("empty page before reaching the oldest event")
		}
		first = page
		if page.Events[0].Seq == page.OldestSeq || page.OldestSeq == 0 {
			break
		}
		cursor = page.Events[0].Seq
	}

	if got := viewHosts(first)[0]; got != "h0000" {
		t.Errorf("oldest reachable event = %q, want h0000", got)
	}
}

// An unparseable cursor behaves like no cursor, matching ?limit's forgiveness: this is a
// debugging surface, and refusing to answer "before=abc" helps nobody who typed it.
func TestHandleGet_BeforeGarbageServesTheTail(t *testing.T) {
	ts, store := newUncappedServer(t)
	seed(t, store, "s1", 50)

	for _, raw := range []string{"abc", "", "-5", "9.5"} {
		v := getView(t, ts.URL, fmt.Sprintf("/v1/sessions/s1?limit=5&before=%s", raw))
		if got, want := viewHosts(v)[4], "h0049"; got != want {
			t.Errorf("before=%q ended at %q, want the tail ending %q", raw, got, want)
		}
	}
}

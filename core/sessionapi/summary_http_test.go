package sessionapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// recordFull puts n events carrying real payloads into the store.
func recordFull(store *session.Store, id string, n int) {
	for i := 0; i < n; i++ {
		e := fullEvent()
		e.SessionID = id
		store.Append(id, e)
	}
}

func getBody(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// ?view=summary is what makes opening a session fast. Asserted on the wire, not
// just on the projection, because the handler has to actually apply it.
func TestHandleGet_ViewSummaryDropsPayloads(t *testing.T) {
	ts, store := newTestServer(t)
	recordFull(store, "s1", 5)

	statusFull, full := getBody(t, ts.URL+"/v1/sessions/s1")
	statusSumm, summ := getBody(t, ts.URL+"/v1/sessions/s1?view=summary")
	if statusFull != http.StatusOK || statusSumm != http.StatusOK {
		t.Fatalf("status full=%d summary=%d", statusFull, statusSumm)
	}

	if len(summ) >= len(full) {
		t.Errorf("summary (%d bytes) is not smaller than full (%d bytes)", len(summ), len(full))
	}
	t.Logf("full=%d summary=%d ratio=%.1fx", len(full), len(summ), float64(len(full))/float64(len(summ)))

	// Only what the timeline never reads. completion/parts/plugins are deliberately
	// NOT here — the events filter searches them, so dropping them silently broke
	// `/text` and `plugin:name`. See summarizeEvent's rule.
	for _, gone := range []string{`"messages"`, `"tools"`, `"toolCalls"`, `"artifact"`, `"params"`, `"result"`} {
		if strings.Contains(string(summ), gone) {
			t.Errorf("%s still present in the summary response", gone)
		}
	}
	// And the timeline fields must remain.
	for _, kept := range []string{`"host"`, `"statusCode"`, `"totalTokens"`, `"invocations"`, `"isAction"`, `"model"`,
		// The filter's fields, on the wire.
		`"completion"`, `"parts"`, `"plugins"`} {
		if !strings.Contains(string(summ), kept) {
			t.Errorf("%s missing from the summary response — the timeline renders it", kept)
		}
	}
	// Same number of events either way: this is a projection, not a filter.
	var fv, sv pipeline.SessionView
	if err := json.Unmarshal(full, &fv); err != nil {
		t.Fatalf("decode full: %v", err)
	}
	if err := json.Unmarshal(summ, &sv); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if len(fv.Events) != len(sv.Events) {
		t.Errorf("summary returned %d events, full returned %d", len(sv.Events), len(fv.Events))
	}
}

// An old client sends no view param, and a client from the future may send one
// this server does not know. Both must get the full events rather than an error
// or a silently emptied timeline — the version-skew path, since abctl and the
// proxy install separately.
func TestHandleGet_UnknownOrAbsentViewReturnsFullEvents(t *testing.T) {
	ts, store := newTestServer(t)
	recordFull(store, "s1", 2)

	for _, q := range []string{"", "?view=", "?view=full", "?view=wat", "?view=SUMMARY"} {
		t.Run("q="+q, func(t *testing.T) {
			status, body := getBody(t, ts.URL+"/v1/sessions/s1"+q)
			if status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			if !strings.Contains(string(body), `"messages"`) {
				t.Errorf("payloads missing for %q — only an exact `view=summary` should project", q)
			}
		})
	}
}

// The detail pane needs the payloads for one row, which is the other half of the
// bargain: the timeline stops carrying them, so a single event has to be fetchable.
func TestHandleGetEvent_ReturnsTheFullEvent(t *testing.T) {
	ts, store := newTestServer(t)
	recordFull(store, "s1", 3)

	// Discover the seqs the store assigned.
	_, listing := getBody(t, ts.URL+"/v1/sessions/s1?view=summary")
	var view pipeline.SessionView
	if err := json.Unmarshal(listing, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Events) != 3 {
		t.Fatalf("setup: %d events", len(view.Events))
	}
	want := view.Events[1]

	status, body := getBody(t, fmt.Sprintf("%s/v1/sessions/s1/events/%d", ts.URL, want.Seq))
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var got pipeline.SessionEvent
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if got.Seq != want.Seq {
		t.Errorf("seq = %d, want %d", got.Seq, want.Seq)
	}
	if got.Inference == nil || len(got.Inference.Messages) == 0 {
		t.Error("the full event came back without its messages — the detail pane has nothing to show")
	}
	if got.Inference.Completion == "" {
		t.Error("completion missing from the full event")
	}
	if len(got.Plugins) == 0 {
		t.Error("plugins missing from the full event")
	}
}

func TestHandleGetEvent_Errors(t *testing.T) {
	ts, store := newTestServer(t)
	recordFull(store, "s1", 2)

	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"unknown session", "/v1/sessions/nope/events/1", http.StatusNotFound},
		// A seq the store never issued, and one it has evicted, are the same
		// answer: the event is not here. Returning a NEIGHBOUR would be worse
		// than 404 — the detail pane would confidently show the wrong event.
		{"seq beyond the end", "/v1/sessions/s1/events/99999", http.StatusNotFound},
		{"seq zero", "/v1/sessions/s1/events/0", http.StatusNotFound},
		{"non-numeric seq", "/v1/sessions/s1/events/abc", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _ := getBody(t, ts.URL+tc.path)
			if status != tc.want {
				t.Errorf("status = %d, want %d", status, tc.want)
			}
		})
	}
}

// The index is how someone discovers the API; a route absent from it is a route
// nobody finds.
func TestHandleIndex_ListsThePerEventRoute(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := getBody(t, ts.URL+"/")
	if !strings.Contains(string(body), "/v1/sessions/{id}/events/{seq}") {
		t.Errorf("index does not mention the per-event route:\n%s", body)
	}
	if !strings.Contains(string(body), "view=summary") {
		t.Errorf("index does not mention view=summary:\n%s", body)
	}
}

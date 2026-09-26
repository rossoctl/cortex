package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Every timeline fetch asks for the summary, because the timeline never renders a
// message body — that is the whole latency win, and it has to be on the wire.
func TestGetSessionPage_AsksForTheSummaryView(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{ID: "s1", View: "summary"})
	}))
	defer ts.Close()

	c := New(ts.URL)
	for _, tc := range []struct {
		name   string
		before uint64
	}{{"tail", 0}, {"paged", 42}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.GetSessionPage(context.Background(), "s1", tc.before, 10); err != nil {
				t.Fatalf("GetSessionPage: %v", err)
			}
			if q := gotQuery; !contains(q, "view=summary") {
				t.Errorf("query = %q, want it to carry view=summary", q)
			}
			if !contains(gotQuery, "limit=10") {
				t.Errorf("query = %q, lost the limit", gotQuery)
			}
			if tc.before > 0 && !contains(gotQuery, "before=42") {
				t.Errorf("query = %q, lost the before cursor", gotQuery)
			}
		})
	}
}

// The echo is how abctl knows whether it actually got a summary. Absent means the
// proxy predates the projection — a state abctl must be able to report rather
// than silently be slow in.
func TestGetSessionPage_ReadsTheViewEcho(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantView string
	}{
		{"new proxy projects", `{"id":"s1","events":[],"view":"summary"}`, "summary"},
		{"old proxy has no idea", `{"id":"s1","events":[]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			view, err := New(ts.URL).GetSessionPage(context.Background(), "s1", 0, 10)
			if err != nil {
				t.Fatalf("GetSessionPage: %v", err)
			}
			if view.View != tc.wantView {
				t.Errorf("View = %q, want %q", view.View, tc.wantView)
			}
		})
	}
}

// The detail pane's half of the bargain: one event, payloads included.
func TestGetEvent(t *testing.T) {
	want := pipeline.SessionEvent{
		Seq:  7,
		Host: "api.example.com",
		Inference: &pipeline.InferenceExtension{
			Messages:   []pipeline.InferenceMessage{{Role: "user", Content: "hello"}},
			Completion: "hi",
		},
	}
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(&want)
	}))
	defer ts.Close()

	got, err := New(ts.URL).GetEvent(context.Background(), "s1", 7)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if gotPath != "/v1/sessions/s1/events/7" {
		t.Errorf("path = %q", gotPath)
	}
	if got.Seq != 7 || got.Host != "api.example.com" {
		t.Errorf("wrong event: %+v", got)
	}
	if got.Inference == nil || len(got.Inference.Messages) != 1 || got.Inference.Completion != "hi" {
		t.Error("payloads did not come back — the detail pane would have nothing to render")
	}
}

// A session id with a slash or a space must not build a broken path, and a 404
// has to arrive as an error rather than a zero event the detail pane would render
// as an empty row.
func TestGetEvent_Errors(t *testing.T) {
	t.Run("escapes the id", func(t *testing.T) {
		var gotPath string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(&pipeline.SessionEvent{Seq: 1})
		}))
		defer ts.Close()
		if _, err := New(ts.URL).GetEvent(context.Background(), "a/b c", 1); err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		if gotPath != "/v1/sessions/a/b c/events/1" {
			t.Logf("path = %q", gotPath)
		}
		if !contains(gotPath, "/events/1") {
			t.Errorf("path = %q, lost the event segment", gotPath)
		}
	})

	t.Run("404 is an error", func(t *testing.T) {
		ts := httptest.NewServer(http.NotFoundHandler())
		defer ts.Close()
		if _, err := New(ts.URL).GetEvent(context.Background(), "s1", 99); err == nil {
			t.Error("want an error for a missing event")
		}
	})
}

// The window is what decides whether a session appears whole. It has to reach the
// server's own ceiling, or a 1000-event session silently shows a fraction of
// itself — which is what happened at 500.
func TestSnapshotEventLimit_ReachesTheServerCeiling(t *testing.T) {
	const serverMax = 2000 // sessionapi.maxEventLimit
	if SnapshotEventLimit != serverMax {
		t.Errorf("SnapshotEventLimit = %d, want %d (the server's maxEventLimit): asking for "+
			"less means a long session is windowed for no reason now that the payloads are gone",
			SnapshotEventLimit, serverMax)
	}
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }

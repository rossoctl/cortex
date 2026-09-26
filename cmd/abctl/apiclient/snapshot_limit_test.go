package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The limit has to be ON THE REQUEST, not merely assumed from the server's default.
//
// If this client stopped sending it, the server's default would keep the response small
// today and the bug would return the day that default changed — with the same symptom as
// before: a request too big to finish inside the 10s client timeout, and an empty
// timeline with nothing saying why.
func TestGetSession_SendsTheLimit(t *testing.T) {
	var gotLimit string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{ID: "s1"})
	}))
	defer ts.Close()

	if _, err := New(ts.URL).GetSession(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if gotLimit != strconv.Itoa(SnapshotEventLimit) {
		t.Errorf("limit sent = %q, want %q", gotLimit, strconv.Itoa(SnapshotEventLimit))
	}
}

func TestGetSessionTail_SendsTheGivenLimit(t *testing.T) {
	var gotLimit string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{ID: "s1"})
	}))
	defer ts.Close()

	if _, err := New(ts.URL).GetSessionTail(context.Background(), "s1", 7); err != nil {
		t.Fatal(err)
	}
	if gotLimit != "7" {
		t.Errorf("limit sent = %q, want %q", gotLimit, "7")
	}
}

// A session id with characters that need escaping must still escape, now that the path
// is built by formatting a query string onto it.
//
// Asserted on EscapedPath, not Path: the decoded path is "/v1/sessions/a/b c" whether or
// not the id was escaped, because the slash stays a slash and the space is escaped by the
// transport regardless — so the obvious assertion holds with url.PathEscape removed and
// proves nothing.
func TestGetSessionTail_EscapesTheID(t *testing.T) {
	var escaped string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped = r.URL.EscapedPath()
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{ID: "x"})
	}))
	defer ts.Close()

	if _, err := New(ts.URL).GetSessionTail(context.Background(), "a/b c", 5); err != nil {
		t.Fatal(err)
	}
	if want := "/v1/sessions/a%2Fb%20c"; escaped != want {
		t.Errorf("escaped path = %q, want %q", escaped, want)
	}
}

// TotalEvents survives the round trip, because the count of what was left out is the
// only way the pane can say so.
func TestGetSession_CarriesTotalEvents(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pipeline.SessionView{
			ID:          "s1",
			Events:      []pipeline.SessionEvent{{Host: "h"}},
			TotalEvents: 4200,
		})
	}))
	defer ts.Close()

	got, err := New(ts.URL).GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalEvents != 4200 {
		t.Errorf("TotalEvents = %d, want 4200", got.TotalEvents)
	}
}

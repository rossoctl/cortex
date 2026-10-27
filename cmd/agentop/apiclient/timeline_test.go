package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rossoctl/cortex/core/session/timeline"
)

func TestGetTimeline(t *testing.T) {
	var gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(timeline.Timeline{ID: "s/1", LastSeq: 9,
			Tools: []timeline.Tool{{Agent: "main", ID: "t1", Name: "Bash"}}})
	}))
	defer ts.Close()

	tl, err := New(ts.URL).GetTimeline(context.Background(), "s/1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/sessions/s%2F1/timeline" {
		t.Errorf("path = %q, want the id escaped", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want none", gotQuery)
	}
	if tl.LastSeq != 9 || len(tl.Tools) != 1 || tl.Tools[0].Name != "Bash" {
		t.Errorf("timeline = %+v", tl)
	}
}

func TestGetTimeline_OldProxyIsNotFound(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	if _, err := New(ts.URL).GetTimeline(context.Background(), "s1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

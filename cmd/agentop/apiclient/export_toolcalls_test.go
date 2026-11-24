package apiclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestExport_IncludeToolCallsIsSentOnlyWhenAsked pins the query the client builds, because the
// opt-in's whole value is that it is OFF by default: a client that sent `include=toolCalls`
// unconditionally would change what every existing `agentop export` returns, which is the one
// thing T20 forbids.
//
// Asserted on the raw query the server received rather than on a decoded value, so a change to
// how the parameter is spelled fails here rather than silently reaching a server that refuses
// unknown include values with a 400.
func TestExport_IncludeToolCallsIsSentOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name          string
		withToolCalls bool
		wantInclude   string
	}{
		{"off by default", false, ""},
		{"on when asked", true, "toolCalls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotInclude string
			var gotRaw string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotInclude = r.URL.Query().Get("include")
				gotRaw = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte("{}\n"))
			}))
			defer ts.Close()

			body, err := New(ts.URL).Export(context.Background(), nil, "", tc.withToolCalls)
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			_, _ = io.Copy(io.Discard, body)
			body.Close()

			if gotInclude != tc.wantInclude {
				t.Errorf("include = %q, want %q (raw query %q)", gotInclude, tc.wantInclude, gotRaw)
			}
		})
	}
}

// TestExport_IncludeComposesWithTheOtherParameters: the three filters are orthogonal, and the
// client must not drop one when another is set.
func TestExport_IncludeComposesWithTheOtherParameters(t *testing.T) {
	var q url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{}\n"))
	}))
	defer ts.Close()

	body, err := New(ts.URL).Export(context.Background(), []string{"s1", "s2"}, "168h", true)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	_, _ = io.Copy(io.Discard, body)
	body.Close()

	if got := q["session"]; len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Errorf("session = %v, want both ids", got)
	}
	if got := q.Get("since"); got != "168h" {
		t.Errorf("since = %q", got)
	}
	if got := q.Get("include"); got != "toolCalls" {
		t.Errorf("include = %q", got)
	}
}

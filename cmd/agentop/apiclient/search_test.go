package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestSearchSession_EscapesTheQuery(t *testing.T) {
	var gotID, gotQ string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotQ = r.PathValue("id"), r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{"seqs":[1,4],"lastSeq":9,"schemaVersion":1}`))
	}))
	defer ts.Close()
	mux := http.NewServeMux()
	mux.Handle("GET /v1/sessions/{id}/search", ts.Config.Handler)
	ts.Config.Handler = mux

	res, err := New(ts.URL).SearchSession(context.Background(), "s/1", "a&b #c%d e")
	if err != nil {
		t.Fatal(err)
	}
	if gotID != "s/1" || gotQ != "a&b #c%d e" {
		t.Errorf("proxy got id %q q %q", gotID, gotQ)
	}
	if !slices.Equal(res.Seqs, []uint64{1, 4}) || res.LastSeq != 9 {
		t.Errorf("result %+v", res)
	}
}

// The route answers an unknown session with a JSON body. A proxy without the route answers
// with the mux's plain text.
func TestSearchSession_TellsTheTwo404sApart(t *testing.T) {
	for _, tc := range []struct {
		body string
		want error
	}{
		{"404 page not found\n", ErrNoSearch},
		{`{"error":"session not found"}` + "\n", ErrNotFound},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, tc.body[:len(tc.body)-1], http.StatusNotFound)
		}))
		_, err := New(ts.URL).SearchSession(context.Background(), "s", "q")
		ts.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("body %q: err %v, want %v", tc.body, err, tc.want)
		}
	}
}

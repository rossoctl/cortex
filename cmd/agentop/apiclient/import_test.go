package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/session/importer"
)

func TestClientImport_PostsBodyAndDecodes(t *testing.T) {
	want := importer.Request{Agent: "claude-code", Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), DryRun: true}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/import" {
			t.Errorf("request %s %s, want POST /v1/import", r.Method, r.URL.Path)
		}
		var got importer.Request
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if got.Agent != want.Agent || !got.Since.Equal(want.Since) || got.DryRun != want.DryRun {
			t.Errorf("body = %+v, want %+v", got, want)
		}
		w.Write([]byte(`{"dryRun":true,"agents":[{"agent":"claude-code","imported":2,"error":"bad\u001b[2J"}]}`))
	}))
	defer ts.Close()
	rep, err := New(ts.URL).Import(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || len(rep.Agents) != 1 || rep.Agents[0].Imported != 2 {
		t.Fatalf("report = %+v", rep)
	}
	// A server string is printed to a terminal, so control runes go.
	if rep.Agents[0].Error != "bad[2J" {
		t.Errorf("Error = %q, want control runes stripped", rep.Agents[0].Error)
	}
}

func TestClientImport_StatusErrors(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
		want error
		text string
	}{
		{http.StatusConflict, ``, ErrImportRunning, ""},
		{http.StatusNotFound, `404 page not found`, ErrNotFound, ""},
		{http.StatusBadRequest, `{"error":"unknown agent \"x\"; supported: claude-code"}`, ErrBadRequest, "supported: claude-code"},
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			w.Write([]byte(tc.body))
		}))
		_, err := New(ts.URL).Import(context.Background(), importer.Request{})
		ts.Close()
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%d: err = %v, want %v containing %q", tc.code, err, tc.want, tc.text)
		}
	}
}

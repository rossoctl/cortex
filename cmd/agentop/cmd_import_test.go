package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/session/importer"
)

// fakeImportServer answers POST /v1/import with code and body, and keeps the request body it got.
func fakeImportServer(t *testing.T, code int, body string, got *importer.Request) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/import" {
			http.NotFound(w, r)
			return
		}
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("decode: %v", err)
			}
		}
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
}

func TestRunImport_PrintsOneLinePerAgent(t *testing.T) {
	srv := fakeImportServer(t, 200, `{"agents":[`+
		`{"agent":"claude-code","imported":12,"replaced":3,"skippedLive":40,"skippedCurrent":7,"skippedExpired":1,"skippedLines":2,"unreadable":2,"stopped":"archive full","notImported":5},`+
		`{"agent":"bob","error":"no store"}]}`, nil)
	defer srv.Close()
	var out, errOut strings.Builder
	code := runImport([]string{"--endpoint", srv.URL}, &out, &errOut)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (one agent has an error)", code)
	}
	want := "claude-code: 12 imported, 3 replaced, 40 skipped (live), 7 skipped (current), 1 skipped (expired), 2 lines not recognized\n" +
		"  unreadable: 2 transcripts could not be read\n" +
		"  stopped: archive full, 5 sessions not imported\n" +
		"bob: 0 imported, 0 replaced, 0 skipped (live), 0 skipped (current), 0 skipped (expired), 0 lines not recognized\n" +
		"  error: no store\n"
	if out.String() != want {
		t.Errorf("stdout =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestRunImport_DryRunPrefix(t *testing.T) {
	var got importer.Request
	srv := fakeImportServer(t, 200, `{"dryRun":true,"agents":[{"agent":"claude-code","imported":1}]}`, &got)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runImport([]string{"--endpoint", srv.URL, "--dry-run", "--agent", "claude-code"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, errOut.String())
	}
	if !got.DryRun || got.Agent != "claude-code" {
		t.Errorf("request = %+v, want dryRun and agent claude-code", got)
	}
	if !strings.HasPrefix(out.String(), "dry run: claude-code: 1 imported,") {
		t.Errorf("stdout = %q, want a dry run: prefix", out.String())
	}
}

func TestRunImport_409(t *testing.T) {
	srv := fakeImportServer(t, http.StatusConflict, ``, nil)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runImport([]string{"--endpoint", srv.URL}, &out, &errOut); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "agentop import: an import is already running") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRunImport_404(t *testing.T) {
	srv := fakeImportServer(t, http.StatusNotFound, `404 page not found`, nil)
	defer srv.Close()
	var out, errOut strings.Builder
	if code := runImport([]string{"--endpoint", srv.URL}, &out, &errOut); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no import route") {
		t.Errorf("stderr = %q, want it to say the proxy has no import route", errOut.String())
	}
}

func TestRunImport_UnknownAgentIs2(t *testing.T) {
	var out, errOut strings.Builder
	if code := runImport([]string{"--endpoint", "http://127.0.0.1:1", "--agent", "vim"}, &out, &errOut); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestRunImport_SinceForms(t *testing.T) {
	var got importer.Request
	srv := fakeImportServer(t, 200, `{"agents":[]}`, &got)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runImport([]string{"--endpoint", srv.URL, "--since", "2026-10-01"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, errOut.String())
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local); !got.Since.Equal(want) {
		t.Errorf("since 2026-10-01 = %v, want local midnight %v", got.Since, want)
	}

	got = importer.Request{}
	before := time.Now()
	if code := runImport([]string{"--endpoint", srv.URL, "--since", "30d"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, errOut.String())
	}
	after := time.Now()
	if got.Since.Before(before.Add(-720*time.Hour)) || got.Since.After(after.Add(-720*time.Hour)) {
		t.Errorf("since 30d = %v, want 720h before a time in [%v, %v]", got.Since, before, after)
	}

	for _, bad := range []string{"bad", "0d", "-3d", "2026-13-01"} {
		if code := runImport([]string{"--endpoint", srv.URL, "--since", bad}, &out, &errOut); code != 2 {
			t.Errorf("--since %q: exit = %d, want 2", bad, code)
		}
	}
}

func TestProgressBar(t *testing.T) {
	for _, tc := range []struct {
		p    importer.Progress
		want string
	}{
		{importer.Progress{Agent: "claude-code", Done: 0, Total: 4}, "importing claude-code [░░░░░░░░░░░░░░░░░░░░]   0%  0/4 sessions"},
		{importer.Progress{Agent: "claude-code", Done: 1, Total: 4}, "importing claude-code [█████░░░░░░░░░░░░░░░]  25%  1/4 sessions"},
		{importer.Progress{Agent: "claude-code", Done: 4, Total: 4}, "importing claude-code [████████████████████] 100%  4/4 sessions"},
		{importer.Progress{Agent: "claude-code"}, "importing claude-code [████████████████████] 100%  0/0 sessions"},
		{importer.Progress{Agent: "claude-code", Done: 9, Total: 4}, "importing claude-code [████████████████████] 100%  9/4 sessions"},
	} {
		if got := progressBar(tc.p); got != tc.want {
			t.Errorf("progressBar(%+v) =\n %q, want\n %q", tc.p, got, tc.want)
		}
	}
}

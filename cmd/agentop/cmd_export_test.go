package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exportLines is the canned NDJSON a fake proxy serves: three records, the shape the
// real endpoint writes.
const exportLines = `{"schema":1,"sessionId":"s1","seq":3}
{"schema":1,"sessionId":"s1","seq":2}
{"schema":1,"sessionId":"old","seq":1}
`

// exportServer serves the canned export at /v1/export and records the query it was
// asked for.
func exportServer(t *testing.T, status int, body string) (*httptest.Server, *strings.Builder) {
	t.Helper()
	var query strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/export" {
			http.NotFound(w, r)
			return
		}
		query.WriteString(r.URL.RawQuery)
		w.WriteHeader(status)
		if status == http.StatusBadRequest {
			// The shape the real endpoint answers a refused query with, server words
			// and all — what the client's ErrBadRequest detail is made of.
			w.Write([]byte(`{"error":"sessionexport: no archived session nope (archived: old, s1)"}`))
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &query
}

// The default is a pipe: records to stdout, the one summary line to stderr — never the
// other way round, because stdout may be the export itself.
func TestRunExport_StreamsToStdout(t *testing.T) {
	srv, query := exportServer(t, http.StatusOK, exportLines)
	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("runExport = %d, stderr:\n%s", code, errOut.String())
	}
	if out.String() != exportLines {
		t.Fatalf("stdout = %q, want the export verbatim", out.String())
	}
	if !strings.Contains(errOut.String(), "3 records") || !strings.Contains(errOut.String(), "(stdout)") {
		t.Fatalf("summary = %q, want the record count and the destination", errOut.String())
	}
	if query.String() != "" {
		t.Fatalf("query = %q, want none for the default flags", query.String())
	}
}

// --out writes the file only after the proxy answered, so a refused query never creates
// it; an existing file is refused without --force and overwritten with it.
func TestRunExport_OutFile(t *testing.T) {
	srv, _ := exportServer(t, http.StatusOK, exportLines)
	dir := t.TempDir()
	path := filepath.Join(dir, "export.ndjson")

	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL, "--out", path}, &out, &errOut); code != 0 {
		t.Fatalf("runExport --out = %d, stderr:\n%s", code, errOut.String())
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != exportLines {
		t.Fatalf("file = %q (%v), want the export", b, err)
	}
	if !strings.Contains(errOut.String(), path) {
		t.Fatalf("summary = %q, want it to name the file", errOut.String())
	}

	if code := runExport([]string{"--endpoint", srv.URL, "--out", path}, &out, &errOut); code != 1 ||
		!strings.Contains(errOut.String(), "already exists") {
		t.Fatalf("second --out = %d %q, want a refusal naming the file", code, errOut.String())
	}
	if code := runExport([]string{"--endpoint", srv.URL, "--out", path, "--force"}, &out, &errOut); code != 0 {
		t.Fatalf("runExport --force = %d, stderr:\n%s", code, errOut.String())
	}

	// The refused query never touches the file: a fresh path, a 400, and no file after.
	refusedSrv, _ := exportServer(t, http.StatusBadRequest, "")
	refused := filepath.Join(dir, "refused.ndjson")
	if code := runExport([]string{"--endpoint", refusedSrv.URL, "--out", refused, "--session", "nope"},
		&out, &errOut); code != 1 {
		t.Fatalf("unknown session = %d, want 1", code)
	}
	if _, err := os.Stat(refused); !os.IsNotExist(err) {
		t.Fatalf("refused query created %s (%v)", refused, err)
	}
	if !strings.Contains(errOut.String(), "nope") || !strings.Contains(errOut.String(), "old") {
		t.Fatalf("stderr = %q, want the proxy's own words naming the ids", errOut.String())
	}
}

// The flags reach the wire as the endpoint reads them: repeatable and comma-separated
// sessions, and the since value passed through for the server to parse.
func TestRunExport_FlagsReachTheQuery(t *testing.T) {
	srv, query := exportServer(t, http.StatusOK, exportLines)
	var out, errOut strings.Builder
	code := runExport([]string{"--endpoint", srv.URL, "--session", "s1", "--session", "old,x",
		"--since", "7d"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("runExport = %d, stderr:\n%s", code, errOut.String())
	}
	got := query.String()
	for _, want := range []string{"session=s1", "session=old", "session=x", "since=7d"} {
		if !strings.Contains(got, want) {
			t.Errorf("query %q lacks %s", got, want)
		}
	}
}

// A proxy with no archive is a 404 with its own fix named, not a generic dial failure.
func TestRunExport_NoArchiveIs404(t *testing.T) {
	srv, _ := exportServer(t, http.StatusNotFound, "")
	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL}, &out, &errOut); code != 1 {
		t.Fatalf("runExport = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no session archive") {
		t.Fatalf("stderr = %q, want the no-archive hint", errOut.String())
	}
}

// --force without --out is a usage error: there is nothing it could mean.
func TestRunExport_ForceWithoutOutIsAUsageError(t *testing.T) {
	var out, errOut strings.Builder
	if code := runExport([]string{"--force"}, &out, &errOut); code != 2 {
		t.Fatalf("runExport --force = %d, want 2", code)
	}
}

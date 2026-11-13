package main

import (
	"fmt"
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

// A REFUSAL TOO LONG FOR THE 512 BYTES getBody READS still leaves the user somewhere to
// go. exportServer's canned body is 72 bytes and always fits, so it cannot exercise this;
// #1830's byte budget keeps a current proxy inside the read, but an older proxy caps the
// refusal by id COUNT, and long ids there overrun it. The JSON is then cut mid-string,
// badRequestDetail finds nothing, and the error is a bare "apiclient: bad request" — so
// the command prints where the archived ids can be read instead of stopping at that.
func TestRunExport_RefusalTooLongToParseStillPointsSomewhere(t *testing.T) {
	// What a count-capping proxy answers for 5 unknown UUIDs against 8 archived ones: 538
	// bytes of message, past the client's read. Built here rather than imported, because
	// the point is a proxy this binary did NOT ship with.
	var ids []string
	for i := range 8 {
		ids = append(ids, fmt.Sprintf("3f2504e0-4f89-11d3-9a0c-0305e82c33%02d", i))
	}
	var unknown []string
	for i := range 5 {
		unknown = append(unknown, fmt.Sprintf("nope-3f2504e0-4f89-11d3-9a0c-0305e82c33%02d", i))
	}
	long := fmt.Sprintf(`{"error":"sessionexport: no archived session %s (archived: %s)"}`,
		strings.Join(unknown, ", "), strings.Join(ids, ", "))
	if len(long) <= 512 {
		t.Fatalf("fixture body is %d bytes; it must exceed the 512 getBody reads to test this", len(long))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(long)) //nolint:errcheck // a test server writing to a closed client is the test's own failure
	}))
	t.Cleanup(srv.Close)

	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL, "--session", unknown[0]}, &out, &errOut); code != 1 {
		t.Fatalf("runExport = %d, want 1 on a refused query", code)
	}
	// The premise: the detail really did not survive the read.
	if strings.Contains(errOut.String(), "no archived session") {
		t.Fatalf("fixture no longer overruns the read; stderr = %q", errOut.String())
	}
	// The fallback: a command the user can run to get the ids the refusal could not carry.
	if !strings.Contains(errOut.String(), "/v1/sessions?archived=true") {
		t.Fatalf("stderr = %q, want a pointer to the archived-session list", errOut.String())
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

// A --session that yields no ids is refused, not read as absence: `--session "$(ids)"`
// with an empty expansion is the scripting case the comma split exists for, and reading
// it as "every session" would turn a broken pipe into a full-archive export.
func TestRunExport_EmptySessionIsAUsageError(t *testing.T) {
	for _, arg := range []string{"", " ", ","} {
		var out, errOut strings.Builder
		if code := runExport([]string{"--session", arg}, &out, &errOut); code != 2 {
			t.Fatalf("runExport --session %q = %d, want 2", arg, code)
		}
		if !strings.Contains(errOut.String(), "needs at least one id") {
			t.Fatalf("stderr = %q, want the refusal to name the problem", errOut.String())
		}
	}
}

// A stray positional is a usage error, not a silently ignored filename: `agentop export
// out.ndjson` would otherwise dump the archive into the terminal and call it success.
// Every sibling command rejects positionals the same way.
func TestRunExport_StrayPositionalIsAUsageError(t *testing.T) {
	var out, errOut strings.Builder
	if code := runExport([]string{"out.ndjson"}, &out, &errOut); code != 2 {
		t.Fatalf("runExport out.ndjson = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "out.ndjson") || !strings.Contains(errOut.String(), "--out") {
		t.Fatalf("stderr = %q, want the argument named and --out suggested", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing on a usage error", out.String())
	}
}

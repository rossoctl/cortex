package main

import (
	"errors"
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

// A REFUSAL THAT DID NAME THE PROBLEM GETS NO HINT, even with --session set. The 400 for
// a bad --since arrives whole and says exactly what is wrong; adding "the proxy did not
// name the sessions it holds" under it would send the reader to look at sessions when
// since is the problem. The old condition — "--session was given and the words do not
// mention sessions" — could not tell the two apart.
func TestRunExport_RefusalThatNamedTheProblemGetsNoSessionHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"since must be a duration or Nd, e.g. 168h or 7d"}`)) //nolint:errcheck // a test server writing to a closed client is the test's own failure
	}))
	t.Cleanup(srv.Close)

	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL, "--session", "s1", "--since", "bogus"},
		&out, &errOut); code != 1 {
		t.Fatalf("runExport = %d, want 1 on a refused query", code)
	}
	if !strings.Contains(errOut.String(), "since must be a duration") {
		t.Fatalf("stderr = %q, want the proxy's own words about since", errOut.String())
	}
	if strings.Contains(errOut.String(), "/v1/sessions?archived=true") {
		t.Fatalf("stderr = %q, want NO session hint when the refusal explained itself", errOut.String())
	}
}

// The hint's curl is runnable: a --endpoint with a trailing slash must not produce
// `host//v1/sessions`, which ServeMux answers with a 307 whose HTML body is all `curl -s`
// would print. Endpoint() is the trimmed form the request itself uses.
func TestRunExport_HintTrimsTheEndpoint(t *testing.T) {
	var out, errOut strings.Builder
	// A 400 body getBody cannot read as the documented shape, so the no-detail path runs
	// and the hint fires: exportServer's canned body parses fine and would not reach it.
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`<html>not the documented shape</html>`)) //nolint:errcheck // a test server writing to a closed client is the test's own failure
	}))
	t.Cleanup(bare.Close)

	if code := runExport([]string{"--endpoint", bare.URL + "/", "--session", "nope"},
		&out, &errOut); code != 1 {
		t.Fatalf("runExport = %d, want 1 on a refused query", code)
	}
	if !strings.Contains(errOut.String(), bare.URL+"/v1/sessions?archived=true") {
		t.Fatalf("stderr = %q, want the hint to name %s/v1/sessions", errOut.String(), bare.URL)
	}
	if strings.Contains(errOut.String(), "//v1/sessions") {
		t.Fatalf("stderr = %q, want no doubled slash in the hint", errOut.String())
	}
}

// A TRUNCATED EXPORT EXITS NON-ZERO. The server resets the connection when its walk dies
// part-way (#1830), rather than ending the body cleanly, so io.Copy fails and this
// command reports it instead of printing a success summary over a short file.
func TestRunExport_TruncatedWalkExitsNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte("{\"schema\":1,\"sessionId\":\"s1\",\"seq\":2}\n")) //nolint:errcheck // a test server writing to a closed client is the test's own failure
		w.(http.Flusher).Flush()
		// What handleExport does when the walk dies past its first record: no terminating
		// chunk, so the client's read breaks rather than returning a clean EOF.
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL}, &out, &errOut); code != 1 {
		t.Fatalf("runExport = %d, want 1 on a truncated export; stderr:\n%s", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "records,") {
		t.Fatalf("stderr = %q, want no success summary over a truncated export", errOut.String())
	}
	if !strings.Contains(errOut.String(), "the walk failed part-way") {
		t.Fatalf("stderr = %q, want the part-way-walk line", errOut.String())
	}
}

// errWriter fails every write, the way a full partition does.
type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

// A FAILED WRITE BLAMES THE DISK, NOT THE WALK. io.Copy returns one error for either
// end, and the walk is the wrong thing to accuse when --out ran out of space: the cause
// is local, and a short write can stop mid-line, so "every complete record" would be
// wrong as well. The server here is healthy and sends the whole export.
func TestRunExport_FailedWriteDoesNotBlameTheWalk(t *testing.T) {
	srv, _ := exportServer(t, http.StatusOK, exportLines)
	var errOut strings.Builder
	// A writer that fails like a full disk, rather than /dev/full: the destination goes
	// through the same lineCounter either way, and this runs on every platform instead of
	// skipping everywhere but Linux.
	code := runExport([]string{"--endpoint", srv.URL}, errWriter{errors.New("no space left on device")}, &errOut)
	if code != 1 {
		t.Fatalf("runExport = %d, want 1 when the write fails; stderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "no space left") {
		t.Fatalf("stderr = %q, want the write error itself", errOut.String())
	}
	if strings.Contains(errOut.String(), "the walk failed") {
		t.Fatalf("stderr = %q, want no accusation against the walk on a write failure", errOut.String())
	}
	if !strings.Contains(errOut.String(), "may end in a partial line") {
		t.Fatalf("stderr = %q, want the partial-line warning", errOut.String())
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

// AN EXPLICITLY EMPTY --since IS A USAGE ERROR TOO, the sibling of --session's. Reading
// it as an absent floor turned `--since "$(window)"` with an empty expansion into a
// full-archive export at exit 0 — a broken variable answered with more data than was
// asked for, which is the one direction a filter must not fail in.
//
// Checked against a server, so a regression shows as "it exported anyway" rather than
// passing on the flag parse alone.
func TestRunExport_EmptySinceIsAUsageError(t *testing.T) {
	srv, query := exportServer(t, http.StatusOK, exportLines)
	// Whitespace-only counts: it is the same broken expansion, and the refusal belongs
	// here rather than as the server's 400 after a round trip.
	for _, arg := range []string{"", " ", "\t", "\n"} {
		var out, errOut strings.Builder
		if code := runExport([]string{"--endpoint", srv.URL, "--since", arg}, &out, &errOut); code != 2 {
			t.Fatalf("runExport --since %q = %d, want 2", arg, code)
		}
		if !strings.Contains(errOut.String(), "--since needs a value") {
			t.Fatalf("stderr = %q, want the refusal to name the problem", errOut.String())
		}
		if out.Len() != 0 {
			t.Fatalf("an empty --since still wrote %d bytes of export", out.Len())
		}
	}
	if query.String() != "" {
		t.Fatalf("query = %q, want the request never to have been made", query.String())
	}

	// The control: --since omitted entirely is still a legitimate "every event".
	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("runExport without --since = %d, stderr:\n%s", code, errOut.String())
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

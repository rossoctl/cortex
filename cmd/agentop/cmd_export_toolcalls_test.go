package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// exportToolCallLines is what a CURRENT proxy answers for ?include=toolCalls: every record
// carries `kind`, which is the signal this command detects support by (spec T28).
const exportToolCallLines = `{"schema":1,"kind":"event","sessionId":"s1","seq":1,"at":"2026-11-10T09:00:00Z"}
{"schema":1,"kind":"toolCall","sessionId":"s1","startedAt":"2026-11-10T09:00:02Z","tool":"Bash","callSeq":2,"callIndex":0,"endReason":"result"}
`

// exportKindButNoToolCalls is a CURRENT proxy answering for an archive that ran no tools:
// `kind` is present, a toolCall record is not. Support, not skew.
const exportKindButNoToolCalls = `{"schema":1,"kind":"event","sessionId":"s1","seq":1,"at":"2026-11-10T09:00:00Z"}
`

// TestRunExport_WithToolCallsSendsInclude: the flag reaches the endpoint, and only when asked.
func TestRunExport_WithToolCallsSendsInclude(t *testing.T) {
	srv, query := exportServer(t, http.StatusOK, exportToolCallLines)
	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL, "--with-tool-calls"}, &out, &errOut); code != 0 {
		t.Fatalf("runExport = %d, stderr:\n%s", code, errOut.String())
	}
	if !strings.Contains(query.String(), "include=toolCalls") {
		t.Errorf("query = %q, want include=toolCalls", query.String())
	}

	srv2, query2 := exportServer(t, http.StatusOK, exportLines)
	out.Reset()
	errOut.Reset()
	if code := runExport([]string{"--endpoint", srv2.URL}, &out, &errOut); code != 0 {
		t.Fatalf("runExport = %d, stderr:\n%s", code, errOut.String())
	}
	if strings.Contains(query2.String(), "include") {
		t.Errorf("query = %q, want no include without the flag", query2.String())
	}
}

// TestRunExport_SkewOnAProxyThatIgnoredTheFlag is the version-skew failure, and it is a
// NON-ZERO EXIT rather than a warning.
//
// A proxy predating the parameter ignores it and answers 200 with the plain stream — it does
// not refuse — so without this the user asks for tool calls, receives an export without them,
// and is told nothing. In a script (`agentop export --with-tool-calls --out f && analyse f`)
// a warning on stderr is lost and the analysis silently runs over the wrong file.
func TestRunExport_SkewOnAProxyThatIgnoredTheFlag(t *testing.T) {
	// The pre-feature answer: a 200 whose records carry no `kind`.
	srv, _ := exportServer(t, http.StatusOK, exportLines)
	var out, errOut strings.Builder
	code := runExport([]string{"--endpoint", srv.URL, "--with-tool-calls"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("runExport = 0 on a proxy that ignored the flag; stderr:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "ignored --with-tool-calls") {
		t.Errorf("stderr = %q, want it to name the ignored flag", errOut.String())
	}
	// The records that DID arrive are still written and still summarised: the user needs to
	// know what they have, not only what they lack.
	if out.String() != exportLines {
		t.Errorf("stdout = %q, want the records the proxy did send", out.String())
	}
	if !strings.Contains(errOut.String(), "3 records") {
		t.Errorf("stderr = %q, want the summary line kept", errOut.String())
	}
}

// TestRunExport_NoToolCallsIsNotSkew is the false positive the `kind` signal exists to avoid.
// An archive that ran no tools produces no toolCall records, and that is a correct export —
// keying detection on their presence would fail this perfectly good run.
func TestRunExport_NoToolCallsIsNotSkew(t *testing.T) {
	srv, _ := exportServer(t, http.StatusOK, exportKindButNoToolCalls)
	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL, "--with-tool-calls"}, &out, &errOut); code != 0 {
		t.Fatalf("runExport = %d for an archive with no tool calls; stderr:\n%s", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "ignored") {
		t.Errorf("stderr = %q, want no skew report", errOut.String())
	}
}

// TestRunExport_EmptyExportIsNotSkew: with no records there is no `kind` to find, and nothing
// was misreported either.
func TestRunExport_EmptyExportIsNotSkew(t *testing.T) {
	srv, _ := exportServer(t, http.StatusOK, "")
	var out, errOut strings.Builder
	if code := runExport([]string{"--endpoint", srv.URL, "--with-tool-calls"}, &out, &errOut); code != 0 {
		t.Fatalf("runExport = %d on an empty export; stderr:\n%s", code, errOut.String())
	}
}

// TestRunExport_SkewIsReportedForAnOutFileToo: the same failure through --out, where the
// misleading artifact is a file a later command will read.
func TestRunExport_SkewIsReportedForAnOutFileToo(t *testing.T) {
	srv, _ := exportServer(t, http.StatusOK, exportLines)
	path := t.TempDir() + "/e.ndjson"
	var out, errOut strings.Builder
	code := runExport([]string{"--endpoint", srv.URL, "--with-tool-calls", "--out", path}, &out, &errOut)
	if code == 0 {
		t.Fatalf("runExport = 0 writing a file without the requested records; stderr:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), path) {
		t.Errorf("stderr = %q, want it to name the file that holds no tool calls", errOut.String())
	}
}

// TestLineCounter_KindSurvivesAFragmentedFirstWrite is the short-read regression.
//
// io.Copy hands Write whatever each read returned, and a read may return fewer bytes than
// asked for. An opted-in record begins `{"schema":1,"kind":` — the token ends at byte 19 — so
// a first chunk shorter than that once latched the check false and failed a VALID export with
// "this proxy ignored --with-tool-calls". The decision now waits for a complete line.
//
// Driven by writing the fragments directly rather than through httptest, which hands the body
// over in one piece and so cannot reach this at all: that is exactly why the original tests
// passed while the bug was live.
func TestLineCounter_KindSurvivesAFragmentedFirstWrite(t *testing.T) {
	const rec = `{"schema":1,"kind":"event","sessionId":"s1","seq":1}` + "\n"
	for _, split := range []int{1, 5, 12, 18, 19, 20, len(rec) - 1, len(rec)} {
		var sink strings.Builder
		l := &lineCounter{w: &sink}
		if _, err := l.Write([]byte(rec[:split])); err != nil {
			t.Fatalf("split %d: %v", split, err)
		}
		if split < len(rec) {
			if _, err := l.Write([]byte(rec[split:])); err != nil {
				t.Fatalf("split %d: %v", split, err)
			}
		}
		if !l.sawKind {
			t.Errorf("split at %d: sawKind false for a record that carries kind — "+
				"a valid export would be reported as skew", split)
		}
		if sink.String() != rec {
			t.Errorf("split at %d: wrote %q, want the record verbatim", split, sink.String())
		}
	}
}

// TestLineCounter_NoKindIsStillDetectedAcrossFragments: the other direction must keep working,
// or the fix would turn a real skew into a silent pass.
func TestLineCounter_NoKindIsStillDetectedAcrossFragments(t *testing.T) {
	const rec = `{"schema":1,"sessionId":"s1","seq":1}` + "\n"
	for _, split := range []int{1, 9, 20, len(rec) - 1} {
		var sink strings.Builder
		l := &lineCounter{w: &sink}
		l.Write([]byte(rec[:split]))
		l.Write([]byte(rec[split:]))
		if l.sawKind {
			t.Errorf("split at %d: sawKind true for a record with no kind", split)
		}
	}
}

// TestLineCounter_UnterminatedRecordStillDecides: a stream whose last record carries no
// trailing newline leaves the check undecided until settleKind runs.
func TestLineCounter_UnterminatedRecordStillDecides(t *testing.T) {
	var sink strings.Builder
	l := &lineCounter{w: &sink}
	l.Write([]byte(`{"schema":1,"kind":"event"`))
	if l.kindSeen {
		t.Fatal("decided with no newline yet")
	}
	l.settleKind()
	if !l.sawKind {
		t.Error("settleKind missed the kind field in the buffered partial record")
	}
}

// TestLineCounter_RunawayFirstLineIsBounded: a stream that never sends a newline must not grow
// the buffer without limit.
func TestLineCounter_RunawayFirstLineIsBounded(t *testing.T) {
	var sink strings.Builder
	l := &lineCounter{w: &sink}
	for range 4 {
		if _, err := l.Write(bytes.Repeat([]byte("x"), 2048)); err != nil {
			t.Fatal(err)
		}
	}
	if !l.kindSeen {
		t.Error("the check never settled on a stream with no newline")
	}
	if len(l.kindHead) != 0 {
		t.Errorf("buffer still holds %d bytes after the bound", len(l.kindHead))
	}
}

package archive

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

var _ session.HostsKeeper = (*Archive)(nil)

// hostEvents is two agents' inference requests to three hosts, with a tunnel row between them.
func hostEvents() []pipeline.SessionEvent {
	cc := pipeline.ParseUserAgent("claude-cli/2.1.0 (external, cli)")
	oc := pipeline.ParseUserAgent("opencode/1.0")
	inf := func(host string, c *pipeline.EventClient, path, requested string) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: host, Client: c,
			HTTPMethod: http.MethodPost, HTTPPath: path, RequestedHost: requested,
			Inference: &pipeline.InferenceExtension{Model: "m"}}
	}
	return []pipeline.SessionEvent{
		inf("glm.example.com:8443", cc, "/v1/messages", "api.anthropic.com"),
		inf("opencode.ai", oc, "/zen/v1/chat/completions", ""),
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.anthropic.com:443", Tunnel: true, HTTPMethod: http.MethodConnect, Client: cc},
		inf("ete.example.com", cc, "/v1/messages", "api.anthropic.com"),
	}
}

// hostsOf is the store's table for id, as a listener hydrates it.
func hostsOf(st *session.Store, id string) (model.InferenceHosts, bool) {
	_, f := st.ViewFacts(id)
	return f.InferenceHosts, f.HostsKnown
}

// A restart changes no session's table: before the new entry exists the archive's table is the
// answer, and after an event with no evidence creates the entry, the merge with it is.
func TestHosts_SameAnswerAcrossARestart(t *testing.T) {
	evs := hostEvents()
	ref := session.New(0, 0, 0)
	defer ref.Close()
	for _, e := range evs {
		ref.Append("s1", e)
	}
	want, ok := hostsOf(ref, "s1")
	if !ok || len(want) != 3 {
		t.Fatalf("fixture: %+v %v", want, ok)
	}

	a, st := restarted(t, t.TempDir(), newClock(), "s1", evs)
	defer a.Close()
	defer st.Close()
	if got, ok := a.InferenceHosts("s1"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("the archive's table: %+v %v, want %+v", got, ok, want)
	}
	got, _ := a.InferenceHosts("s1")
	got[0].Last = 99
	if again, _ := a.InferenceHosts("s1"); !reflect.DeepEqual(again, want) {
		t.Fatal("a change to the archive's answer reached its index")
	}
	if got, ok := hostsOf(st, "s1"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("after a restart, no entry: %+v %v, want %+v", got, ok, want)
	}
	st.Append("s1", evs[2]) // a tunnel row: no evidence, but it creates the entry
	if got, ok := hostsOf(st, "s1"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("after a restart, entry: %+v %v, want %+v", got, ok, want)
	}
	if got, ok := a.InferenceHosts("nobody"); !ok || got != nil {
		t.Fatalf("an id the archive does not hold: %+v %v, want (nil, true)", got, ok)
	}
}

// metaPath is root's session.json for id.
func metaPath(root, id string) string { return filepath.Join(root, dataDirName, dirName(id), metaFile) }

// A session.json the fold did not write a table of this rule into holds history with no table:
// a file from before the table, one of another rule version, and one with no summary at all.
// Each is unknown, before the store's entry exists and after. Unchanged, the file is known.
func TestHosts_ASessionJSONWithNoTableIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(doc map[string]any)
		known bool
	}{
		{"unchanged", func(map[string]any) {}, true},
		{"no table", func(doc map[string]any) { delete(doc["summary"].(map[string]any), "inferenceHosts") }, false},
		{"another rule version", func(doc map[string]any) {
			doc["summary"].(map[string]any)["inferenceHosts"].(map[string]any)["v"] = 99
		}, false},
		{"no summary", func(doc map[string]any) { delete(doc, "summary") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			a, st := restarted(t, root, newClock(), "s1", hostEvents())
			a.Close()
			st.Close()
			b, err := os.ReadFile(metaPath(root, "s1"))
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			if _, ok := doc["summary"].(map[string]any)["inferenceHosts"]; !ok {
				t.Fatalf("no inferenceHosts in %s", b)
			}
			tc.edit(doc)
			if b, err = json.Marshal(doc); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metaPath(root, "s1"), b, 0o600); err != nil {
				t.Fatal(err)
			}

			a = openTest(t, root, newClock())
			defer a.Close()
			st = session.New(0, 0, 0)
			defer st.Close()
			st.AddRecorder(a)
			if _, ok := hostsOf(st, "s1"); ok != tc.known {
				t.Fatalf("no entry: known %v, want %v", ok, tc.known)
			}
			st.Append("s1", hostEvents()[0])
			if _, ok := hostsOf(st, "s1"); ok != tc.known {
				t.Fatalf("an entry over the history: known %v, want %v", ok, tc.known)
			}
		})
	}
}

// After a crash, reconcile folds the events session.json had not counted, so the table covers
// every event on disk.
func TestHosts_ACrashedSessionKeepsItsTable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, dataDirName, dirName("s1"))
	evs := hostEvents()
	for i := range evs {
		evs[i].Seq, evs[i].SessionID = uint64(i+1), "s1"
	}
	info := writeSegmentIn(t, dir, time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local), evs)
	m := newMeta("s1")
	m.Summary.Add("s1", &evs[0])
	stale := info
	stale.LastSeq, stale.Events, stale.Bytes = 1, 1, 10
	m.Segments = []SegmentInfo{stale}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	whole := session.NewSummaryFold()
	for i := range evs {
		whole.Add("s1", &evs[i])
	}
	want, _ := whole.InferenceHosts()

	a := openTest(t, root, newClock())
	defer a.Close()
	if got, ok := a.InferenceHosts("s1"); !ok || len(want) != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("after reconcile: %+v %v, want %+v", got, ok, want)
	}
}

// An entry re-created while the archive's writer still holds an earlier entry's evidence in its
// queue sees that evidence once the writer writes it: ViewFacts reads the archive's table when
// it answers, not when the entry is created.
func TestHosts_AnEntryReCreatedBeforeTheWriterCatchesUpSeesItsHistory(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	st := session.New(0, 0, 1)
	defer st.Close()
	st.AddRecorder(a)
	evs := hostEvents()
	st.Append("s1", evs[0]) // seq 1, to glm
	settle(a)
	a.gate = make(chan struct{}) // hold the writer
	st.Append("s1", evs[3])      // seq 2, to ete: queued
	st.Append("s2", evs[2])      // max_sessions 1 evicts s1
	st.Append("s1", evs[2])      // s1 again, numbered after seq 2
	close(a.gate)
	settle(a)

	got, ok := hostsOf(st, "s1")
	want := model.InferenceHosts{
		{Agent: "claude-code", Host: "glm.example.com:8443", Last: 1},
		{Agent: "claude-code", Host: "ete.example.com", Last: 2},
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("after the writer caught up: %+v %v, want %+v", got, ok, want)
	}
}

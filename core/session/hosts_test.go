package session

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
)

const claudeUA = "claude-cli/2.1.0 (external, cli)"

// hostsArchive is fakeArchive that is also a HostsKeeper.
type hostsArchive struct {
	*fakeArchive
	hosts   map[string]model.InferenceHosts
	unknown map[string]bool
}

func newHostsArchive() *hostsArchive {
	return &hostsArchive{fakeArchive: newFakeArchive(), hosts: map[string]model.InferenceHosts{}, unknown: map[string]bool{}}
}

func (k *hostsArchive) InferenceHosts(id string) (model.InferenceHosts, bool) {
	return k.hosts[id], !k.unknown[id]
}

// inferenceTo is an outbound inference request from ua to host: evidence for the table.
func inferenceTo(host, ua string) pipeline.SessionEvent {
	return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: host,
		RequestedHost: "api.anthropic.com", HTTPMethod: http.MethodPost, HTTPPath: "/v1/messages",
		Client: pipeline.ParseUserAgent(ua), Inference: &pipeline.InferenceExtension{Model: "m"}}
}

func seen(agent, host string, last, unrouted uint64) model.InferenceHostSeen {
	return model.InferenceHostSeen{Agent: agent, Host: host, Last: last, LastUnrouted: unrouted}
}

func wantHosts(t *testing.T, name string, f model.SessionFacts, want model.InferenceHosts, known bool) {
	t.Helper()
	if f.HostsKnown != known || !reflect.DeepEqual(f.InferenceHosts, want) {
		t.Fatalf("%s: hosts %+v known %v, want %+v known %v", name, f.InferenceHosts, f.HostsKnown, want, known)
	}
}

// With no keeper the store vouches for its own table: (nil, known) before the entry exists, and
// the entry's own evidence after. A trim sheds nothing, and a caller's change to the answer
// does not reach the store.
func TestViewFacts_HostsWithNoKeeper(t *testing.T) {
	s := New(0, 2, 0)
	defer s.Close()
	_, f := s.ViewFacts("s1")
	wantHosts(t, "no entry", f, nil, true)

	s.Append("s1", inferenceTo("glm.example.com:8443", claudeUA))
	s.Append("s1", tunnelRow())
	s.Append("s1", inferenceTo("ete.example.com", claudeUA))
	s.Append("s1", inferenceTo("glm.example.com:8443", claudeUA))
	s.Append("s1", tunnelRow())
	s.Append("s1", tunnelRow()) // max_events 2: every request left the view
	_, f = s.ViewFacts("s1")
	want := model.InferenceHosts{seen("claude-code", "glm.example.com:8443", 4, 0), seen("claude-code", "ete.example.com", 3, 0)}
	wantHosts(t, "entry", f, want, true)

	f.InferenceHosts[0].Last = 99
	_, f = s.ViewFacts("s1")
	wantHosts(t, "after a change to an earlier answer", f, want, true)
}

// With no entry the keeper's whole table is the answer, known or not, and a copy.
func TestViewFacts_WithNoEntryTheKeeperAnswers(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	k := newHostsArchive()
	k.hosts["s1"] = model.InferenceHosts{seen("claude-code", "glm.example.com:8443", 4, 0)}
	k.hosts["s2"] = model.InferenceHosts{seen("opencode", "opencode.ai", 2, 2)}
	k.unknown["s2"] = true
	s.AddRecorder(k)

	_, f := s.ViewFacts("s1")
	wantHosts(t, "s1", f, k.hosts["s1"], true)
	f.InferenceHosts[0].Last = 99
	if k.hosts["s1"][0].Last != 4 {
		t.Fatal("ViewFacts handed out the keeper's own slice")
	}
	_, f = s.ViewFacts("s2")
	wantHosts(t, "s2", f, k.hosts["s2"], false)
	_, f = s.ViewFacts("nobody")
	wantHosts(t, "an id nobody holds", f, nil, true)
}

// An entry numbered after history merges the keeper's table with its own, by the highest seq for
// each pair. HostsKnown is the keeper's.
func TestViewFacts_AnEntryAfterHistoryMergesTheKeepersTable(t *testing.T) {
	for _, known := range []bool{true, false} {
		s := New(0, 0, 0)
		k := newHostsArchive()
		k.last["s1"] = 10
		k.hosts["s1"] = model.InferenceHosts{seen("claude-code", "glm.example.com:8443", 9, 0), seen("claude-code", "ete.example.com", 7, 7)}
		k.unknown["s1"] = !known
		s.AddRecorder(k)

		s.Append("s1", inferenceTo("ete.example.com", claudeUA)) // seq 11
		s.Append("s1", tunnelRow())
		_, f := s.ViewFacts("s1")
		wantHosts(t, "merged", f, model.InferenceHosts{
			seen("claude-code", "glm.example.com:8443", 9, 0), seen("claude-code", "ete.example.com", 11, 7)}, known)
		s.Close()
	}
}

// An entry with no history before it (the keeper's LastSeq is 0) does not take the keeper's
// table, so the keeper's "unknown" cannot reach it.
func TestViewFacts_AnEntryWithNoHistoryIsItsOwnTable(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	k := newHostsArchive()
	k.hosts["s1"] = model.InferenceHosts{seen("claude-code", "glm.example.com:8443", 9, 0)}
	k.unknown["s1"] = true
	s.AddRecorder(k)
	s.Append("s1", inferenceTo("ete.example.com", claudeUA))
	_, f := s.ViewFacts("s1")
	wantHosts(t, "entry", f, model.InferenceHosts{seen("claude-code", "ete.example.com", 1, 0)}, true)
}

// A rename moves the entry and not the history before it, which stays under the old id. So
// the new id's table is the entry's own, and known; the old id's is the keeper's again.
func TestViewFacts_RekeyClearsTheHistorysTable(t *testing.T) {
	s := New(0, 0, 0)
	defer s.Close()
	k := newHostsArchive()
	k.last[DefaultSessionID] = 5
	k.hosts[DefaultSessionID] = model.InferenceHosts{seen("claude-code", "glm.example.com:8443", 4, 0)}
	k.unknown[DefaultSessionID] = true
	s.AddRecorder(k)
	s.Append(DefaultSessionID, inferenceTo("ete.example.com", claudeUA)) // seq 6
	_, f := s.ViewFacts(DefaultSessionID)
	wantHosts(t, "before the rename", f, model.InferenceHosts{
		seen("claude-code", "glm.example.com:8443", 4, 0), seen("claude-code", "ete.example.com", 6, 0)}, false)

	s.Rekey(DefaultSessionID, "ctx-1")
	_, f = s.ViewFacts("ctx-1")
	wantHosts(t, "the new id", f, model.InferenceHosts{seen("claude-code", "ete.example.com", 6, 0)}, true)
	_, f = s.ViewFacts(DefaultSessionID)
	wantHosts(t, "the old id", f, k.hosts[DefaultSessionID], false)
}

// The table survives session.json: a fold carried through JSON and given more events has the
// table of one fold over all of them. An empty table stays known.
func TestSummaryFold_HostsResumeFromJSON(t *testing.T) {
	evs := hostRuleFixture()
	whole := NewSummaryFold()
	for i := range evs {
		whole.Add("s1", &evs[i])
	}
	first := NewSummaryFold()
	for i := range evs[:5] {
		first.Add("s1", &evs[i])
	}
	resumed := roundTrip(t, first)
	for i := range evs[5:] {
		resumed.Add("s1", &evs[5+i])
	}
	w, wk := whole.InferenceHosts()
	g, gk := resumed.InferenceHosts()
	if !wk || !gk || len(w) < 3 || !reflect.DeepEqual(g, w) {
		t.Fatalf("resumed %+v (known %v), whole %+v (known %v)", g, gk, w, wk)
	}
	if _, known := roundTrip(t, NewSummaryFold()).InferenceHosts(); !known {
		t.Fatal("an empty table came back unknown")
	}
}

func roundTrip(t *testing.T, f *SummaryFold) *SummaryFold {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	out := NewSummaryFold()
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A session.json with no table, or with a table of another rule version, holds history the
// fold has no table for. Unknown is sticky: later Adds keep it, and it persists as absent.
func TestSummaryFold_HostsOfALegacyOrOtherVersionDocAreUnknown(t *testing.T) {
	for name, doc := range map[string]string{
		"no table":        `{"eventCount":3}`,
		"a later version": `{"eventCount":3,"inferenceHosts":{"v":2,"hosts":[{"agent":"claude-code","host":"h","last":1}]}}`,
		"no version":      `{"eventCount":3,"inferenceHosts":{"hosts":[{"agent":"claude-code","host":"h","last":1}]}}`,
	} {
		f := NewSummaryFold()
		if err := json.Unmarshal([]byte(doc), f); err != nil {
			t.Fatal(err)
		}
		if h, known := f.InferenceHosts(); known || h != nil {
			t.Errorf("%s: hosts %+v known %v, want unknown", name, h, known)
		}
		e := inferenceTo("ete.example.com", claudeUA)
		e.Seq = 4
		f.Add("s1", &e)
		if _, known := f.InferenceHosts(); known {
			t.Errorf("%s: an Add made the table known", name)
		}
		b, _ := json.Marshal(f)
		if bytes.Contains(b, []byte(`"inferenceHosts"`)) {
			t.Errorf("%s: an unknown table was written: %s", name, b)
		}
	}
}

// Agent names are folded to their canonical spelling on decode, as the fold's agents are, and
// two spellings of one agent merge into one entry.
func TestSummaryFold_HostsFoldRetiredAgentSpellings(t *testing.T) {
	f := NewSummaryFold()
	doc := `{"inferenceHosts":{"v":1,"hosts":[{"agent":"bob-shell","host":"h","last":3,"lastUnrouted":3},{"agent":"bob","host":"h","last":5}]}}`
	if err := json.Unmarshal([]byte(doc), f); err != nil {
		t.Fatal(err)
	}
	h, known := f.InferenceHosts()
	if want := (model.InferenceHosts{seen("bob", "h", 5, 3)}); !known || !reflect.DeepEqual(h, want) {
		t.Fatalf("hosts %+v known %v, want %+v", h, known, want)
	}
}

// InferenceHosts hands out a copy: the archive's writer goes on folding into its own.
func TestSummaryFold_InferenceHostsIsACopy(t *testing.T) {
	f := NewSummaryFold()
	e := inferenceTo("ete.example.com", claudeUA)
	e.Seq = 1
	f.Add("s1", &e)
	h, _ := f.InferenceHosts()
	h[0].Last = 99
	if h, _ := f.InferenceHosts(); h[0].Last != 1 {
		t.Fatal("a change to the answer reached the fold")
	}
}

// hostRuleFixture is one event of each kind the host rule tells apart.
func hostRuleFixture() []pipeline.SessionEvent {
	opencode := "opencode/1.0"
	main := func(e pipeline.SessionEvent, tools bool, assistant bool) pipeline.SessionEvent {
		ext := *e.Inference
		ext.AgentRole = pipeline.AgentRoleMain
		if tools {
			ext.Tools = []pipeline.InferenceTool{{Name: "read"}}
		}
		if assistant {
			ext.Messages = []pipeline.InferenceMessage{{Role: "user"}, {Role: "assistant"}}
		}
		e.Inference = &ext
		return e
	}
	unrouted := func(host, ua, method, path string) pipeline.SessionEvent {
		e := inferenceTo(host, ua)
		e.RequestedHost, e.HTTPMethod, e.HTTPPath = "", method, path
		return e
	}
	subagent := inferenceTo("glm.example.com:8443", claudeUA)
	subagent.Inference.AgentRole = pipeline.AgentRoleSubagent
	response := inferenceTo("glm.example.com:8443", claudeUA)
	response.Phase = pipeline.SessionResponse
	inbound := inferenceTo("glm.example.com:8443", claudeUA)
	inbound.Direction = pipeline.Inbound
	noClient := inferenceTo("ete.example.com", "")
	noClient.Client = nil
	evs := []pipeline.SessionEvent{
		main(inferenceTo("glm.example.com:8443", claudeUA), true, false),  // the opening turn
		main(inferenceTo("glm.example.com:8443", claudeUA), false, false), // a one-shot: aside
		subagent, // aside
		main(inferenceTo("ete.example.com", claudeUA), true, true),                     // a continuation
		unrouted("opencode.ai", opencode, http.MethodPost, "/zen/v1/chat/completions"), // captured, unrouted
		unrouted("opencode.ai", opencode, http.MethodGet, "/zen/v1/models"),            // not captured
		unrouted("127.0.0.1:11434", "curl/8.7.1", http.MethodPost, "/v1/responses"),    // captured, unrouted
		unrouted("127.0.0.1:11434", "curl/8.7.1", http.MethodPost, "/v1/messages/count_tokens"),
		response, inbound, tunnelRow(), noClient,
		inferenceTo("glm.example.com:8443", "bob/2.0.5"),
	}
	for i := range evs {
		evs[i].Seq = uint64(i + 1)
	}
	return evs
}

// TestHostRule_Golden holds the host rule to a golden table, as session.json persists it. A
// persisted table is only right for the rule that wrote it. So when this fails because you
// changed hostEvidenceOf, applyHost, TurnOf, CapturePath or AgentName on purpose, bump
// hostRuleVersion and then write the new table to the golden file. A table of the old version
// then decodes as unknown.
func TestHostRule_Golden(t *testing.T) {
	f := NewSummaryFold()
	evs := hostRuleFixture()
	for i := range evs {
		f.Add("s1", &evs[i])
	}
	h, _ := f.InferenceHosts()
	got, err := json.MarshalIndent(hostsDoc{V: hostRuleVersion, Hosts: h}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "inference-hosts.golden.json")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the host rule changed: bump hostRuleVersion, then write this to %s:\n%s", path, got)
	}
}

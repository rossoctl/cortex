package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// inferenceReq is a request the inference parser recognised, sent to host.
func inferenceReq(host string) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		Phase:     pipeline.SessionRequest,
		Host:      host,
		Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"},
	}
}

// interleaved is what live traffic puts between two inference requests: a bridged CONNECT's
// tunnel-open, an MCP call and its response, the inference response itself, and a denial. None
// of them may move InferenceHost — each carries a host, and three of them an extension.
//
// THE DENIAL CARRIES AN INFERENCE PARSE, as a live one does: the parser runs before the plugin
// that turns the request away (the router's pinned_server_removed, for one). It went nowhere, so
// its host says nothing about where the session's inference goes; the inference-router's own
// history rule (wentTo) skips a denied row for the same reason. Last, so that interleaved()[:3]
// stays a run with no inference parse at all.
func interleaved() []pipeline.SessionEvent {
	return []pipeline.SessionEvent{
		{Phase: pipeline.SessionRequest, Tunnel: true, HTTPMethod: "CONNECT", Host: "api.anthropic.com:443"},
		{Phase: pipeline.SessionRequest, Host: "tools.local:8080", MCP: &pipeline.MCPExtension{Method: "tools/call"}},
		{Phase: pipeline.SessionResponse, Host: "tools.local:8080", MCP: &pipeline.MCPExtension{Method: "tools/call"}},
		{Phase: pipeline.SessionResponse, Host: "elsewhere.example.com", Inference: &pipeline.InferenceExtension{TotalTokens: 10}},
		{Phase: pipeline.SessionDenied, Host: "denied.example.com", Inference: &pipeline.InferenceExtension{Model: "claude-opus-5-5"}},
	}
}

func TestInferenceHost_TheLatestInferenceRequestWins(t *testing.T) {
	s := New(5*time.Minute, 0, 0)
	defer s.Close()
	s.Append("s1", inferenceReq("ete.example.com"))
	for _, e := range interleaved() {
		s.Append("s1", e)
	}
	if got := summaryOf(t, s, "s1").InferenceHost; got != "ete.example.com" {
		t.Fatalf("InferenceHost = %q after a tunnel row, an MCP call, a response and a denial; want the inference request's ete.example.com", got)
	}
	s.Append("s1", inferenceReq("glm.example.com:8443"))
	if got := summaryOf(t, s, "s1").InferenceHost; got != "glm.example.com:8443" {
		t.Fatalf("InferenceHost = %q, want the later request's glm.example.com:8443", got)
	}
}

// Absent, not "": an unknown value must not render as a real one, so the key is omitted for a
// session no inference request reached.
func TestInferenceHost_AbsentForASessionWithNoInferenceTraffic(t *testing.T) {
	s := New(5*time.Minute, 0, 0)
	defer s.Close()
	for _, e := range interleaved()[:3] {
		s.Append("s1", e)
	}
	sum := summaryOf(t, s, "s1")
	if sum.InferenceHost != "" {
		t.Fatalf("InferenceHost = %q for a session with no inference request", sum.InferenceHost)
	}
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "inferenceHost") {
		t.Errorf("the summary sends inferenceHost for a session with no inference traffic: %s", b)
	}
}

// A trim drops the request that set it, and the field stays: it is the session's latest value,
// not a figure over what the store still holds.
func TestInferenceHost_SurvivesATrim(t *testing.T) {
	s := New(5*time.Minute, 2, 0)
	defer s.Close()
	s.Append("s1", inferenceReq("ete.example.com"))
	for _, e := range interleaved() {
		s.Append("s1", e)
	}
	if got := summaryOf(t, s, "s1").InferenceHost; got != "ete.example.com" {
		t.Fatalf("InferenceHost = %q after the request that set it was trimmed", got)
	}
}

// The archive's fold says what the store says, and keeps it through session.json at every split.
func TestSummaryFold_InferenceHostAgreesAndResumes(t *testing.T) {
	evs := append([]pipeline.SessionEvent{inferenceReq("ete.example.com")}, interleaved()...)
	evs = append(evs, inferenceReq("glm.example.com:8443"))
	evs = append(evs, interleaved()...)

	s := New(5*time.Minute, 0, 0)
	defer s.Close()
	whole := NewSummaryFold()
	for i := range evs {
		s.Append("s1", evs[i])
		whole.Add("s1", &evs[i])
	}
	want := summaryOf(t, s, "s1").InferenceHost
	if want != "glm.example.com:8443" || whole.Summary("s1").InferenceHost != want {
		t.Fatalf("store says %q, fold says %q", want, whole.Summary("s1").InferenceHost)
	}
	for split := range len(evs) + 1 {
		f := NewSummaryFold()
		for i := range evs[:split] {
			f.Add("s1", &evs[i])
		}
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		g := NewSummaryFold()
		if err := json.Unmarshal(b, g); err != nil {
			t.Fatal(err)
		}
		for i := split; i < len(evs); i++ {
			g.Add("s1", &evs[i])
		}
		if got := g.Summary("s1").InferenceHost; got != want {
			t.Errorf("split %d: InferenceHost = %q, want %q", split, got, want)
		}
	}
}

// A session.json written before the field existed decodes to no host, and the next inference
// request sets it.
func TestSummaryFold_AnOlderSessionJSONHasNoInferenceHost(t *testing.T) {
	f := NewSummaryFold()
	if err := json.Unmarshal([]byte(`{"eventCount":3}`), f); err != nil {
		t.Fatal(err)
	}
	if got := f.Summary("s1").InferenceHost; got != "" {
		t.Fatalf("InferenceHost = %q from a document without one", got)
	}
	e := inferenceReq("ete.example.com")
	f.Add("s1", &e)
	if got := f.Summary("s1").InferenceHost; got != "ete.example.com" {
		t.Fatalf("InferenceHost = %q, want ete.example.com", got)
	}
}

// A session resumed from the archive reports the archive's inferenceHost until its own next
// inference request, and says that is where the value came from: no request this store entry
// holds set it. After a restart that is what tells a client the inference-router holds no pin
// for the session, which it sets by a request it routes and keeps in memory only.
func TestInferenceHost_SaysWhenOnlyTheHistorySetIt(t *testing.T) {
	earlier := NewSummaryFold()
	first := routedTo("ete.example.com", claudeUA, "team-a")
	earlier.Add("s1", &first)
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 1}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	for _, e := range interleaved() {
		st.Append("s1", e)
	}
	sum := summaryOf(t, st, "s1")
	if sum.InferenceHost != "ete.example.com" || sum.InferenceServer != "team-a" || !sum.InferenceHostFromHistory {
		t.Fatalf("resumed with no inference request since: InferenceHost %q, InferenceServer %q, FromHistory %v; want the history's ete.example.com and team-a, from history",
			sum.InferenceHost, sum.InferenceServer, sum.InferenceHostFromHistory)
	}
	if b, _ := json.Marshal(sum); !strings.Contains(string(b), `"inferenceHostFromHistory":true`) {
		t.Errorf("the wire does not say it: %s", b)
	}

	st.Append("s1", inferenceReq("ete.example.com"))
	sum = summaryOf(t, st, "s1")
	if sum.InferenceHost != "ete.example.com" || sum.InferenceServer != "" || sum.InferenceHostFromHistory {
		t.Fatalf("after its own unrouted request to the same host: InferenceHost %q, InferenceServer %q, FromHistory %v; want ete.example.com, no server, not from history",
			sum.InferenceHost, sum.InferenceServer, sum.InferenceHostFromHistory)
	}
	if b, _ := json.Marshal(sum); strings.Contains(string(b), "inferenceHostFromHistory") {
		t.Errorf("the wire sends the flag for a host the entry set: %s", b)
	}
}

// A session with no history before its entry never carries the flag, whatever it has sent.
func TestInferenceHost_IsNeverFromHistoryWithoutOne(t *testing.T) {
	st := New(0, 0, 0)
	defer st.Close()
	st.Append("s1", inferenceReq("ete.example.com"))
	st.Append("s2", interleaved()[0])
	for _, id := range []string{"s1", "s2"} {
		if sum := summaryOf(t, st, id); sum.InferenceHostFromHistory {
			t.Errorf("%s: InferenceHostFromHistory with no history: %+v", id, sum)
		}
	}
}

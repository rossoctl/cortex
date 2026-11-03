package session

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// foldRecorder folds exactly what a Recorder is handed, which is what the archive will fold.
type foldRecorder struct{ folds map[string]*SummaryFold }

func (r *foldRecorder) Record(id string, e *pipeline.SessionEvent) {
	f := r.folds[id]
	if f == nil {
		f = NewSummaryFold()
		r.folds[id] = f
	}
	f.Add(id, e)
}

// One realistic mixture rather than one fixture per rule, for TestSumCost's reason: the rules
// interact, and what matters is the sum. Contentless events sit between the turns because live
// traffic interleaves them (gotcha #14).
func foldFixture(t *testing.T) []pipeline.SessionEvent {
	t.Helper()
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	bob := &pipeline.EventClient{Name: "bob-shell", Version: "2.0.5"}
	turn := func(content string, tokens int, rec event.Event, c *pipeline.EventClient) []pipeline.SessionEvent {
		req := titleEvent(content)
		req.Phase, req.Client = pipeline.SessionRequest, c
		// One host per agent, so the session's inference host moves between turns and the fold
		// and the store must agree on where it ends. Not a latest-wins check: the first and the
		// latest turns are both claude-code's, so a first-wins fold would pass here too. That
		// rule is pinned in inference_host_test.go.
		req.Host = c.Name + ".example.com"
		tunnel := pipeline.SessionEvent{Phase: pipeline.SessionRequest, Tunnel: true, HTTPMethod: "CONNECT"}
		resp := pipeline.SessionEvent{
			Phase:     pipeline.SessionResponse,
			Client:    c,
			Inference: &pipeline.InferenceExtension{TotalTokens: tokens},
			Plugins:   costRecord(t, rec),
		}
		return []pipeline.SessionEvent{req, tunnel, resp}
	}
	var evs []pipeline.SessionEvent
	evs = append(evs, turn("fix the flaky test in the session store", 1200,
		event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured",
			Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 100, USD: 0.01, Tier: "input"}}}, claude)...)
	evs = append(evs, turn("now explain why it was flaky", 800,
		event.Event{CostUSD: 0.03, Settled: true, Currency: "Bobcoins"}, bob)...)
	evs = append(evs, pipeline.SessionEvent{Phase: pipeline.SessionDenied,
		Plugins: costRecord(t, event.Event{CostUSD: 0.05, Settled: true, Provenance: "authoritative"})})
	evs = append(evs, turn("and the third thing", 0,
		event.Event{Source: event.SourceUsageFallback,
			Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 200, USD: 0.02, Tier: "input"}}}, claude)...)
	// A main-agent turn with a tool manifest, so the fixture carries a CTX figure.
	evs = append(evs, pipeline.SessionEvent{Phase: pipeline.SessionResponse, Client: claude,
		At: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC),
		Inference: &pipeline.InferenceExtension{TotalTokens: 300, PromptTokens: 9000,
			Tools: []pipeline.InferenceTool{{Name: "read"}}}})
	return evs
}

// The archive reports a disk-only session's figures from a SummaryFold, and the session API
// shows them beside resident sessions' ListSessions figures. For an untrimmed, unclaimed
// session the two must be equal, or one column means two things depending on where a row
// came from.
func TestSummaryFold_AgreesWithListSessions(t *testing.T) {
	s := New(5*time.Minute, 0, 0)
	defer s.Close()
	r := &foldRecorder{folds: map[string]*SummaryFold{}}
	s.AddRecorder(r)
	for _, e := range foldFixture(t) {
		s.Append("s1", e)
	}

	want := summaryOf(t, s, "s1")
	got := r.folds["s1"].Summary("s1")

	if got.EventCount != want.EventCount || got.Title != want.Title || got.Agent != want.Agent ||
		got.TotalTokens != want.TotalTokens || got.CostMicros != want.CostMicros ||
		got.AvoidedMicros != want.AvoidedMicros || got.Saturated != want.Saturated ||
		!slices.Equal(got.Currencies, want.Currencies) || !sameContext(got.PromptContext, want.PromptContext) ||
		got.InferenceHost != want.InferenceHost {
		t.Fatalf("fold  = %+v\nstore = %+v", got, want)
	}
	if want.CostMicros == 0 || want.AvoidedMicros == 0 || want.Title == "" || want.Agent == "" ||
		len(want.Currencies) != 2 || want.PromptContext == nil || want.InferenceHost != "claude-code.example.com" {
		t.Fatalf("fixture no longer exercises every figure: %+v", want)
	}
}

// The default and pending buckets name no agent, in the fold as in the store.
func TestSummaryFold_NamesNoAgentForTheDefaultAndPendingBuckets(t *testing.T) {
	for _, id := range []string{DefaultSessionID, PendingSessionID("claude-code")} {
		f := NewSummaryFold()
		f.Add(id, &pipeline.SessionEvent{Client: &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}})
		if a := f.Summary(id).Agent; a != "" {
			t.Errorf("%s: Agent = %q, want empty", id, a)
		}
	}
}

// The archive persists a fold in session.json and resumes it after a restart, so a fold carried
// through JSON and then given more events must report exactly what one fold over all of them
// does — including the title rank, which decides whether a later /rename may replace the title.
func TestSummaryFold_ResumesFromJSON(t *testing.T) {
	evs := foldFixture(t)
	whole := NewSummaryFold()
	for i := range evs {
		whole.Add("s1", &evs[i])
	}

	first := NewSummaryFold()
	for i := range evs[:4] {
		first.Add("s1", &evs[i])
	}
	b, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	resumed := NewSummaryFold()
	if err := json.Unmarshal(b, resumed); err != nil {
		t.Fatal(err)
	}
	for i := range evs[4:] {
		resumed.Add("s1", &evs[4+i])
	}
	w, g := whole.Summary("s1"), resumed.Summary("s1")
	if w.EventCount != g.EventCount || w.Title != g.Title || w.Agent != g.Agent || w.TotalTokens != g.TotalTokens ||
		w.CostMicros != g.CostMicros || w.AvoidedMicros != g.AvoidedMicros || !slices.Equal(w.Currencies, g.Currencies) ||
		w.InferenceHost != g.InferenceHost {
		t.Fatalf("resumed = %+v\nwhole   = %+v", g, w)
	}
}

// rankRename is 0, so a decoded fold with no titleRank must not claim it was renamed — that
// would lock the title against every later candidate but a /rename.
func TestSummaryFold_AMissingTitleRankIsNotARename(t *testing.T) {
	f := NewSummaryFold()
	if err := json.Unmarshal([]byte(`{"eventCount":3}`), f); err != nil {
		t.Fatal(err)
	}
	f.Add("s1", ptr(titleEvent("a title that should win")))
	if got := f.Summary("s1").Title; got != "a title that should win" {
		t.Fatalf("Title = %q, want the candidate", got)
	}
}

func ptr[T any](v T) *T { return &v }

// sameContext compares two CTX figures by value, the time by instant.
func sameContext(a, b *pipeline.PromptContext) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Tokens == b.Tokens && a.Stated == b.Stated && a.Msgs == b.Msgs && a.At.Equal(b.At)
}

// CTX survives session.json. PromptContextFold has no restore path, so the fold keeps the figure
// it read and merges it with what it folds afterwards — which is the max one uninterrupted fold
// takes, since the published order is the fold's order. Split at every event.
func TestSummaryFold_PromptContextResumesFromJSON(t *testing.T) {
	evs := foldFixture(t)
	whole := NewSummaryFold()
	for i := range evs {
		whole.Add("s1", &evs[i])
	}
	want := whole.Summary("s1").PromptContext
	if want == nil {
		t.Fatal("fixture carries no CTX figure")
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
		if got := g.Summary("s1").PromptContext; !sameContext(got, want) {
			t.Errorf("split %d: PromptContext = %+v, want %+v", split, got, want)
		}
	}
}

// A session.json written before an agent-name rename holds the spelling of its day, and
// UnmarshalJSON is where that persisted label re-enters memory — every reader of a restored
// fold (Summary, withPriorLocked's agentOfLocked, Add's dedup, a re-Marshal) reads f.agents,
// so folding HERE is what keeps a resumed session on today's row instead of serving the
// retired spelling beside it. The labels are what applyAgent computed when they were written
// (event.AgentLabel is AgentName-folded at append), so folding again is idempotent for every
// current spelling and changes only the retired ones.
func TestSummaryFold_UnmarshalFoldsRetiredAgentSpellings(t *testing.T) {
	// The JSON a pre-rename Cortex persisted: both of Bob's faces under the names they
	// carried then, plus a current spelling and an unrecognised client that must not move.
	const written = `{"eventCount":3,"agents":[` +
		`{"name":"bob-shell","label":"bob-shell"},` +
		`{"name":"ibm-bob","label":"ibm-bob"},` +
		`{"name":"claude-code","label":"claude-code"},` +
		`{"name":"curl","label":"curl","unknown":true}],"totalTokens":10}`
	var f SummaryFold
	if err := json.Unmarshal([]byte(written), &f); err != nil {
		t.Fatal(err)
	}
	if got := f.Summary("s1").Agent; got != "bob" {
		t.Errorf("Agent = %q, want %q — a session.json written before the rename serves the retired spelling",
			got, "bob")
	}

	// The dedup must see the folded name: a resumed session's next event names today's
	// "bob", and a restored entry still keyed "bob-shell" would take a second agent slot
	// and keep winning firstAgentLabel by order.
	e := evFrom(&pipeline.EventClient{Name: "bob", Version: "2.0.5"})
	e.SessionID = "s1"
	f.Add("s1", &e)
	if n := len(f.agents); n != 4 {
		t.Errorf("a resumed session's event added a %dth agent; the restored entry must dedup under the folded name:\n%v", n, f.agents)
	}
	if got := f.Summary("s1").Agent; got != "bob" {
		t.Errorf("Agent after resume = %q, want %q", got, "bob")
	}

	// Current and unrecognised spellings are untouched: folding is idempotent for them.
	for _, a := range f.agents {
		switch a.name {
		case "claude-code", "curl":
			if a.label != a.name {
				t.Errorf("agent %q: label = %q, want it unchanged", a.name, a.label)
			}
		}
	}
}

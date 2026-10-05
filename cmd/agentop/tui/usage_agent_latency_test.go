package tui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
)

// usageAgentAxisJSON is a group=agent window that two recognised agents and one raw User-Agent
// shared. Its one bucket's latency is the mean over all of them — the figure the narrowing must
// never hand to one agent.
const usageAgentAxisJSON = `{
  "window":"1h0m0s","bucketSeconds":60,"group":"agent","currencies":["USD"],
  "buckets":[
    {"at":"2026-09-29T10:00:00Z","requests":25,"latMeanMs":50,"latStdDevMs":4,"latSamples":25,
     "series":{"claude-code":{"requests":14},"bob-shell":{"requests":8},"curl/8.4.0":{"requests":3}}}],
  "totals":{"requests":25}}`

// agentLatencyServer answers the narrowing read with usageAgentAxisJSON and the agent= read with
// own, and counts the agent= reads and keeps the last one's query.
func agentLatencyServer(t *testing.T, own string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var reads atomic.Int32
	var query atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.Query().Has("agent") {
			_, _ = w.Write([]byte(usageAgentAxisJSON))
			return
		}
		reads.Add(1)
		query.Store(r.URL.RawQuery)
		_, _ = w.Write([]byte(own))
	}))
	t.Cleanup(ts.Close)
	return ts, &reads, &query
}

// Scoped to a recognised agent, the pane's latency is that agent's own, read from the ring the
// server keeps for it (#1294). The counts and the window's units still come from the narrowing.
//
// THE BUCKET TIMES ARE THE SAME INSTANT WRITTEN TWO WAYS, so the graft is shown to match on the
// instant: two times decoded from a non-local offset carry distinct *time.Location values, and
// compare unequal with == however equal they are.
func TestFetchUsage_ScopedLatencyIsTheAgentsOwn(t *testing.T) {
	ts, reads, query := agentLatencyServer(t, `{"window":"1h0m0s","bucketSeconds":60,"group":"none","agent":"claude-code",
		"buckets":[{"at":"2026-09-29T06:00:00-04:00","requests":14,"latMeanMs":80,"latStdDevMs":6,"latSamples":14}],
		"totals":{"requests":14}}`)
	m := &model{client: apiclient.New(ts.URL), agentScope: "claude-code"}
	msg := m.fetchUsage()().(usageLoadedMsg)
	if msg.err != nil {
		t.Fatalf("fetch errored: %v", msg.err)
	}
	if !msg.agentLatency {
		t.Fatal("agentLatency is false; the pane would say latency is unavailable for an agent the server has it for")
	}
	b := msg.snap.Buckets[0]
	if b.LatMeanMs != 80 || b.LatStdDevMs != 6 || b.LatSamples != 14 {
		t.Errorf("bucket latency = %v±%v over %d, want the agent's own 80±6 over 14 (the window's is 50±4 over 25)",
			b.LatMeanMs, b.LatStdDevMs, b.LatSamples)
	}
	if b.Requests != 14 || msg.snap.Totals.Requests != 14 {
		t.Errorf("requests = %d in the bucket, %d in total; want the narrowed 14 in both", b.Requests, msg.snap.Totals.Requests)
	}
	if !slices.Equal(msg.windowUnits, []string{"USD"}) {
		t.Errorf("windowUnits = %v, want the window's [USD]: costUngroupedRow states the residual in them", msg.windowUnits)
	}
	if reads.Load() != 1 {
		t.Fatalf("%d agent= reads, want 1", reads.Load())
	}
	q, _ := url.ParseQuery(query.Load().(string))
	if q.Get("agent") != "claude-code" || q.Has("session") || q.Has("group") {
		t.Errorf("agent= read asked %q; want agent=claude-code with no session and no group, the ring's answer", query.Load())
	}
}

// AN ANSWER THAT IS NOT THE AGENT'S OWN IS NOT GRAFTED, and the pane says latency is
// unavailable rather than plotting it.
//
// The first case is the one that matters most: a server that predates agent= ignores it and
// answers for every agent, so grafting would draw the whole window's response times under one
// agent's name. The second is a server that applied agent= by narrowing the agent axis, as every
// one did before the per-agent rings — its buckets have requests and no latency sample.
func TestFetchUsage_ScopedLatencyRefusesAnAnswerThatIsNotTheAgents(t *testing.T) {
	for _, tc := range []struct{ name, own string }{
		{"a server that ignored agent=", usageAgentAxisJSON},
		{"a server that narrowed the agent axis", `{"window":"1h0m0s","bucketSeconds":60,"group":"none","agent":"claude-code",
			"buckets":[{"at":"2026-09-29T10:00:00Z","requests":14}],"totals":{"requests":14}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _, _ := agentLatencyServer(t, tc.own)
			m := &model{client: apiclient.New(ts.URL), agentScope: "claude-code"}
			msg := m.fetchUsage()().(usageLoadedMsg)
			if msg.err != nil {
				t.Fatalf("fetch errored: %v", msg.err)
			}
			if msg.agentLatency {
				t.Error("agentLatency is true for an answer that is not the agent's own")
			}
			if b := msg.snap.Buckets[0]; b.LatMeanMs != 0 || b.LatSamples != 0 {
				t.Errorf("bucket latency = %v over %d, want none — not the window's 50 over 25", b.LatMeanMs, b.LatSamples)
			}
		})
	}
}

// Under Other and within a session the agent= read is not made: the server keeps no ring for
// Other, and answers an agent within a session by the same narrowing the pane already did.
func TestFetchUsage_ScopedLatencyIsNotAskedWhereNoRingCouldAnswer(t *testing.T) {
	for _, tc := range []struct{ name, scope, session string }{
		{"Other", otherAgents, ""},
		{"an agent within a session", "claude-code", "sess-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, reads, _ := agentLatencyServer(t, `{"agent":"claude-code"}`)
			m := &model{client: apiclient.New(ts.URL), agentScope: tc.scope}
			m.usage.session = tc.session
			msg := m.fetchUsage()().(usageLoadedMsg)
			if msg.err != nil {
				t.Fatalf("fetch errored: %v", msg.err)
			}
			if reads.Load() != 0 || msg.agentLatency {
				t.Errorf("%d agent= reads, agentLatency %v; want none and false", reads.Load(), msg.agentLatency)
			}
		})
	}
}

// With the agent's own latency in hand the scoped pane plots it, and the summary line carries it.
func TestUsagePane_ScopedLatencyIsPlotted(t *testing.T) {
	m := fitModel(t, paneUsage, 120, 40, nil)
	for !m.usage.metric.isLatency() {
		m.usage.cycleMetric()
	}
	m.agentScope = "claude-code"
	for i := range m.usage.snap.Buckets {
		m.usage.snap.Buckets[i].LatMeanMs, m.usage.snap.Buckets[i].LatStdDevMs, m.usage.snap.Buckets[i].LatSamples = 900, 100, 5
	}
	m.usage.agentLatency = true
	body := m.renderUsage(m.width, m.bodyHeight)
	if strings.Contains(body, "not available") {
		t.Errorf("the pane says latency is unavailable for an agent whose latency it holds:\n%s", body)
	}
	if !strings.ContainsRune(body, whiskerMean) || !strings.Contains(body, "LATENCY 0.90s") {
		t.Errorf("the scoped latency is not plotted, or not summarised:\n%s", body)
	}
}

// Where it cannot be plotted the pane says why, because each reason has a different way out.
func TestUsagePane_UnavailableScopedLatencyNamesTheReason(t *testing.T) {
	for _, tc := range []struct{ name, scope, session, want string }{
		{"Other", otherAgents, "", "Other pools agents"},
		{"an agent within a session", "claude-code", "sess-1", "Within a session"},
		{"a proxy without per-agent rings", "claude-code", "", "predates per-agent latency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fitModel(t, paneUsage, 120, 40, nil)
			for !m.usage.metric.isLatency() {
				m.usage.cycleMetric()
			}
			m.agentScope, m.usage.session = tc.scope, tc.session
			body := m.renderUsage(m.width, m.bodyHeight)
			if !strings.Contains(body, "not available per agent") || !strings.Contains(body, tc.want) {
				t.Errorf("want the unavailable note with %q:\n%s", tc.want, body)
			}
		})
	}
}

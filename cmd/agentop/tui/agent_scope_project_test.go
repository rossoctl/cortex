package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
	coremodel "github.com/rossoctl/cortex/core/model"
)

// projectServer answers /v1/usage and records each query. With echo set it answers a project=
// request for that project with two model series and echoes project and agent. Otherwise it acts
// as a proxy older than project=: it ignores the key and answers group=project with two projects.
func projectServer(t *testing.T, echo bool) (*apiclient.Client, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var asked []url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		asked = append(asked, q)
		mu.Unlock()
		snap := usage.Snapshot{Window: q.Get("window"), Group: usage.Group(q.Get("group")), Agent: q.Get("agent")}
		var series map[string]int64
		switch {
		case echo && q.Has("project"):
			snap.Project = coremodel.LenientString(q.Get("project"))
			series = map[string]int64{"opus": 70, "sonnet": 30}
		case snap.Group == usage.GroupProject:
			series = map[string]int64{"/repo/a": 100, "/repo/b": 900}
		}
		b := usage.Bucket{Series: map[string]usage.Counts{}}
		for label, micros := range series {
			b.Series[label] = usage.Counts{CostMicros: micros}
			snap.Totals.Add(usage.Counts{CostMicros: micros})
		}
		snap.Buckets = []usage.Bucket{b}
		_ = json.NewEncoder(w).Encode(snap)
	}))
	t.Cleanup(ts.Close)
	return apiclient.New(ts.URL), func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return asked
	}
}

// A reply that echoes the project is used as the server gave it, broken down by the asked axis.
func TestFetchUsageScopedProject_UsesTheEchoedReply(t *testing.T) {
	client, asked := projectServer(t, true)
	snap, err := fetchUsageScopedProject(context.Background(), client, "today", 0, "claude-code", "/repo/a", usage.GroupModel)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Group != usage.GroupModel || snap.Totals.CostMicros != 100 {
		t.Errorf("snapshot = %+v, want the group=model reply with 100 micros", snap)
	}
	if n := len(asked()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// Review focus 1: a proxy older than project= ignores it and answers for the agent's whole
// traffic. With no echo the read falls back to group=project, narrowed here to the project.
func TestFetchUsageScopedProject_NoEchoFallsBack(t *testing.T) {
	client, asked := projectServer(t, false)
	snap, err := fetchUsageScopedProject(context.Background(), client, "today", 0, "claude-code", "/repo/a", usage.GroupModel)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Totals.CostMicros != 100 {
		t.Errorf("totals = %d micros, want 100 (/repo/a only)", snap.Totals.CostMicros)
	}
	qs := asked()
	if len(qs) != 2 || qs[0].Get("project") != "/repo/a" || qs[1].Get("group") != string(usage.GroupProject) || qs[1].Has("project") {
		t.Errorf("queries = %v, want project=/repo/a, then group=project with no project key", qs)
	}
}

// Other, (other) and the agent Other are agentop's own fold labels: no server knows them, so
// project= never goes on the wire with one, whether or not the server echoes. A server that
// echoes agent=Other&project=X would answer a zero that agentop takes as the project's spend.
func TestFetchUsageScopedProject_NeverSendsOther(t *testing.T) {
	client, asked := projectServer(t, true)
	for _, c := range []struct{ agent, project string }{
		{"", otherProjects}, {"", projectOverflowLabel}, {otherAgents, "/repo/a"},
	} {
		// (other) is not a series of the reply, beside no overflow series: a zero, not an error.
		if _, err := fetchUsageScopedProject(context.Background(), client, "today", 0, c.agent, c.project, usage.GroupNone); err != nil {
			t.Fatalf("agent %q, project %q: %v", c.agent, c.project, err)
		}
	}
	for _, q := range asked() {
		if q.Has("project") {
			t.Errorf("query %v carries project=", q)
		}
	}
}

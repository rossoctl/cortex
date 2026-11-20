package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/cost/usage"
	coremodel "github.com/rossoctl/cortex/core/model"
)

// echoProjectModel is a sessions-pane model scoped to claude-code and ~/works/cortex. Its server
// echoes project and agent, and answers each query with reply(q).
func echoProjectModel(t *testing.T, reply func(q url.Values) usage.Snapshot) *model {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		snap := reply(q)
		snap.Window, snap.Agent = q.Get("window"), q.Get("agent")
		snap.Project = coremodel.LenientString(q.Get("project"))
		if snap.Group == "" {
			snap.Group = usage.Group(q.Get("group"))
		}
		for _, b := range snap.Buckets {
			for _, c := range b.Series {
				snap.Totals.Add(c)
			}
		}
		_ = json.NewEncoder(w).Encode(snap)
	}))
	t.Cleanup(ts.Close)
	m, _ := projectsModel(t, defaultProjectSeries)
	m.client = apiclient.New(ts.URL)
	m.agentScope, m.projectScope = projAgent, "~/works/cortex"
	m.pane = paneSessions
	return m
}

func seriesBucket(series map[string]int64) []usage.Bucket {
	b := usage.Bucket{Series: map[string]usage.Counts{}}
	for label, micros := range series {
		b.Series[label] = projectCounts(micros)
	}
	return []usage.Bucket{b}
}

// drawerLines opens the drawer, delivers its reply and renders it.
func drawerLines(t *testing.T, m *model) string {
	t.Helper()
	m.spend.expanded = true
	m.Update(m.fetchSpendDrawer()())
	axis, window := m.drawerLabels()
	return strings.Join(renderSpendDrawerAxes(m.spend.drawer.snap, m.spend.drawer.err, axis,
		m.spend.drawerAxis, m.spendAxes(), window, m.spend.drawerProject, m.width), "\n")
}

func TestSpendDrawer_ProjectScopeShowsModelRows(t *testing.T) {
	m := echoProjectModel(t, func(q url.Values) usage.Snapshot {
		return usage.Snapshot{Buckets: seriesBucket(map[string]int64{"claude-opus-5": 9_000_000, "claude-sonnet-5": 3_000_000})}
	})
	lines := drawerLines(t, m)
	if m.spend.drawer.snap == nil || m.spend.drawer.snap.Group != usage.GroupModel {
		t.Fatalf("drawer reply %+v, want the group=model reply", m.spend.drawer.snap)
	}
	for _, want := range []string{"claude-opus-5", "claude-sonnet-5"} {
		if !strings.Contains(lines, want) {
			t.Errorf("drawer lines have no %s row:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "no breakdown") || !strings.Contains(lines, "[a] [model] · endpoint") {
		t.Errorf("drawer lines say there is no breakdown, or have no model · endpoint hint:\n%s", lines)
	}
}

func TestSpendDrawer_ProjectScopeGroupNoneShowsNote(t *testing.T) {
	m := echoProjectModel(t, func(q url.Values) usage.Snapshot {
		return usage.Snapshot{Group: usage.GroupNone, Buckets: seriesBucket(nil)}
	})
	lines := drawerLines(t, m)
	_, window := m.drawerLabels()
	if want := "no breakdown for this scope in " + window; !strings.Contains(lines, want) {
		t.Errorf("drawer lines do not say %q:\n%s", want, lines)
	}
}

func TestSpendDrawer_OtherProjectHasNoAxes(t *testing.T) {
	m, _ := projectsModel(t, defaultProjectSeries)
	m.agentScope, m.projectScope = projAgent, otherProjects
	m.pane = paneSessions
	if axes := m.spendAxes(); axes != nil {
		t.Errorf("spendAxes %q under Other, want nil", axes)
	}
}

func TestSpendBand_ProjectScopeShowsEachUnit(t *testing.T) {
	m := echoProjectModel(t, func(q url.Values) usage.Snapshot {
		snap := usage.Snapshot{Priced: true, Currencies: []string{"Bobcoins", "USD"}}
		if usage.Group(q.Get("group")) == usage.GroupCurrency {
			snap.Buckets = seriesBucket(map[string]int64{"USD": 12_000_000, "Bobcoins": 950_000})
		} else {
			snap.Buckets = seriesBucket(map[string]int64{"claude-opus-5": 12_950_000})
		}
		return snap
	})
	msg := bandReply(t, m)
	m.spend.chains[spanHour].reqSeq = msg.req
	m.applySpendLoaded(msg)
	if got := bandValue(m.spanReadings()[spanHour]); got != "$12.00 + 0.95 Bobcoins" {
		t.Errorf("hour cell = %q, want $12.00 + 0.95 Bobcoins", got)
	}
}

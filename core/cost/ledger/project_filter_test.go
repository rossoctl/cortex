package ledger

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// project= matches the group=project label, so an empty project matches "unknown".
func TestFilterProject_MatchesTheLabel(t *testing.T) {
	rows := []Row{{Project: "/repo/a"}, {Project: "/repo/b"}, {Project: ""}}
	if got := FilterProject(rows, "/repo/a"); len(got) != 1 || got[0].Project != "/repo/a" {
		t.Errorf("FilterProject(/repo/a) = %+v, want the one /repo/a row", got)
	}
	if got := FilterProject(rows, usage.UnknownProjectLabel); len(got) != 1 || got[0].Project != "" {
		t.Errorf("FilterProject(unknown) = %+v, want the one row with no project", got)
	}
}

// agent= and project= both narrow the rows before group=model breaks them down.
func TestSnapshot_ProjectAndAgent(t *testing.T) {
	now := at
	w := newTestWriter(t, t.TempDir(), func() time.Time { return now })
	w.Record("s1", projectEvent(t, at, "/repo/a", 0.25))
	w.Record("s1", projectEvent(t, at, "/repo/b", 0.25))
	bob := projectEvent(t, at, "/repo/a", 0.25)
	bob.Inference.Model = "m2"
	bob.Client = &pipeline.EventClient{Name: "bob", Version: "1.0"}
	w.Record("s1", bob)
	spec := usage.Spec{Label: usage.WindowToday, From: usage.StartOfLocalDay(at), To: at.Add(time.Minute)}
	snap, err := w.Snapshot(context.Background(), spec, usage.GroupModel, "claude-code", "/repo/a")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	series := snap.Buckets[0].Series
	if got, want := seriesKeys(series), []string{"opus"}; !slices.Equal(got, want) {
		t.Fatalf("series = %v, want %v", got, want)
	}
	if got := series["opus"].Requests; got != 1 {
		t.Errorf("opus Requests = %d, want 1", got)
	}
	if snap.Project != "/repo/a" {
		t.Errorf("Project = %q, want /repo/a", snap.Project)
	}
}

// One project billed in two units keeps both series, and Currencies names both.
func TestSnapshot_ProjectCurrencySplit(t *testing.T) {
	now := at
	w := newTestWriter(t, t.TempDir(), func() time.Time { return now })
	w.Record("s1", projectEvent(t, at, "/repo/a", 0.25))
	credits := projectEvent(t, at, "/repo/a", 0.25)
	rec, err := json.Marshal(event.Event{
		CostUSD: 0.25, Settled: true, Currency: "Bobcoins",
		Source: event.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	credits.Plugins = map[string]json.RawMessage{event.Key: rec}
	w.Record("s1", credits)
	w.Record("s1", projectEvent(t, at, "/repo/b", 0.25))
	spec := usage.Spec{Label: usage.WindowToday, From: usage.StartOfLocalDay(at), To: at.Add(time.Minute)}
	snap, err := w.Snapshot(context.Background(), spec, usage.GroupCurrency, "", "/repo/a")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := []string{"Bobcoins", "USD"}
	if got := seriesKeys(snap.Buckets[0].Series); !slices.Equal(got, want) {
		t.Errorf("series = %v, want %v", got, want)
	}
	if got := slices.Sorted(slices.Values(snap.Currencies)); !slices.Equal(got, want) {
		t.Errorf("Currencies = %v, want %v", got, want)
	}
	if snap.Totals.Requests != 2 {
		t.Errorf("Totals.Requests = %d, want 2: /repo/b must be filtered out", snap.Totals.Requests)
	}
}

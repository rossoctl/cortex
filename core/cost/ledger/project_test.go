package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
)

// projectEvent is a settled $costUSD response from claude-code in project at when.
func projectEvent(t *testing.T, when time.Time, project string, costUSD float64) *pipeline.SessionEvent {
	t.Helper()
	e := costedEventAt(t, when, "opus", costUSD)
	e.Project = project
	e.Client = &pipeline.EventClient{Name: "claude-code", Version: "1.0"}
	return e
}

func seriesKeys(s map[string]usage.Counts) []string {
	var out []string
	for k := range s {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// group=project over a month reads rows of two days, and an empty project is the unknown series.
func TestWriter_GroupProjectOverAMonth(t *testing.T) {
	day2 := at.Add(24 * time.Hour)
	now := at
	w := newTestWriter(t, t.TempDir(), func() time.Time { return now })
	w.Record("s1", projectEvent(t, at, "/repo/a", 0.25))
	w.Record("s1", projectEvent(t, at, "", 0.10))
	now = day2
	w.Record("s1", projectEvent(t, day2, "/repo/b", 0.50))
	w.Record("s1", projectEvent(t, day2, "/repo/a", 0.25))
	now = day2.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	spec, err := usage.ParseWindowSpec(usage.WindowMonth, now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	snap, err := w.Snapshot(context.Background(), spec, usage.GroupProject, "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Group != usage.GroupProject {
		t.Fatalf("Group = %q, want %q", snap.Group, usage.GroupProject)
	}
	series := snap.Buckets[0].Series
	if got, want := seriesKeys(series), []string{"/repo/a", "/repo/b", usage.UnknownProjectLabel}; !slices.Equal(got, want) {
		t.Fatalf("series = %v, want %v", got, want)
	}
	for label, want := range map[string]int64{"/repo/a": 500_000, "/repo/b": 500_000, usage.UnknownProjectLabel: 100_000} {
		if got := series[label].CostMicros; got != want {
			t.Errorf("%s CostMicros = %d, want %d", label, got, want)
		}
	}
	if snap.Totals.CostMicros != 1_100_000 {
		t.Errorf("Totals.CostMicros = %d, want 1100000", snap.Totals.CostMicros)
	}
}

// Two repos whose paths share a prefix longer than 96 bytes stay two series: the cut keeps the end.
func TestWriter_LongProjectsWithOnePrefixStayApart(t *testing.T) {
	base := "~/src/github.com/example-org/" + strings.Repeat("a", 70) + "-"
	now := at
	w := newTestWriter(t, t.TempDir(), func() time.Time { return now })
	w.Record("s1", projectEvent(t, at, base+"frontend", 0.25))
	w.Record("s1", projectEvent(t, at, base+"backend", 0.25))
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	spec, err := usage.ParseWindowSpec(usage.WindowToday, now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	snap, err := w.Snapshot(context.Background(), spec, usage.GroupProject, "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	keys := seriesKeys(snap.Buckets[0].Series)
	if len(keys) != 2 {
		t.Fatalf("series = %q, want two, one per repo", keys)
	}
	for _, k := range keys {
		if len(k) > maxLabelLen || !strings.HasSuffix(k, "end") {
			t.Errorf("label %q: want at most %d bytes that keep the end of the path", k, maxLabelLen)
		}
	}
}

// A row written before the field existed has no project key: it reads as unknown.
func TestRow_AHistoricalRowWithNoProjectReadsAsUnknown(t *testing.T) {
	const historical = `{"at":"2026-09-01T10:00:00Z","endpoint":"api.anthropic.com",` +
		`"model":"claude-opus-5","agent":"claude-code/2.1.270","provenance":"authoritative",` +
		`"requests":3,"tokens":1000,"costMicros":4170000}`
	var r Row
	if err := json.Unmarshal([]byte(historical), &r); err != nil {
		t.Fatalf("a historical row must stay readable: %v", err)
	}
	if r.Project != "" {
		t.Errorf("Project = %q, want empty; the decoder must not invent a value", r.Project)
	}
	if got, ok := labelFor(r, usage.GroupProject); got != usage.UnknownProjectLabel || !ok {
		t.Errorf("labelFor = %q, %v; want %q, true", got, ok, usage.UnknownProjectLabel)
	}
}

// Past the per-minute cap the project folds to (other) with every other label, and no cost is lost.
func TestWriter_OverflowFoldsTheProjectToo(t *testing.T) {
	now := at
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return now })
	const sent = maxLabelsPerMinute + 1
	for i := range sent {
		w.Record("s1", projectEvent(t, at, fmt.Sprintf("/repo/%d", i), 0.25))
	}
	now = at.Add(time.Minute)
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	rows := readAllRows(t, dir)
	var total int64
	overflowRows := 0
	for _, r := range rows {
		total += r.CostMicros
		if r.Project != overflowLabel {
			continue
		}
		overflowRows++
		for name, got := range map[string]string{"Endpoint": r.Endpoint, "Model": r.Model, "Agent": r.Agent, "Provenance": r.Provenance} {
			if got != overflowLabel {
				t.Errorf("overflow row %s = %q, want %q", name, got, overflowLabel)
			}
		}
	}
	if overflowRows != 1 {
		t.Errorf("got %d overflow rows, want 1", overflowRows)
	}
	if len(rows) > maxLabelsPerMinute {
		t.Errorf("got %d rows, want at most %d", len(rows), maxLabelsPerMinute)
	}
	if want := int64(sent) * 250_000; total != want {
		t.Errorf("CostMicros = %d, want %d: the cap must never cost dollars", total, want)
	}
}

// agent= narrows the rows before group=project breaks them down.
func TestSnapshot_AgentAndGroupProject(t *testing.T) {
	now := at
	w := newTestWriter(t, t.TempDir(), func() time.Time { return now })
	w.Record("s1", projectEvent(t, at, "/repo/a", 0.25))
	w.Record("s1", projectEvent(t, at, "/repo/b", 0.25))
	bob := projectEvent(t, at, "/repo/c", 0.25)
	bob.Client = &pipeline.EventClient{Name: "bob", Version: "1.0"}
	w.Record("s1", bob)
	spec := usage.Spec{Label: usage.WindowToday, From: usage.StartOfLocalDay(at), To: at.Add(time.Minute)}
	snap, err := w.Snapshot(context.Background(), spec, usage.GroupProject, "claude-code")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got, want := seriesKeys(snap.Buckets[0].Series), []string{"/repo/a", "/repo/b"}; !slices.Equal(got, want) {
		t.Errorf("series = %v, want %v", got, want)
	}
}

// One project billed in two units names both, so a per-row figure can say it is mixed.
func TestSeriesCurrenciesIn_GroupProject(t *testing.T) {
	rows := []Row{
		{Endpoint: "bob", Project: "/repo/a", Currency: "credits", Counts: usage.Counts{Requests: 1, PricedRequests: 1}},
		{Endpoint: "anthropic", Project: "/repo/a", Counts: usage.Counts{Requests: 1, PricedRequests: 1}},
		{Endpoint: "anthropic", Counts: usage.Counts{Requests: 1, PricedRequests: 1}},
	}
	got := SeriesCurrenciesIn(rows, usage.GroupProject, foldSeries(rows, usage.GroupProject))
	if want := []string{"USD", "credits"}; !slices.Equal(got["/repo/a"], want) {
		t.Errorf("/repo/a = %v, want %v", got["/repo/a"], want)
	}
	if want := []string{"USD"}; !slices.Equal(got[usage.UnknownProjectLabel], want) {
		t.Errorf("unknown = %v, want %v", got[usage.UnknownProjectLabel], want)
	}
}

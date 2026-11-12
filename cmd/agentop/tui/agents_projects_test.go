package tui

import (
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
)

func TestProjectDisplayNames(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"~/works/cortex", "~/moca"}, []string{"cortex", "moca"}},
		{[]string{"~"}, []string{"~"}},
		{[]string{"~/works/go/src/github.com/kagenti/kagenti-extensions", "~/git/kagenti/kagenti-extensions"},
			[]string{"github.com/kagenti/kagenti-extensions", "git/kagenti/kagenti-extensions"}},
		{[]string{"~/a/b", "~/x/a/b"}, []string{"~/a/b", "x/a/b"}},
		{[]string{"/srv/app", "~/app", "~/tools"}, []string{"srv/app", "~/app", "tools"}},
	}
	for _, c := range cases {
		if got := projectDisplayNames(c.in); !slices.Equal(got, c.want) {
			t.Errorf("projectDisplayNames(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func projectCounts(micros int64) usage.Counts {
	return usage.Counts{CostMicros: micros, PricedRequests: 1}
}

func TestProjectRowsFromSnapshot_FoldsUnknownAndOverflowIntoOther(t *testing.T) {
	snap := &usage.Snapshot{Buckets: []usage.Bucket{
		{Series: map[string]usage.Counts{
			"~/works/cortex":          projectCounts(90_000_000),
			"~/moca":                  projectCounts(38_110_000),
			usage.UnknownProjectLabel: projectCounts(10_000_000),
		}},
		{Series: map[string]usage.Counts{
			"~/works/cortex":     projectCounts(7_400_000),
			projectOverflowLabel: projectCounts(5_520_000),
			"~":                  projectCounts(1_000),
		}},
	}}
	rows := projectRowsFromSnapshot(snap)
	var labels []string
	for _, r := range rows {
		labels = append(labels, r.label)
	}
	if want := []string{"~/works/cortex", "~/moca", "~", otherProjects}; !slices.Equal(labels, want) {
		t.Fatalf("labels = %q, want %q", labels, want)
	}
	if got := rows[0].CostMicros; got != 97_400_000 {
		t.Errorf("~/works/cortex CostMicros = %d, want 97400000", got)
	}
	if got := rows[3].CostMicros; got != 15_520_000 {
		t.Errorf("Other CostMicros = %d, want 15520000", got)
	}
}

func TestProjectRowsFromSnapshot_UnitsFollowTheSeries(t *testing.T) {
	snap := &usage.Snapshot{
		Currencies:       []string{"USD", "credits"},
		SeriesCurrencies: map[string][]string{"~/moca": {"credits"}},
		Buckets: []usage.Bucket{{Series: map[string]usage.Counts{
			"~/moca":         projectCounts(2_000_000),
			"~/works/cortex": projectCounts(1_000_000),
		}}},
	}
	for _, r := range projectRowsFromSnapshot(snap) {
		want := snap.Currencies
		if r.label == "~/moca" {
			want = []string{"credits"}
		}
		if !slices.Equal(r.units, want) {
			t.Errorf("row %q units = %q, want %q", r.label, r.units, want)
		}
	}
}

package tui

import (
	"strings"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// otherProjects is the project sub-row for spend that no named project holds. It is the same
// string as otherAgents, so agentRowsFromBuckets puts it last. A project label is a path ("/…" or
// "~…"), so no real project has this name.
const otherProjects = otherAgents

// projectOverflowLabel is the server's series for the overflow past MaxSeriesInResponse. It
// mirrors the unexported overflowLabel in core/cost/usage/labels.go.
const projectOverflowLabel = "(other)"

// namedProject reports whether a group=project series is one project, not the unknown or the
// overflow series. "~" is a named project.
func namedProject(label string) bool {
	return label != usage.UnknownProjectLabel && label != projectOverflowLabel
}

// projectRowsFromSnapshot gives one row per project of a group=project snapshot, in
// usage.SortSeriesLabels order, with the unknown and overflow series merged into otherProjects,
// which is last.
func projectRowsFromSnapshot(snap *usage.Snapshot) []agentRow {
	return seriesRowsFromSnapshot(foldSeriesInto(snap, namedProject, otherProjects))
}

// projectDisplayNames gives the name to show for each project label, in the same order: the
// shortest tail of "/"-separated segments that no other label has as its tail of the same segment
// count. When no tail is unique (a duplicate label), it is the full label.
func projectDisplayNames(labels []string) []string {
	segs := make([][]string, len(labels))
	for i, l := range labels {
		segs[i] = strings.Split(l, "/")
	}
	tail := func(s []string, k int) string { return strings.Join(s[len(s)-k:], "/") }
	out := make([]string, len(labels))
	for i, s := range segs {
		out[i] = labels[i]
		for k := 1; k <= len(s); k++ {
			t := tail(s, k)
			unique := true
			for j, o := range segs {
				if j != i && len(o) >= k && tail(o, k) == t {
					unique = false
					break
				}
			}
			if unique {
				out[i] = t
				break
			}
		}
	}
	return out
}

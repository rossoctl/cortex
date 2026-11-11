package tui

import (
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/session/timeline"
)

// endsRunTools are the tools that end the run by design when they get no result. The
// source is core/session/timeline/agents.go. Do not add other names.
var endsRunTools = map[string]bool{"StructuredOutput": true, "SubagentHandback": true}

// toolRow is one tool call of the timeline, with the figures its events-table row shows.
type toolRow struct {
	timeline.Tool
	Parallel bool       // another tool of the same response has the same End
	EndsRun  bool       // Lost and Name is StructuredOutput or SubagentHandback
	RunFrom  *time.Time // SpawnedAt of the agent whose SpawnTool is this tool's ID
	RunTo    *time.Time // its ReturnedAt; nil while the subagent runs
	BgFrom   *time.Time // Start of this Bash tool's job (fact 4)
	BgTo     *time.Time // the job's End; nil while it runs
}

// jobKey finds a Bash tool's job: by its tool ID, or, from an older proxy that sends no
// ToolUseID, by the start time. Two calls whose results share one request share that time.
type jobKey struct {
	agent string
	tool  string
	start int64
}

// indexTools groups the tools of tl by the seq of the response that asked for them, in
// timeline order. It returns nil for a nil tl.
func indexTools(tl *timeline.Timeline) map[uint64][]toolRow {
	if tl == nil {
		return nil
	}
	spawned := make(map[string]timeline.Agent, len(tl.Agents))
	for _, a := range tl.Agents {
		if a.SpawnTool != "" {
			spawned[a.SpawnTool] = a
		}
	}
	jobs := make(map[jobKey]timeline.Job, len(tl.Jobs))
	for _, j := range tl.Jobs {
		k := jobKey{agent: j.Agent, tool: j.ToolUseID}
		if j.ToolUseID == "" {
			k = jobKey{agent: j.Agent, start: j.Start.UnixNano()}
		}
		if _, dup := jobs[k]; !dup {
			jobs[k] = j
		}
	}
	type endKey struct {
		seq uint64
		end int64
	}
	ends := map[endKey]int{}
	for _, t := range tl.Tools {
		if t.End != nil {
			ends[endKey{t.Seq, t.End.UnixNano()}]++
		}
	}

	idx := make(map[uint64][]toolRow)
	for _, t := range tl.Tools {
		r := toolRow{Tool: t, EndsRun: t.Lost && endsRunTools[t.Name]}
		if t.End != nil {
			r.Parallel = ends[endKey{t.Seq, t.End.UnixNano()}] > 1
			if t.Name == "Bash" {
				j, ok := jobs[jobKey{agent: t.Agent, tool: t.ID}]
				if !ok {
					j, ok = jobs[jobKey{agent: t.Agent, start: t.End.UnixNano()}]
				}
				if ok {
					r.BgFrom, r.BgTo = &j.Start, j.End
				}
			}
		}
		if a, ok := spawned[t.ID]; ok && a.SpawnedAt != nil {
			r.RunFrom, r.RunTo = a.SpawnedAt, a.ReturnedAt
		}
		idx[t.Seq] = append(idx[t.Seq], r)
	}
	return idx
}

// status is the STATUS cell (rule 7).
func (t toolRow) status() string {
	switch {
	case t.End == nil:
		return "running"
	case t.Error:
		return "error"
	case t.EndsRun:
		return ""
	case t.Lost:
		return "lost"
	}
	return "ok"
}

// duration is the DURATION sort value (rule 11).
func (t toolRow) duration(now time.Time) time.Duration {
	switch {
	case t.End == nil:
		return now.Sub(t.Start)
	case t.Lost:
		return 0
	}
	return t.End.Sub(t.Start)
}

// durationCell is the DURATION cell (rule 8).
func (t toolRow) durationCell(now time.Time) string {
	switch {
	case t.End == nil:
		return "⋯ " + formatDuration(now.Sub(t.Start))
	case t.Lost:
		return ""
	}
	s := formatDuration(t.End.Sub(t.Start))
	if t.Parallel && s != "" {
		s = "∥" + s
	}
	return s
}

// trailing is the cell after DURATION (rule 10): run, bg, ends run and the preview, joined
// with " · ".
func (t toolRow) trailing(now time.Time) string {
	parts := make([]string, 0, 4)
	if t.RunFrom != nil {
		parts = append(parts, "run "+span(*t.RunFrom, t.RunTo, now))
	}
	if t.BgFrom != nil {
		parts = append(parts, "bg "+span(*t.BgFrom, t.BgTo, now))
	}
	if t.EndsRun {
		parts = append(parts, "ends run")
	}
	if t.Preview != "" {
		parts = append(parts, t.Preview)
	}
	return strings.Join(parts, " · ")
}

// span formats to−from, or "⋯ <now−from>" while to is nil.
func span(from time.Time, to *time.Time, now time.Time) string {
	if to == nil {
		return "⋯ " + formatDuration(now.Sub(from))
	}
	return formatDuration(to.Sub(from))
}

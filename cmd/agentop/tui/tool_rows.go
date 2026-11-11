package tui

import (
	"sort"
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
	Pos      int        // position among the tools of its response; tells apart two calls with no ID
	BgFrom   *time.Time // Start of this Bash tool's job (fact 4)
	BgTo     *time.Time // the job's End; nil while it runs
}

// jobKey finds a Bash tool's job from an older proxy, which sends no ToolUseID: its agent and
// start, which is the tool's end. Two calls whose results share one request share that time.
type jobKey struct {
	agent string
	start int64
}

// indexTools groups the tools of tl by the seq of the response that asked for them, in
// timeline order. It returns nil for a nil tl.
func indexTools(tl *timeline.Timeline) map[uint64][]toolRow {
	if tl == nil {
		return nil
	}
	// A reused ID (call_0) can spawn many subagents. Build sets SpawnedAt to at most its call's
	// Start, so each subagent, earliest first, takes the first free call of its key from then on.
	type spawnKey struct{ parent, id string }
	calls := map[spawnKey][]int{}
	for i, t := range tl.Tools {
		if t.ID != "" && (t.Name == "Agent" || t.Name == "Task") {
			k := spawnKey{t.Agent, t.ID}
			calls[k] = append(calls[k], i)
		}
	}
	var agents []timeline.Agent
	for _, a := range tl.Agents {
		if a.SpawnTool != "" && a.SpawnedAt != nil {
			agents = append(agents, a)
		}
	}
	sort.SliceStable(agents, func(i, j int) bool { return agents[i].SpawnedAt.Before(*agents[j].SpawnedAt) })
	runOf := map[int]timeline.Agent{}
	for _, a := range agents {
		first := -1 // the first free call; it takes the subagent when no free call starts from then on
		for _, i := range calls[spawnKey{a.Parent, a.SpawnTool}] {
			if _, taken := runOf[i]; taken {
				continue
			}
			if first < 0 {
				first = i
			}
			if !tl.Tools[i].Start.Before(*a.SpawnedAt) {
				first = i
				break
			}
		}
		if first >= 0 {
			runOf[first] = a
		}
	}
	// Build ends a call at the request carrying its first result, clamped up to the call's
	// Start, and starts the job at that request. So a job's call has End = max(job Start, Start).
	// Build keeps only the first result of an ID, so with a reused ID (call_0) the first such
	// call owns the job. The request can arrive before the asking response ends, so its Seq can
	// be lower than the call's: Seq does not order them.
	byTool := map[int]timeline.Job{}
	legacy := map[jobKey]timeline.Job{}
	for _, j := range tl.Jobs {
		if j.ToolUseID == "" {
			k := jobKey{j.Agent, j.Start.UnixNano()}
			if _, dup := legacy[k]; !dup {
				legacy[k] = j
			}
			continue
		}
		for i, t := range tl.Tools {
			if t.Agent != j.Agent || t.ID != j.ToolUseID || t.Name != "Bash" || t.End == nil {
				continue
			}
			end := j.Start
			if end.Before(t.Start) {
				end = t.Start
			}
			if _, dup := byTool[i]; t.End.Equal(end) && !dup {
				byTool[i] = j
				break
			}
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
	for i, t := range tl.Tools {
		r := toolRow{Tool: t, EndsRun: t.Lost && endsRunTools[t.Name], Pos: len(idx[t.Seq])}
		if t.End != nil {
			r.Parallel = ends[endKey{t.Seq, t.End.UnixNano()}] > 1
			if t.Name == "Bash" {
				j, ok := byTool[i]
				if !ok {
					j, ok = legacy[jobKey{t.Agent, t.End.UnixNano()}]
				}
				if ok {
					r.BgFrom, r.BgTo = &j.Start, j.End
				}
			}
		}
		if a, ok := runOf[i]; ok {
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

// trailing is the cell after DURATION (rule 10): run, bg and ends run, joined with " · ".
// The preview (a command and its arguments) is not in it: it made the table hard to read.
// Search finds it in the row's dropped text, and Enter opens the full call.
func (t toolRow) trailing(now time.Time) string {
	parts := make([]string, 0, 3)
	if t.RunFrom != nil {
		parts = append(parts, "run "+span(*t.RunFrom, t.RunTo, now))
	}
	if t.BgFrom != nil {
		parts = append(parts, "bg "+span(*t.BgFrom, t.BgTo, now))
	}
	if t.EndsRun {
		parts = append(parts, "ends run")
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

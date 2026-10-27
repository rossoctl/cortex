package sessionapi

import (
	"cmp"
	"log/slog"
	"slices"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// withArchivedEvents is the one merge rule for pages: take what the store holds, newest first up
// to the limit — which ViewPage already did.
//
// ONE RULE COVERS EVERY CASE. A resident session with nothing on disk never reaches the archive's
// events; one resumed after a restart is a few events in memory over a history on disk; one
// paged past its resident tail is answered from disk alone; and one that is only on disk has a
// nil view from the store.
//
// totalEvents and oldestSeq follow the store's rule — present only when the response is not the
// whole session — over the merged history. oldestSeq is the archive's oldest retained segment
// when it reaches further back than memory; totalEvents is at least what either side holds, which
// is what a client uses it for: telling "there is more behind me" from "this is the beginning".
func (s *Server) withArchivedEvents(id string, before uint64, limit int, view *pipeline.SessionView) *pipeline.SessionView {
	return mergeArchived(id, before, limit, view, func(cursor uint64, need int) ([]pipeline.SessionEvent, archive.PageInfo, bool, bool) {
		return s.readArchive(id, cursor, need)
	})
}

// archivePage returns up to need of a session's archived events numbered below cursor, oldest
// first. ok is false when the archive holds nothing under the id; failed reports a read that
// stopped early.
type archivePage func(cursor uint64, need int) (events []pipeline.SessionEvent, info archive.PageInfo, ok, failed bool)

func (s *Server) readArchive(id string, cursor uint64, need int) ([]pipeline.SessionEvent, archive.PageInfo, bool, bool) {
	var older []pipeline.SessionEvent
	info, ok, err := s.archive.Page(id, cursor, max(need, 0), func(e *pipeline.SessionEvent) bool {
		older = append(older, *e)
		return true
	})
	if err != nil {
		slog.Debug("sessionapi: archive page read failed; serving memory alone", "error", err, "sessionID", id)
	}
	slices.Reverse(older)
	return older, info, ok, err != nil
}

func mergeArchived(id string, before uint64, limit int, view *pipeline.SessionView, page archivePage) *pipeline.SessionView {
	cursor, need, memTotal, memOldest := before, limit, 0, uint64(0)
	if view != nil {
		memTotal = view.TotalEvents
		if memTotal == 0 {
			memTotal = len(view.Events)
		}
		if len(view.Events) > 0 {
			memOldest = view.OldestSeq
			if memOldest == 0 {
				memOldest = view.Events[0].Seq
			}
			if contiguousBelow(view.Events, before) {
				cursor, need = view.Events[0].Seq, limit-len(view.Events)
			}
		}
	}
	older, info, ok, _ := page(cursor, need)
	if !ok {
		return view
	}
	if view == nil {
		view = &pipeline.SessionView{ID: id}
	}
	view.Events = mergeBySeq(older, view.Events, limit)

	oldest := info.OldestSeq
	if memOldest != 0 && memOldest < oldest {
		oldest = memOldest
	}
	if before == 0 && len(view.Events) > 0 && view.Events[0].Seq <= oldest {
		view.TotalEvents, view.OldestSeq = 0, 0 // the whole session
	} else {
		view.TotalEvents = max(info.TotalEvents, memTotal, len(view.Events))
		view.OldestSeq = oldest
	}
	return view
}

func contiguousBelow(events []pipeline.SessionEvent, before uint64) bool {
	for i := 1; i < len(events); i++ {
		if events[i].Seq != events[i-1].Seq+1 {
			return false
		}
	}
	return before == 0 || events[len(events)-1].Seq+1 == before
}

func mergeBySeq(disk, mem []pipeline.SessionEvent, limit int) []pipeline.SessionEvent {
	out := make([]pipeline.SessionEvent, 0, len(disk)+len(mem))
	i, j := 0, 0
	for i < len(disk) || j < len(mem) {
		switch {
		case j == len(mem) || (i < len(disk) && disk[i].Seq < mem[j].Seq):
			out = append(out, disk[i])
			i++
		case i < len(disk) && disk[i].Seq == mem[j].Seq:
			i++
		default:
			out = append(out, mem[j])
			j++
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// withArchivedRows adds to the store's list every archived session it does not hold, marked
// resident: false, and reports the archive's usage. A session in both is listed once, as its
// resident row: the store knows what only it can — whether it is active, which agent claimed it —
// and that row already carries the archive's earlier history (session.PriorKeeper).
func (s *Server) withArchivedRows(resident []session.SessionSummary) ([]session.SessionSummary, *pipeline.ArchiveUsage) {
	held := make(map[string]bool, len(resident))
	for _, r := range resident {
		held[r.ID] = true
	}
	notResident := false
	rows := resident
	for _, a := range s.archive.Summaries() {
		if held[a.ID] {
			continue
		}
		a.Resident = &notResident
		rows = append(rows, a)
	}
	slices.SortStableFunc(rows, func(x, y session.SessionSummary) int {
		return cmp.Compare(y.UpdatedAt.UnixNano(), x.UpdatedAt.UnixNano())
	})
	st := s.archive.Stats()
	return rows, &pipeline.ArchiveUsage{
		Bytes:          st.Bytes,
		MaxBytes:       st.MaxBytes,
		RetentionDays:  st.RetentionDays,
		DroppedEvents:  st.DroppedEvents,
		WriteErrors:    st.WriteErrors,
		DroppedRenames: st.DroppedRenames,
		Paused:         st.Paused,
	}
}

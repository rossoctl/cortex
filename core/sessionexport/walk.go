package sessionexport

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// Filter selects what a Walk carries. Its zero value carries every session and every event.
type Filter struct {
	// Sessions holds the ids of the sessions to carry; nil or empty carries every session the
	// archive holds. An id the archive does not hold is an UnknownSessionsError from Walk
	// naming the ids that are there, so a typo fails loudly rather than exporting nothing for
	// it silently.
	Sessions map[string]struct{}

	// Since is the oldest event time to carry; the zero time carries everything. A session
	// whose last update is older than it is skipped whole, and one that straddles it stops at
	// its first event below — see Walk for why stopping, not filtering, is sound.
	Since time.Time
}

// WalkStats is what a walk carried.
type WalkStats struct {
	// Sessions is how many sessions contributed at least one record.
	Sessions int
	// Records is how many records were written: the walk's output line count.
	Records int
}

// UnknownSessionsError is Walk's answer to a Filter naming session ids the archive does not
// hold. Requested is those ids; Known is every id the archive does hold — so a caller can
// fail a user with the names they could have meant, the way agentop's cost --agent does for
// an unknown agent.
type UnknownSessionsError struct {
	Requested []string
	Known     []string
}

// The rendered message is bounded in BYTES, not in ids, because the HTTP layer forwards it
// as a 400 body and a client bounds what it reads of one (agentop reads 512 bytes): an
// archive holding a year of sessions must not turn a typo into an answer the asker cannot
// parse. A count cap cannot do this job — session ids are client-asserted and bounded only
// by session.MaxSessionIDLen (256), so five requested ids and eight known ones render 538
// bytes at plain UUID length, and a single maximum-length id overruns the budget alone.
// The struct's fields stay complete: a programmatic caller gets every id; a printed one
// gets as many as fit and a count of the rest.
//
// The budget is the whole message, split between the two lists so that neither can starve
// the other, and sized well under 512 to leave room for the `{"error":...}` the HTTP layer
// wraps it in and for any JSON escaping of an id's own bytes.
const (
	errorMsgBudget      = 400
	errorListIDMaxLen   = 64 // one long id is truncated rather than spending the whole budget
	errorRequestedShare = 2  // the requested ids get half the budget; they are what was asked
)

func (e *UnknownSessionsError) Error() string {
	// The requested ids are rendered first and to half the budget: they are the asker's own
	// words, so they matter more than the suggestions. Whatever half they leave unspent is
	// handed to the known list rather than wasted.
	req := cappedList(e.Requested, errorMsgBudget/errorRequestedShare)
	known := cappedList(e.Known, errorMsgBudget-len(req))
	return fmt.Sprintf("sessionexport: no archived session %s (archived: %s)", req, known)
}

// cappedList renders ids for an error message under a byte budget: as many as fit verbatim,
// then a count of the rest — "a, b and 3 more" rather than a list that grows with the
// archive. An id longer than errorListIDMaxLen is truncated with an ellipsis, so that one
// pathological id neither hides every other nor blows the budget by itself.
//
// At least one id is always rendered, however long it is: "and 12 more" alone names nothing
// the asker can act on, and a truncated id is still a recognisable prefix.
func cappedList(ids []string, budget int) string {
	var b strings.Builder
	for i, id := range ids {
		if len(id) > errorListIDMaxLen {
			id = id[:errorListIDMaxLen] + "…"
		}
		// The separator and the "and N more" tail this id would force are part of its cost:
		// a list must not fit its last id only to overrun on the tail that follows it.
		cost := len(id)
		if i > 0 {
			cost += len(", ")
		}
		if i > 0 && b.Len()+cost+len(fmt.Sprintf(" and %d more", len(ids)-i)) > budget {
			fmt.Fprintf(&b, " and %d more", len(ids)-i)
			return b.String()
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(id)
	}
	return b.String()
}

// Walk projects every archived event the filter admits into w, one session at a time, and
// reports what it carried.
//
// THE DURABLE ARCHIVE ONLY, NOT THE STORE. A walk is a backfill: it reads what a previous
// process or an earlier moment of this one flushed to disk, and a session with nothing
// flushed yet does not appear. The open segment reads up to its last flush, at most a tick
// behind — the same gap Page documents, and the reason an export is not a consistent
// snapshot of a live session's tail. Merging memory in is what the session API's pages are
// for; an export that tried would race the writer for the newest events and still not be
// consistent.
//
// ORDER, FOR A CONSUMER THAT NEEDS ONE: sessions most recently updated first, events newest
// first within a session. Neither is the order a timeline wants, and a consumer sorts by
// `at` then `seq` — the rule Record's own docs state — rather than either being promised
// here. The orders are named only so a consumer knows what it is looking at mid-walk.
//
// ERRORS COME BEFORE RECORDS WHERE THEY CAN. A filter naming unknown sessions is rejected
// before the first record is written, so a caller streaming a response can still answer a
// proper 400. A read error past that point leaves a truncated NDJSON file whose every
// complete line is still valid — Writer's stated design property — and the stats report
// what was carried up to it.
func Walk(a *archive.Archive, w *Writer, f Filter) (WalkStats, error) {
	summaries := a.Summaries()
	var carry []session.SessionSummary
	if len(f.Sessions) == 0 {
		carry = summaries
	} else {
		// CLONED, NOT ALIASED: the matching loop below deletes what it finds, and deleting
		// from the caller's map would leave a reused Filter naming every session — the one
		// silent widening this package must not do. The clone is the whole defence; nothing
		// else between here and the delete touches f.
		want := maps.Clone(f.Sessions)
		var missing []string
		for _, s := range summaries {
			if _, ok := want[s.ID]; ok {
				carry = append(carry, s)
				delete(want, s.ID)
			}
		}
		for id := range want {
			missing = append(missing, id)
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			known := make([]string, 0, len(summaries))
			for _, s := range summaries {
				known = append(known, s.ID)
			}
			slices.Sort(known)
			return WalkStats{}, &UnknownSessionsError{Requested: missing, Known: known}
		}
	}

	var stats WalkStats
	for _, s := range carry {
		// A session the archive last touched before the floor cannot hold an event above it:
		// UpdatedAt is when its newest event landed. Skipping whole sessions is what keeps a
		// bounded since from paying to decode every old segment in the archive.
		if !f.Since.IsZero() && s.UpdatedAt.Before(f.Since) {
			continue
		}
		wrote, writeErr := 0, error(nil)
		err := a.Earlier(s.ID, 0, func(e *model.SessionEvent) bool {
			// STOP, NOT FILTER: Earlier walks newest first, and a session's events are
			// appended in time order — approximately. At is stamped when an event is built
			// and Append takes the store lock later, so two concurrent requests in one
			// session can land with seq and At inverted by that build→append gap; the floor
			// is therefore fuzzy by it, which for a relative since means the odd event
			// within microseconds of the floor lands on the wrong side. The first event
			// below the floor is still where every later one is below it too, and the
			// alternative — filtering without stopping — costs decoding the whole session
			// to carry its tail, which is the one thing a since exists to avoid.
			if !f.Since.IsZero() && e.At.Before(f.Since) {
				return false
			}
			if err := w.WriteEvent(e, s.ID); err != nil {
				writeErr = err
				return false
			}
			wrote++
			return true
		})
		// Folded before the error checks, not after: the docs promise the stats report
		// what was carried up to a failure, and a read error on a session's second page
		// must not erase the 500 lines its first page already wrote.
		if wrote > 0 {
			stats.Sessions++
			stats.Records += wrote
		}
		if err != nil {
			return stats, err
		}
		if writeErr != nil {
			return stats, writeErr
		}
	}
	return stats, nil
}

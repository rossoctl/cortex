package sessionexport

import (
	"fmt"
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

func (e *UnknownSessionsError) Error() string {
	return fmt.Sprintf("sessionexport: no archived session %s (archived: %s)",
		strings.Join(e.Requested, ", "), strings.Join(e.Known, ", "))
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
		want := f.Sessions
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
			// appended in time order — a bridged tunnel open is stamped with the request it
			// precedes, so at worst two events share an instant. At falls with seq, up to
			// those same-instant ties, and the floor compares instants rather than positions,
			// so the first event below it is where every later one is below it too. The
			// alternative — filtering without stopping — costs decoding the whole session to
			// carry its tail, which is the one thing a since exists to avoid.
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
		if err != nil {
			return stats, err
		}
		if writeErr != nil {
			return stats, writeErr
		}
		if wrote > 0 {
			stats.Sessions++
			stats.Records += wrote
		}
	}
	return stats, nil
}

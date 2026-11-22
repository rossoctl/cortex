package sessionexport

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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

	// ToolNameKey labels a hashed tool name on toolCall records. Read only when the Writer
	// carries them; a zero key leaves a hashed name ABSENT rather than exporting it in clear
	// (see ToolNameKey.label), so a misconfigured install loses a label instead of leaking one.
	ToolNameKey ToolNameKey
}

// WalkStats is what a walk carried.
type WalkStats struct {
	// Sessions is how many sessions contributed at least one record.
	Sessions int
	// Records is how many records were written: the walk's output line count, every kind
	// included.
	Records int
	// ToolCalls is how many of Records were toolCall records, and Truncated how many sessions
	// hit maxCallsPerSession and so carry a marker record. Both are zero unless the Writer
	// carries toolCall records.
	//
	// Truncated IS WORTH A FIELD rather than leaving the marker records to be counted by a
	// reader: a caller that logs or summarises a walk should be able to say "this export was
	// incomplete for N sessions" without parsing its own output back.
	ToolCalls int
	Truncated int
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
// the other, and sized under 512 to leave room for the `{"error":...}` the HTTP layer wraps
// it in and for the fixed text around the two lists.
//
// EVERY WIDTH HERE IS THE JSON-ESCAPED ONE, because the message is sent inside a JSON
// string: the handler marshals it (sessionapi/export.go). Session ids are client-asserted
// and only have to be printable (session.validHeaderSessionID), so an id may hold bytes
// that cost more encoded than they do raw — `<`, `>` and `&` are HTML-escaped to 6 bytes
// each by encoding/json's default, as are U+2028 and U+2029, and a control rune becomes a
// 6-byte \u00XX. Charging raw len() would make a 64-byte id cost 384 bytes on the wire, so
// a budget counted raw is not a budget on what a client reads. One `&` in a client's own
// ids is enough: without escaped accounting, every typo against that archive renders a 400
// too long for agentop to parse.
//
// errorMsgBudget is that escaped width for the whole message. 400 left only ten bytes under
// a 512-byte client read once both lists were charged at their escaped width, which is too
// thin to absorb a change to the fixed text; 360 keeps about 50.
const (
	errorMsgBudget      = 360
	errorListIDMaxLen   = 64 // one long id is truncated rather than spending the whole budget
	errorRequestedShare = 2  // the requested ids get half the budget; they are what was asked
)

// escapedLen is what s costs inside the JSON string the HTTP layer wraps the message in:
// json.Marshal's output minus the two quotes it adds. This, not len(s), is the width every
// budget below is counted in.
func escapedLen(s string) int {
	b, err := json.Marshal(s)
	if err != nil {
		// Marshalling a string fails only on invalid UTF-8, which encoding/json replaces
		// rather than rejecting; charge the worst case instead of under-counting.
		return len(s) * 6
	}
	return len(b) - 2
}

// truncateToEscaped cuts s so that it costs at most max ESCAPED bytes, appending an ellipsis
// when it cuts. Truncating by raw length would not bound the cost: errorListIDMaxLen bytes of
// `<` escape to six times that. It cuts only on rune boundaries, because a byte slice through
// a multi-byte rune leaves dangling bytes that json.Marshal renders as U+FFFD — an ugly
// prefix, and 3 bytes the cut did not mean to spend. Non-ASCII ids are deliberately allowed,
// so that case is reachable.
//
// This is what makes the "at least one id is always rendered" rule below safe: the one id a
// list is obliged to render can no longer overrun the whole budget by itself.
func truncateToEscaped(s string, max int) string {
	if escapedLen(s) <= max {
		return s
	}
	// Grow a prefix rune by rune while it and the ellipsis still fit. Linear in runes and
	// bounded by max, so it costs nothing worth optimising at these sizes.
	const ellipsis = "…"
	room := max - escapedLen(ellipsis)
	end := 0
	for end < len(s) {
		_, size := utf8.DecodeRuneInString(s[end:])
		if escapedLen(s[:end+size]) > room {
			break
		}
		end += size
	}
	return s[:end] + ellipsis
}

func (e *UnknownSessionsError) Error() string {
	// The fixed text is part of the message, so it comes out of the budget before either
	// list gets a share — otherwise two full lists plus this frame can still overrun.
	const frame = "sessionexport: no archived session  (archived: )"
	budget := errorMsgBudget - len(frame)
	// The requested ids are rendered first and to half the budget: they are the asker's own
	// words, so they matter more than the suggestions. Whatever half they leave unspent is
	// handed to the known list rather than wasted.
	req := cappedList(e.Requested, budget/errorRequestedShare)
	known := cappedList(e.Known, budget-escapedLen(req))
	return fmt.Sprintf("sessionexport: no archived session %s (archived: %s)", req, known)
}

// cappedList renders ids for an error message under a byte budget: as many as fit verbatim,
// then a count of the rest — "a, b and 3 more" rather than a list that grows with the
// archive. An id longer than errorListIDMaxLen is truncated with an ellipsis, so that one
// pathological id neither hides every other nor blows the budget by itself.
//
// The budget is spent in ESCAPED bytes — see the note on errorMsgBudget — so an id of bytes
// that expand under json.Marshal buys proportionally less of the list.
//
// At least one id is always rendered, however long it is: "and 12 more" alone names nothing
// the asker can act on, and a truncated id is still a recognisable prefix.
func cappedList(ids []string, budget int) string {
	var b strings.Builder
	// used tracks the escaped width of what b holds; b.Len() is the raw width, which is not
	// what the budget is counted in.
	used := 0
	for i, id := range ids {
		id = truncateToEscaped(id, errorListIDMaxLen)
		// The separator and the "and N more" tail this id would force are part of its cost:
		// a list must not fit its last id only to overrun on the tail that follows it.
		cost := escapedLen(id)
		if i > 0 {
			cost += len(", ")
		}
		if i > 0 && used+cost+len(fmt.Sprintf(" and %d more", len(ids)-i)) > budget {
			fmt.Fprintf(&b, " and %d more", len(ids)-i)
			return b.String()
		}
		if i > 0 {
			b.WriteString(", ")
			used += len(", ")
		}
		b.WriteString(id)
		used += escapedLen(id)
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
		// paired accumulates this session's events when the Writer wants toolCall records, and
		// stays nil otherwise so a plain walk keeps its O(1)-per-event memory.
		//
		// WHY ACCUMULATE AT ALL, against this function's whole streaming design: a call's result
		// arrives in a LATER request, so toolpair.Calls needs every event of a session in one
		// slice and cannot run inside this callback. Measured 2026-11-22 on a 75-session,
		// 44,325-event archive: pairing the deepest session cost 7 ms against the 1.45 s the
		// archive read already costs — 0.45% — and peak heap moved 279 to 319 MiB. Holding one
		// session is also what the timeline already pays per pane refresh.
		//
		// BOUNDED IN EVENTS, NOT ONLY IN RECORDS. maxCallsPerSession caps what is WRITTEN, which
		// it can only do after the accumulation is already complete — so on its own it bounds the
		// output and not the memory, and a session far longer than any observed here would grow
		// this slice without limit. maxPairedEvents is the memory bound, and it is the one §6.4
		// actually asks for.
		//
		// THE NEWEST EVENTS ARE THE ONES KEPT, which is why a bound is sound rather than merely
		// cheap: Earlier walks NEWEST FIRST, so the events this drops are the session's oldest.
		// A dropped prefix costs the calls whose asking response is in it — the pairing sees
		// their results with no call to close, and emits nothing for them, exactly as it does for
		// a since floor that cuts mid-session (T33). It never mislabels a call it does emit.
		var paired []model.SessionEvent
		droppedEvents := 0
		if w.WantsToolCalls() {
			paired = make([]model.SessionEvent, 0, min(maxPairedEvents, 256))
		}
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
			if paired != nil {
				if len(paired) >= maxPairedEvents {
					// Counted, not silently skipped: the marker record below reports it, so a
					// capped session cannot read as a complete one. The event records themselves
					// are unaffected — they are already written above, and this bound applies
					// only to what the pairing holds.
					droppedEvents++
				} else {
					// The VALUE, not the pointer: Earlier pages 500 events at a time, so holding
					// pointers would keep each page's decoded events alive for the whole session
					// and make the retained set the pointer graph rather than the slice.
					paired = append(paired, *e)
				}
			}
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
		if len(paired) == 0 {
			continue
		}
		// ASCENDING SEQ, which toolpair.Calls requires ("events must be one session's, in seq
		// order") and which Earlier does not give: it walks NEWEST FIRST. Reversing is not a
		// tidiness step — a descending slice pairs nothing, because every result precedes the
		// call it closes, so each call closes as still-open and T17 then drops every record.
		// It fails silently and completely, which is why the tests assert the order rather
		// than the output alone.
		slices.Reverse(paired)
		recs, dropped := projectToolCalls(paired, s.ID, f.ToolNameKey)
		for _, r := range recs {
			if err := w.WriteToolCall(r); err != nil {
				return stats, err
			}
			stats.Records++
			stats.ToolCalls++
		}
		if dropped > 0 || droppedEvents > 0 {
			// §6.4: say so in band. A consumer must not read a capped session as a complete one.
			// BOTH bounds report here, because either one makes the session's tool time a
			// lower bound rather than a total.
			if err := w.WriteMarker(markerRecord(s.ID, dropped, droppedEvents)); err != nil {
				return stats, err
			}
			stats.Records++
			stats.Truncated++
		}
	}
	return stats, nil
}

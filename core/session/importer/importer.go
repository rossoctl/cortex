package importer

import (
	"context"
	"errors"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/ledger"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// StoppedArchiveFull is AgentReport.Stopped when the archive has no headroom left.
const StoppedArchiveFull = "archive full"

// Options configures Run.
type Options struct {
	Archive *archive.Archive // required
	Store   *session.Store   // the live store; nil in tests
	Ledger  *ledger.Writer   // nil: no ledger rows
	Rates   pricing.Resolver
	Since   time.Time
	DryRun  bool
}

// Run imports the sessions of one source. A session the proxy recorded live is never
// changed. A cancelled ctx stops the run between two sessions.
//
// A session that the archive removed (an erase, a clear, max_bytes or retention_days) is new
// again here, but its ledger rows are not removed with it. When the archive has a forget record
// for the session, toLedger skips each turn in the span of that record. With no record, toLedger
// skips each turn that has a live row near it.
//
// A session older than the archive's retention_days is skipped, so its turns do not reach the
// ledger even when the ledger keeps rows longer.
func Run(ctx context.Context, src Source, o Options) AgentReport {
	r := AgentReport{Agent: src.Agent()}
	mark := model.ImportedSource(src.Agent())
	var dryBytes int64             // the bytes a dry run would have written, for the size check
	sent := map[string]bool{}      // the request ids this run sent to the ledger; see toLedger
	var sum session.SessionSummary // the archived copy of the session that skip last let through
	var found bool
	// skip applies Rules 1 and 2 and the retention check, and counts a skipped session. Sessions
	// calls it before the parse with a time not before Updated, and Run calls it again after.
	skip := func(id string, updated time.Time) bool {
		if err := ctx.Err(); err != nil {
			r.Error = err.Error()
			return true
		}
		if r.Stopped != "" {
			r.NotImported++
			return true
		}
		// Rule 1: a session with live events is not touched.
		if o.Store != nil && o.Store.ViewTail(id, 1) != nil {
			r.SkippedLive++
			return true
		}
		sum, found = o.Archive.Lookup(id)
		if found && sum.Source == "" {
			r.SkippedLive++
			return true
		}
		// Rule 2: the archived copy is as new as the transcript.
		if found && !updated.After(sum.UpdatedAt) {
			r.SkippedCurrent++
			return true
		}
		// The next prune would delete a session past the archive's retention. A zero time is a
		// session with no events: Rule 5 skips it after the parse.
		if d := o.Archive.RetentionDays(); d > 0 && !updated.IsZero() && updated.Before(time.Now().Add(-time.Duration(d)*24*time.Hour)) {
			r.SkippedExpired++
			return true
		}
		return false
	}
	for s, err := range src.Sessions(o.Since, skip) {
		if err != nil {
			r.Error = err.Error()
			break
		}
		if err := ctx.Err(); err != nil {
			r.Error = err.Error()
			break
		}
		r.SkippedLines += s.Skipped
		if s.Unreadable {
			r.Unreadable++
			continue
		}
		if skip(s.ID, s.Updated) {
			continue
		}

		// Rule 5.
		if len(s.Events) == 0 {
			continue
		}
		// Rule 6: keep headroom for live traffic.
		if st := o.Archive.Stats(); st.Bytes+dryBytes+s.SizeHint > st.MaxBytes*9/10 {
			r.Stopped = StoppedArchiveFull
			r.NotImported++
			continue
		}
		if o.DryRun {
			dryBytes += s.SizeHint
			count(&r, found)
			continue
		}

		// The turns of the archived copy, read before the write replaces it.
		var prior *priorTurns
		if found && o.Ledger != nil {
			p, err := readPrior(ctx, o.Archive, s.ID)
			if err != nil {
				r.Error = err.Error()
				break
			}
			prior = p
		}
		for i := range s.Events {
			s.Events[i].Source = mark
			price(&s.Events[i], src.PricingHost(), o.Rates)
		}
		// Rules 3 and 4, and 7.
		if err := o.Archive.WriteSession(s.ID, s.Events, found); err != nil {
			if errors.Is(err, archive.ErrLive) || errors.Is(err, archive.ErrExists) {
				r.SkippedLive++
				continue
			}
			r.Error = err.Error()
			break
		}
		count(&r, found)
		if o.Ledger != nil {
			// shortcut: a turn that an earlier import read while its message was partly written keeps
			// that earlier row, because its request id is in the archived copy. Replace the row if
			// that undercount shows up.
			// shortcut: the archive already holds s, so a re-run skips it as current and its rows stay
			// lost. Only a failed ledger read or append (ENOSPC, EROFS) or a Close during it gets here. Undo the archive write if it shows up.
			var span *forgotSpan
			if !found {
				if f, t, ok := o.Archive.Forgotten(s.ID); ok {
					span = &forgotSpan{f, t}
				}
			}
			if err := toLedger(ctx, o.Ledger, s, prior, sent, span); err != nil {
				r.Error = "cost ledger: " + err.Error()
				break
			}
		}
	}
	return r
}

func count(r *AgentReport, replace bool) {
	if replace {
		r.Replaced++
	} else {
		r.Imported++
	}
}

// priorTurns is what the archived copy of a replaced session holds: the request ids of its
// responses, and the time of its oldest event.
type priorTurns struct {
	ids    map[string]bool
	oldest time.Time
}

// readPrior reads the archived copy of id.
func readPrior(ctx context.Context, a *archive.Archive, id string) (*priorTurns, error) {
	p := &priorTurns{ids: map[string]bool{}}
	_, _, _, err := a.Find(ctx, id, func(e *model.SessionEvent) bool {
		if e.Phase == model.SessionResponse {
			p.ids[e.RequestID] = true
		}
		if p.oldest.IsZero() || e.At.Before(p.oldest) {
			p.oldest = e.At
		}
		return false
	}, nil, func(string) bool { return false })
	return p, err
}

// toLedger records each response of s that is not before the ledger's retention cutoff, and
// whose request id is not in sent. It adds the ids it records to sent: a fork copies its
// parent's turns with their request ids, and one run must count them one time.
//
// On a replace (prior not nil) it records only the responses whose request id is not in prior:
// the ledger already has the turns of the earlier import. It also skips a request before
// prior.oldest, because a prune can delete the oldest segments of the archived copy.
//
// With a forget record (span not nil) it skips each turn whose request is in
// [span.from - 1 minute, span.to]: the ledger counted those turns before the archive removed the
// session. The minute of slack is there because a transcript time comes before the time at
// which the proxy sees the request.
//
// With no record, or before the span, it skips a response when the ledger has a live row for the
// same project within one minute of it: the proxy recorded that turn live.
// shortcut: rows carry no session id, so a turn of a session that did not go through Cortex is
// skipped when another session in the same project had live traffic in that minute. Keep the
// session id on ledger rows if that undercount shows up.
func toLedger(ctx context.Context, w *ledger.Writer, s Session, prior *priorTurns, sent map[string]bool, span *forgotSpan) error {
	cutoff := w.RetentionCutoff()
	requestAt := map[string]time.Time{}
	var turns, after []*model.SessionEvent
	for i := range s.Events {
		e := &s.Events[i]
		switch e.Phase {
		case model.SessionRequest:
			requestAt[e.RequestID] = e.At
		case model.SessionResponse:
			at, ok := requestAt[e.RequestID]
			if !ok || e.At.Before(cutoff) || (e.RequestID != "" && sent[e.RequestID]) {
				continue
			}
			if prior != nil && (prior.ids[e.RequestID] || at.Before(prior.oldest)) {
				continue
			}
			if span != nil && at.After(span.to) {
				after = append(after, e)
				continue
			}
			if span != nil && !at.Before(span.from.Add(-time.Minute)) {
				continue
			}
			turns = append(turns, e)
		}
	}
	if err := record(w, after, sent); err != nil {
		return err
	}
	if len(turns) == 0 {
		return nil
	}
	from, to := turns[0].At, turns[0].At
	for _, e := range turns {
		if e.At.Before(from) {
			from = e.At
		}
		if e.At.After(to) {
			to = e.At
		}
	}
	live := map[liveKey]bool{}
	// The archive holds s already, so a cancel here must not stop the read.
	rows, _, err := w.Window(context.WithoutCancel(ctx), from.Add(-time.Minute), to.Add(time.Minute))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.Source == "" {
			live[liveKey{r.At.Truncate(time.Minute).Unix(), r.Project}] = true
		}
	}
	var keep []*model.SessionEvent
	for _, e := range turns {
		k := turnKey(e)
		m, p := k.minute, k.project
		if live[k] || live[liveKey{m - 60, p}] || live[liveKey{m + 60, p}] {
			continue
		}
		keep = append(keep, e)
	}
	return record(w, keep, sent)
}

// record sends turns to the ledger and adds their request ids to sent.
func record(w *ledger.Writer, turns []*model.SessionEvent, sent map[string]bool) error {
	if len(turns) == 0 {
		return nil
	}
	if err := w.RecordBatch(turns); err != nil {
		return err
	}
	for _, e := range turns {
		if e.RequestID != "" {
			sent[e.RequestID] = true
		}
	}
	return nil
}

// forgotSpan is the span of a forget record: the ledger counted the session's turns in it.
type forgotSpan struct{ from, to time.Time }

// turnKey is the minute and project of e, in the form of the ledger row it makes.
func turnKey(e *model.SessionEvent) liveKey {
	p := model.TruncateLabelTail(model.SanitizeControlRunes(e.Project), event.MaxLabelLen) // as Row.Project
	return liveKey{e.At.Truncate(time.Minute).Unix(), p}
}

type liveKey struct {
	minute  int64
	project string
}

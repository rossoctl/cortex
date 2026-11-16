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
// A live session that the archive pruned (max_bytes or retention_days) is new again here, but
// its ledger rows are not pruned with it. toLedger skips each turn that has a live row near it.
//
// An erased or pruned imported session is new again here too. toLedger skips each of its turns
// that has a transcript row near it that an earlier run wrote.
// shortcut: rows carry no session id, so a new session's turn is skipped when another session in
// the same project was imported by an earlier run in that minute. Keep the session id on ledger
// rows if that undercount shows up.
//
// A session older than the archive's retention_days is skipped, so its turns do not reach the
// ledger even when the ledger keeps rows longer.
func Run(ctx context.Context, src Source, o Options) AgentReport {
	r := AgentReport{Agent: src.Agent()}
	mark := model.ImportedSource(src.Agent())
	var dryBytes int64             // the bytes a dry run would have written, for the size check
	written := map[liveKey]bool{}  // see toLedger
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
			if err := toLedger(ctx, o.Ledger, s, prior, sent, written); err != nil {
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
// It also skips a response when the ledger has a live row for the same project within one
// minute of it: the proxy recorded that turn live, and the archive later pruned the session.
// On a new session it also skips a response with a transcript row near it, unless only this run
// wrote that row (written[k] true): an earlier run imported the turn, and the user erased the
// session. written[k] is false when an earlier run wrote a row at k. A replace
// does not need that check, because prior already drops the turns of the earlier import.
// shortcut: rows carry no session id, so a turn of a session that did not go through Cortex is
// skipped when another session in the same project had live traffic in that minute. Keep the
// session id on ledger rows if that undercount shows up.
func toLedger(ctx context.Context, w *ledger.Writer, s Session, prior *priorTurns, sent map[string]bool, written map[liveKey]bool) error {
	newSession := prior == nil
	cutoff := w.RetentionCutoff()
	requestAt := map[string]time.Time{}
	var turns []*model.SessionEvent
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
			turns = append(turns, e)
		}
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
		k := liveKey{r.At.Truncate(time.Minute).Unix(), r.Project}
		if r.Source != event.SourceTranscript {
			if r.Source == "" {
				live[k] = true
			}
			continue
		}
		if _, seen := written[k]; !seen {
			written[k] = false // a row that an earlier run wrote
		}
		if newSession && !written[k] {
			live[k] = true
		}
	}
	var keep []*model.SessionEvent
	var keys []liveKey
	for _, e := range turns {
		k := turnKey(e)
		m, p := k.minute, k.project
		if live[k] || live[liveKey{m - 60, p}] || live[liveKey{m + 60, p}] {
			continue
		}
		keep = append(keep, e)
		keys = append(keys, k)
	}
	if err := w.RecordBatch(keep); err != nil {
		return err
	}
	for i, k := range keys {
		if _, seen := written[k]; !seen {
			written[k] = true
		}
		if id := keep[i].RequestID; id != "" {
			sent[id] = true
		}
	}
	return nil
}

// turnKey is the minute and project of e, in the form of the ledger row it makes.
func turnKey(e *model.SessionEvent) liveKey {
	p := model.TruncateLabelTail(model.SanitizeControlRunes(e.Project), event.MaxLabelLen) // as Row.Project
	return liveKey{e.At.Truncate(time.Minute).Unix(), p}
}

type liveKey struct {
	minute  int64
	project string
}

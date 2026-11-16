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
func Run(ctx context.Context, src Source, o Options) AgentReport {
	r := AgentReport{Agent: src.Agent()}
	mark := model.ImportedSource(src.Agent())
	var dryBytes int64 // the bytes a dry run would have written, for the size check
	for s, err := range src.Sessions(o.Since) {
		if err != nil {
			r.Error = err.Error()
			break
		}
		if r.Stopped != "" {
			r.NotImported++
			continue
		}
		if err := ctx.Err(); err != nil {
			r.Error = err.Error()
			break
		}
		r.SkippedLines += s.Skipped

		// Rule 1: a session with live events is not touched.
		if o.Store != nil && o.Store.ViewTail(s.ID, 1) != nil {
			r.SkippedLive++
			continue
		}
		sum, found := o.Archive.Lookup(s.ID)
		if found && sum.Source == "" {
			r.SkippedLive++
			continue
		}
		// Rule 2: the archived copy is as new as the transcript.
		if found && !s.Updated.After(sum.LastRequestAt) {
			r.SkippedCurrent++
			continue
		}
		// The next prune would delete a session past the archive's retention.
		if d := o.Archive.RetentionDays(); d > 0 && s.Updated.Before(time.Now().Add(-time.Duration(d)*24*time.Hour)) {
			r.SkippedExpired++
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
			// After is the zero time on a new session, so every turn goes to the ledger.
			var after time.Time
			if found {
				after = sum.LastRequestAt
			}
			// shortcut: the archive already holds s, so a re-run skips it as current and its rows stay
			// lost. Only a failed append (ENOSPC, EROFS) or a Close during it gets here. Undo the archive write if it shows up.
			if err := toLedger(ctx, o.Ledger, s, after); err != nil {
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

// toLedger records each response of s whose request is after after, and is not before the
// ledger's retention cutoff. The ledger already has the turns of an earlier import.
//
// It also skips a response when the ledger has a live row for the same project within one
// minute of it: the proxy recorded that turn live, and the archive later pruned the session.
// shortcut: rows carry no session id, so a turn of a session that did not go through Cortex is
// skipped when another session in the same project had live traffic in that minute. Keep the
// session id on ledger rows if that undercount shows up.
func toLedger(ctx context.Context, w *ledger.Writer, s Session, after time.Time) error {
	cutoff := w.RetentionCutoff()
	requestAt := map[string]time.Time{}
	var turns []*model.SessionEvent
	for i := range s.Events {
		e := &s.Events[i]
		switch e.Phase {
		case model.SessionRequest:
			requestAt[e.RequestID] = e.At
		case model.SessionResponse:
			if at, ok := requestAt[e.RequestID]; ok && at.After(after) && !e.At.Before(cutoff) {
				turns = append(turns, e)
			}
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
	// An error leaves live empty: the import then records every turn, as before this check.
	rows, _, _ := w.Window(ctx, from.Add(-time.Minute), to.Add(time.Minute))
	for _, r := range rows {
		if r.Source == "" {
			live[liveKey{r.At.Truncate(time.Minute).Unix(), r.Project}] = true
		}
	}
	var keep []*model.SessionEvent
	for _, e := range turns {
		m := e.At.Truncate(time.Minute)
		p := model.TruncateLabelTail(model.SanitizeControlRunes(e.Project), event.MaxLabelLen) // as Row.Project
		if live[liveKey{m.Unix(), p}] || live[liveKey{m.Add(-time.Minute).Unix(), p}] || live[liveKey{m.Add(time.Minute).Unix(), p}] {
			continue
		}
		keep = append(keep, e)
	}
	return w.RecordBatch(keep)
}

type liveKey struct {
	minute  int64
	project string
}

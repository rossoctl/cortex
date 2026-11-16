package importer

import (
	"context"
	"errors"
	"time"

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
// shortcut: an imported session expires from the archive 30 days after the import, and a
// later import then writes it as new and sends its turns to the ledger again. The ledger skips
// turns before RetentionCutoff, so only a turn inside both windows (about one day at a month
// boundary) counts two times. Keep the imported turn ids if that becomes a problem.
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
			toLedger(o.Ledger, s, after)
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
func toLedger(w *ledger.Writer, s Session, after time.Time) {
	cutoff := w.RetentionCutoff()
	requestAt := map[string]time.Time{}
	for i := range s.Events {
		e := &s.Events[i]
		switch e.Phase {
		case model.SessionRequest:
			requestAt[e.RequestID] = e.At
		case model.SessionResponse:
			if at, ok := requestAt[e.RequestID]; ok && at.After(after) && !e.At.Before(cutoff) {
				w.Record(s.ID, e)
			}
		}
	}
}

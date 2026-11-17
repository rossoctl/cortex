// Package opencode is the importer source for OpenCode. It reads opencode.db through the
// sqlite3 program, and makes one importer.Session from each group of sessions that OpenCode
// sends with the same X-Session-Id. It does not write to the database.
package opencode

import (
	"cmp"
	"errors"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/importer"
	"github.com/rossoctl/cortex/core/session/importer/sqlitecli"
)

const agentName = "opencode"

type source struct {
	dbPath string
	prog   string // the sqlite3 program; tests set a missing one
}

// New returns the source. dbPath "" resolves $XDG_DATA_HOME/opencode/opencode.db, or
// ~/.local/share/opencode/opencode.db, when Sessions runs.
func New(dbPath string) importer.Source { return source{dbPath: dbPath, prog: sqlitecli.Program} }

func (source) Agent() string { return agentName }

// PricingHost is OpenCode Zen, where most turns go. Its *-free models then price as zero.
func (source) PricingHost() string { return "opencode.ai" }

// sessionRow is one row of the session query.
type sessionRow struct {
	ID          string  `json:"id"`
	ParentID    *string `json:"parent_id"`
	ForkID      *string `json:"fork_session_id"`
	Directory   string  `json:"directory"`
	Version     string  `json:"version"`
	TimeCreated int64   `json:"time_created"`
	Last        *int64  `json:"last"` // in ms; nil when no row gives an event time
	Messages    int     `json:"messages"`
}

// affinity is the id OpenCode sends as X-Session-Id: the parent, else the fork source, else
// the session itself. It is one step, not a walk to the top.
func (r *sessionRow) affinity() string {
	for _, p := range []*string{r.ParentID, r.ForkID} {
		if p != nil && *p != "" {
			return *p
		}
	}
	return r.ID
}

// group is the session rows with the same affinity id.
type group struct {
	id       string
	rows     []*sessionRow // the row whose id is the group id first, when it exists
	last     int64
	messages int
}

const sessionQuery = `select s.id, s.parent_id, s.fork_session_id, s.directory, s.version, s.time_created,
  (select max(max(time_created, min(case when json_extract(data, '$.time.completed') > 0
      then json_extract(data, '$.time.completed') else time_created end, time_updated)))
    from session_message where session_id = s.id and type = 'assistant' and json_valid(data)
      and not (coalesce(json_type(data, '$.error'), 'null') != 'null'
        and coalesce(json_array_length(data, '$.content'), 0) = 0)) as last,
  (select count(*) from session_message where session_id = s.id) as messages
from session_v2 s`

func (s source) Sessions(since time.Time, skip func(id string, updated time.Time) bool, progress func(done, total int)) iter.Seq2[importer.Session, error] {
	return func(yield func(importer.Session, error) bool) {
		db := s.dbPath
		if db == "" {
			data := os.Getenv("XDG_DATA_HOME")
			if data == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					yield(importer.Session{}, err)
					return
				}
				data = filepath.Join(home, ".local", "share")
			}
			db = filepath.Join(data, "opencode", "opencode.db")
		}
		if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
			return // OpenCode is not installed
		} else if err != nil {
			yield(importer.Session{}, err)
			return
		}
		var rows []sessionRow
		if err := sqlitecli.Query(s.prog, db, sessionQuery, &rows); err != nil {
			yield(importer.Session{}, err)
			return
		}
		home, _ := os.UserHomeDir()
		all := groups(rows)
		for i, g := range all {
			progress(i, len(all))
			var at time.Time // zero: no events
			if g.last > 0 {
				at = time.UnixMilli(g.last)
			}
			if !since.IsZero() && at.Before(since) {
				continue
			}
			if skip(g.id, at) {
				continue
			}
			if !yield(s.read(db, g, home), nil) {
				return
			}
		}
		progress(len(all), len(all))
	}
}

// groups groups the session rows by affinity id, and gives the groups that have messages,
// newest first.
func groups(rows []sessionRow) []*group {
	byID := map[string]*group{}
	var out []*group
	for i := range rows {
		r := &rows[i]
		id := r.affinity()
		g, ok := byID[id]
		if !ok {
			g = &group{id: id}
			byID[id] = g
			out = append(out, g)
		}
		g.rows = append(g.rows, r)
		g.messages += r.Messages
		if r.Last != nil && *r.Last > g.last {
			g.last = *r.Last
		}
	}
	for _, g := range out {
		slices.SortFunc(g.rows, func(a, b *sessionRow) int {
			if (a.ID == g.id) != (b.ID == g.id) {
				if a.ID == g.id {
					return -1
				}
				return 1
			}
			return cmp.Or(cmp.Compare(a.TimeCreated, b.TimeCreated), strings.Compare(a.ID, b.ID))
		})
	}
	out = slices.DeleteFunc(out, func(g *group) bool { return g.messages == 0 })
	slices.SortFunc(out, func(a, b *group) int {
		return cmp.Or(cmp.Compare(b.last, a.last), strings.Compare(a.id, b.id))
	})
	return out
}

// read reads the messages of each session row of g and makes the session.
func (s source) read(db string, g *group, home string) importer.Session {
	sess := importer.Session{ID: g.id}
	p := parser{home: home, projects: map[string]string{}}
	hasRoot := g.rows[0].ID == g.id
	read := 0
	for _, r := range g.rows {
		var msgs []message
		if err := sqlitecli.Query(s.prog, db, "select id, type, time_created, time_updated, data from session_message where session_id = "+
			sqlitecli.Quote(r.ID)+" order by seq", &msgs); err != nil {
			if r.ID == g.id {
				slog.Warn("importer: cannot read an OpenCode session", "session", r.ID, "error", err)
				return importer.Session{ID: g.id, Unreadable: true}
			}
			slog.Warn("importer: cannot read an OpenCode session", "session", r.ID, "error", err)
			sess.Skipped++
			continue
		}
		read++
		for _, m := range msgs {
			sess.SizeHint += int64(len(m.Data))
		}
		events, skipped := p.session(r, msgs)
		sess.Events = append(sess.Events, events...)
		sess.Skipped += skipped
	}
	if !hasRoot && read == 0 {
		slog.Warn("importer: cannot read an OpenCode session group", "session", g.id)
		return importer.Session{ID: g.id, Unreadable: true}
	}
	// Stable: on a tie a request stays before its response.
	slices.SortStableFunc(sess.Events, func(a, b model.SessionEvent) int { return a.At.Compare(b.At) })
	for _, e := range sess.Events {
		if e.At.After(sess.Updated) {
			sess.Updated = e.At
		}
	}
	return sess
}

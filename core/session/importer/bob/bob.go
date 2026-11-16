// Package bob is the importer source for Bob (Bob Shell and Bob IDE). It reads the tasks in
// ~/.bob/db/bob.db through the sqlite3 program, and makes one importer.Session from each
// root task and its child tasks. It does not write to the database.
package bob

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

const agentName = "bob"

type source struct {
	dbPath string
	prog   string // the sqlite3 program; tests set a missing one
}

// New returns the source. dbPath "" resolves ~/.bob/db/bob.db when Sessions runs.
func New(dbPath string) importer.Source { return source{dbPath: dbPath, prog: sqlitecli.Program} }

func (source) Agent() string       { return agentName }
func (source) PricingHost() string { return "api.us-east.bob.ibm.com" }

// task is one row of the task query.
type task struct {
	ID       string  `json:"id"`
	ParentID string  `json:"parent_id"` // JSON null leaves it ""
	TaskType string  `json:"task_type"`
	Version  *string `json:"version"`
	Dir      string  `json:"dir"`
	Model    string  `json:"model"`
	Last     *int64  `json:"last"` // newest message, in ms; nil when the task has no messages
}

// tree is a root task and its child tasks.
type tree struct {
	root  *task
	tasks []*task // the root first
	last  int64
}

const taskQuery = `select t.id, t.parent_id, t.task_type, t.version,
  coalesce(nullif(t.directory,''), json_extract(t.env,'$.workspace'), '') as dir,
  json_extract(t.env,'$.model.id') as model,
  (select max(m.created_at) from messages m where m.task_id = t.id) as last
from tasks t`

func (s source) Sessions(since time.Time, skip func(id string, updated time.Time) bool) iter.Seq2[importer.Session, error] {
	return func(yield func(importer.Session, error) bool) {
		db := s.dbPath
		if db == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				yield(importer.Session{}, err)
				return
			}
			db = filepath.Join(home, ".bob", "db", "bob.db")
		}
		if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
			return // Bob is not installed
		} else if err != nil {
			yield(importer.Session{}, err)
			return
		}
		var tasks []task
		if err := sqlitecli.Query(s.prog, db, taskQuery, &tasks); err != nil {
			yield(importer.Session{}, err)
			return
		}
		home, _ := os.UserHomeDir()
		for _, tr := range trees(tasks) {
			at := time.UnixMilli(tr.last)
			if !since.IsZero() && at.Before(since) {
				continue
			}
			if skip(tr.root.ID, at) {
				continue
			}
			if !yield(s.read(db, tr, home), nil) {
				return
			}
		}
	}
}

// trees groups the tasks by their root task, and gives the trees that have messages, newest
// first. A parent_id that names a missing task ends the walk to the root. In a cycle, the
// task with the smallest id is the root.
func trees(tasks []task) []*tree {
	byID := make(map[string]*task, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	rootOf := func(t *task) *task {
		path := []*task{t}
		for {
			p, ok := byID[t.ParentID]
			if !ok {
				return t
			}
			if i := slices.Index(path, p); i >= 0 {
				// A cycle: each start point must find the same root, so take its smallest id.
				return slices.MinFunc(path[i:], func(a, b *task) int { return strings.Compare(a.ID, b.ID) })
			}
			path = append(path, p)
			t = p
		}
	}
	byRoot := map[string]*tree{}
	var out []*tree
	for i := range tasks {
		t := &tasks[i]
		r := rootOf(t)
		tr, ok := byRoot[r.ID]
		if !ok {
			tr = &tree{root: r, tasks: []*task{r}}
			byRoot[r.ID] = tr
			out = append(out, tr)
		}
		if t != r {
			tr.tasks = append(tr.tasks, t)
		}
		if t.Last != nil && *t.Last > tr.last {
			tr.last = *t.Last
		}
	}
	out = slices.DeleteFunc(out, func(tr *tree) bool { return tr.last == 0 })
	slices.SortFunc(out, func(a, b *tree) int {
		return cmp.Or(cmp.Compare(b.last, a.last), strings.Compare(a.root.ID, b.root.ID))
	})
	return out
}

// read reads the messages of each task of tr and makes the session.
func (s source) read(db string, tr *tree, home string) importer.Session {
	sess := importer.Session{ID: tr.root.ID}
	p := parser{home: home, projects: map[string]string{}, rootDir: tr.root.Dir, read: map[string]bool{}, turned: map[string]bool{}}
	for _, t := range tr.tasks {
		p.read[t.ID] = t.Last != nil
	}
	for _, t := range tr.tasks {
		var rows []row
		if err := sqlitecli.Query(s.prog, db, "select id, role, data, created_at from messages where task_id = "+
			sqlitecli.Quote(t.ID)+" order by created_at, rowid", &rows); err != nil {
			if t == tr.root {
				slog.Warn("importer: cannot read a Bob task", "task", t.ID, "error", err)
				return importer.Session{ID: tr.root.ID, Unreadable: true}
			}
			sess.Skipped++
			continue
		}
		for _, r := range rows {
			sess.SizeHint += int64(len(r.Data))
		}
		events, skipped := p.task(t, t == tr.root, rows)
		sess.Events = append(sess.Events, events...)
		sess.Skipped += skipped
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

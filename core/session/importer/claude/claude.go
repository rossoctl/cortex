// Package claude is the importer source for Claude Code. It reads the transcripts under
// <config dir>/projects and makes one importer.Session from each session transcript and
// its subagent transcripts. It does not write a file and does not take the harvest lock.
package claude

import (
	"bufio"
	"cmp"
	"encoding/json"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
	ccstore "github.com/rossoctl/cortex/core/observe/claude"
	"github.com/rossoctl/cortex/core/session/importer"
)

const agentName = "claude-code"

type source struct{ configDir string }

// New returns the source. configDir "" resolves claude.DefaultConfigDir() (honours
// CLAUDE_CONFIG_DIR) when Sessions runs.
func New(configDir string) importer.Source { return source{configDir: configDir} }

func (source) Agent() string       { return agentName }
func (source) PricingHost() string { return "api.anthropic.com" }

// subFile is one subagent transcript of a session.
type subFile struct {
	path, id string
	mod      time.Time
}

func (s source) Sessions(since time.Time, skip func(id string, updated time.Time) bool, progress func(done, total int)) iter.Seq2[importer.Session, error] {
	return func(yield func(importer.Session, error) bool) {
		dir := s.configDir
		if dir == "" {
			d, err := ccstore.DefaultConfigDir()
			if err != nil {
				yield(importer.Session{}, err)
				return
			}
			dir = d
		}
		metas, err := transcripts(dir)
		if err != nil {
			yield(importer.Session{}, err)
			return
		}
		ids := make([]string, 0, len(metas))
		for id := range metas {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b string) int {
			return cmp.Or(metas[b].mod.Compare(metas[a].mod), strings.Compare(a, b))
		})
		home, _ := os.UserHomeDir()
		for i, id := range ids {
			progress(i, len(ids))
			m := metas[id]
			subs := subagentFiles(filepath.Join(filepath.Dir(m.path), id, "subagents"))
			if !since.IsZero() && m.mod.Before(since) &&
				!slices.ContainsFunc(subs, func(f subFile) bool { return !f.mod.Before(since) }) {
				continue
			}
			// No line is newer than its file, so the newest mtime is not before Session.Updated
			// either. It lets skip drop a live or expired session without a read of its files.
			mod := m.mod
			for _, f := range subs {
				if f.mod.After(mod) {
					mod = f.mod
				}
			}
			if skip(id, mod) {
				continue
			}
			var sess importer.Session
			if newest, err := newestAt(m.path, subs); err != nil {
				sess = unreadable(id, m.path, err)
			} else if skip(id, newest) {
				continue
			} else {
				sess = readSession(id, m.path, subs, home)
			}
			if !yield(sess, nil) {
				return
			}
		}
		progress(len(ids), len(ids))
	}
}

// transcripts lists <dir>/projects/*/<id>.jsonl by id, with the files of ccstore.ReadSessions.
// It does not read the files: ReadSessions parses each one for its title. A symlink counts as
// its target, and of two files with one id the newer wins.
func transcripts(dir string) (map[string]subFile, error) {
	projects := filepath.Join(dir, "projects")
	entries, err := os.ReadDir(projects)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]subFile{}
	for _, e := range entries {
		proj := filepath.Join(projects, e.Name())
		files, _ := os.ReadDir(proj) // one unreadable project must not stop the others
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(proj, f.Name())
			st, err := os.Stat(path)
			if err != nil || st.IsDir() {
				continue
			}
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			if prev, dup := out[id]; dup && !st.ModTime().After(prev.mod) {
				continue
			}
			out[id] = subFile{path: path, id: id, mod: st.ModTime()}
		}
	}
	return out, nil
}

// subagentFiles lists agent-*.jsonl in dir and in each dir/workflows/<run>/. The harvest's
// Subagents map is not used: it holds only the agents that have a .meta.json file.
func subagentFiles(dir string) []subFile {
	out := listAgents(dir)
	runs, _ := os.ReadDir(filepath.Join(dir, "workflows"))
	for _, r := range runs {
		if r.IsDir() {
			out = append(out, listAgents(filepath.Join(dir, "workflows", r.Name()))...)
		}
	}
	return out
}

func listAgents(dir string) []subFile {
	entries, _ := os.ReadDir(dir)
	var out []subFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, subFile{
			path: filepath.Join(dir, name),
			id:   strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl"),
			mod:  fi.ModTime(),
		})
	}
	return out
}

// unreadable is the session of a main transcript that cannot be read.
func unreadable(id, path string, err error) importer.Session {
	slog.Warn("importer: cannot read a Claude Code transcript", "path", path, "error", err)
	return importer.Session{ID: id, Unreadable: true}
}

// readSession parses the main transcript and the subagent transcripts of one session.
func readSession(id, path string, subs []subFile, home string) importer.Session {
	p := parser{home: home, projects: map[string]string{}}
	main, err := p.file(path, "")
	if err != nil {
		return unreadable(id, path, err)
	}
	s := importer.Session{ID: id, Events: main.events, Skipped: main.skipped, SizeHint: main.size}
	for _, f := range subs {
		r, err := p.file(f.path, f.id)
		if err != nil {
			s.Skipped++
			continue
		}
		s.Events = append(s.Events, r.events...)
		s.Skipped += r.skipped
		s.SizeHint += r.size
	}
	// Stable: on a tie the main file stays first, and a request stays before its response.
	slices.SortStableFunc(s.Events, func(a, b model.SessionEvent) int { return a.At.Compare(b.At) })
	for _, e := range s.Events {
		if e.At.After(s.Updated) {
			s.Updated = e.At
		}
	}
	return s
}

// newestAt reads the timestamps of the files of one session, without the parse. It gives
// the newest assistant line, and the newest user line that an assistant line follows. Each
// event that the parse makes has the time of one of these lines, so the result is not before
// Session.Updated. It gives an error when the main transcript cannot be opened. A subagent file
// it cannot open gives the zero time.
func newestAt(path string, subs []subFile) (time.Time, error) {
	t, err := newestInFile(path)
	if err != nil {
		return t, err
	}
	for _, f := range subs {
		if n, _ := newestInFile(f.path); n.After(t) {
			t = n
		}
	}
	return t, nil
}

func newestInFile(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	var newest, user time.Time
	r := bufio.NewReader(f)
	for {
		raw, rerr := r.ReadBytes('\n')
		var l struct {
			Type      string           `json:"type"`
			Timestamp time.Time        `json:"timestamp"`
			Message   *json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &l) == nil && l.Message != nil {
			switch l.Type {
			case "user":
				if l.Timestamp.After(user) {
					user = l.Timestamp
				}
			case "assistant":
				for _, t := range []time.Time{l.Timestamp, user} {
					if t.After(newest) {
						newest = t
					}
				}
				user = time.Time{}
			}
		}
		if rerr != nil {
			return newest, nil
		}
	}
}

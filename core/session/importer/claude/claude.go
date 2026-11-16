// Package claude is the importer source for Claude Code. It reads the transcripts under
// <config dir>/projects and makes one importer.Session from each session transcript and
// its subagent transcripts. It does not write a file and does not take the harvest lock.
package claude

import (
	"cmp"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
	ccstore "github.com/rossoctl/cortex/core/observe/claude"
	"github.com/rossoctl/cortex/core/session"
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

func (s source) Sessions(since time.Time) iter.Seq2[importer.Session, error] {
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
		metas, _, _, err := ccstore.ReadSessions(dir, nil)
		if err != nil {
			yield(importer.Session{}, err)
			return
		}
		ids := make([]string, 0, len(metas))
		for id := range metas {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b string) int {
			return cmp.Or(metas[b].LogModTime.Compare(metas[a].LogModTime), strings.Compare(a, b))
		})
		home, _ := os.UserHomeDir()
		for _, id := range ids {
			m := metas[id]
			subs := subagentFiles(filepath.Join(filepath.Dir(m.LogFile), id, "subagents"))
			if !since.IsZero() && m.LogModTime.Before(since) &&
				!slices.ContainsFunc(subs, func(f subFile) bool { return !f.mod.Before(since) }) {
				continue
			}
			sess, ok := readSession(id, m.LogFile, subs, home)
			if !ok {
				continue
			}
			if !yield(sess, nil) {
				return
			}
		}
	}
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

// readSession parses the main transcript and the subagent transcripts of one session. It
// gives false when the main transcript cannot be read.
func readSession(id, path string, subs []subFile, home string) (importer.Session, bool) {
	p := parser{home: home, projects: map[string]string{}}
	main, err := p.file(path, "")
	if err != nil {
		slog.Warn("importer: cannot read a Claude Code transcript", "path", path, "error", err)
		return importer.Session{}, false
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
	var in session.Interner
	for i := range s.Events {
		in.InternEvent(&s.Events[i])
		if e := &s.Events[i]; e.Phase == model.SessionRequest && e.At.After(s.Updated) {
			s.Updated = e.At
		}
	}
	return s, true
}

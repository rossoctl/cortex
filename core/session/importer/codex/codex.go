// Package codex is the importer source for Codex. It reads the rollout files under
// <codex home>/sessions and makes one importer.Session from the root rollout of a session
// and the rollouts of its subagents. It does not write a file.
package codex

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session/importer"
)

const agentName = "codex"

type source struct{ home string }

// New returns the source. home "" resolves $CODEX_HOME, else ~/.codex, when Sessions runs.
func New(home string) importer.Source { return source{home: home} }

func (source) Agent() string { return agentName }

// PricingHost is the OpenAI host: the bundled table prices the GPT models under every host,
// and a rollout does not say if Codex used an API key or a ChatGPT login.
func (source) PricingHost() string { return "api.openai.com" }

// rollout is one rollout file, from its session_meta line.
type rollout struct {
	path, thread, session string
	forkedFrom            string // the thread id this rollout forks, or ""
	mod                   time.Time
	size                  int64
}

// openRollout opens a rollout file. The listing and the parse both use it, so a test can
// make an open fail.
var openRollout = openFile

// openFile opens path, and reads it through zstd when its name ends with .zst.
func openFile(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil || !strings.HasSuffix(path, ".zst") {
		return f, err
	}
	d, err := zstd.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return zstdFile{d, f}, nil
}

type zstdFile struct {
	*zstd.Decoder
	f *os.File
}

func (z zstdFile) Close() error {
	z.Decoder.Close()
	return z.f.Close()
}

func (s source) dir() (string, error) {
	if s.home != "" {
		return s.home, nil
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".codex"), nil
}

func (s source) Sessions(since time.Time, skip func(id string, updated time.Time) bool, progress func(done, total int)) iter.Seq2[importer.Session, error] {
	return func(yield func(importer.Session, error) bool) {
		dir, err := s.dir()
		if err != nil {
			yield(importer.Session{}, err)
			return
		}
		files, unopened := list(filepath.Join(dir, "sessions"))
		groups := group(files)
		// A fork's parent can be archived. The source does not import an archived rollout, but
		// the ledger can already hold its turns.
		threads := map[string]rollout{}
		archived, _ := list(filepath.Join(dir, "archived_sessions"))
		for _, f := range archived {
			threads[f.thread] = f
		}
		for _, files := range groups {
			for _, f := range files {
				threads[f.thread] = f
			}
		}
		sids := make([]string, 0, len(groups))
		newest := map[string]time.Time{}
		for id, files := range groups {
			sids = append(sids, id)
			for _, f := range files {
				if f.mod.After(newest[id]) {
					newest[id] = f.mod
				}
			}
		}
		slices.SortFunc(sids, func(a, b string) int {
			return cmp.Or(newest[b].Compare(newest[a]), strings.Compare(a, b))
		})
		total := len(unopened) + len(sids)
		// A rollout that cannot be opened names no session, so it is counted on its own.
		for i, mod := range unopened {
			progress(i, total)
			if (since.IsZero() || !mod.Before(since)) && !yield(importer.Session{Unreadable: true}, nil) {
				return
			}
		}
		home, _ := os.UserHomeDir()
		for i, id := range sids {
			progress(len(unopened)+i, total)
			// Codex writes each line when it happens, so no line is newer than its file.
			if !since.IsZero() && newest[id].Before(since) {
				continue
			}
			if skip(id, newest[id]) {
				continue
			}
			// The mtime is always after the newest event, so only this time lets Rule 2 skip an
			// unchanged session before the slow parse.
			if at, ok := newestAt(groups[id]); ok && skip(id, at) {
				continue
			}
			if !yield(readSession(id, groups[id], threads, home), nil) {
				return
			}
		}
		progress(total, total)
	}
}

// list finds the rollout files under dir at all depths and reads the first line of each.
// unopened is the mtime of each file that cannot be opened or read. A missing dir gives none.
func list(dir string) (out []rollout, unopened []time.Time) {
	// An error in one directory must not stop the others, so the walk ignores it.
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != dir {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !d.Type().IsRegular() || !strings.HasPrefix(name, "rollout-") ||
			!(strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.zst")) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		r, err := readMeta(path)
		if err != nil {
			slog.Warn("importer: cannot list a Codex rollout", "path", path, "error", err)
			if !errors.Is(err, errNoMeta) && !errors.Is(err, io.EOF) {
				unopened = append(unopened, fi.ModTime())
			}
			return nil
		}
		r.path, r.mod, r.size = path, fi.ModTime(), fi.Size()
		out = append(out, r)
		return nil
	})
	return out, unopened
}

// errNoMeta is the error of a rollout whose first line is not a session_meta line.
var errNoMeta = errors.New("the first line is not a session_meta line")

// readMeta gives the thread id, the session id and the forked thread id from the first line
// of a rollout.
func readMeta(path string) (rollout, error) {
	f, err := openRollout(path)
	if err != nil {
		return rollout{}, err
	}
	defer f.Close()
	raw, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(raw) == 0 {
		return rollout{}, err
	}
	var l struct {
		Type    string `json:"type"`
		Payload struct {
			ID           string `json:"id"`
			SessionID    string `json:"session_id"`
			ForkedFromID string `json:"forked_from_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &l) != nil || l.Type != "session_meta" || l.Payload.ID == "" {
		return rollout{}, errNoMeta
	}
	// Codex sends session_id as the Session-Id header; when it is absent it reads it as id.
	return rollout{thread: l.Payload.ID, session: cmp.Or(l.Payload.SessionID, l.Payload.ID), forkedFrom: l.Payload.ForkedFromID}, nil
}

// newestAt reads each rollout of a session fast and gives a time that is not before the At of
// any event that the parse makes: the newest token record that closes an open turn, or the
// newest model output. ok is false when a file cannot be read, so that the parse reports it.
// shortcut: it trusts that the lines of a rollout are in time order, as Codex writes them.
func newestAt(files []rollout) (newest time.Time, ok bool) {
	for _, f := range files {
		t, err := newestInFile(f.path)
		if err != nil {
			return time.Time{}, false
		}
		if t.After(newest) {
			newest = t
		}
	}
	return newest, true
}

func newestInFile(path string) (time.Time, error) {
	f, err := openRollout(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	var newest time.Time
	open := false // a model output came after the last close; see parser.file
	r := bufio.NewReader(f)
	for {
		raw, rerr := r.ReadBytes('\n')
		var l line
		var it struct {
			Type string          `json:"type"`
			Role string          `json:"role"`
			Info json.RawMessage `json:"info"`
		}
		if json.Unmarshal(raw, &l) == nil && json.Unmarshal(l.Payload, &it) == nil {
			switch {
			case l.Type == "response_item" && (outputTypes[it.Type] || (it.Type == "message" && it.Role == "assistant")):
				open, newest = true, maxTime(newest, l.Timestamp)
			case l.Type == "compacted",
				// The parse skips an item that it refuses, and the turn stays open.
				l.Type == "response_item" && open && !toolOutputTypes[it.Type] && validItem(l.Payload):
				open = false
			case open && (l.Type == "token_usage_record" ||
				(l.Type == "event_msg" && it.Type == "token_count" && len(it.Info) > 0 && string(it.Info) != "null")):
				open, newest = false, maxTime(newest, l.Timestamp)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return newest, nil
			}
			return time.Time{}, rerr
		}
	}
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// group puts the rollouts by session id. Of two rollouts with one thread id the newer wins:
// a reverted thread, or a .jsonl and its .jsonl.zst during compression.
func group(files []rollout) map[string][]rollout {
	byThread := map[string]rollout{}
	for _, f := range files {
		if prev, dup := byThread[f.thread]; dup && !f.mod.After(prev.mod) {
			continue
		}
		byThread[f.thread] = f
	}
	out := map[string][]rollout{}
	for _, f := range byThread {
		out[f.session] = append(out[f.session], f)
	}
	for _, rs := range out {
		// The root first, so that on a tie of At its events stay before the subagents' events.
		slices.SortFunc(rs, func(a, b rollout) int {
			return cmp.Or(cmp.Compare(isChild(a), isChild(b)), strings.Compare(a.path, b.path))
		})
	}
	return out
}

func isChild(f rollout) int {
	if f.thread == f.session {
		return 0
	}
	return 1
}

// readSession parses the rollouts of one session. threads gives a fork's parent rollout.
func readSession(id string, files []rollout, threads map[string]rollout, home string) importer.Session {
	p := parser{home: home, projects: map[string]string{}, parents: map[string][]usage{}}
	s := importer.Session{ID: id}
	for _, f := range files {
		subagent := ""
		if f.thread != id {
			subagent = f.thread
		}
		r, err := p.file(f.path, f.thread, subagent, p.copied(f, threads), false)
		if err != nil {
			if subagent == "" {
				slog.Warn("importer: cannot read a Codex rollout", "path", f.path, "error", err)
				return importer.Session{ID: id, Unreadable: true}
			}
			s.Skipped++
			continue
		}
		s.Events = append(s.Events, r.events...)
		s.Skipped += r.skipped
		s.SizeHint += f.size
	}
	// Stable: on a tie the root stays first, and a request stays before its response.
	slices.SortStableFunc(s.Events, func(a, b model.SessionEvent) int { return a.At.Compare(b.At) })
	for _, e := range s.Events {
		if e.At.After(s.Updated) {
			s.Updated = e.At
		}
	}
	return s
}

// copied gives the usages of the turns of f's parent, which a fork's rollout starts with a
// copy of. It gives none when f is not a fork or its parent rollout cannot be read.
func (p *parser) copied(f rollout, threads map[string]rollout) []usage {
	parent, ok := threads[f.forkedFrom]
	if f.forkedFrom == "" || !ok {
		return nil
	}
	if u, done := p.parents[parent.path]; done {
		return u
	}
	r, err := p.file(parent.path, parent.thread, "", nil, true)
	if err != nil {
		r.usages = nil
	}
	p.parents[parent.path] = r.usages
	return r.usages
}

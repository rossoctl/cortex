// Package pi is the importer source for Pi (pi.dev). It reads the session files under
// <agent dir>/sessions/<cwd dir>/<time>_<id>.jsonl and makes one importer.Session from each
// file. It does not write a file.
package pi

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/session/importer"
)

const (
	agentName   = "pi"
	agentDirEnv = "PI_CODING_AGENT_DIR"
)

type source struct{ agentDir string }

// New returns the source. agentDir "" resolves $PI_CODING_AGENT_DIR (a leading ~ is HOME),
// else ~/.pi/agent, when Sessions runs.
func New(agentDir string) importer.Source { return source{agentDir: agentDir} }

func (source) Agent() string { return agentName }

// PricingHost is pi.dev for every turn: Pi's provider is a free-form name. No pricing row is
// scoped to pi.dev, so each turn prices by model id through the "*" rows.
func (source) PricingHost() string { return "pi.dev" }

// sessionFile is one session file.
type sessionFile struct {
	path string
	mod  time.Time
}

func (s source) Sessions(since time.Time, skip func(id string, updated time.Time) bool, progress func(done, total int)) iter.Seq2[importer.Session, error] {
	return func(yield func(importer.Session, error) bool) {
		home, _ := os.UserHomeDir()
		dir := s.agentDir
		if dir == "" {
			dir = defaultAgentDir(home)
		}
		files, err := sessionFiles(filepath.Join(dir, "sessions"))
		if err != nil {
			yield(importer.Session{}, err)
			return
		}
		slices.SortFunc(files, func(a, b sessionFile) int {
			return cmp.Or(b.mod.Compare(a.mod), strings.Compare(a.path, b.path))
		})
		p := parser{home: home, projects: map[string]string{}}
		for i, f := range files {
			progress(i, len(files))
			if !since.IsZero() && f.mod.Before(since) {
				continue
			}
			id, err := headerID(f.path)
			if id == "" {
				id = idFromName(f.path)
			}
			// No entry is newer than its file, so the mtime is not before Session.Updated.
			if skip(id, f.mod) {
				continue
			}
			var sess importer.Session
			if err != nil {
				sess = unreadable(id, f.path, err)
			} else if newest, err := newestAt(f.path); err != nil {
				sess = unreadable(id, f.path, err)
			} else if skip(id, newest) {
				continue
			} else {
				sess = p.session(id, f.path)
			}
			if !yield(sess, nil) {
				return
			}
		}
		progress(len(files), len(files))
	}
}

// defaultAgentDir is Pi's agent directory, by the rule of piExtensionPath in cmd/agentop.
func defaultAgentDir(home string) string {
	dir := strings.TrimSpace(os.Getenv(agentDirEnv))
	switch {
	case dir == "":
		return filepath.Join(home, ".pi", "agent")
	case dir == "~" || strings.HasPrefix(dir, "~/"):
		return filepath.Join(home, dir[1:])
	}
	return dir
}

// sessionFiles lists <sessions>/*/*.jsonl. A missing sessions directory gives no files.
func sessionFiles(sessions string) ([]sessionFile, error) {
	dirs, err := os.ReadDir(sessions)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []sessionFile
	for _, d := range dirs {
		proj := filepath.Join(sessions, d.Name())
		entries, _ := os.ReadDir(proj) // one unreadable directory must not stop the others
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(proj, e.Name())
			st, err := os.Stat(path)
			if err != nil || st.IsDir() {
				continue
			}
			out = append(out, sessionFile{path: path, mod: st.ModTime()})
		}
	}
	return out, nil
}

// idFromName is the id part of <time>_<id>.jsonl.
func idFromName(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if _, id, ok := strings.Cut(name, "_"); ok {
		return id
	}
	return name
}

// header is the first line of a Pi session file.
type header struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Cwd  string `json:"cwd"`
}

// errNoHeader is the error of a file whose first parsed line is not a session header.
var errNoHeader = errors.New("no session header")

// headerID reads the id of the file's session header. Like Pi, it skips lines that are not
// JSON. It gives "" and an error when the file cannot be read or has no header.
func headerID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		raw, rerr := r.ReadBytes('\n')
		var h header
		if raw = bytes.TrimSpace(raw); len(raw) > 0 && json.Unmarshal(raw, &h) == nil {
			if h.Type != "session" || h.ID == "" {
				return "", errNoHeader
			}
			return h.ID, nil
		}
		if rerr != nil {
			return "", errNoHeader
		}
	}
}

func unreadable(id, path string, err error) importer.Session {
	slog.Warn("importer: cannot read a Pi session file", "path", path, "error", err)
	return importer.Session{ID: id, Unreadable: true}
}

// newestAt gives the newest timestamp of an assistant message entry, without the parse. Each
// event that the parse makes has that time or an earlier one, so the result is not before
// Session.Updated.
func newestAt(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	var newest time.Time
	r := bufio.NewReader(f)
	for {
		raw, rerr := r.ReadBytes('\n')
		var l struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Message   *struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &l) == nil && l.Type == "message" && l.Message != nil &&
			l.Message.Role == "assistant" && l.Timestamp.After(newest) {
			newest = l.Timestamp
		}
		if rerr != nil {
			return newest, nil
		}
	}
}

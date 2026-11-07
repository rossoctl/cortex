package archive

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
)

const (
	// metaFile is a session's summary and segment list; metaTmp is where a rewrite is staged.
	metaFile = "session.json"
	metaTmp  = ".session.json.tmp"

	// formatVersion is session.json's "v". Additive changes keep it; a reader seeing a higher
	// one it cannot read skips the session rather than misread it.
	formatVersion = 1
)

// sessionMeta is session.json: everything a list of archived sessions needs, and the segment
// ranges a page needs to choose what to decode, without opening a segment.
//
// THE FOLD AND THE SEGMENT LIST ARE ONE SNAPSHOT. Both are written together from the writer
// goroutine's state, so the summary always covers exactly the events the listed ranges hold.
// reconcile depends on that to fold, after a crash, only what the snapshot had not.
type sessionMeta struct {
	V         int                  `json:"v"`
	ID        string               `json:"id"`
	CreatedAt time.Time            `json:"createdAt"`
	UpdatedAt time.Time            `json:"updatedAt"`
	Summary   *session.SummaryFold `json:"summary"`
	Segments  []SegmentInfo        `json:"segments"`
}

// lastSeq is the highest seq the session holds on disk: what the store must number after.
func (m *sessionMeta) lastSeq() uint64 {
	var last uint64
	for _, s := range m.Segments {
		last = max(last, s.LastSeq)
	}
	return last
}

// writeMeta replaces dir's session.json atomically: staged, fsynced, renamed, and the directory
// fsynced, so a crash leaves either the old file or the new one, never a torn one.
func writeMeta(dir string, m *sessionMeta) error {
	m.V = formatVersion
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp := filepath.Join(dir, metaTmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, metaFile)); err != nil {
		return err
	}
	return syncDir(dir)
}

// readMeta reads dir's session.json.
func readMeta(dir string) (*sessionMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return nil, err
	}
	var m sessionMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.V > formatVersion {
		return nil, fmt.Errorf("archive: %s is format v%d, newer than v%d", dir, m.V, formatVersion)
	}
	if m.ID == "" {
		return nil, fmt.Errorf("archive: %s names no session", dir)
	}
	if m.Summary == nil {
		m.Summary = session.NewSummaryFold()
	}
	return &m, nil
}

// loaded is one session directory read at startup.
type loaded struct {
	dir  string
	meta *sessionMeta
}

// loadMetas reads every session directory under dataDir. One that cannot be read is skipped,
// counted and logged rather than failing the rest: a damaged session must not cost the others.
func loadMetas(dataDir string) ([]loaded, int, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, 0, err
	}
	var out []loaded
	skipped := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(dataDir, e.Name())
		m, err := readMeta(dir)
		if err != nil {
			skipped++
			slog.Warn("session archive: skipping a session it cannot read", "dir", dir, "error", err)
			continue
		}
		out = append(out, loaded{dir: dir, meta: m})
	}
	return out, skipped, nil
}

// reconcile brings m into line with the segment files in dir after a process that may have
// crashed, and reports whether it changed anything.
//
// session.json is rewritten every few seconds, so a crash can leave it behind the files in two
// ways, both on segments that were open at the time — a process never reopens a segment, so no
// other kind can be stale:
//
//   - A listed segment grew after its entry was written. Its range is read back from the file.
//     THIS ONE IS A CORRECTNESS MATTER, not bookkeeping: lastSeq seeds the store's numbering, and
//     a stale one would number new events into seqs already on disk.
//   - A segment was opened after the last write and is not listed at all. It is read and added.
//
// Events past what the snapshot had recorded are folded into the summary, and only those: the
// summary and the list are one snapshot (see sessionMeta), so each event is counted exactly once.
// A listed segment whose file is gone (deleted by retention before the list was rewritten) is
// dropped, and an unlisted empty one — created, then killed before its first event — is removed.
func reconcile(dir string, m *sessionMeta) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	onDisk := map[string]fs.FileInfo{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), segmentExt) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return false, err
		}
		onDisk[e.Name()] = fi
	}

	changed := false
	kept := m.Segments[:0]
	for _, s := range m.Segments {
		fi, ok := onDisk[s.File]
		if !ok {
			changed = true
			continue
		}
		delete(onDisk, s.File)
		if fi.Size() != s.Bytes {
			fresh, err := scanSegment(filepath.Join(dir, s.File), fi, s.LastSeq, m)
			if err != nil {
				return false, err
			}
			s, changed = fresh, true
		}
		kept = append(kept, s)
	}
	m.Segments = kept

	for name, fi := range onDisk {
		path := filepath.Join(dir, name)
		info, err := scanSegment(path, fi, 0, m)
		if err != nil {
			return false, err
		}
		changed = true
		if info.Events == 0 {
			os.Remove(path)
			continue
		}
		m.Segments = append(m.Segments, info)
	}
	if changed {
		slices.SortFunc(m.Segments, func(a, b SegmentInfo) int { return cmp.Compare(a.FirstSeq, b.FirstSeq) })
		for _, s := range m.Segments {
			if s.LastWrite.After(m.UpdatedAt) {
				m.UpdatedAt = s.LastWrite
			}
		}
	}
	return changed, nil
}

// scanSegment reads a segment's true range from the file, folding into m's summary every event
// numbered after `after`. A torn tail ends the scan; what precedes it counts.
func scanSegment(path string, fi fs.FileInfo, after uint64, m *sessionMeta) (SegmentInfo, error) {
	info := SegmentInfo{File: filepath.Base(path), Bytes: fi.Size(), LastWrite: fi.ModTime()}
	_, err := readSegment(path, func(e *model.SessionEvent) bool {
		if info.Events == 0 {
			info.FirstSeq = e.Seq
		}
		info.LastSeq = e.Seq
		info.Events++
		if e.Seq > after {
			m.Summary.Add(m.ID, e)
		}
		return true
	})
	return info, err
}

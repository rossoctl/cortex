package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// forgetFile holds one forget record per line, in the archive root next to data/, so a clear
// does not remove it. A record tells an import that the cost ledger already counted the turns of a
// removed session in [from, to].
const forgetFile = "forgotten.jsonl"

// maxLedgerDays clamps WithLedgerRetentionDays.
const maxLedgerDays = 3650 // ledger.MaxRetentionDays; the archive does not import the ledger.

// WithLedgerRetentionDays sets the cost ledger's retention. Zero (the default) means no ledger,
// and the archive writes no forget records. Negative is 0; past 3650 it is clamped.
func WithLedgerRetentionDays(n int) Option { return func(a *Archive) { a.ledgerDays = n } }

// forgetRecord is one line of forgetFile. From and To are Unix milliseconds.
type forgetRecord struct {
	ID   string `json:"id"`
	From int64  `json:"from"`
	To   int64  `json:"to"`
}

// Forgotten returns the span of the forget record for id: the first archive time of the removed
// session and the time of its newest request. Safe from any goroutine.
func (a *Archive) Forgotten(id string) (from, to time.Time, ok bool) {
	a.fgMu.RLock()
	span, ok := a.forgotten[id]
	a.fgMu.RUnlock()
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	return time.UnixMilli(span[0]), time.UnixMilli(span[1]), true
}

// mergeForgotten adds a record to the map: the smallest from and the largest to win.
func (a *Archive) mergeForgotten(r forgetRecord) {
	a.fgMu.Lock()
	defer a.fgMu.Unlock()
	if old, ok := a.forgotten[r.ID]; ok {
		r.From, r.To = min(r.From, old[0]), max(r.To, old[1])
	}
	a.forgotten[r.ID] = [2]int64{r.From, r.To}
}

// loadForgotten reads forgetFile into the map. A line that does not parse is skipped and counted.
// It runs in Open, before the writer goroutine starts.
func (a *Archive) loadForgotten() {
	b, err := os.ReadFile(filepath.Join(a.root, forgetFile))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("session archive: could not read the forget records", "error", err)
		}
		return
	}
	// A crash during an append can leave a torn last line. The next append must not join it.
	a.forgetTorn = len(b) > 0 && b[len(b)-1] != '\n'
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		a.forgetLines++
		var r forgetRecord
		if json.Unmarshal(line, &r) != nil || r.ID == "" {
			a.badForgetLines.Add(1)
			continue
		}
		a.mergeForgotten(r)
	}
	if n := a.badForgetLines.Load(); n > 0 {
		slog.Warn("session archive: some forget records could not be read and are skipped", "lines", n)
	}
}

// forget writes a forget record for each of ss that holds tokens or cost, before the caller
// removes them from disk. Writer goroutine only.
//
// The map gets each record first, so this process uses it even when the write fails. A failed
// write does not stop the remove: removes are how disk space comes back after a full disk.
func (a *Archive) forget(ss []*sessionState) {
	if a.ledgerDays == 0 {
		return
	}
	var buf []byte
	for _, s := range ss {
		sum := s.meta.Summary.Summary(s.meta.ID)
		if sum.LastRequestAt.IsZero() || (sum.TotalTokens <= 0 && sum.CostMicros <= 0) {
			continue
		}
		r := forgetRecord{ID: s.meta.ID, From: s.meta.CreatedAt.UnixMilli(), To: sum.LastRequestAt.UnixMilli()}
		a.mergeForgotten(r)
		line, _ := json.Marshal(r) // three plain fields: cannot fail
		buf = append(append(buf, line...), '\n')
		a.forgetLines++
	}
	if len(buf) == 0 {
		return
	}
	if a.forgetTorn {
		buf = append([]byte("\n"), buf...)
	}
	path := filepath.Join(a.root, forgetFile)
	_, statErr := os.Stat(path)
	err := writeSync(path, os.O_APPEND, buf)
	if err == nil && errors.Is(statErr, os.ErrNotExist) {
		err = syncDir(a.root)
	}
	if err != nil {
		a.forgetTorn = true // a short write may have left part of a line
		a.forgetWriteErrors.Add(1)
		slog.Warn("session archive: could not write a forget record; after a restart an import can count the session again",
			"error", err)
		return
	}
	a.forgetTorn = false
}

// compactForgotten drops records whose span ends before the ledger can hold its turns, and
// rewrites forgetFile when at least half of its lines are dead. Writer goroutine only, or Open.
func (a *Archive) compactForgotten(now time.Time) {
	if a.ledgerDays == 0 {
		return
	}
	// One day more than the ledger's cutoff, which is midnight minus ledgerDays-1 days: the
	// ledger keeps whole-day files.
	y, m, d := now.Date()
	expireBefore := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -a.ledgerDays).UnixMilli()
	a.fgMu.Lock()
	for id, span := range a.forgotten {
		if span[1] < expireBefore {
			delete(a.forgotten, id)
		}
	}
	live := make([]forgetRecord, 0, len(a.forgotten))
	for id, span := range a.forgotten {
		live = append(live, forgetRecord{ID: id, From: span[0], To: span[1]})
	}
	a.fgMu.Unlock()

	if a.forgetLines == 0 || (a.forgetLines-len(live))*2 < a.forgetLines {
		return
	}
	var buf []byte
	for _, r := range live {
		line, _ := json.Marshal(r)
		buf = append(append(buf, line...), '\n')
	}
	path := filepath.Join(a.root, forgetFile)
	tmp := filepath.Join(a.root, "."+forgetFile+".tmp")
	err := writeSync(tmp, os.O_TRUNC, buf)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		err = syncDir(a.root)
	}
	if err != nil {
		os.Remove(tmp)
		slog.Warn("session archive: could not compact the forget records", "error", err)
		return
	}
	a.forgetLines, a.forgetTorn = len(live), false
}

// writeSync writes b to path, opened with flag, and fsyncs it.
func writeSync(path string, flag int, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|flag, fileMode)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	return errors.Join(werr, serr, f.Close())
}

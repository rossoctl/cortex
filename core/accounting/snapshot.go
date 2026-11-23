package accounting

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// snapshotEvery is how often the Tracker state is saved while the process runs. A SIGKILL loses
// at most this much state; Close saves the rest.
const snapshotEvery = 60 * time.Second

// snapshotMaxAge is the oldest snapshot Restore accepts. No provider cache lives longer than 1h,
// so older state can only price a cache that has expired (design §5.4; the proxy refuses the
// same).
const snapshotMaxAge = time.Hour

// snapshotMaxBytes caps the saved file. The library writes every thread it holds, up to its
// 10,000-thread cap and with no age limit, so on a laptop the file grows for weeks; at the cap it
// can pass 300 MB, rewritten and synced on every save (rossoctl/accountant#88 asks for a cutoff).
// Past this size the threads seen longest ago are left out of the file (trimSnapshot); they stay
// in memory, so only a restart forgets them, and a thread that old has most likely no live cache
// left. If the format is one trimSnapshot does not know, the file is removed instead and a restart
// starts fresh. A var so a test can lower it.
var snapshotMaxBytes = 20 << 20 // 20 MiB

// snapshotFileOverhead is what the file holds besides the encoded state: the JSON object, its two
// keys and the savedAt timestamp, with room to spare.
const snapshotFileOverhead = 128

// snapshotStateBudget is the most state bytes that fit a file of fileMax bytes. The file carries the
// state as base64, a third larger than the state, so the state budget is three quarters of what is
// left after the overhead; budgeting the raw state against fileMax would let the file reach 4/3 of
// the cap.
func snapshotStateBudget(fileMax int) int {
	return base64.StdEncoding.DecodedLen(max(0, fileMax-snapshotFileOverhead))
}

// trimSnapshotFn is trimSnapshot, a var so a test can reach the fallback for a format it cannot
// trim (the library writes only the version trimSnapshot knows).
var trimSnapshotFn = trimSnapshot

// snapshotFile is the file's form. State is accountant.Trackers.Snapshot's JSON, which encoding/json
// writes as base64 for a []byte. SavedAt is checked, not the file's mtime: a copy or a restore
// from backup changes the mtime and leaves the content's age as it was.
type snapshotFile struct {
	SavedAt time.Time `json:"savedAt"`
	State   []byte    `json:"state"`
}

// saveLoop saves the state every snapshotEvery until Close.
func (h *Host) saveLoop() {
	defer h.saver.Done()
	t := time.NewTicker(snapshotEvery)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			// Nothing changed since the last save: skip it, and the tracker lock with it (the
			// library holds that lock for the whole marshal).
			if !h.dirty.Swap(false) {
				continue
			}
			if err := h.save(); err != nil {
				h.dirty.Store(true) // try again next tick
				slog.Warn("accounting: could not save the accountant state", "path", h.snapshotPath, "error", err)
			}
		}
	}
}

// save writes the snapshot atomically: a temp file in the same directory, synced, then a rename,
// so a reader never sees half a file. Mode 0600: the state holds no content, but it holds session ids.
func (h *Host) save() error {
	state, err := h.trackers.Snapshot()
	if err != nil {
		return err
	}
	if budget := snapshotStateBudget(snapshotMaxBytes); len(state) > budget {
		trimmed, dropped, terr := trimSnapshotFn(state, budget)
		if terr != nil {
			// A format this code does not know: no guess at its shape. Remove the old file so a
			// restart does not load out-of-date state; the restart starts fresh.
			if !h.snapshotTooBig {
				h.snapshotTooBig = true
				slog.Warn("accounting: the accountant state is too large to save and could not be trimmed; a restart will start fresh",
					"path", h.snapshotPath, "bytes", len(state), "limit", snapshotMaxBytes, "error", terr)
			}
			if err := os.Remove(h.snapshotPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		// WARN once per crossing, not on every save: the periodic save runs every minute.
		if !h.snapshotTooBig {
			h.snapshotTooBig = true
			slog.Warn("accounting: the accountant state is over its size limit; the threads seen longest ago are left out of the saved file",
				"path", h.snapshotPath, "bytes", len(state), "limit", snapshotMaxBytes, "threadsLeftOut", dropped)
		}
		state = trimmed
	} else {
		h.snapshotTooBig = false
	}
	data, err := json.Marshal(snapshotFile{SavedAt: h.now().UTC(), State: state})
	if err != nil {
		return err
	}
	dir := filepath.Dir(h.snapshotPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(h.snapshotPath)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has moved it
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Synced before the rename, or a power loss can leave the renamed file empty (#1547).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, h.snapshotPath)
}

// restore loads the saved state. A missing file is a first start and says nothing. A corrupt or
// stale one starts fresh with one WARN: the figures it would give are wrong or expired, and
// accounting fails open.
func (h *Host) restore() {
	data, err := os.ReadFile(h.snapshotPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		slog.Warn("accounting: could not read the accountant state; starting fresh", "path", h.snapshotPath, "error", err)
		return
	}
	if err := h.restoreFrom(data); err != nil {
		slog.Warn("accounting: the saved accountant state is not usable; starting fresh", "path", h.snapshotPath, "error", err)
	}
}

func (h *Host) restoreFrom(data []byte) error {
	var f snapshotFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	if f.SavedAt.IsZero() {
		return fmt.Errorf("no savedAt")
	}
	age := h.now().Sub(f.SavedAt)
	if age < 0 {
		return fmt.Errorf("saved %s in the future (the clock stepped back?): the state's age is unknown", (-age).Round(time.Second))
	}
	if age > snapshotMaxAge {
		return fmt.Errorf("saved %s ago, older than %s: every cache it describes has expired", age.Round(time.Second), snapshotMaxAge)
	}
	return h.trackers.Restore(f.State)
}

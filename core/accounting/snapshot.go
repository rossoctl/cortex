package accounting

import (
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

// snapshotMaxBytes caps the saved state. The library writes every thread it holds, up to its
// 10,000-thread cap and with no age limit, so on a laptop the file grows for weeks; at the cap it
// can pass 300 MB, rewritten and synced on every save (rossoctl/accountant#88 asks for a cutoff).
// Past this size the state is not saved, and the old file is removed so a restart does not load
// state that is out of date: a restart then starts fresh, which costs the figures of caches that
// were still warm, nothing more. A var so a test can lower it.
var snapshotMaxBytes = 20 << 20 // 20 MiB

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
			// library holds that lock for the whole marshal; review finding).
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
	if len(state) > snapshotMaxBytes {
		// WARN once per crossing, not on every save: the periodic save runs every minute.
		if !h.snapshotTooBig {
			h.snapshotTooBig = true
			slog.Warn("accounting: the accountant state is too large to save; a restart will start fresh",
				"path", h.snapshotPath, "bytes", len(state), "limit", snapshotMaxBytes)
		}
		if err := os.Remove(h.snapshotPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	h.snapshotTooBig = false
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
	if age := h.now().Sub(f.SavedAt); age > snapshotMaxAge {
		return fmt.Errorf("saved %s ago, older than %s: every cache it describes has expired", age.Round(time.Second), snapshotMaxAge)
	}
	return h.trackers.Restore(f.State)
}

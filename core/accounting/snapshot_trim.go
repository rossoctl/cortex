package accounting

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// trimmableSnapshotVersion is the accountant snapshot format trimSnapshot knows (accountant
// v0.3.0, internal/engine/persist.go snapshotVersion). A snapshot of any other version is not
// trimmed: save falls back to removing the file.
const trimmableSnapshotVersion = 1

// trimSnapshot returns state with the threads seen longest ago left out, so the result is at most
// max bytes. It keeps the newest threads whole and in their order; dropped is how many it left out.
//
// A STOPGAP until the library can do it (rossoctl/accountant#88, SnapshotSince). It reads only the
// format's version and each thread's age, the same age the library's own trim uses: the previous
// request's At, else the thread's Created. Every thread it keeps is the library's own bytes,
// unparsed, so nothing here rebuilds library state; Restore validates the result at the next start.
// An unknown version, or a shape it does not recognise, is an error, never a guess.
func trimSnapshot(state []byte, max int) (out []byte, dropped int, err error) {
	var snap struct {
		Version int                `json:"version"`
		Threads *[]json.RawMessage `json:"threads"`
	}
	if err := json.Unmarshal(state, &snap); err != nil {
		return nil, 0, fmt.Errorf("snapshot not readable: %w", err)
	}
	if snap.Version != trimmableSnapshotVersion {
		return nil, 0, fmt.Errorf("snapshot version %d, can trim only %d", snap.Version, trimmableSnapshotVersion)
	}
	if snap.Threads == nil {
		return nil, 0, fmt.Errorf("snapshot has no threads field")
	}
	threads := *snap.Threads

	type aged struct {
		i  int
		at time.Time
	}
	ages := make([]aged, len(threads))
	for i, raw := range threads {
		var t struct {
			Created   time.Time `json:"created"`
			PrevPrint *struct {
				At time.Time `json:"at"`
			} `json:"prevPrint"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, 0, fmt.Errorf("snapshot thread %d not readable: %w", i, err)
		}
		at := t.Created
		if t.PrevPrint != nil && !t.PrevPrint.At.IsZero() {
			at = t.PrevPrint.At
		}
		ages[i] = aged{i, at}
	}
	// Newest first; equal ages keep the snapshot's own (key) order.
	sort.SliceStable(ages, func(a, b int) bool { return ages[a].at.After(ages[b].at) })

	const frame = len(`{"version":1,"threads":[]}`)
	size := frame
	keep := make([]bool, len(threads))
	for _, a := range ages {
		n := len(threads[a.i]) + 1 // and its comma
		if size+n > max {
			break
		}
		size += n
		keep[a.i] = true
	}
	kept := make([]json.RawMessage, 0, len(threads))
	for i, raw := range threads {
		if keep[i] {
			kept = append(kept, raw)
		}
	}
	out, err = json.Marshal(struct {
		Version int               `json:"version"`
		Threads []json.RawMessage `json:"threads"`
	}{snap.Version, kept})
	if err != nil {
		return nil, 0, err
	}
	return out, len(threads) - len(kept), nil
}

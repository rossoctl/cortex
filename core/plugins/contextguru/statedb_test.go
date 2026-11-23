package contextguru

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	cgstore "github.com/rossoctl/context-guru/store"
)

// freshPlugin is a plugin configured the default way, with the state database in the test's
// directory.
func freshPlugin(t *testing.T) *ContextGuru {
	t.Helper()
	p := New()
	if err := p.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return p
}

// The session state survives the plugin: what one instance wrote, a fresh instance (a restart, or
// a config reload) loads on the session's next request.
func TestStateDB_StateSurvivesANewInstance(t *testing.T) {
	first := freshPlugin(t)
	if first.stateDisk.Load() == nil {
		t.Fatal("no database attached")
	}
	first.store.Put(cgstore.SumPrefix+"s-persist", []byte("SUMMARY"))
	first.store.Put(cgstore.BilledPrefix+"s-persist", []byte("946000"))
	if err := first.Shutdown(context.Background()); err != nil { // flushes
		t.Fatal(err)
	}

	second := freshPlugin(t)
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	mem, ok := second.store.(*cgstore.Memory)
	if !ok {
		t.Fatalf("store is %T, want *store.Memory", second.store)
	}
	mem.LoadSession("s-persist") // what the engine does on the session's next request
	if got, ok := mem.Get(cgstore.SumPrefix + "s-persist"); !ok || string(got) != "SUMMARY" {
		t.Errorf("summary after a new instance = %q, %v; want SUMMARY", got, ok)
	}
	if got, ok := mem.Get(cgstore.BilledPrefix + "s-persist"); !ok || string(got) != "946000" {
		t.Errorf("billed input after a new instance = %q, %v; want 946000", got, ok)
	}
}

// The file holds text made from the conversation, so it is the owner's alone.
func TestStateDB_FileIsPrivate(t *testing.T) {
	p := freshPlugin(t)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	path, err := stateDBPathFor(false)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
}

// With no usable place for the file, the state stays in memory and the plugin still works.
func TestStateDB_FailsOpenToMemory(t *testing.T) {
	prev := stateDBPathFor
	t.Cleanup(func() { stateDBPathFor = prev })
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDBPathFor = func(bool) (string, error) { return filepath.Join(blocker, stateDBFileName), nil }

	p := freshPlugin(t)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if p.stateDisk.Load() != nil {
		t.Error("a database was attached at a path that cannot exist")
	}
	p.store.Put(cgstore.SumPrefix+"s-mem", []byte("X"))
	if got, ok := p.store.Get(cgstore.SumPrefix + "s-mem"); !ok || string(got) != "X" {
		t.Errorf("in-memory store = %q, %v; want X", got, ok)
	}
}

// store.enabled: false gives a no-op store, which has no state to keep: no database.
func TestStateDB_NoneWhenTheStoreIsOff(t *testing.T) {
	p := New()
	if err := p.Configure([]byte(`{"engine":{"preset":"balanced","store":{"enabled":false}}}`)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if p.stateDB != nil || p.stateDisk.Load() != nil {
		t.Error("a database was attached to a disabled store")
	}
}

// Configure starts nothing: the disk flusher starts in Init, so a pipeline that a failed build or
// reload drops before Start holds no goroutine. Init then attaches it, and Shutdown closes it.
func TestStateDB_ConfigureStartsNoFlusher(t *testing.T) {
	p := New()
	if err := p.Configure(nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.stateDB == nil {
		t.Fatal("Configure opened no database")
	}
	if p.stateDisk.Load() != nil {
		t.Fatal("Configure started a disk copy; it belongs in Init")
	}
	if err := p.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.stateDisk.Load() == nil {
		t.Fatal("Init attached no disk copy")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.stateDisk.Load() != nil {
		t.Error("Shutdown left the disk copy in place")
	}
}

// The state keys get the database's one-day memory lifetime only when a database backs them.
// On the fail-open path the engine's own default (10000s) stays, so a summary is gone from memory
// after three hours.
func TestStateDB_DayTTLOnlyWithADatabase(t *testing.T) {
	survives3h := func(p *ContextGuru) bool {
		t.Helper()
		mem, ok := p.store.(*cgstore.Memory)
		if !ok {
			t.Fatalf("store is %T, want *store.Memory", p.store)
		}
		now := time.Now()
		mem.SetClock(func() time.Time { return now })
		mem.Put(cgstore.SumPrefix+"s-ttl", []byte("S"))
		now = now.Add(3 * time.Hour)
		_, ok = mem.Get(cgstore.SumPrefix + "s-ttl")
		return ok
	}
	withDB := freshPlugin(t)
	t.Cleanup(func() { _ = withDB.Shutdown(context.Background()) })
	if !survives3h(withDB) {
		t.Error("with a database, a summary is gone from memory after 3h; want the 24h state lifetime")
	}

	prev := stateDBPathFor
	t.Cleanup(func() { stateDBPathFor = prev })
	stateDBPathFor = func(bool) (string, error) { return "", errors.New("no place") }
	noDB := freshPlugin(t)
	t.Cleanup(func() { _ = noDB.Shutdown(context.Background()) })
	if survives3h(noDB) {
		t.Error("with no database, a summary is still in memory after 3h; want the engine's default lifetime")
	}
}

// Metrics reports the disk copy's failed operations, so a database that stopped persisting shows
// after its first WARN has scrolled by.
func TestStateDB_ErrorsAreAMetric(t *testing.T) {
	p := freshPlugin(t)
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	for _, m := range p.Metrics() {
		if m.Name == "state_db_errors" {
			if m.Value != 0 {
				t.Errorf("state_db_errors = %v on a healthy database, want 0", m.Value)
			}
			return
		}
	}
	t.Error("Metrics has no state_db_errors")
}

// The path reaches SQLite as a file: URI, where '#' starts a fragment, '?' a query and '%XX' an
// escape. Each must name the intended 0600 file, and nothing may appear beside the directory.
func TestStateDB_PathWithURICharacters(t *testing.T) {
	for _, name := range []string{"a#b", "a?b", "a%41b"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			path := filepath.Join(parent, name, stateDBFileName)
			db, err := openStateDB(path)
			if err != nil {
				t.Fatalf("openStateDB: %v", err)
			}
			t.Cleanup(func() {
				stateDBMu.Lock()
				delete(stateDBs, path)
				stateDBMu.Unlock()
				_ = db.Close()
			})
			if _, err := db.Exec(`CREATE TABLE probe (x INTEGER)`); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Size() == 0 {
				t.Errorf("%s is empty: SQLite wrote another file", path)
			}
			if mode := fi.Mode().Perm(); mode != 0o600 {
				t.Errorf("mode = %o, want 600", mode)
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != name {
					t.Errorf("SQLite wrote %q beside the state directory", e.Name())
				}
			}
		})
	}
}

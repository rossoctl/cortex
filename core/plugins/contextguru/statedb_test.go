package contextguru

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	return p
}

// The session state survives the plugin: what one instance wrote, a fresh instance (a restart, or
// a config reload) loads on the session's next request.
func TestStateDB_StateSurvivesANewInstance(t *testing.T) {
	first := freshPlugin(t)
	if first.stateDisk == nil {
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
	if p.stateDisk != nil {
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
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	if p.stateDisk != nil {
		t.Error("a database was attached to a disabled store")
	}
}

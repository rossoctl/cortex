package filestore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/storage"
)

var ctx = context.Background()

// clock is a settable time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// openT opens a store at path on clk, flushing only on Close unless the test sets an
// interval, and closes it when the test ends.
func openT(t *testing.T, path string, clk *clock, opts ...option) *Store {
	t.Helper()
	opts = append([]option{withClock(clk.now), withFlushEvery(time.Hour)}, opts...)
	s, err := openWith(path, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)} }

func get(t *testing.T, s *Store, key string) string {
	t.Helper()
	v, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return v
}

func TestStore_AMissingKeyReadsEmpty(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "s.json"), newClock())
	if v := get(t, s, "nope"); v != "" {
		t.Errorf("Get = %q, want empty", v)
	}
	if h, err := s.HashGet(ctx, "nope"); err != nil || len(h) != 0 {
		t.Errorf("HashGet = %v, %v; want an empty map", h, err)
	}
}

func TestStore_ATTLExpiresTheKeyAndSetReplacesIt(t *testing.T) {
	clk := newClock()
	s := openT(t, filepath.Join(t.TempDir(), "s.json"), clk)
	if err := s.Set(ctx, "k", "v1", time.Minute); err != nil {
		t.Fatal(err)
	}
	clk.advance(59 * time.Second)
	if v := get(t, s, "k"); v != "v1" {
		t.Fatalf("Get before expiry = %q, want v1", v)
	}
	// Set with no TTL replaces the value and the expiry, as redis SET does.
	if err := s.Set(ctx, "k", "v2", 0); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	if v := get(t, s, "k"); v != "v2" {
		t.Fatalf("Get = %q, want v2 kept with no expiry", v)
	}
	if err := s.Set(ctx, "k", "v3", time.Minute); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	if v := get(t, s, "k"); v != "" {
		t.Errorf("Get at expiry = %q, want the key gone", v)
	}
}

func TestStore_ExpireSetsATTLDeletesAtZeroAndIgnoresAMissingKey(t *testing.T) {
	clk := newClock()
	s := openT(t, filepath.Join(t.TempDir(), "s.json"), clk)
	_ = s.Set(ctx, "k", "v", 0)
	if err := s.Expire(ctx, "k", time.Minute); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	if v := get(t, s, "k"); v != "" {
		t.Errorf("Get after Expire's TTL = %q, want the key gone", v)
	}
	_ = s.Set(ctx, "k", "v", 0)
	if err := s.Expire(ctx, "k", 0); err != nil {
		t.Fatal(err)
	}
	if v := get(t, s, "k"); v != "" {
		t.Errorf("Get after Expire(0) = %q, want the key deleted", v)
	}
	if err := s.Expire(ctx, "missing", time.Minute); err != nil {
		t.Errorf("Expire on a missing key = %v, want nil", err)
	}
	if v := get(t, s, "missing"); v != "" {
		t.Errorf("Expire created %q", v)
	}
}

func TestStore_IncrCountsFromZeroAndKeepsTheTTL(t *testing.T) {
	clk := newClock()
	s := openT(t, filepath.Join(t.TempDir(), "s.json"), clk)
	if n, err := s.Incr(ctx, "n", 3); err != nil || n != 3 {
		t.Fatalf("Incr = %d, %v; want 3", n, err)
	}
	_ = s.Expire(ctx, "n", time.Minute)
	if n, err := s.Incr(ctx, "n", -1); err != nil || n != 2 {
		t.Fatalf("Incr = %d, %v; want 2", n, err)
	}
	clk.advance(time.Minute)
	if v := get(t, s, "n"); v != "" {
		t.Errorf("Get = %q, want the TTL Expire set kept by Incr", v)
	}
	_ = s.Set(ctx, "word", "abc", 0)
	if _, err := s.Incr(ctx, "word", 1); err == nil {
		t.Error("Incr on a non-integer = nil error, want one")
	}
}

func TestStore_HashOperations(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "s.json"), newClock())
	if n, err := s.HashIncr(ctx, "h", "a", 2); err != nil || n != 2 {
		t.Fatalf("HashIncr = %d, %v; want 2", n, err)
	}
	if n, err := s.HashIncr(ctx, "h", "a", 5); err != nil || n != 7 {
		t.Fatalf("HashIncr = %d, %v; want 7", n, err)
	}
	if set, err := s.HashSetNX(ctx, "h", "b", "x"); err != nil || !set {
		t.Fatalf("HashSetNX new field = %v, %v; want true", set, err)
	}
	if set, err := s.HashSetNX(ctx, "h", "b", "y"); err != nil || set {
		t.Fatalf("HashSetNX existing field = %v, %v; want false", set, err)
	}
	h, err := s.HashGet(ctx, "h")
	if err != nil || h["a"] != "7" || h["b"] != "x" || len(h) != 2 {
		t.Fatalf("HashGet = %v, %v", h, err)
	}
	h["a"] = "changed"
	if again, _ := s.HashGet(ctx, "h"); again["a"] != "7" {
		t.Error("HashGet returned the store's own map")
	}
}

// A key holds a string or a hash, and using it as the other is an error, as redis
// answers WRONGTYPE.
func TestStore_UsingAKeyAsTheOtherTypeIsAnError(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "s.json"), newClock())
	_ = s.Set(ctx, "str", "v", 0)
	_, _ = s.HashIncr(ctx, "hash", "f", 1)
	if _, err := s.Get(ctx, "hash"); err == nil {
		t.Error("Get on a hash = nil error")
	}
	if _, err := s.Incr(ctx, "hash", 1); err == nil {
		t.Error("Incr on a hash = nil error")
	}
	if _, err := s.HashGet(ctx, "str"); err == nil {
		t.Error("HashGet on a string = nil error")
	}
	if _, err := s.HashIncr(ctx, "str", "f", 1); err == nil {
		t.Error("HashIncr on a string = nil error")
	}
	if _, err := s.HashSetNX(ctx, "str", "f", "v"); err == nil {
		t.Error("HashSetNX on a string = nil error")
	}
}

// The point of the store: what it holds survives the process. A key keeps its TTL
// across the reopen, and one that expired while the store was closed is gone.
func TestStore_ReopeningRestoresWhatCloseSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	clk := newClock()
	s, err := openWith(path, withClock(clk.now), withFlushEvery(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Set(ctx, "kept", "v", 0)
	_ = s.Set(ctx, "short", "v", time.Minute)
	_ = s.Set(ctx, "long", "v", time.Hour)
	_, _ = s.HashIncr(ctx, "h", "f", 4)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	clk.advance(2 * time.Minute)
	r := openT(t, path, clk)
	for key, want := range map[string]string{"kept": "v", "short": "", "long": "v"} {
		if v := get(t, r, key); v != want {
			t.Errorf("after reopen Get(%q) = %q, want %q", key, v, want)
		}
	}
	if h, _ := r.HashGet(ctx, "h"); h["f"] != "4" {
		t.Errorf("after reopen HashGet = %v, want f=4", h)
	}
	clk.advance(time.Hour)
	if v := get(t, r, "long"); v != "" {
		t.Errorf("Get(long) = %q, want its TTL kept across the reopen", v)
	}
}

// A change is saved while the store is open, not only at Close, so a process that
// dies without closing it loses at most one interval.
func TestStore_SavesAChangeWithinTheFlushInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	tick := make(chan time.Time)
	s := openT(t, path, newClock(), withTick(tick))
	_ = s.Set(ctx, "k", "v", 0)

	// The loop takes the second tick only once it has saved after the first.
	tick <- time.Time{}
	tick <- time.Time{}
	r, err := openWith(path, withFlushEvery(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if v, _ := r.Get(ctx, "k"); v != "v" {
		t.Fatal("the change was not saved while the store was open")
	}
}

// A file that does not parse is set aside rather than refusing to open: the store
// holds state worth keeping, not state worth failing a proxy start over.
func TestStore_AnUnreadableFileIsSetAsideAndTheStoreStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openT(t, path, newClock())
	if v := get(t, s, "k"); v != "" {
		t.Errorf("Get = %q, want an empty store", v)
	}
	if b, err := os.ReadFile(path + ".corrupt"); err != nil || string(b) != "{not json" {
		t.Errorf("set-aside copy = %q, %v; want the unreadable file kept", b, err)
	}
}

// The file names sessions, so it is the user's alone, as the session archive is.
func TestStore_TheFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dir", "s.json")
	s := openT(t, path, newClock())
	_ = s.Set(ctx, "k", "v", 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %v, want 0600", perm)
	}
	di, _ := os.Stat(filepath.Dir(path))
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %v, want 0700", perm)
	}
}

func TestStore_AfterCloseOperationsFail(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "s.json"), newClock())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if err := s.Set(ctx, "k", "v", 0); err == nil {
		t.Error("Set after Close = nil error")
	}
}

// storage.Open reaches the driver by the file scheme, as it reaches redis by
// "redis".
func TestOpen_IsRegisteredForTheFileScheme(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	st, err := storage.Open("file", "file://"+path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer st.Close()
	if err := st.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the scheme's path was not the store's file: %v", err)
	}
}

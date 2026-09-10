package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// #868: a yanked file has to be findable. os.TempDir() on macOS is
// /var/folders/<opaque>/T, so the old path ran 92 characters and nobody could
// retype it or guess where to look.
//
// The directory is under ~/.cortex, not /tmp: a fixed path in a world-writable
// directory cannot enforce its own mode (os.MkdirAll returns nil for an existing
// path whatever its owner), can be pre-created as a symlink that redirects where
// events land, and is squatted by whoever yanks first on a shared host. Pins the
// directory, the 0600 perms, and the absence of the redundant name prefix.
func TestYankEventToFileUsesPrivatePerUserDir(t *testing.T) {
	p := yankOne(t)

	want := mustYankDir(t)
	if got := filepath.Dir(p); got != want {
		t.Errorf("yanked into %q, want %q", got, want)
	}
	base := filepath.Base(p)
	if strings.HasPrefix(base, "abctl-event-") {
		t.Errorf("filename %q still carries the abctl-event- prefix, which is "+
			"redundant inside %s", base, want)
	}
	if !strings.HasSuffix(base, ".json") {
		t.Errorf("filename %q lost its .json suffix", base)
	}

	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// Load-bearing, not cosmetic: events carry identity subjects, raw LLM
	// completions and tool arguments.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perms %v, want 0600 — yanked events are operator-only", perm)
	}

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var back pipeline.SessionEvent
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("yanked file is not valid JSON: %v", err)
	}
	if back.Host != "api.example.com" {
		t.Errorf("round-tripped Host = %q, want api.example.com", back.Host)
	}
}

// The timestamp is only second-granular, so the random tail is what stops a
// second yank in the same second from silently clobbering the first. Guards
// against "simplifying" to a stable filename.
func TestYankEventToFileTwiceSameSecondDistinct(t *testing.T) {
	p1 := yankOne(t)
	p2 := yankOne(t)

	if p1 == p2 {
		t.Fatalf("two yanks produced the same path %q — the second overwrote "+
			"the first", p1)
	}
	for _, p := range []string{p1, p2} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%q does not exist: %v", p, err)
		}
	}
}

// The other half of #868: the path used to vanish after flashDuration, before a
// user could copy it. Pressing `y` now leaves it up until the next keypress.
func TestYankFlashIsStickyUntilKeypress(t *testing.T) {
	m := newTestDetailModel(t)

	if cmd := m.handleKey(keyRune('y')); cmd != nil {
		t.Log("y returned a command; what matters is the footer below")
	}
	path := strings.TrimPrefix(m.flash, "yanked → ")
	t.Cleanup(func() { os.Remove(path) })
	if !strings.Contains(m.flash, "yanked → ") {
		t.Fatalf("y did not flash a yank message, got %q", m.flash)
	}
	if !strings.Contains(m.footerView(), mustYankDir(t)) {
		t.Fatal("the yanked path is not rendered in the footer")
	}

	// Past the point where a timed flash would have expired. flashUntil is left
	// zero by setStickyFlash, so this is already long past.
	if !m.flashSticky {
		t.Error("yank flash is not sticky")
	}
	if !strings.Contains(m.footerView(), mustYankDir(t)) {
		t.Error("the path stopped rendering despite being sticky — a user who " +
			"looked away has lost it again (#868)")
	}

	// Any key dismisses it.
	m.handleKey(keyRune('j'))
	if m.flashSticky {
		t.Error("a keypress did not clear the sticky flag")
	}
	if strings.Contains(m.footerView(), mustYankDir(t)) {
		t.Error("the notice survived a keypress")
	}
}

// Stickiness is yank-only. flashDuration is shared with ten other producers
// (hot-reload failures, fetch errors), and this change must not have made those
// stay up forever.
func TestNonYankFlashStillExpires(t *testing.T) {
	m := newTestDetailModel(t)

	m.setFlash("hot-reload failed: boom")
	if m.flashSticky {
		t.Fatal("setFlash produced a sticky flash")
	}
	if !strings.Contains(m.footerView(), "hot-reload failed") {
		t.Fatal("flash not rendered while live")
	}

	// Expire it the way the 1Hz tick would observe.
	m.flashUntil = time.Now().Add(-time.Second)
	if strings.Contains(m.footerView(), "hot-reload failed") {
		t.Error("a timed flash outlived flashUntil")
	}
}

// A sticky flash must not survive a later timed one — otherwise a yank notice
// would pin the footer past an error the operator needs to see.
func TestTimedFlashClearsStickiness(t *testing.T) {
	m := newTestDetailModel(t)
	m.setStickyFlash("yanked → /home/u/.cortex/abctl-events/x.json")
	m.setFlash("catalog fetch failed: boom")

	if m.flashSticky {
		t.Error("a timed flash inherited the previous flash's stickiness")
	}
	m.flashUntil = time.Now().Add(-time.Second)
	if strings.Contains(m.footerView(), "catalog fetch failed") {
		t.Error("the timed flash did not expire")
	}
}

// The gap the reviewer identified: every other test here runs against a yankDir
// that either did not exist or was created by the test, so none exercised
// os.MkdirAll's existing-directory semantics — which is where the /tmp problem
// lived. MkdirAll returns nil for a path that already exists whatever its owner
// or mode, so it cannot tighten a loose one.
//
// Under ~/.cortex that is no longer a security question: the parent is 0700 and
// owned by the user, so nobody else can pre-create the directory, plant a symlink
// in its place, or squat the name. This asserts yank still works when the
// directory already exists — the common case on every run after the first — and
// documents that the mode of a pre-existing directory is not tightened.
func TestYankEventToFileWithPreExistingDir(t *testing.T) {
	dir := mustYankDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil { // deliberately looser
		t.Fatal(err)
	}

	p := yankOne(t)

	if got := filepath.Dir(p); got != dir {
		t.Errorf("yanked into %q, want %q", got, dir)
	}
	// The file's own mode is what protects the contents, and CreateTemp sets it
	// regardless of the directory's mode.
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perms %v, want 0600 — the directory's mode must not affect "+
			"the file's", perm)
	}
}

// A timed flash landing shortly before a yank must not keep the yanked path on
// screen after the dismissing keypress. setStickyFlash leaves flashSticky true
// and footerView's other arm reads flashUntil, so a deadline still in the future
// would outlive the dismissal. The other tests cannot catch this: they start from
// a zero-valued model, where that arm is already in the past.
func TestStickyFlash_ClearsAStaleDeadline(t *testing.T) {
	m := newTestDetailModel(t)

	m.setFlash("hot-reload succeeded") // flashUntil = now + flashDuration
	m.setStickyFlash("yanked → /x/y/z.json")
	if !m.flashUntil.IsZero() {
		t.Error("setStickyFlash left a stale flashUntil")
	}

	m.handleKey(keyRune('j')) // clears flashSticky
	if strings.Contains(m.footerView(), "yanked") {
		t.Error("the yanked path outlived the dismissing keypress, on the " +
			"previous timed flash's deadline")
	}
}

// mustYankDir resolves the yank directory or fails the test.
func mustYankDir(t *testing.T) string {
	t.Helper()
	d, err := yankDir()
	if err != nil {
		t.Fatalf("yankDir: %v", err)
	}
	return d
}

// yankOne writes one event via the real code path and schedules cleanup.
func yankOne(t *testing.T) string {
	t.Helper()
	p, err := yankEventToFile(sampleEvent())
	if err != nil {
		t.Fatalf("yankEventToFile: %v", err)
	}
	t.Cleanup(func() { os.Remove(p) })
	return p
}

func sampleEvent() *pipeline.SessionEvent {
	return &pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Outbound,
		Phase:     pipeline.SessionRequest,
		Host:      "api.example.com",
	}
}

// newTestDetailModel is a model sitting on the detail pane with an event
// focused, which is the only state where `y` yanks.
func newTestDetailModel(t *testing.T) *model {
	t.Helper()
	m := &model{
		pane:        paneDetail,
		detailEvent: sampleEvent(),
		width:       200,
		height:      40,
		bodyHeight:  12,
	}
	return m
}

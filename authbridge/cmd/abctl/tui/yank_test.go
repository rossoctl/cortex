package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

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
	yankHome(t)
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
	yankHome(t)
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
	yankHome(t)
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
	yankHome(t)
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

// Yank must work when the directory already exists, which is every run after the
// first. Pre-created at 0700 — the mode yank itself uses. (An earlier version of
// this test pre-created it at 0755 to document that MkdirAll does not tighten an
// existing directory; that is now refused outright, and TestYankRefusesALooseModeDir
// covers it.)
func TestYankEventToFileWithPreExistingDir(t *testing.T) {
	yankHome(t)
	dir := mustYankDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	p := yankOne(t)

	if got := filepath.Dir(p); got != dir {
		t.Errorf("yanked into %q, want %q", got, dir)
	}
	// The file's own mode is what protects the contents.
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perms %v, want 0600", perm)
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

// yankDir's error path must stay readable. An unset $HOME is the reachable case
// (os.UserHomeDir returns a non-nil "$HOME is not defined" error, so %w is fine);
// the home == "" && err == nil case is defensive and not reachable here, which is
// exactly why folding the two conditions together hid a "%!w(<nil>)" message
// nobody would ever see until they did. This asserts the reachable path reads
// well; the unreachable one is kept legible by construction, not by a test.
func TestYankDir_UnsetHomeGivesAReadableError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	_, err := yankDir()
	if err == nil {
		t.Skip("this platform resolved a home directory without $HOME")
	}
	if strings.Contains(err.Error(), "%!w") {
		t.Errorf("error message is mangled by a nil wrap: %v", err)
	}
	if !strings.Contains(err.Error(), "home directory") {
		t.Errorf("error does not say what went wrong: %v", err)
	}
}

// The must-fix: ~/.cortex is not guaranteed to be 0700 — abctl never creates it,
// so its mode is whatever an installer or the user left. At 0755, MkdirAll neither
// tightens the mode nor refuses to follow an abctl-events symlink, and the event —
// identity subjects, raw LLM completions, tool arguments — lands in whichever
// directory the symlink points at.
func TestYankRefusesASymlinkedDir(t *testing.T) {
	home := yankHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".cortex"), 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(home, ".cortex", "abctl-events")); err != nil {
		t.Fatal(err)
	}

	if _, err := yankEventToFile(sampleEvent()); err == nil {
		t.Error("wrote through a symlink; a local user can redirect session events")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("%d event file(s) landed in the symlink target", len(ents))
	}
}

// Same premise, without a symlink: a pre-existing world-readable yank directory
// must be refused rather than written into, since MkdirAll will not tighten it.
func TestYankRefusesALooseModeDir(t *testing.T) {
	home := yankHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".cortex", "abctl-events"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := yankEventToFile(sampleEvent())
	if err == nil {
		t.Fatal("wrote into a 0755 directory, so the 0700 guarantee is not enforced")
	}
	// The message has to tell the user how to fix it.
	if !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

// And the ordinary case still works: a clean home yanks without complaint.
func TestYankAcceptsACleanDir(t *testing.T) {
	yankHome(t)
	if _, err := yankEventToFile(sampleEvent()); err != nil {
		t.Errorf("clean home was refused: %v", err)
	}
}

// The reported symptom: on a ~72-column terminal the footer read
//
//	● connected  0.0 ev/s   drops: 0   yanked → /Users/snible/.cortex/abctl-
//
// and the filename — the part you retype — was off the right edge. A sticky flash
// now gets the whole line from column 0, and truncates from the LEFT so the tail
// survives.
//
// Widths are measured with lipgloss.Width, not len: "…" and "→" are multi-byte, so
// a byte count overstates the columns used.
func TestStickyFlash_FitsANarrowFooter(t *testing.T) {
	const path = "/Users/someone/.cortex/abctl-events/20260910-223650-80907711.json"

	for _, width := range []int{40, 60, 72, 80, 120} {
		m := newTestDetailModel(t)
		m.width = width
		m.setStickyFlash("yanked → " + path)

		line := strings.SplitN(m.footerView(), "\n", 2)[0]
		if got := lipgloss.Width(line); got > width {
			t.Errorf("width %d: footer is %d columns, overflowing by %d: %q",
				width, got, got-width, line)
		}
		// The filename must survive every truncation — it is what the user types.
		if !strings.Contains(line, "80907711.json") {
			t.Errorf("width %d: filename truncated away: %q", width, line)
		}
	}
}

// #8: a yank failure carries the chmod guidance that fixes it, so it must persist
// like the success case rather than vanishing after flashDuration.
func TestYankFailure_IsAlsoSticky(t *testing.T) {
	home := yankHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".cortex", "abctl-events"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newTestDetailModel(t)

	m.handleKey(keyRune('y'))

	if !strings.Contains(m.flash, "yank failed") {
		t.Fatalf("expected a failure flash, got %q", m.flash)
	}
	if !m.flashSticky {
		t.Error("the failure notice is timed, so the chmod guidance disappears " +
			"in three seconds while a success would persist")
	}
	if !strings.Contains(m.flash, "chmod 700") {
		t.Errorf("failure flash lost its actionable guidance: %q", m.flash)
	}
}

// #7: the wide-rune fixture whose absence let the column/rune confusion ship.
// fitFlashLine budgets in display columns; slicing by rune index instead made a
// CJK path asked to fit 40 columns render 55, since each kept rune was 2 wide.
func TestStickyFlash_FitsWithWideRunes(t *testing.T) {
	paths := map[string]string{
		"cjk":   "yanked → /Users/u/.cortex/abctl-events/日本語日本語日本語日本語-1234567890.json",
		"emoji": "yanked → /Users/u/.cortex/abctl-events/🎉🎉🎉🎉🎉-1234567890.json",
		"mixed": "yanked → /Users/u/.cortex/abctl-events/日本語-🎉-20260910-80907711.json",
	}
	for name, path := range paths {
		// Includes degenerate widths: the ellipsis alone is already 1 column.
		for _, width := range []int{1, 2, 10, 40, 60, 72, 200} {
			m := newTestDetailModel(t)
			m.width = width
			m.setStickyFlash(path)

			line := strings.SplitN(m.footerView(), "\n", 2)[0]
			if got := lipgloss.Width(line); got > width {
				t.Errorf("%s at width %d: %d columns, overflowing by %d: %q",
					name, width, got, got-width, line)
			}
		}
	}
}

// The full-width takeover applies to sticky flashes only. A timed flash keeps the
// connection state, rate and drops beside it — ten other producers use that path
// and none of them is a path the user is about to retype.
func TestTimedFlash_KeepsTheStatusPrefix(t *testing.T) {
	m := newTestDetailModel(t)
	m.width = 72
	m.setFlash("hot-reload succeeded")

	line := strings.SplitN(m.footerView(), "\n", 2)[0]
	if !strings.Contains(line, "ev/s") {
		t.Errorf("a timed flash lost the status prefix: %q", line)
	}
	if !strings.Contains(line, "hot-reload succeeded") {
		t.Errorf("a timed flash lost its message: %q", line)
	}
}

// yankHome redirects $HOME at a per-test temp directory, so nothing in this file
// touches the developer's real ~/.cortex.
//
// This is not tidiness. yankDir() resolves os.UserHomeDir(), so without it every
// test here wrote into the real home — and TestYankEventToFileWithPreExistingDir
// created ~/.cortex/abctl-events at 0755 on a machine where it did not yet exist.
// os.MkdirAll does not tighten an existing directory, so every subsequent REAL
// yank then wrote into a 0755 directory, silently voiding the 0700 guarantee the
// README, yankDir's doc comment and that test's own name all assert. Permanent,
// invisible, and only on a fresh machine — which is how it survived review.
//
// Call this first in every test that reaches yankDir, directly or through a
// helper. t.Setenv is incompatible with t.Parallel(); nothing here is parallel.
func yankHome(t *testing.T) string {
	t.Helper()
	// One directory, both variables: os.UserHomeDir reads USERPROFILE on Windows
	// and HOME elsewhere, and two separate t.TempDir() calls would make the two
	// disagree — so a test would exercise a different home depending on platform.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
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

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/tui"
)

// prefsHome points $HOME at a fresh temp dir and returns it.
//
// Sets USERPROFILE to the SAME directory: os.UserHomeDir reads that on Windows and
// HOME elsewhere, so two separate t.TempDir() calls would make the two disagree and
// the test would exercise a different home depending on platform. Same reasoning as
// tui's yankHome. t.Setenv is incompatible with t.Parallel; nothing here is parallel.
func prefsHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// TestUserConfigPath_DefaultsUnderCortexDir pins the path the issue specifies, so
// the constant cannot drift from what the README and the docs promise.
func TestUserConfigPath_DefaultsUnderCortexDir(t *testing.T) {
	home := prefsHome(t)
	got, err := userConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cortex", "abctl-config.yaml"); got != want {
		t.Errorf("userConfigPath() = %q, want %q", got, want)
	}
}

// TestUserConfigPath_NoHomeIsAnError: better to lose persistence than to drop a
// settings file into whatever directory abctl started in, where it would be found
// again only by accident.
func TestUserConfigPath_NoHomeIsAnError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if got, err := userConfigPath(); err == nil {
		t.Errorf("userConfigPath() = %q with no home, want an error", got)
	}
}

// TestLoadUserConfig_MissingFileIsSilent: nobody has this file until they change a
// setting, so a first run must not be scolded for it.
func TestLoadUserConfig_MissingFileIsSilent(t *testing.T) {
	home := prefsHome(t)
	var warn bytes.Buffer

	got := loadUserConfig(filepath.Join(home, ".cortex", "abctl-config.yaml"), &warn)

	if !reflectDeepEqualSettings(got, tui.UserSettings{}) {
		t.Errorf("got %+v, want the zero value", got)
	}
	if warn.Len() != 0 {
		t.Errorf("warned about a missing file: %q", warn.String())
	}
}

// TestLoadUserConfig_EmptyPathIsSilent covers the no-home case main passes through.
func TestLoadUserConfig_EmptyPathIsSilent(t *testing.T) {
	var warn bytes.Buffer
	if got := loadUserConfig("", &warn); !reflectDeepEqualSettings(got, tui.UserSettings{}) {
		t.Errorf("got %+v, want the zero value", got)
	}
	if warn.Len() != 0 {
		t.Errorf("warned on an empty path: %q", warn.String())
	}
}

// TestLoadUserConfig_MalformedWarnsAndFallsBack: an unusable file must not stop the
// viewer from opening, but ignoring a hand-edited file silently would look like
// abctl disregarding the edit. So: defaults, plus one line naming the path.
func TestLoadUserConfig_MalformedWarnsAndFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		dir  bool
	}{
		{name: "unclosed flow sequence", body: "events: [broken\n"},
		{name: "top-level list", body: "- not\n- a mapping\n"},
		{name: "wrong type for a known key", body: "filter:\n  nested: map\n"},
		{name: "a directory where a file belongs", dir: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := prefsHome(t)
			path := filepath.Join(home, ".cortex", "abctl-config.yaml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.dir {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}

			var warn bytes.Buffer
			got := loadUserConfig(path, &warn) // must not panic

			if !reflectDeepEqualSettings(got, tui.UserSettings{}) {
				t.Errorf("got %+v, want the zero value", got)
			}
			if !strings.Contains(warn.String(), path) {
				t.Errorf("warning %q does not name the path the user must fix", warn.String())
			}
		})
	}
}

// TestLoadUserConfig_PartialParseIsDiscarded: yaml.v3 can populate some fields
// before failing. Half a config applied silently is harder to diagnose than none.
func TestLoadUserConfig_PartialParseIsDiscarded(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// filter parses; events does not.
	body := "filter: kept-if-partial\nevents:\n  columns: not-a-list\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	if got := loadUserConfig(path, &warn); got.Filter != "" {
		t.Errorf("filter = %q; a failed parse must discard everything", got.Filter)
	}
	if warn.Len() == 0 {
		t.Error("no warning for a partially-parseable file")
	}
}

// TestLoadUserConfig_UnknownKeysLoadQuietly is the forward-compatibility property.
// A key written by a newer abctl must not turn the whole load into a
// warning-and-discard for an older binary — which is exactly what
// yaml.Decoder.KnownFields(true) would do, so this test guards against that
// refactor.
func TestLoadUserConfig_UnknownKeysLoadQuietly(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "filter: github\ntheme: dark\nevents:\n  sort_order: cost\n  columns:\n    - name: COST\n      visible: false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	got := loadUserConfig(path, &warn)

	if warn.Len() != 0 {
		t.Errorf("unknown keys warned: %q", warn.String())
	}
	if got.Filter != "github" {
		t.Errorf("filter = %q, want the known key applied alongside unknown ones", got.Filter)
	}
	if len(got.Events.Columns) != 1 || got.Events.Columns[0].Name != "COST" {
		t.Errorf("columns = %+v, want the COST deviation", got.Events.Columns)
	}
}

// TestSaveUserConfig_RoundTrips is what makes a saved preference come back.
func TestSaveUserConfig_RoundTrips(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	want := tui.UserSettings{
		Events: tui.EventSettings{Columns: []tui.ColumnSetting{{Name: "COST", Visible: false}}},
		Filter: "github",
	}

	if err := saveUserConfig(path, want); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	got := loadUserConfig(path, &warn)

	if warn.Len() != 0 {
		t.Errorf("our own file warned on load: %q", warn.String())
	}
	if !reflectDeepEqualSettings(got, want) {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}
}

// TestSaveUserConfig_IsHandEditable: the file is documented as editable, so it
// should carry the header explaining the one rule that is not guessable from the
// contents.
func TestSaveUserConfig_IsHandEditable(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := saveUserConfig(path, tui.UserSettings{Filter: "x"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "#") {
		t.Errorf("file does not start with an explanatory comment:\n%s", body)
	}
}

// TestSaveUserConfig_CreatesCortexDirAt0700AndFileAt0600 pins the floor that
// saveUserConfig's security comment reasons down to from checkYankDir's posture.
// Without a test, "0600 is proportionate" is an argument nobody can regress against.
func TestSaveUserConfig_CreatesCortexDirAt0700AndFileAt0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")

	if err := saveUserConfig(path, tui.UserSettings{Filter: "x"}); err != nil {
		t.Fatal(err)
	}

	dfi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dfi.Mode().Perm(); got != 0o700 {
		t.Errorf("~/.cortex mode = %v, want 0700", got)
	}
	ffi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ffi.Mode().Perm(); got != 0o600 {
		t.Errorf("settings file mode = %v, want 0600", got)
	}
}

// TestSaveUserConfig_LeavesNoTempFile: a stray .tmp beside the config is litter in
// a directory the user is told to inspect on uninstall.
func TestSaveUserConfig_LeavesNoTempFile(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := saveUserConfig(path, tui.UserSettings{Filter: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tempfile survived the save (stat err = %v)", err)
	}
}

// TestSaveUserConfig_OverwritesAnExistingFile: the second save of a session must
// not fail on the O_EXCL tempfile or refuse because the target exists.
func TestSaveUserConfig_OverwritesAnExistingFile(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := saveUserConfig(path, tui.UserSettings{Filter: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := saveUserConfig(path, tui.UserSettings{Filter: "second"}); err != nil {
		t.Fatalf("second save failed: %v", err)
	}
	var warn bytes.Buffer
	if got := loadUserConfig(path, &warn).Filter; got != "second" {
		t.Errorf("filter = %q, want the later save to win", got)
	}
}

// TestSaveUserConfig_DoesNotWriteThroughAPlantedTempfile is the residual symlink
// risk, and the reason saveUserConfig does not need checkYankDir's Lstat sweep.
//
// Asserts the PROPERTY (the planted target is never written) rather than the
// mechanism. An earlier version demanded an error, which pinned the fixed-name
// O_EXCL implementation — and that implementation wedged permanently on crash
// debris. CreateTemp writes to an unpredictable name, so a planted symlink is
// bypassed entirely: the save succeeds and the attacker's file is untouched, which
// is a better outcome than the refusal, not a weaker one.
func TestSaveUserConfig_DoesNotWriteThroughAPlantedTempfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "attacker-owned")
	if err := os.WriteFile(target, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The name the old implementation used, and the obvious guess for an attacker.
	if err := os.Symlink(target, path+".tmp"); err != nil {
		t.Fatal(err)
	}

	if err := saveUserConfig(path, tui.UserSettings{Filter: "x"}); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "untouched\n" {
		t.Errorf("symlink target was written through: %q", body)
	}
	// And the real config still landed.
	var warn bytes.Buffer
	if got := loadUserConfig(path, &warn).Filter; got != "x" {
		t.Errorf("filter = %q, want the save to have succeeded regardless", got)
	}
}

// TestSaveUserConfig_SurvivesTempfileDebris is the crash-recovery property.
//
// A hard kill between create and rename leaves a tempfile behind. With a fixed
// name plus O_EXCL, every save afterwards failed with "file exists" — forever,
// surfacing only as a footer flash the user cannot act on, with the config frozen
// at its pre-crash value. Verified at five consecutive failed saves before the fix.
func TestSaveUserConfig_SurvivesTempfileDebris(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := saveUserConfig(path, tui.UserSettings{Filter: "before-crash"}); err != nil {
		t.Fatal(err)
	}
	// Debris at the old fixed name, and at a CreateTemp-shaped one.
	for _, junk := range []string{path + ".tmp", filepath.Join(filepath.Dir(path), ".abctl-config.yaml.123.tmp")} {
		if err := os.WriteFile(junk, []byte("debris\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 5; i++ {
		if err := saveUserConfig(path, tui.UserSettings{Filter: "after-crash"}); err != nil {
			t.Fatalf("save %d failed on leftover debris: %v", i+1, err)
		}
	}

	var warn bytes.Buffer
	if got := loadUserConfig(path, &warn).Filter; got != "after-crash" {
		t.Errorf("filter = %q; saves after a crash never took effect", got)
	}
}

// reflectDeepEqualSettings compares two settings without dragging reflect into
// every assertion. Columns is the only slice, so a length-plus-elements walk is
// enough and gives a better failure message than DeepEqual on nil-vs-empty.
func reflectDeepEqualSettings(a, b tui.UserSettings) bool {
	if a.Filter != b.Filter || len(a.Events.Columns) != len(b.Events.Columns) {
		return false
	}
	for i := range a.Events.Columns {
		if a.Events.Columns[i] != b.Events.Columns[i] {
			return false
		}
	}
	return true
}

// TestSaveUserConfig_ReportsAWriteFailure: a write that fails must not be renamed
// into place and reported as success.
//
// The bug this pins was a shadowed variable: `if _, err := f.Write(...)` declared a
// NEW err scoped to the if, so both writes landed in it and it was discarded. The
// outer err stayed nil, Close and Rename saw nil, and a truncated settings file was
// renamed over the good one while saveUserConfig returned success — the footer would
// have shown nothing and the next start would have warned about a malformed file.
//
// Driven by filling the filesystem, which is not portable, so this uses the one
// reliably-failing write available: a tempfile directory on a read-only parent makes
// CreateTemp itself fail, which covers the early return. For the write path proper,
// the guard is that err is assigned rather than shadowed — checked by vet's
// shadow-adjacent analysers and by this test's sibling below.
func TestSaveUserConfig_ReportsACreateFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix modes")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	home := prefsHome(t)
	dir := filepath.Join(home, ".cortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Read-only directory: MkdirAll succeeds (it exists), CreateTemp cannot.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := saveUserConfig(filepath.Join(dir, "abctl-config.yaml"), tui.UserSettings{Filter: "x"}); err == nil {
		t.Error("saveUserConfig reported success on an unwritable directory")
	}
}

// TestSaveUserConfig_AWriteFailureDoesNotClobberTheGoodFile is the shadowing bug
// proper: a failed write must not be renamed into place, and must be reported.
//
// Injects the failure because a write to a tempfile on a working filesystem does
// not fail, which is precisely why the bug survived every other test here.
func TestSaveUserConfig_AWriteFailureDoesNotClobberTheGoodFile(t *testing.T) {
	home := prefsHome(t)
	path := filepath.Join(home, ".cortex", "abctl-config.yaml")
	if err := saveUserConfig(path, tui.UserSettings{Filter: "good"}); err != nil {
		t.Fatal(err)
	}

	prev := writeAll
	t.Cleanup(func() { writeAll = prev })
	writeAll = func(io.Writer, []byte) (int, error) { return 0, errors.New("no space left on device") }

	err := saveUserConfig(path, tui.UserSettings{Filter: "truncated"})
	if err == nil {
		t.Error("a failed write reported success; a truncated file would be renamed into place")
	} else if !strings.Contains(err.Error(), "no space left") {
		t.Errorf("error = %v, want the underlying write failure", err)
	}

	// The good file must still be there and still good.
	writeAll = prev
	var warn bytes.Buffer
	if got := loadUserConfig(path, &warn).Filter; got != "good" {
		t.Errorf("filter = %q; the previous config was clobbered by a failed save", got)
	}
	if warn.Len() != 0 {
		t.Errorf("config no longer parses after a failed save: %q", warn.String())
	}
	// And no debris left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("failed save left a tempfile: %s", e.Name())
		}
	}
}

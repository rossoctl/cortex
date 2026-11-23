// Package testhome keeps agentop's tests out of the real user profile.
//
// os.UserHomeDir reads HOME on Linux and macOS but USERPROFILE on Windows, so a test that sets
// only HOME reads and writes the developer's real home on Windows: its ~/.cortex, ~/.codex and
// ~/.claude. Set points both at one directory. Isolate, called from TestMain, points every home
// and profile variable at a fresh temporary directory for the whole run, so a test that sets no
// home at all cannot reach the real profile either.
//
// Only test code imports this package.
package testhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Set points the home directory at dir for the rest of the test: HOME and USERPROFILE both, so
// os.UserHomeDir returns dir on every platform. dir may be "" to test a missing home. Like
// t.Setenv, it cannot be used in a parallel test.
func Set(t testing.TB, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// Temp points the home directory at a new t.TempDir() and returns it.
func Temp(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	Set(t, dir)
	return dir
}

// unset are variables that, left as they are, point a test at real directories in the
// profile: the XDG base directories (unset, they derive from HOME), and the agents' own homes.
// XDG_RUNTIME_DIR is not one: it is the runtime directory (/run/user/<uid>) through which
// `systemctl --user` reaches the user's bus, not a place in the profile, and the systemd tests
// need it.
var unset = []string{
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	"CODEX_HOME", "CLAUDE_CONFIG_DIR",
}

// moved are the variables Isolate changes, and Real puts back.
var moved = append([]string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}, unset...)

// original holds moved's values before Isolate, and whether each was set.
var original = map[string]*string{}

// Isolate points HOME and USERPROFILE, and Windows' APPDATA and LOCALAPPDATA, at a fresh
// temporary home for the rest of the process, and unsets the variables that name directories
// inside the real profile. Call it in TestMain before m.Run, and the returned cleanup after:
//
//	cleanup := testhome.Isolate()
//	code := m.Run()
//	cleanup()
//	os.Exit(code)
//
// Child processes a test starts inherit the same environment. Isolate panics if it cannot make
// the directory: running the tests against the real profile instead is worse than not running.
//
// The Go toolchain's directories (GOPATH, GOMODCACHE, GOCACHE, GOENV) default to places under
// the home directory. Isolate pins each one that is not set to its real default first, so a
// `go build` a test runs uses the developer's module cache, build cache and `go env -w`
// settings, not a cold, temporary copy.
func Isolate() (cleanup func()) {
	for _, k := range moved {
		if v, ok := os.LookupEnv(k); ok {
			original[k] = &v
		} else {
			original[k] = nil
		}
	}
	pinGoDirs()
	root, err := os.MkdirTemp("", "agentop-test-home-")
	if err != nil {
		panic("testhome: " + err.Error())
	}
	isolatedRoot = root
	home := filepath.Join(root, "home")
	appData := filepath.Join(home, "AppData", "Roaming")
	localAppData := filepath.Join(home, "AppData", "Local")
	for _, d := range []string{appData, localAppData} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			panic("testhome: " + err.Error())
		}
	}
	for k, v := range map[string]string{"HOME": home, "USERPROFILE": home, "APPDATA": appData, "LOCALAPPDATA": localAppData} {
		if err := os.Setenv(k, v); err != nil {
			panic("testhome: " + err.Error())
		}
	}
	for _, k := range unset {
		os.Unsetenv(k)
	}
	return func() { os.RemoveAll(root) }
}

// pinGoDirs sets GOPATH, GOMODCACHE, GOCACHE and GOENV, each that is not set, to the default the
// go command would use with the real home directory.
func pinGoDirs() {
	set := func(k, v string) {
		if os.Getenv(k) == "" && v != "" {
			os.Setenv(k, v)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		set("GOPATH", filepath.Join(home, "go"))
	}
	if gp := filepath.SplitList(os.Getenv("GOPATH")); len(gp) > 0 {
		set("GOMODCACHE", filepath.Join(gp[0], "pkg", "mod"))
	}
	if dir, err := os.UserCacheDir(); err == nil {
		set("GOCACHE", filepath.Join(dir, "go-build"))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		set("GOENV", filepath.Join(dir, "go", "env"))
	}
}

// Real points the home directory and the variables Isolate moved back at the real ones, for the
// rest of the test: for the tests that must act on the real account (a launchd agent, a systemd
// --user unit) and have their own guards for when they may. Like t.Setenv, it cannot be used in
// a parallel test.
func Real(t testing.TB) {
	t.Helper()
	if isolatedRoot == "" {
		return // not isolated: already real
	}
	for _, k := range moved {
		if v := original[k]; v != nil {
			t.Setenv(k, *v)
		} else {
			t.Setenv(k, "") // so the test's end restores the isolated value
			os.Unsetenv(k)
		}
	}
}

// isolatedRoot is the temporary home Isolate made, or "".
var isolatedRoot string

// AssertIsolated fails the test unless the home directory is inside the temporary one Isolate
// made: a guard, in each package whose TestMain isolates, that the isolation is still there.
// Call it before the test sets a home of its own.
func AssertIsolated(t testing.TB) {
	t.Helper()
	if isolatedRoot == "" {
		t.Fatal("testhome.Isolate was not called: this package's TestMain must isolate the home directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if rel, err := filepath.Rel(isolatedRoot, home); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("the home directory is %s, outside the isolated %s", home, isolatedRoot)
	}
}

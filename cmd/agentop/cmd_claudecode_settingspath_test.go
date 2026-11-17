package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A relative --settings is recorded as the absolute path it named, so uninstall
// finds the project file from any directory.
func TestClaudeCodeEnable_RecordsARelativeSettingsPathAbsolute(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	projDir := filepath.Join(sc.home, "proj")
	proj := filepath.Join(projDir, ".claude", "settings.json")
	writeHomeFile(t, proj, "{}\n")
	t.Chdir(projDir)
	var out, errb bytes.Buffer
	if code := runClaudeCode([]string{"enable", "--yes", "--settings", ".claude/settings.json"}, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	if st, err := readState(filepath.Join(sc.home, stateRel)); err != nil || st == nil || st.Settings != proj {
		t.Fatalf("the record is %+v (%v), want one naming %s", st, err, proj)
	}

	t.Chdir(sc.home)
	code, got := sc.uninstall(t, "--yes")
	wantLines(t, code, 0, got, markLine("✓", "unrouted")+"Claude Code no longer goes through Cortex · ~/proj/.claude/settings.json\n")
	if doc, err := readSettings(proj); err != nil || len(envStrings(doc)) != 0 {
		t.Errorf("the project's settings still route: %v (%v)", doc, err)
	}
}

// setup --claude-code after a project enable, as every upgrade runs it, routes
// the default file too. Uninstall unroutes both, and keeps the CA the user set in
// the project file since.
func TestUninstallUnroutesEverySettingsFileCortexRouted(t *testing.T) {
	const corpCA = "/etc/corp-ca.pem"
	sc := newUninstallScene(t)
	sc.setUp(t)
	proj := filepath.Join(sc.home, "proj", ".claude", "settings.json")
	writeHomeFile(t, proj, "{}\n")
	cfg := filepath.Join(sc.home, ".cortex", "config.yaml")
	if code := claudeCodeEnable2(proj, cfg, filepath.Join(sc.home, stateRel), true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixture: enable --settings exit %d", code)
	}
	handEdit(t, proj, map[string]string{envCACerts: corpCA})
	sc.setUp(t, "--claude-code")

	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 1, out,
		markLine("!", "unrouted")+"removed what Cortex set · ~/proj/.claude/settings.json\n",
		markLine("✓", "unrouted")+"Claude Code no longer goes through Cortex\n")
	if env := readEnv(t, proj); env[envCACerts] != corpCA || len(env) != 1 {
		t.Errorf("the project's env is %v, want only %s=%s:\n%s", env, envCACerts, corpCA, out)
	}
	if env := readEnv(t, filepath.Join(sc.home, settingsRel)); len(env) != 0 {
		t.Errorf("the default settings still route: %v", env)
	}
}

// Each settings file keeps its own entry: a later enable adds one beside the
// others, a re-enable refreshes only its Written, and a drop takes out only its
// own, keeping the next entry whole when it moves up and deleting the record
// with the last.
func TestClaudeCodeRecordKeepsAnEntryPerSettingsFile(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	corp := "http://corp.example:3128"
	entry := func(settings, written string, prior *string) managedState {
		return managedState{Settings: settings, Prior: map[string]*string{envProxy: prior}, Written: map[string]string{envProxy: written}}
	}
	for _, e := range []managedState{entry("/a", "w1", nil), entry("/b", "w1", &corp), entry("/c", "w3", &corp), entry("/b", "w2", nil)} {
		if err := writeClaudeCodeState(state, e); err != nil {
			t.Fatal(err)
		}
	}
	st, err := readState(state)
	if err != nil || len(st.OtherFiles) != 2 {
		t.Fatalf("the record is %+v (%v), want three entries", st, err)
	}
	if b := recordFor(st, "/b"); b.Written[envProxy] != "w2" || b.Prior[envProxy] == nil || *b.Prior[envProxy] != corp {
		t.Errorf("/b's entry is %+v, want Written w2 and its first Prior kept", b)
	}

	for _, c := range []struct {
		drop string
		want []string // the files the record names afterwards, first one first
	}{{"/b", []string{"/a", "/c"}}, {"/a", []string{"/c"}}, {"/elsewhere", []string{"/c"}}, {"/c", nil}} {
		if err := dropRecord(state, c.drop); err != nil {
			t.Fatal(err)
		}
		st, err := readState(state)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		if st != nil {
			got = append(got, st.Settings)
			for _, o := range st.OtherFiles {
				got = append(got, o.Settings)
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("after dropping %s the record names %q, want %q", c.drop, got, c.want)
		}
		if c.drop == "/a" && (st.Written[envProxy] != "w3" || st.Prior[envProxy] == nil || *st.Prior[envProxy] != corp) {
			t.Errorf("/c's entry moved up as %+v, want its Written w3 and Prior kept", st)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("the record outlived its last entry (%v)", err)
	}
}

// An entry an older enable recorded relative to a project names only that
// project's file: an enable on the default file from $HOME adds its own entry.
func TestClaudeCodeEnable_LeavesAnOlderRelativeEntryToItsProject(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	projDir := filepath.Join(sc.home, "proj")
	writeHomeFile(t, filepath.Join(projDir, settingsRel), "{}\n")
	state := filepath.Join(sc.home, stateRel)
	t.Chdir(projDir)
	if code := claudeCodeEnable2(settingsRel, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixture: relative enable exit %d", code)
	}

	t.Chdir(sc.home)
	var errb bytes.Buffer
	if code := runClaudeCode([]string{"enable", "--yes"}, io.Discard, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	st, err := readState(state)
	if err != nil || st.Settings != settingsRel || len(st.OtherFiles) != 1 || st.OtherFiles[0].Settings != filepath.Join(sc.home, settingsRel) {
		t.Errorf("the record is %+v (%v), want the relative entry and one for the default file", st, err)
	}
}

// A relative --settings given again where an older enable recorded it finds
// that entry, so disable takes it out.
func TestClaudeCodeDisable_FindsAnOlderRelativeEntry(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	projDir := filepath.Join(sc.home, "proj")
	writeHomeFile(t, filepath.Join(projDir, settingsRel), "{}\n")
	state := filepath.Join(sc.home, stateRel)
	t.Chdir(projDir)
	if code := claudeCodeEnable2(settingsRel, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixture: relative enable exit %d", code)
	}

	var errb bytes.Buffer
	if code := runClaudeCode([]string{"disable", "--yes", "--settings", settingsRel}, io.Discard, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("the record outlived the disable of the file it names (%v)", err)
	}
}

// An entry recorded under a symlink names the file it resolves to.
func TestClaudeCodeRecordMatchesASymlinkedPath(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	st := &managedState{Settings: "/a", OtherFiles: []managedState{{Settings: filepath.Join(link, "settings.json")}}}
	if got := recordIndex(st, filepath.Join(dir, "settings.json")); got != 1 {
		t.Errorf("recordIndex = %d, want 1, the entry under the link", got)
	}
}

// One entry with no Prior makes the record unreadable, as a first entry with none
// does.
func TestClaudeCodeRecordRefusesAnEntryWithNoPrior(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	writeHomeFile(t, state, `{"settings":"/a","prior":{},"other_files":[{"settings":"/b"}]}`)
	if st, err := readState(state); err == nil {
		t.Errorf("readState = %+v, want an error for the entry with no prior", st)
	}
}

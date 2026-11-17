package main

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// The same relative --settings in another project names another file, so
// disable and enable there leave the older entry to its own project, and say
// how to delete it.
func TestClaudeCodeLeavesAnOlderRelativeEntryToTheProjectThatHoldsIt(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	a, b := filepath.Join(sc.home, "a"), filepath.Join(sc.home, "b")
	writeHomeFile(t, filepath.Join(a, settingsRel), "{}\n")
	writeHomeFile(t, filepath.Join(b, settingsRel), "{}\n")
	state := filepath.Join(sc.home, stateRel)
	t.Chdir(a)
	if code := claudeCodeEnable2(settingsRel, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixture: relative enable exit %d", code)
	}

	t.Chdir(b)
	for _, action := range []string{"disable", "enable"} {
		var out, errb bytes.Buffer
		if code := runClaudeCode([]string{action, "--yes", "--settings", settingsRel}, &out, &errb); code != 0 {
			t.Fatalf("%s in b exit %d: %s", action, code, errb.String())
		}
		if action == "disable" && !strings.Contains(out.String(), "delete it from "+state) {
			t.Errorf("disable in b does not say how to delete the older entry:\n%s", out.String())
		}
		if st, err := readState(state); err != nil || st == nil || st.Settings != settingsRel {
			t.Errorf("after %s in b the record is %+v (%v), want a's relative entry first", action, st, err)
		}
	}
	if st, _ := readState(state); st == nil || len(st.OtherFiles) != 1 || st.OtherFiles[0].Settings != filepath.Join(b, settingsRel) {
		t.Errorf("the record is %+v, want an entry of b's own beside a's", st)
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
// that entry, so disable takes it out and keeps every edit: one key edited
// since, that key alone left, every key edited, and a record from before
// Written.
func TestClaudeCodeDisable_FindsAnOlderRelativeEntry(t *testing.T) {
	const corpCA = "/etc/corp-ca.pem"
	everyKey := map[string]string{}
	for _, k := range managedKeys {
		everyKey[k] = corpCA
	}
	everyKey[envProxy] = "http://corp.example:3128"
	for _, c := range []struct {
		name string
		want map[string]string // the env disable leaves
		edit func(t *testing.T, proj, state string)
	}{
		{"a key edited since", map[string]string{envCACerts: corpCA}, func(t *testing.T, proj, _ string) {
			handEdit(t, proj, map[string]string{envCACerts: corpCA})
		}},
		{"only an edited key left", map[string]string{envCACerts: corpCA}, func(t *testing.T, proj, _ string) {
			writeHomeFile(t, proj, `{"env":{"`+envCACerts+`":"`+corpCA+`"}}`)
		}},
		{"every key edited", everyKey, func(t *testing.T, proj, _ string) {
			handEdit(t, proj, everyKey)
		}},
		{"a record from before Written", nil, func(t *testing.T, _, state string) {
			st, _ := readState(state)
			st.Written = nil
			if err := writeStateFile(state, *st); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			proj := filepath.Join(sc.home, "proj", settingsRel)
			writeHomeFile(t, proj, "{}\n")
			state := filepath.Join(sc.home, stateRel)
			t.Chdir(filepath.Join(sc.home, "proj"))
			if code := claudeCodeEnable2(settingsRel, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
				t.Fatalf("fixture: relative enable exit %d", code)
			}
			c.edit(t, proj, state)

			var errb bytes.Buffer
			if code := runClaudeCode([]string{"disable", "--yes", "--settings", settingsRel}, io.Discard, &errb); code != 0 {
				t.Fatalf("disable exit %d: %s", code, errb.String())
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Errorf("the record outlived the disable of the file it names (%v)", err)
			}
			if env := readEnv(t, proj); !maps.Equal(env, c.want) {
				t.Errorf("env is %v, want %v", env, c.want)
			}
		})
	}
}

// Another project's file can hold the same Cortex values, so a relative
// --settings there keeps the older entry as it is unless the run confirms it:
// a declined change, a run with nothing to change, and a failed enable all
// leave it.
func TestClaudeCodeKeepsAnOlderRelativeEntryUnlessTheRunConfirmsIt(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	a, b := filepath.Join(sc.home, "a"), filepath.Join(sc.home, "b")
	writeHomeFile(t, filepath.Join(a, settingsRel), "{}\n")
	state := filepath.Join(sc.home, stateRel)
	t.Chdir(a)
	if code := claudeCodeEnable2(settingsRel, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixture: relative enable exit %d", code)
	}
	routed, err := os.ReadFile(filepath.Join(a, settingsRel))
	if err != nil {
		t.Fatal(err)
	}
	bFile := filepath.Join(b, settingsRel)
	writeHomeFile(t, bFile, string(routed))

	t.Chdir(b)
	answerPrompt(t, &claudeCodeConfirm, "n\n")
	for _, c := range []struct {
		name string
		args []string
		edit map[string]string
		code int
	}{
		{"a declined disable", []string{"disable"}, nil, exitDeclined},
		{"an enable with nothing to change", []string{"enable"}, nil, 0},
		{"a failed enable", []string{"enable", "--yes"}, map[string]string{envCACerts: "/etc/b-corp.pem"}, 1},
	} {
		if c.edit != nil {
			handEdit(t, bFile, c.edit)
		}
		var out bytes.Buffer
		if code := runClaudeCode(append(c.args, "--settings", settingsRel), &out, io.Discard); code != c.code {
			t.Errorf("%s: exit %d, want %d", c.name, code, c.code)
		}
		if !strings.Contains(out.String(), "older agentop") {
			t.Errorf("%s: no note on the older entry:\n%s", c.name, out.String())
		}
		if st, _ := readState(state); st == nil || st.Settings != settingsRel {
			t.Errorf("after %s the record is %+v, want a's relative entry", c.name, st)
		}
	}
}

// Without --yes, a change the user confirms takes the older relative entry for
// the file, and a disable with nothing to remove keeps the entry and says how to
// take it.
func TestClaudeCodeTakesAnOlderRelativeEntryWithAConfirmedChange(t *testing.T) {
	for _, c := range []struct {
		name   string
		action string
		edit   func(t *testing.T, proj string)
		want   string // the record's first entry afterwards; "" is proj itself
		note   string
	}{
		{"a confirmed enable", "enable", func(t *testing.T, proj string) {
			doc, err := readSettings(proj)
			if err != nil {
				t.Fatal(err)
			}
			delete(envRaw(doc), envProxy)
			if err := writeSettings(proj, doc); err != nil {
				t.Fatal(err)
			}
		}, "", ""},
		{"a disable with nothing to remove", "disable", func(t *testing.T, proj string) {
			edits := map[string]string{}
			for _, k := range managedKeys {
				edits[k] = "/etc/corp-ca.pem"
			}
			handEdit(t, proj, edits)
		}, settingsRel, "run again with --yes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			proj := filepath.Join(sc.home, "proj", settingsRel)
			writeHomeFile(t, proj, "{}\n")
			state := filepath.Join(sc.home, stateRel)
			t.Chdir(filepath.Join(sc.home, "proj"))
			if code := claudeCodeEnable2(settingsRel, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
				t.Fatalf("fixture: relative enable exit %d", code)
			}
			c.edit(t, proj)
			if c.want == "" {
				c.want = proj
			}

			answerPrompt(t, &claudeCodeConfirm, "y\n")
			var out, errb bytes.Buffer
			if code := runClaudeCode([]string{c.action, "--settings", settingsRel}, &out, &errb); code != 0 {
				t.Fatalf("%s exit %d: %s", c.action, code, errb.String())
			}
			if st, _ := readState(state); st == nil || st.Settings != c.want {
				t.Errorf("the record is %+v, want its first entry %s", st, c.want)
			}
			if !strings.Contains(out.String(), c.note) {
				t.Errorf("the output does not say %q:\n%s", c.note, out.String())
			}
		})
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

// readOnlyDir makes dir read-only for the rest of the test, so a record written
// there cannot be replaced.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// A disable that restores the file but cannot take its entry out says so, and
// still succeeds.
func TestClaudeCodeDisable_WarnsWhenItKeepsTheEntry(t *testing.T) {
	first, cfg := fixture(t, "{}")
	second := filepath.Join(t.TempDir(), "settings.json")
	state := filepath.Join(t.TempDir(), "state.json")
	for _, s := range []string{first, second} {
		if code := claudeCodeEnable2(s, cfg, state, true, io.Discard, io.Discard); code != 0 {
			t.Fatalf("fixture: enable on %s exit %d", s, code)
		}
	}
	readOnlyDir(t, filepath.Dir(state))
	var errb bytes.Buffer
	if code := claudeCodeDisable2(first, state, true, io.Discard, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "could not take "+first+" out of "+state) {
		t.Errorf("disable kept the entry and said nothing:\n%s", errb.String())
	}
}

// When uninstall cannot take one file's entry out, its fix keeps the other
// files' entries, which their own fixes read.
func TestUninstallFixForAKeptEntryKeepsTheOthers(t *testing.T) {
	home := t.TempDir()
	env := &setupEnv{home: home, goos: runtime.GOOS}
	state, proj := filepath.Join(home, stateRel), filepath.Join(home, "proj", settingsRel)
	writeHomeFile(t, state, "")
	if err := writeStateFile(state, managedState{Settings: proj, Prior: map[string]*string{},
		OtherFiles: []managedState{{Settings: filepath.Join(home, settingsRel), Prior: map[string]*string{}}}}); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, filepath.Dir(state))

	r := unrouteClaudeCode(env, proj, state)
	_, _, err := r.run(nil)
	if err == nil {
		t.Fatal("the row took the entry out of a read-only record")
	}
	if fixes := r.fixesFor(err); len(fixes) != 1 || strings.HasPrefix(fixes[0].byHand, "rm ") ||
		!strings.Contains(fixes[0].byHand, "delete the entry for ~/proj/"+settingsRel) {
		t.Errorf("the fix is %+v, want one deleting only this file's entry", fixes)
	}
}

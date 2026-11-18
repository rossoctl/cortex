package main

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A relative --settings is recorded as the absolute path it named, so the record
// names the project file from any directory.
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
		t.Errorf("the record is %+v (%v), want one naming %s", st, err, proj)
	}
}

// olderEntryScene is a set-up install whose record holds proj/.claude/settings.json,
// written with content, as an older agentop recorded it: relative, from proj,
// where the test stays.
func olderEntryScene(t *testing.T, content string) (sc setupScene, proj, state string) {
	t.Helper()
	sc = newUninstallScene(t)
	sc.setUp(t)
	proj, state = filepath.Join(sc.home, "proj", settingsRel), filepath.Join(sc.home, stateRel)
	writeHomeFile(t, proj, content)
	t.Chdir(filepath.Dir(filepath.Dir(proj)))
	if code := claudeCodeEnable2(settingsRel, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixture: relative enable exit %d", code)
	}
	return sc, proj, state
}

// recordNames is the files the record names, first one first.
func recordNames(t *testing.T, state string) []string {
	t.Helper()
	st, err := readState(state)
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		return nil
	}
	names := []string{st.Settings}
	for _, o := range st.OtherFiles {
		names = append(names, o.Settings)
	}
	return names
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

// Uninstall cannot find the file an older relative entry names, so it names the
// entry, leaves it and that file as they are, keeps ~/.cortex for the earlier
// values the entry holds, and exits 1. The files it can find it unroutes.
func TestUninstallLeavesAnOlderRelativeEntryAndSaysSo(t *testing.T) {
	const mine = "http://127.0.0.1:47699"
	const row = "kept the record's entry for .claude/settings.json: an older agentop recorded it without its directory"
	leftBehind := "Left behind:\n    .claude/settings.json in the project an older agentop enabled it in\n"
	// The project's file is unread, so the fix reads the entry, which names every key.
	fix := []string{
		`fix: in .claude/settings.json, remove each "env" key whose value is still the one under "written" in that entry, and keep any that differ` + "\n",
		`fix: and restore each key whose "prior" in that entry is not null` + "\n",
		"fix: then delete the record's entry for .claude/settings.json from ~/.cortex/claude-code-state.json\n",
	}
	for _, c := range []struct {
		name      string
		installed bool
		others    string // "", "entries": a project's and the default file's, "bare": the default file routed with no entry
		want      []string
	}{
		{"alone", true, "", []string{markLine("!", "unrouted") + row, leftBehind}},
		{"beside a project and the default file", true, "entries", []string{markLine("!", "unrouted") + row, leftBehind}},
		{"beside a default file with no entry of its own", true, "bare", []string{markLine("!", "unrouted") + row, leftBehind}},
		{"with nothing else installed", false, "", []string{"Nothing to uninstall.\n", "K" + row[1:]}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			cfg, state := filepath.Join(sc.home, cortexCfgRel), filepath.Join(sc.home, stateRel)
			proj := filepath.Join(sc.home, "proj", settingsRel)
			writeHomeFile(t, proj, `{"env":{"`+envProxy+`":"`+mine+`"}}`)
			if c.installed {
				sc.setUp(t)
				t.Chdir(filepath.Join(sc.home, "proj"))
				if code := claudeCodeEnable2(settingsRel, cfg, state, true, io.Discard, io.Discard); code != 0 {
					t.Fatalf("fixture: relative enable exit %d", code)
				}
			} else {
				m := mine
				if err := writeClaudeCodeState(state, managedState{Settings: settingsRel, Prior: map[string]*string{envProxy: &m}}); err != nil {
					t.Fatal(err)
				}
			}
			routed, _ := os.ReadFile(proj)
			def, other := filepath.Join(sc.home, settingsRel), filepath.Join(sc.home, "other", settingsRel)
			switch c.others {
			case "entries":
				writeHomeFile(t, other, "{}\n")
				for _, f := range []string{other, def} {
					if code := claudeCodeEnable2(f, cfg, state, true, io.Discard, io.Discard); code != 0 {
						t.Fatalf("fixture: enable %s exit %d", f, code)
					}
				}
			case "bare":
				want, err := wanted(cfg)
				if err != nil {
					t.Fatal(err)
				}
				writeHomeFile(t, def, `{"env":{"`+envProxy+`":"`+want[envProxy]+`"}}`)
			}
			t.Chdir(sc.home)

			code, out := sc.uninstall(t, "--yes", "--purge")
			wantLines(t, code, 1, out, append(c.want, fix...)...)
			if after, _ := os.ReadFile(proj); !bytes.Equal(after, routed) {
				t.Errorf("the project's file changed:\n%s", after)
			}
			if st, err := readState(state); err != nil || st == nil || st.Settings != settingsRel || len(st.OtherFiles) != 0 {
				t.Errorf("the record is %+v (%v), want the relative entry alone", st, err)
			}
			unrouted := map[string][]string{"entries": {other, def}, "bare": {def}}[c.others]
			for _, f := range unrouted {
				if env := readEnv(t, f); len(env) != 0 {
					t.Errorf("%s still routes: %v", f, env)
				}
			}
		})
	}
}

// The same relative --settings in another project names another file, so
// disable and enable there leave the older entry to its own project, and say
// how to delete it.
func TestClaudeCodeLeavesAnOlderRelativeEntryToTheProjectThatHoldsIt(t *testing.T) {
	sc, _, state := olderEntryScene(t, "{}\n")
	b := filepath.Join(sc.home, "b")
	writeHomeFile(t, filepath.Join(b, settingsRel), "{}\n")
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
			t.Errorf("after %s in b the record is %+v (%v), want proj's relative entry first", action, st, err)
		}
	}
	if names, want := recordNames(t, state), []string{settingsRel, filepath.Join(b, settingsRel)}; !slices.Equal(names, want) {
		t.Errorf("the record names %q, want %q", names, want)
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
		if got := recordNames(t, state); !slices.Equal(got, c.want) {
			t.Errorf("after dropping %s the record names %q, want %q", c.drop, got, c.want)
		}
		if st, _ := readState(state); c.drop == "/a" && (st.Written[envProxy] != "w3" || st.Prior[envProxy] == nil || *st.Prior[envProxy] != corp) {
			t.Errorf("/c's entry moved up as %+v, want its Written w3 and Prior kept", st)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("the record outlived its last entry (%v)", err)
	}
}

// A relative --settings given again where an older enable recorded it finds
// that entry, so disable takes it out and keeps every edit: one key edited
// since, and a record from before Written.
func TestClaudeCodeDisable_FindsAnOlderRelativeEntry(t *testing.T) {
	const corpCA = "/etc/corp-ca.pem"
	for _, c := range []struct {
		name string
		want map[string]string // the env disable leaves
		edit func(t *testing.T, proj, state string)
	}{
		{"a key edited since", map[string]string{envCACerts: corpCA}, func(t *testing.T, proj, _ string) {
			handEdit(t, proj, map[string]string{envCACerts: corpCA})
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
			_, proj, state := olderEntryScene(t, "{}\n")
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

// Without --yes, an enable the user confirms takes the older relative entry for
// the file.
func TestClaudeCodeTakesAnOlderRelativeEntryWithAConfirmedEnable(t *testing.T) {
	_, proj, state := olderEntryScene(t, "{}\n")
	doc, err := readSettings(proj)
	if err != nil {
		t.Fatal(err)
	}
	delete(envRaw(doc), envProxy)
	if err := writeSettings(proj, doc); err != nil {
		t.Fatal(err)
	}

	answerPrompt(t, &claudeCodeConfirm, "y\n")
	var errb bytes.Buffer
	if code := runClaudeCode([]string{"enable", "--settings", settingsRel}, io.Discard, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	if names := recordNames(t, state); !slices.Equal(names, []string{proj}) {
		t.Errorf("the record names %q, want %s alone", names, proj)
	}
}

// A disable with --yes that removes nothing keeps the older relative entry,
// beside any other, and says how to delete it: in the project that
// holds it, with every key edited or only an edited key left, and in another
// project holding only the user's own key.
func TestClaudeCodeKeepsAnOlderRelativeEntryWhenADisableRemovesNothing(t *testing.T) {
	everyKey := map[string]string{envProxy: "http://corp.example:3128"}
	for _, k := range managedKeys[1:] {
		everyKey[k] = "/etc/corp-ca.pem"
	}
	for _, c := range []struct {
		name   string
		dir    string // where disable runs, under home
		env    map[string]string
		others bool // the default file is routed too
	}{
		{"every key edited", "proj", everyKey, false},
		{"every key edited, beside the default file", "proj", everyKey, true},
		{"only an edited key left", "proj", map[string]string{envCACerts: "/etc/corp-ca.pem"}, false},
		{"another project with only its own key", "b", map[string]string{envCACerts: "/etc/b-corp.pem"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc, proj, state := olderEntryScene(t, "{}\n")
			want := []string{settingsRel}
			if def := filepath.Join(sc.home, settingsRel); c.others {
				if code := claudeCodeEnable2(def, filepath.Join(sc.home, cortexCfgRel), state, true, io.Discard, io.Discard); code != 0 {
					t.Fatalf("fixture: default enable exit %d", code)
				}
				want = append(want, def)
			}
			routed := readEnv(t, proj)
			file := filepath.Join(sc.home, c.dir, settingsRel)
			writeHomeFile(t, file, `{"env":{}}`)
			handEdit(t, file, c.env)

			t.Chdir(filepath.Join(sc.home, c.dir))
			var out, errb bytes.Buffer
			if code := runClaudeCode([]string{"disable", "--yes", "--settings", settingsRel}, &out, &errb); code != 0 {
				t.Fatalf("disable exit %d: %s", code, errb.String())
			}
			if env := readEnv(t, file); !maps.Equal(env, c.env) {
				t.Errorf("env is %v, want %v kept", env, c.env)
			}
			if names := recordNames(t, state); !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(want))) {
				t.Errorf("the record names %q, want %q", names, want)
			}
			if !strings.Contains(out.String(), "delete it from "+state) {
				t.Errorf("disable does not say how to delete the older entry:\n%s", out.String())
			}
			if c.dir == "b" && !maps.Equal(readEnv(t, proj), routed) {
				t.Errorf("proj's settings changed: %v", readEnv(t, proj))
			}
		})
	}
}

// An enable that changes nothing leaves the file routed, so it keeps the entry,
// names it as older, and names the runs that record it under the absolute path.
func TestClaudeCodeEnableThatChangesNothingSaysHowToRecordTheOlderEntry(t *testing.T) {
	_, proj, state := olderEntryScene(t, "{}\n")

	var out, errb bytes.Buffer
	if code := runClaudeCode([]string{"enable", "--yes", "--settings", settingsRel}, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	if s := out.String(); !strings.Contains(s, "older agentop") || !strings.Contains(s, "run disable and then enable") || strings.Contains(s, "delete it from") {
		t.Errorf("the note after an enable that changes nothing is:\n%s", s)
	}
	for _, action := range []string{"disable", "enable"} {
		if code := runClaudeCode([]string{action, "--yes", "--settings", settingsRel}, io.Discard, &errb); code != 0 {
			t.Fatalf("%s exit %d: %s", action, code, errb.String())
		}
	}
	if names := recordNames(t, state); !slices.Equal(names, []string{proj}) {
		t.Errorf("the record names %q, want %s alone", names, proj)
	}
}

// Putting the older entry back keeps what another run records while the prompt
// waits: an entry for another file, and the disable of this one.
func TestClaudeCodeKeepsWhatAnotherRunRecordsDuringThePrompt(t *testing.T) {
	for _, c := range []struct {
		name  string
		other func(proj, other, cfg, state string) int
		want  func(proj, other string) []string // the files the record names afterwards
	}{
		{"an enable on another file", func(_, other, cfg, state string) int {
			return claudeCodeEnable2(other, cfg, state, true, io.Discard, io.Discard)
		}, func(_, other string) []string { return []string{settingsRel, other} }},
		{"a disable of this file", func(proj, _, _, state string) int {
			return claudeCodeDisable2(proj, state, true, io.Discard, io.Discard)
		}, func(string, string) []string { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc, proj, state := olderEntryScene(t, "{}\n")
			other, cfg := filepath.Join(sc.home, "other", settingsRel), filepath.Join(sc.home, cortexCfgRel)
			writeHomeFile(t, other, "{}\n")

			saved := claudeCodeConfirm
			t.Cleanup(func() { claudeCodeConfirm = saved })
			claudeCodeConfirm = func(io.Writer) bool {
				if code := c.other(proj, other, cfg, state); code != 0 {
					t.Errorf("%s during the prompt exit %d", c.name, code)
				}
				return false
			}
			if code := runClaudeCode([]string{"disable", "--settings", settingsRel}, io.Discard, io.Discard); code != exitDeclined {
				t.Fatalf("disable exit %d, want %d", code, exitDeclined)
			}
			if names, want := recordNames(t, state), c.want(proj, other); !slices.Equal(names, want) {
				t.Errorf("the record names %q, want %q", names, want)
			}
		})
	}
}

// An enable that records its values and then cannot write the settings file
// puts the older entry back as it was.
func TestClaudeCodeEnable_PutsBackTheOlderEntryItFailedToApply(t *testing.T) {
	_, proj, state := olderEntryScene(t, "{}\n")
	st, _ := readState(state)
	st.Written[envProxy] = "http://127.0.0.1:47699"
	if err := writeStateFile(state, *st); err != nil {
		t.Fatal(err)
	}
	doc, err := readSettings(proj)
	if err != nil {
		t.Fatal(err)
	}
	delete(envRaw(doc), envProxy)
	if err := writeSettings(proj, doc); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, filepath.Dir(proj))

	if code := runClaudeCode([]string{"enable", "--yes", "--settings", settingsRel}, io.Discard, io.Discard); code != 1 {
		t.Fatalf("enable exit %d, want 1", code)
	}
	if got, _ := readState(state); got == nil || got.Settings != settingsRel || !maps.Equal(got.Written, st.Written) {
		t.Errorf("the record is %+v, want the relative entry with Written %v", got, st.Written)
	}
}

// A record that cannot take the older entry as absolute still lets a relative
// disable restore what the entry recorded.
func TestClaudeCodeDisable_RestoresWhenTheOlderEntryCannotBeRecorded(t *testing.T) {
	const mine = "http://127.0.0.1:47699"
	_, proj, state := olderEntryScene(t, `{"env":{"`+envProxy+`":"`+mine+`"}}`)
	readOnlyDir(t, filepath.Dir(state))

	var errb bytes.Buffer
	if code := runClaudeCode([]string{"disable", "--yes", "--settings", settingsRel}, io.Discard, &errb); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	if env := readEnv(t, proj); !maps.Equal(env, map[string]string{envProxy: mine}) {
		t.Errorf("env is %v, want only %s=%s restored", env, envProxy, mine)
	}
	if !strings.Contains(errb.String(), "could not record the older entry") {
		t.Errorf("disable did not say it kept the relative path:\n%s", errb.String())
	}
}

// An enable whose older entry cannot be recorded changes nothing: its own record
// write would fail too, and disable would then read its values as edits.
func TestClaudeCodeEnable_StopsWhenTheOlderEntryCannotBeRecorded(t *testing.T) {
	_, proj, state := olderEntryScene(t, "{}\n")
	env := readEnv(t, proj)
	delete(env, envCACerts) // so enable has a key to put back
	before, _ := json.Marshal(map[string]any{"env": env})
	writeHomeFile(t, proj, string(before))
	readOnlyDir(t, filepath.Dir(state))

	var errb bytes.Buffer
	if code := runClaudeCode([]string{"enable", "--yes", "--settings", settingsRel}, io.Discard, &errb); code != 1 {
		t.Errorf("enable exit %d, want 1: %s", code, errb.String())
	}
	if after, _ := os.ReadFile(proj); !bytes.Equal(before, after) {
		t.Errorf("enable changed the file:\n%s", after)
	}
	if !strings.Contains(errb.String(), "could not record the older entry") {
		t.Errorf("enable did not say why it stopped:\n%s", errb.String())
	}
}

// An entry recorded under a symlink names the file it resolves to, and the same
// text names it first. A relative entry names only its own text, even where its
// directory links to an absolute path.
func TestClaudeCodeRecordMatchesASymlinkedPath(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("symlinks unavailable: ", err)
	}
	st := &managedState{Settings: "/a", OtherFiles: []managedState{{Settings: filepath.Join(link, "settings.json")}}}
	if got := recordIndex(st, filepath.Join(dir, "settings.json")); got != 1 {
		t.Errorf("recordIndex = %d, want 1, the entry under the link", got)
	}
	st = &managedState{Settings: filepath.Join(link, "settings.json"), OtherFiles: []managedState{{Settings: filepath.Join(dir, "settings.json")}}}
	if got := recordIndex(st, filepath.Join(dir, "settings.json")); got != 1 {
		t.Errorf("recordIndex = %d, want 1, the entry with the same text", got)
	}

	proj := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(proj, ".claude")); err != nil {
		t.Skip("symlinks unavailable: ", err)
	}
	t.Chdir(proj)
	if got := recordIndex(&managedState{Settings: settingsRel}, filepath.Join(dir, "settings.json")); got != -1 {
		t.Errorf("recordIndex = %d, want -1: a relative entry matched an absolute path", got)
	}
}

// One entry with no Prior makes the record unreadable, as a first entry with none
// does, with an error naming the entry. Such an entry is never written: the record
// keeps the entries it had.
func TestClaudeCodeRecordRefusesAnEntryWithNoPrior(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	writeHomeFile(t, state, `{"settings":"/a","prior":{},"other_files":[{"settings":"/b"}]}`)
	if st, err := readState(state); err == nil || !strings.Contains(err.Error(), `("/b"); fix or delete that entry by hand`) {
		t.Errorf("readState = %+v (%v), want an error naming the entry with no prior and how to recover", st, err)
	}

	writeHomeFile(t, state, `{"settings":"/a","prior":{}}`)
	for _, e := range []managedState{{Settings: "/b"}, {Prior: map[string]*string{}}} {
		if err := writeClaudeCodeState(state, e); err == nil {
			t.Errorf("writeClaudeCodeState(%+v) recorded an entry readState refuses", e)
		}
	}
	if st, err := readState(state); err != nil || st.Settings != "/a" || len(st.OtherFiles) != 0 {
		t.Errorf("the record is %+v (%v), want /a's entry alone", st, err)
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
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
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

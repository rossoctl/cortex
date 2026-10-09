package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/core/clientstate"
)

// newUninstallScene is a setup scene for uninstall. Its prompt fails the test
// unless the test answers it, as an unanswered one would wait on /dev/tty. It has
// no opencode, so this machine's is never run; uninstallUsesOpenCode gives it one.
func newUninstallScene(t *testing.T) setupScene {
	t.Helper()
	sc := newSetupScene(t, ok200)
	saved, find := uninstallConfirm, uninstallFindOpenCode
	uninstallConfirm = func(io.Writer) bool {
		t.Error("uninstall asked, and the test gave no answer")
		return false
	}
	uninstallFindOpenCode = func() (string, error) { return "", errors.New("no opencode in this scene") }
	t.Cleanup(func() { uninstallConfirm, uninstallFindOpenCode = saved, find })
	return sc
}

// uninstallUsesOpenCode gives the scene f as its opencode, at fakeOpenCodePath.
func uninstallUsesOpenCode(t *testing.T, f *openCodeCLI) {
	t.Helper()
	stubOpenCodeSeams(t, f)
	saved := uninstallFindOpenCode
	uninstallFindOpenCode = func() (string, error) { return fakeOpenCodePath, nil }
	t.Cleanup(func() { uninstallFindOpenCode = saved })
}

// answerUninstall answers uninstall's prompt with answer.
func answerUninstall(t *testing.T, answer string) {
	t.Helper()
	saved := uninstallConfirm
	uninstallConfirm = func(w io.Writer) bool { return uninstallConfirmFrom(strings.NewReader(answer), w) }
	t.Cleanup(func() { uninstallConfirm = saved })
}

// setUp runs setup --yes with args on sc, and fails the test unless it applied.
func (sc setupScene) setUp(t *testing.T, args ...string) string {
	t.Helper()
	code, out := sc.run(t, append([]string{"--from", sc.stage, "--yes"}, args...)...)
	if code != 0 {
		t.Fatalf("setup exit %d:\n%s", code, out)
	}
	return out
}

// uninstall runs agentop uninstall on sc. It deletes what it finds, so HOME must
// first be the scene's temp dir, and launchctl and systemctl its stubs: the real
// ones act on the io.rossoctl.cortex this machine runs.
func (sc setupScene) uninstall(t *testing.T, args ...string) (int, string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home != sc.home || !strings.HasPrefix(home, os.TempDir()) {
		t.Fatalf("HOME is %q (%v), not the scene's temp dir %q", home, err, sc.home)
	}
	for _, name := range []string{"launchctl", "systemctl"} {
		if got, err := exec.LookPath(name); err != nil || !strings.HasPrefix(got, os.TempDir()) {
			t.Fatalf("%s resolves to %q (%v), not the scene's stub", name, got, err)
		}
	}
	var out, errb bytes.Buffer
	code := runUninstall(args, &out, &errb)
	return code, out.String() + errb.String()
}

func writeHomeFile(t *testing.T, path, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // a profile-like fixture
		t.Fatal(err)
	}
}

// outsideCortex is files without ~/.cortex, which uninstall keeps.
func outsideCortex(files map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range files {
		if k != ".cortex" && !strings.HasPrefix(k, ".cortex"+string(filepath.Separator)) {
			out[k] = v
		}
	}
	return out
}

// consentRow is the consent screen's row for verb, or "".
func consentRow(out, verb string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "    "+verb+" ") {
			return l
		}
	}
	return ""
}

// failRemoval makes the removal labelled label fail, leaving "the thing" behind.
func failRemoval(t *testing.T, label string) {
	t.Helper()
	failRemovalWhere(t, func(r removal) bool { return r.label == label })
}

// failRemovalWhere makes each removal match picks fail as failRemoval's does. Its
// error carries no remedy, so its ✗ names the removal's own.
func failRemovalWhere(t *testing.T, match func(removal) bool) {
	t.Helper()
	saved := uninstallRemovalsHook
	uninstallRemovalsHook = func(rs []removal) []removal {
		out := append([]removal(nil), rs...)
		for i := range out {
			if match(out[i]) {
				out[i].run = func(*checklist.Running) (string, []string, error) {
					return "", []string{"the thing"}, errors.New("it would not stop")
				}
			}
		}
		return out
	}
	t.Cleanup(func() { uninstallRemovalsHook = saved })
}

// fixesUnder are the fix: rows under the first line that starts with mark, past
// the rows of detail between them.
func fixesUnder(out, mark string) []string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, mark) {
			continue
		}
		var fixes []string
		for _, r := range lines[i+1:] {
			if !strings.HasPrefix(r, "      ") {
				break
			}
			if f, ok := strings.CutPrefix(r, "      fix: "); ok {
				fixes = append(fixes, f)
			}
		}
		return fixes
	}
	return nil
}

// Uninstall puts back every file setup changed outside ~/.cortex and removes the
// ones it added, but for the .bak copies setup made of the files it changed: those
// stay, holding what the files held before setup. settings.json is compared
// parsed, as writeSettings reformats it.
func TestUninstallUndoesSetup(t *testing.T) {
	sc := newUninstallScene(t)
	const rcBody, settingsBody = "alias ll='ls -l'\n", `{"model":"opus","env":{"FOO":"bar"}}`
	rc, settings := filepath.Join(sc.home, ".zshrc"), filepath.Join(sc.home, settingsRel)
	writeHomeFile(t, rc, rcBody)
	writeHomeFile(t, settings, settingsBody)
	before := outsideCortex(homeFiles(t, sc.home))
	sc.setUp(t, "--claude-code")
	var baks []string
	for k := range outsideCortex(homeFiles(t, sc.home)) {
		if _, ok := before[k]; !ok && strings.HasSuffix(k, ".bak") {
			baks = append(baks, k)
		}
	}
	slices.Sort(baks)
	preSetup := map[string]string{settingsRel + ".bak": settingsBody, ".zshrc.bak": rcBody}
	if want := []string{settingsRel + ".bak", ".zshrc.bak"}; !slices.Equal(baks, want) {
		t.Fatalf("fixture: setup made the .bak files %q, want %q", baks, want)
	}

	code, out := sc.uninstall(t, "--yes")
	if code != 0 || !strings.Contains(out, "\n  Uninstalled.\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	after := outsideCortex(homeFiles(t, sc.home))
	for _, bak := range baks {
		if got := readFile(t, filepath.Join(sc.home, bak)); got != preSetup[bak] {
			t.Errorf("%s holds %q, want what was there before setup, %q", bak, got, preSetup[bak])
		}
		delete(after, bak)
	}
	var got, want any
	if err := json.Unmarshal([]byte(readFile(t, settings)), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(settingsBody), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("settings.json is %v, want %v as before setup", got, want)
	}
	delete(before, settingsRel)
	delete(after, settingsRel)
	sameFiles(t, before, after)
	if got := readFile(t, rc); got != rcBody {
		t.Errorf(".zshrc is %q, want %q as before setup", got, rcBody)
	}
	if _, err := os.Stat(sc.loaded); !os.IsNotExist(err) {
		t.Errorf("the fake job is still loaded (stat: %v)", err)
	}
	for _, name := range []string{"agentop", "cortex"} {
		if _, err := os.Lstat(filepath.Join(sc.home, ".local", "bin", name)); !os.IsNotExist(err) {
			t.Errorf("~/.local/bin/%s is still there (lstat: %v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sc.home, ".cortex", "config.yaml")); err != nil {
		t.Errorf("the config went without --purge: %v", err)
	}
}

// --purge is the only way ~/.cortex goes, and the consent screen says what it
// holds either way: in the Kept line without it, in the delete row with it.
func TestUninstallPurgeIsTheOnlyWayCortexDirGoes(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(fmt.Sprintf("purge=%v", purge), func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			answerUninstall(t, "y\n")
			var args []string
			if purge {
				args = []string{"--purge"}
			}
			code, out := sc.uninstall(t, args...)
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			_, err := os.Lstat(filepath.Join(sc.home, ".cortex"))
			row := consentRow(out, "delete")
			if !purge {
				if err != nil {
					t.Errorf("~/.cortex went without --purge: %v", err)
				}
				wantLines(t, code, 0, out, "  Kept: ~/.cortex (config, CA, logs, usage and session history) — add --purge to delete it\n",
					"  Uninstalled.\n  Kept ~/.cortex. Delete it with: rm -rf ~/.cortex\n")
				if row != "" {
					t.Errorf("a delete row without --purge: %q", row)
				}
				return
			}
			if !os.IsNotExist(err) {
				t.Errorf("--purge left ~/.cortex (lstat: %v)", err)
			}
			if !strings.Contains(row, " ~/.cortex ") || !strings.HasSuffix(row, " config, CA, logs, usage and session history") {
				t.Errorf("the delete row is %q, want ~/.cortex and its usage and session history named:\n%s", row, out)
			}
			if strings.Contains(out, "Kept") {
				t.Errorf("--purge still says ~/.cortex is kept:\n%s", out)
			}
		})
	}
}

// A removal that fails is reported, and the ones after it still run.
func TestUninstallKeepsGoingAfterAFailure(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	failRemoval(t, "stopped")
	code, out := sc.uninstall(t, "--yes")
	failed := strings.Index(out, markLine("✗", "stopped")+"it would not stop\n")
	path := strings.Index(out, markLine("✓", "PATH")+"~/.zshrc\n")
	bins := strings.Index(out, markLine("✓", "removed")+"agentop, cortex from ~/.local/bin\n")
	if code != 1 || failed < 0 || path < failed || bins < path {
		t.Errorf("exit %d; want 1, the ✗ stopped row, then the PATH and binaries rows ✓:\n%s", code, out)
	}
	if !strings.Contains(out, "\n  Left behind:\n    the thing\n") || strings.Contains(out, "Uninstalled.") {
		t.Errorf("the ending does not list the thing as left behind:\n%s", out)
	}
	everyFailHasAFix(t, out)
	if hasPathMarker(filepath.Join(sc.home, ".zshrc")) {
		t.Error("the PATH lines are still in ~/.zshrc")
	}
	for _, name := range []string{"agentop", "cortex"} {
		if _, err := os.Lstat(filepath.Join(sc.home, ".local", "bin", name)); !os.IsNotExist(err) {
			t.Errorf("~/.local/bin/%s is still there (lstat: %v)", name, err)
		}
	}
}

// Only y or yes goes ahead: Enter, n and EOF decline. A decline, and a run with no
// terminal to ask on and no --yes, change nothing.
func TestUninstallAsksAndDefaultsToNo(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{{"\n", false}, {"n\n", false}, {"", false}, {"no\n", false}, {"y\n", true}, {"yes\n", true}, {" YES \n", true}, {"y", true}} {
		var w bytes.Buffer
		if got := uninstallConfirmFrom(strings.NewReader(c.in), &w); got != c.want || w.String() != "  Continue? [y/N] " {
			t.Errorf("answer %q: confirmed=%v after %q, want %v after the [y/N] prompt", c.in, got, w.String(), c.want)
		}
	}
	t.Run("declined", func(t *testing.T) {
		sc := newUninstallScene(t)
		sc.setUp(t, "--claude-code")
		answerUninstall(t, "n\n")
		before := homeFiles(t, sc.home)
		code, out := sc.uninstall(t)
		wantLines(t, code, exitDeclined, out, "  Continue? [y/N]   Not changed.\n")
		sameFiles(t, before, homeFiles(t, sc.home))
		if _, err := os.Stat(sc.loaded); err != nil {
			t.Error("a declined uninstall unloaded the job")
		}
	})
	t.Run("no terminal", func(t *testing.T) {
		sc := newUninstallScene(t)
		sc.setUp(t)
		stubTerminal(t, false, "")
		before := homeFiles(t, sc.home)
		code, out := sc.uninstall(t)
		wantLines(t, code, exitDeclined, out, "  Not changed: no terminal to ask on. Re-run with --yes.\n")
		sameFiles(t, before, homeFiles(t, sc.home))
	})
}

// A PATH block edited since setup wrote it is the user's now: uninstall leaves
// the profile as it is, warns, and lists it as left behind.
func TestUninstallLeavesAnEditedPATHBlock(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	rc := filepath.Join(sc.home, ".zshrc")
	bin := filepath.Join(sc.home, ".local", "bin")
	edited := strings.Replace(readFile(t, rc), `export PATH="`+bin+`:$PATH"`, `export PATH="`+bin+`:$HOME/go/bin:$PATH"`, 1)
	if edited == readFile(t, rc) {
		t.Fatal("fixture: no export line to edit")
	}
	writeHomeFile(t, rc, edited)
	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 1, out, markLine("!", "PATH"),
		"\n  Left behind:\n    the PATH lines in ~/.zshrc — delete the two lines under \""+pathMarker+"\"\n")
	if got := readFile(t, rc); got != edited {
		t.Errorf("uninstall changed the edited ~/.zshrc to %q", got)
	}
}

// A background Cortex, which setup --no-service starts, is stopped and its
// pidfile removed.
func TestUninstallStopsABackgroundCortex(t *testing.T) {
	sc := newUninstallScene(t)
	var pid int
	stopProcessOnCleanup(t, &pid)
	sc.setUp(t, "--no-service")
	pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
	if pid = readPIDFile(pidFile); !alive(pid) {
		t.Fatal("fixture: setup --no-service left no live proxy")
	}
	// ps names the stub's sleep as cortex, as it names the real background proxy.
	installStub(t, "ps", "#!/bin/sh\ncase \"$*\" in\n  *comm=*) echo cortex ;;\n  *) exec /bin/ps \"$@\" ;;\nesac\n")
	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 0, out, markLine("✓", "stopped")+"background Cortex stopped\n")
	if row := consentRow(out, "stop and remove"); !strings.Contains(row, " the background Cortex ") || !strings.HasSuffix(row, " ~/.cortex/proxy.pid") {
		t.Errorf("the consent row is %q, want the background Cortex and its pidfile", row)
	}
	if alive(pid) {
		t.Errorf("the background Cortex (pid %d) is still running", pid)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("proxy.pid is still there (stat: %v)", err)
	}
}

// A job the supervisor will not unload is left loaded. Uninstall says so, with
// the supervisor's own commands to finish it, and exits 1.
func TestUninstallReportsAJobItCouldNotUnload(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	failingUnload(t, sc.loaded)
	sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", filepath.Join(sc.home, ".local", "bin", "cortex"))
	if err != nil {
		t.Fatal(err)
	}
	code, out := sc.uninstall(t, "--yes")
	byHand := unloadByHand(&setupEnv{home: sc.home, goos: runtime.GOOS}, sp.unitFile)
	wantLines(t, code, 1, out, markLine("✗", "stopped"),
		"\n  Left behind:\n    the "+supervisorName(runtime.GOOS)+" job is still loaded — stop it with: "+byHand+"\n")
	if fixes := fixesUnder(out, markLine("✗", "stopped")); !slices.Equal(fixes, []string{byHand}) {
		t.Errorf("the ✗ stopped row's fixes are %q, want the supervisor's own commands %q:\n%s", fixes, byHand, out)
	}
	everyFailHasAFix(t, out)
	if _, err := os.Stat(sc.loaded); err != nil {
		t.Error("fixture: the job is not loaded, so its unload did not fail")
	}
}

// After launchd's bootout returns the job is still being torn down. Uninstall
// waits until it is gone, so the rows after it do not race the teardown.
func TestUninstallWaitsForTheJobToLeaveLaunchd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("waitUnloaded waits on launchd only: systemd's disable --now returns once the unit has stopped")
	}
	sc := newUninstallScene(t)
	sc.setUp(t)
	tearing := slowTeardown(t, sc.loaded, 3)
	code, out := sc.uninstall(t, "--yes")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if left := strings.TrimSpace(readFile(t, tearing)); left != "0" {
		t.Errorf("uninstall returned with the job still being torn down (%s polls to go):\n%s", left, out)
	}
}

// slowTeardown makes fakeSupervisor's bootout return while launchctl print still
// finds the job for the next polls polls, as launchd's teardown does. The file it
// returns holds how many polls are left.
func slowTeardown(t *testing.T, loaded string, polls int) string {
	t.Helper()
	fake, err := exec.LookPath("launchctl")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(fake); !strings.Contains(string(b), loaded) { //nolint:gosec // the scene's stub
		t.Fatalf("%s on PATH is not fakeSupervisor's", fake)
	}
	tearing := filepath.Join(t.TempDir(), "tearing")
	installStub(t, "launchctl", `#!/bin/sh
case "$1" in
  bootout) '`+fake+`' "$@" || exit $?; echo `+strconv.Itoa(polls)+` > '`+tearing+`'; exit 0 ;;
  print)
    n=$(cat '`+tearing+`' 2>/dev/null || echo 0)
    if [ "$n" -gt 0 ]; then echo $((n - 1)) > '`+tearing+`'; echo 'state = running'; exit 0; fi ;;
esac
exec '`+fake+`' "$@"
`)
	return tearing
}

func TestUninstallWithNothingInstalled(t *testing.T) {
	t.Run("a fresh machine", func(t *testing.T) {
		sc := newUninstallScene(t)
		code, out := sc.uninstall(t)
		wantLines(t, code, 0, out, "\n  Nothing to uninstall.\n")
		if strings.Contains(out, "This will") || strings.Contains(out, "still here") {
			t.Errorf("a plan, or a ~/.cortex that is not there:\n%s", out)
		}
	})
	t.Run("only ~/.cortex", func(t *testing.T) {
		sc := newUninstallScene(t)
		mustMkdir(t, filepath.Join(sc.home, ".cortex"))
		code, out := sc.uninstall(t)
		wantLines(t, code, 0, out,
			"\n  Nothing to uninstall.\n  ~/.cortex is still here (config, CA, logs, usage and session history): rm -rf ~/.cortex\n")
	})
}

// Setup's undo line names agentop uninstall, which undoes setup however it ran.
func TestSetupUndoHintNamesUninstall(t *testing.T) {
	for _, flags := range [][]string{nil, {"--claude-code"}, {"--no-service"}, {"--claude-code", "--no-service"}} {
		t.Run(strings.Join(append([]string{"setup"}, flags...), " "), func(t *testing.T) {
			sc := newSetupScene(t, ok200)
			var pid int
			stopProcessOnCleanup(t, &pid)
			out := sc.setUp(t, flags...)
			pid = readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid"))
			if !strings.HasSuffix(out, "\n  Undo any time: agentop uninstall\n") {
				t.Errorf("the ending does not end on the uninstall undo line:\n%s", out)
			}
		})
	}
}

// IBM Bob's proxy setting and the bob shell function are Cortex's routing too:
// uninstall plans both, runs both, and neither routes afterwards. IBM Bob's
// settings are where agentop knows them on macOS only.
func TestUninstallUnroutesBobAndTheBobShell(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	rc := filepath.Join(sc.home, ".zshrc")
	if code := bobShellEnable(rc, true, io.Discard, io.Discard); code != 0 || strings.Count(readFile(t, rc), bobShellBlock) != 1 {
		t.Fatalf("fixture: bobshell enable exit %d", code)
	}
	bob := ""
	if runtime.GOOS == "darwin" {
		var err error
		if bob, err = bobSettingsPath(sc.home); err != nil {
			t.Fatal(err)
		}
		writeHomeFile(t, bob, "{\n  \"editor.fontSize\": 14\n}\n")
		want, _ := bobWanted(filepath.Join(sc.home, ".cortex", "config.yaml"), sc.home)
		if err := bobWriteKey(bob, bobProxyKey, want); err != nil {
			t.Fatal(err)
		}
		doc, err := readSettings(bob)
		if v, _ := doc[bobProxyKey].(string); err != nil || want == "" || bobOwns(v, want) != bobOurs {
			t.Fatalf("fixture: Bob's %s is %q, which bobOwns does not call Cortex's %q (%v)", bobProxyKey, v, want, err)
		}
	}
	code, out := sc.uninstall(t, "--yes")
	want := []string{markLine("✓", "removed") + "the bob shell function from ~/.zshrc\n"}
	if bob != "" {
		want = append(want, markLine("✓", "unrouted")+"IBM Bob no longer goes through Cortex · restart Bob\n")
		if row := consentRow(out, "unroute"); !strings.Contains(row, " IBM Bob ") {
			t.Errorf("the consent row is %q, want IBM Bob's", row)
		}
	}
	wantLines(t, code, 0, out, want...)
	if row := consentRow(out, "remove"); !strings.Contains(row, " the bob shell function ") || !strings.HasSuffix(row, " ~/.zshrc") {
		t.Errorf("the first remove row is %q, want the bob shell function's", row)
	}
	if strings.Contains(readFile(t, rc), bobShellMarkerStart) {
		t.Errorf("~/.zshrc still defines the bob shell function:\n%s", readFile(t, rc))
	}
	if bob != "" {
		if doc, err := readSettings(bob); err != nil || doc[bobProxyKey] != nil {
			t.Errorf("IBM Bob's settings still set %s: %v (%v)", bobProxyKey, doc[bobProxyKey], err)
		}
	}
}

// Enable's state record with nothing routed in settings.json, which the user
// deleted say, is Cortex's own leftover. Uninstall removes it, so doctor does not
// read Claude Code as routed afterwards, and writes no settings.json.
func TestUninstallRemovesARecordWithNothingRouted(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t, "--claude-code")
	state, settings := filepath.Join(sc.home, stateRel), filepath.Join(sc.home, settingsRel)
	if !fileExists(state) {
		t.Fatal("fixture: setup --claude-code wrote no state record")
	}
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 0, out, markLine("✓", "unrouted")+"removed Cortex's record (settings had nothing routed)\n")
	if fileExists(state) {
		t.Error("the state record is still there")
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Errorf("uninstall wrote a settings.json (stat: %v)", err)
	}
}

// The usage errors return before uninstall looks at anything; the scene is there
// in case one does not.
func TestUninstallUsage(t *testing.T) {
	newUninstallScene(t)
	for _, arg := range []string{"--help", "-h"} {
		var out, errb bytes.Buffer
		if code := runUninstall([]string{arg}, &out, &errb); code != 0 || out.String() != uninstallUsage || errb.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 0 and the usage on stdout", arg, code, out.String(), errb.String())
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, "flag provided but not defined: -bogus\n"},
		{[]string{"now"}, "agentop: unexpected argument \"now\"\n"},
	} {
		var out, errb bytes.Buffer
		if code := runUninstall(c.args, &out, &errb); code != 2 || out.Len() != 0 || !strings.HasPrefix(errb.String(), c.want) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want 2 and %q on stderr", c.args, code, out.String(), errb.String(), c.want)
		}
	}
}

// Enable's record names the settings file Cortex routed, which --settings may
// have made a project's. Uninstall unroutes that file, and leaves the default one
// alone unless its own HTTPS_PROXY is Cortex's: disable without the record deletes
// every managed key, a corporate proxy and CA included.
func TestUninstallUnroutesTheSettingsFileTheRecordNames(t *testing.T) {
	for _, c := range []struct {
		name, global string
		cortexGlobal bool // the default file routes through Cortex as well
	}{
		{"a corporate proxy in the default file", `{"env":{"HTTPS_PROXY":"http://corp.example.com:8080","NODE_EXTRA_CA_CERTS":"/etc/corp-ca.pem"}}`, false},
		{"Cortex in the default file too", `{"model":"opus","env":{"HTTPS_PROXY":"http://127.0.0.1:47600"}}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			global := filepath.Join(sc.home, settingsRel)
			writeHomeFile(t, global, c.global)
			proj := filepath.Join(sc.home, "proj", ".claude", "settings.json")
			writeHomeFile(t, proj, "{}\n")
			state := filepath.Join(sc.home, stateRel)
			cfg := filepath.Join(sc.home, ".cortex", "config.yaml")
			if code := claudeCodeEnable2(proj, cfg, state, true, io.Discard, io.Discard); code != 0 {
				t.Fatalf("fixture: enable --settings exit %d", code)
			}
			if st, err := readState(state); err != nil || st == nil || st.Settings != proj {
				t.Fatalf("fixture: the record is %+v (%v), want one naming %s", st, err, proj)
			}
			if cortexProxyIn(&setupEnv{home: sc.home, cortexDir: filepath.Join(sc.home, ".cortex")}, global) != c.cortexGlobal {
				t.Fatalf("fixture: Cortex's proxy in the default file = %v, want %v", !c.cortexGlobal, c.cortexGlobal)
			}

			code, out := sc.uninstall(t, "--yes")
			wantLines(t, code, 0, out, markLine("✓", "unrouted")+"Claude Code no longer goes through Cortex · ~/proj/.claude/settings.json\n")
			if doc, err := readSettings(proj); err != nil || len(envStrings(doc)) != 0 {
				t.Errorf("the project's settings still route: %v (%v)", doc, err)
			}
			if fileExists(state) {
				t.Error("the record is still there")
			}
			defaultRow := false
			for _, l := range strings.Split(out, "\n") {
				defaultRow = defaultRow || strings.HasPrefix(l, "    unroute ") && strings.HasSuffix(l, " ~/.claude/settings.json")
			}
			if !c.cortexGlobal {
				if got := readFile(t, global); got != c.global {
					t.Errorf("the default settings changed to %q, want %q as they were", got, c.global)
				}
				if defaultRow || fileExists(global+".bak") {
					t.Errorf("the default settings were planned or written (a .bak: %v):\n%s", fileExists(global+".bak"), out)
				}
				return
			}
			if doc, err := readSettings(global); err != nil || doc["model"] != "opus" || len(envStrings(doc)) != 0 {
				t.Errorf("the default settings are %v (%v), want only the model left", doc, err)
			}
			if !defaultRow {
				t.Errorf("no unroute row for the default settings:\n%s", out)
			}
		})
	}
}

// A bob shell block that is not the one enable wrote, edited or added twice, is
// the user's now: uninstall leaves the rc file alone, warns, and lists it.
func TestUninstallLeavesAnEditedBobShellBlock(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t, "--no-modify-path")
	rc := filepath.Join(sc.home, ".zshrc")
	if code := bobShellEnable(rc, true, io.Discard, io.Discard); code != 0 {
		t.Fatalf("fixture: bobshell enable exit %d", code)
	}
	edited := strings.Replace(readFile(t, rc), `agentop exec -- bob "$@"`, `agentop exec -- bob --verbose "$@"`, 1)
	if edited == readFile(t, rc) {
		t.Fatal("fixture: nothing to edit in the block")
	}
	writeHomeFile(t, rc, edited)
	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 1, out, markLine("!", "bob shell"),
		"\n  Left behind:\n    the bob shell function in ~/.zshrc — delete the lines from \""+bobShellMarkerStart+"\" to \""+bobShellMarkerEnd+"\"\n")
	if got := readFile(t, rc); got != edited {
		t.Errorf("uninstall changed the edited ~/.zshrc to %q", got)
	}
}

// A managed key the user has changed since enable set it is theirs, exactly as an
// edited bob shell block is: uninstall unroutes the rest, leaves that one, and
// lists it. It takes the same care disable does — the row is ! rather than ✓, so
// a run that leaves a HTTPS_PROXY behind does not report itself as having
// unrouted Claude Code outright.
func TestUninstallLeavesAClaudeCodeKeyChangedSinceEnable(t *testing.T) {
	sc := newUninstallScene(t)
	settings := filepath.Join(sc.home, settingsRel)
	// A prior value, so the row has a restore to report alongside the key it leaves.
	writeHomeFile(t, settings, `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`)
	sc.setUp(t, "--claude-code")
	const edited = "http://127.0.0.1:9999"
	handEdit(t, settings, map[string]string{envProxy: edited})

	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 1, out,
		markLine("!", "unrouted")+"Claude Code no longer goes through Cortex · restored your "+envNoTelem+"\n",
		"\n  Left behind:\n    "+envProxy+" in ~/.claude/settings.json, changed since Cortex set "+
			"them — remove them by hand if you want them gone\n")
	env := readEnv(t, settings)
	if env[envProxy] != edited {
		t.Errorf("%s = %q, want the edit %q left alone", envProxy, env[envProxy], edited)
	}
	if env[envNoTelem] != "1" {
		t.Errorf("%s = %q, want the prior value restored", envNoTelem, env[envNoTelem])
	}
	for _, k := range managedKeys {
		if k == envProxy || k == envNoTelem {
			continue
		}
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q survived the unroute", k, v)
		}
	}
	// No value in what the ending prints: it gets pasted into issues and logs.
	if strings.Contains(out, edited) {
		t.Errorf("the run named the left key's value:\n%s", out)
	}
	// The record stays: it is the only note of what the left key held before
	// enable, and the key is still set.
	if !fileExists(filepath.Join(sc.home, stateRel)) {
		t.Error("the record was deleted with a managed key still in the settings")
	}
}

// Every managed key changed: there is nothing of Cortex's to take out, so the
// unroute must change nothing rather than report an unroute it did not do. This
// is the branch where the by-hand fix has only the left keys to name, and it must
// not come out empty — a ✗ with no fix line is what that would look like.
func TestUninstallLeavesEveryClaudeCodeKeyChangedSinceEnable(t *testing.T) {
	sc := newUninstallScene(t)
	settings := filepath.Join(sc.home, settingsRel)
	sc.setUp(t, "--claude-code")
	edits := map[string]string{}
	for i, k := range managedKeys {
		edits[k] = "mine-" + string(rune('a'+i))
	}
	handEdit(t, settings, edits)
	before := readFile(t, settings)

	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 1, out,
		markLine("!", "unrouted")+"left "+someKeys(managedKeys)+" as they are\n",
		"\n  Left behind:\n    "+someKeys(managedKeys)+" in ~/.claude/settings.json, changed since "+
			"Cortex set them — remove them by hand if you want them gone\n")
	if got := readFile(t, settings); got != before {
		t.Errorf("the settings changed to %q, want %q as they were", got, before)
	}
	if !fileExists(filepath.Join(sc.home, stateRel)) {
		t.Error("the record was deleted with every managed key still in the settings")
	}
	// remedy.byHand is never empty, and this is the one branch where the keys to
	// remove and the keys to restore are both empty.
	byHand := unrouteByHand(&setupEnv{home: sc.home}, settings, filepath.Join(sc.home, stateRel),
		nil, managedKeys)
	if !strings.Contains(byHand, "decide about "+someKeys(managedKeys)) {
		t.Errorf("the by-hand fix is %q, want it to name the keys left", byHand)
	}
}

// --purge deletes ~/.cortex last. Claude Code's unroute reads the record there,
// so a value the user had set before setup comes back rather than going.
func TestUninstallPurgeStillRestoresClaudeCode(t *testing.T) {
	sc := newUninstallScene(t)
	settings := filepath.Join(sc.home, settingsRel)
	const before = `{"model":"opus","env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1","FOO":"bar"}}`
	writeHomeFile(t, settings, before)
	sc.setUp(t, "--claude-code")
	code, out := sc.uninstall(t, "--yes", "--purge")
	wantLines(t, code, 0, out,
		markLine("✓", "unrouted")+"Claude Code no longer goes through Cortex · restored your CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\n",
		markLine("✓", "purged")+"~/.cortex\n")
	if strings.Contains(out, "cannot read the record") {
		t.Errorf("the unroute could not read the record:\n%s", out)
	}
	var got, want any
	if err := json.Unmarshal([]byte(readFile(t, settings)), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(before), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("settings.json is %v, want %v as before setup", got, want)
	}
	if _, err := os.Lstat(filepath.Join(sc.home, ".cortex")); !os.IsNotExist(err) {
		t.Errorf("--purge left ~/.cortex (lstat: %v)", err)
	}
}

// --purge deletes ~/.cortex only when no removal before it failed.
// Something left without a failure, PATH lines the user edited, does not stop it.
func TestUninstallPurgeKeepsCortexDirAfterAFailure(t *testing.T) {
	const keptRow, leftLine = "kept ~/.cortex, as a removal above failed\n",
		"    ~/.cortex — once the fixes above are done: rm -rf ~/.cortex\n"
	for _, c := range []struct {
		name, label string
		flags       []string
	}{
		{"Claude Code's unroute", "unrouted", []string{"--claude-code"}},
		{"the service", "stopped", nil},
		{"the PATH lines", "PATH", nil},
		{"the binaries", "removed", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t, c.flags...)
			failRemoval(t, c.label)
			code, out := sc.uninstall(t, "--yes", "--purge")
			wantLines(t, code, 1, out, markLine("✗", c.label), markLine("!", "purged")+keptRow, "\n  Left behind:\n", leftLine)
			if _, err := os.Stat(filepath.Join(sc.home, ".cortex", "config.yaml")); err != nil {
				t.Errorf("--purge deleted ~/.cortex after a failure: %v", err)
			}
		})
	}

	// Claude Code's unroute fails for real: its fix restores from the record, and
	// the settings still route to the CA, so both stay.
	t.Run("a settings file it cannot write", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: a 0o500 directory is still writable")
		}
		sc := newUninstallScene(t)
		settings := filepath.Join(sc.home, settingsRel)
		writeHomeFile(t, settings, `{"env":{"`+envNoTelem+`":"1"}}`)
		sc.setUp(t, "--claude-code")
		writeCA(t, filepath.Join(sc.home, ".cortex", "ca"), time.Now().Add(90*24*time.Hour)) // the fake supervisor starts no Cortex to mint it
		if err := os.Chmod(filepath.Dir(settings), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(settings), 0o755) })
		code, out := sc.uninstall(t, "--yes", "--purge")
		wantLines(t, code, 1, out, markLine("✗", "unrouted"), markLine("!", "purged")+keptRow, leftLine)
		for _, f := range []string{filepath.Join(sc.home, stateRel), filepath.Join(sc.home, ".cortex", "ca", "ca.crt")} {
			if !fileExists(f) {
				t.Errorf("%s went, and the fix lines need it", f)
			}
		}
	})

	t.Run("edited PATH lines", func(t *testing.T) {
		sc := newUninstallScene(t)
		sc.setUp(t)
		rc, bin := filepath.Join(sc.home, ".zshrc"), filepath.Join(sc.home, ".local", "bin")
		writeHomeFile(t, rc, strings.Replace(readFile(t, rc), `export PATH="`+bin+`:$PATH"`, `export PATH="`+bin+`:$HOME/go/bin:$PATH"`, 1))
		code, out := sc.uninstall(t, "--yes", "--purge")
		wantLines(t, code, 1, out, markLine("!", "PATH"), markLine("✓", "purged")+"~/.cortex\n")
		if _, err := os.Lstat(filepath.Join(sc.home, ".cortex")); !os.IsNotExist(err) {
			t.Errorf("--purge left ~/.cortex (lstat: %v)", err)
		}
	})
}

// openCodeEnabled routes the scene's opencode through Cortex as configure opencode
// enable does, with NO_PROXY, a variable of the user's own, beside Cortex's. It
// returns the fake, its service running or not, and its calls so far forgotten.
func openCodeEnabled(t *testing.T, sc setupScene, running bool) *openCodeCLI {
	t.Helper()
	f := &openCodeCLI{env: map[string]string{"NO_PROXY": "corp.example"}}
	uninstallUsesOpenCode(t, f)
	if code, _, errOut := runOC("enable", "--yes"); code != 0 {
		t.Fatalf("fixture: configure opencode enable exit %d: %s", code, errOut)
	}
	if len(f.env) != len(openCodeKeyOrder)+1 || !fileExists(filepath.Join(sc.home, opencodeStateRel)) {
		t.Fatalf("fixture: enable left the service environment %v, and a record: %v", f.env, fileExists(filepath.Join(sc.home, opencodeStateRel)))
	}
	f.running, f.calls = running, nil
	return f
}

// configure opencode enable routes OpenCode through its service environment, and
// names disable as the off switch. Uninstall runs that disable before the proxy
// stops and agentop goes: Cortex's values come out, NO_PROXY stays, and a service
// that runs is restarted so it drops them now. Its CLI is judged by Cortex's
// values, as configure opencode has it judged.
func TestUninstallUnroutesOpenCode(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			f := openCodeEnabled(t, sc, running)
			cortexs := maps.Clone(f.env)
			delete(cortexs, "NO_PROXY")
			code, out := sc.uninstall(t, "--yes")
			where, done, restarts := "OpenCode's service environment", "OpenCode's service environment no longer routes it through Cortex", 0
			if running {
				where, done, restarts = where+" · restarts the service, interrupting its sessions", done+" · restarted its service", 1
			}
			wantLines(t, code, 0, out, markLine("✓", "unrouted")+done+"\n", "\n  Uninstalled.\n")
			if row := consentRow(out, "unroute"); !strings.Contains(row, " OpenCode ") || !strings.HasSuffix(row, where) {
				t.Errorf("the consent row is %q, want OpenCode and %q:\n%s", row, where, out)
			}
			if !maps.Equal(f.env, map[string]string{"NO_PROXY": "corp.example"}) {
				t.Errorf("the service environment is %v, want only NO_PROXY left", f.env)
			}
			if got := f.restarts(); got != restarts {
				t.Errorf("%d service restarts, want %d", got, restarts)
			}
			if !maps.Equal(f.cliWant, cortexs) {
				t.Errorf("the CLI ran judged by %v, want Cortex's values %v", f.cliWant, cortexs)
			}
			if fileExists(filepath.Join(sc.home, opencodeStateRel)) {
				t.Error("enable's record is still there")
			}
			if i, j := strings.Index(out, markLine("✓", "unrouted")+"OpenCode"), strings.Index(out, markLine("✓", "stopped")); i < 0 || j < i {
				t.Errorf("OpenCode is not unrouted before the service stops:\n%s", out)
			}
		})
	}
}

// An OpenCode whose service environment holds nothing of Cortex's gets no row and
// no change: a proxy of the user's own stays. Nor does one there is no opencode to
// change it with, or one whose environment cannot be read with no record of
// enable's to say Cortex routed it.
func TestUninstallLeavesAnOpenCodeCortexDidNotRoute(t *testing.T) {
	notJSON := "not json"
	for _, c := range []struct {
		name string
		f    *openCodeCLI // nil: no opencode
	}{
		{"a proxy of the user's own", &openCodeCLI{env: map[string]string{"HTTPS_PROXY": "http://proxy.corp.example:8080"}, running: true}},
		{"no opencode", nil},
		{"an environment it cannot read", &openCodeCLI{getEnv: &notJSON, running: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			before := map[string]string{}
			if c.f != nil {
				uninstallUsesOpenCode(t, c.f)
				before = maps.Clone(c.f.env)
			}
			code, out := sc.uninstall(t, "--yes")
			if code != 0 || strings.Contains(out, "OpenCode") {
				t.Errorf("exit %d, and OpenCode named; want 0 and no OpenCode row:\n%s", code, out)
			}
			if c.f != nil && (!maps.Equal(c.f.env, before) || len(c.f.writes()) > 0 || c.f.restarts() > 0) {
				t.Errorf("the service environment went from %v to %v, by %q", before, c.f.env, c.f.calls)
			}
		})
	}
}

// A failed OpenCode row's fix is the change by hand, as agentop is gone by the
// time it is read: a row per key to unset, then the restart. Not disable's own
// "run agentop configure opencode disable again". An environment that cannot be
// read fails the row only where enable's record says Cortex routed it, and its
// fix shows what to look for. Either way ~/.cortex stays under --purge.
func TestUninstallNamesTheFixForOpenCode(t *testing.T) {
	restart := "then, if OpenCode's service runs: " + fakeOpenCodePath + " service restart"
	var unsets []string
	for _, k := range openCodeKeyOrder {
		unsets = append(unsets, fakeOpenCodePath+" service unset env "+k)
	}
	notJSON := "not json"
	for _, c := range []struct {
		name    string
		breakIt func(f *openCodeCLI)
		want    []string
	}{
		{"an unset that fails part way", func(f *openCodeCLI) { f.failUnset = "HTTP_PROXY" }, append(slices.Clone(unsets), restart)},
		{"an environment it cannot read", func(f *openCodeCLI) { f.getEnv = &notJSON }, []string{
			"see which of " + strings.Join(openCodeKeyOrder, ", ") + " hold Cortex's values: " + fakeOpenCodePath + " service get env",
			"and unset each of those: " + fakeOpenCodePath + " service unset env <name>", restart}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			f := openCodeEnabled(t, sc, true)
			c.breakIt(f)
			code, out := sc.uninstall(t, "--yes", "--purge")
			if fixes := fixesUnder(out, markLine("✗", "unrouted")); code != 1 || !slices.Equal(fixes, c.want) {
				t.Errorf("exit %d and the ✗ unrouted fixes %q, want 1 and %q:\n%s", code, fixes, c.want, out)
			}
			if strings.Contains(out, "disable again") {
				t.Errorf("a line names agentop, which uninstall removes:\n%s", out)
			}
			everyFailHasAFix(t, out)
			if !fileExists(filepath.Join(sc.home, opencodeStateRel)) {
				t.Error("enable's record went: --purge ran after the failure")
			}
		})
	}
}

// A value written into enable's record by hand goes back, as disable puts it back;
// the fix for a failed row says so by key, never printing the value, which can
// carry a password. A restart that fails leaves the row ! with the restart to run.
func TestUninstallOpenCodeRecordAndRestart(t *testing.T) {
	const corp = "http://alice:s3cret@proxy.corp.example:8080"
	record := func(t *testing.T, sc setupScene) {
		t.Helper()
		st, err := readState(filepath.Join(sc.home, opencodeStateRel))
		if err != nil || st == nil {
			t.Fatalf("fixture: no record (%v)", err)
		}
		v := corp
		st.Prior["HTTPS_PROXY"] = &v
		b, err := json.Marshal(st) // not writeState, which keeps the record it finds
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sc.home, opencodeStateRel), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("restored", func(t *testing.T) {
		sc := newUninstallScene(t)
		sc.setUp(t)
		f := openCodeEnabled(t, sc, false)
		record(t, sc)
		code, out := sc.uninstall(t, "--yes")
		wantLines(t, code, 0, out, markLine("✓", "unrouted")+"OpenCode's service environment no longer routes it through Cortex · restored your HTTPS_PROXY\n")
		if !maps.Equal(f.env, map[string]string{"NO_PROXY": "corp.example", "HTTPS_PROXY": corp}) {
			t.Errorf("the service environment is %v, want NO_PROXY and the recorded HTTPS_PROXY", f.env)
		}
	})
	t.Run("a failure's fix", func(t *testing.T) {
		sc := newUninstallScene(t)
		sc.setUp(t)
		f := openCodeEnabled(t, sc, false)
		record(t, sc)
		f.failSet = "HTTPS_PROXY"
		code, out := sc.uninstall(t, "--yes")
		fixes := fixesUnder(out, markLine("✗", "unrouted"))
		want := "set HTTPS_PROXY back to your earlier value, in ~/.cortex/opencode-state.json: " + fakeOpenCodePath + " service set env HTTPS_PROXY <value>"
		if code != 1 || len(fixes) == 0 || fixes[0] != want {
			t.Errorf("exit %d and the fixes %q, want 1 and first %q:\n%s", code, fixes, want, out)
		}
		if strings.Contains(out, "s3cret") {
			t.Errorf("a recorded value was printed:\n%s", out)
		}
	})
	t.Run("a restart that fails", func(t *testing.T) {
		sc := newUninstallScene(t)
		sc.setUp(t)
		f := openCodeEnabled(t, sc, true)
		f.restartErr = errors.New("opencode service restart: exit status 1")
		code, out := sc.uninstall(t, "--yes")
		wantLines(t, code, 1, out, markLine("!", "unrouted")+"OpenCode's service environment no longer routes it through Cortex\n",
			"\n  Left behind:\n    OpenCode's service, which may still run with Cortex's proxy — restart it: "+fakeOpenCodePath+" service restart\n")
	})
}

// The pre-rename binaries are removed with the rest when they are Cortex's own,
// as setup's cleanup keeps one while a service or background proxy still runs it.
// A stranger's file by the same name stays.
func TestUninstallRemovesOurPreRenameBinaries(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	bin := filepath.Join(sc.home, ".local", "bin")
	writeExe(t, filepath.Join(bin, "authbridge-proxy"), "#!/bin/sh\n# github.com/rossoctl/cortex/cmd/authbridge-proxy\n")
	writeExe(t, filepath.Join(bin, "abctl"), "#!/bin/sh\necho somebody else's abctl\n")
	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 0, out, markLine("✓", "removed")+"agentop, cortex, authbridge-proxy from ~/.local/bin\n")
	if _, err := os.Lstat(filepath.Join(bin, "authbridge-proxy")); !os.IsNotExist(err) {
		t.Errorf("our authbridge-proxy is still there (lstat: %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(bin, "abctl")); err != nil {
		t.Errorf("a stranger's abctl went: %v", err)
	}
}

// lockedWriter lets a test read what a spinner goroutine may still be writing.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// On a terminal a removal runs under a spinner, which its row then replaces.
func TestUninstallRowsRunUnderASpinnerOnATerminal(t *testing.T) {
	plainOutput(t)
	var w lockedWriter
	ui := checklist.New(&w, true)
	applyRemovals(&setupEnv{home: "/h"}, ui, []removal{{label: "stopped", run: func(*checklist.Running) (string, []string, error) {
		time.Sleep(250 * time.Millisecond)
		return "background Cortex stopped", nil, nil
	}}}, nil)
	ui.Close()
	out := w.String()
	// The row's time ends at column 78: 42 columns of row, then the 4 of "0.Ns".
	row := "\r\x1b[K  ✓ stopped      background Cortex stopped" + strings.Repeat(" ", 78-42-4) + "0."
	for _, want := range []string{"\x1b[?25l", " stopped      \r", row, "\x1b[?25h"} {
		if !strings.Contains(out, want) {
			t.Errorf("animated output lacks %q: %q", want, out)
		}
	}
}

// fakeUninstallSignals stands in for the signals uninstall catches, and records
// that it subscribed to them.
func fakeUninstallSignals(t *testing.T) (chan<- os.Signal, *atomic.Bool) {
	t.Helper()
	c := make(chan os.Signal, 1)
	var subscribed atomic.Bool
	saved := uninstallSignals
	uninstallSignals = func() (<-chan os.Signal, func()) {
		subscribed.Store(true)
		return c, func() {}
	}
	t.Cleanup(func() { uninstallSignals = saved })
	return c, &subscribed
}

// A Ctrl-C or SIGTERM during the removals lets the running one finish and skips
// the rest, which the ending lists as not removed. Uninstall returns rather than
// dying of the signal, so on a terminal the cursor the spinner hid comes back.
func TestUninstallASignalSkipsTheRemovalsAfterTheRunningOne(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t, "--claude-code")
	sigs, subscribed := fakeUninstallSignals(t)
	saved, savedAnimate := uninstallRemovalsHook, uninstallAnimate
	uninstallAnimate = func(io.Writer) bool { return true }
	uninstallRemovalsHook = func(rs []removal) []removal {
		out := append([]removal(nil), rs...)
		if len(out) != 4 || out[1].label != "stopped" {
			t.Errorf("fixture: removals %v, want unrouted, stopped, PATH, removed", out)
			return out
		}
		run := out[1].run
		out[1].run = func(act *checklist.Running) (string, []string, error) {
			if !subscribed.Load() {
				t.Error("uninstall removes before it catches signals: one now would kill it with the cursor hidden")
			}
			sigs <- syscall.SIGTERM
			return run(act)
		}
		return out
	}
	t.Cleanup(func() { uninstallRemovalsHook, uninstallAnimate = saved, savedAnimate })

	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, 1, out, "  ✓ stopped      "+supervisorName(runtime.GOOS)+" job removed",
		"\n  Interrupted.\n  Left behind:\n    not removed (interrupted): PATH the PATH lines\n"+
			"    not removed (interrupted): removed agentop, cortex\n")
	if !strings.Contains(out, "\x1b[?25l") || !strings.HasSuffix(out, "\x1b[?25h") {
		t.Errorf("the output does not end by showing the cursor its spinner hid: %q", out)
	}
	if !hasPathMarker(filepath.Join(sc.home, ".zshrc")) {
		t.Error("the PATH lines went after the signal")
	}
	for _, name := range []string{"agentop", "cortex"} {
		if _, err := os.Lstat(filepath.Join(sc.home, ".local", "bin", name)); err != nil {
			t.Errorf("~/.local/bin/%s went after the signal: %v", name, err)
		}
	}
	if _, err := os.Stat(sc.loaded); !os.IsNotExist(err) {
		t.Errorf("the removal the signal came during did not finish: the job is still loaded (stat: %v)", err)
	}
}

// A signal at the prompt declines, as setup's does: at once, rather than when the
// user answers, with exit 3 and nothing removed.
func TestUninstallASignalAtThePromptChangesNothing(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t, "--claude-code")
	sigs, subscribed := fakeUninstallSignals(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	saved := uninstallConfirm
	uninstallConfirm = func(w io.Writer) bool {
		_, _ = fmt.Fprint(w, "  Continue? [y/N] ")
		if !subscribed.Load() {
			t.Error("uninstall asks before it catches signals")
		}
		sigs <- os.Interrupt
		// A yes after the Ctrl-C, too late: uninstall has stopped waiting by then.
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		return true
	}
	t.Cleanup(func() { uninstallConfirm = saved })
	before := homeFiles(t, sc.home)
	start := time.Now()
	code, out := sc.uninstall(t)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("uninstall took %s, waiting for the answer after the signal", took)
	}
	wantLines(t, code, exitDeclined, out, "  Continue? [y/N] \n  Not changed.\n")
	if strings.Contains(out, "✓") {
		t.Errorf("a removal ran after the signal:\n%s", out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
	if _, err := os.Stat(sc.loaded); err != nil {
		t.Error("the job was unloaded after a signal at the prompt")
	}
}

// uninstallSigintChild marks the child TestUninstallARealSigintStillShowsTheCursor runs.
const uninstallSigintChild = "AGENTOP_TEST_UNINSTALL_SIGINT_CHILD"

// The same with a real SIGINT, in a child, as the fake channel cannot show that
// uninstall catches one at all: without a handler the signal kills the process
// before its deferred Close shows the cursor again.
func TestUninstallARealSigintStillShowsTheCursor(t *testing.T) {
	if os.Getenv(uninstallSigintChild) == "1" {
		sc := newUninstallScene(t)
		saved, savedAnimate := uninstallRemovalsHook, uninstallAnimate
		t.Cleanup(func() { uninstallRemovalsHook, uninstallAnimate = saved, savedAnimate })
		uninstallAnimate = func(io.Writer) bool { return true }
		uninstallRemovalsHook = func([]removal) []removal {
			step := func(label string, fn func()) removal {
				return removal{label: label, item: checklist.Item{Verb: "remove", What: "the " + label},
					run: func(*checklist.Running) (string, []string, error) { fn(); return "done", nil, nil }}
			}
			return []removal{step("first", func() {}), step("second", func() {
				_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
				time.Sleep(300 * time.Millisecond) // for it to arrive while this removal runs
			}), step("third", func() {})}
		}
		code, out := sc.uninstall(t, "--yes")
		fmt.Printf("%s\nEXIT:%d\n", out, code)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUninstallARealSigintStillShowsTheCursor$", "-test.count=1") //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(), uninstallSigintChild+"=1")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if err != nil {
		t.Fatalf("the child died of the signal (%v), so its deferred Close never showed the cursor:\n%q", err, out)
	}
	if hid, shown := strings.LastIndex(out, "\x1b[?25l"), strings.LastIndex(out, "\x1b[?25h"); hid < 0 || shown < hid {
		t.Errorf("the cursor the spinner hid is not shown again: %q", out)
	}
	for _, want := range []string{"✓ second", "not removed (interrupted): third the third\n", "\nEXIT:1\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the child's output lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "✓ third") {
		t.Errorf("the removal after the signal ran: %q", out)
	}
}

// A signal before the first removal, while planning under --yes, declines too.
func TestUninstallASignalWhilePlanningChangesNothing(t *testing.T) {
	sc := newUninstallScene(t)
	sc.setUp(t)
	sigs, _ := fakeUninstallSignals(t)
	saved := uninstallRemovalsHook
	uninstallRemovalsHook = func(rs []removal) []removal { sigs <- os.Interrupt; return rs }
	t.Cleanup(func() { uninstallRemovalsHook = saved })
	before := homeFiles(t, sc.home)
	code, out := sc.uninstall(t, "--yes")
	wantLines(t, code, exitDeclined, out, "\n  Not changed.\n")
	if strings.Contains(out, "✓") {
		t.Errorf("a removal ran after a signal while planning:\n%s", out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
}

// Every ✗ uninstall draws has a fix: line, the command that finishes by hand
// what the removal left. agentop is gone by the time the user reads it, so a fix
// for agentop's own settings names the file and the edit instead.
func TestUninstallNamesTheFixForEachFailure(t *testing.T) {
	bin := func(sc setupScene) string { return filepath.Join(sc.home, ".local", "bin") }
	// readOnly makes dir unwritable until the test ends. Root writes there anyway.
	readOnly := func(t *testing.T, dir string) {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("running as root: a 0o500 directory is still writable")
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}
	for _, c := range []struct {
		name, label string
		flags, args []string
		darwinOnly  bool
		// breakIt makes the removal fail, and returns the fix its ✗ must name: one
		// line per fix: row, as a fix done in steps has a row for each.
		breakIt func(t *testing.T, sc setupScene) string
	}{
		{"a job the supervisor will not unload", "stopped", nil, nil, false, func(t *testing.T, sc setupScene) string {
			failingUnload(t, sc.loaded)
			sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "", filepath.Join(bin(sc), "cortex"))
			if err != nil {
				t.Fatal(err)
			}
			return unloadByHand(&setupEnv{home: sc.home, goos: runtime.GOOS}, sp.unitFile)
		}},
		{"a background Cortex that will not stop", "stopped", []string{"--no-service"}, nil, false, func(t *testing.T, sc setupScene) string {
			installStub(t, "ps", "#!/bin/sh\ncase \"$*\" in\n  *comm=*) echo cortex ;;\n  *) exec /bin/ps \"$@\" ;;\nesac\n")
			failRemoval(t, "stopped")
			return "kill " + strconv.Itoa(readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid")))
		}},
		{"binaries in a read-only bin dir", "removed", nil, nil, false, func(t *testing.T, sc setupScene) string {
			readOnly(t, bin(sc))
			return "rm ~/.local/bin/agentop ~/.local/bin/cortex"
		}},
		// The user had CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC set before setup: the
		// fix restores it from the record rather than removing it with Cortex's keys.
		{"Claude Code settings in a read-only dir", "unrouted", []string{"--claude-code"}, nil, false, func(t *testing.T, sc setupScene) string {
			settings := filepath.Join(sc.home, settingsRel)
			doc, err := readSettings(settings)
			if err != nil {
				t.Fatal(err)
			}
			remove := setKeysBut(t, doc, envNoTelem)
			readOnly(t, filepath.Dir(settings))
			return `in ~/.claude/settings.json, remove the "env" keys ` + strings.Join(remove[:3], ", ") + ", …\n" +
				"and restore " + envNoTelem + " — your earlier values are in ~/.cortex/" + clientstate.RelPath
		}},
		{"IBM Bob", "unrouted", nil, nil, true, func(t *testing.T, sc setupScene) string {
			bob, err := bobSettingsPath(sc.home)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := bobWanted(filepath.Join(sc.home, ".cortex", "config.yaml"), sc.home)
			writeHomeFile(t, bob, "{}\n")
			if err := bobWriteKey(bob, bobProxyKey, want); err != nil {
				t.Fatal(err)
			}
			failRemovalWhere(t, func(r removal) bool { return r.item.What == "IBM Bob" })
			return `remove "` + bobProxyKey + `" from ` + (&setupEnv{home: sc.home}).tilde(bob)
		}},
		{"the bob shell function", "removed", nil, nil, false, func(t *testing.T, sc setupScene) string {
			if code := bobShellEnable(filepath.Join(sc.home, ".zshrc"), true, io.Discard, io.Discard); code != 0 {
				t.Fatalf("fixture: bobshell enable exit %d", code)
			}
			failRemovalWhere(t, func(r removal) bool { return r.item.What == "the bob shell function" })
			return `delete the lines from "` + bobShellMarkerStart + `" to "` + bobShellMarkerEnd + `" in ~/.zshrc`
		}},
		{"the PATH lines", "PATH", nil, nil, false, func(t *testing.T, _ setupScene) string {
			failRemoval(t, "PATH")
			return `delete the two lines marked "added by Cortex" from ~/.zshrc`
		}},
		{"~/.cortex under --purge", "purged", nil, []string{"--purge"}, false, func(t *testing.T, _ setupScene) string {
			failRemoval(t, "purged")
			return "rm -rf ~/.cortex"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.darwinOnly && runtime.GOOS != "darwin" {
				t.Skip("IBM Bob's settings are where agentop knows them on macOS only")
			}
			sc := newUninstallScene(t)
			var pid int
			stopProcessOnCleanup(t, &pid)
			if c.name == "Claude Code settings in a read-only dir" {
				writeHomeFile(t, filepath.Join(sc.home, settingsRel), `{"env":{"`+envNoTelem+`":"1"}}`)
			}
			sc.setUp(t, c.flags...)
			pid = readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid"))
			want := c.breakIt(t, sc)
			code, out := sc.uninstall(t, append([]string{"--yes"}, c.args...)...)
			if code != 1 {
				t.Errorf("exit %d, want 1:\n%s", code, out)
			}
			if fixes := fixesUnder(out, markLine("✗", c.label)); !slices.Equal(fixes, strings.Split(want, "\n")) {
				t.Errorf("the ✗ %s row's fixes are %q, want %q:\n%s", c.label, fixes, strings.Split(want, "\n"), out)
			}
			everyFailHasAFix(t, out)
		})
	}
}

// A fix that uses agentop is printed only while agentop is in the bin dir and no
// removal still to run takes it away; otherwise the fix by hand is.
func TestUninstallFixesNameAgentopOnlyWhileItStays(t *testing.T) {
	plainOutput(t)
	const withAgentop, byHand = "agentop configure bob disable", `remove "http.proxy" from the settings`
	fail := removal{label: "unrouted", fix: remedy{agentop: withAgentop, byHand: byHand},
		run: func(*checklist.Running) (string, []string, error) {
			return "", []string{"the settings"}, errors.New("refused")
		}}
	bins := removal{label: "removed", dropsAgentop: true,
		run: func(*checklist.Running) (string, []string, error) { return "agentop from ~/.local/bin", nil, nil }}
	for _, c := range []struct {
		name      string
		installed bool
		rs        []removal
		want      string
	}{
		{"agentop stays", true, []removal{fail}, withAgentop},
		{"agentop removed after the failure", true, []removal{fail, bins}, byHand},
		{"no agentop in the bin dir", false, []removal{fail}, byHand},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local", "bin")
			if c.installed {
				writeExe(t, filepath.Join(bin, "agentop"), "#!/bin/sh\n")
			}
			var w bytes.Buffer
			ui := checklist.New(&w, false)
			applyRemovals(&setupEnv{home: home, binDir: bin}, ui, c.rs, nil)
			ui.Close()
			if fixes := fixesUnder(w.String(), markLine("✗", "unrouted")); !slices.Equal(fixes, []string{c.want}) {
				t.Errorf("fixes %q, want [%q]:\n%s", fixes, c.want, w.String())
			}
		})
	}
}

// Claude Code's native installer puts claude in ~/.local/bin too, as a link to the
// version it keeps elsewhere. Removing the PATH lines would take it off PATH, so
// uninstall keeps them while the bin dir holds an executable that is not Cortex's:
// it says so on the consent screen and in a · row, neither left behind nor a
// change to the exit status. Cortex's own leftovers there need no PATH, and a
// file that is not executable is no tool: with only those, the lines go.
func TestUninstallKeepsThePATHLinesForOtherToolsInTheBinDir(t *testing.T) {
	for _, c := range []struct {
		name  string
		tools []string // other executables in the bin dir; "claude" is a link
		names string   // as the rows name them; "" when the lines go
	}{
		{"claude", []string{"claude"}, "claude"},
		{"four tools", []string{"claude", "gh", "uv", "zoxide"}, "claude, gh, uv, …"},
		{"only Cortex's", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newUninstallScene(t)
			sc.setUp(t)
			bin := filepath.Join(sc.home, ".local", "bin")
			writeExe(t, filepath.Join(bin, "agentop.new"), "#!/bin/sh\n# make dev-install's, interrupted\n")
			writeExe(t, filepath.Join(bin, ".agentop-setup-123"), "#!/bin/sh\n")
			writeHomeFile(t, filepath.Join(bin, "README"), "not a tool\n")
			for _, tool := range c.tools {
				if tool != "claude" {
					writeExe(t, filepath.Join(bin, tool), "#!/bin/sh\n")
					continue
				}
				version := filepath.Join(sc.home, ".local", "share", "claude", "versions", "2.0.0")
				writeExe(t, version, "#!/bin/sh\n")
				if err := os.Symlink(version, filepath.Join(bin, "claude")); err != nil {
					t.Fatal(err)
				}
			}
			rc := filepath.Join(sc.home, ".zshrc")
			before := readFile(t, rc)

			code, out := sc.uninstall(t, "--yes")
			if c.names == "" {
				wantLines(t, code, 0, out, markLine("✓", "PATH")+"~/.zshrc\n", "\n  Uninstalled.\n")
				if hasPathMarker(rc) || strings.Contains(out, "Kept: the PATH lines") {
					t.Errorf("the PATH lines were kept for Cortex's own files:\n%s", out)
				}
				return
			}
			why := "~/.local/bin also holds " + c.names
			wantLines(t, code, 0, out,
				"\n  Kept: the PATH lines in ~/.zshrc, which other tools need: "+why+"\n",
				"\n"+markLine("·", "PATH")+"kept the PATH lines in ~/.zshrc: "+why+"\n",
				"\n  Uninstalled.\n")
			if got := readFile(t, rc); got != before {
				t.Errorf("the PATH lines went though ~/.local/bin holds %s: ~/.zshrc is %q, want %q", c.names, got, before)
			}
			if strings.Contains(out, "Left behind") || strings.Contains(consentRow(out, "remove"), "the PATH lines") {
				t.Errorf("the kept PATH lines were planned for removal, or left behind:\n%s", out)
			}

			// Run again, uninstall has nothing left to remove, and asks nothing.
			code, out = sc.uninstall(t)
			wantLines(t, code, 0, out, "\n  Nothing to uninstall.\n  Kept the PATH lines in ~/.zshrc: "+why+"\n")
		})
	}
}

// setKeysBut are the managed keys doc's env sets, in managedKeys order, but for
// skip. The fixtures need four or more, so the fix's list is cut to three.
func setKeysBut(t *testing.T, doc map[string]any, skip string) []string {
	t.Helper()
	var keys []string
	for _, k := range managedKeys {
		if _, ok := envStrings(doc)[k]; ok && k != skip {
			keys = append(keys, k)
		}
	}
	if len(keys) < 4 {
		t.Fatalf("fixture: setup set only %q", keys)
	}
	return keys
}

// The record of what the user had before enable can hold a secret, a proxy URL
// with a password in it. The fix for a Claude Code unroute that failed names the
// keys and where the record is, never a value from it, on stdout or stderr, as
// what a terminal shows gets pasted and logged. Each fix line stays near 120
// columns.
func TestUninstallFixNeverPrintsARecordedValue(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0o500 directory is still writable")
	}
	const password = "s3cret"
	sc := newUninstallScene(t)
	sc.setUp(t, "--claude-code")
	settings, state := filepath.Join(sc.home, settingsRel), filepath.Join(sc.home, stateRel)
	st, err := readState(state)
	if err != nil || st == nil || st.Settings != settings {
		t.Fatalf("fixture: the record is %+v (%v), want one naming %s", st, err, settings)
	}
	proxy := "http://user:" + password + "@proxy:8080"
	st.Prior[envProxy] = &proxy
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	remove := setKeysBut(t, doc, envProxy)
	dir := filepath.Dir(settings)
	if err := os.Chmod(dir, 0o500); err != nil { // so the unroute fails, and its fix is drawn
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	code, out := sc.uninstall(t, "--yes") // stdout, then stderr
	if strings.Contains(out, password) {
		t.Errorf("uninstall printed the recorded value %q:\n%s", password, out)
	}
	want := []string{
		`in ~/.claude/settings.json, remove the "env" keys ` + strings.Join(remove[:3], ", ") + ", …",
		"and restore " + envProxy + " — your earlier values are in ~/.cortex/" + clientstate.RelPath,
	}
	fixes := fixesUnder(out, markLine("✗", "unrouted"))
	if code != 1 || !slices.Equal(fixes, want) {
		t.Errorf("exit %d, fixes %q; want 1 and %q:\n%s", code, fixes, want, out)
	}
	for _, f := range fixes {
		if n := len([]rune("      fix: " + f)); n > 120 {
			t.Errorf("a fix line is %d columns, want 120 at most: %q", n, f)
		}
	}
}

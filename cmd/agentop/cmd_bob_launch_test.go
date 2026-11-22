package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/internal/testhome"
)

// macLaunch is a bobLaunch with the macOS half on, pointed entirely into a temporary
// directory. Forced on rather than read from runtime.GOOS so these paths run on the
// Linux CI runners too, and never resolved from the process: `go test` has the
// developer's real $HOME and $SHELL, and resolving those would edit their ~/.bashrc.
func macLaunch(t *testing.T) bobLaunch {
	t.Helper()
	dir := t.TempDir()
	return bobLaunch{
		macOS:     true,
		shell:     "/bin/bash",
		rcPath:    filepath.Join(dir, ".bashrc"),
		statePath: filepath.Join(dir, ".cortex", "bob-state.json"),
	}
}

// stubProbe replaces the probe for one test, so no test runs the developer's own
// startup files. Returns a pointer to the number of times it ran.
func stubProbe(t *testing.T, value string, err error) *int {
	t.Helper()
	ran := 0
	saved := bobProbeLaunch
	bobProbeLaunch = func(string) (string, error) {
		ran++
		return value, err
	}
	t.Cleanup(func() { bobProbeLaunch = saved })
	return &ran
}

// The whole mechanism, end to end: both settings keys, the block, and the record that
// lets disable tell agentop's false from the user's.
func TestBobEnable_MacOSWritesBothKeysTheBlockAndTheRecord(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	ran := stubProbe(t, ca, nil)
	var out, errb bytes.Buffer
	if code := bobHalf(t, l, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
	doc := bobDoc(t, settings)
	if doc[bobProxyKey] != "http://127.0.0.1:47600" {
		t.Errorf("%s = %v", bobProxyKey, doc[bobProxyKey])
	}
	if doc[bobSysCertsKey] != false {
		t.Errorf("%s = %v, want false", bobSysCertsKey, doc[bobSysCertsKey])
	}
	if got := strings.Count(mustRead(t, l.rcPath), bobLaunchBlock(ca)); got != 1 {
		t.Errorf("rc file holds %d copies of the block, want 1:\n%s", got, mustRead(t, l.rcPath))
	}
	st, err := readState(l.statePath)
	if err != nil || st == nil {
		t.Fatalf("no ownership record: %v", err)
	}
	if p, ok := st.Prior[bobSysCertsKey]; !ok || p != nil || st.Settings != settings {
		t.Errorf("record does not say agentop added %s to %s: %+v", bobSysCertsKey, settings, st)
	}
	if *ran != 1 {
		t.Errorf("probe ran %d times, want once", *ran)
	}
	for _, want := range []string{"reopen it from the Dock or Spotlight", "bobide"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output omits %q:\n%s", want, out.String())
		}
	}
	// The keychain route is gone on macOS: no sudo, no trust change.
	if strings.Contains(out.String(), "sudo") || strings.Contains(out.String(), "add-trusted-cert") {
		t.Errorf("still prints a keychain trust change:\n%s", out.String())
	}
}

// The issue that started this: a Bob that has never saved a setting has no
// settings.json, and enable must create it rather than refuse.
func TestBobEnable_MacOSCreatesAMissingSettingsFile(t *testing.T) {
	settings, cfg := fixture(t, "")
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	stubProbe(t, ca, nil)
	var out, errb bytes.Buffer
	if code := bobHalf(t, l, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
	doc := bobDoc(t, settings)
	if doc[bobProxyKey] == nil || doc[bobSysCertsKey] != false || len(doc) != 2 {
		t.Errorf("created file holds %v, want exactly the two keys", doc)
	}
	if _, err := os.Stat(settings + ".bak"); err == nil {
		t.Error("wrote a .bak of a file that did not exist")
	}
}

// enable then disable returns BOTH files byte for byte, the promise bobshell makes for
// its block and bob makes for its settings — including an rc file whose last line has
// no newline, the case that once made bobshell's round trip lossy.
func TestBobRoundTrip_MacOSIsByteIdentical(t *testing.T) {
	for name, rc := range map[string]string{
		"no rc file":          "",
		"trailing newline":    "export EDITOR=vim\n",
		"no trailing newline": "export EDITOR=vim",
	} {
		t.Run(name, func(t *testing.T) {
			settings, cfg := fixture(t, bobHandFormatted)
			l := macLaunch(t)
			if rc != "" {
				if err := os.WriteFile(l.rcPath, []byte(rc), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, ca := bobWanted(cfg, t.TempDir())
			stubProbe(t, ca, nil)
			var out, errb bytes.Buffer
			c := bobHalf(t, l, settings, cfg, true, &out, &errb)
			if code := c.enable(); code != 0 {
				t.Fatalf("enable exit %d: %s%s", code, out.String(), errb.String())
			}
			out.Reset()
			errb.Reset()
			if code := c.disable(); code != 0 {
				t.Fatalf("disable exit %d: %s%s", code, out.String(), errb.String())
			}
			if got := mustRead(t, settings); got != bobHandFormatted {
				t.Errorf("settings not restored byte for byte:\ngot:\n%s\nwant:\n%s", got, bobHandFormatted)
			}
			got, err := os.ReadFile(l.rcPath)
			if rc == "" {
				// enable created the file; disable leaves it, empty. bobshell does the
				// same, and deleting a dotfile the user may since have edited is not
				// disable's call.
				if err != nil || len(got) != 0 {
					t.Errorf("rc file after round trip = %q, %v; want empty", got, err)
				}
			} else if string(got) != rc {
				t.Errorf("rc not restored byte for byte: got %q, want %q", got, rc)
			}
			if _, err := os.Stat(l.statePath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("ownership record survived disable: %v", err)
			}
		})
	}
}

// A false the user set is byte-identical to the one enable writes. Without the record,
// disable could not tell them apart; with it, the user's stays.
func TestBobDisable_MacOSLeavesTheUsersOwnFalse(t *testing.T) {
	settings, cfg := fixture(t, `{"http.systemCertificates": false}`)
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	stubProbe(t, ca, nil)
	var out, errb bytes.Buffer
	c := bobHalf(t, l, settings, cfg, true, &out, &errb)
	if code := c.enable(); code != 0 {
		t.Fatalf("enable exit %d: %s%s", code, out.String(), errb.String())
	}
	if _, err := os.Stat(l.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("recorded ownership of a value the user set: %v", err)
	}
	out.Reset()
	if code := c.disable(); code != 0 {
		t.Fatalf("disable exit %d: %s%s", code, out.String(), errb.String())
	}
	if v, ok := bobDoc(t, settings)[bobSysCertsKey]; !ok || v != false {
		t.Errorf("disable removed the user's own %s (now %v, present %v)", bobSysCertsKey, v, ok)
	}
	if !strings.Contains(out.String(), "which agentop did not set") {
		t.Errorf("does not say why the key was left:\n%s", out.String())
	}
}

// A record that no longer matches — the value changed since enable — is the user's
// value now, and stays.
func TestBobDisable_MacOSLeavesAValueChangedSinceEnable(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	stubProbe(t, ca, nil)
	var out, errb bytes.Buffer
	c := bobHalf(t, l, settings, cfg, true, &out, &errb)
	if code := c.enable(); code != 0 {
		t.Fatalf("enable exit %d: %s", code, errb.String())
	}
	if err := bobWriteKey(settings, bobSysCertsKey, true); err != nil {
		t.Fatal(err)
	}
	if code := c.disable(); code != 0 {
		t.Fatalf("disable exit %d: %s", code, errb.String())
	}
	if v := bobDoc(t, settings)[bobSysCertsKey]; v != true {
		t.Errorf("%s = %v, want the user's true left in place", bobSysCertsKey, v)
	}
}

// A value somebody chose is refused, and the refusal writes NOTHING — the block
// included, which is written first on the success path.
func TestBobEnable_MacOSRefusesAChosenSystemCertificates(t *testing.T) {
	for name, body := range map[string]string{
		"true":      `{"http.systemCertificates": true}`,
		"non-bool":  `{"http.systemCertificates": "yes"}`,
		"json null": `{"http.systemCertificates": null}`,
	} {
		t.Run(name, func(t *testing.T) {
			settings, cfg := fixture(t, body)
			l := macLaunch(t)
			ran := stubProbe(t, "", nil)
			asked := noPrompt(t, true)
			var out, errb bytes.Buffer
			if code := bobHalf(t, l, settings, cfg, true, &out, &errb).enable(); code != 1 {
				t.Errorf("exit %d, want 1:\n%s%s", code, out.String(), errb.String())
			}
			if !strings.Contains(errb.String(), bobSysCertsKey) {
				t.Errorf("refusal does not name the key:\n%s", errb.String())
			}
			if mustRead(t, settings) != body {
				t.Error("changed the settings file for a refusal")
			}
			if _, err := os.Stat(l.rcPath); !errors.Is(err, os.ErrNotExist) {
				t.Error("wrote the shell block for a refusal")
			}
			if *asked != 0 || *ran != 0 {
				t.Errorf("prompted %d / probed %d times before refusing", *asked, *ran)
			}
		})
	}
}

// The block lands before the settings. Written the other way round, a failed block
// write would leave Bob's extensions off the keychain with no Cortex CA instead.
// Pinned by making the settings write fail and checking the block is there.
func TestBobEnable_MacOSWritesTheBlockBeforeTheSettings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permission this test relies on")
	}
	settings, cfg := fixture(t, "{}")
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	stubProbe(t, ca, nil)
	dir := filepath.Dir(settings)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var out, errb bytes.Buffer
	if code := bobHalf(t, l, settings, cfg, true, &out, &errb).enable(); code != 1 {
		t.Fatalf("exit %d with an unwritable settings directory, want 1", code)
	}
	if !strings.Contains(mustRead(t, l.rcPath), bobLaunchBlock(ca)) {
		t.Error("the block was not written before the settings write failed")
	}
}

// What the probe found is what enable says. The empty case is the one that matters
// most: a block in a file the login shell never reads fails silently, and bash on
// macOS is the usual way to get there.
func TestBobEnable_MacOSReportsTheProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		err     error
		want    []string
		wantNot []string
	}{
		// Success is SILENCE on a fresh enable: "Enabled." followed by nothing reads
		// as verified, and the already-enabled arm is where the probe's answer is
		// the point of the run.
		{name: "set to the CA", value: "CA", want: []string{"Enabled."}, wantNot: []string{"WARNING", "Checked:"}},
		{name: "not set", value: "", want: []string{"WARNING", "does not set", "~/.bash_profile"}},
		{name: "set to something else", value: "/elsewhere.pem", want: []string{"WARNING", "/elsewhere.pem", "something after the block"}},
		{name: "probe failed", err: errors.New("bash did not finish"), want: []string{"Not checked: bash did not finish"}, wantNot: []string{"WARNING", "Checked:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, "{}")
			l := macLaunch(t)
			_, ca := bobWanted(cfg, t.TempDir())
			v := tc.value
			if v == "CA" {
				v = ca
			}
			stubProbe(t, v, tc.err)
			var out, errb bytes.Buffer
			if code := bobHalf(t, l, settings, cfg, true, &out, &errb).enable(); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output omits %q:\n%s", w, out.String())
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out.String(), w) {
					t.Errorf("output has %q:\n%s", w, out.String())
				}
			}
		})
	}
}

// A re-run with everything in place writes nothing and still probes: re-running enable
// is how someone asks whether it works.
func TestBobEnable_MacOSAlreadyEnabledStillProbes(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	ran := stubProbe(t, ca, nil)
	var out, errb bytes.Buffer
	c := bobHalf(t, l, settings, cfg, true, &out, &errb)
	if code := c.enable(); code != 0 {
		t.Fatalf("first enable exit %d: %s", code, errb.String())
	}
	beforeRC, beforeSettings := mustRead(t, l.rcPath), mustRead(t, settings)
	out.Reset()
	asked := noPrompt(t, true)
	c.yes = false
	if code := c.enable(); code != 0 {
		t.Fatalf("second enable exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Already enabled") {
		t.Errorf("second run does not say it is already enabled:\n%s", out.String())
	}
	if *asked != 0 {
		t.Error("asked permission to do nothing")
	}
	if mustRead(t, l.rcPath) != beforeRC || mustRead(t, settings) != beforeSettings {
		t.Error("second run changed a file")
	}
	if *ran != 2 {
		t.Errorf("probe ran %d times across two enables, want 2", *ran)
	}
}

// bobshell's advice case: a shell agentop does not know the startup file of. The
// settings are still written; the block is printed, and no file is created.
func TestBobEnable_MacOSUnknownShellPrintsTheBlock(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	l := macLaunch(t)
	l.rcPath, l.rcErr = "", errors.New("I only know where zsh and bash read their startup files, and $SHELL is /usr/bin/fish")
	_, ca := bobWanted(cfg, t.TempDir())
	stubProbe(t, ca, nil)
	var out, errb bytes.Buffer
	if code := bobHalf(t, l, settings, cfg, true, &out, &errb).enable(); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), bobLaunchBlock(ca)) {
		t.Errorf("does not print the block to place by hand:\n%s", out.String())
	}
	if bobDoc(t, settings)[bobSysCertsKey] != false {
		t.Error("did not write the settings")
	}
}

// bobshell's rules for a block that is not exactly what agentop writes: neither enable
// nor disable touches it.
func TestBobLaunchBlock_EditedOrDuplicatedIsLeftAlone(t *testing.T) {
	_, cfg := fixture(t, "")
	_, ca := bobWanted(cfg, t.TempDir())
	edited := "\n" + bobLaunchMarkerStart + "\nexport NODE_EXTRA_CA_CERTS=/mine.pem\n" + bobLaunchMarkerEnd + "\n"
	for name, rc := range map[string]string{
		"edited":     edited,
		"duplicated": bobLaunchBlock(ca) + bobLaunchBlock(ca),
	} {
		t.Run(name, func(t *testing.T) {
			settings, cfg := fixture(t, "{}")
			l := macLaunch(t)
			if err := os.WriteFile(l.rcPath, []byte(rc), 0o644); err != nil {
				t.Fatal(err)
			}
			stubProbe(t, ca, nil)
			var out, errb bytes.Buffer
			c := bobHalf(t, l, settings, cfg, true, &out, &errb)
			if code := c.enable(); code != 0 {
				t.Fatalf("enable exit %d: %s", code, errb.String())
			}
			if code := c.disable(); code != 0 {
				t.Fatalf("disable exit %d: %s", code, errb.String())
			}
			if mustRead(t, l.rcPath) != rc {
				t.Errorf("changed a block agentop did not write exactly once:\n%s", mustRead(t, l.rcPath))
			}
			if !strings.Contains(out.String(), "Left alone") {
				t.Errorf("does not say it left the block:\n%s", out.String())
			}
		})
	}
}

// Both halves write the SAME rc file on macOS, so this is the test of the
// shared-file apply: enable's two blocks must both land, and disable must take
// both out. Until the applies re-read the file at write time, the second
// whole-file write silently dropped the first half's block — status then
// reported the CA-trust block missing right after a successful enable. Found
// live rather than by the suite, because bobHalf deliberately gives the halves
// separate scratch files; this test points them at the one file, as runBob does
// on macOS.
func TestBobEnableDisable_SharedRCFileKeepsBothBlocks(t *testing.T) {
	settings, cfg := fixture(t, "{}")
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	stubProbe(t, ca, nil)
	var out, errb bytes.Buffer
	c := bobHalf(t, l, settings, cfg, true, &out, &errb)
	c.shellRC = l.rcPath // the one file both halves write, as on macOS

	if code := c.enable(); code != 0 {
		t.Fatalf("enable exit %d: %s%s", code, out.String(), errb.String())
	}
	got := mustRead(t, l.rcPath)
	if !strings.Contains(got, bobLaunchBlock(ca)) {
		t.Errorf("the CA-trust block did not survive the shell half's write:\n%s", got)
	}
	if !strings.Contains(got, bobShellBlock) {
		t.Errorf("the bob function did not land:\n%s", got)
	}

	out.Reset()
	if code := c.disable(); code != 0 {
		t.Fatalf("disable exit %d: %s%s", code, out.String(), errb.String())
	}
	if got := mustRead(t, l.rcPath); got != "" {
		t.Errorf("disable left bytes behind in the shared rc file: %q", got)
	}
}

// The off switch works whatever state Bob is in: with no settings file at all, the
// block enable wrote still comes out.
func TestBobDisable_MacOSWithNoSettingsFileStillRemovesTheBlock(t *testing.T) {
	settings, cfg := fixture(t, "")
	l := macLaunch(t)
	_, ca := bobWanted(cfg, t.TempDir())
	if err := os.WriteFile(l.rcPath, []byte("export EDITOR=vim\n"+bobLaunchBlock(ca)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := bobHalf(t, l, settings, cfg, true, &out, &errb).disable(); code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
	if got := mustRead(t, l.rcPath); got != "export EDITOR=vim\n" {
		t.Errorf("rc after disable = %q", got)
	}
	if _, err := os.Stat(settings); !errors.Is(err, os.ErrNotExist) {
		t.Error("disable created a settings file")
	}
}

// Status, and the table row it shares a verdict with, call Bob configured only with
// all three in place. A proxy alone is the state an earlier enable left, and on macOS
// it is not enough for Bob's extensions to read Cortex's CA.
func TestBobStatus_MacOSNeedsAllThree(t *testing.T) {
	_, cfg0 := fixture(t, "")
	_, ca := bobWanted(cfg0, t.TempDir())
	for _, tc := range []struct {
		name     string
		settings string
		block    bool
		wantYes  bool
		state    configureState
	}{
		{"all three", `{"http.proxy": "http://127.0.0.1:47600", "http.systemCertificates": false}`, true, true, configureOn},
		{"proxy only", `{"http.proxy": "http://127.0.0.1:47600"}`, false, false, configurePartial},
		{"no block", `{"http.proxy": "http://127.0.0.1:47600", "http.systemCertificates": false}`, false, false, configurePartial},
		{"system certificates on", `{"http.proxy": "http://127.0.0.1:47600", "http.systemCertificates": true}`, true, false, configurePartial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, cfg := fixture(t, tc.settings)
			l := macLaunch(t)
			want, ca := bobWanted(cfg, t.TempDir())
			if tc.block {
				if err := os.WriteFile(l.rcPath, []byte(bobLaunchBlock(ca)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ran := stubProbe(t, ca, nil)
			var out, errb bytes.Buffer
			if code := bobHalf(t, l, settings, cfg, false, &out, &errb).status(); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if gotYes := strings.HasPrefix(out.String(), bobStatusYes+"\n"); gotYes != tc.wantYes {
				t.Errorf("verdict yes = %v, want %v:\n%s", gotYes, tc.wantYes, out.String())
			}
			if got := bobState(settings, cfg, want, l, ca); got.State != tc.state {
				t.Errorf("table state = %s, want %s (%s)", got.State, tc.state, got.Detail)
			}
			// Status reads files and runs nothing.
			if *ran != 0 {
				t.Errorf("status ran the probe %d times", *ran)
			}
		})
	}
	_ = ca
}

// The probe against a real bash, which is the only test of the block itself: the guard
// passes for IBM Bob's bundle id, and a login shell reaches ~/.bashrc only through
// ~/.bash_profile — the case the WARNING exists for.
func TestBobProbeLaunch_RealBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	ca := "/tmp/cortex test/ca.crt"
	for _, tc := range []struct {
		name    string
		profile string
		want    string
	}{
		{"profile sources bashrc", "[ -f ~/.bashrc ] && . ~/.bashrc\n", ca},
		{"bashrc never read", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			testhome.Set(t, home)
			// A value agentop itself was started with must not pass for one the
			// shell set.
			t.Setenv(envCACerts, "/inherited.pem")
			if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(bobLaunchBlock(ca)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(tc.profile), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := bobProbeLaunch(bash)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if got != tc.want {
				t.Errorf("probe = %q, want %q", got, tc.want)
			}
		})
	}
}

// The guard admits IBM Bob and nothing else: another VS Code fork resolving its own
// environment, or a plain login shell, does not get the variable.
func TestBobLaunchBlock_GuardAdmitsOnlyBob(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash on PATH")
	}
	block := bobLaunchBlock("/ca.crt")
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"IBM Bob resolving", []string{"VSCODE_RESOLVING_ENVIRONMENT=1", "__CFBundleIdentifier=" + bobBundleID}, "/ca.crt"},
		{"VS Code resolving", []string{"VSCODE_RESOLVING_ENVIRONMENT=1", "__CFBundleIdentifier=com.microsoft.VSCode"}, ""},
		{"Bob's id in a terminal", []string{"__CFBundleIdentifier=" + bobBundleID}, ""},
		{"a terminal", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bash, "--noprofile", "--norc", "-c", block+`printf %s "$NODE_EXTRA_CA_CERTS"`)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, tc.env...)
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("NODE_EXTRA_CA_CERTS = %q, want %q", out, tc.want)
			}
		})
	}
}

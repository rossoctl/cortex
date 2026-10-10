package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// doctorScene is a setup scene for doctor. Its health endpoint answers 503 while
// down is set, and its /readyz names a plugin that is not ready while notReady
// is. PATH holds only the scene's stubs and the system dirs, so no agentop or
// python3 of this machine's is found. The unread report is never fetched: the
// scene serves no stats server, so whatever answered at its config's stats address
// would be some other proxy on this machine.
type doctorScene struct {
	setupScene
	down, notReady *atomic.Bool
}

func newDoctorScene(t *testing.T) doctorScene {
	t.Helper()
	down, notReady := new(atomic.Bool), new(atomic.Bool)
	sc := newSetupScene(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case down.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.URL.Path == "/readyz" && notReady.Load():
			http.Error(w, "outbound plugin not ready: tokenexchange", http.StatusServiceUnavailable)
		}
	})
	var keep []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.HasPrefix(dir, os.TempDir()) { // installStub's dirs
			keep = append(keep, dir)
		}
	}
	t.Setenv("PATH", strings.Join(append(keep, "/usr/bin", "/bin", "/usr/sbin", "/sbin"), string(os.PathListSeparator)))
	for _, name := range []string{"launchctl", "systemctl", "lsof"} {
		if got, err := exec.LookPath(name); err != nil || !strings.HasPrefix(got, os.TempDir()) {
			t.Fatalf("%s resolves to %q (%v), not the scene's stub", name, got, err)
		}
	}
	saved := fetchUnreadReport
	fetchUnreadReport = func(string) (tlsbridge.UnreadReport, bool) { return tlsbridge.UnreadReport{}, false }
	t.Cleanup(func() { fetchUnreadReport = saved })
	return doctorScene{setupScene: sc, down: down, notReady: notReady}
}

func (sc setupScene) doctor(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runDoctor(args, &out, &errb)
	return code, out.String() + errb.String()
}

// installed runs setup on sc, then writes the CA Cortex mints as it starts: the
// fake supervisor starts nothing, so nothing else would.
func (sc setupScene) installed(t *testing.T, args ...string) {
	t.Helper()
	if code, out := sc.run(t, append([]string{"--from", sc.stage, "--yes"}, args...)...); code != 0 {
		t.Fatalf("setup exit %d:\n%s", code, out)
	}
	writeCA(t, filepath.Join(sc.home, ".cortex", "ca"), time.Now().Add(90*24*time.Hour))
}

// writeCA writes a self-signed CA valid until notAfter into dir as ca.crt, and the
// same PEM as bundle.crt.
func writeCA(t *testing.T, dir string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "authbridge-tls-bridge-ca"},
		NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca.crt", "bundle.crt"} {
		if err := os.WriteFile(filepath.Join(dir, name), crt, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// markLine is the start of a checklist line: its glyph, then the padded label.
func markLine(glyph, label string) string { return fmt.Sprintf("  %s %-12s ", glyph, label) }

// everyFailHasAFix walks every line of out: each ✗ line is followed by a fix:
// row, after the rows of detail an uninstall ✗ may carry under it.
func everyFailHasAFix(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if !strings.Contains(l, "✗") {
			continue
		}
		j := i + 1
		for j < len(lines) && strings.HasPrefix(lines[j], "      ") && !strings.HasPrefix(lines[j], "      fix: ") {
			j++ // a detail row
		}
		if j == len(lines) || !strings.HasPrefix(lines[j], "      fix: ") {
			t.Errorf("line %d, %q, is not followed by a fix: line:\n%s", i+1, l, out)
		}
	}
}

// fixAfter is the fix on the row after the first line that starts with mark,
// and whether there is such a line.
func fixAfter(out, mark string) (string, bool) {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, mark) {
			if i+1 < len(lines) {
				return strings.TrimPrefix(lines[i+1], "      fix: "), true
			}
			return "", true
		}
	}
	return "", false
}

func TestDoctorAfterSetupFindsNothingToFix(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t, "--claude-code")
	before := homeFiles(t, sc.home)
	code, out := sc.doctor(t)
	if code != 0 || strings.Contains(out, "✗") || !strings.HasSuffix(out, "\n\n  Nothing to fix.\n") {
		t.Errorf("exit %d, want 0 with nothing to fix:\n%s", code, out)
	}
	if !strings.HasPrefix(out, "  rosso cortex · doctor   "+version+" · "+runtime.GOOS+"/"+runtime.GOARCH+"\n\n") {
		t.Errorf("the header is not doctor's:\n%s", out)
	}
	for _, label := range []string{"installed", "config", "started", "routed", "CA"} {
		if !strings.Contains(out, "\n"+markLine("✓", label)) {
			t.Errorf("no ✓ %s line:\n%s", label, out)
		}
	}
	// The PATH edit reaches new terminals only, and this test's PATH lacks the bin dir.
	if want := "\n" + markLine("!", "PATH") + "in ~/.zshrc; new terminals only\n  "; !strings.Contains(out, want) {
		t.Errorf("no PATH advisory %q:\n%s", want, out)
	}
	sameFiles(t, before, homeFiles(t, sc.home))
}

func TestDoctorNamesTheFixForEachBrokenStep(t *testing.T) {
	const routedFix = "agentop setup --claude-code" // setup ran with --claude-code
	for _, c := range []struct {
		name, label, fix string
		breakIt          func(t *testing.T, sc doctorScene)
	}{
		{"PATH block removed", "PATH", routedFix, func(t *testing.T, sc doctorScene) {
			rc := filepath.Join(sc.home, ".zshrc")
			b, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			block := pathBlock(filepath.Join(sc.home, ".local", "bin"))
			if !strings.Contains(string(b), block) {
				t.Fatalf("setup left no PATH block in .zshrc:\n%s", b)
			}
			if err := os.WriteFile(rc, []byte(strings.Replace(string(b), block, "", 1)), 0o644); err != nil { //nolint:gosec
				t.Fatal(err)
			}
		}},
		{"job unloaded", "started", routedFix, func(t *testing.T, sc doctorScene) {
			if err := os.Remove(sc.loaded); err != nil { // the fake supervisor's bootout
				t.Fatal(err)
			}
			sc.down.Store(true) // and the proxy it ran stops answering
		}},
		// The stamp check: the same version, other bytes, so the service runs a cortex
		// that is no longer the installed one.
		{"cortex replaced", "started", routedFix, func(t *testing.T, sc doctorScene) {
			writeExe(t, filepath.Join(sc.home, ".local", "bin", "cortex"), "#!/bin/sh\n# rebuilt\necho cortex v9.9.9\n")
		}},
		{"bundle.crt deleted", "CA", "agentop setup --restart", func(t *testing.T, sc doctorScene) {
			if err := os.Remove(filepath.Join(sc.home, ".cortex", "ca", "bundle.crt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"unit deleted", "started", routedFix, func(t *testing.T, sc doctorScene) {
			sp, err := resolveServicePaths(filepath.Join(sc.home, ".cortex", "config.yaml"), "",
				filepath.Join(sc.home, ".local", "bin", "cortex"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(sp.unitFile); err != nil {
				t.Fatal(err)
			}
		}},
		{"config deleted", "config", routedFix, func(t *testing.T, sc doctorScene) {
			if err := os.Remove(filepath.Join(sc.home, ".cortex", "config.yaml")); err != nil {
				t.Fatal(err)
			}
		}},
		{"cortex deleted", "installed", installerOneLiner, func(t *testing.T, sc doctorScene) {
			if err := os.Remove(filepath.Join(sc.home, ".local", "bin", "cortex")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			args := []string{"--claude-code"}
			if c.label == "CA" {
				// Not routed: the routed check would name the missing file too, and so
				// fail the run without the CA's own ✗.
				args = nil
			}
			sc.installed(t, args...)
			c.breakIt(t, sc)
			code, out := sc.doctor(t)
			if code != 1 || !strings.HasSuffix(out, "\n\n  Run the fix lines above, then agentop doctor again.\n") {
				t.Errorf("exit %d, want 1 and the fix ending:\n%s", code, out)
			}
			fix, ok := fixAfter(out, markLine("✗", c.label))
			if !ok || fix != c.fix {
				t.Errorf("want a ✗ %s line with the fix %q on the row after it, got %q (found: %v):\n%s",
					c.label, c.fix, fix, ok, out)
			}
			everyFailHasAFix(t, out)
		})
	}
}

// On a machine with nothing installed the binaries step refuses, in repair mode,
// with the installer one-liner as its fix; doctor shows that first.
func TestDoctorWithNothingInstalledSaysHowToInstall(t *testing.T) {
	sc := newDoctorScene(t)
	before := homeFiles(t, sc.home)
	code, out := sc.doctor(t)
	first := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "✗") {
			first = l
			break
		}
	}
	if code != 1 || !strings.HasPrefix(first, markLine("✗", "installed")) {
		t.Errorf("exit %d, want 1 with the first ✗ naming installed, got %q:\n%s", code, first, out)
	}
	if fix, _ := fixAfter(out, markLine("✗", "installed")); fix != installerOneLiner {
		t.Errorf("the installed line's fix is %q, want the installer one-liner:\n%s", fix, out)
	}
	everyFailHasAFix(t, out)
	sameFiles(t, before, homeFiles(t, sc.home))
}

// Each plan result maps to one line, and every ✗ to a fix: the problem's own, or
// the one doctor passes in.
func TestDoctorCheckMapsEachPlanResult(t *testing.T) {
	plainOutput(t)
	const fix = "agentop setup --claude-code"
	fail, warn, ok := markLine("✗", "thing"), markLine("!", "thing"), markLine("✓", "thing")
	for _, c := range []struct {
		name   string
		p      stepPlan
		prob   *problem
		want   string
		failed bool
	}{
		{"a problem with its fixes", stepPlan{}, &problem{reason: "r1", fix: []string{"do a", "do b"}},
			fail + "r1\n      fix: do a\n      fix: do b\n", true},
		{"a problem with none", stepPlan{}, &problem{reason: "r2"}, fail + "r2\n      fix: " + fix + "\n", true},
		{"a problem over a done plan", stepPlan{done: true, doneMsg: "fine"}, &problem{reason: "r3"},
			fail + "r3\n      fix: " + fix + "\n", true},
		{"hidden", stepPlan{done: true, hidden: true, doneMsg: "unseen"}, nil, "", false},
		{"advice", stepPlan{done: true, advice: &problem{reason: "r4", fix: []string{"exec zsh"}}}, nil,
			warn + "r4\n      fix: exec zsh\n", false},
		{"done", stepPlan{done: true, doneMsg: "all good"}, nil, ok + "all good\n", false},
		{"a change with where", stepPlan{verb: "add", what: "~/.local/bin to PATH", where: "~/.zshrc (2 marked lines)"}, nil,
			fail + "setup would add ~/.local/bin to PATH · ~/.zshrc (2 marked lines)\n      fix: " + fix + "\n", true},
		{"a change without", stepPlan{verb: "run", what: "the cortex proxy"}, nil,
			fail + "setup would run the cortex proxy\n      fix: " + fix + "\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			ui := checklist.New(&b, false)
			failed := doctorCheck(ui, "thing", c.p, c.prob, fix)
			if failed != c.failed || b.String() != c.want {
				t.Errorf("failed %v, want %v; got\n%q\nwant\n%q", failed, c.failed, b.String(), c.want)
			}
		})
	}
}

func TestDoctorChecksTheCA(t *testing.T) {
	plainOutput(t)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour
	date := func(d time.Duration) string { return now.Add(d).Local().Format(time.DateOnly) }
	const caFile, mark = "~/.cortex/ca/ca.crt", "  %s CA           "
	expiring := func(d time.Duration) string {
		return fmt.Sprintf(mark, "!") + caFile + " expires " + date(d) + "\n      fix: agentop service restart\n" +
			"      Cortex mints a new CA as it restarts; restart the agents that use it afterwards\n"
	}
	expired := func(d time.Duration) string {
		return fmt.Sprintf(mark, "✗") + caFile + " expired " + date(d) + "\n      fix: agentop service restart\n" +
			"      Cortex mints a new CA as it restarts; restart the agents that use it afterwards\n"
	}
	remint := "\n      fix: agentop setup --restart\n"
	enabled := func(dir string) string { return "tls_bridge:\n  mode: enabled\n  ca_dir: " + dir + "\n" }
	for _, c := range []struct {
		name   string
		bridge func(dir string) string // the config's tls_bridge block; nil writes no config
		ca     func(t *testing.T, dir string)
		want   string
		failed bool
	}{
		{"ok", enabled, func(t *testing.T, dir string) { writeCA(t, dir, now.Add(90*day)) },
			fmt.Sprintf(mark, "✓") + caFile + " · expires " + date(90*day) + "\n", false},
		{"31 days", enabled, func(t *testing.T, dir string) { writeCA(t, dir, now.Add(31*day)) },
			fmt.Sprintf(mark, "✓") + caFile + " · expires " + date(31*day) + "\n", false},
		{"29 days", enabled, func(t *testing.T, dir string) { writeCA(t, dir, now.Add(29*day)) }, expiring(29 * day), false},
		{"soon", enabled, func(t *testing.T, dir string) { writeCA(t, dir, now.Add(10*day)) }, expiring(10 * day), false},
		// x509 reads a certificate as valid through its NotAfter, and expired a second later.
		{"at its NotAfter", enabled, func(t *testing.T, dir string) { writeCA(t, dir, now) }, expiring(0), false},
		{"a second past", enabled, func(t *testing.T, dir string) { writeCA(t, dir, now.Add(-time.Second)) }, expired(-time.Second), true},
		{"expired", enabled, func(t *testing.T, dir string) { writeCA(t, dir, now.Add(-3*day)) }, expired(-3 * day), true},
		{"no ca.crt", enabled, func(*testing.T, string) {}, fmt.Sprintf(mark, "✗") + "no ca.crt in ~/.cortex/ca" + remint, true},
		{"no bundle.crt", enabled, func(t *testing.T, dir string) {
			writeCA(t, dir, now.Add(60*day))
			if err := os.Remove(filepath.Join(dir, "bundle.crt")); err != nil {
				t.Fatal(err)
			}
		}, fmt.Sprintf(mark, "✗") + "no bundle.crt in ~/.cortex/ca" + remint, true},
		{"garbage ca.crt", enabled, func(t *testing.T, dir string) {
			writeCA(t, dir, now.Add(45*day))
			if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a certificate\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, fmt.Sprintf(mark, "✗") + caFile + " is not a certificate" + remint, true},
		{"no tls_bridge", func(string) string { return "" }, func(*testing.T, string) {}, "", false},
		// A bridge that is off mints no CA, so a missing one is nothing a fix clears.
		{"bridge disabled", func(dir string) string { return "tls_bridge:\n  mode: disabled\n  ca_dir: " + dir + "\n" },
			func(*testing.T, string) {}, "", false},
		{"no config", nil, func(*testing.T, string) {}, "", false},
		{"a config that will not load", func(string) string { return "listener: [\n" }, func(*testing.T, string) {}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTestSetupEnv(t)
			dir := filepath.Join(env.cortexDir, "ca")
			if c.bridge != nil {
				body := "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47600\n" + c.bridge(dir)
				if err := os.MkdirAll(env.cortexDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(env.configPath(), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c.ca(t, dir)
			var b bytes.Buffer
			failed := checkCA(env, checklist.New(&b, false), now)
			if failed != c.failed || b.String() != c.want {
				t.Errorf("failed %v, want %v; got\n%q\nwant\n%q", failed, c.failed, b.String(), c.want)
			}
		})
	}
}

// The usage errors return before doctor looks at anything; the scene is there in
// case one does not.
func TestDoctorUsage(t *testing.T) {
	newDoctorScene(t)
	for _, arg := range []string{"--help", "-h"} {
		var out, errb bytes.Buffer
		if code := runDoctor([]string{arg}, &out, &errb); code != 0 || out.String() != doctorUsage || errb.Len() != 0 {
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
		if code := runDoctor(c.args, &out, &errb); code != 2 || out.Len() != 0 || !strings.HasPrefix(errb.String(), c.want) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want 2 and %q on stderr", c.args, code, out.String(), errb.String(), c.want)
		}
	}
}

// Routed means Cortex routed Claude Code: enable's state record is there, or the
// proxy in settings.json is the one enable would write from the config. A proxy
// the user set, a corporate one say, is not Cortex's.
func TestClaudeCodeRoutedMeansCortexRoutedIt(t *testing.T) {
	settingsWith := func(proxy string) string { return `{"env":{"HTTPS_PROXY":"` + proxy + `"}}` }
	for _, c := range []struct {
		name     string
		state    bool
		settings string // "" writes none
		want     bool
	}{
		{"state record present", true, "", true},
		{"no state, the config's proxy", false, settingsWith("http://127.0.0.1:47610"), true},
		{"no state, a corporate proxy", false, settingsWith("http://proxy.corp:8080"), false},
		{"no state, a Cortex-looking proxy the config does not name", false, settingsWith("http://127.0.0.1:47600"), false},
		{"no settings", false, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTestSetupEnv(t)
			// Port 47610, not the default, so the match is with this config.
			writeExe(t, env.configPath(), "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47610\n")
			if c.state {
				writeExe(t, filepath.Join(env.home, stateRel), "{}\n")
			}
			if c.settings != "" {
				writeExe(t, filepath.Join(env.home, settingsRel), c.settings)
			}
			if got := claudeCodeRouted(env); got != c.want {
				t.Errorf("claudeCodeRouted = %v, want %v", got, c.want)
			}
		})
	}
}

// A corporate HTTPS_PROXY in settings.json, with Claude Code never routed, is
// the user's own: doctor checks no routing, so it neither fails nor names it.
func TestDoctorLeavesACorporateProxyAlone(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t) // no --claude-code
	writeExe(t, filepath.Join(sc.home, settingsRel), `{"env":{"HTTPS_PROXY":"http://proxy.corp:8080"}}`)
	code, out := sc.doctor(t)
	if code != 0 || strings.Contains(out, "✗") || !strings.Contains(out, notRoutedLine) {
		t.Errorf("exit %d, want 0 with the not-routed line and no ✗ line for a corporate proxy:\n%s", code, out)
	}
}

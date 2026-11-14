package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// notRoutedLine is the dim line doctor prints in place of the routed check when
// Cortex has not routed Claude Code.
const notRoutedLine = "  · routed       Claude Code is not routed; agentop setup --claude-code routes it\n"

// stubsOnlyPath leaves PATH with installStub's dirs alone: no system dir, so
// nothing of this machine's, python3 included, is found.
func stubsOnlyPath(t *testing.T) {
	t.Helper()
	var keep []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.HasPrefix(dir, os.TempDir()) {
			keep = append(keep, dir)
		}
	}
	t.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator)))
}

// wantLines checks that out holds each of want, and that doctor exited code.
func wantLines(t *testing.T, code, wantCode int, out string, want ...string) {
	t.Helper()
	if code != wantCode {
		t.Errorf("exit %d, want %d:\n%s", code, wantCode, out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// Not routed is a dim line where the routed check would be, after started.
func TestDoctorSaysWhenClaudeCodeIsNotRouted(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "\n"+markLine("✓", "started")+supervisorName(runtimeGOOS())+" · healthy\n"+notRoutedLine)
}

func TestDoctorVersionsMustAgree(t *testing.T) {
	for _, c := range []struct{ name, cortex, reason, fix string }{
		{"another version", "#!/bin/sh\necho cortex v9.9.7\n", "agentop is v9.9.9, cortex is v9.9.7", installerOneLiner},
		{"no version", "#!/bin/sh\nexit 3\n", "cortex did not report its version", installerOneLiner},
		// A cpex install gets the plain one-liner, which moves it to the default build.
		{"cpex build", "#!/bin/sh\necho cortex \\(cpex\\) v9.9.7\n", "agentop is v9.9.9, cortex is v9.9.7", installerOneLiner},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			sc.installed(t)
			writeExe(t, filepath.Join(sc.home, ".local", "bin", "cortex"), c.cortex)
			code, out := sc.doctor(t)
			wantLines(t, code, 1, out, markLine("✗", "installed")+c.reason+"\n      fix: "+c.fix+"\n")
		})
	}
}

func TestDoctorNamesTheAgentopThatWinsOnPath(t *testing.T) {
	t.Run("another agentop first", func(t *testing.T) {
		sc := newDoctorScene(t)
		sc.installed(t)
		installStub(t, "agentop", "#!/bin/sh\necho agentop v1.2.3\n")
		other, _ := filepath.Abs(filepath.Dir(lookPathOrFatal(t, "agentop")))
		code, out := sc.doctor(t)
		wantLines(t, code, 0, out, markLine("!", "installed")+"the agentop on PATH is "+filepath.Join(other, "agentop")+
			", not ~/.local/bin/agentop\n      fix: put ~/.local/bin ahead of "+other+" on PATH, or remove that agentop\n")
	})
	t.Run("this agentop", func(t *testing.T) {
		sc := newDoctorScene(t)
		sc.installed(t)
		t.Setenv("PATH", filepath.Join(sc.home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		code, out := sc.doctor(t)
		wantLines(t, code, 0, out, "\n"+markLine("✓", "installed")+"agentop, cortex v9.9.9 · ~/.local/bin\n",
			"\n"+markLine("✓", "PATH")+"~/.local/bin\n")
	})
}

func lookPathOrFatal(t *testing.T, name string) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if p := filepath.Join(dir, name); isExecutable(p) {
			return p
		}
	}
	t.Fatalf("no %s on PATH", name)
	return ""
}

// A bridge that is off is a setting, not a failure: advice naming the key.
func TestDoctorAdvisesWhenTheBridgeIsOff(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	cfg := filepath.Join(sc.home, ".cortex", "config.yaml")
	b, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(b), "\n  mode: enabled\n") {
		t.Fatalf("the scene's config has no enabled bridge (%v):\n%s", err, b)
	}
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(b), "\n  mode: enabled\n", "\n  mode: disabled\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "\n"+markLine("!", "config")+"the TLS bridge is off, so Cortex reads no HTTPS traffic\n"+
		"      fix: set tls_bridge.mode: enabled, with a ca_dir, in ~/.cortex/config.yaml; then agentop service restart\n")
	if strings.Contains(out, " CA ") {
		t.Errorf("a CA line for a bridge that is off:\n%s", out)
	}
}

// Up is not ready: /readyz names the plugin still waiting on something.
func TestDoctorAdvisesWhenPluginsAreNotReady(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	sc.notReady.Store(true)
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "\n"+markLine("!", "started")+"plugins not ready — outbound plugin not ready: tokenexchange\n"+
		"      fix: see ~/.cortex/proxy.log for what it waits on\n")
}

// Routed values that name CA files no longer there trust nothing Claude Code can
// read; a restart writes them again.
func TestDoctorFailsRoutingWhoseCAFilesAreGone(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t, "--claude-code")
	if err := os.RemoveAll(filepath.Join(sc.home, ".cortex", "ca")); err != nil {
		t.Fatal(err)
	}
	code, out := sc.doctor(t)
	wantLines(t, code, 1, out, "\n"+markLine("✗", "routed")+"Claude Code's CA files are not there: ~/.cortex/ca/ca.crt, ~/.cortex/ca/bundle.crt\n"+
		"      fix: agentop setup --claude-code --restart\n")
}

func TestDoctorAdvisesWhenPython3IsMissing(t *testing.T) {
	const advice = "\n  ! python3      cortex-session-dump needs python3, which is not on PATH\n"
	for _, c := range []struct {
		name          string
		dump, python3 bool
		want          bool
	}{
		{"session dump, no python3", true, false, true},
		{"session dump and python3", true, true, false},
		{"no session dump", false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			sc.installed(t)
			if c.dump {
				writeExe(t, filepath.Join(sc.home, ".local", "bin", "cortex-session-dump"), "#!/usr/bin/env python3\n")
			}
			stubsOnlyPath(t)
			if c.python3 {
				installStub(t, "python3", "#!/bin/sh\nexit 0\n")
			}
			code, out := sc.doctor(t)
			if code != 0 || strings.Contains(out, advice) != c.want {
				t.Errorf("exit %d, want 0 and the python3 advice %v:\n%s", code, c.want, out)
			}
		})
	}
}

// A healthy --no-service install is checked as one, and its fixes keep it one:
// setup without --no-service would put a service in its place.
func TestDoctorChecksABackgroundProxyAsOne(t *testing.T) {
	sc := newDoctorScene(t)
	var pid int
	stopProcessOnCleanup(t, &pid)
	sc.installed(t, "--no-service")
	pid = readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid"))
	if pid == 0 {
		t.Fatal("setup --no-service left no proxy.pid")
	}
	// ps names the stub's sleep as cortex, as it names the real background proxy.
	installStub(t, "ps", "#!/bin/sh\ncase \"$*\" in\n  *comm=*) echo cortex ;;\n  *) exec /bin/ps \"$@\" ;;\nesac\n")
	code, out := sc.doctor(t)
	wantLines(t, code, 0, out, "\n"+markLine("✓", "started")+"in the background (no supervisor)\n",
		"Claude Code is not routed; agentop setup --claude-code --no-service routes it\n")
	if strings.Contains(out, "✗") {
		t.Errorf("a ✗ for a healthy background proxy:\n%s", out)
	}

	rc := filepath.Join(sc.home, ".zshrc")
	if err := os.WriteFile(rc, nil, 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	code, out = sc.doctor(t)
	if fix, _ := fixAfter(out, markLine("✗", "PATH")); code != 1 || fix != "agentop setup --no-service" {
		t.Errorf("exit %d and the PATH fix %q, want 1 and agentop setup --no-service:\n%s", code, fix, out)
	}
}

// Nothing removes proxy.pid as the background proxy goes, after a reboot say, so
// a --no-service install whose proxy is gone still has one: doctor checks it as
// that install, and its fixes keep it one. A unit beside a stale proxy.pid is a
// service install.
func TestDoctorChecksABackgroundInstallWhoseProxyIsGone(t *testing.T) {
	for _, c := range []struct {
		name    string
		pidfile func(pid int) string // proxy.pid once the proxy is gone
	}{
		{"its pid is gone", func(pid int) string { return strconv.Itoa(pid) + "\n" }},
		{"it names no pid", func(int) string { return "\n" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			var pid int
			stopProcessOnCleanup(t, &pid)
			sc.installed(t, "--no-service")
			pidFile := filepath.Join(sc.home, ".cortex", "proxy.pid")
			if pid = readPIDFile(pidFile); pid == 0 {
				t.Fatal("setup --no-service left no proxy.pid")
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
			for i := 0; i < 50 && alive(pid); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if alive(pid) {
				t.Fatalf("fixture: pid %d is still alive", pid)
			}
			writeHomeFile(t, pidFile, c.pidfile(pid))
			code, out := sc.doctor(t)
			if fix, _ := fixAfter(out, markLine("✗", "started")); code != 1 || fix != "agentop setup --no-service" {
				t.Errorf("exit %d and the started fix %q, want 1 and agentop setup --no-service:\n%s", code, fix, out)
			}
			wantLines(t, code, 1, out, "Claude Code is not routed; agentop setup --claude-code --no-service routes it\n")
		})
	}
	t.Run("a unit beside it", func(t *testing.T) {
		sc := newDoctorScene(t)
		sc.installed(t)
		writeHomeFile(t, filepath.Join(sc.home, ".cortex", "proxy.pid"), "2147483646\n")
		code, out := sc.doctor(t)
		wantLines(t, code, 0, out, "\n"+markLine("✓", "started")+supervisorName(runtimeGOOS())+" · healthy\n"+notRoutedLine)
	})
}

// A background proxy has no service to restart, and setup without --no-service
// would put one in its place: the CA's fixes keep it unsupervised.
func TestDoctorCAFixesKeepABackgroundProxy(t *testing.T) {
	plainOutput(t)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		ca   func(t *testing.T, dir string)
		want string
	}{
		{"expiring", func(t *testing.T, dir string) { writeCA(t, dir, now.Add(12*24*time.Hour)) },
			"  ! CA           ~/.cortex/ca/ca.crt expires " + now.Add(12*24*time.Hour).Local().Format(time.DateOnly) +
				"\n      fix: agentop setup --no-service --restart\n"},
		{"expired", func(t *testing.T, dir string) { writeCA(t, dir, now.Add(-2*24*time.Hour)) },
			"  ✗ CA           ~/.cortex/ca/ca.crt expired " + now.Add(-2*24*time.Hour).Local().Format(time.DateOnly) +
				"\n      fix: agentop setup --no-service --restart\n"},
		{"missing", func(*testing.T, string) {}, "  ✗ CA           no ca.crt in ~/.cortex/ca\n      fix: agentop setup --no-service --restart\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newTestSetupEnv(t)
			env.opts.noService = true
			dir := filepath.Join(env.cortexDir, "ca")
			writeExe(t, env.configPath(), "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:47600\n"+
				"tls_bridge:\n  mode: enabled\n  ca_dir: "+dir+"\n")
			c.ca(t, dir)
			var b bytes.Buffer
			checkCA(env, checklist.New(&b, false), now)
			if !strings.HasPrefix(b.String(), c.want) {
				t.Errorf("got\n%q\nwant it to start\n%q", b.String(), c.want)
			}
		})
	}
}

// A background proxy is done once its pid is alive, which the plan asks; doctor
// asks /healthz too, and one that stops answering is a ✗ in place of the ✓.
func TestDoctorFailsAProxyThatStopsAnswering(t *testing.T) {
	sc := newDoctorScene(t)
	var pid int
	stopProcessOnCleanup(t, &pid)
	sc.installed(t, "--no-service")
	pid = readPIDFile(filepath.Join(sc.home, ".cortex", "proxy.pid"))
	if pid == 0 {
		t.Fatal("setup --no-service left no proxy.pid")
	}
	installStub(t, "ps", "#!/bin/sh\ncase \"$*\" in\n  *comm=*) echo cortex ;;\n  *) exec /bin/ps \"$@\" ;;\nesac\n")
	sc.down.Store(true)
	health := resolveHealthURL(filepath.Join(sc.home, ".cortex", "config.yaml"))
	code, out := sc.doctor(t)
	wantLines(t, code, 1, out, "\n"+markLine("✗", "started")+"not answering "+health+"\n      fix: agentop setup --no-service --restart\n")
	if strings.Contains(out, markLine("✓", "started")) || strings.Contains(out, markLine("!", "started")) {
		t.Errorf("a second started line beside the ✗:\n%s", out)
	}
}

// routeProject routes Claude Code through Cortex in a project's settings file, as
// configure claude-code enable --settings does, so enable's record names that
// file. It returns the file.
func routeProject(t *testing.T, sc doctorScene) string {
	t.Helper()
	proj := filepath.Join(sc.home, "proj", ".claude", "settings.json")
	writeHomeFile(t, proj, "{}\n")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(proj, filepath.Join(sc.home, ".cortex", "config.yaml"), filepath.Join(sc.home, stateRel), true, &out, &errb); code != 0 {
		t.Fatalf("fixture: enable --settings exit %d: %s%s", code, out.String(), errb.String())
	}
	if st, err := readState(filepath.Join(sc.home, stateRel)); err != nil || st == nil || st.Settings != proj {
		t.Fatalf("fixture: the record is %+v (%v), want one naming %s", st, err, proj)
	}
	return proj
}

// Enable's record can name a project's settings file, which setup's step never
// writes. Doctor checks that file, not ~/.claude/settings.json: routed, so nothing
// to fix, rather than a ✗ whose fix routes Claude Code globally or tells the user
// to remove their own proxy. The global file is left as it was.
func TestDoctorChecksTheSettingsFileTheRecordNames(t *testing.T) {
	for _, c := range []struct{ name, global string }{
		{"no global settings", ""},
		{"a corporate proxy in the global settings", `{"env":{"HTTPS_PROXY":"http://corp.example.com:8080","NODE_EXTRA_CA_CERTS":"/etc/corp-ca.pem"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			sc.installed(t)
			global := filepath.Join(sc.home, settingsRel)
			if c.global != "" {
				writeHomeFile(t, global, c.global)
			}
			routeProject(t, sc)
			before := homeFiles(t, sc.home)
			code, out := sc.doctor(t)
			wantLines(t, code, 0, out, "\n"+markLine("✓", "routed")+"Claude Code → Cortex · ~/proj/.claude/settings.json\n", "\n  Nothing to fix.\n")
			if strings.Contains(out, "✗") || strings.Contains(out, "is not routed;") {
				t.Errorf("doctor checked the global settings, not the project's the record names:\n%s", out)
			}
			sameFiles(t, before, homeFiles(t, sc.home))
			if c.global == "" && fileExists(global) {
				t.Errorf("doctor wrote %s", global)
			} else if c.global != "" && readFile(t, global) != c.global {
				t.Errorf("the global settings are %q, want %q as they were", readFile(t, global), c.global)
			}
		})
	}
}

// The project file the record names no longer routing is a ✗ whose fix routes
// that file again; its CA files gone, a ✗ whose fix restarts Cortex, which
// writes them, without --claude-code, which would route the global file.
func TestDoctorFailsTheRecordedSettingsFileWhenItNoLongerRoutes(t *testing.T) {
	for _, c := range []struct {
		name, reason, fix string
		breakIt           func(t *testing.T, sc doctorScene, proj string)
	}{
		{"its proxy removed", "Claude Code is not routed through Cortex in ~/proj/.claude/settings.json",
			"agentop configure claude-code enable --settings ~/proj/.claude/settings.json",
			func(t *testing.T, _ doctorScene, proj string) { writeHomeFile(t, proj, "{}\n") }},
		{"its CA files gone", "Claude Code's CA files are not there: ~/.cortex/ca/ca.crt, ~/.cortex/ca/bundle.crt",
			"agentop setup --restart",
			func(t *testing.T, sc doctorScene, _ string) {
				if err := os.RemoveAll(filepath.Join(sc.home, ".cortex", "ca")); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			sc.installed(t)
			proj := routeProject(t, sc)
			c.breakIt(t, sc, proj)
			code, out := sc.doctor(t)
			wantLines(t, code, 1, out, "\n"+markLine("✗", "routed")+c.reason+"\n      fix: "+c.fix+"\n")
			everyFailHasAFix(t, out)
		})
	}
}

// Off PATH with no marker, as setup --no-modify-path leaves it, the ✗ PATH line
// has two fixes: setup, which adds the lines, and the line to add by hand, for
// whoever chose to keep setup out of their dotfiles.
func TestDoctorPATHFixAlsoNamesTheLineToAdd(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t, "--no-modify-path")
	code, out := sc.doctor(t)
	bin := filepath.Join(sc.home, ".local", "bin")
	want := []string{"agentop setup", `or add it yourself, in ~/.zshrc:  export PATH="` + bin + `:$PATH"`}
	if fixes := fixesUnder(out, markLine("✗", "PATH")); code != 1 || !slices.Equal(fixes, want) {
		t.Errorf("exit %d, ✗ PATH fixes %q; want 1 and %q:\n%s", code, fixes, want, out)
	}
	everyFailHasAFix(t, out)
}

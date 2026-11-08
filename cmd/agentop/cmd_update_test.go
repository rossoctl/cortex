package main

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeInstaller puts a curl on PATH that serves an installer which prints its
// arguments and exits 7, or fails as curl does when fails is set.
func fakeInstaller(t *testing.T, fails bool) {
	t.Helper()
	body := "#!/bin/sh\n[ \"$2\" = '" + installerURL + "' ] || exit 9\nprintf '%s\\n' 'echo \"installer: $*\"' 'exit 7'\n"
	if fails {
		body = "#!/bin/sh\necho 'curl: (6) Could not resolve host' >&2\nexit 6\n"
	}
	installStub(t, "curl", body)
}

// latestIs serves latestReleaseURL as GitHub does: a redirect to the tag's page,
// or a 404 when tag is "".
func latestIs(t *testing.T, tag string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if tag == "" {
			http.NotFound(w, nil)
			return
		}
		w.Header().Set("Location", "https://github.com/rossoctl/cortex/releases/tag/"+tag)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	saved := latestReleaseURL
	latestReleaseURL = srv.URL
	t.Cleanup(func() { latestReleaseURL = saved })
}

// The scene installs v9.9.9 of both binaries.
func TestUpdateSkipsTheInstallerWhenCurrent(t *testing.T) {
	for _, c := range []struct {
		name, latest, cortex string
		args                 []string
		want                 string // stdout
		code                 int
	}{
		{name: "newest", latest: "v9.9.9", want: "Already at v9.9.9.\n"},
		{name: "pinned", latest: "v9.9.8", args: []string{"--ref=v9.9.9"}, want: "Already at v9.9.9.\n"},
		{name: "older", latest: "v9.9.10", want: "installer: \n", code: 7},
		{name: "pinned to another", latest: "v9.9.9", args: []string{"--ref=v9.9.8"}, want: "installer: --ref=v9.9.8\n", code: 7},
		{name: "main", latest: "v9.9.9", args: []string{"--ref=main"}, want: "installer: --ref=main\n", code: 7},
		{name: "newest unknown", want: "installer: \n", code: 7},
		// setup repairs a pair that disagree.
		{name: "cortex behind", latest: "v9.9.9", cortex: "#!/bin/sh\necho cortex v9.9.8\n", want: "installer: \n", code: 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			sc.installed(t)
			if c.cortex != "" {
				writeExe(t, filepath.Join(sc.home, ".local", "bin", "cortex"), c.cortex)
			}
			latestIs(t, c.latest)
			fakeInstaller(t, false)
			var out, errb bytes.Buffer
			if code := runUpdate(c.args, &out, &errb); code != c.code || out.String() != c.want {
				t.Errorf("exit %d stdout %q, want %d %q\n%s", code, out.String(), c.code, c.want, errb.String())
			}
		})
	}
}

func TestUpdateRunsTheInstallerAndKeepsTheInstall(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t, "--claude-code")
	writeExe(t, filepath.Join(sc.home, ".local", "bin", "cortex"), "#!/bin/sh\necho cortex \\(cpex\\) v9.9.9\n")
	fakeInstaller(t, false)
	var out, errb bytes.Buffer
	code := runUpdate([]string{"--ref=v9.9.9-rc.1", "-y"}, &out, &errb)
	// The installer's exit status is update's.
	if code != 7 {
		t.Fatalf("exit %d, want 7:\n%s%s", code, out.String(), errb.String())
	}
	if want := "installer: --claude-code --cpex --ref=v9.9.9-rc.1 --yes\n"; out.String() != want {
		t.Errorf("stdout %q, want %q", out.String(), want)
	}
}

func TestUpdateKeepsABackgroundProxy(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t, "--no-service")
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(updateFlags(env, "", false), " "); got != "--no-service" {
		t.Errorf("flags %q, want --no-service", got)
	}
}

// A routing to a project's settings file is not one setup's --claude-code makes.
func TestUpdateLeavesAProjectRoutingAlone(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	proj := filepath.Join(t.TempDir(), ".claude", "settings.json")
	var out, errb bytes.Buffer
	if code := claudeCodeEnable2(proj, filepath.Join(sc.home, ".cortex", "config.yaml"), filepath.Join(sc.home, stateRel), true, &out, &errb); code != 0 {
		t.Fatalf("enable exit %d:\n%s%s", code, out.String(), errb.String())
	}
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := updateFlags(env, "", false); len(got) != 0 {
		t.Errorf("flags %q, want none", got)
	}
}

// A failed download must not run an empty script, which sh would answer with 0.
func TestUpdateFailsWhenTheDownloadFails(t *testing.T) {
	newDoctorScene(t).installed(t)
	latestIs(t, "v9.9.10")
	fakeInstaller(t, true)
	var out, errb bytes.Buffer
	if code := runUpdate(nil, &out, &errb); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "Could not resolve host") {
		t.Errorf("stderr lacks curl's error: %q", errb.String())
	}
}

func TestUpdateRejectsAnArgument(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runUpdate([]string{"now"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `unexpected argument "now"`) {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

// Ctrl-C reaches both shells of updateScript. The installer catches it and rolls
// back, and update must exit with its status. dash, sh on Debian, dies of an INT it
// does not catch, so this runs the script under dash where dash is installed.
func TestUpdateScriptExitsWithTheInstallersStatusOnInterrupt(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("no dash")
	}
	if signal.Ignored(os.Interrupt) {
		// A shell cannot trap a signal it started with ignored, as under a background job.
		t.Skip("SIGINT is ignored in this process")
	}
	installStub(t, "sh", "#!"+dash+"\nexec "+dash+" \"$@\"\n")
	installStub(t, "curl", "#!/bin/sh\nprintf '%s\\n' \"trap 'exit 3' INT\" 'echo ready' 'sleep 5 >/dev/null & wait'\n")
	cmd := exec.Command("sh", "-c", updateScript, installerURL)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		t.Fatalf("installer said %q", line)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	_ = cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 3 {
		t.Errorf("exit %d, want the installer's 3", code)
	}
}

// An install-only install has binaries and PATH but no config. A full setup would
// write one and start a proxy.
func TestUpdateKeepsAnInstallOnlyInstall(t *testing.T) {
	newDoctorScene(t).installed(t, "--install-only")
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(updateFlags(env, "", false), " "); got != "--install-only" {
		t.Errorf("flags %q, want --install-only", got)
	}
}

// A default install puts the bin dir on PATH or its marker in the profile. With
// neither, the install did not edit the profile, and update must not either.
func TestUpdateKeepsTheProfileAsItIs(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t, "--no-modify-path")
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(updateFlags(env, "", false), " "); got != "--no-modify-path" {
		t.Errorf("flags %q, want --no-modify-path", got)
	}
	// On PATH, there is nothing for setup to edit.
	t.Setenv("PATH", filepath.Join(sc.home, ".local", "bin")+string(filepath.ListSeparator)+os.Getenv("PATH"))
	if env, err = newSetupEnv(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := updateFlags(env, "", false); len(got) != 0 {
		t.Errorf("flags %q on PATH, want none", got)
	}
}

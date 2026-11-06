package main

import (
	"bytes"
	"path/filepath"
	"strings"
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

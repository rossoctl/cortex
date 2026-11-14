package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
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
		release              string // the tag fakeStagedRelease serves, if any
		args                 []string
		want                 string // stdout
		code                 int
	}{
		{name: "newest", latest: "v9.9.9", want: "Already at v9.9.9.\n"},
		{name: "pinned", latest: "v9.9.8", args: []string{"--ref=v9.9.9"}, want: "Already at v9.9.9.\n"},
		{name: "older", latest: "v9.9.10", release: "v9.9.10", want: "staged setup\n", code: 7},
		{name: "pinned to another", latest: "v9.9.9", release: "v9.9.8", args: []string{"--ref=v9.9.8"}, want: "staged setup\n", code: 7},
		{name: "main", latest: "v9.9.9", args: []string{"--ref=main"}, want: "installer: --ref=main\n", code: 7},
		// setup repairs a pair that disagree.
		{name: "cortex behind", latest: "v9.9.9", release: "v9.9.9", cortex: "#!/bin/sh\necho cortex v9.9.8\n", want: "staged setup\n", code: 7},
		// A cpex cortex at the newest tag still gets the default build.
		{name: "cpex at the tag", latest: "v9.9.9", release: "v9.9.9", cortex: "#!/bin/sh\necho cortex \\(cpex\\) v9.9.9\n", want: cpexUpdateNotice + "\nstaged setup\n", code: 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := newDoctorScene(t)
			sc.installed(t)
			if c.cortex != "" {
				writeExe(t, filepath.Join(sc.home, ".local", "bin", "cortex"), c.cortex)
			}
			latestIs(t, c.latest)
			fakeInstaller(t, false)
			if c.release != "" {
				fakeStagedRelease(t, c.release, false)
			}
			var out, errb bytes.Buffer
			if code := runUpdate(c.args, &out, &errb); code != c.code || out.String() != c.want {
				t.Errorf("exit %d stdout %q, want %d %q\n%s", code, out.String(), c.code, c.want, errb.String())
			}
		})
	}
}

// fakeSetup is the staged agentop: it writes its arguments and whether it saw
// CORTEX_INSTALLER_DREW to $FAKE_SETUP_ARGS, then exits 7.
const fakeSetup = `#!/bin/sh
printf '%s\n' "$@" "drew=${CORTEX_INSTALLER_DREW-unset}" > "$FAKE_SETUP_ARGS"
echo staged setup
exit 7
`

// fakeStagedRelease serves the tag's release for this machine with right checksums,
// sets TMPDIR to a new empty dir and records provenance checks instead of making
// them. setupArgs is the file the staged agentop writes its arguments to.
func fakeStagedRelease(t *testing.T, tag string, praxis bool) (setupArgs string, checked *[]string, files map[string][]byte) {
	t.Helper()
	files = map[string][]byte{}
	for _, name := range releaseTarballs(tag, runtime.GOOS, runtime.GOARCH, praxis) {
		body := "#!/bin/sh\necho " + tarballBinary(name) + "\n"
		if tarballBinary(name) == "agentop" {
			body = fakeSetup
		}
		files[name] = tarGz(t, tarEntry{name: tarballBinary(name), typ: tar.TypeReg, body: body})
	}
	releaseServer(t, tag, withSums(files))
	setupArgs = filepath.Join(t.TempDir(), "setup-args")
	emptyTmp(t)
	t.Setenv("FAKE_SETUP_ARGS", setupArgs)
	checked = new([]string)
	saved := provenanceCheckFn
	provenanceCheckFn = func(_ context.Context, name string, _ []byte, gotTag string) error {
		if gotTag != tag {
			t.Errorf("provenance checked for %s, want %s", gotTag, tag)
		}
		*checked = append(*checked, name)
		return nil
	}
	t.Cleanup(func() { provenanceCheckFn = saved })
	return setupArgs, checked, files
}

// hashDir is the SHA-256 of each file in dir, by name.
func hashDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sums := map[string]string{}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sums[e.Name()] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	return sums
}

func TestUpdateHandsOffToTheStagedSetup(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t, "--claude-code", "--no-service")
	writeExe(t, filepath.Join(sc.home, ".local", "bin", "cortex"), "#!/bin/sh\necho cortex \\(cpex\\) v9.9.9\n")
	t.Setenv("CORTEX_INSTALLER_DREW", "1")
	setupArgs, checked, files := fakeStagedRelease(t, "v9.9.9-rc.1", false)
	tmp := os.Getenv("TMPDIR")
	var out, errb bytes.Buffer
	code := runUpdate([]string{"--ref=v9.9.9-rc.1", "-y"}, &out, &errb)
	// The staged setup's exit status is update's.
	if code != 7 || out.String() != cpexUpdateNotice+"\nstaged setup\n" {
		t.Fatalf("exit %d stdout %q, want 7 and the notice then the staged setup's line:\n%s", code, out.String(), errb.String())
	}
	names := releaseTarballs("v9.9.9-rc.1", runtime.GOOS, runtime.GOARCH, false)
	if !strings.HasPrefix(names[1], "cortex_") || !slices.Equal(*checked, names) {
		t.Errorf("checked %q, want %q", *checked, names)
	}
	var size int
	for _, n := range names {
		size += len(files[n])
	}
	b, err := os.ReadFile(setupArgs)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(got) != 9 {
		t.Fatalf("setup args %q, want 9 lines", got)
	}
	want := []string{"setup", "--from", got[2], "--claude-code", "--no-service", "--yes", fmt.Sprintf("--handoff-bytes=%d", size), got[7], "drew=unset"}
	if !slices.Equal(got, want) {
		t.Errorf("setup args %q, want %q", got, want)
	}
	if !strings.HasPrefix(got[2], filepath.Join(tmp, "cortex-stage.")) {
		t.Errorf("--from %q, want a stage under %s", got[2], tmp)
	}
	if !regexp.MustCompile(`^--handoff-seconds=[0-9]+$`).MatchString(got[7]) {
		t.Errorf("arg %q, want --handoff-seconds=<digits>", got[7])
	}
	assertEmptyDir(t, tmp)
}

func TestInstallReleasePraxis(t *testing.T) {
	setupArgs, checked, _ := fakeStagedRelease(t, "v9.9.9", true)
	var out, errb bytes.Buffer
	err := installRelease(&setupEnv{}, "v9.9.9", []string{"--praxis"}, &out, &errb)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("err %v, want exit 7:\n%s", err, errb.String())
	}
	if !slices.Contains(*checked, "cortex-gateway_v9.9.9_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz") {
		t.Errorf("checked %q, want the cortex-gateway tarball", *checked)
	}
	b, err := os.ReadFile(setupArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(string(b), "\n"), "--praxis") {
		t.Errorf("setup args %q lack --praxis", b)
	}
}

func TestUpdateChecksumMismatchChangesNothing(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	_, checked, files := fakeStagedRelease(t, "v9.9.10", false)
	agentop := releaseTarballs("v9.9.10", runtime.GOOS, runtime.GOARCH, false)[0]
	wrong := maps.Clone(files)
	wrong[agentop] = []byte("not the tarball")
	delete(wrong, "checksums.txt")
	files["checksums.txt"] = sumsFor(wrong)
	releaseServer(t, "v9.9.10", files)
	binDir := filepath.Join(sc.home, ".local", "bin")
	before := hashDir(t, binDir)
	var out, errb bytes.Buffer
	if code := runUpdate([]string{"--ref=v9.9.10", "-y"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "checksum mismatch for agentop_v9.9.10") {
		t.Errorf("exit %d stderr %q, want 1 and a checksum mismatch", code, errb.String())
	}
	if after := hashDir(t, binDir); !maps.Equal(before, after) {
		t.Errorf("bin dir changed: %v, was %v", after, before)
	}
	if len(*checked) != 0 {
		t.Errorf("checked %q before the checksums passed", *checked)
	}
	assertEmptyDir(t, os.Getenv("TMPDIR"))
}

func TestUpdateUnreachableAttestationAPIFailsClosed(t *testing.T) {
	sc := newDoctorScene(t)
	sc.installed(t)
	setupArgs, _, _ := fakeStagedRelease(t, "v9.9.10", false)
	provenanceCheckFn = checkProvenance // fakeStagedRelease's cleanup restores it
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	savedAPI, savedTM := attestationsAPI, provenanceTrustedMaterial
	attestationsAPI = srv.URL + "/"
	provenanceTrustedMaterial = func() (root.TrustedMaterial, error) {
		t.Error("trusted root loaded before the attestation API answered")
		return nil, errors.New("not here")
	}
	t.Cleanup(func() { attestationsAPI, provenanceTrustedMaterial = savedAPI, savedTM })
	binDir := filepath.Join(sc.home, ".local", "bin")
	before := hashDir(t, binDir)
	env, err := newSetupEnv(setupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if err := installRelease(env, "v9.9.10", nil, &out, &errb); err == nil || !strings.Contains(err.Error(), "attestation API") {
		t.Errorf("err %v, want the attestation API named", err)
	}
	if _, err := os.Stat(setupArgs); !os.IsNotExist(err) {
		t.Errorf("staged setup ran (stat %v)", err)
	}
	if after := hashDir(t, binDir); !maps.Equal(before, after) {
		t.Errorf("bin dir changed: %v, was %v", after, before)
	}
	assertEmptyDir(t, os.Getenv("TMPDIR"))
}

func TestInstallReleaseMainUsesTheWebsiteInstaller(t *testing.T) {
	fakeInstaller(t, false)
	var out, errb bytes.Buffer
	err := installRelease(&setupEnv{}, "main", []string{"--ref=main"}, &out, &errb)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || out.String() != "installer: --ref=main\n" {
		t.Errorf("err %v stdout %q, want exit 7 and the installer's line\n%s", err, out.String(), errb.String())
	}
}

// The newest release is a v* tag, so not reading it must not fall back to the
// unchecked installer (D1).
func TestUpdateFailsClosedWithoutTheNewestRelease(t *testing.T) {
	newDoctorScene(t).installed(t)
	latestIs(t, "")
	fakeInstaller(t, false)
	var out, errb bytes.Buffer
	if code := runUpdate(nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "could not read the newest release") {
		t.Errorf("exit %d stdout %q stderr %q, want 1 and the newest release named", code, out.String(), errb.String())
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
	if code := runUpdate([]string{"--ref=main"}, &out, &errb); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "Could not resolve host") {
		t.Errorf("stderr lacks curl's error: %q", errb.String())
	}
	t.Run("release", func(t *testing.T) {
		releaseServer(t, "v9.9.10", map[string][]byte{})
		var out, errb bytes.Buffer
		if code := runUpdate([]string{"--ref=v9.9.10"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "download failed: checksums.txt") {
			t.Errorf("exit %d stderr %q, want 1 and the failed download named", code, errb.String())
		}
	})
}

func TestUpdateRejectsAnArgument(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runUpdate([]string{"now"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `unexpected argument "now"`) {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}

// Read at package init: a later signal.Notify clears signal.Ignored.
var sigintIgnoredAtStart = signal.Ignored(os.Interrupt)

// Ctrl-C reaches both shells of updateScript. The installer catches it and rolls
// back, and update must exit with its status. dash, sh on Debian, dies of an INT it
// does not catch, so this runs the script under dash where dash is installed.
func TestUpdateScriptExitsWithTheInstallersStatusOnInterrupt(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("no dash")
	}
	if sigintIgnoredAtStart {
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

// praxisUpdateFlags writes body as the config of a new install and returns the
// update flags for it.
func praxisUpdateFlags(t *testing.T, body string) []string {
	t.Helper()
	home := t.TempDir()
	env := &setupEnv{home: home, goos: runtime.GOOS, binDir: filepath.Join(home, ".local", "bin"), cortexDir: filepath.Join(home, ".cortex")}
	env.pathEnv = env.binDir
	if err := os.MkdirAll(env.cortexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.configPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return updateFlags(env, "", false)
}

func TestUpdateFlags_PraxisConfigPassesPraxis(t *testing.T) {
	if got := praxisUpdateFlags(t, "praxis:\n  enabled: true\n"); !slices.Contains(got, "--praxis") {
		t.Errorf("flags %q, want --praxis", got)
	}
}

func TestUpdateFlags_DirectConfigNoPraxis(t *testing.T) {
	if got := praxisUpdateFlags(t, "praxis:\n  enabled: false\n"); slices.Contains(got, "--praxis") {
		t.Errorf("flags %q, want no --praxis", got)
	}
}

// A config that does not load says nothing about the mode, so update passes nothing.
func TestUpdateFlags_PraxisConfigBrokenPassesNothing(t *testing.T) {
	if got := praxisUpdateFlags(t, "praxis: [\n  enabled: true\n"); slices.Contains(got, "--praxis") {
		t.Errorf("flags %q, want no --praxis", got)
	}
}

// A noexec temp dir fails the hand-off with EACCES at start; installRelease takes
// that as the sign to restage, and the copy under ~/.cortex/tmp must still run.
func TestRestageUnderHomeAfterExecPermissionError(t *testing.T) {
	stage := t.TempDir()
	bin := filepath.Join(stage, "agentop")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(bin).Run(); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("exec of a file that cannot run: got %v, want fs.ErrPermission", err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", "-c", "exit 126").Run(); err == nil || errors.Is(err, fs.ErrPermission) {
		t.Fatalf("an exit 126 from setup itself must not read as a permission error: %v", err)
	}
	home := t.TempDir()
	dir, err := restageUnderHome(stage, home)
	if err != nil {
		t.Fatal(err)
	}
	if !isStagingDir(dir, home) {
		t.Fatalf("restaged dir %s is not a staging dir setup may delete", dir)
	}
	if err := exec.Command(filepath.Join(dir, "agentop")).Run(); err != nil {
		t.Fatalf("restaged agentop does not run: %v", err)
	}
	if _, err := restageUnderHome(stage, "relative"); err == nil {
		t.Fatal("a relative HOME must not stage under the working directory")
	}
}

// runStagedFixture makes a stage whose agentop has mode, and a build that runs
// it with a log file. A copy keeps mode 0644, so build makes any stage but the
// first one runnable: that is the noexec temp dir, without a noexec mount.
func runStagedFixture(t *testing.T, script string, mode os.FileMode) (stage, log string, build func(string) *exec.Cmd) {
	t.Helper()
	stage = t.TempDir()
	log = filepath.Join(t.TempDir(), "runs")
	if err := os.WriteFile(filepath.Join(stage, "agentop"), []byte(script), mode); err != nil {
		t.Fatal(err)
	}
	first := stage
	build = func(s string) *exec.Cmd {
		if s != first {
			if err := os.Chmod(filepath.Join(s, "agentop"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return exec.Command(filepath.Join(s, "agentop"), log)
	}
	return stage, log, build
}

func TestRunStagedRestagesAfterExecPermissionError(t *testing.T) {
	stage, log, build := runStagedFixture(t, "#!/bin/sh\necho \"$0\" >> \"$1\"\n", 0o644)
	home := t.TempDir()
	var stderr bytes.Buffer
	left, err := runStaged(stage, home, build, &stderr)
	if err != nil {
		t.Fatalf("runStaged: %v", err)
	}
	if left == stage || !isStagingDir(left, home) {
		t.Fatalf("stage left = %s, want a new staging dir under %s", left, home)
	}
	if _, err := os.Stat(stage); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("first stage %s is not removed: %v", stage, err)
	}
	if _, err := os.Stat(filepath.Join(left, "agentop")); err != nil {
		t.Fatalf("new stage must stay for the caller to remove: %v", err)
	}
	if got, _ := os.ReadFile(log); string(got) != filepath.Join(left, "agentop")+"\n" {
		t.Fatalf("runs = %q, want one run of the copied agentop", got)
	}
	if !strings.Contains(stderr.String(), "staged under "+filepath.Join(home, ".cortex", "tmp")) {
		t.Fatalf("stderr = %q, want the restage line", stderr.String())
	}
}

func TestRunStagedDoesNotRetryWhenSetupFails(t *testing.T) {
	stage, log, build := runStagedFixture(t, "#!/bin/sh\necho run >> \"$1\"\nexit 126\n", 0o755)
	home := t.TempDir()
	left, err := runStaged(stage, home, build, io.Discard)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || left != stage {
		t.Fatalf("runStaged = (%s, %v), want (%s, an ExitError)", left, err, stage)
	}
	if got, _ := os.ReadFile(log); string(got) != "run\n" {
		t.Fatalf("runs = %q, want exactly one run", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".cortex")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a setup failure must not restage: %v", err)
	}
}

func TestRunStagedKeepsFirstStageWhenRestageFails(t *testing.T) {
	stage, _, build := runStagedFixture(t, "#!/bin/sh\n", 0o644)
	left, err := runStaged(stage, "relative", build, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "could not stage under ~/.cortex/tmp") || left != stage {
		t.Fatalf("runStaged = (%s, %v), want (%s, a restage error)", left, err, stage)
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatalf("first stage must stay for the caller to remove: %v", err)
	}
}

// A tag goes into a stage file name, a URL path and the SAN, so only a release tag reaches them.
func TestInstallReleaseRefusesANonReleaseTag(t *testing.T) {
	newDoctorScene(t).installed(t)
	fakeInstaller(t, false)
	for _, latest := range []string{"main-latest", "v1/../../x"} {
		latestIs(t, latest)
		var errb bytes.Buffer
		if code := runUpdate(nil, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "could not read the newest release") {
			t.Errorf("newest %q: exit %d stderr %q, want 1 and the newest release named", latest, code, errb.String())
		}
	}
	for _, ref := range []string{"v1/../../x", "v1.2", "v1.2.3/x"} {
		var errb bytes.Buffer
		if code := runUpdate([]string{"--ref=" + ref}, io.Discard, &errb); code != 1 || !strings.Contains(errb.String(), "is not a release tag") {
			t.Errorf("--ref=%s: exit %d stderr %q, want 1 and a refused tag", ref, code, errb.String())
		}
	}
}

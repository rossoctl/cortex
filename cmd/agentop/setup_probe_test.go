package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSetupSupervisorUsable(t *testing.T) {
	t.Run("a reachable supervisor", func(t *testing.T) {
		fakeSupervisor(t)
		if !setupSupervisorUsable(runtime.GOOS) {
			t.Error("reported unusable")
		}
	})
	t.Run("a sandbox", func(t *testing.T) {
		fakeNoSupervisor(t)
		if setupSupervisorUsable(runtime.GOOS) {
			t.Error("reported usable")
		}
	})
}

// TestSetupSupervisorUsableAsks pins the probe itself: `launchctl list`, because
// `launchctl print gui/<uid>` answers inside a sandbox where bootstrap fails.
func TestSetupSupervisorUsableAsks(t *testing.T) {
	for _, tc := range []struct{ goos, tool, want string }{
		{"darwin", "launchctl", "list"},
		{"linux", "systemctl", "--user show-environment"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			calls, appendLine := callLog(t)
			installStub(t, tc.tool, "#!/bin/sh\n"+appendLine+"\nexit 0\n")
			if !setupSupervisorUsable(tc.goos) {
				t.Errorf("a %s that answers read unusable", tc.tool)
			}
			if got := readCallLog(t, calls); len(got) != 1 || got[0] != tc.want {
				t.Errorf("ran %s %q, want exactly [%q]", tc.tool, got, tc.want)
			}
		})
	}
}

func TestPortInUseDialsLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if !portInUse(port) {
		t.Error("a listening port read free")
	}
	_ = ln.Close()
	if portInUse(port) {
		t.Error("a closed port read busy")
	}
}

// fakeProc points procRoot at a temp tree holding /proc/<pid>/exe → exe.
func fakeProc(t *testing.T, pid int, exe string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	saved := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = saved })
}

func TestPortHolderByLsof(t *testing.T) {
	fakeProc(t, 4242, "")
	calls, appendLine := callLog(t)
	installStub(t, "lsof", "#!/bin/sh\n"+appendLine+"\n"+`case "$1" in
  -nP) case "$2" in -iTCP@127.0.0.1:47600) printf 'p4242\n' ;; esac ;;
  -p) printf 'p4242\nftxt\nn/opt/x/cortex\n' ;;
esac
exit 0
`)
	pid, exe, ok := portHolder("47600")
	if !ok || pid != 4242 || exe != "/opt/x/cortex" {
		t.Errorf("holder = %d %q %v, want 4242 /opt/x/cortex", pid, exe, ok)
	}
	// The stub answers with or without -a, so only the arguments show it. Without -a,
	// lsof ORs -p and -d: every process's executable, and the first may not be ours.
	const wantTxt = "-p 4242 -a -d txt -Fn"
	var txt []string
	for _, l := range readCallLog(t, calls) {
		if strings.HasPrefix(l, "-p ") {
			txt = append(txt, l)
		}
	}
	if len(txt) != 1 || txt[0] != wantTxt {
		t.Errorf("lsof's executable lookup ran as %q, want [%q]", txt, wantTxt)
	}
	// lsof names no pid here, so portHolder asks ss next. A stub that finds nothing
	// keeps this off the real ss, which on a Linux box running Cortex would answer.
	installStub(t, "ss", "#!/bin/sh\nexit 0\n")
	if _, _, ok := portHolder("47601"); ok {
		t.Error("a port nobody holds reported a holder")
	}
}

// A sandbox can blind lsof without removing it. install.sh then asks ss, and so
// must portHolder.
func TestPortHolderBySsWhenLsofFindsNothing(t *testing.T) {
	fakeProc(t, 77, "/srv/cortex")
	installStub(t, "lsof", "#!/bin/sh\nexit 1\n")
	installStub(t, "ss", "#!/bin/sh\n"+
		`echo 'LISTEN 0 4096 127.0.0.1:47600 0.0.0.0:* users:(("cortex",pid=77,fd=3))'`+"\n")
	pid, exe, ok := portHolder("47600")
	if !ok || pid != 77 || exe != "/srv/cortex" {
		t.Errorf("holder = %d %q %v, want 77 /srv/cortex from ss", pid, exe, ok)
	}
}

// TestReadPIDFileDigitsOnly pins install.sh's rule: after the trailing newlines its
// $(cat) strips, the pidfile must be digits only.
func TestReadPIDFileDigitsOnly(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{"123\n", 123},
		{"123", 123},
		{"123\n\n", 123},
		{"+9", 0},
		{" 9", 0},
		{"9 \n", 0},
		{"9 9", 0},
		{"-5", 0},
		{"", 0},
		{"\n", 0},
		{"x", 0},
	} {
		path := filepath.Join(t.TempDir(), "proxy.pid")
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readPIDFile(path); got != tc.want {
			t.Errorf("readPIDFile(%q) = %d, want %d", tc.body, got, tc.want)
		}
	}
	if got := readPIDFile(filepath.Join(t.TempDir(), "absent")); got != 0 {
		t.Errorf("a missing pidfile read as %d", got)
	}
}

// TestSsListenerPIDLoopbackOrWildcardOnly: a listener on an external address only
// does not hold the loopback port. Naming it as the holder would read as a foreign
// proxy and refuse the install.
func TestSsListenerPIDLoopbackOrWildcardOnly(t *testing.T) {
	line := func(addr string, pid int) string {
		return "LISTEN 0 4096 " + addr + ` 0.0.0.0:* users:(("cortex",pid=` + strconv.Itoa(pid) + ",fd=3))"
	}
	for _, tc := range []struct {
		name, out string
		want      int
	}{
		{"an external address", line("192.168.1.5:47600", 71), 0},
		{"IPv6 loopback", line("[::1]:47600", 72), 72},
		{"an external address, then IPv4 loopback",
			line("192.168.1.5:47600", 71) + "\n" + line("127.0.0.1:47600", 73), 73},
	} {
		if got := ssListenerPID(tc.out, "47600"); got != tc.want {
			t.Errorf("%s: pid = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPortHolderBySsWhenThereIsNoLsof(t *testing.T) {
	fakeProc(t, 77, "/srv/cortex (deleted)")
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	ss := "#!/bin/sh\n" +
		`echo 'LISTEN 0 4096 0.0.0.0:47600 0.0.0.0:* users:(("cortex",pid=77,fd=3))'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ss"), []byte(ss), 0o700); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	pid, exe, ok := portHolder("47600")
	if !ok || pid != 77 || exe != "/srv/cortex" {
		t.Errorf("holder = %d %q %v, want 77 /srv/cortex", pid, exe, ok)
	}
}

func TestOurProxy(t *testing.T) {
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "proxy.pid")
	if !ourProxy(binDir, pidFile, 9, filepath.Join(binDir, "cortex")) {
		t.Error("the installed cortex read as foreign")
	}
	if ourProxy(binDir, pidFile, 9, "/usr/bin/python3") {
		t.Error("python read as ours")
	}
	if !ourProxy(binDir, pidFile, 9, filepath.Join(binDir, "authbridge-proxy")) {
		t.Error("v0.7.0's authbridge-proxy read as foreign")
	}
	if ourProxy(binDir, pidFile, 9, filepath.Join(t.TempDir(), "authbridge-proxy")) {
		t.Error("an authbridge-proxy from another directory read as ours")
	}

	// /proc names a running binary by its resolved path, so a bin dir reached
	// through a link (a symlinked HOME, /var -> /private/var) differs as text.
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "cortex"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedBin := filepath.Join(t.TempDir(), "bin")
	if err := os.Symlink(realDir, linkedBin); err != nil {
		t.Fatal(err)
	}
	if !ourProxy(linkedBin, pidFile, 9, filepath.Join(realDir, "cortex")) {
		t.Error("the installed cortex, through a linked bin dir, read as foreign")
	}
	// The same with the binary gone, as GNU readlink -f still resolves it: only the
	// last component may be missing.
	goneDir := t.TempDir()
	linkedGone := filepath.Join(t.TempDir(), "bin")
	if err := os.Symlink(goneDir, linkedGone); err != nil {
		t.Fatal(err)
	}
	if !ourProxy(linkedGone, pidFile, 9, filepath.Join(goneDir, "cortex")) {
		t.Error("a removed install's cortex, through a linked bin dir, read as foreign")
	}
	if ourProxy(linkedGone, pidFile, 9, filepath.Join(realDir, "cortex")) {
		t.Error("a cortex from another directory read as ours while ours is missing")
	}
	// Both missing, in different directories: the fallback must keep the directory,
	// or two absent files that merely share a name would compare equal.
	if ourProxy(linkedGone, pidFile, 9, filepath.Join(t.TempDir(), "cortex")) {
		t.Error("a missing cortex from another directory read as ours while ours is missing too")
	}

	if err := os.WriteFile(pidFile, []byte("9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ourProxy(binDir, pidFile, 9, "/elsewhere/cortex") {
		t.Error("the pidfile's process read as foreign")
	}
}

func TestProxyRunningNamesCortexOnly(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	// A symlink, not a copy: macOS SIGKILLs a copy of its own /bin/sleep at exec, and
	// both kernels name the process after the path it was exec'd by, the link's.
	start := func(t *testing.T, name string) int {
		bin := filepath.Join(t.TempDir(), name)
		if err := os.Symlink(sleep, bin); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd.Process.Pid
	}
	pidFile := filepath.Join(t.TempDir(), "proxy.pid")
	write := func(pid int) {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(start(t, "cortex"))
	if _, ok := proxyRunning(pidFile); !ok {
		t.Error("a live process named cortex read as not running")
	}
	write(start(t, "not-cortex"))
	if _, ok := proxyRunning(pidFile); ok {
		t.Error("a live process with another name read as the proxy")
	}
	write(999999999)
	if _, ok := proxyRunning(pidFile); ok {
		t.Error("a dead pid read as running")
	}
}

// TestPidfileProcessUnnamed covers a sandbox whose ps cannot see the pidfile's
// process: pidfileProcessUnnamed says so, and proxyRunning still trusts the
// pidfile, as install.sh's proxy_running does.
func TestPidfileProcessUnnamed(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "proxy.pid")
	self := os.Getpid()
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(self)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pidfileProcessUnnamed(pidFile) {
		t.Error("a process ps can name read as unnamed")
	}
	installStub(t, "ps", "#!/bin/sh\nexit 1\n")
	if !pidfileProcessUnnamed(pidFile) {
		t.Error("a live process ps cannot see read as named")
	}
	if pid, ok := proxyRunning(pidFile); !ok || pid != self {
		t.Errorf("proxyRunning = %d %v under a blind ps, want %d true", pid, ok, self)
	}
	if err := os.WriteFile(pidFile, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pidfileProcessUnnamed(pidFile) {
		t.Error("a dead pid read as an unnamed live process")
	}
}

func TestStartUnsupervised(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(ok.Close)
	dir := t.TempDir()
	bin := filepath.Join(dir, "cortex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	pid, healthy, err := startUnsupervised(bin, dir, ok.URL+"/healthz")
	if pid > 0 {
		t.Cleanup(func() { _ = stopPID(pid) }) // before Fatalf, so a failed check cannot leak it
	}
	if err != nil || !healthy || pid <= 0 {
		t.Fatalf("start = %d %v %v", pid, healthy, err)
	}
	if readPIDFile(filepath.Join(dir, "proxy.pid")) != pid {
		t.Error("the pidfile does not name the started process")
	}
	if sid, err := unix.Getsid(pid); err != nil || sid != pid {
		t.Errorf("the proxy's session = %d %v, want its own (%d)", sid, err, pid)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)
	dir2 := t.TempDir()
	quits := filepath.Join(dir2, "cortex")
	if err := os.WriteFile(quits, []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	if _, _, err := startUnsupervised(quits, dir2, bad.URL+"/healthz"); err == nil {
		t.Error("a proxy that exits at once was reported started")
	}
	if _, err := os.Stat(filepath.Join(dir2, "proxy.pid")); err == nil {
		t.Error("the pidfile of an exited proxy was left behind")
	}
}

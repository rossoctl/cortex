package main

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cortexPorts are the listeners install.sh probes before a fresh install: forward,
// session API, stats and health. 47603 is not probed, as install.sh does not.
var cortexPorts = []string{"47600", "47601", "47602", "47604"}

// stepError is a failure with the lines that explain it.
type stepError struct {
	reason string
	detail []string
}

func (e stepError) Error() string { return e.reason }

// setupSupervisorUsable is install.sh's supervisor_usable. On macOS it asks
// `launchctl list`, which fails inside a sandbox where bootstrap would — unlike
// `launchctl print gui/<uid>`, which succeeds there and so cannot tell.
func setupSupervisorUsable(goos string) bool {
	var cmd *exec.Cmd
	switch goos {
	case "darwin":
		cmd = exec.Command("launchctl", "list")
	case "linux":
		cmd = exec.Command("systemctl", "--user", "show-environment")
	default:
		return false
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run() == nil
}

// portInUse reports whether something accepts connections on port on loopback,
// v4 or v6 — which a wildcard listener also does. A var because tests run on
// machines with a real Cortex on these ports.
var portInUse = func(port string) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
	}
	return false
}

// procRoot is /proc, a var so tests can fake a process's exe link.
var procRoot = "/proc"

// portHolder is install.sh's port_holder: the pid and executable listening on port.
// It asks lsof for each bind address install.sh asks about, then ss when lsof is
// absent or names no pid, as install.sh does: a sandbox can blind lsof. ok is
// false unless both halves are known — never a placeholder. Accepted: loopback
// and wildcard binds only, as install.sh asks, so a forward port bound to a LAN
// address is never found here.
func portHolder(port string) (pid int, exe string, ok bool) {
	if _, err := exec.LookPath("lsof"); err == nil {
		for _, addr := range []string{"127.0.0.1", "[::1]", "0.0.0.0", "[::]"} {
			out, _ := exec.Command("lsof", "-nP", "-iTCP@"+addr+":"+port, "-sTCP:LISTEN", "-Fp").Output()
			if p, err := strconv.Atoi(firstPrefixed(string(out), "p")); err == nil && p > 0 {
				pid = p
				break
			}
		}
	}
	if pid <= 0 {
		if _, err := exec.LookPath("ss"); err == nil {
			out, _ := exec.Command("ss", "-Hltnp", "sport = :"+port).Output()
			pid = ssListenerPID(string(out), port)
		}
	}
	if pid <= 0 {
		return 0, "", false
	}
	if exe = setupPIDExePath(pid); exe == "" {
		return 0, "", false
	}
	return pid, exe, true
}

// ssListenerPID reads ss -Hltnp output: the first line whose local address is
// port on loopback or a wildcard, and the pid= on it.
func ssListenerPID(out, port string) int {
	addr := regexp.MustCompile(`(^|[^0-9])127\.0\.0\.1:` + port + `$|^\[::1\]:` + port +
		`$|^(0\.0\.0\.0|\*|\[::\]|::):` + port + `$`)
	pidRE := regexp.MustCompile(`pid=([0-9]+)`)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !addr.MatchString(f[3]) {
			continue
		}
		if m := pidRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// setupPIDExePath is install.sh's pid_exe_path: /proc's exe link, else lsof's txt
// entry (the -a matters: without it lsof ORs the filters), else ps's first word.
func setupPIDExePath(pid int) string {
	if link, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "exe")); err == nil {
		return strings.TrimSuffix(link, " (deleted)")
	}
	if out, _ := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "txt", "-Fn").Output(); len(out) > 0 {
		if n := firstPrefixed(string(out), "n"); n != "" {
			return n
		}
	}
	if out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output(); err == nil {
		if f := strings.Fields(strings.SplitN(string(out), "\n", 2)[0]); len(f) > 0 {
			return f[0]
		}
	}
	return ""
}

// holderJob is the command that stops the supervisor job pid runs under, or ""
// when it runs under none that can be found: on macOS a job `launchctl list`
// gives pid for, on Linux the systemd service /proc's cgroup puts it in. Such a
// job, KeepAlive or Restart=, may start again what a kill stops.
func holderJob(goos string, pid int) string {
	if goos == "darwin" {
		out, err := exec.Command("launchctl", "list").Output()
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) == 3 && f[0] == strconv.Itoa(pid) {
				return launchdBootout(f[2])
			}
		}
		return ""
	}
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup")) //nolint:gosec // /proc
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		// cgroup v2's "0::<path>", or v1's systemd hierarchy, "<n>:name=systemd:<path>".
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 || f[1] != "" && f[1] != "name=systemd" {
			continue
		}
		unit := filepath.Base(f[2])
		if !strings.HasSuffix(unit, ".service") || strings.HasPrefix(unit, "user@") {
			continue
		}
		if strings.Contains(f[2], "/user@") {
			return "systemctl --user disable --now " + unit
		}
		return "sudo systemctl disable --now " + unit
	}
	return ""
}

func firstPrefixed(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

// readPIDFile returns the pid in path, or 0 unless it is digits only: install.sh's
// `"" | *[!0-9]*` rule, applied after the trailing newlines its `$(cat)` strips.
// So no sign and no spaces, which strconv.Atoi and TrimSpace would let through.
func readPIDFile(path string) int {
	b, err := os.ReadFile(path) //nolint:gosec // ~/.cortex/proxy.pid
	if err != nil {
		return 0
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// ourProxy is foreign_proxy_holder's rule, inverted: the holder is ours if the
// pidfile names it, or its executable is the installed cortex, by path or after
// resolving links (samePath). Or v0.7.0's authbridge-proxy, adoptablePID's rule:
// it ran under the same label and pidfile, so an upgrade from it finds it on the
// port. A --supervise child runs its parent's executable, so it matches too.
// install.sh never needed this: it asks foreign_proxy_holder only after a failed
// service install, and the install replaces that job.
func ourProxy(binDir, pidFile string, pid int, exe string) bool {
	if pf := readPIDFile(pidFile); pf > 0 && pf == pid {
		return true
	}
	return samePath(exe, filepath.Join(binDir, "cortex")) || samePath(exe, filepath.Join(binDir, "authbridge-proxy"))
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// psComm is ps's comm for pid, and whether ps answered at all.
func psComm(pid int) (string, bool) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	s := strings.TrimSpace(string(out))
	return s, err == nil && s != ""
}

// proxyRunning is install.sh's proxy_running: the pidfile names a live process,
// and ps either cannot see it (sandbox-blind) or calls it cortex.
func proxyRunning(pidFile string) (int, bool) {
	pid := readPIDFile(pidFile)
	if !alive(pid) {
		return 0, false
	}
	comm, seen := psComm(pid)
	if !seen || filepath.Base(comm) == "cortex" {
		return pid, true
	}
	return 0, false
}

// preRenameProxyRunning is install.sh's pre_rename_proxy_running: the pidfile
// names a live process whose executable is binDir's authbridge-proxy, v0.7.0's
// background proxy. proxyRunning does not count it: ps does not call it cortex.
func preRenameProxyRunning(binDir, pidFile string) (int, bool) {
	pid := readPIDFile(pidFile)
	if !alive(pid) {
		return 0, false
	}
	exe := setupPIDExePath(pid)
	if exe == "" || !samePath(exe, filepath.Join(binDir, "authbridge-proxy")) {
		return 0, false
	}
	return pid, true
}

// pidfileProcessUnnamed is install.sh's pidfile_process_unnamed: a live pidfile
// process ps cannot name, which may still re-exec a pre-rename binary.
func pidfileProcessUnnamed(pidFile string) bool {
	pid := readPIDFile(pidFile)
	if !alive(pid) {
		return false
	}
	_, seen := psComm(pid)
	return !seen
}

func portList(ports []string) string { return strings.Join(ports, ", ") }

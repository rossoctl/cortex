package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/core/config"
)

// runMainEnv makes a re-executed copy of this test binary run something other than the
// tests. These tests are about what the process does with its ports and its data on disk,
// which no function short of main() decides, so they run main() itself in a child.
const runMainEnv = "CORTEX_TEST_RUN"

func TestMain(m *testing.M) {
	// The supervisor gives the gateway an empty env, so its copy cannot read runMainEnv.
	if filepath.Base(os.Args[0]) == gatewayBinary {
		fakeGateway(os.Args[1:])
	}
	switch os.Getenv(runMainEnv) {
	case "main":
		endWithTest()
		main()
		os.Exit(0)
	case "stuck-child":
		// The supervisor re-executes this binary without -supervise; that copy stands in for
		// a proxy whose shutdown hangs, so the supervisor's own deadline is what ends it.
		if slices.Contains(os.Args[1:], "-supervise") {
			endWithTest()
			superviseStopGrace = 0
			main()
			os.Exit(0)
		}
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		fmt.Fprintln(os.Stderr, "stuck child: ignoring SIGTERM")
		blockForever()
	case "idle-child":
		// A proxy that starts at once and stops on SIGTERM, for the tests about the
		// gateway beside it. Its supervisor retries at once and logs each retry.
		if slices.Contains(os.Args[1:], "-supervise") {
			endWithTest()
			superviseRestartDelay = time.Millisecond
			slog.SetLogLoggerLevel(slog.LevelDebug)
			main()
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "idle child pid %d\n", os.Getpid())
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// endWithTest ends this process and its group once its stdin closes. startChildAt holds
// the write end, so whatever ends the test binary also ends every process it started.
func endWithTest() {
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = syscall.Kill(0, syscall.SIGKILL)
	}()
}

// blockForever parks the process until a signal ends it. A read on a pipe nothing writes
// to, not select {}: with no other goroutine, select {} is a fatal deadlock.
func blockForever() {
	r, w, err := os.Pipe()
	if err == nil {
		defer w.Close()
		_, _ = r.Read(make([]byte, 1))
	}
	os.Exit(1)
}

// fakeGateway stands in for cortex-gateway: it listens on the gateway listener of
// its -c file and, with --exit-on-stdin-eof, exits when stdin closes. It answers
// --version as the real gateway does.
func fakeGateway(args []string) {
	// The supervisor starts only a gateway of its own version. This copy is the same build.
	if slices.Contains(args, "--version") {
		fmt.Println(gatewayBinary + " " + version)
		os.Exit(0)
	}
	var path string
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			path = args[i+1]
		}
	}
	b, err := os.ReadFile(path)
	var f gatewayFile
	if err == nil {
		err = yaml.Unmarshal(b, &f)
	}
	if err == nil && len(f.Listeners) == 0 {
		err = errors.New("no listener")
	}
	var ln net.Listener
	if err == nil {
		ln, err = net.Listen("tcp", f.Listeners[0].Address)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake gateway: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "fake gateway pid %d\n", os.Getpid())
	if slices.Contains(args, "--exit-on-stdin-eof") {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Fprintf(os.Stderr, "fake gateway pid %d: stdin closed, exiting\n", os.Getpid())
		os.Exit(0)
	}
	// Block in Accept, not select {}: with no other goroutine, select {} is a fatal deadlock.
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		_ = c.Close()
	}
}

// lockedBuffer collects a child's output while the test reads it.
type lockedBuffer struct {
	mu   sync.Mutex
	b    bytes.Buffer
	grew chan struct{} // closed by the next Write
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.grew != nil {
		close(l.grew)
		l.grew = nil
	}
	return l.b.Write(p)
}

// snapshot returns the output so far and a channel that closes on the next Write.
func (l *lockedBuffer) snapshot() (string, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.grew == nil {
		l.grew = make(chan struct{})
	}
	return l.b.String(), l.grew
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type proxyFixture struct {
	home, config string
}

// newProxyFixture writes a forward-only, loopback-only config. local puts it where a local
// install's is — under $HOME/.cortex, which turns the session archive and the cost ledger
// on — and !local somewhere else, which is how a pod's config looks to this binary.
func newProxyFixture(t *testing.T, local bool) proxyFixture {
	t.Helper()
	f := proxyFixture{home: t.TempDir()}
	dir := filepath.Join(f.home, cortexDirName)
	if !local {
		dir = filepath.Join(f.home, "elsewhere")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.config = filepath.Join(dir, "config.yaml")
	writeProxyConfig(t, f.config, anyPort)
	return f
}

// anyPort has the kernel pick the port when the child binds. Picking one here and handing it
// over raced: a port free when the test asked could be taken by the time the child bound
// it, which failed the child with "already in use" (#1452). startProxy reads back where the
// child landed.
const anyPort = "127.0.0.1:0"

// writeProxyConfig writes the fixture's config to path with the forward proxy on forward and
// every other listener on anyPort.
func writeProxyConfig(t *testing.T, path, forward string) {
	t.Helper()
	body := fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  bind_loopback_only: true
  forward_proxy_addr: %s
  session_api_addr: %s
  health_addr: %s
  transparent_proxy_addr: %s
stats:
  address: %s
pipeline:
  inbound: {plugins: []}
  outbound: {plugins: []}
`, forward, anyPort, anyPort, anyPort, anyPort)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// childEnv is this environment with HOME moved and anything that would route the child's
// traffic through, or make it trust, a Cortex the developer is running dropped.
func childEnv(home, run string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HOME", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", runMainEnv:
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+home, runMainEnv+"="+run)
}

type child struct {
	cmd  *exec.Cmd
	out  *lockedBuffer
	done chan error
}

func startChild(t *testing.T, home, run string, args ...string) *child {
	t.Helper()
	return startChildAt(t, os.Args[0], home, run, args...)
}

// startChildAt is startChild with a copy of this test binary at path.
func startChildAt(t *testing.T, path, home, run string, args ...string) *child {
	t.Helper()
	c := &child{out: &lockedBuffer{}, done: make(chan error, 1)}
	c.cmd = exec.Command(path, args...) //nolint:gosec // this test binary
	c.cmd.Env = childEnv(home, run)
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	// Its own process group, as under launchd, so cleanup can end a supervisor's child too:
	// one left behind would hold the output pipe open and keep Wait from returning.
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The child's stdin, which endWithTest watches. Wait closes it.
	if _, err := c.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- c.cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		<-c.done
		if t.Failed() {
			t.Logf("child output:\n%s", c.out.String())
		}
	})
	return c
}

// await waits until the child's output satisfies ok, and returns that output. It fails
// when the child exits first.
func (c *child) await(t *testing.T, what string, ok func(out string) bool) string {
	t.Helper()
	for {
		out, grew := c.out.snapshot()
		if ok(out) {
			return out
		}
		select {
		case <-grew:
		case err := <-c.done:
			c.done <- err
			// Wait has returned, so the output is complete.
			if out := c.out.String(); ok(out) {
				return out
			}
			t.Fatalf("the child exited (%v) before its output showed %s:\n%s", err, what, c.out.String())
		}
	}
}

// wait waits for the child to exit and for every process sharing its output to close it.
func (c *child) wait() error {
	err := <-c.done
	c.done <- err
	return err
}

// proxy is a running child and the forward proxy address it bound.
type proxy struct {
	*child
	forward string
}

// Where the child's servers landed, read off the lines each logs as it starts serving.
var (
	healthListening  = regexp.MustCompile(`msg="health server listening" [^\n]*\baddr=(\S+)`)
	forwardListening = regexp.MustCompile(`msg="HTTP server listening" [^\n]*\bname=forward-proxy addr=(\S+)`)
)

func startProxy(t *testing.T, f proxyFixture) proxy {
	t.Helper()
	c := startChild(t, f.home, "main", "-config", f.config)
	// Each server's listener is bound before it logs, so the addresses answer once logged.
	// Both servers start after main's signal.Notify, so a SIGTERM from here on reaches
	// the shutdown path.
	out := c.await(t, "its health and forward-proxy addresses", func(out string) bool {
		return healthListening.MatchString(out) && forwardListening.MatchString(out)
	})
	h, fw := healthListening.FindStringSubmatch(out), forwardListening.FindStringSubmatch(out)
	resp, err := http.Get("http://" + h[1] + "/healthz")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health check: status %d", resp.StatusCode)
	}
	return proxy{child: c, forward: fw[1]}
}

// hangThroughProxy sends a plain-HTTP request through the forward proxy to an upstream
// that holds it until release is called, and returns once the upstream holds it — a
// request in flight. result carries the request's outcome: nil for a 200.
func hangThroughProxy(t *testing.T, forward string) (release func(), result <-chan error) {
	t.Helper()
	arrived := make(chan struct{}, 1)
	released := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(released) }) }
	up := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-released
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = up.Serve(ln) }()
	t.Cleanup(func() { release(); _ = up.Close() })

	proxyURL, _ := url.Parse("http://" + forward)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	res := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + ln.Addr().String() + "/hang")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if err == nil && resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		res <- err
	}()
	select {
	case <-arrived:
	case err := <-res:
		t.Fatalf("the request never reached the upstream through the proxy: %v", err)
	}
	return release, res
}

// A second proxy started over a running one must give up on the ports BEFORE it touches
// ~/.cortex. Opening the session archive is not read-only — it removes trash, prunes, and
// is closed on the fatal path — and the running proxy is writing the same files. On
// 2026-10-07 two supervised children did exactly that against an orphaned proxy, replaying
// segments it had not finished (incompleteSegments=11), before each died on the bind.
func TestSecondInstanceExitsBeforeOpeningData(t *testing.T) {
	f := newProxyFixture(t, true)
	first := startProxy(t, f)
	if !strings.Contains(first.out.String(), "session archive enabled") {
		t.Fatalf("fixture is not a local install: the first proxy did not open an archive")
	}

	// Its own config beside the first's, so it can name the port the first one bound: the
	// forward proxy is the first socket it reserves.
	over := filepath.Join(filepath.Dir(f.config), "over.yaml")
	writeProxyConfig(t, over, first.forward)
	second := startChild(t, f.home, "main", "-config", over)
	_ = second.wait()
	out := second.out.String()
	if second.cmd.ProcessState.Success() {
		t.Fatalf("second proxy exited 0 over a running one")
	}
	for _, opened := range []string{"session archive enabled", "cost ledger enabled"} {
		if strings.Contains(out, opened) {
			t.Errorf("second proxy logged %q before giving up on the ports", opened)
		}
	}
	if !strings.Contains(out, "already in use") {
		t.Errorf("second proxy did not say a port was taken:\n%s", out)
	}
}

// A local install stops at once: it releases every port and exits rather than draining the
// requests in flight, which its clients retry. A drain made every restart wait out the
// longest open request — a stream that never ends held it to its 15s deadline, long enough
// for launchd's 5s exit timeout to kill the supervisor and orphan the proxy on its ports.
func TestLocalInstallStopsWithoutDraining(t *testing.T) {
	f := newProxyFixture(t, true)
	p := startProxy(t, f)
	_, result := hangThroughProxy(t, p.forward)

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The upstream holds the request until cleanup, so the proxy exits with it in flight.
	// Stopping at once is the property under test, so this wait has a bound: 4s, inside
	// launchd's 5s window and well above the 1s budget of a stop without a drain.
	select {
	case err := <-p.done:
		p.done <- err
	case <-time.After(4 * time.Second): // timing: launchd's 5s exit window is the property
		t.Fatal("the proxy is still running 4s after SIGTERM; a local install stops at once rather than draining")
	}
	if !p.cmd.ProcessState.Success() {
		t.Errorf("exit was not clean: %v", p.cmd.ProcessState)
	}
	if err := <-result; err == nil {
		t.Error("the request in flight completed; a local install drops it with its connection")
	}
	if out := p.out.String(); !strings.Contains(out, "drain=false") {
		t.Errorf("a local install's shutdown is not logged as drain=false:\n%s", out)
	}
}

// Anywhere else — a pod — the drain stays: endpoint removal propagates after SIGTERM, so
// callers are still being routed here while it runs.
func TestPodStillDrains(t *testing.T) {
	f := newProxyFixture(t, false)
	p := startProxy(t, f)
	release, result := hangThroughProxy(t, p.forward)

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	p.await(t, "a draining shutdown", func(out string) bool {
		return strings.Contains(out, `msg="shutting down"`) && strings.Contains(out, "drain=true")
	})
	select {
	case err := <-p.done:
		p.done <- err
		t.Fatalf("a pod's proxy exited at once with a request in flight: %v", err)
	default:
	}
	// The drain lets the request in flight finish, and then the proxy exits.
	release()
	if err := <-result; err != nil {
		t.Errorf("the request in flight did not complete through the drain: %v", err)
	}
	_ = p.wait()
	if !p.cmd.ProcessState.Success() {
		t.Errorf("exit was not clean: %v", p.cmd.ProcessState)
	}
}

// A SIGTERM during startup takes the shutdown path. In praxis mode main reads the gateway
// token after its signal.Notify and before any server starts, so a FIFO there holds main
// in that window while the signal arrives.
func TestStartupSIGTERMShutsDown(t *testing.T) {
	f := newProxyFixture(t, false)
	praxis := fmt.Sprintf("praxis: {enabled: true, listen: %q, egress: %q}\n", freeAddr(t), freeAddr(t))
	body, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, append(body, praxis...), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(f.home, "token")
	if err := syscall.Mkfifo(token, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(gatewayTokenEnv, token)
	c := startChild(t, f.home, "main", "-config", f.config)

	// Opening the FIFO to write returns once main opens it to read.
	type opened struct {
		w   *os.File
		err error
	}
	open := make(chan opened, 1)
	go func() {
		w, err := os.OpenFile(token, os.O_WRONLY, 0)
		open <- opened{w, err}
	}()
	var o opened
	select {
	case o = <-open:
	case err := <-c.done:
		c.done <- err
		t.Fatalf("the child exited (%v) before it read the gateway token:\n%s", err, c.out.String())
	}
	if o.err != nil {
		t.Fatal(o.err)
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_, err = o.w.WriteString("startup-token\n")
	if cerr := o.w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	c.await(t, "a shutdown", func(out string) bool {
		return strings.Contains(out, `msg="shutting down"`)
	})
	_ = c.wait()
	if !c.cmd.ProcessState.Success() {
		t.Errorf("exit was not clean: %v", c.cmd.ProcessState)
	}
}

// The supervisor ends a proxy that will not stop inside launchd's exit timeout (5s, pinned
// as ExitTimeOut in the plist agentop writes). Waiting longer is what let launchd SIGKILL
// the supervisor mid-wait: launchd only SIGTERMs the rest of the process group, which a
// proxy that has already taken its one SIGTERM ignores, so it lived on holding its ports.
// The bound itself is checked in TestSuperviseDelays_AreOrdered; here the grace is 0.
func TestSupervisorEndsAStuckProxyInsideLaunchdsWindow(t *testing.T) {
	home := t.TempDir()
	sup := startChild(t, home, "stuck-child", "-supervise")
	sup.await(t, "the supervised child starting", func(out string) bool {
		return strings.Contains(out, "stuck child: ignoring SIGTERM")
	})

	if err := sup.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The child ignores SIGTERM, so only the kill ends it, and Wait returns once it is gone.
	_ = sup.wait()
	if !strings.Contains(sup.out.String(), "killing it") {
		t.Errorf("supervisor exited without killing the stuck proxy:\n%s", sup.out.String())
	}
}

// praxisRig is a bin dir with this test binary copied in as cortex and as
// cortex-gateway, and a praxis-mode config. Copies, not links: os.Executable
// resolves a symlink on Linux, and the supervisor finds the gateway next to itself.
type praxisRig struct {
	home, bin, config, listen string
}

func newPraxisRig(t *testing.T, praxis bool) praxisRig {
	t.Helper()
	r := praxisRig{home: t.TempDir(), listen: freeAddr(t)}
	r.bin = filepath.Join(r.home, "bin")
	dir := filepath.Join(r.home, cortexDirName)
	for _, d := range []string{r.bin, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"cortex", gatewayBinary} {
		if err := os.WriteFile(filepath.Join(r.bin, n), self, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.config = filepath.Join(dir, "config.yaml")
	writeProxyConfig(t, r.config, anyPort)
	praxisBlock := fmt.Sprintf("praxis:\n  enabled: %v\n  listen: %s\n", praxis, r.listen)
	b, _ := os.ReadFile(r.config)
	if err := os.WriteFile(r.config, append(b, praxisBlock...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(r.config); err != nil {
		t.Fatalf("the fixture config does not load: %v", err)
	}
	return r
}

func (r praxisRig) supervise(t *testing.T) *child {
	t.Helper()
	return startChildAt(t, filepath.Join(r.bin, "cortex"), r.home, "idle-child", "-supervise", "-config", r.config)
}

var (
	idleChildPid   = regexp.MustCompile(`idle child pid (\d+)`)
	fakeGatewayPid = regexp.MustCompile(`fake gateway pid (\d+)`)
)

// pids waits until out holds at least n lines that re matches, and returns their pids.
func (c *child) pids(t *testing.T, re *regexp.Regexp, n int) []int {
	t.Helper()
	out := c.await(t, fmt.Sprintf("%d lines matching %s", n, re), func(out string) bool {
		return len(re.FindAllStringSubmatch(out, -1)) >= n
	})
	var pids []int
	for _, m := range re.FindAllStringSubmatch(out, -1) {
		p, _ := strconv.Atoi(m[1])
		pids = append(pids, p)
	}
	return pids
}

// alive reports whether pid runs. A zombie has exited: in a container whose PID 1
// does not reap (CI's job container), an orphan stays a zombie, and kill(pid, 0)
// still succeeds on it.
func alive(pid int) bool {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) {
			return b[i+2] != 'Z'
		}
	}
	return syscall.Kill(pid, 0) == nil
}

// A cortex crash restarts cortex only: the gateway beside it keeps its pid.
func TestSupervisor_CortexRestartKeepsGateway(t *testing.T) {
	r := newPraxisRig(t, true)
	sup := r.supervise(t)
	cortex := sup.pids(t, idleChildPid, 1)[0]
	gw := sup.pids(t, fakeGatewayPid, 1)[0]

	if err := syscall.Kill(cortex, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if again := sup.pids(t, idleChildPid, 2); again[1] == cortex {
		t.Fatalf("cortex restarted with the same pid %d", cortex)
	}
	if gws := sup.pids(t, fakeGatewayPid, 1); len(gws) != 1 || !alive(gw) {
		t.Fatalf("the gateway changed after a cortex crash: pids %v, %d alive = %v", gws, gw, alive(gw))
	}

	if err := sup.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = sup.wait()
	// The supervisor reaps both children before it exits.
	newCortex := sup.pids(t, idleChildPid, 2)[1]
	for _, pid := range []int{newCortex, gw} {
		if alive(pid) {
			t.Errorf("pid %d still runs after the supervisor stopped", pid)
		}
	}
}

// A SIGKILL of the supervisor closes the gateway's stdin, so the gateway exits.
func TestSupervisor_KillLeavesNoGateway(t *testing.T) {
	r := newPraxisRig(t, true)
	sup := r.supervise(t)
	sup.pids(t, idleChildPid, 1)
	gw := sup.pids(t, fakeGatewayPid, 1)[0]
	if err := sup.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// The idle cortex child is left behind; startChild's cleanup kills its group.
	sup.await(t, "the gateway seeing its stdin close", func(out string) bool {
		return strings.Contains(out, fmt.Sprintf("fake gateway pid %d: stdin closed", gw))
	})
}

// Direct mode starts no gateway and writes no gateway file, even with no
// cortex-gateway binary next to cortex.
func TestSupervisor_DirectModeStartsNoGateway(t *testing.T) {
	r := newPraxisRig(t, false)
	if err := os.Remove(filepath.Join(r.bin, gatewayBinary)); err != nil {
		t.Fatal(err)
	}
	sup := r.supervise(t)
	sup.pids(t, idleChildPid, 1)
	// Once the supervisor has exited, every child loop it ran has logged and returned.
	if err := sup.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = sup.wait()
	if out := sup.out.String(); strings.Contains(out, gatewayBinary) {
		t.Errorf("direct mode logged the gateway:\n%s", out)
	}
	for _, n := range []string{gatewayTokenName, gatewayYAMLName} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(r.config), n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: err = %v, want not exist", n, err)
		}
	}
}

// A gateway that cannot start (here, a missing binary) does not stop cortex: the
// supervisor says so at ERROR one time, retries with backoff, and still stops on SIGTERM.
func TestSupervisor_GatewayStartFailureKeepsCortex(t *testing.T) {
	r := newPraxisRig(t, true)
	gwPath := filepath.Join(r.bin, gatewayBinary)
	if err := os.Remove(gwPath); err != nil {
		t.Fatal(err)
	}
	sup := r.supervise(t)
	cortex := sup.pids(t, idleChildPid, 1)[0]
	// A second failure, logged at DEBUG, shows the retry.
	sup.await(t, "a second failed gateway start", func(out string) bool {
		return strings.Count(out, "cannot start the child") >= 2
	})
	if !alive(cortex) {
		t.Fatalf("cortex pid %d stopped after the gateway failed to start:\n%s", cortex, sup.out.String())
	}
	var errLines []string
	for _, l := range strings.Split(sup.out.String(), "\n") {
		if strings.Contains(l, "cannot start the child") && strings.Contains(l, "ERROR") {
			errLines = append(errLines, l)
		}
	}
	if len(errLines) != 1 || !strings.Contains(errLines[0], gwPath) {
		t.Fatalf("want one ERROR line that names %s, got %q:\n%s", gwPath, errLines, sup.out.String())
	}

	if err := sup.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = sup.wait()
	if alive(cortex) {
		t.Errorf("cortex pid %d still runs after the supervisor stopped", cortex)
	}
}

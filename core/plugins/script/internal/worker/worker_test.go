package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The test binary is its own fake script. With FAKE_WORKER set, TestMain runs the
// named behaviour instead of the tests, so nothing here needs Python or a shell.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_WORKER"); mode != "" {
		fakeWorker(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeWorker(mode string) {
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	switch mode {
	case "no-handshake":
		fmt.Fprintln(out, "hello, I am not a handshake")
		out.Flush()
		time.Sleep(time.Hour)
	case "wrong-protocol":
		send(map[string]any{"cortex": 99})
		time.Sleep(time.Hour)
	case "exit-at-start":
		os.Exit(3)
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_START_DELAY")); err == nil {
		time.Sleep(d)
	}
	if mode == "stderr-flood-start" {
		os.Stderr.WriteString(strings.Repeat("x", 2<<20) + "\n")
	}
	fmt.Fprintln(out, "starting up") // stray output before the handshake is skipped
	send(Handshake{Cortex: Protocol, Hooks: []string{"request"}, Enforce: mode == "enforce"})
	if mode == "deaf" {
		time.Sleep(time.Hour) // handshakes, then never reads stdin
	}
	if mode == "spawn-child" || mode == "spawn-child-then-crash" {
		// A grandchild in the same process group; the test checks a timeout kills it.
		child := exec.Command("sleep", "3600")
		_ = child.Start()
		os.WriteFile(os.Getenv("FAKE_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	count := 0
	for in.Scan() {
		var msg map[string]any
		json.Unmarshal(in.Bytes(), &msg)
		count++
		switch mode {
		case "hang", "spawn-child":
			time.Sleep(time.Hour)
		case "stderr-flood-call":
			os.Stderr.WriteString(strings.Repeat("x", 2<<20) + "\n")
		case "stderr-flood-unterminated":
			os.Stderr.WriteString(strings.Repeat("x", 2<<20))
		case "slow":
			time.Sleep(300 * time.Millisecond)
		case "spawn-child-then-crash":
			os.Exit(2)
		case "crash-after-one":
			if count > 1 {
				os.Exit(2)
			}
		case "stale-id":
			send(map[string]any{"id": -1, "stale": true})
		}
		fmt.Fprintln(out, "log line", count) // the script's own output, between replies
		send(map[string]any{"id": msg["id"], "count": count, "echo": msg["echo"]})
	}
}

func fake(t *testing.T, mode string, extraEnv ...string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Path:         exe,
		Env:          append(append(os.Environ(), "FAKE_WORKER="+mode), extraEnv...),
		StartTimeout: 5 * time.Second,
		CallTimeout:  2 * time.Second,
	}
}

// call makes one call, starting w first: a Call never waits for a start.
func call(t *testing.T, w *Worker, echo string) (map[string]any, Reply) {
	t.Helper()
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r, err := w.Call(context.Background(), map[string]any{"echo": echo})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(r.Line, &got); err != nil {
		t.Fatalf("reply %q: %v", r.Line, err)
	}
	return got, r
}

func TestWorker_HandshakeAndCalls(t *testing.T) {
	w := New(fake(t, "enforce"))
	t.Cleanup(w.Stop)
	hs, err := w.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !hs.Enforce || !hs.Has("request") || hs.Has("response") {
		t.Errorf("handshake = %+v", hs)
	}
	for i := 1; i <= 3; i++ {
		got, r := call(t, w, "hi")
		if got["count"] != float64(i) || got["echo"] != "hi" {
			t.Errorf("call %d: reply %v", i, got)
		}
		// State lives in the process: the count rises, so it is one process throughout.
		if len(r.Logs) != 1 || r.Logs[0] != "log line "+strconv.Itoa(i) {
			t.Errorf("call %d: logs %q", i, r.Logs)
		}
	}
}

// A Call to a worker never started starts it in the background and fails at once,
// so a script that is slow to start does not hold a request up.
func TestWorker_CallStartsTheProcessInTheBackground(t *testing.T) {
	w := New(fake(t, "plain"))
	t.Cleanup(w.Stop)
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrStarting) {
		t.Fatalf("first Call = %v, want ErrStarting", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the background start never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_HandshakeFailures(t *testing.T) {
	for mode, want := range map[string]string{
		"no-handshake":   "no handshake",
		"wrong-protocol": "protocol 99",
		"exit-at-start":  "no handshake",
	} {
		t.Run(mode, func(t *testing.T) {
			cfg := fake(t, mode)
			cfg.StartTimeout = 300 * time.Millisecond
			w := New(cfg)
			t.Cleanup(w.Stop)
			_, err := w.Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Start error = %v, want one mentioning %q", err, want)
			}
		})
	}
}

// A reply carrying another call's id is the script's business, not this call's reply:
// it is kept as output and the real reply is still found.
func TestWorker_IgnoresARepliesWithAnotherID(t *testing.T) {
	w := New(fake(t, "stale-id"))
	t.Cleanup(w.Stop)
	got, r := call(t, w, "x")
	if got["echo"] != "x" {
		t.Errorf("reply %v", got)
	}
	if len(r.Logs) != 2 || !strings.Contains(r.Logs[0], "stale") {
		t.Errorf("logs %q", r.Logs)
	}
}

func TestWorker_TimeoutKillsAndRestarts(t *testing.T) {
	cfg := fake(t, "hang")
	cfg.CallTimeout = 200 * time.Millisecond
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := w.Call(context.Background(), map[string]any{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timed-out call took %v", d)
	}
	// Backing off: refused immediately rather than started again.
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrBackoff) {
		t.Errorf("second Call error = %v, want ErrBackoff", err)
	}
}

// A start in progress, from a call or from Start, holds nothing a call waits for:
// calls made meanwhile fail at once, and Start waits for that same start.
func TestWorker_CallsDuringAStartFailAtOnce(t *testing.T) {
	for _, by := range []string{"call", "Start"} {
		t.Run("started by "+by, func(t *testing.T) {
			cfg := fake(t, "plain", "FAKE_START_DELAY=1500ms")
			cfg.CallTimeout = 500 * time.Millisecond
			w := New(cfg)
			t.Cleanup(w.Stop)
			started := make(chan error, 1)
			if by == "call" {
				if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrStarting) {
					t.Fatalf("first Call = %v, want ErrStarting", err)
				}
			} else {
				go func() { _, err := w.Start(context.Background()); started <- err }()
			}
			time.Sleep(100 * time.Millisecond)
			for i := range 3 {
				t0 := time.Now()
				_, err := w.Call(context.Background(), map[string]any{})
				if !errors.Is(err, ErrStarting) {
					t.Fatalf("call %d during the start = %v, want ErrStarting", i, err)
				}
				if d := time.Since(t0); d > 100*time.Millisecond {
					t.Fatalf("call %d during the start took %v", i, d)
				}
			}
			t0 := time.Now()
			w.Handshake()
			if d := time.Since(t0); d > 100*time.Millisecond {
				t.Errorf("Handshake during the start took %v", d)
			}
			// Start joins the start in progress rather than running a second child.
			hs, err := w.Start(context.Background())
			if err != nil || hs.Cortex != Protocol {
				t.Fatalf("Start = %+v, %v", hs, err)
			}
			if by == "Start" {
				if err := <-started; err != nil {
					t.Fatalf("first Start: %v", err)
				}
			}
			if got, _ := call(t, w, "x"); got["count"] != float64(1) {
				t.Errorf("reply %v: not the first call to one child", got)
			}
		})
	}
}

// Calls queued behind a slow script may give up waiting, but a script that answers
// each call within the call timeout is not killed for it: its state survives.
func TestWorker_AQueueThatOutlastsACallerKeepsTheScript(t *testing.T) {
	cfg := fake(t, "slow")
	cfg.CallTimeout = time.Second
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 6 { // 6 x 300ms queues past a 1s timeout; no one call comes near it
		wg.Add(1)
		go func() { defer wg.Done(); w.Call(context.Background(), map[string]any{}) }()
	}
	wg.Wait()
	got := callKept(t, w)
	if got["count"].(float64) < 2 {
		t.Errorf("reply %v: the script was restarted", got)
	}
}

// Under a queue longer than the call timeout, a message the script is sent is one its
// caller still waits for: the script answers no call nobody reads, so calls keep
// succeeding at the rate it can answer them.
func TestWorker_AnOverloadedScriptAnswersOnlyCallersStillWaiting(t *testing.T) {
	cfg := fake(t, "slow") // 300ms a call
	cfg.CallTimeout = time.Second
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ok atomic.Int64
	var wg sync.WaitGroup
	end := time.Now().Add(3 * time.Second)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(end) {
				if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
					ok.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	got := callKept(t, w)
	answered := int64(got["count"].(float64)) - 1 // the script's count includes callKept's call
	if answered != ok.Load() {
		t.Errorf("the script answered %d calls and %d callers got a reply: %d answers went unread",
			answered, ok.Load(), answered-ok.Load())
	}
	if ok.Load() == 0 {
		t.Error("no call succeeded")
	}
}

// A script's stderr is read to the end whatever its line lengths, so a line longer
// than the logger keeps does not leave the script blocked writing the next one.
func TestWorker_LongStderrLinesDoNotStallTheScript(t *testing.T) {
	for _, mode := range []string{"stderr-flood-start", "stderr-flood-call", "stderr-flood-unterminated"} {
		t.Run(mode, func(t *testing.T) {
			w := New(fake(t, mode))
			t.Cleanup(w.Stop)
			if _, err := w.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			for i := range 3 {
				if _, err := w.Call(context.Background(), map[string]any{}); err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
			}
		})
	}
}

// Lines after one too long to keep are still logged, and the long one is logged cut.
func TestLogStderr_KeepsLoggingAfterALongLine(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	logStderr(strings.NewReader("before\n"+strings.Repeat("x", 2<<20)+"\n\nafter\n"+strings.Repeat("y", 2<<20)), "s")
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	for _, want := range []string{"line=before", "line=after", "line=xxx", "line=yyy", `line=""`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%.300s", want, out)
		}
	}
	if n := strings.Count(out, "script stderr"); n != 5 {
		t.Errorf("logged %d lines, want 5", n)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A script that exits takes the processes it started with it.
func TestWorker_AnExitKillsTheWholeProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	w := New(fake(t, "spawn-child-then-crash", "FAKE_PIDFILE="+pidfile))
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.Call(context.Background(), map[string]any{}) // the script exits on it
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the script's exit", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A child that exited and was reaped before the worker noticed is not signalled
// again: by then its process group id may belong to an unrelated group.
func TestWorker_AReapedChildIsNotSignalledLater(t *testing.T) {
	w := New(fake(t, "crash-after-one"))
	t.Cleanup(w.Stop)
	call(t, w, "first")
	p := w.proc
	var late atomic.Int64
	real := killGroup
	killGroup = func(cmd *exec.Cmd) {
		if p.exited() {
			late.Add(1)
		}
		real(cmd)
	}
	t.Cleanup(func() { killGroup = real })
	p.stdin.Write([]byte("{}\n")) // the second message makes it exit
	<-p.done
	w.Call(context.Background(), map[string]any{})
	if n := late.Load(); n != 0 {
		t.Errorf("signalled a reaped child's process group %d times", n)
	}
}

// A caller that goes away mid-call stops waiting without killing the script.
func TestWorker_ACallerThatGivesUpKeepsTheScript(t *testing.T) {
	for name, mk := range map[string]func() (context.Context, context.CancelFunc){
		"cancelled mid-call": func() (context.Context, context.CancelFunc) {
			ctx, c := context.WithCancel(context.Background())
			time.AfterFunc(50*time.Millisecond, c)
			return ctx, c
		},
		"its own deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		},
		"cancelled before it": func() (context.Context, context.CancelFunc) {
			ctx, c := context.WithCancel(context.Background())
			c()
			return ctx, c
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := New(fake(t, "slow"))
			t.Cleanup(w.Stop)
			if _, err := w.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			for range 5 {
				ctx, cancel := mk()
				t0 := time.Now()
				_, err := w.Call(ctx, map[string]any{})
				cancel()
				if err == nil {
					t.Fatal("a call whose caller gave up succeeded")
				}
				if d := time.Since(t0); d > 250*time.Millisecond {
					t.Fatalf("the call outlived its caller by %v", d)
				}
			}
			callKept(t, w)
		})
	}
}

// sameChild reports whether a and b drive one child process.
func sameChild(a, b *Worker) bool { return a.child == b.child }

// callKept calls w and fails unless the running script answers it.
func callKept(t *testing.T, w *Worker) map[string]any {
	t.Helper()
	r, err := w.Call(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("the next call = %v: the script was killed", err)
	}
	var got map[string]any
	json.Unmarshal(r.Line, &got)
	return got
}

func TestWorker_TimeoutKillsTheWholeProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	cfg := fake(t, "spawn-child", "FAKE_PIDFILE="+pidfile)
	cfg.CallTimeout = 200 * time.Millisecond
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("call to a hanging script succeeded")
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_CrashThenRestartAfterBackoff(t *testing.T) {
	w := New(fake(t, "crash-after-one"))
	t.Cleanup(w.Stop)
	var clockMu sync.Mutex
	clock := time.Now()
	w.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	call(t, w, "first")
	if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("call to a crashing script succeeded")
	}
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrBackoff) {
		t.Fatalf("call during backoff = %v, want ErrBackoff", err)
	}
	clockMu.Lock()
	clock = clock.Add(minBackoff + time.Millisecond)
	clockMu.Unlock()
	// After the backoff the restart runs in the background: the call that finds no
	// child fails at once instead of waiting for it.
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrStarting) {
		t.Fatalf("call after backoff = %v, want ErrStarting", err)
	}
	// A new process: its count starts over, which is the state loss a crash costs.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := w.Call(context.Background(), map[string]any{"echo": "again"})
		if err == nil {
			var got map[string]any
			json.Unmarshal(r.Line, &got)
			if got["count"] != float64(1) {
				t.Errorf("after restart, reply %v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never restarted: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorker_StopRefusesCalls(t *testing.T) {
	w := New(fake(t, "plain"))
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	call(t, w, "x")
	w.Stop()
	if _, err := w.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("Call after Stop succeeded")
	}
}

func TestParseLoginPath(t *testing.T) {
	for out, want := range map[string]string{
		"Welcome!\nnvm loaded\n\n" + pathMarker + "/opt/homebrew/bin:/usr/bin" + pathMarker + "\n": "/opt/homebrew/bin:/usr/bin",
		"no markers at all":            "",
		pathMarker + "only one marker": "",
	} {
		if got := parseLoginPath(out); got != want {
			t.Errorf("parseLoginPath(%q) = %q, want %q", out, got, want)
		}
	}
}

// A login shell whose profile prints still yields its PATH.
func TestLoginPath_ProfileThatPrints(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	shell := filepath.Join(dir, "chatty-shell")
	// A shell that greets before running what it was asked to.
	os.WriteFile(shell, []byte("#!"+sh+"\necho 'Welcome to the machine'\nshift\nPATH=/from/login exec "+sh+" \"$@\"\n"), 0o700)
	t.Setenv("SHELL", shell)
	loginPathMu.Lock()
	loginPathVal, loginPathTried = "", time.Time{}
	old := loginPathTimeout
	loginPathTimeout = 30 * time.Second
	loginPathMu.Unlock()
	t.Cleanup(func() { loginPathVal, loginPathTried, loginPathTimeout = "", time.Now(), old })
	if got := loginPath(); got != "/from/login" {
		t.Errorf("loginPath() = %q, want /from/login", got)
	}
}

func TestEnviron(t *testing.T) {
	loginPathMu.Lock()
	loginPathVal, loginPathTried = "", time.Now() // no login shell in a test: use the base PATH
	loginPathMu.Unlock()
	got := Environ([]string{
		"HOME=/h", "HTTPS_PROXY=http://127.0.0.1:47600", "http_proxy=x", "NO_PROXY=a",
		"PATH=/usr/bin", "PYTHONPATH=/mine",
	}, "/sdk")
	want := []string{"HOME=/h", "PATH=/usr/bin", "PYTHONPATH=/sdk:/mine"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("Environ = %q, want %q", got, want)
	}
}

// A shell whose profile leaves a background process holding stdout still answers
// within the probe's budget: with its PATH when it printed one, empty when it hung.
func TestLoginPath_BackgroundChildHoldingStdout(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, tc := range map[string]struct{ body, want string }{
		"printed then exited": {"sleep 5 &\nshift\nPATH=/from/login exec " + sh + " \"$@\"\n", "/from/login"},
		"hung":                {"sleep 5 &\nsleep 5\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			shell := filepath.Join(t.TempDir(), "shell")
			os.WriteFile(shell, []byte("#!"+sh+"\n"+tc.body), 0o700)
			t.Setenv("SHELL", shell)
			loginPathMu.Lock()
			loginPathVal, loginPathTried = "", time.Time{}
			old := loginPathTimeout
			loginPathTimeout = 500 * time.Millisecond
			loginPathMu.Unlock()
			t.Cleanup(func() { loginPathVal, loginPathTried, loginPathTimeout = "", time.Now(), old })
			t0 := time.Now()
			got := loginPath()
			if d := time.Since(t0); d > 2*time.Second {
				t.Errorf("loginPath took %v against a %v budget", d, loginPathTimeout)
			}
			if got != tc.want {
				t.Errorf("loginPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// waitStopped waits for w to refuse calls; a released worker stops in the background.
func waitStopped(t *testing.T, w *Worker, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := w.Call(context.Background(), map[string]any{})
		if err != nil && strings.Contains(err.Error(), "stopped") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still answers", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPool_ReusesAnUnchangedScript(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	one := p.NewHolder()
	a := p.Acquire(one, cfg, "h1")
	t.Cleanup(a.Stop)
	call(t, a, "x")
	p.Activate(one)
	// The next build, same file: the same worker, with its state.
	two := p.NewHolder()
	if b := p.Acquire(two, cfg, "h1"); !sameChild(a, b) {
		t.Fatal("an unchanged script got a new worker")
	}
	p.Activate(two)
	p.Release(one) // the old pipeline is drained
	if got, _ := call(t, a, "y"); got["count"] != float64(2) {
		t.Errorf("state lost across builds: %v", got)
	}
	p.Release(two)
	waitStopped(t, a, "a worker no build holds")
}

// A reused worker takes the call timeout of the build that acquired it, so an edited
// timeout applies to the running script without restarting it.
func TestPool_AReusedWorkerTakesTheNewCallTimeout(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "hang")
	cfg.CallTimeout = 10 * time.Second
	a := p.Acquire(p.NewHolder(), cfg, "h1")
	t.Cleanup(a.Stop)
	if _, err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.CallTimeout = 200 * time.Millisecond
	b := p.Acquire(p.NewHolder(), cfg, "h1")
	if !sameChild(a, b) {
		t.Fatal("a changed timeout restarted the script")
	}
	t0 := time.Now()
	if _, err := b.Call(context.Background(), map[string]any{}); err == nil {
		t.Fatal("call to a hanging script succeeded")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("call took %v: the new 200ms timeout was not applied", d)
	}
}

// A build's timeout governs that build's calls only: one configured and then refused,
// or one still draining beside a newer one, leaves the other's calls as they were.
func TestPool_ABuildsCallTimeoutStaysWithThatBuild(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "slow") // answers in 300ms
	running := p.NewHolder()
	a := p.Acquire(running, cfg, "h1")
	t.Cleanup(a.Stop)
	if _, err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Activate(running)
	cfg.CallTimeout = 50 * time.Millisecond
	refused := p.NewHolder()
	p.Acquire(refused, cfg, "h1") // configured, never started
	if _, err := a.Call(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("the running build's call = %v: it took the refused build's timeout", err)
	}
	next := p.NewHolder()
	p.Acquire(next, cfg, "h1")
	p.Activate(next) // the old build drains beside it
	if _, err := a.Call(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("the draining build's call = %v: it took the newer build's timeout", err)
	}
}

// The environment is fixed for a process's life, so a changed one gets a new worker.
func TestPool_AChangedEnvironmentGetsANewWorker(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	one := p.NewHolder()
	a := p.Acquire(one, cfg, "h1")
	t.Cleanup(a.Stop)
	cfg.Env = append(slices.Clone(cfg.Env), "PATH=/elsewhere")
	b := p.Acquire(p.NewHolder(), cfg, "h1")
	t.Cleanup(b.Stop)
	if sameChild(a, b) {
		t.Fatal("a changed environment kept the old worker")
	}
}

// An edited script gets a new worker, and the old one keeps serving the running
// pipeline until that pipeline is released — not merely until the new build is made,
// which may still be refused.
func TestPool_AnEditedScriptKeepsItsOldWorkerUntilTheOldBuildIsReleased(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	one := p.NewHolder()
	old := p.Acquire(one, cfg, "h1")
	p.Activate(one)
	two := p.NewHolder()
	edited := p.Acquire(two, cfg, "h2")
	t.Cleanup(edited.Stop)
	if sameChild(edited, old) {
		t.Fatal("an edited script kept its old worker")
	}
	call(t, old, "still serving") // the running pipeline still works
	p.Activate(two)
	call(t, old, "draining")
	p.Release(one)
	waitStopped(t, old, "the old version")
	call(t, edited, "new")
}

// A build that is configured and then refused never starts and is never shut down.
// The next build to start sweeps what it held; the running build keeps its workers.
func TestPool_AnAbandonedBuildIsSweptWhenALaterOneStarts(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	running := p.NewHolder()
	kept := p.Acquire(running, cfg, "h1")
	t.Cleanup(kept.Stop)
	p.Activate(running)

	refused := p.NewHolder()
	orphan := p.Acquire(refused, cfg, "h2") // never activated, never released
	call(t, orphan, "x")

	next := p.NewHolder()
	p.Acquire(next, cfg, "h1")
	p.Activate(next)
	waitStopped(t, orphan, "a refused build's worker")
	call(t, kept, "the running build's worker")
}

// The call deadline bounds the whole exchange, the write included. A script that
// stops reading fills the pipe; a body larger than the pipe buffer — which every
// inference request is — must still time out rather than block the request.
func TestWorker_TimeoutCoversTheWrite(t *testing.T) {
	cfg := fake(t, "deaf")
	cfg.CallTimeout = 200 * time.Millisecond
	w := New(cfg)
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.Call(context.Background(), map[string]any{"body": strings.Repeat("x", 2<<20)})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call to a script that does not read succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the call blocked past its 200ms deadline")
	}
	start := time.Now()
	if _, err := w.Call(context.Background(), map[string]any{}); !errors.Is(err, ErrBackoff) {
		t.Errorf("next call = %v, want ErrBackoff", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the next call waited %v behind the stuck one", d)
	}
	stopped := make(chan struct{})
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked behind the stuck call")
	}
}

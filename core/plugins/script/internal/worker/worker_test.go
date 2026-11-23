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
	"runtime"
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
		block()
	case "wrong-protocol":
		send(map[string]any{"cortex": 99})
		block()
	case "exit-at-start":
		os.Exit(3)
	}
	if path := os.Getenv("FAKE_STARTED"); path != "" {
		signal(path)
	}
	if path := os.Getenv("FAKE_START_GATE"); path != "" {
		await(path)
	}
	if mode == "stderr-flood-start" {
		os.Stderr.WriteString(strings.Repeat("x", 2<<20) + "\n")
	}
	fmt.Fprintln(out, "starting up") // stray output before the handshake is skipped
	send(Handshake{Cortex: Protocol, Hooks: []string{"request"}, Enforce: mode == "enforce"})
	if mode == "deaf" {
		block() // handshakes, then never reads stdin
	}
	if mode == "spawn-child" || mode == "spawn-child-then-crash" {
		// A grandchild in the same process group, holding the test's FIFO open for
		// as long as it lives; the test checks a timeout or an exit kills it.
		hold, _ := os.OpenFile(os.Getenv("FAKE_HOLD"), os.O_WRONLY, 0)
		child := exec.Command("sleep", "3600")
		child.Stdout = hold
		if err := child.Start(); err != nil {
			os.Exit(1) // the FIFO closes unsignalled, so the test reports no grandchild
		}
		hold.Write([]byte{1})
		hold.Close()
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
			block()
		case "stderr-flood-call":
			os.Stderr.WriteString(strings.Repeat("x", 2<<20) + "\n")
		case "stderr-flood-unterminated":
			os.Stderr.WriteString(strings.Repeat("x", 2<<20))
		case "slow":
			await(os.Getenv("FAKE_GATE"))
		case "spawn-child-then-crash":
			os.Exit(2)
		case "crash-after-one":
			if count > 1 {
				os.Exit(2)
			}
		case "stale-id":
			send(map[string]any{"id": -1, "stale": true})
		case "fetch", "fetch-deaf", "fetch-late":
			// Asks for every held-back part and for one the call does not hold, then
			// replies with what it got.
			if mode == "fetch-late" {
				await(os.Getenv("FAKE_GATE"))
			}
			fetched := map[string]any{}
			names, _ := msg["more"].([]any)
			for _, n := range append(names, "absent") {
				send(map[string]any{"id": msg["id"], "fetch": n})
				if mode == "fetch-deaf" {
					block()
				}
				in.Scan()
				var answer map[string]any
				json.Unmarshal(in.Bytes(), &answer)
				if answer["id"] != msg["id"] || answer["fetch"] != n {
					fetched["mismatch"] = answer
				}
				fetched[n.(string)] = answer["value"]
			}
			_, inline := msg["big"]
			send(map[string]any{"id": msg["id"], "fetched": fetched, "more": msg["more"], "inline": inline})
			continue
		}
		fmt.Fprintln(out, "log line", count) // the script's own output, between replies
		send(map[string]any{"id": msg["id"], "count": count, "echo": msg["echo"]})
	}
}

// block never returns: it reads a pipe nothing writes to.
func block() {
	r, w, _ := os.Pipe()
	r.Read(make([]byte, 1))
	runtime.KeepAlive(w)
}

// await reads one byte from the FIFO at path: the fake script's step waits for the
// test to release it.
func await(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	f.Read(make([]byte, 1))
}

// signal writes one byte to the FIFO at path, telling the test a step was reached.
func signal(path string) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write([]byte{1})
}

// fifo makes a FIFO the test holds open read-write, so neither side's open blocks
// and the fake script never reads EOF from it while the test runs.
func fifo(t *testing.T) (string, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return path, f
}

// gate returns the FAKE_GATE setting and a release that lets the fake script take
// n more of the steps it holds.
func gate(t *testing.T) (string, func(n int)) {
	t.Helper()
	path, f := fifo(t)
	return "FAKE_GATE=" + path, func(n int) {
		if _, err := f.Write(bytes.Repeat([]byte{1}, n)); err != nil {
			t.Fatal(err)
		}
	}
}

// grandchild opens the FIFO at path that the fake script's grandchild holds, once
// the grandchild runs. Reading it to EOF waits for the grandchild to die.
func grandchild(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path) // returns once the script opens it to write
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
		t.Fatalf("the grandchild never started: %v", err)
	}
	return f
}

// holdFIFO makes the FIFO a fake script's grandchild holds, and its FAKE_HOLD setting.
func holdFIFO(t *testing.T) (path, env string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "hold")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, "FAKE_HOLD=" + path
}

// expiring is a context whose cancellation by context.DeadlineExceeded reads as its
// deadline passing.
type expiring struct{ context.Context }

func (e expiring) Err() error {
	if err := e.Context.Err(); err != nil && context.Cause(e.Context) == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return e.Context.Err()
}

// newExpiring returns a context whose deadline passes when expire is called.
func newExpiring(parent context.Context) (ctx context.Context, expire, cancel func()) {
	c, cancelCause := context.WithCancelCause(parent)
	return expiring{c}, func() { cancelCause(context.DeadlineExceeded) }, func() { cancelCause(context.Canceled) }
}

// deadline is one call timeout a worker set.
type deadline struct {
	d      time.Duration
	expire func()
}

// clock stands in for a worker's call timeouts. Each one passes only when the test
// expires it, and is sent on made as it is set: a call's wait for the script first,
// then, once it holds the script, its exchange.
type clock struct {
	made chan deadline
	// passed makes every timeout pass as it is set.
	passed atomic.Bool
	mu     sync.Mutex
	set    []func()
}

// useClock gives w's calls c's timeouts. Call it after registering w's Stop: every
// timeout passes at cleanup, before the stop, so a call a failed test leaves
// holding the script lets the stop through.
func useClock(t *testing.T, w *Worker) *clock {
	c := &clock{made: make(chan deadline, 256)}
	w.withTimeout = c.withTimeout
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, expire := range c.set {
			expire()
		}
	})
	return c
}

func (c *clock) withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, expire, cancel := newExpiring(parent)
	if c.passed.Load() {
		expire()
	}
	c.mu.Lock()
	c.set = append(c.set, expire)
	c.mu.Unlock()
	c.made <- deadline{d, expire}
	return ctx, cancel
}

// exchange returns the timeout of the exchange the call that done reports on
// begins once it holds the script, and fails if the call ends first.
func exchange(t *testing.T, c *clock, done <-chan error) deadline {
	t.Helper()
	select {
	case d := <-c.made:
		return d
	case err := <-done:
		t.Fatalf("the call ended before it held the script: %v", err)
		return deadline{}
	}
}

// callAsync runs one call and returns its error when it ends.
func callAsync(ctx context.Context, w *Worker, more map[string]any) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := w.Call(ctx, map[string]any{}, more)
		done <- err
	}()
	return done
}

func fake(t *testing.T, mode string, extraEnv ...string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Path: exe,
		// A race-built fake exits at once rather than after the race runtime's exit pause.
		Env:          append(append(os.Environ(), "FAKE_WORKER="+mode, "GORACE=atexit_sleep_ms=0"), extraEnv...),
		StartTimeout: time.Hour,
		CallTimeout:  time.Hour,
	}
}

// call makes one call, starting w first: a Call never waits for a start.
func call(t *testing.T, w *Worker, echo string) (map[string]any, Reply) {
	t.Helper()
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r, err := w.Call(context.Background(), map[string]any{"echo": echo}, nil)
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
	if _, err := w.Call(context.Background(), map[string]any{}, nil); !errors.Is(err, ErrStarting) {
		t.Fatalf("first Call = %v, want ErrStarting", err)
	}
	// The call began a start; once it is done, calls are answered.
	w.sem <- struct{}{}
	s, running := w.start, w.proc != nil
	w.unlock()
	if s == nil && !running {
		t.Fatal("the call started nothing")
	}
	if s != nil {
		<-s.done
	}
	if _, err := w.Call(context.Background(), map[string]any{}, nil); err != nil {
		t.Fatalf("Call after the background start = %v", err)
	}
}

func TestWorker_HandshakeFailures(t *testing.T) {
	for mode, tc := range map[string]struct {
		want  string
		start time.Duration
	}{
		// It never sends a handshake, so its start ends when the start timeout does.
		"no-handshake":   {"no handshake", time.Nanosecond},
		"wrong-protocol": {"protocol 99", 0},
		"exit-at-start":  {"no handshake", 0},
	} {
		t.Run(mode, func(t *testing.T) {
			cfg := fake(t, mode)
			if tc.start > 0 {
				cfg.StartTimeout = tc.start
			}
			w := New(cfg)
			t.Cleanup(w.Stop)
			_, err := w.Start(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start error = %v, want one mentioning %q", err, tc.want)
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
	w := New(fake(t, "hang"))
	t.Cleanup(w.Stop)
	c := useClock(t, w)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := callAsync(context.Background(), w, nil)
	<-c.made                      // the wait for the script
	exchange(t, c, done).expire() // the exchange, which the script never answers
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v", err)
	}
	// Backing off: refused immediately rather than started again.
	if _, err := w.Call(context.Background(), map[string]any{}, nil); !errors.Is(err, ErrBackoff) {
		t.Errorf("second Call error = %v, want ErrBackoff", err)
	}
}

// A start in progress, from a call or from Start, holds nothing a call waits for:
// calls made meanwhile fail at once, and Start waits for that same start.
func TestWorker_CallsDuringAStartFailAtOnce(t *testing.T) {
	for _, by := range []string{"call", "Start"} {
		t.Run("started by "+by, func(t *testing.T) {
			// The script signals once it runs, then handshakes only when released.
			startedPath, startedFIFO := fifo(t)
			startGate, startGateFIFO := fifo(t)
			w := New(fake(t, "plain", "FAKE_STARTED="+startedPath, "FAKE_START_GATE="+startGate))
			t.Cleanup(w.Stop)
			started := make(chan error, 1)
			if by == "call" {
				if _, err := w.Call(context.Background(), map[string]any{}, nil); !errors.Is(err, ErrStarting) {
					t.Fatalf("first Call = %v, want ErrStarting", err)
				}
			} else {
				go func() { _, err := w.Start(context.Background()); started <- err }()
			}
			if _, err := startedFIFO.Read(make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			for i := range 3 {
				_, err := w.Call(context.Background(), map[string]any{}, nil)
				if !errors.Is(err, ErrStarting) {
					t.Fatalf("call %d during the start = %v, want ErrStarting", i, err)
				}
			}
			if hs := w.Handshake(); hs.Cortex != 0 {
				t.Errorf("Handshake during the first start = %+v, want none yet", hs)
			}
			if _, err := startGateFIFO.Write([]byte{1}); err != nil {
				t.Fatal(err)
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
// each call within the call timeout is not killed for it: its state survives. A
// call's time in the queue does not count against its exchange.
func TestWorker_AQueueThatOutlastsACallerKeepsTheScript(t *testing.T) {
	gateEnv, release := gate(t)
	w := New(fake(t, "slow", gateEnv))
	t.Cleanup(w.Stop)
	c := useClock(t, w)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := callAsync(context.Background(), w, nil)
	<-c.made
	exchange(t, c, first) // first holds the script, which has not answered it
	second := callAsync(context.Background(), w, nil)
	secondQueued := <-c.made
	gaveUp := callAsync(context.Background(), w, nil)
	(<-c.made).expire()
	if err := <-gaveUp; err == nil {
		t.Fatal("a call whose time ran out in the queue succeeded")
	}
	release(1)
	if err := <-first; err != nil {
		t.Fatalf("first call = %v", err)
	}
	exchange(t, c, second) // second holds the script now
	secondQueued.expire()
	release(1)
	if err := <-second; err != nil {
		t.Fatalf("second call = %v: its wait in the queue cut its exchange short", err)
	}
	release(1)
	if got := callKept(t, w); got["count"] != float64(3) {
		t.Errorf("reply %v, want the third the script answered", got)
	}
}

// Under a queue longer than the call timeout, a message the script is sent is one its
// caller still waits for: the script answers no call nobody reads.
func TestWorker_AnOverloadedScriptAnswersOnlyCallersStillWaiting(t *testing.T) {
	gateEnv, release := gate(t)
	w := New(fake(t, "slow", gateEnv))
	t.Cleanup(w.Stop)
	c := useClock(t, w)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := callAsync(context.Background(), w, nil)
	<-c.made
	exchange(t, c, first) // first holds the script, which has not answered it
	// Callers queued behind it run out of time while they wait.
	var queued []<-chan error
	for range 8 {
		queued = append(queued, callAsync(context.Background(), w, nil))
	}
	for range queued {
		(<-c.made).expire()
	}
	for i, q := range queued {
		if err := <-q; err == nil {
			t.Fatalf("queued call %d succeeded after its time ran out", i)
		}
	}
	release(1)
	if err := <-first; err != nil {
		t.Fatalf("first call = %v", err)
	}
	// Callers whose time has run out by the time the script is free send nothing.
	c.passed.Store(true)
	for i := range 20 {
		if _, err := w.Call(context.Background(), map[string]any{}, nil); err == nil {
			t.Fatalf("call %d succeeded after its time ran out", i)
		}
	}
	c.passed.Store(false)
	release(1)
	if got := callKept(t, w); got["count"] != float64(2) {
		t.Errorf("reply %v: the script was sent calls whose callers had stopped waiting", got)
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
				if _, err := w.Call(context.Background(), map[string]any{}, nil); err != nil {
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
	all := buf.String()
	mu.Unlock()
	// Keep only this test's lines: a stopped worker from an earlier test can log its
	// last unterminated line into this logger after its Stop returns.
	var own []string
	for _, l := range strings.Split(all, "\n") {
		if strings.Contains(l, " script=s ") {
			own = append(own, l)
		}
	}
	out := strings.Join(own, "\n")
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
	hold, holdEnv := holdFIFO(t)
	w := New(fake(t, "spawn-child-then-crash", holdEnv))
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := grandchild(t, hold)
	w.Call(context.Background(), map[string]any{}, nil) // the script exits on it
	if _, err := io.ReadAll(g); err != nil {
		t.Fatal(err)
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
	w.Call(context.Background(), map[string]any{}, nil)
	if n := late.Load(); n != 0 {
		t.Errorf("signalled a reaped child's process group %d times", n)
	}
}

// A caller that goes away mid-call stops waiting without killing the script.
func TestWorker_ACallerThatGivesUpKeepsTheScript(t *testing.T) {
	// Each returns the caller's context, what makes it give up, and its cancel.
	for name, tc := range map[string]struct {
		mk     func() (ctx context.Context, giveUp, cancel func())
		before bool
	}{
		"cancelled mid-call": {mk: func() (context.Context, func(), func()) {
			ctx, c := context.WithCancel(context.Background())
			return ctx, c, c
		}},
		"its own deadline": {mk: func() (context.Context, func(), func()) {
			return newExpiring(context.Background())
		}},
		"cancelled before it": {before: true, mk: func() (context.Context, func(), func()) {
			ctx, c := context.WithCancel(context.Background())
			c()
			return ctx, c, c
		}},
	} {
		t.Run(name, func(t *testing.T) {
			gateEnv, release := gate(t)
			w := New(fake(t, "slow", gateEnv))
			t.Cleanup(w.Stop)
			c := useClock(t, w)
			if _, err := w.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			for range 5 {
				ctx, giveUp, cancel := tc.mk()
				done := callAsync(ctx, w, nil)
				<-c.made // the wait for the script
				if !tc.before {
					exchange(t, c, done) // the call holds the script, which has not answered it
					giveUp()
				}
				err := <-done
				cancel()
				if err == nil {
					t.Fatal("a call whose caller gave up succeeded")
				}
				if !tc.before {
					release(1) // the script answers the call nobody waits for
					continue
				}
				// The call has let go of the script.
				select {
				case w.sem <- struct{}{}:
					w.unlock()
				default:
					t.Fatal("a call whose caller had already gone holds the script")
				}
			}
			release(1)
			callKept(t, w)
		})
	}
}

// sameChild reports whether a and b drive one child process.
func sameChild(a, b *Worker) bool { return a.child == b.child }

// callKept calls w and fails unless the running script answers it.
func callKept(t *testing.T, w *Worker) map[string]any {
	t.Helper()
	r, err := w.Call(context.Background(), map[string]any{}, nil)
	if err != nil {
		t.Fatalf("the next call = %v: the script was killed", err)
	}
	var got map[string]any
	json.Unmarshal(r.Line, &got)
	return got
}

func TestWorker_TimeoutKillsTheWholeProcessGroup(t *testing.T) {
	hold, holdEnv := holdFIFO(t)
	w := New(fake(t, "spawn-child", holdEnv))
	t.Cleanup(w.Stop)
	c := useClock(t, w)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := grandchild(t, hold)
	done := callAsync(context.Background(), w, nil)
	<-c.made
	exchange(t, c, done).expire()
	if err := <-done; err == nil {
		t.Fatal("call to a hanging script succeeded")
	}
	if _, err := io.ReadAll(g); err != nil {
		t.Fatal(err)
	}
}

func TestWorker_CrashThenRestartAfterBackoff(t *testing.T) {
	w := New(fake(t, "crash-after-one"))
	t.Cleanup(w.Stop)
	var clockMu sync.Mutex
	clock := time.Now()
	w.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	call(t, w, "first")
	if _, err := w.Call(context.Background(), map[string]any{}, nil); err == nil {
		t.Fatal("call to a crashing script succeeded")
	}
	if _, err := w.Call(context.Background(), map[string]any{}, nil); !errors.Is(err, ErrBackoff) {
		t.Fatalf("call during backoff = %v, want ErrBackoff", err)
	}
	clockMu.Lock()
	clock = clock.Add(minBackoff + time.Millisecond)
	clockMu.Unlock()
	// After the backoff the restart runs in the background: the call that finds no
	// child fails at once instead of waiting for it.
	if _, err := w.Call(context.Background(), map[string]any{}, nil); !errors.Is(err, ErrStarting) {
		t.Fatalf("call after backoff = %v, want ErrStarting", err)
	}
	// A new process: its count starts over, which is the state loss a crash costs.
	// Start waits for the restart that call began.
	if got, _ := call(t, w, "again"); got["count"] != float64(1) {
		t.Errorf("after restart, reply %v", got)
	}
}

func TestWorker_StopRefusesCalls(t *testing.T) {
	w := New(fake(t, "plain"))
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	call(t, w, "x")
	w.Stop()
	if _, err := w.Call(context.Background(), map[string]any{}, nil); err == nil {
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
	loginPathTimeout = time.Hour
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
// while that process lives: with its PATH when it printed one, empty when it hung
// and its probe was ended. The process waits on a FIFO the test releases on
// cleanup, so the exited shell's probe ends only through loginPathWaitDelay.
func TestLoginPath_BackgroundChildHoldingStdout(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, tc := range map[string]struct {
		body, want string
		hangs      bool
	}{
		"printed then exited": {"read _ < \"$HOLD\" &\nshift\nPATH=/from/login exec " + sh + " \"$@\"\n", "/from/login", false},
		"hung":                {"read _ < \"$HOLD\" &\necho > \"$STARTED\"\nread _ < \"$HOLD\"\n", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// fifo makes a FIFO, opened read-write so opening it neither blocks nor
			// sends EOF; closing it on cleanup does.
			fifo := func(name string) (string, *os.File) {
				path := filepath.Join(dir, name)
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { f.Close() })
				return path, f
			}
			hold, _ := fifo("hold")
			started, startedR := fifo("started")
			shell := filepath.Join(dir, "shell")
			os.WriteFile(shell, []byte("#!"+sh+"\n"+tc.body), 0o700)
			t.Setenv("SHELL", shell)
			t.Setenv("HOLD", hold)
			t.Setenv("STARTED", started)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if tc.hangs {
				// The probe ends once the shell is running and hung.
				go func() {
					if _, err := startedR.Read(make([]byte, 1)); err == nil {
						cancel()
					}
				}()
			}
			loginPathMu.Lock()
			loginPathVal, loginPathTried = "", time.Time{}
			old := loginPathContext
			loginPathContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
			loginPathMu.Unlock()
			t.Cleanup(func() {
				loginPathMu.Lock()
				defer loginPathMu.Unlock()
				loginPathVal, loginPathTried, loginPathContext = "", time.Now(), old
			})
			got := loginPath()
			if tc.hangs && ctx.Err() == nil {
				t.Error("loginPath returned before the shell signalled it was hung")
			}
			if got != tc.want {
				t.Errorf("loginPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// exitOf returns a channel closed when w's running child exits.
func exitOf(t *testing.T, w *Worker) <-chan struct{} {
	t.Helper()
	w.sem <- struct{}{}
	defer w.unlock()
	if w.proc == nil {
		t.Fatal("no child is running")
	}
	return w.proc.done
}

// waitStopped waits for w to refuse calls; a released worker stops in the
// background. exited is w's child's exit, which a stop brings about while it holds
// the worker, so the next call sees the stop.
func waitStopped(t *testing.T, w *Worker, exited <-chan struct{}, what string) {
	t.Helper()
	<-exited
	if _, err := w.Call(context.Background(), map[string]any{}, nil); !errors.Is(err, errStopped) {
		t.Fatalf("%s still answers: %v", what, err)
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
	exited := exitOf(t, a)
	p.Release(two)
	waitStopped(t, a, exited, "a worker no build holds")
}

// callBoundBy makes one call on w and fails unless both its timeouts, the wait for
// the script and the exchange, are want.
func callBoundBy(t *testing.T, c *clock, w *Worker, want time.Duration, what string) {
	t.Helper()
	if _, err := w.Call(context.Background(), map[string]any{}, nil); err != nil {
		t.Fatalf("%s: call = %v", what, err)
	}
	for range 2 {
		if d := (<-c.made).d; d != want {
			t.Errorf("%s: the call was bounded by %v, want %v", what, d, want)
		}
	}
}

// A reused worker takes the call timeout of the build that acquired it, so an edited
// timeout applies to the running script without restarting it.
func TestPool_AReusedWorkerTakesTheNewCallTimeout(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	cfg.CallTimeout = 10 * time.Second // timing: the fake clock records it
	a := p.Acquire(p.NewHolder(), cfg, "h1")
	t.Cleanup(a.Stop)
	c := useClock(t, a)
	if _, err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.CallTimeout = 200 * time.Millisecond // timing: the fake clock records it
	b := p.Acquire(p.NewHolder(), cfg, "h1")
	if !sameChild(a, b) {
		t.Fatal("a changed timeout restarted the script")
	}
	callBoundBy(t, c, b, 200*time.Millisecond, "the new build")
}

// A build's timeout governs that build's calls only: one configured and then refused,
// or one still draining beside a newer one, leaves the other's calls as they were.
func TestPool_ABuildsCallTimeoutStaysWithThatBuild(t *testing.T) {
	p := NewPool()
	cfg := fake(t, "plain")
	running := p.NewHolder()
	a := p.Acquire(running, cfg, "h1")
	t.Cleanup(a.Stop)
	c := useClock(t, a)
	if _, err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Activate(running)
	cfg.CallTimeout = 50 * time.Millisecond // timing: the fake clock records it
	refused := p.NewHolder()
	p.Acquire(refused, cfg, "h1") // configured, never started
	callBoundBy(t, c, a, time.Hour, "the running build beside a refused one")
	next := p.NewHolder()
	p.Acquire(next, cfg, "h1")
	p.Activate(next) // the old build drains beside it
	callBoundBy(t, c, a, time.Hour, "the draining build")
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
	exited := exitOf(t, old)
	p.Release(one)
	waitStopped(t, old, exited, "the old version")
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
	exited := exitOf(t, orphan)

	next := p.NewHolder()
	p.Acquire(next, cfg, "h1")
	p.Activate(next)
	waitStopped(t, orphan, exited, "a refused build's worker")
	call(t, kept, "the running build's worker")
}

// The call deadline bounds the whole exchange, the write included. A script that
// stops reading fills the pipe; a body larger than the pipe buffer — which every
// inference request is — must still time out rather than block the request.
func TestWorker_TimeoutCoversTheWrite(t *testing.T) {
	w := New(fake(t, "deaf"))
	t.Cleanup(w.Stop)
	c := useClock(t, w)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.Call(context.Background(), map[string]any{"body": strings.Repeat("x", 2<<20)}, nil)
		done <- err
	}()
	<-c.made
	exchange(t, c, done).expire() // the exchange, whose write the script never reads
	if err := <-done; err == nil {
		t.Fatal("a call to a script that does not read succeeded")
	}
	// Neither the next call nor Stop waits behind the stuck write.
	if _, err := w.Call(context.Background(), map[string]any{}, nil); !errors.Is(err, ErrBackoff) {
		t.Errorf("next call = %v, want ErrBackoff", err)
	}
	w.Stop()
}

// A held-back part reaches the child only when it asks, named in "more" and
// answered on its stdin; a name the call does not hold is answered with null.
func TestWorker_FetchesWhatTheCallHoldsBack(t *testing.T) {
	w := New(fake(t, "fetch"))
	t.Cleanup(w.Stop)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := map[string]any{"echo": "x"}
	r, err := w.Call(context.Background(), msg, map[string]any{"big": "body", "parsed": map[string]int{"n": 1}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Fetched map[string]any `json:"fetched"`
		More    []string       `json:"more"`
		Inline  bool           `json:"inline"`
	}
	if err := json.Unmarshal(r.Line, &got); err != nil {
		t.Fatalf("reply %q: %v", r.Line, err)
	}
	if !slices.Equal(got.More, []string{"big", "parsed"}) || got.Inline {
		t.Errorf("more = %v, inline = %v", got.More, got.Inline)
	}
	want := map[string]any{"big": "body", "parsed": map[string]any{"n": float64(1)}, "absent": nil}
	if fmt.Sprint(got.Fetched) != fmt.Sprint(want) {
		t.Errorf("fetched %v, want %v", got.Fetched, want)
	}
	if _, ok := msg["more"]; ok {
		t.Error("Call left more in the caller's message")
	}
}

// encodeCount counts how often it is encoded.
type encodeCount struct{ n *atomic.Int32 }

func (c encodeCount) MarshalJSON() ([]byte, error) {
	c.n.Add(1)
	return []byte(`"part"`), nil
}

// A held-back part is the caller's live request, which the plugins after this one
// change once the caller stops waiting. So a fetch that comes after Call has
// returned gets null, and the part is never encoded.
func TestWorker_FetchAfterTheCallerGaveUpGetsNull(t *testing.T) {
	gateEnv, release := gate(t)
	w := New(fake(t, "fetch-late", gateEnv)) // fetches only when released
	t.Cleanup(w.Stop)
	c := useClock(t, w)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var encoded atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := callAsync(ctx, w, map[string]any{"body": encodeCount{&encoded}})
	<-c.made
	exchange(t, c, done) // the call holds the script, which has not fetched yet
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a call whose caller gave up succeeded")
	}
	release(2) // the late fetch, then the next call
	// This call waits for the child to reply to the first, so the late fetch is done.
	if _, err := w.Call(context.Background(), map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if n := encoded.Load(); n != 0 {
		t.Errorf("the part was encoded %d times after its caller gave up", n)
	}
}

// The call deadline covers the answer to a fetch: a script that asks for a part
// larger than the pipe buffer and then stops reading must not block the request.
func TestWorker_TimeoutCoversAFetch(t *testing.T) {
	w := New(fake(t, "fetch-deaf"))
	t.Cleanup(w.Stop)
	c := useClock(t, w)
	if _, err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	part := askedFor{asked: make(chan struct{}), value: strings.Repeat("x", 2<<20)}
	done := callAsync(context.Background(), w, map[string]any{"body": part})
	<-c.made
	ex := exchange(t, c, done)
	<-part.asked // the script asked for the part and reads nothing more
	ex.expire()
	if err := <-done; err == nil {
		t.Fatal("a call to a script that stopped reading succeeded")
	}
}

// askedFor is a held-back part that reports when it is fetched.
type askedFor struct {
	asked chan struct{}
	value string
}

func (a askedFor) MarshalJSON() ([]byte, error) {
	close(a.asked)
	return json.Marshal(a.value)
}

package main

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The supervisor never parsed config before this, so it learns the address the same
// way it learns everything else: from the argv it is about to hand the child. Both
// spellings and both forms, because main.go uses flag.String and so accepts all four.
func TestConfigPathFromArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"separate single dash", []string{"-config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"separate double dash", []string{"--config", "/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined single dash", []string{"-config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"joined double dash", []string{"--config=/etc/c.yaml"}, "/etc/c.yaml"},
		{"among others", []string{"-local", "--config", "/etc/c.yaml", "-ca-dir", "/d"}, "/etc/c.yaml"},
		{"absent", []string{"-local"}, ""},
		{"dangling value", []string{"-local", "--config"}, ""},
		// Repeated: flag.Parse takes the last, so the child reads the last. Binding the
		// address named by the first would hold a port the child never asked for.
		{"repeated, last wins", []string{"--config", "/etc/a.yaml", "--config", "/etc/b.yaml"}, "/etc/b.yaml"},
		{"repeated across spellings", []string{"-config=/etc/a.yaml", "--config", "/etc/b.yaml"}, "/etc/b.yaml"},
		{"repeated then dangling", []string{"--config", "/etc/a.yaml", "--config"}, ""},
		// A value the flag before it consumed is not itself a flag. flag.Parse reads
		// this as -config with the literal value "--config=/etc/b.yaml".
		{"a value that looks like the flag", []string{"--config", "--config=/etc/b.yaml"}, "--config=/etc/b.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configPathFromArgs(tc.args); got != tc.want {
				t.Errorf("configPathFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// Two obligations in one table. The address must be the one the CHILD will bind, which
// takes the child's whole resolution sequence and not just a config read; and every miss
// must still yield an address rather than an error, because the whole point of the
// fallback is to speak when the proxy cannot start and "the config will not load" is one
// of the reasons it cannot start.
func TestSupervisorForwardAddr_ResolvesTheAddressTheChildWillBind(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("mode: proxy-sidecar\nlistener:\n  forward_proxy_addr: 127.0.0.1:48123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("listener: [this is not a mapping\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The two configs where Load alone disagrees with the child, which runs Load AND
	// ApplyPreset: bind_loopback_only localises a bare port, and an omitted
	// forward_proxy_addr gets the proxy-sidecar preset. Reading the address before the
	// preset put the fallback on every interface in the first case and on a port the
	// child never binds in the second.
	loopback := filepath.Join(dir, "loopback.yaml")
	if err := os.WriteFile(loopback, []byte("mode: proxy-sidecar\nlistener:\n  bind_loopback_only: true\n  forward_proxy_addr: \":48155\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	presetOnly := filepath.Join(dir, "preset.yaml")
	if err := os.WriteFile(presetOnly, []byte("mode: proxy-sidecar\nlistener:\n  roles: [forward]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No `mode:` at all. The child defaults it (main.go:500) before applying the preset,
	// so it lands on :8081 -- and a fallback that applied the preset without the default
	// would get nothing from it, because ApplyPreset switches on the mode.
	noMode := filepath.Join(dir, "no-mode.yaml")
	if err := os.WriteFile(noMode, []byte("listener:\n  roles: [forward]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reverse-only: the preset fills an address only for an active role, so the child
	// binds no forward proxy and there is nothing for the fallback to stand in for. It
	// still has to answer with an address rather than an empty string.
	reverseOnly := filepath.Join(dir, "reverse.yaml")
	if err := os.WriteFile(reverseOnly, []byte("mode: proxy-sidecar\nlistener:\n  roles: [reverse]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"config names an address", []string{"--config", good}, "127.0.0.1:48123"},
		{"config will not parse", []string{"--config", broken}, fallbackDefaultAddr},
		{"config is not there", []string{"--config", filepath.Join(dir, "absent.yaml")}, fallbackDefaultAddr},
		{"no config flag at all", []string{"-local"}, fallbackDefaultAddr},
		{"loopback-only localises a bare port", []string{"--config", loopback}, "127.0.0.1:48155"},
		{"preset fills an omitted address", []string{"--config", presetOnly}, ":8081"},
		{"an omitted mode is defaulted first", []string{"--config", noMode}, ":8081"},
		{"reverse-only leaves no forward address", []string{"--config", reverseOnly}, fallbackDefaultAddr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorForwardAddr(supervisorConfig(configPathFromArgs(tc.args))); got != tc.want {
				t.Errorf("supervisorForwardAddr(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// The exact bytes, because two different fields of this response are read by two
// different clients and a plausible-looking refactor can drop either without any other
// test noticing. Measured against real clients: Claude Code prints the body, Go's
// net/http returns the reason phrase as the error, curl discards both.
func TestFallbackRefusal_Bytes(t *testing.T) {
	got := string(fallbackRefusal)
	want := "HTTP/1.1 502 " + fallbackReason + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(fallbackBody)) + "\r\n" +
		"Connection: close\r\n" +
		"Proxy-Connection: close\r\n" +
		"\r\n" + fallbackBody
	if got != want {
		t.Errorf("refusal =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(fallbackBody, "\n") {
		t.Error("the body spans lines; a trailing newline renders as a stray '.' inside Claude Code's error framing")
	}
	if !strings.Contains(fallbackBody, "agentop service start") {
		t.Error("the body does not name the command that fixes it, which is the whole point of it")
	}
}

// A Go client reads the reason phrase and nothing else. This is the half that would be
// lost by rewriting the response as http.Error, which always writes the canonical
// status text -- a change that would still pass every other test here.
func TestFallbackServer_GoClientSeesTheReasonPhrase(t *testing.T) {
	addr := startFallback(t)

	pu, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
		Timeout:   5 * time.Second,
	}
	_, err = c.Get("https://cortex.invalid/") //nolint:noctx // bounded by Timeout
	if err == nil {
		t.Fatal("the request succeeded; the fallback must refuse every connection")
	}
	if !strings.Contains(err.Error(), fallbackReason) {
		t.Errorf("go client error = %v, want it to carry the reason phrase %q", err, fallbackReason)
	}
}

// Claude Code reads the body. Asserted over a raw CONNECT because that is what an agent
// routed at an HTTPS endpoint actually sends.
func TestFallbackServer_ConnectGetsTheBody(t *testing.T) {
	addr := startFallback(t)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.HasPrefix(got, "HTTP/1.1 502 "+fallbackReason) {
		t.Errorf("status line = %q, want the 502 and the reason phrase", strings.SplitN(got, "\r\n", 2)[0])
	}
	if !strings.HasSuffix(got, fallbackBody) {
		t.Errorf("response does not end in the body; got %q", got)
	}
}

// close() has to actually free the address, because the supervisor closes the fallback
// precisely so the child can bind it. A close that leaked the port would turn this
// feature into the port conflict of #1122 on every single restart.
func TestFallbackServer_CloseFreesTheAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fallbackServer{}
	fb.openOn(ln)
	addr := ln.Addr().String()

	fb.close()

	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the address is still held after close: %v — the child could not bind it", err)
	}
	_ = again.Close()

	fb.close() // idempotent: the supervisor closes before every start, including the first
}

// startFallback opens a fallback server on a port the kernel picks and returns its
// address. openOn rather than open so there is no window between finding a free port
// and binding it.
func startFallback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fallbackServer{}
	fb.openOn(ln)
	addr := ln.Addr().String()
	t.Cleanup(fb.close)
	return addr
}

// The sequence the supervisor runs, asserted as a property rather than by driving
// runSupervisor, which execs a real binary: the fallback must be closed while a child
// holds the address and open again once it does not. Binding the address in between is
// the stand-in for the child.
func TestFallbackServer_YieldsTheAddressToAChildAndTakesItBack(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	fb := &fallbackServer{}
	fb.openOn(ln)

	// What the supervisor does before every cmd.Start().
	fb.close()
	child, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the child could not bind %s after the fallback closed: %v", addr, err)
	}

	// The bind above is the assertion: it succeeds only because the fallback is not
	// listening, which is what the supervisor must guarantee before every cmd.Start().
	// Release it again to stand in for the child exiting.
	_ = child.Close()

	// What the supervisor does once the child exits.
	fb.open(addr)
	t.Cleanup(fb.close)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("nothing is listening on %s after the child exited: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("CONNECT x:443 HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), fallbackReason) {
		t.Errorf("after the child exited the address answers %q, want the refusal", string(b))
	}
}

// The request line reaches ~/.cortex/proxy.log, and an absolute-form plain-HTTP request
// carries a query string that can carry an API key -- the repo's standing rule is to
// record the address but never the query. CONNECT lines have no query and must survive
// intact, since they are the only thing identifying a refused tunnel.
func TestRequestLineForLog(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"connect passes through", "CONNECT api.anthropic.com:443 HTTP/1.1\r\n", "CONNECT api.anthropic.com:443 HTTP/1.1"},
		{"plain path passes through", "GET /v1/models HTTP/1.1\r\n", "GET /v1/models HTTP/1.1"},
		{
			"query is redacted",
			"GET http://h/v1/m?api_key=sk-secret&x=1 HTTP/1.1\r\n",
			"GET http://h/v1/m?<redacted>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := requestLineForLog(tc.in)
			if got != tc.want {
				t.Errorf("requestLineForLog(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "sk-secret") {
				t.Error("the logged line still carries the secret")
			}
		})
	}
}

// A request bigger than bufio's buffer leaves unread bytes behind, and closing on those
// sends RST where the client may lose the response it was about to read. Asserted with a
// body well past fallbackMaxRequestLine, which is also the cap that keeps a newline-free
// client from making the supervisor allocate without bound.
func TestFallbackServer_LargeRequestStillGetsTheResponse(t *testing.T) {
	addr := startFallback(t)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	body := strings.Repeat("x", 4*fallbackMaxRequestLine)
	req := "POST http://h/v1/messages HTTP/1.1\r\nHost: h\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	// Half-close so the drain ends on EOF rather than on fallbackDrainTimeout. The
	// condition under test is unread bytes in the SERVER's receive queue, which a client
	// FIN does not flush, so this changes nothing about what is being asserted -- it only
	// removes a dependency on 32 KiB crossing loopback inside 250ms.
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("reading the response failed (a reset would look like this): %v", err)
	}
	if !strings.HasPrefix(string(b), "HTTP/1.1 502 "+fallbackReason) {
		t.Errorf("response = %q, want the 502 and the reason phrase", string(b))
	}
}

// The cap on the request read, asserted the only way that distinguishes it from no cap:
// a client that writes past it and then goes quiet WITHOUT ever sending a newline. With
// the cap the LimitReader hits EOF and the refusal goes out at once; without it
// ReadString blocks for the whole of fallbackIOTimeout while bufio grows its buffer, and
// the supervisor is the one process here that nothing restarts before the next login.
// The two-second read deadline is what fails in that case -- deliberately far below the
// five-second timeout it would otherwise wait out, so this is not a tight race.
func TestFallbackServer_UnterminatedRequestIsCappedNotAwaited(t *testing.T) {
	addr := startFallback(t)

	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Write([]byte(strings.Repeat("x", 2*fallbackMaxRequestLine))); err != nil {
		t.Fatal(err)
	}

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, len(fallbackRefusal))
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatalf("no refusal within 2s of an unterminated request: %v — the read is waiting for a newline that is not coming", err)
	}
	if !strings.HasPrefix(string(b), "HTTP/1.1 502 "+fallbackReason) {
		t.Errorf("response = %q, want the 502 and the reason phrase", string(b))
	}
}

// refuse has to route the request line through requestLineForLog, not log it raw. Without
// this nothing connects the helper to its one call site: reverting that line leaves
// requestLineForLog fully tested, unused, and query strings back in ~/.cortex/proxy.log.
func TestFallbackServer_LogsNoQueryString(t *testing.T) {
	var buf syncBuffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	addr := startFallback(t)
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("GET http://h/v1/m?api_key=sk-secret HTTP/1.1\r\nHost: h\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// refuse logs before it writes, so having the response in hand means the log line is
	// already in the buffer.
	if _, err := io.ReadFull(c, make([]byte, len(fallbackRefusal))); err != nil {
		t.Fatal(err)
	}

	logged := buf.String()
	if strings.Contains(logged, "sk-secret") {
		t.Errorf("the log carries the query string, which can carry an API key:\n%s", logged)
	}
	if !strings.Contains(logged, "?<redacted>") {
		t.Errorf("the log does not show the redaction, so the line may not be going through requestLineForLog at all:\n%s", logged)
	}
}

// syncBuffer is a log sink safe to read while a server goroutine writes to it. The
// bind-failure test below needs no such thing -- open logs on the test's own goroutine --
// but refuse logs on the goroutine serving the connection.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A port conflict is the one condition where this bind fails every restart cycle: the
// child cannot take the address and neither can the fallback. Repeating the same WARN
// per cycle is what supervise.go was rewritten to stop doing, so it is said once and
// then at DEBUG -- asserted here by filtering the logger at WARN and counting.
func TestFallbackServer_BindFailureIsExplainedOnceNotEveryCycle(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	addr := held.Addr().String()

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	fb := &fallbackServer{}
	for range 4 {
		fb.open(addr) // every one of these fails: `held` has the address
	}
	t.Cleanup(fb.close)

	if got := strings.Count(buf.String(), "could not open the fallback listener"); got != 1 {
		t.Errorf("the bind failure was logged at WARN %d times across 4 cycles, want 1:\n%s", got, buf.String())
	}
}

// Once per RUN of failures, not once per supervisor lifetime. The difference is the case
// that matters: without the reset, a transient failure spends the WARN and a persistent
// conflict afterwards is DEBUG-only, so nothing in proxy.log names the condition the
// operator has to fix. A successful bind in between is the signal that the next failure
// is a new condition.
func TestFallbackServer_BindFailureIsExplainedAgainAfterASuccess(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := held.Addr().String()

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	fb := &fallbackServer{}
	t.Cleanup(fb.close)

	fb.open(addr) // fails: held has the address. WARN 1.
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	fb.open(addr) // succeeds, which re-arms the WARN
	fb.close()

	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("could not re-take %s to stage the second conflict: %v", addr, err)
	}
	defer func() { _ = again.Close() }()

	fb.open(addr) // fails again, and this is a new condition. WARN 2.

	if got := strings.Count(buf.String(), "could not open the fallback listener"); got != 2 {
		t.Errorf("the second run of failures was logged at WARN %d times, want 2 total — a successful bind in between must re-arm it:\n%s", got, buf.String())
	}
}

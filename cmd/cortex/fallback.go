package main

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/config"
)

// fallbackDefaultAddr is where the forward proxy is pinned for a local install. It is
// the answer whenever the config cannot give one, which is not a rare path: a config
// that will not load is one of the reasons the proxy cannot start, and that is exactly
// when the fallback has something to say. Kept equal to the default in
// cmd/agentop/setup_step_service.go, which picks it for the same reason.
const fallbackDefaultAddr = "127.0.0.1:47600"

// configPathFromArgs finds the -config value in the arguments the supervisor is about
// to hand the child. main.go declares it with flag.String, so all four spellings are
// accepted and all four are read here.
//
// Last occurrence wins, because that is what flag.Parse does with a repeated flag: the
// child would read the last one, and a fallback that bound the address named by the
// first would hold a port the child never wanted. Unreachable from the installed
// service, whose plist passes -config exactly once -- but silently disagreeing with the
// flag package is a trap for the next reader, and agreeing with it costs three lines.
func configPathFromArgs(args []string) string {
	path := ""
	// A value consumed by the flag before it is not itself a flag -- `--config
	// --config=/x` names the file "--config=/x", which is what flag.Parse does with it.
	// Without this the second element would be read as a flag naming /x instead.
	consumed := -1
	for i, a := range args {
		if i == consumed {
			continue
		}
		switch {
		case a == "-config" || a == "--config":
			if i+1 < len(args) {
				path = args[i+1]
				consumed = i + 1
			} else {
				path = "" // dangling; flag.Parse would reject it outright
			}
		case strings.HasPrefix(a, "-config="):
			path = strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			path = strings.TrimPrefix(a, "--config=")
		}
	}
	return path
}

// supervisorConfig loads the config at path once, for the fallback address and for
// praxis mode, or returns nil when there is no path or it will not load. Every miss
// is direct mode with the default fallback address, for the reason on
// fallbackDefaultAddr.
//
// The three steps below mirror the child's own sequence -- Load, coerce the mode,
// ApplyPreset (main.go:491, :500, :501) -- because only all three yield the address the
// child will actually bind. Load alone diverges from it three ways. A config carrying
// bind_loopback_only with a bare ":47600" has the child on 127.0.0.1 while the fallback
// would take every interface: an unauthenticated listener on the LAN of a machine whose
// config asked for loopback only. A config that omits forward_proxy_addr has the child on
// the preset's :8081 while the fallback would sit on 47600, answering for a proxy that is
// running. And ApplyPreset fills a listener address only for the mode it is handed, so
// omitting the coercion would leave a config with no `mode:` at all in that second case --
// the child defaults the mode and the preset fills :8081; the fallback would not.
//
// Coercing rather than honouring a foreign mode is faithful to the child, which refuses
// anything but proxy-sidecar (main.go:495) and so binds nothing at all. The fallback
// still has somewhere useful to be: whatever the config pinned, else the laptop default,
// which is where a client routed at this install points either way.
func supervisorConfig(path string) *config.Config {
	if path == "" {
		return nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		// WARN, not DEBUG: this runs once per supervisor lifetime, outside the restart
		// loop, so there is no flood to suppress -- and it is the one line saying the
		// fallback is listening somewhere other than where the config asked.
		slog.Warn("supervisor: cannot read the config for the fallback address; using the default",
			"config", path, "addr", fallbackDefaultAddr, "error", err)
		return nil
	}
	cfg.Mode = config.ModeProxySidecar
	config.ApplyPreset(cfg)
	return cfg
}

// supervisorForwardAddr is the address the fallback listener binds: the forward proxy's
// own, so a client routed at it reaches the fallback when no child holds it. cfg is
// supervisorConfig's, nil when it did not load.
func supervisorForwardAddr(cfg *config.Config) string {
	// Still reachable after the preset, which fills an address only for an ACTIVE role
	// (presets.go:29): a reverse-only config leaves this empty, and the child then binds
	// no forward proxy at all. Nothing is routed at the fallback in that case, so where
	// it listens does not much matter -- but it has to listen somewhere.
	if cfg == nil || cfg.Listener.ForwardProxyAddr == "" {
		return fallbackDefaultAddr
	}
	return cfg.Listener.ForwardProxyAddr
}

// The refusal is fail-closed by design: traffic does not reach the internet unobserved
// just because the proxy is down. What it buys over a refused connection is a client
// that can say why, and that depends on which field the client reads -- measured, not
// assumed:
//
//	curl 8.7.1        "CONNECT tunnel failed, response 502"   neither field
//	Go net/http       the reason phrase, as the error          reason phrase
//	Claude Code       prints it inside "API Error: 502 ..."    body
//
// So both are set. This is also why the response is written as bytes rather than through
// http.Error: Go's net/http always writes the canonical status text for the code and
// offers no way to set a reason phrase, which would silently cost the Go half.
//
// The body is one line with no trailing newline. Claude Code wraps it in framing of its
// own, and a trailing newline landed a stray "." on a line by itself.
const (
	fallbackReason = "Cortex proxy is not serving"
	fallbackBody   = "Cortex is routed in this environment (HTTPS_PROXY) but is not serving — run: agentop service start"
)

// fallbackIOTimeout bounds a connection the fallback is answering. Without it a client
// that opens and never writes would hold a goroutine for as long as the proxy stays
// down, which can be the whole of a long port conflict.
const fallbackIOTimeout = 5 * time.Second

// fallbackDrainTimeout bounds the post-response drain, which exists only to make the
// close a FIN. Short on purpose -- see refuse.
const fallbackDrainTimeout = 250 * time.Millisecond

// fallbackMaxRequestLine caps how much of the request is read. bufio's ReadString grows
// its buffer until it finds the delimiter, so without this a client streaming
// newline-free bytes for the whole of fallbackIOTimeout could make the supervisor
// allocate without bound -- and the supervisor is the one process here that nothing
// restarts before the next login, so an OOM in it costs the service, not just the
// diagnostic. 8 KiB is well past any real request line.
const fallbackMaxRequestLine = 8 << 10

var fallbackRefusal = []byte("HTTP/1.1 502 " + fallbackReason + "\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Length: " + strconv.Itoa(len(fallbackBody)) + "\r\n" +
	"Connection: close\r\n" +
	"Proxy-Connection: close\r\n" +
	"\r\n" + fallbackBody)

// fallbackServer answers every connection with fallbackRefusal. The supervisor holds it
// open only while no child is serving, so in steady state it is closed and a healthy
// install is unchanged: no extra hop, no second process, no new port.
type fallbackServer struct {
	mu                sync.Mutex
	ln                net.Listener
	warnedBindFailure bool // true once a bind failure has been logged at WARN
}

// open binds addr and starts answering. A bind that fails is logged and otherwise
// ignored: restarting the child is the supervisor's actual job, and losing the
// diagnostic listener must never stop it. The usual cause is the previous child not
// having released the port yet, which the next iteration retries anyway.
//
// During a port conflict the child exits immediately and this bind fails for the same
// reason, so this runs every restart cycle -- the same per-restart WARN flood that
// runSupervisor's own restart line was rewritten to quiet (see "quiet" there). The
// child already logs an ERROR naming the address and its holder on every attempt, so
// this would be a third per-cycle message carrying no new information. State it once
// at WARN; every bind failure after that logs the same message and fields at DEBUG.
//
// Reset on a SUCCESSFUL bind (in openOn), which makes this once per RUN of failures.
// Not in close(), the obvious-looking place: close() runs immediately before every
// cmd.Start(), so resetting there would re-arm the WARN once per cycle and restore
// exactly the flood this exists to stop. Resetting on success cannot, because the
// condition that makes this log at all -- another process holding the address -- is
// also what stops the bind ever succeeding to reset it. Nothing else plausibly makes
// it alternate: a child's listening socket is released the moment cmd.Wait reaps the
// process (a LISTEN socket never enters TIME_WAIT), so the previous child is not a
// source of intermittent failure. And it closes the gap the alternative leaves, which
// is the case that matters: a transient failure spending the WARN, after which a
// PERSISTENT conflict is DEBUG-only and nothing in proxy.log names it. No recovery line
// covers that -- restartPolicy.healthy only reports recovered when blocked > 0, which a
// fallback bind failure does not touch.
func (f *fallbackServer) open(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		f.mu.Lock()
		first := !f.warnedBindFailure
		f.warnedBindFailure = true
		f.mu.Unlock()

		// Shaped after runSupervisor's own restart line, which solves this exact problem
		// the same way: pick the level, then log once.
		unbound := slog.Warn
		if !first {
			unbound = slog.Debug
		}
		unbound("supervisor: could not open the fallback listener, so a client will see a refused connection rather than an explanation",
			"addr", addr, "error", err)
		return
	}
	f.openOn(ln)
}

// openOn is open's second half, split out so a test can hand in a listener on a port
// the kernel picked instead of racing to bind a port it found free.
func (f *fallbackServer) openOn(ln net.Listener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		_ = ln.Close()
		return
	}
	f.ln = ln
	// The address was bindable, so the next run of failures is a new condition and gets
	// its own WARN -- see open's comment on why success is the safe place to re-arm.
	f.warnedBindFailure = false
	slog.Debug("supervisor: fallback listener open", "addr", ln.Addr().String())
	go f.serve(ln)
}

// close frees the address. The supervisor calls this immediately before starting a
// child, which is the only reason the child can bind at all, so a close that leaked the
// port would make every restart a port conflict.
func (f *fallbackServer) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return
	}
	_ = f.ln.Close()
	f.ln = nil
}

func (f *fallbackServer) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // closed, which is the normal way this ends
		}
		go refuse(c)
	}
}

// refuse answers one connection and closes it. The request is read as far as its first
// line and then ignored -- the answer does not depend on it.
//
// Logging is DEBUG per connection on purpose: clients retry, Claude Code three times
// per request, so anything louder would fill the log while the proxy is down.
func refuse(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(fallbackIOTimeout))
	line, err := bufio.NewReader(io.LimitReader(c, fallbackMaxRequestLine)).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	slog.Debug("supervisor: refused a request while the proxy is not serving",
		"request", requestLineForLog(line), "remote", c.RemoteAddr().String())
	if _, err := c.Write(fallbackRefusal); err != nil {
		return
	}

	// Closing with bytes still unread in the receive queue makes the kernel send RST
	// rather than FIN, and a reset can make the peer discard the response it was about
	// to read -- which would be the opaque failure this whole feature exists to remove.
	// Reading one line leaves exactly that whenever the request is bigger than bufio's
	// buffer: an absolute-form POST with a body. (CONNECT fits, which is why the tests
	// are not flaky -- but that is an accident of buffer size, not a guarantee.) So
	// half-close to say the response is complete, then drain what is pending so the
	// close is a FIN. Briefly: a client that got "Connection: close" closes its own
	// side, and waiting out fallbackIOTimeout for one that does not would pin this
	// goroutine for no gain.
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	// Bounded by time, not by bytes: io.Discard retains nothing, so there is no
	// allocation to cap here, and a cap is what made the first attempt at this still
	// reset on a body larger than the cap. fallbackMaxRequestLine bounds the one read
	// that does allocate -- bufio's.
	_ = c.SetReadDeadline(time.Now().Add(fallbackDrainTimeout))
	_, _ = io.Copy(io.Discard, c)
}

// requestLineForLog reduces the request line to what is safe to write to proxy.log. An
// absolute-form plain-HTTP request carries a full URL, and a query string can carry an
// API key -- the same reason the session timeline records a query-stripped path. CONNECT
// lines are host:port and pass through unchanged.
func requestLineForLog(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.IndexByte(line, '?'); i >= 0 {
		return line[:i] + "?<redacted>"
	}
	return line
}

package forwardproxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// Tests for the TLS bridge's per-program rules
// (docs/superpowers/specs/2026-10-09-tls-bridge-per-program-design.md): a refusal is
// remembered against the program that refused, not the host it was talking to, so a
// different program on the same host is still bridged.

const (
	exeClaude = "/bin/claude"
	exeHelm   = "/opt/homebrew/bin/helm"
	exeCurl   = "/usr/bin/curl"
	exePython = "/usr/bin/python3"
)

// freshProc is fproc for a process started after the bridge CA, which a test about an
// ordinary program needs. fproc's processes start in 1970, so to the bridge every one of
// them predates its CA and is remembered on its own (Program.ProcessKey) rather than as
// its program. An hour from now postdates any CA a test mints, however far that CA's
// NotBefore is backdated.
func freshProc(pid, ppid int32, exe string) peerproc.Proc {
	p := fproc(pid, ppid, exe)
	p.Start = time.Now().Add(time.Hour)
	return p
}

// staleStart is a process start before every CA a test mints, as Program.Start has it.
var staleStart = time.Unix(1, 0).UnixNano()

// freshStart is a process start after every CA a test mints, as Program.Start has it.
func freshStart() int64 { return time.Now().Add(time.Hour).UnixNano() }

// bridgeScene is a forward proxy with process attribution and a TLS bridge in front of
// a TLS origin, served the way cortex serves it.
type bridgeScene struct {
	proxyURL, backendURL, target string
	server                       *Server
	engine                       *tlsbridge.Engine
	bridgeCA                     *x509.Certificate
	// trusting holds the bridge CA and the origin's certificate: a client the bridge can
	// read. refusing holds only the origin's: a client that refuses the bridge's leaf but
	// completes a tunnelled handshake to the real server.
	trusting, refusing *x509.CertPool
}

func newBridgeScene(t *testing.T, store *session.Store, procs *fakeProcs) *bridgeScene {
	t.Helper()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	t.Cleanup(origin.Close)
	originCAPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})
	src, err := tlsbridge.NewEphemeralSource()
	if err != nil {
		t.Fatalf("NewEphemeralSource: %v", err)
	}
	up, err := tlsbridge.NewUpstreamClient(originCAPEM, false)
	if err != nil {
		t.Fatalf("NewUpstreamClient: %v", err)
	}
	sc := &bridgeScene{target: strings.TrimPrefix(origin.URL, "https://")}
	sc.engine = &tlsbridge.Engine{
		Decision: mustBridgeDecision(t, portOf(sc.target)),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Programs: tlsbridge.NewProgramSkipSet(),
		Upstream: up,
		CAPEM:    src.CACertPEM(),
	}
	sc.bridgeCA, _ = src.Issuer()
	sc.trusting = x509.NewCertPool()
	sc.trusting.AddCert(sc.bridgeCA)
	sc.trusting.AddCert(origin.Certificate())
	sc.refusing = x509.NewCertPool()
	sc.refusing.AddCert(origin.Certificate())
	sc.proxyURL, sc.backendURL, _ = newProcessProxy(t, store, procs, func(s *Server) {
		s.TLSBridge = sc.engine
		sc.server = s
	})
	return sc
}

// handshake opens a CONNECT to the origin as pid and completes TLS over it trusting
// roots. bridged reports whether the leaf it was shown is the bridge's, which tells a
// decrypted connection from a tunnelled one.
func (sc *bridgeScene) handshake(t *testing.T, procs *fakeProcs, pid int32, roots *x509.CertPool) (bridged bool, err error) {
	t.Helper()
	raw, _, resp := connectAs(t, procs, sc.proxyURL, sc.target, pid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d", resp.StatusCode)
	}
	tc := tls.Client(raw, &tls.Config{ServerName: "127.0.0.1", RootCAs: roots})
	if err := tc.Handshake(); err != nil {
		return false, err
	}
	defer func() { _ = tc.Close() }()
	return bytes.Equal(tc.ConnectionState().PeerCertificates[0].RawIssuer, sc.bridgeCA.RawSubject), nil
}

// tunnelReasons is the reason on every tunnel-open row recorded under id, in order.
func tunnelReasons(store *session.Store, id string) []pipeline.TunnelReason {
	v := store.View(id)
	if v == nil {
		return nil
	}
	var out []pipeline.TunnelReason
	for _, e := range v.Events {
		if e.Tunnel && e.Phase == pipeline.SessionRequest && e.TunnelReason != "" {
			out = append(out, e.TunnelReason)
		}
	}
	return out
}

// The headline. A program that refused the leaf is passed through on its next
// connection, and a different program talking to the same host is still read. Before,
// one refusal hid the host from every program until a window passed.
func TestBridgeProgram_ARefusingProgramIsPassedThroughAndOthersStillBridged(t *testing.T) {
	procs := newFakeProcs(freshProc(300, 1, exeHelm), freshProc(400, 1, exeCurl))
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	helm := tlsbridge.Program{Exe: exeHelm}.Key()

	if _, err := sc.handshake(t, procs, 300, sc.refusing); err == nil {
		t.Fatal("first connection: helm completed a handshake on a leaf it does not trust")
	}
	eventually(t, func() bool { return sc.engine.Programs.Contains(helm) }, "helm's refusal to be recorded against helm")
	eventually(t, func() bool {
		reasons := tunnelReasons(store, session.DefaultSessionID)
		return len(reasons) > 0 && reasons[0] == pipeline.TunnelClientRejectedCA
	}, "the first connection's tunnel row to say client-rejected-ca")
	if sc.engine.Skip.Contains(hostOnly(sc.target)) {
		t.Error("helm's refusal was recorded against the host too, which hides the host from every program")
	}

	bridged, err := sc.handshake(t, procs, 300, sc.refusing)
	if err != nil {
		t.Fatalf("second connection: helm was not passed through: %v", err)
	}
	if bridged {
		t.Fatal("second connection: helm was shown the bridge's leaf again")
	}
	eventually(t, func() bool {
		return slices.Contains(tunnelReasons(store, session.DefaultSessionID), pipeline.TunnelProgramRefused)
	}, "a program-refused tunnel row")

	if bridged, err := sc.handshake(t, procs, 400, sc.trusting); err != nil || !bridged {
		t.Fatalf("curl trusts the CA but was not bridged to the same host (bridged=%v, err=%v)", bridged, err)
	}
}

// Review focus 3. The same interpreter run by an agent and run on its own can disagree
// about the CA, so one refusing must not pass the other through.
func TestBridgeProgram_SameExecutableUnderAnAgentIsTrackedApart(t *testing.T) {
	procs := newFakeProcs(freshProc(100, 1, exeClaude), freshProc(500, 100, exePython), freshProc(600, 1, exePython))
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	// Naming a session through its own header is what makes pid 100 an agent.
	sendAs(t, procs.clientFor(sc.proxyURL, 100), sc.backendURL+"/v1/messages", procClaudeUA, session.ClaudeCodeSessionHeader, "s1")

	if _, err := sc.handshake(t, procs, 600, sc.refusing); err == nil {
		t.Fatal("python3 on its own completed a handshake on a leaf it does not trust")
	}
	eventually(t, func() bool { return sc.engine.Programs.Contains(tlsbridge.Program{Exe: exePython}.Key()) },
		"python3's refusal to be recorded with no agent")
	if bridged, err := sc.handshake(t, procs, 500, sc.trusting); err != nil || !bridged {
		t.Fatalf("python3 under the agent was passed through because python3 on its own refused (bridged=%v, err=%v)", bridged, err)
	}
}

// A client whose program cannot be named keeps today's behaviour: its refusal is
// recorded against the host, and the host memory decides its next connection. A named
// program ignores the host memory, and its success clears the host's entry.
func TestBridgeProgram_AnUnnamedClientKeepsTheHostMemory(t *testing.T) {
	procs := newFakeProcs(freshProc(400, 1, exeCurl)) // pid 999 is in no table, so its executable is unknown
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	host := hostOnly(sc.target)

	if _, err := sc.handshake(t, procs, 999, sc.refusing); err == nil {
		t.Fatal("the unnamed client completed a handshake on a leaf it does not trust")
	}
	eventually(t, func() bool { return sc.engine.Skip.Contains(host) }, "the refusal to be recorded against the host")
	if bridged, err := sc.handshake(t, procs, 999, sc.refusing); err != nil || bridged {
		t.Fatalf("the unnamed client's retry was not tunnelled by the host memory (bridged=%v, err=%v)", bridged, err)
	}
	if bridged, err := sc.handshake(t, procs, 400, sc.trusting); err != nil || !bridged {
		t.Fatalf("curl was held back by another client's host entry (bridged=%v, err=%v)", bridged, err)
	}
	eventually(t, func() bool { return !sc.engine.Skip.Contains(host) }, "curl's success to clear the host's entry")
}

// The usual refusal is a process that started before the CA existed — a first install,
// a recreated ~/.cortex, a CA renewal — and such a process cannot have loaded the CA, so
// its refusals are remembered against that process alone. Against its program they
// would stop the key a freshly started instance also has until it names a session, and
// no new instance would be read until Cortex restarted: the "restart the client" advice
// the rejection prints would stop working.
func TestBridgeProgram_AStaleProcessDoesNotStopItsProgram(t *testing.T) {
	stale, fresh := fproc(100, 1, exeClaude), freshProc(200, 1, exeClaude)
	procs := newFakeProcs(stale, fresh)
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	staleProg := tlsbridge.Program{Exe: exeClaude, PID: stale.PID, Start: stale.Start.UnixNano()}
	claude := tlsbridge.Program{Exe: exeClaude}.Key()

	if _, err := sc.handshake(t, procs, 100, sc.refusing); err == nil {
		t.Fatal("the stale process completed a handshake on a leaf it does not trust")
	}
	eventually(t, func() bool { return sc.engine.Programs.Contains(staleProg.ProcessKey()) },
		"the stale process's refusal to be recorded against that process")
	// Its second and third refusals. A CONNECT cannot reach the forge again while the
	// process's own window is open, and windows are 30s and up, so these go to
	// bridgeServe, which forges unconditionally and records through the same rule. They
	// land in that open window and so count once (tlsbridge's tests pin the stop itself);
	// what matters here is that however many there are, none reaches the program's key.
	for range 2 {
		tl := discardTunnel()
		tl.program = &staleProg
		sc.server.bridgeServe(rejectingClient(t), sc.target, hostOnly(sc.target), tl)
	}
	if sc.engine.Programs.Contains(claude) {
		t.Fatal("the stale process's refusals were recorded against its program, which a fresh claude shares")
	}

	if bridged, err := sc.handshake(t, procs, 200, sc.trusting); err != nil || !bridged {
		t.Fatalf("a freshly started claude was not bridged after a stale one refused (bridged=%v, err=%v)", bridged, err)
	}
	if bridged, err := sc.handshake(t, procs, 100, sc.refusing); err != nil || bridged {
		t.Fatalf("the stale process's next connection was not passed through (bridged=%v, err=%v)", bridged, err)
	}
	eventually(t, func() bool {
		return slices.Contains(tunnelReasons(store, session.DefaultSessionID), pipeline.TunnelProgramRefused)
	}, "the stale process's next connection to be recorded as program-refused")
}

// A process that started after the CA could have loaded it, so its refusal is evidence
// about the program and is recorded under Key, where it decides every process running
// that program.
func TestBridgeServe_RecordsANamedProgramsRefusalAgainstTheProgram(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	host := hostOnly(authority)
	prog := tlsbridge.Program{Exe: exeHelm, PID: 300, Start: freshStart()}
	tl := discardTunnel()
	tl.program = &prog

	s.bridgeServe(rejectingClient(t), authority, host, tl)
	if !s.TLSBridge.Programs.Contains(prog.Key()) {
		t.Error("the refusal was not recorded against the program")
	}
	if s.TLSBridge.Programs.Contains(prog.ProcessKey()) {
		t.Error("a process started after the CA had its refusal recorded against the process alone")
	}
	if s.TLSBridge.Skip.Contains(host) {
		t.Error("the refusal was recorded against the host as well")
	}
}

func TestBridgeServe_RecordsAStaleProcesssRefusalAgainstTheProcess(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	host := hostOnly(authority)
	prog := tlsbridge.Program{Exe: exeClaude, PID: 100, Start: staleStart}
	tl := discardTunnel()
	tl.program = &prog

	s.bridgeServe(rejectingClient(t), authority, host, tl)
	if !s.TLSBridge.Programs.Contains(prog.ProcessKey()) {
		t.Error("a process older than the CA did not have its refusal recorded against the process")
	}
	if s.TLSBridge.Programs.Contains(prog.Key()) {
		t.Error("a process older than the CA had its refusal recorded against its program")
	}
	if s.TLSBridge.Skip.Contains(host) {
		t.Error("the refusal was recorded against the host as well")
	}
}

// Only a process known to predate the CA is told apart. An unknown start, or a CA whose
// NotBefore cannot be read, is no evidence either way, so the refusal is the program's,
// as it was before processes were told apart.
func TestBridgeServe_WithoutEvidenceOfStalenessRecordsAgainstTheProgram(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start int64
		noCA  bool
	}{
		{name: "unknown start", start: 0},
		{name: "unknown CA NotBefore", start: staleStart, noCA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, authority := bridgeForRejectTest(t)
			s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
			if tc.noCA {
				s.TLSBridge.CAPEM = nil // CAPEM is read only for diagnostics and this rule; the terminator mints from its source
			}
			prog := tlsbridge.Program{Exe: exeHelm, PID: 300, Start: tc.start}
			tl := discardTunnel()
			tl.program = &prog

			s.bridgeServe(rejectingClient(t), authority, hostOnly(authority), tl)
			if !s.TLSBridge.Programs.Contains(prog.Key()) {
				t.Error("the refusal was not recorded against the program")
			}
			if s.TLSBridge.Programs.Contains(prog.ProcessKey()) {
				t.Error("the refusal was recorded against the process with no evidence it predates the CA")
			}
		})
	}
}

// The rejection line names the agent a program runs under: python3 under an agent and
// python3 alone are remembered apart, and without it their lines read the same.
func TestBridgeServe_RejectionLogNamesTheAgent(t *testing.T) {
	var logbuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	for _, prog := range []tlsbridge.Program{{Exe: exePython, Agent: exeClaude}, {Exe: exePython}} {
		logbuf.Reset()
		tl := discardTunnel()
		tl.program = &prog
		s.bridgeServe(rejectingClient(t), authority, hostOnly(authority), tl)
		got := logbuf.String()
		if !strings.Contains(got, "program="+exePython) {
			t.Fatalf("the rejection line does not name the program:\n%s", got)
		}
		if named := strings.Contains(got, "agent="); named != (prog.Agent != "") {
			t.Errorf("agent %q: line names an agent = %v:\n%s", prog.Agent, named, got)
		}
		if prog.Agent != "" && !strings.Contains(got, "agent="+exeClaude) {
			t.Errorf("the rejection line does not name the agent %s:\n%s", exeClaude, got)
		}
	}
}

// Review focus 1: a tunnel with no program on it — the transparent listener, and every
// caller handing bridgeServe a bare tunnelLog — records against the host, as before.
func TestBridgeServe_UnnamedTunnelRecordsAgainstTheHost(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	host := hostOnly(authority)

	s.bridgeServe(rejectingClient(t), authority, host, discardTunnel())
	if !s.TLSBridge.Skip.Contains(host) {
		t.Error("an unnamed tunnel's refusal was not recorded against the host")
	}
}

// Review focus 2: an Engine built without Programs, as any outside consumer of core may
// build one, records against the host even when a program was named, and does not panic.
func TestBridgeServe_ProgramWithoutProgramMemoryFallsBackToTheHost(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t) // no Programs
	host := hostOnly(authority)
	tl := discardTunnel()
	tl.program = &tlsbridge.Program{Exe: exeHelm}

	s.bridgeServe(rejectingClient(t), authority, host, tl)
	if !s.TLSBridge.Skip.Contains(host) {
		t.Error("with no program memory the refusal was not recorded against the host")
	}
}

// A completed handshake clears both of the connection's keys: its program's, and its
// process's own, where a refusal from before the CA would have been kept.
func TestBridgeServe_ASuccessClearsTheProgramsEntry(t *testing.T) {
	s, _, authority := bridgeForRejectTest(t)
	s.TLSBridge.Programs = tlsbridge.NewProgramSkipSet()
	host := hostOnly(authority)
	prog := tlsbridge.Program{Exe: exeCurl, PID: 400, Start: freshStart()}
	s.TLSBridge.Programs.Fail(prog.Key())
	s.TLSBridge.Programs.Fail(prog.ProcessKey())
	tl := discardTunnel()
	tl.program = &prog

	// bridgeServe blocks serving the decrypted connection, so run it and wait.
	go s.bridgeServe(trustingClient(t, s.TLSBridge.CAPEM), authority, host, tl)
	eventually(t, func() bool {
		return !s.TLSBridge.Programs.Contains(prog.Key()) && !s.TLSBridge.Programs.Contains(prog.ProcessKey())
	}, "a completed handshake to clear the program's entry and the process's")
}

// fakeTrust predicts a refusal for the executables it names.
type fakeTrust map[string]bool

func (f fakeTrust) OSTrustOnly(exe string) bool { return f[exe] }

// Goal 1. A program the bridge knows would refuse — a Go program on macOS while macOS
// does not trust the CA — is passed through on its FIRST connection, and nothing is
// recorded against it or the host, because it was never shown a leaf.
func TestBridgeProgram_APredictedRefusalWorksFromTheFirstConnection(t *testing.T) {
	procs := newFakeProcs(freshProc(300, 1, exeHelm), freshProc(400, 1, exeCurl))
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	sc.engine.Trust = fakeTrust{exeHelm: true}

	bridged, err := sc.handshake(t, procs, 300, sc.refusing)
	if err != nil {
		t.Fatalf("helm's first connection failed: %v", err)
	}
	if bridged {
		t.Fatal("helm was shown the bridge's leaf")
	}
	eventually(t, func() bool {
		return slices.Contains(tunnelReasons(store, session.DefaultSessionID), pipeline.TunnelOSTrustOnly)
	}, "an os-trust-only tunnel row")
	if sc.engine.Programs.Contains(tlsbridge.Program{Exe: exeHelm}.Key()) {
		t.Error("a refusal was recorded for a program that was never shown a leaf")
	}
	if sc.engine.Skip.Contains(hostOnly(sc.target)) {
		t.Error("the host was skipped although nothing refused")
	}
	if bridged, err := sc.handshake(t, procs, 400, sc.trusting); err != nil || !bridged {
		t.Fatalf("curl was not bridged (bridged=%v, err=%v)", bridged, err)
	}
}

// Review focus 4. A program that refused while macOS still trusted the CA keeps its own
// reason when the prediction also applies: both pass it through, and the row says what
// actually happened to it.
func TestBridgeProgram_RefusedBeforeWinsOverOSTrust(t *testing.T) {
	procs := newFakeProcs(freshProc(300, 1, exeHelm))
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	sc.engine.Programs.Fail(tlsbridge.Program{Exe: exeHelm}.Key())
	sc.engine.Trust = fakeTrust{exeHelm: true}

	if bridged, err := sc.handshake(t, procs, 300, sc.refusing); err != nil || bridged {
		t.Fatalf("helm was not passed through (bridged=%v, err=%v)", bridged, err)
	}
	eventually(t, func() bool {
		return slices.Contains(tunnelReasons(store, session.DefaultSessionID), pipeline.TunnelProgramRefused)
	}, "a program-refused tunnel row")
	if slices.Contains(tunnelReasons(store, session.DefaultSessionID), pipeline.TunnelOSTrustOnly) {
		t.Error("the row says os-trust-only for a program that refused")
	}
}

// tlsbridge reports with its own copies of these strings, since it does not import
// pipeline; the two must not drift.
func TestUnreadReasonsMatchTunnelReasons(t *testing.T) {
	if tlsbridge.UnreadProgramRefused != string(pipeline.TunnelProgramRefused) ||
		tlsbridge.UnreadOSTrustOnly != string(pipeline.TunnelOSTrustOnly) {
		t.Fatal("tlsbridge's unread reasons differ from the tunnel reasons the rows carry")
	}
}

// End to end: what the bridge passed through, and why, reaches the report, every
// connection counted on the entry that passed it through — the program's for a process
// started after the CA, and the process's own for one started before it, which the
// report names.
func TestBridgeProgram_TheReportListsWhatIsNotRead(t *testing.T) {
	stalePython, freshCurl := fproc(600, 1, exePython), freshProc(400, 1, exeCurl)
	procs := newFakeProcs(freshProc(300, 1, exeHelm), stalePython, freshCurl)
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	sc.engine.Unread = tlsbridge.NewUnreadLog()
	sc.engine.Trust = fakeTrust{exeHelm: true}
	refusers := map[int32]string{
		600: tlsbridge.Program{Exe: exePython, PID: 600, Start: stalePython.Start.UnixNano()}.ProcessKey(),
		400: tlsbridge.Program{Exe: exeCurl}.Key(),
	}

	if _, err := sc.handshake(t, procs, 300, sc.refusing); err != nil {
		t.Fatalf("helm: %v", err)
	}
	for pid, key := range refusers {
		if _, err := sc.handshake(t, procs, pid, sc.refusing); err == nil {
			t.Fatalf("pid %d completed a handshake on a leaf it does not trust", pid)
		}
		eventually(t, func() bool { return sc.engine.Programs.Contains(key) }, "the refusal to be recorded")
		if bridged, err := sc.handshake(t, procs, pid, sc.refusing); err != nil || bridged {
			t.Fatalf("pid %d's next connection was not passed through (bridged=%v, err=%v)", pid, bridged, err)
		}
	}
	var report tlsbridge.UnreadReport
	defer func() {
		if t.Failed() {
			t.Logf("report: %+v", report.Programs)
		}
	}()
	eventually(t, func() bool {
		report = sc.engine.UnreadReport()
		var helm, python, curl bool
		for _, p := range report.Programs {
			switch p.Program {
			case exeHelm:
				helm = p.Reason == tlsbridge.UnreadOSTrustOnly && p.LastHost != "" && p.PID == 0
			case exePython:
				python = p.Reason == tlsbridge.UnreadProgramRefused && p.PID == 600 && p.Connections == 2
			case exeCurl:
				curl = p.Reason == tlsbridge.UnreadProgramRefused && p.PID == 0 && p.Connections == 2
			}
		}
		return helm && python && curl
	}, "helm (os-trust-only), the stale python3 process and curl (program-refused, both connections each) in the report")
}

// A process older than the CA that refused has its own entry, and its program can have
// one too, from a process that started after the CA and refused as well. That process's
// next connection is counted once, on the program's entry, which is the one checked first.
func TestBridgeProgram_AConnectionIsCountedOnItsProgramsEntryFirst(t *testing.T) {
	stale := fproc(100, 1, exeClaude)
	procs := newFakeProcs(stale)
	store := session.New(0, 0, 0)
	defer store.Close()
	sc := newBridgeScene(t, store, procs)
	sc.engine.Unread = tlsbridge.NewUnreadLog()
	prog := tlsbridge.Program{Exe: exeClaude, PID: stale.PID, Start: stale.Start.UnixNano()}
	sc.engine.Programs.Fail(prog.Key())
	sc.engine.Programs.Fail(prog.ProcessKey())

	if bridged, err := sc.handshake(t, procs, 100, sc.refusing); err != nil || bridged {
		t.Fatalf("the process was not passed through (bridged=%v, err=%v)", bridged, err)
	}
	byPID := map[int32]tlsbridge.UnreadProgram{}
	for _, p := range sc.engine.UnreadReport().Programs {
		byPID[p.PID] = p
	}
	progEntry, progListed := byPID[0]
	procEntry, procListed := byPID[stale.PID]
	if !progListed || !procListed {
		t.Fatalf("program entry listed = %v, process entry listed = %v; want both", progListed, procListed)
	}
	if progEntry.Connections != 1 || procEntry.Connections != 0 {
		t.Errorf("program entry = %+v, process entry = %+v; want the connection on the program's alone", progEntry, procEntry)
	}
}

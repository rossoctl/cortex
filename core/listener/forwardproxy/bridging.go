package forwardproxy

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// bridgeVerdict applies the TLS bridge's rules to a CONNECT, in the order the design
// gives them (docs/superpowers/specs/2026-10-09-tls-bridge-per-program-design.md): the
// host's own eligibility, then what is known about the client's program, and the host
// memory only for a client whose program cannot be named. It returns the reason the
// bytes stay opaque, or bridge true and no reason. With bridge true and a named
// program it sets tl.program, so bridgeServe records the outcome against the program.
func (s *Server) bridgeVerdict(r *http.Request, tl *tunnelLog, host string, first []byte) (reason pipeline.TunnelReason, bridge bool) {
	prog, named := s.bridgeProgram(r)
	if s.failClosed() {
		// Host and port alone decide: no memory of earlier failures and no
		// prediction of a refusal. An intercepted host is always bridged or
		// refused, never tunnelled.
		v, why := s.TLSBridge.Decision.Classify(host, portOf(r.Host), first)
		if v != tlsbridge.Terminate && !s.TLSBridge.Decision.Intercepts(host, portOf(r.Host)) {
			return passthroughReason(why), false
		}
		if v != tlsbridge.Terminate && len(first) == 5 {
			return pipeline.TunnelRefusedNonTLS, false
		}
		// A short peek is a slow client, not evidence of another protocol: the
		// handshake decides, and fails within its own bound if it is not TLS.
		if named {
			tl.program = &prog
		}
		return "", true
	}
	if !named {
		// Unchanged from before programs could be named, order included.
		if s.TLSBridge.Skip.Contains(host) {
			// Distinct from client-rejected-ca: this client may trust the CA
			// perfectly well and is being tunnelled because another one did not.
			return pipeline.TunnelSkipCached, false
		}
		v, why := s.TLSBridge.Decision.Classify(host, portOf(r.Host), first)
		return passthroughReason(why), v == tlsbridge.Terminate
	}
	v, why := s.TLSBridge.Decision.Classify(host, portOf(r.Host), first)
	if v != tlsbridge.Terminate {
		return passthroughReason(why), false
	}
	// Either key: the program's, which every process running it shares, and this
	// process's own, where bridgeMemory keeps a refusal from a process older than the CA.
	// The connection is noted under the key that matched, so the unread report counts it
	// on the entry that passed it through.
	for _, key := range [...]string{prog.Key(), prog.ProcessKey()} {
		if s.TLSBridge.Programs.Contains(key) {
			s.TLSBridge.Unread.Note(key, string(pipeline.TunnelProgramRefused), host)
			return pipeline.TunnelProgramRefused, false
		}
	}
	if t := s.TLSBridge.Trust; t != nil && t.OSTrustOnly(prog.Exe) {
		s.TLSBridge.Unread.Note(prog.Key(), string(pipeline.TunnelOSTrustOnly), host)
		return pipeline.TunnelOSTrustOnly, false
	}
	tl.program = &prog
	return "", true
}

// bridgeMemory is where a failed handshake on tl is recorded: the program's entry when
// the bridge rules named the client's program, and the host's otherwise — which is
// what the transparent listener, a client whose program could not be named, and an
// Engine without Programs all get.
//
// A process that started before the bridge CA gets an entry of its own instead
// (Program.ProcessKey). It cannot have loaded that CA, so its refusal says nothing
// about the program, and under the program's key a stale agent would stop every new
// instance of it, which shares that key until it names a session. An unknown start or
// NotBefore is no evidence the process is stale, and it records against the program.
func (s *Server) bridgeMemory(tl *tunnelLog, host string) (*tlsbridge.SkipSet, string) {
	if tl == nil || tl.program == nil || s.TLSBridge.Programs == nil {
		return s.TLSBridge.Skip, host
	}
	if nb := s.caNotBeforeTime(); tl.program.Start != 0 && !nb.IsZero() && time.Unix(0, tl.program.Start).Before(nb) {
		return s.TLSBridge.Programs, tl.program.ProcessKey()
	}
	return s.TLSBridge.Programs, tl.program.Key()
}

// failClosed reports whether tls_bridge.fail_closed is on.
func (s *Server) failClosed() bool { return s.TLSBridge != nil && s.TLSBridge.FailClosed }

// refuseFailClosed ends a connection fail-closed will neither bridge nor tunnel:
// it records the open with reason and a close with status and kind, then closes
// the client.
func (s *Server) refuseFailClosed(client net.Conn, host string, tl *tunnelLog, reason pipeline.TunnelReason, status int, kind string, err error) {
	slog.Warn("tls-bridge refused (fail_closed)", "host", pipeline.SanitizeLogValue(host),
		"reason", reason, "client", clientAddr(client), "error", err)
	tl.open(reason)
	tl.close(status, &pipeline.EventError{Kind: kind, Message: err.Error()}, 0, 0)
	_ = client.Close()
}

// verifyUpstream checks that authority is reachable and its certificate verifies.
// HEAD avoids GET side-effects; a non-2xx status still returns err==nil (cert
// verified), which is all we need. Only a transport/TLS error fails here. The
// verify is bounded by its own context timeout so a slow/stalled origin can't pin
// the caller — the timeout is on this probe ONLY, not on the relay (which must
// stay unbounded for streaming responses).
func (s *Server) verifyUpstream(authority string) error {
	ctx, cancel := context.WithTimeout(context.Background(), upstreamVerifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+authority, nil)
	if err != nil {
		return err
	}
	resp, err := s.TLSBridge.Upstream.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// logUpstreamUnverified reports that the bridge could not verify host's certificate, so the
// host's traffic passes through unread: no plugin sees it and nothing is parsed. That is a
// loss of visibility, so the first failure for a host is a WARN. Repeats stay at INFO, so a
// host that always fails cannot fill the log.
func (s *Server) logUpstreamUnverified(host string, err error) {
	level := slog.LevelInfo
	if _, seen := s.upstreamUnverifiedWarned.LoadOrStore(host, struct{}{}); !seen {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "tls-bridge passthrough", "host", pipeline.SanitizeLogValue(host),
		"reason", "upstream-verify", "error", err)
}

// bridgeServe attempts to terminate the client's TLS and serve the decrypted
// connection through the pipeline. Returns true when it handled the connection
// (bridged, or the client's connection died post-forge and there is nothing left
// to tunnel), false when it declined and the caller should tunnel instead.
//
// tl records the tunnel-open event with the reason the bytes stayed opaque, and
// bridgeServe decides it on every path it takes, because two of the reasons are
// discovered only in here. On the declining paths and a failed forge it records the
// open itself. On the bridged path it records nothing: markBridged defers the open to
// the tunnel's first decrypted request that records a row, and must run before
// ServeConn starts any handler.
//
// A failed forge returns true with no decrypted request to answer the tunnel, so
// bridgeServe records its close too. A bridged tunnel is settled by whichever of
// finish and the last handler's release comes last, which records what the tunnel
// still owes — its open and a close — only if no request recorded a row. On the
// false path the caller tunnels and records the close itself. skipProbe skips the
// origin probe: see step 1. Under fail-closed bridgeServe never returns false.
func (s *Server) bridgeServe(client net.Conn, authority, host string, tl *tunnelLog, skipProbe bool) bool {
	// 1) Verify upstream reachability + cert via the dedicated client, BEFORE forging.
	//
	//    Skipped when the pre-dial already failed: the probe would only re-confirm a
	//    dead origin at up to upstreamVerifyTimeout more, and its passthrough fallback
	//    dials the same dead origin. Bridging proceeds so a plugin redirect
	//    (inference-router moving a pinned session) can still send the request to a
	//    live server (#1595); the re-originated request is verified against whatever
	//    host it actually goes to. Skipped too when the CONNECT already verified the
	//    origin before its 200 (fail-closed).
	if !skipProbe {
		if err := s.verifyUpstream(authority); err != nil {
			if s.failClosed() {
				s.refuseFailClosed(client, host, tl, pipeline.TunnelOriginUnverified,
					http.StatusBadGateway, pipeline.ErrorKindUpstreamUnverified, err)
				return true
			}
			s.logUpstreamUnverified(host, err)
			tl.open(pipeline.TunnelOriginUnverified)
			return false // fall back to plain tunnel — agent's own e2e TLS still reaches origin
		}
	}

	// 2) Forge + terminate downstream. Fail-closed requires the SNI to name the
	//    CONNECT host, so policy judged on that host is policy for the bytes inside.
	tconn, err := s.TLSBridge.Term.Terminate(client, hostOnly(authority), s.failClosed())
	if err != nil {
		if errors.Is(err, tlsbridge.ErrSNIMismatch) {
			s.refuseFailClosed(client, host, tl, pipeline.TunnelSNIMismatch,
				http.StatusMisdirectedRequest, pipeline.ErrorKindMisdirectedRequest, err)
			return true
		}
		reason := handshakeFailureReason(err)
		// Seed a skip either way — the forged handshake killed this connection, so the
		// client's retry needs a tunnel whatever went wrong. The skip is the program's
		// when the bridge rules named the client's program, and the host's otherwise
		// (bridgeMemory), so a named program's failure costs no other program anything.
		// But only ESCALATE for a real rejection: that is the one failure class that is
		// evidence of a persistent trust problem. A client that merely cancels requests,
		// or one tripping a cipher mismatch, would otherwise walk its entry up to the
		// ceiling — on the host memory taking every other client's observability with
		// it, the same defect this change exists to fix, one level down; on the program
		// memory stopping a program that was never refusing.
		//
		// The decision is here rather than inside SkipSet because the reason vocabulary
		// belongs to the session-event layer, and tlsbridge has no other business
		// knowing about it.
		//
		// Fail-closed seeds nothing: the retry is bridged again, and fails again,
		// until the client trusts the CA.
		var memory *tlsbridge.SkipSet
		var key string
		if !s.failClosed() {
			memory, key = s.bridgeMemory(tl, host)
			if reason == pipeline.TunnelClientRejectedCA {
				memory.Fail(key)
			} else {
				memory.FailTransient(key)
			}
		}
		if memory != nil && memory == s.TLSBridge.Programs {
			// The failed connection was not read either, and it names the host the
			// program refused, which the report would otherwise lack until its next try.
			// Under the key just failed, so the note joins that entry. A host's entry is
			// the report's hosts list, which needs no note.
			s.TLSBridge.Unread.Note(key, string(pipeline.TunnelProgramRefused), host)
		}
		// UNCONDITIONAL, and it names the client. Success elsewhere must not silence
		// this: it used to sit behind bridgedRequests == 0, which treats CA trust as a
		// property of the deployment. It is a property of each client, and on a
		// machine running several agents they routinely disagree — one predates the
		// CA, the rest do not — so the counter was non-zero and the message that
		// explains the failure never printed.
		//
		// The client address is the discriminator, not the host: every client dials
		// the same host, so the host cannot tell them apart. It has to be captured
		// HERE, because the connection is gone by the time anyone reads the log and no
		// later process listing can attribute it.
		args := []any{
			"host", pipeline.SanitizeLogValue(host),
			"reason", reason,
			"client", clientAddr(client),
			"error", err,
		}
		if tl != nil && tl.program != nil {
			args = append(args, "program", tl.program.Exe)
			// python3 under an agent and python3 alone are remembered apart, and would
			// otherwise log the same line.
			if tl.program.Agent != "" {
				args = append(args, "agent", tl.program.Agent)
			}
		}
		// The restart advice goes ONLY on a real rejection. An EOF or a cipher
		// mismatch would send someone restarting agents over something that was never
		// about trust — and a minting failure on our own side is the worst case for
		// that, since nothing they do to the client can fix it.
		if reason == pipeline.TunnelClientRejectedCA {
			// Short enough to read unwrapped. The lsof recipe for mapping the client
			// port to a process lives in docs/laptop-service.md rather than being
			// repeated on every occurrence of this line.
			//
			// The fingerprint and the file are what make a MOVED CA diagnosable:
			// ca_not_before alone reads as recent for a client holding a different
			// ~/.cortex/ca, since every generated CA shares one CN. See caFingerprint.
			args = append(args,
				"ca_not_before", s.caNotBefore(),
				"ca_fingerprint", s.caFingerprint(),
				"ca_file", s.caFileHint(),
				"fix", "restart clients started before ca_not_before, or point the client at ca_file "+
					"and compare its fingerprint (openssl x509 -noout -fingerprint -sha256)")
		}
		msg := "tls-bridge passthrough"
		if s.failClosed() {
			msg = "tls-bridge refused (fail_closed)"
			_ = client.Close()
		}
		slog.Warn(msg, args...)
		tl.open(reason)
		// The CONNECT was answered 200 before the handshake, so that is the status; the
		// error is what killed the tunnel. Without this row the tunnel would read as
		// still open, which is the one thing it is not.
		tl.close(http.StatusOK, &pipeline.EventError{Kind: "tls_handshake", Message: err.Error()}, 0, 0)
		// Deliberately NOT also calling noteBridgeHandshakeFailure. It fires
		// warnBridgeUnused, whose fix is "point the client at the trust anchor" — so
		// with bridgedRequests == 0 a single rejected forge produced two warnings with
		// two different remedies, back to back. This one is strictly better informed:
		// it knows which client and which CA. warnBridgeUnused still covers its own
		// case, reached from the tunnel-threshold path.
		return true // conn is dead post-forge; nothing left to tunnel
	}
	// A completed forged handshake is proof a client here trusts the CA, so it clears
	// any skip left by a different client that does not — which is what stops one stale
	// agent suppressing this host for everyone until a window elapses.
	s.TLSBridge.Skip.Succeed(host)
	// A named program's own entries are cleared too, its process's along with its
	// program's. The host's is cleared either way: a completed handshake proves this
	// host's leaf works, and a client whose program cannot be named gets the benefit.
	if tl != nil && tl.program != nil && s.TLSBridge.Programs != nil {
		s.TLSBridge.Programs.Succeed(tl.program.Key())
		s.TLSBridge.Programs.Succeed(tl.program.ProcessKey())
	}
	// Bridged: defer the open, with no reason, which is what tells agentop to fold this
	// row into the decrypted inner request whose own action is the interesting one.
	markBridged(tl)

	// 3) Serve the decrypted conn through the UNCHANGED pipeline.
	tlsbridge.ServeConnIdle(tconn, s.bridgedHandler(authority, tl), s.BridgedIdleTimeout)
	// ServeConn returns once the connection has closed. A bridged tunnel's decrypted
	// requests answer it and agentop folds the open into the first of them, so a close row
	// there would render as an orphan response. With no recorded request at all there is
	// nothing to fold into, so finish records the open and a close — now, or when the last
	// handler still running returns.
	tl.finish()
	return true
}

// bridgeable reports whether the TLS bridge could take a connection to
// authority's port. The dead-origin fall-through and its memo are for those
// connections only: anywhere else the bridge would decline by port anyway, so
// a failed pre-dial keeps its immediate 502 instead of a 200 the bridge cannot
// use.
func (s *Server) bridgeable(authority string) bool {
	return s.TLSBridge != nil && s.TLSBridge.Decision.HandlesPort(portOf(authority))
}

// bridgedHandler serves one bridged tunnel's decrypted requests through the pipeline.
func (s *Server) bridgedHandler(authority string, tl *tunnelLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tl.admit() {
			return
		}
		defer tl.release()
		if tl.conn != nil {
			r = r.WithContext(context.WithValue(r.Context(), connProcKey{}, tl.conn))
		}
		// Before anything reads identity off r. A decrypted request is a NEW
		// request — the accepting listener's wrapper never saw it, so whatever
		// it stamped on or stripped from the CONNECT has to be redone here, or
		// the guest's own headers decide the session (#1569 review).
		if s.AdoptBridgedRequest != nil && tl.connect != nil {
			var ok bool
			if r, ok = s.AdoptBridgedRequest(w, tl.connect, r); !ok {
				return
			}
		}
		r.URL.Scheme = "https"
		r.URL.Host = authority // host:port — preserves non-443 origins
		s.serveOutbound(w, r, tl)
	})
}

// noteBridgeAttempt counts a CONNECT that classification chose to terminate, and
// warns once if the bridge has been asked to decrypt this many times and never
// managed it.
//
// This is the failure that looks like a bug in whatever plugin you are testing:
// tool-prune, the parsers and every body reader correctly do nothing, because
// there is no plaintext to act on. Naming the trust anchor turns a silent dead
// end into a one-line fix.
func (s *Server) noteBridgeAttempt() {
	n := s.bridgeAttempts.Add(1)
	if s.TLSBridge == nil || n < tunnelWarnThreshold || s.bridgedRequests.Load() > 0 {
		return
	}
	s.warnBridgeUnused("bridge_attempts", n, "the client does not trust the bridge CA")
}

// noteBridgeHandshakeFailure reports the unambiguous case: the client refused the
// forged certificate. Unlike the attempt threshold this needs no accumulation —
// one refusal already proves the trust anchor is not installed. It matters that
// this path warns, because a refusal adds the host to Skip, so later requests
// never reach noteBridgeAttempt and the threshold alone would never be crossed.
func (s *Server) noteBridgeHandshakeFailure() {
	s.warnBridgeUnused("bridge_attempts", s.bridgeAttempts.Load(),
		"the client rejected the bridge certificate, so it does not trust the bridge CA")
}

func (s *Server) warnBridgeUnused(countKey string, count uint64, cause string) {
	s.bridgeWarnOnce.Do(func() {
		s.bridgeWarned.Store(true)
		msg := "tls-bridge: enabled but nothing has been decrypted — every request is tunnelling through opaquely, so body-reading plugins (parsers, tool-prune) cannot act"
		if s.failClosed() {
			msg = "tls-bridge: enabled but nothing has been decrypted — fail_closed is refusing every intercepted request"
		}
		slog.Warn(msg,
			countKey, count,
			"tunnels_opened", s.tunnelsOpened.Load(),
			"bridged_requests", 0,
			"likely_cause", cause,
			"fix", "point the client at the trust anchor, e.g. NODE_EXTRA_CA_CERTS="+s.caFileHint())
	})
}

func (s *Server) caFileHint() string {
	if s.TLSBridge != nil && s.TLSBridge.CAFile != "" {
		return s.TLSBridge.CAFile
	}
	return "<ca_dir>/ca.crt"
}

// warnFired reports whether the bridge-health warning has already been emitted.
// Exists for tests: sync.Once has no public "has it run" query, and asserting
// on log output would couple the test to the message text.
func (s *Server) warnFired() bool { return s.bridgeWarned.Load() }

// caNotBeforeTime is the bridge CA's NotBefore, which is the line dividing clients
// that can trust it from clients that cannot: CA files are read once at process
// start, so anything older than this is holding a different CA (or none). Zero when
// there is no CA or it cannot be parsed. Parsed once — the value is fixed for the
// process.
func (s *Server) caNotBeforeTime() time.Time {
	s.caNotBeforeOnce.Do(func() {
		if s.TLSBridge == nil || len(s.TLSBridge.CAPEM) == 0 {
			return
		}
		blk, _ := pem.Decode(s.TLSBridge.CAPEM)
		if blk == nil {
			return
		}
		crt, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return
		}
		s.caNotBeforeAt = crt.NotBefore
	})
	return s.caNotBeforeAt
}

// caNotBefore is caNotBeforeTime as the rejection line prints it, "unknown" when it is
// zero. Rendered from the one parse, so the cutoff the log advises restarting clients
// against is the one bridgeMemory tells stale processes apart by.
func (s *Server) caNotBefore() string {
	nb := s.caNotBeforeTime()
	if nb.IsZero() {
		return "unknown"
	}
	return nb.Local().Format(time.RFC3339)
}

// caFingerprint is the bridge CA's SHA-256, in the encoding
//
//	openssl x509 -in ca.crt -noout -fingerprint -sha256
//
// prints — uppercase hex, colon-separated — because the only use for this value is
// someone comparing it by eye against that command's output on the file their
// client actually loaded.
//
// It answers the question ca_not_before cannot. `--local` derives ca_dir from
// $HOME, so a redirected $HOME (a sandbox, a per-project home) gets a CA of its
// own; every one of them is spelled ~/.cortex/ca and every one carries
// CN=authbridge-tls-bridge-ca. When a client rejects a leaf, a recent
// ca_not_before is equally consistent with "this client predates the CA" and
// "this client trusts a different CA that looks identical". The fingerprint is
// what separates them. Parsed once — the value is fixed for the process.
func (s *Server) caFingerprint() string {
	s.caFingerprintOnce.Do(func() {
		s.caFingerprintStr = "unknown"
		if s.TLSBridge == nil || len(s.TLSBridge.CAPEM) == 0 {
			return
		}
		blk, _ := pem.Decode(s.TLSBridge.CAPEM)
		if blk == nil {
			return
		}
		crt, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return
		}
		// Rendering lives in tlsbridge.FingerprintSHA256 so this and the --local
		// startup check cannot drift apart on encoding.
		s.caFingerprintStr = tlsbridge.FingerprintSHA256(crt)
	})
	return s.caFingerprintStr
}

// handshakeFailureReason narrows a failed forge to what we can actually claim.
//
// Terminator.Terminate returns exactly one error, from conn.Handshake(), and several
// very different things arrive through it: the client refusing our leaf, the client
// vanishing, a version/cipher/ALPN mismatch, and our own minter failing. Labelling
// them all "the client does not trust our CA" would send people restarting agents
// over problems that were never about trust.
//
// "remote error: tls:" is the discriminator — it means the PEER sent us an alert, so
// it actively rejected something rather than merely going away. bad_certificate and
// unknown_ca are the two alerts a client sends when it will not accept our chain.
//
// Matched on the string because the error is wrapped in the unexported
// *tls.permanentError: errors.As against tls.AlertError returns false for it, which I
// verified rather than assumed.
func handshakeFailureReason(err error) pipeline.TunnelReason {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "remote error: tls: bad certificate"),
		strings.Contains(msg, "remote error: tls: unknown certificate authority"):
		return pipeline.TunnelClientRejectedCA
	case errors.Is(err, io.EOF), strings.Contains(msg, "EOF"):
		return pipeline.TunnelClientHungUp
	}
	return pipeline.TunnelHandshakeFailed
}

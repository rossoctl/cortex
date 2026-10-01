// Package forwardproxy implements an HTTP forward proxy listener.
// Agents set HTTP_PROXY to route outbound traffic through this proxy
// for transparent token exchange.
package forwardproxy

import (
	"bufio"
	"bytes"
	"context"
	cryptotls "crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"errors"
	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/bodyread"
	"github.com/rossoctl/cortex/core/listener/internal/sseframe"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/spiffe"
	"github.com/rossoctl/cortex/core/tlsbridge"
	authtls "github.com/rossoctl/cortex/core/tlsconfig"
)

// maxBodySize is TWO ceilings, not one: the buffered request/response body cap,
// and the per-frame cap passed to sseframe.NewReader on both streaming paths
// (see handleStreamingResponse and streamFallbackBuffered). Raising it moves
// both, so worst-case resident bytes for one in-flight request are roughly
// 2x this value — a body buffer plus a frame scratch buffer — and the practical
// ceiling for a listener is that times the number of concurrent requests.
//
// An earlier value of 1 << 20 (1MB) matched Envoy's default
// per_stream_buffer_limit_bytes, but proved too small for LLM traffic: a
// Claude Code session against the Anthropic endpoint produced a 5,250,133-byte
// request, since an agent resends the whole conversation plus its full tool
// manifest on every turn. A body over the cap is rejected before the pipeline
// runs, so the limit also decides whether telemetry is recorded at all.
//
// The frame cap is the more generous half of the raise: a single SSE event is
// far smaller than a full request body, so 10MB per frame is well above
// anything observed. Splitting the two into separate constants would let the
// frame cap stay tight, and is worth doing if per-request memory ever matters
// more than the simplicity of one number.
const maxBodySize = 10 << 20 // 10 MB

// streamReadIdleTimeout caps how long the proxy waits for the next
// byte off a streaming response body. The time.Duration is applied
// per ReadFrame iteration (see streamingResponseBody). A wedged
// upstream that goes silent for longer than this aborts the stream,
// rather than hanging the agent indefinitely. Long enough to permit
// slow tool work between SSE heartbeats; short enough to surface a
// dead connection within a few minutes. Tools that need longer idle
// gaps should emit SSE heartbeats — it's what comment lines in SSE
// are for.
const streamReadIdleTimeout = 5 * time.Minute

// upstreamVerifyTimeout bounds the pre-forge HEAD reachability/cert probe in
// bridgeServe. It applies to that probe only — never to the relay, which must
// stay unbounded so streaming responses aren't cut off.
const upstreamVerifyTimeout = 10 * time.Second

// Server is an HTTP forward proxy that performs token exchange on outbound requests.
//
// OutboundPipeline is a holder so the bound pipeline can be hot-swapped
// under the running listener; each handleRequest Loads through it so
// in-flight requests finish on the pipeline they started with.
type Server struct {
	OutboundPipeline *pipeline.Holder
	Sessions         *session.Store       // nil when session tracking is disabled
	Shared           pipeline.SharedStore // process-scoped store; set by main, may be nil
	Client           *http.Client

	// SkipHosts, when non-nil and matching the request Host, causes
	// the listener to forward the request as a transparent proxy:
	// no pipeline run, no session recording. Applies to both HTTP
	// (handleRequest) and CONNECT-tunnel (handleConnect) paths so
	// matched destinations behave identically regardless of scheme.
	// See core/config/config.go ListenerConfig.SkipHosts for
	// motivation.
	SkipHosts *skiphost.Matcher

	TLSBridge *tlsbridge.Engine // nil = disabled; set by caller after NewServer

	// SessionIDHeaders are the request headers consulted, in order, for a
	// client-supplied session id to bucket events under. The first one
	// present and usable wins; when none is, bucketing falls back to
	// ActiveSession() and then the default bucket, exactly as before.
	// Empty disables header-based bucketing. See config.SessionConfig
	// SessionIDHeaders for the default and how to turn it off.
	SessionIDHeaders []string

	// ClientAffinity files a request that carries no session header under the newest
	// session of the SAME coding agent, instead of under ActiveSession() — the one
	// global "most recently updated" id, which with two agents running files each
	// one's header-less calls into the other's session. See config.SessionConfig
	// ClientAffinity; false keeps today's resolution byte for byte.
	ClientAffinity bool

	// bufferedFallbackOnce keeps the SSE-buffered-path notice to one line per
	// process; the condition is a supported chain shape, not an error.
	bufferedFallbackOnce sync.Once

	// Bridge-health counters. When the TLS bridge is enabled but the client
	// does not trust its CA, every HTTPS request opens a CONNECT tunnel and
	// nothing is ever decrypted: the pipeline sees opaque tunnels, every
	// body-reading plugin no-ops, and the proxy looks configured but inert.
	// Nothing errors, so the only symptom is silence. These count the two
	// outcomes so the listener can say so out loud.
	tunnelsOpened   atomic.Uint64
	bridgeAttempts  atomic.Uint64
	bridgedRequests atomic.Uint64

	// caNotBefore is parsed on first use and never changes for the process.
	caNotBeforeOnce sync.Once
	caNotBeforeStr  string
	// caFingerprint is likewise derived once; the CA is fixed for the process.
	caFingerprintOnce sync.Once
	caFingerprintStr  string
	bridgeWarnOnce    sync.Once
	bridgeWarned      atomic.Bool
}

// MTLSOptions configures outbound mTLS for the forward proxy. When
// non-nil, every outbound dial:
//
//  1. opens a plain TCP connection to the destination
//  2. attempts a TLS handshake using the local SVID
//  3. on handshake success → returns the *tls.Conn
//  4. on handshake failure → closes and returns the error (TLS-or-fail)
//
// There is no per-connection fallback to plaintext. To match Istio's
// PeerAuthentication semantics — and to keep proxy-sidecar's outbound
// behavior consistent with envoy-sidecar's, which has no native
// "try TLS, fall back" primitive — permissive mode does not pass
// MTLSOptions to NewServer at all (callers leave it nil so the
// transport stays plaintext). Strict mode passes MTLSOptions and the
// dial fails closed when the peer can't terminate.
//
// A successful handshake whose peer cert fails verification is always
// a hard error.
type MTLSOptions struct {
	Source  spiffe.X509Source
	Metrics *authtls.Metrics
}

// NewServer creates a forward proxy server with a default HTTP client.
// When mtls is non-nil, every outbound dial does TLS-or-fail using the
// local SVID; see MTLSOptions for semantics.
func NewServer(outbound *pipeline.Holder, sessions *session.Store, mtls *MTLSOptions) (*Server, error) {
	transport := &http.Transport{
		// Sane Go defaults for everything except DialContext, which we
		// customize when mTLS is on.
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// No ResponseHeaderTimeout: Streamable HTTP / MCP servers may
		// hold response headers open until a slow tool completes, even
		// when the eventual response is application/json (the server
		// picks JSON vs SSE per call). A fixed time-to-headers ceiling
		// reproduces the original 502 — the pre-headers wait is part
		// of the same long tool execution we're trying to permit. The
		// inbound request context + the per-Read idle timer on
		// streaming bodies are the bounds; an unrecoverably wedged
		// upstream is closed when the client cancels the request.
	}

	if mtls != nil {
		if mtls.Source == nil {
			return nil, fmt.Errorf("forwardproxy: MTLSOptions.Source is required when mtls is non-nil")
		}
		tlsCfg, err := authtls.ClientConfig(mtls.Source)
		if err != nil {
			return nil, fmt.Errorf("forwardproxy: build client tls config: %w", err)
		}
		transport.DialContext = mtlsDialer(tlsCfg, mtls.Metrics).DialContext
	}

	return &Server{
		OutboundPipeline: outbound,
		Sessions:         sessions,
		Client: &http.Client{
			// No Client.Timeout — Go applies that to the entire
			// request lifecycle including body read, which kills
			// streaming responses. Time-to-headers is enforced via
			// transport.ResponseHeaderTimeout above; per-read idle
			// behavior on streaming bodies is in streamingResponseBody.
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// mtlsDialer returns a dialer-shaped object whose DialContext does
// TLS-or-fail. We construct it once per Server so the *tls.Config /
// metrics references are stable across connections.
type mtlsDialFunc struct {
	plain   *net.Dialer
	tlsCfg  *cryptotls.Config
	metrics *authtls.Metrics
}

func mtlsDialer(cfg *cryptotls.Config, metrics *authtls.Metrics) *mtlsDialFunc {
	return &mtlsDialFunc{
		plain:   &net.Dialer{Timeout: 10 * time.Second},
		tlsCfg:  cfg,
		metrics: metrics,
	}
}

func (d *mtlsDialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	plain, err := d.plain.DialContext(ctx, network, addr)
	if err != nil {
		// TCP failure — separate bug class from "peer doesn't speak TLS".
		// Returned as-is so callers see the underlying dial error.
		return nil, err
	}

	// Per-handshake config: clone so we can set ServerName for SNI
	// without polluting the shared template.
	hsCfg := d.tlsCfg.Clone()
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr == nil && host != "" {
		hsCfg.ServerName = host
	}

	tlsConn := cryptotls.Client(plain, hsCfg)
	hsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		_ = tlsConn.Close()
		if d.metrics != nil {
			d.metrics.OutboundFailed.Add(1)
		}
		return nil, fmt.Errorf("forwardproxy mtls: handshake to %s failed: %w", addr, err)
	}

	if d.metrics != nil {
		d.metrics.OutboundTLSSucceeded.Add(1)
	}
	return tlsConn, nil
}

// Handler returns the HTTP handler for the forward proxy.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleRequest)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	s.serveOutbound(w, r, nil)
}

// serveOutbound runs the outbound pipeline for one decrypted/plaintext request
// and re-originates it. tl is the bridged tunnel the request was decrypted from,
// nil for a plaintext request. A non-nil tl marks requests produced by TLS
// bridging: they are origin-form (the caller sets r.URL.Scheme/Host) and must
// re-originate via the dedicated upstream client, never the mesh-mTLS s.Client.
func (s *Server) serveOutbound(w http.ResponseWriter, r *http.Request, tl *tunnelLog) {
	isBridge := tl != nil
	if isBridge {
		s.bridgedRequests.Add(1)
	}
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    r.Method,
		Scheme:    r.URL.Scheme,
		Host:      r.Host,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	// Pin the calling agent HERE, at construction, from the headers as the CLIENT sent
	// them. Same rule as the session identity below and for the same reason, one step
	// earlier: pctx.Headers is a clone that plugins write to, and ClientInfo's memo fills
	// on first READ, which without this line is an event-construction site downstream of
	// the whole pipeline. A plugin that rewrote User-Agent would re-file this request's
	// spend under a name of its choosing, and the request and response events could
	// disagree depending on which asked first. Latent while no plugin touches that header
	// — and silent on the day one does, which is why it is pinned rather than watched.
	pctx.ResolveClient()

	// SkipHosts short-circuit: forward as a transparent proxy. No
	// pipeline run, no body buffering, no session recording, no
	// response-phase work. RunFinish is also skipped (no defer
	// registered) because the pipeline never ran and has nothing to
	// finalize. See ListenerConfig.SkipHosts for motivation.
	//
	// Audit log: Match keys on r.Host (the agent-supplied Host header
	// at the listener boundary), and the request is then dialed against
	// r.URL via s.Client.Do(r). A forged Host that diverges from the
	// dial target would skip-match yet send to a different upstream —
	// the same trust shape as ext_proc's :authority. Logging the host
	// + matched pattern at INFO leaves a per-skip audit trail so
	// successful self-exemption isn't invisible.
	pat, skipped := s.SkipHosts.MatchPattern(pctx.Host)
	if skipped {
		slog.Info("forward-proxy: skip_hosts match — bypassing pipeline + session recording",
			"host", pctx.Host, "pattern", pat, "method", r.Method, "path", r.URL.Path)
	}

	// Finisher dispatch runs after every exit path. RunFinish is a
	// no-op when pctx.dispatched is empty (pre-pipeline rejects), so
	// this defer is safe on every path including the body-too-large
	// early return. Suppressed when skipped because no plugin saw
	// this request and there is nothing to finalize.
	if !skipped {
		defer func() {
			s.OutboundPipeline.RunFinish(r.Context(), pctx, pipeline.OutcomeFromContext(pctx))
		}()
	}

	if !skipped && s.OutboundPipeline.NeedsRequestBody() && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			bodyread.LogError("forward-proxy", r, len(body), maxBodySize, err)
			status, msg := bodyread.Rejection(err)
			http.Error(w, msg, status)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		pctx.Body = body
		slog.Debug("forward-proxy: buffered request body", "host", r.Host, "bodyLen", len(body))
	}

	// Establish the session identity ONCE, here, and keep it in a local: the
	// identity handed to plugins and the bucket events are recorded under must be
	// the same answer, not two rules that happen to agree. Plugins key on
	// pctx.Session.ID (sessionbudget's Redis counters, contextguru's compaction
	// state, sparc), so resolving this from ActiveSession() alone attributed two
	// concurrent coding-agent sessions to whichever spoke last.
	//
	// A local rather than pctx.OutboundSessionID, deliberately. That field is
	// exported and the pipeline runs between here and every recording site, so a
	// plugin could otherwise write it and silently re-file both the request and
	// its paired response — which resolveOutboundSessionID's own contract forbids,
	// and which would fail invisibly. The listener's answer wins; the pin is
	// assigned from this local after the pipeline has run.
	var sessionID string
	if !skipped && s.Sessions != nil {
		if sessionID = s.resolvePluginSessionID(r.Header); sessionID != "" {
			pctx.Session = s.sessionViewFor(sessionID)
		}
	}

	if !skipped {
		action := s.OutboundPipeline.Run(r.Context(), pctx)

		if action.Type == pipeline.Reject {
			s.recordOutboundRejectIn(tl, pctx, action, s.recordingSessionID(sessionID, r.Header))
			// Render as a JSON-RPC error frame when the rejected
			// request was MCP JSON-RPC, so the agent's MCP client
			// surfaces this as one failed tool call rather than a
			// transport break. Falls through to plain HTTP-level
			// rejection for non-MCP traffic.
			httpx.WriteRejectionForRequest(w, action, pctx)
			return
		}
	}

	if !skipped && s.Sessions != nil {
		sid := s.recordingSessionID(sessionID, r.Header)
		// Pin this session so the paired response event records into the
		// same bucket. Without it, recordOutboundResponseEvent re-resolves
		// ActiveSession() at response time, which interleaving traffic (a
		// health probe under "default") can flip mid-stream — mis-filing a
		// streaming inference response away from its request's session.
		pctx.OutboundSessionID = sid
		// Snapshot-copy the protocol extension so the request event
		// doesn't see response-phase mutations on the same MCP/Inference
		// struct (e.g. token counts assigned in OnResponse).
		plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
		ev := pipeline.SessionEvent{
			At:          time.Now(),
			Direction:   pipeline.Outbound,
			Phase:       pipeline.SessionRequest,
			RequestID:   pctx.RequestID(),
			MCP:         pipeline.SnapshotMCP(pctx.Extensions.MCP),
			Inference:   pipeline.SnapshotInference(pctx.Extensions.Inference),
			Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
			Plugins:     plugins,
			Identity:    pipeline.SnapshotIdentity(pctx),
			Host:        pctx.Host,
			HTTPMethod:  pctx.Method,
			HTTPPath:    pctx.Path,
			Client:      pctx.ClientInfo(),
		}
		// Record EVERY message that reaches the pipeline — even when no
		// plugin acted and no parser matched (Invocations/MCP/Inference all
		// nil). The session API is an observability surface; a request the
		// pipeline saw but no plugin touched is still a network message the
		// operator wants to see (it carries Host, and the paired response
		// carries StatusCode). skip_hosts traffic never reaches here (the
		// !skipped guard above), so it stays suppressed by design.
		s.appendOutbound(tl, sid, ev)
	}

	// Propagate every header mutation the outbound pipeline made to the
	// forwarded request. pctx.Headers started as a clone of r.Header, so
	// plugins' set / replace / delete operations on it are the intended
	// upstream-facing header set. Only Authorization used to be forwarded,
	// silently dropping any other injected header (e.g. static-inject's
	// x-api-key). Content-Length / Content-Encoding are managed by the
	// body-rewrite block below and the transport, so leave them untouched.
	// Mirrors reverseproxy's forwarded-request header sync.
	skip := func(k string) bool {
		return strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding")
	}
	for k := range r.Header {
		if skip(k) {
			continue
		}
		if _, ok := pctx.Headers[k]; !ok {
			r.Header.Del(k) // plugin removed it
		}
	}
	for k, vv := range pctx.Headers {
		if skip(k) {
			continue
		}
		if len(vv) == 0 {
			r.Header.Del(k) // pctx.Headers[k] = nil is a delete, same as Del(k)
			continue
		}
		r.Header[k] = append([]string(nil), vv...) // set / overwrite
	}

	// If a WritesRequestBody plugin rewrote pctx.Body, ship the new bytes
	// upstream and clear Content-Encoding (see forwardproxy response
	// path for the rationale).
	if pctx.BodyMutated() {
		r.Body = io.NopCloser(bytes.NewReader(pctx.Body))
		r.ContentLength = int64(len(pctx.Body))
		r.Header.Set("Content-Length", fmt.Sprintf("%d", len(pctx.Body)))
		r.Header.Del("Content-Encoding")
	}

	// Remove hop-by-hop headers
	r.Header.Del("Connection")
	r.Header.Del("Keep-Alive")
	r.Header.Del("Proxy-Authenticate")
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Proxy-Connection")
	r.Header.Del("TE")
	r.Header.Del("Trailer")
	r.Header.Del("Transfer-Encoding")
	r.Header.Del("Upgrade")

	// Strip the client's Accept-Encoding so Go's transport negotiates content
	// coding on its own behalf.
	//
	// net/http auto-decompresses a gzip response ONLY when the transport added
	// Accept-Encoding itself. Forwarding the caller's header suppresses that:
	// resp.Body then yields raw compressed bytes. Every body-reading plugin
	// sees binary — the SSE re-framer finds no "data:" lines and reports a
	// clean EOF on its first ReadFrame, so a gzipped text/event-stream relayed
	// zero frames downstream and finalized aggregating plugins on empty state.
	// The symptom is silent in both directions: the client sees a stream that
	// opens and dies, and token telemetry reads as absent rather than wrong.
	//
	// This proxy re-frames and inspects bodies, so it cannot be encoding-blind.
	// Taking ownership of the negotiation means the transport hands us
	// plaintext and strips Content-Encoding from resp.Header, keeping the
	// bytes we relay consistent with the headers we forward.
	//
	// Gated on a plugin actually inspecting the response body, mirroring the
	// reverse proxy (see listener/reverseproxy: same reasoning, same
	// condition). When nothing reads the body this listener is a pure
	// pass-through, so leaving the client's header intact avoids forcing an
	// upstream→client decompression that buys nothing — which matters for a
	// remote backend or a large non-streamed body, and is harmless either way
	// on a loopback sidecar hop.
	if !skipped && (s.OutboundPipeline.NeedsResponseBody() || s.OutboundPipeline.HasStreamingResponders()) {
		r.Header.Del("Accept-Encoding")
	}

	// Clear RequestURI — set by the server but must be empty for client requests
	r.RequestURI = ""

	client := s.Client
	if isBridge && s.TLSBridge != nil {
		client = s.TLSBridge.Upstream
	}
	resp, err := client.Do(r)
	if err != nil {
		slog.Warn("forward-proxy: upstream request failed", "host", r.Host, "method", r.Method, "error", err)
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		// Without this the request event is the only trace, and a row with no
		// status reads as a request still in flight (#1045). Same reason the
		// CONNECT dial-failure path below records.
		//
		// The 502 goes on the EVENT ONLY, never onto pctx.StatusCode:
		// OutcomeFromContext reads zero as OutcomeError and any non-zero as
		// OutcomeAllow, so assigning it would tell every Finisher this
		// succeeded and have lineage label the span "ok".
		if !skipped {
			s.recordOutboundResponseEvent(pctx, http.StatusBadGateway, pipeline.TransportError(err))
		}
		return
	}
	defer resp.Body.Close()

	// Response phase: populate pctx and run plugins in reverse order.
	pctx.StatusCode = resp.StatusCode
	pctx.ResponseHeaders = resp.Header.Clone()

	// SkipHosts: bypass response-phase pipeline + recording entirely.
	// Stream the upstream body straight through to the caller. Falls
	// out below to the unconditional header copy + io.Copy.
	if !skipped {
		// Branch on Content-Type per response. The Streamable HTTP transport
		// lets the server pick application/json vs text/event-stream per
		// response (the client Accepts both), so the same tool may return
		// JSON on one call and SSE on the next. Decide here rather than
		// negotiating, and don't take the streaming path when a plugin
		// declares WritesRequestBody (mutating a body we've already started
		// forwarding is incompatible with streaming) — fall back to
		// buffered with a warning log instead.
		if isEventStream(resp.Header.Get("Content-Type")) && resp.Body != nil {
			if s.OutboundPipeline.WritesResponseBody() {
				// A response mutator needs the whole response to rewrite it, so
				// it can't stream — fall back to the buffered path with a warning.
				// A request-only mutator does NOT land here: it never touches
				// these bytes, so the relay stays incremental.
				// Once, not per response: a response mutator on an SSE chain is a
				// supported configuration (cpex, sparc), not a misconfiguration, so
				// warning every request is log spam at request rate.
				s.bufferedFallbackOnce.Do(func() {
					slog.Info("forward-proxy: text/event-stream responses will use the buffered path — a WritesResponseBody plugin is in the chain", "host", r.Host)
				})
			} else if s.OutboundPipeline.HasStreamingResponders() {
				// Streaming-aware plugins (inference-parser, a2a-parser) parse
				// each SSE frame; handleStreamingResponse re-frames via sseframe.
				s.handleStreamingResponse(w, r, resp, pctx)
				return
			} else {
				// No streaming responder: relay the SSE stream byte-for-byte
				// with per-write flushing. Re-framing (handleStreamingResponse)
				// would drop the event:/id:/retry: lines that generic SSE
				// clients (e.g. an MCP Streamable HTTP client) depend on. Fixes #642.
				//
				// A plugin that declares ReadsBody (but not WritesRequestBody, and is
				// not a StreamingResponder) also lands here, and its OnResponse
				// runs against an empty pctx.ResponseBody: streamPassthrough
				// forwards the stream without buffering it. We deliberately don't
				// buffer to satisfy such a plugin — that would reintroduce the
				// #642 timeout on a live stream. A plugin that must inspect a
				// streamed body should implement StreamingResponder. Warn
				// (mirroring the WritesRequestBody fallback above) so the
				// misconfiguration surfaces instead of the plugin silently seeing
				// no body. Reaching this branch only rules out WritesResponseBody and
				// HasStreamingResponders — a request-only mutator can still be here —
				// so ask about the response side specifically rather than asserting
				// what NeedsBody implies.
				if s.OutboundPipeline.NeedsResponseBody() {
					slog.Warn("forward-proxy: text/event-stream response with a ReadsBody plugin that is not a StreamingResponder — streaming byte-for-byte; its OnResponse will see an empty body (implement StreamingResponder to inspect a streamed body)", "host", r.Host)
				}
				s.streamPassthrough(w, r, resp, pctx)
				return
			}
		}

		if s.OutboundPipeline.NeedsResponseBody() && resp.Body != nil {
			// Both failures below record the UPSTREAM's status, not the 502 we
			// answer with, and say proxy_error: the upstream replied fine and we
			// could not buffer what it sent. Blaming it for our own ceiling would
			// send an operator looking at the wrong end. Recorded at all for the
			// same reason as the transport-failure path above (#1045) — these
			// returns left no trace either.
			respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
			if err != nil {
				slog.Warn("forward-proxy: response body read error", "host", r.Host, "error", err)
				http.Error(w, `{"error":"response body read error"}`, http.StatusBadGateway)
				s.recordOutboundResponseEvent(pctx, resp.StatusCode, &pipeline.EventError{
					Kind:    "proxy_error",
					Message: err.Error(),
				})
				return
			}
			if len(respBody) > maxBodySize {
				slog.Warn("forward-proxy: response body too large", "host", r.Host, "len", len(respBody))
				http.Error(w, `{"error":"response body too large"}`, http.StatusBadGateway)
				s.recordOutboundResponseEvent(pctx, resp.StatusCode, &pipeline.EventError{
					Kind:    "proxy_error",
					Message: fmt.Sprintf("response body too large (%d bytes)", len(respBody)),
				})
				return
			}
			pctx.ResponseBody = respBody
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
		}

		// DETACHED AND BOUNDED, because the body above is already whole. Everything from here on
		// is finalization: a client that hung up during that read leaves a done context, and
		// RunResponse refuses a done context before calling any plugin — returning a Deny this
		// call site cannot tell from a policy reject, since it only tests action.Type. It would
		// then write a rejection and RETURN, skipping the settle and the response row for a
		// response that arrived complete. This is the BUFFERED OUTBOUND PATH, which is where
		// most non-streamed inference responses go.
		//
		// ONE CONTEXT PER DISPATCH, which is the rule at every finalization site in this tree: the
		// dispatches run in order, so a shared budget means whatever the response phase spends is
		// taken from the TERMINAL frame — the dispatch that turns folded state into a charge. See
		// httpx.TeardownContext, and the dispatch-site table in listener/parity.
		phaseCtx, cancelPhase := httpx.TeardownContext(r.Context())
		defer cancelPhase()

		respAction := s.OutboundPipeline.RunResponse(phaseCtx, pctx)
		if respAction.Type == pipeline.Reject {
			httpx.WriteRejection(w, respAction)
			return
		}

		// Streaming-aware plugins use a single code path for both shapes:
		// for the buffered application/json case we deliver the whole body
		// as one last=true frame so plugins finalize their running state.
		// Plugins that didn't migrate — i.e. don't implement
		// StreamingResponder — are unaffected (RunResponseFrame skips them).
		if s.OutboundPipeline.HasStreamingResponders() && resp.Body != nil {
			// Its own budget, per the rule above.
			finalCtx, cancelFinal := httpx.TeardownContext(r.Context())
			defer cancelFinal()
			respFrameAction := s.OutboundPipeline.RunResponseFrame(finalCtx, pctx, pctx.ResponseBody, true)
			if respFrameAction.Type == pipeline.Reject {
				httpx.WriteRejection(w, respFrameAction)
				return
			}
		}

		// A plugin that called pctx.SetResponseBody flipped the mutation flag.
		// Use the replaced bytes and rewrite Content-Length so the downstream
		// client gets a consistent response. Content-Encoding is cleared
		// because the framework can't know if the plugin also decompressed;
		// safer to ship plain bytes than a broken archive.
		if pctx.ResponseBodyMutated() {
			resp.Body = io.NopCloser(bytes.NewReader(pctx.ResponseBody))
			resp.ContentLength = int64(len(pctx.ResponseBody))
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(pctx.ResponseBody)))
			resp.Header.Del("Content-Encoding")
		}

		s.recordOutboundResponseEvent(pctx, resp.StatusCode, nil)
	}

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Debug("response copy error", "host", r.Host, "error", err)
	}
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
// false path the caller tunnels and records the close itself.
func (s *Server) bridgeServe(client net.Conn, authority, host string, tl *tunnelLog) bool {
	// 1) Verify upstream reachability + cert via the dedicated client, BEFORE forging.
	//    HEAD avoids GET side-effects; a non-2xx status still returns err==nil (cert
	//    verified), which is all we need. Only a transport/TLS error fails here. The
	//    verify is bounded by its own context timeout so a slow/stalled origin can't
	//    pin the bridging goroutine — the timeout is on this probe ONLY, not on the
	//    relay (which must stay unbounded for streaming responses).
	ctx, cancel := context.WithTimeout(context.Background(), upstreamVerifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+authority, nil)
	if err != nil {
		slog.Info("tls-bridge passthrough", "host", host, "reason", "upstream-verify", "error", err)
		tl.open(pipeline.TunnelOriginUnverified)
		return false
	}
	resp, err := s.TLSBridge.Upstream.Do(req)
	if err != nil {
		slog.Info("tls-bridge passthrough", "host", host, "reason", "upstream-verify", "error", err)
		tl.open(pipeline.TunnelOriginUnverified)
		return false // fall back to plain tunnel — agent's own e2e TLS still reaches origin
	}
	_ = resp.Body.Close()

	// 2) Forge + terminate downstream.
	tconn, err := s.TLSBridge.Term.Terminate(client, hostOnly(authority))
	if err != nil {
		reason := handshakeFailureReason(err)
		// Seed a skip either way — the forged handshake killed this connection, so the
		// client's retry needs a tunnel whatever went wrong. But only ESCALATE for a
		// real rejection: that is the one failure class that is evidence of a
		// persistent trust problem. A client that merely cancels requests, or one
		// tripping a cipher mismatch, would otherwise walk this host up to the ceiling
		// and take every other client's observability with it — the same defect this
		// change exists to fix, one level down.
		//
		// The decision is here rather than inside SkipSet because the reason vocabulary
		// belongs to the session-event layer, and tlsbridge has no other business
		// knowing about it.
		if reason == pipeline.TunnelClientRejectedCA {
			s.TLSBridge.Skip.Fail(host)
		} else {
			s.TLSBridge.Skip.FailTransient(host)
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
			"host", host,
			"reason", reason,
			"client", clientAddr(client),
			"error", err,
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
		slog.Warn("tls-bridge passthrough", args...)
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
	// Bridged: defer the open, with no reason, which is what tells agentop to fold this
	// row into the decrypted inner request whose own action is the interesting one.
	markBridged(tl)

	// 3) Serve the decrypted conn through the UNCHANGED pipeline.
	tlsbridge.ServeConn(tconn, s.bridgedHandler(authority, tl))
	// ServeConn returns once the connection has closed. A bridged tunnel's decrypted
	// requests answer it and agentop folds the open into the first of them, so a close row
	// there would render as an orphan response. With no recorded request at all there is
	// nothing to fold into, so finish records the open and a close — now, or when the last
	// handler still running returns.
	tl.finish()
	return true
}

// bridgedHandler serves one bridged tunnel's decrypted requests through the pipeline.
func (s *Server) bridgedHandler(authority string, tl *tunnelLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tl.admit() {
			return
		}
		defer tl.release()
		r.URL.Scheme = "https"
		r.URL.Host = authority // host:port — preserves non-443 origins
		s.serveOutbound(w, r, tl)
	})
}

// resolveOutboundSessionID picks the bucket an outbound event is recorded
// under, in descending order of trustworthiness:
//
//  1. A session id the client put on the request (SessionIDHeaders). This is
//     the only source that can tell two concurrent agent sessions apart:
//     ActiveSession() is a single global "most recently updated" id, so with
//     two Claude Code windows open, whichever spoke last would swallow the
//     other's events.
//  2. ActiveSession(), which correlates an agent's outbound calls with the
//     inbound A2A turn that caused them — the in-cluster case, where the
//     agent is not the one holding the session id.
//  3. The default bucket, for traffic belonging to neither.
//
// clientHeaders must be the headers as RECEIVED (r.Header), not pctx.Headers.
// The two diverge: pctx.Headers starts as a clone of r.Header but the outbound
// pipeline has already run by the time events are recorded, so it holds the
// upstream-facing set including any plugin rewrite (staticinject writes a
// configurable target header, so this is reachable by configuration, not just
// in theory). A bucket key means "what the client claimed about its session", so
// a plugin rewriting a header for the gateway's benefit must not silently re-file
// telemetry — and it would fail invisibly, since "" means "fall back", never an
// error. Pass nil where there is no request to read (opaque tunnels); resolution
// then falls through to the two fallbacks below.
//
// Safe to call with s.Sessions == nil: ActiveSession() is skipped and the default
// bucket is returned, so a future caller that forgets the nil check on Sessions
// gets a usable answer rather than a panic.
func (s *Server) resolveOutboundSessionID(clientHeaders http.Header) string {
	if sid := s.resolvePluginSessionID(clientHeaders); sid != "" {
		return sid
	}
	return session.DefaultSessionID
}

// recordingSessionID turns the identity resolved at hydration into the bucket an
// event is recorded under. resolved is the listener's own answer, carried in a
// local so no plugin can rewrite it; "" means hydration found no identity (or ran
// on a path that has none), and recording — unlike a plugin — must still file the
// event somewhere, so the full order including the default bucket applies.
func (s *Server) recordingSessionID(resolved string, clientHeaders http.Header) string {
	if resolved != "" {
		return resolved
	}
	return s.resolveOutboundSessionID(clientHeaders)
}

// appendOutbound records ev under sid. For a request decrypted from a bridged tunnel tl is
// that tunnel, and the tunnel's first recorded row also records its open; see
// tunnelLog.recordWith.
func (s *Server) appendOutbound(tl *tunnelLog, sid string, ev pipeline.SessionEvent) {
	if tl != nil && tl.recordWith(sid, ev) {
		return
	}
	s.Sessions.Append(sid, ev)
}

// tunnelSessionID is recordingSessionID for a tunnel's own row. Under client affinity
// resolved is "" only for affinity's ambiguous answer or for no identity at all, and
// neither sends a tunnel row to default: a CONNECT rarely carries a User-Agent, so the
// ambiguous answer would file nearly every tunnel away from the request it carries.
// Those rows keep ActiveSession() at recording time, as without affinity.
func (s *Server) tunnelSessionID(resolved string, clientHeaders http.Header) string {
	if s.ClientAffinity && resolved == "" && s.Sessions != nil {
		if sid := s.Sessions.ActiveSession(); sid != "" {
			return sid
		}
		return session.DefaultSessionID
	}
	return s.recordingSessionID(resolved, clientHeaders)
}

// resolvePluginSessionID is resolveOutboundSessionID without the default-bucket
// fallback: it returns "" when neither a client header nor an active session
// names one, and is the identity handed to plugins.
//
// The missing fallback is the point. Recording must file an event somewhere, so
// "default" is the right last resort there. A plugin asking "which session is
// this?" must be able to hear "nothing known" — handing it ID "default" would
// silently satisfy sessionbudget's DefaultSessionFallback, an opt-in config, and
// start enforcing budgets against a shared bucket where today it deliberately
// skips (see its no_session_id path). Same precedence otherwise, so the two
// answers cannot diverge on any request that has an identity at all.
//
// TRUST — the decision this closes, recorded because it was previously deferred:
// the id may be client-asserted, and handing it to plugins means a client can
// name which session's state it is handed. Session is documented as an
// observability and correlation key rather than an authorization subject (see
// pipeline.Context.Session), and this resolution assumes that. IBAC is the plugin
// that tests the assumption, because it reads Session.LastIntent() to judge an
// outbound call and can deny on it — so where header bucketing is enabled, a
// client naming another session's id can have its action judged against that
// session's recorded intent. Before this, that required winning an
// ActiveSession() race; naming a header is deterministic. Session ids are also
// enumerable through the unauthenticated session API.
//
// Accepted for the case this is built for: a laptop, where the user owns every
// session and the goal is telling their own concurrent sessions apart. NOT
// acceptable where session attribution is a trust boundary — a shared or
// standalone forward proxy, or any deployment running an authorization plugin on
// session state. Those must set session.id_headers to an empty list, which
// returns this to ActiveSession() only.
//
// The open alternative is to narrow what plugins receive for a client-asserted
// id — the ID, which is all that sessionbudget, contextguru and sparc key on, but
// not the recorded events, so no client can adopt another session's intent. Its
// cost is sparc's InferenceRequests() correlation for header-identified sessions.
// Stated in full under "Decision recorded: client-asserted ids as plugin input"
// in #984, which is where to argue with it.
func (s *Server) resolvePluginSessionID(clientHeaders http.Header) string {
	affinity := s.affinityOn()
	if sid := session.IDFromHeaders(clientHeaders, s.SessionIDHeaders); sid != "" {
		if affinity {
			s.Sessions.Claim(sid, affinityClient(clientHeaders))
		}
		return sid
	}
	if affinity {
		// See session.Store.SessionForClient for the order. The default bucket is its
		// "ambiguous" answer, and plugins hear that as "" — the no-identity answer this
		// function exists to give them — while recording files it under default.
		switch sid := s.Sessions.SessionForClient(affinityClient(clientHeaders)); sid {
		case "":
		case session.DefaultSessionID:
			return ""
		default:
			return sid
		}
	}
	if s.Sessions != nil {
		if sid := s.Sessions.ActiveSession(); sid != "" {
			return sid
		}
	}
	return ""
}

// affinityOn reports whether header-less requests are filed by coding agent. It needs
// header bucketing: a client's session is the one its header named, so with id_headers
// empty no session is ever any client's and every known agent would collect in its
// pending bucket forever.
func (s *Server) affinityOn() bool {
	return s.ClientAffinity && s.Sessions != nil && len(s.SessionIDHeaders) > 0
}

// affinityClient names the coding agent behind a request for session attribution. Read
// from the headers as RECEIVED, like the session header beside it, so it is the same
// User-Agent pctx.ResolveClient pins. Nil headers — the transparent path — name nobody.
func affinityClient(clientHeaders http.Header) string {
	if clientHeaders == nil {
		return ""
	}
	return pipeline.ParseUserAgent(clientHeaders.Get("User-Agent")).AffinityName()
}

// sessionViewFor returns the recorded view for sid, or an empty view carrying
// just the id when nothing has been recorded under it yet.
//
// Hydration runs BEFORE the request event is appended, so on a session's first
// request Store.View returns nil even though the id is known. Passing that nil
// through would tell every plugin "no session" on the first call of every
// session, and sessionbudget skips enforcement entirely on an empty id — so a
// client rotating session ids would never be metered. An empty Events slice is
// also the truthful answer: the session exists, and nothing has been recorded
// under it yet.
//
// Synthesizing rather than creating the bucket keeps this a read: hydration must
// not have the side effect of minting store entries for traffic that may still
// be rejected before anything is recorded.
func (s *Server) sessionViewFor(sid string) *pipeline.SessionView {
	if s.Sessions != nil {
		if v := s.Sessions.View(sid); v != nil {
			return v
		}
	}
	return &pipeline.SessionView{ID: sid}
}

// recordOutboundResponseEvent emits the SessionResponse event for a
// completed outbound response. Extracted from handleRequest so the
// streaming path can call it once at end-of-stream and the buffered
// path can call it once after RunResponse — both go through the same
// gate and snapshotting logic.
//
// fail is set only on the transport-failure path, where DeriveError cannot
// supply an error because pctx.StatusCode stays zero (see that call site). Nil
// everywhere else.
func (s *Server) recordOutboundResponseEvent(pctx *pipeline.Context, statusCode int, fail *pipeline.EventError) {
	if s.Sessions == nil {
		return
	}
	// Prefer the session pinned when the request event was recorded so the
	// response lands in the same bucket. Fall back to ActiveSession() only
	// when nothing was pinned (defensive — a response-only path), then to
	// the default bucket. See Context.OutboundSessionID.
	sid := pctx.OutboundSessionID
	if sid == "" {
		sid = s.Sessions.ActiveSession()
	}
	if sid == "" {
		sid = session.DefaultSessionID
	}
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionResponse,
		RequestID:   pctx.RequestID(),
		MCP:         pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:   pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		StatusCode:  statusCode,
		Error:       pipeline.EventErrorOr(pctx, fail),
		Duration:    pipeline.DurationSince(pctx.StartedAt),
		Client:      pctx.ClientInfo(),
	}
	// Always record — see the request-phase comment. This is what surfaces
	// responses no plugin acted on (e.g. a generic 404), carrying StatusCode
	// + Error even with empty invocations.
	s.Sessions.Append(sid, ev)
}

// isEventStream reports whether a Content-Type header value names the
// SSE media type. Content-Type may carry parameters (charset=, boundary=,
// etc.) so we match on the bare type/subtype prefix and tolerate any
// suffix. Case-insensitive per RFC 9110 §8.3.1.
func isEventStream(contentType string) bool {
	if contentType == "" {
		return false
	}
	// Strip parameters: "text/event-stream; charset=utf-8" → "text/event-stream".
	if idx := strings.IndexByte(contentType, ';'); idx >= 0 {
		contentType = contentType[:idx]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream")
}

// handleStreamingResponse forwards a text/event-stream response to the
// downstream client frame-by-frame. Each parsed SSE event is delivered
// to the pipeline's StreamingResponder plugins (recording-only today)
// and then written + flushed to the client immediately. End-of-stream
// is signaled to plugins with one final last=true call so aggregating
// plugins (inference-parser, a2a-parser) can finalize their running
// state. RunResponse is intentionally NOT invoked on this path —
// streaming-aware plugins move their finalization logic into
// OnResponseFrame(last=true), and legacy non-migrated plugins are
// not called on streaming responses (cleaner contract; no fragmented
// double-dispatch).
func (s *Server) handleStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, pctx *pipeline.Context) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher means the downstream connection can't deliver
		// bytes incrementally — fall back to buffered. http.Flusher
		// is supported by net/http's default ResponseWriter, so this
		// is a defensive guard for exotic wrappers (httptest with a
		// custom recorder, embedded servers).
		slog.Warn("forward-proxy: ResponseWriter does not support flushing — falling back to buffered for streaming response", "host", r.Host)
		s.streamFallbackBuffered(w, r, resp, pctx)
		return
	}

	// Defer the final last=true dispatch + session-event recording so
	// every exit path (normal EOF, upstream read error, downstream
	// client-write error) finalizes aggregating plugins and records
	// the response event. Without this, a client disconnect mid-stream
	// leaves inference/a2a stuck in an unfinalized state and emits no
	// SessionResponse row to agentop.
	defer func() {
		// Use a detached, BOUNDED context for finalization: the client may have
		// cancelled the request context after reading the full stream, but
		// aggregating plugins (inference-parser, session-budget) still need their
		// last=true dispatch to finalize state — and detaching alone would leave that
		// dispatch with nothing that could ever stop it. See httpx.TeardownContext.
		finalCtx, cancelFinal := httpx.TeardownContext(r.Context())
		defer cancelFinal()
		// DELIVERED: the headers and every frame are on the wire by now, which the Warn below
		// already says. Marking it keeps a late refusal from becoming this request's OUTCOME as
		// well — the same statement ext_proc's flush and the reverse proxy's finalize make.
		pctx.MarkResponseDelivered()
		finalAction := s.OutboundPipeline.RunResponseFrame(finalCtx, pctx, nil, true)
		if finalAction.Type == pipeline.Reject {
			// Headers already sent; we can't promote to 502, but
			// surface the policy violation so operators see it.
			slog.Warn("forward-proxy: streaming response rejected on finalization (headers already sent)",
				"host", r.Host, "violation", finalAction.Violation)
		}
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, nil)
	}()

	// Forward headers and the streaming status code BEFORE the first
	// frame is written. Strip Content-Length since we'll be writing
	// chunked, and clear hop-by-hop headers as net/http would.
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	reader := sseframe.NewReader(idleReader(resp.Body, streamReadIdleTimeout), maxBodySize)
	bytesWritten := 0
	for {
		frame, err := reader.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Read error or oversized single frame. The client has
			// already received some frames; the cleanest signal is to
			// close the connection and log. We can't promote this to
			// 502 — headers are sent.
			slog.Warn("forward-proxy: streaming response read error", "host", r.Host, "error", err, "bytesWritten", bytesWritten)
			break
		}

		// Record-only dispatch: invoke plugins then write+flush.
		// A future enforcement-aware version can inspect-before-forward;
		// see StreamingResponder doc.
		respAction := s.OutboundPipeline.RunResponseFrame(r.Context(), pctx, frame, false)
		if respAction.Type == pipeline.Reject {
			// Headers + earlier frames already on the wire — log and
			// stop forwarding. The downstream client sees a truncated
			// stream, which is the best we can do without inspect-
			// before-forward semantics.
			slog.Warn("forward-proxy: streaming response rejected mid-stream by plugin",
				"host", r.Host, "violation", respAction.Violation)
			break
		}

		// Write the frame back as one or more SSE data lines. The
		// sseframe reader folds multi-line `data:` events with `\n`
		// separators per the spec; re-split here so each original line
		// gets its own `data: ` prefix and the downstream parser sees
		// the same event boundaries the upstream produced. For the
		// single-line JSON-RPC payloads this targets, this loop is
		// equivalent to writing `data: <frame>\n\n` once.
		if !writeSSEFrame(w, frame) {
			slog.Debug("forward-proxy: streaming write error", "host", r.Host)
			break
		}
		flusher.Flush()
		bytesWritten += len(frame)
	}
}

// streamPassthrough forwards a text/event-stream response to the downstream
// client byte-for-byte with per-write flushing. It is the streaming path when
// no StreamingResponder plugin is configured (a plain proxy pipeline). Unlike
// handleStreamingResponse it does NOT parse or re-frame the stream through
// sseframe — it relays the exact upstream bytes so that event:, id:, retry:,
// and comment lines survive, which generic SSE consumers such as an MCP
// Streamable HTTP client require. Without this path such a response fell
// through to an unflushed io.Copy and never reached the client until the
// upstream closed the connection (issue #642).
//
// Unlike handleStreamingResponse, the response-phase pipeline (RunResponse) IS
// run before the first byte. That is safe only because this path is reached
// exclusively when no StreamingResponder is configured, so RunResponse cannot
// double-dispatch a plugin that also handles OnResponseFrame. Running it lets
// header/status-based response gates fire on streamed responses too — e.g.
// opa's response-phase deny (status + headers) and litellm-budgettrack's cost
// accounting (a response header) — and a deny is honored before any byte is
// written. The plugins reachable here do not read pctx.ResponseBody in
// OnResponse, so leaving the body unbuffered is fine; body-level response
// inspection on a stream requires implementing StreamingResponder.
func (s *Server) streamPassthrough(w http.ResponseWriter, r *http.Request, resp *http.Response, pctx *pipeline.Context) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No incremental delivery possible on this ResponseWriter — buffer.
		// http.Flusher is implemented by net/http's default writer; this is a
		// defensive guard for test recorders and exotic wrappers.
		slog.Warn("forward-proxy: ResponseWriter does not support flushing — falling back to buffered for streaming response", "host", r.Host)
		s.streamFallbackBuffered(w, r, resp, pctx)
		return
	}

	// Run the response-phase pipeline before the first byte so header/status
	// gates still fire on a streamed response and a deny short-circuits before
	// anything is written. streamFallbackBuffered runs its own RunResponse, so
	// this is done only on the flushing path to avoid double-dispatch.
	if respAction := s.OutboundPipeline.RunResponse(r.Context(), pctx); respAction.Type == pipeline.Reject {
		httpx.WriteRejection(w, respAction)
		return
	}

	// Record the response event on every exit path (normal EOF, upstream read
	// error, downstream write error) so a SessionResponse row still lands.
	defer s.recordOutboundResponseEvent(pctx, resp.StatusCode, nil)

	// Forward headers + status before the first byte. Drop Content-Length since
	// we relay an open-ended chunked stream.
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	// Copy raw chunks and flush each so intermittent SSE events reach the client
	// immediately. idleReader bounds a wedged upstream; total size stays
	// unbounded so long-lived streams aren't cut off.
	body := idleReader(resp.Body, streamReadIdleTimeout)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				slog.Debug("forward-proxy: streaming write error", "host", r.Host, "error", writeErr)
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				slog.Warn("forward-proxy: streaming response read error", "host", r.Host, "error", readErr)
			}
			return
		}
	}
}

// streamFallbackBuffered handles the rare case of a streaming
// Content-Type response on a ResponseWriter that doesn't support
// http.Flusher — buffer the whole SSE body, then re-parse it through
// sseframe so streaming-aware plugins receive one OnResponseFrame call
// per SSE event followed by last=true. Without per-frame dispatch the
// inference parser (and any future fold-and-finalize plugin) would try
// to JSON-decode the whole SSE blob as one chunk, fail, and clobber a
// correctly-parsed completion. Production ResponseWriters implement
// http.Flusher so this path is mostly hit in tests.
func (s *Server) streamFallbackBuffered(w http.ResponseWriter, r *http.Request, resp *http.Response, pctx *pipeline.Context) {
	// Both buffering failures keep the upstream's status and say proxy_error, as
	// on the non-streaming path — see the comment there.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil {
		slog.Warn("forward-proxy: response body read error", "host", r.Host, "error", err)
		http.Error(w, `{"error":"response body read error"}`, http.StatusBadGateway)
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, &pipeline.EventError{
			Kind:    "proxy_error",
			Message: err.Error(),
		})
		return
	}
	if len(respBody) > maxBodySize {
		slog.Warn("forward-proxy: response body too large", "host", r.Host, "len", len(respBody))
		http.Error(w, `{"error":"response body too large"}`, http.StatusBadGateway)
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, &pipeline.EventError{
			Kind:    "proxy_error",
			Message: fmt.Sprintf("response body too large (%d bytes)", len(respBody)),
		})
		return
	}
	pctx.ResponseBody = respBody
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	// The same detach as the fold and the settle below, on the site the earlier fix walked
	// past: this runs after io.ReadAll too, so a hangup during the read denies the response
	// phase and returns before anything is recorded.
	phaseCtx, cancelPhase := httpx.TeardownContext(r.Context())
	defer cancelPhase()
	respAction := s.OutboundPipeline.RunResponse(phaseCtx, pctx)
	if respAction.Type == pipeline.Reject {
		httpx.WriteRejection(w, respAction)
		return
	}
	if s.OutboundPipeline.HasStreamingResponders() {
		// Re-parse the buffered SSE body frame-by-frame so plugins see the
		// same per-event shape as the real streaming path. A Reject is
		// honored here — headers are not yet on the wire.
		//
		// ON A DETACHED, BOUNDED CONTEXT, exactly as the streaming path's finish defer is, and
		// for a reason that is easy to miss here: the whole body is already in hand by this
		// point (io.ReadAll above), so a client that hung up while it was being read leaves a
		// cancelled r.Context() — and RunResponseFrame refuses a cancelled context before
		// calling any plugin, returning a Deny this loop cannot tell apart from a policy
		// reject. It would then write a rejection and RETURN, skipping
		// recordOutboundResponseEvent below: no settled cost and no response row for a
		// response that arrived complete. Detaching also restores the meaning of a Reject
		// here — with the cancellation case gone, one can only come from a plugin.
		//
		// TWO CONTEXTS, ONE PER PIECE OF WORK. The fold below dispatches every frame through
		// the pipeline, and a plugin doing anything slow per frame would spend the budget the
		// SETTLE needs — the terminal dispatch is the one that turns the folded state into a
		// charge, and it would inherit whatever was left. Each gets its own deadline, on the
		// same rule the reverse proxy's finalize() follows: the clock starts when the work
		// does, not when its parent did.
		foldCtx, cancelFold := httpx.TeardownContext(r.Context())
		defer cancelFold()
		reader := sseframe.NewReader(bytes.NewReader(respBody), maxBodySize)
		for {
			frame, ferr := reader.ReadFrame()
			if ferr == io.EOF {
				break
			}
			if ferr != nil {
				slog.Warn("forward-proxy: streaming response read error in fallback", "host", r.Host, "error", ferr)
				break
			}
			frameAction := s.OutboundPipeline.RunResponseFrame(foldCtx, pctx, frame, false)
			if frameAction.Type == pipeline.Reject {
				httpx.WriteRejection(w, frameAction)
				return
			}
		}
		// A FRESH DEADLINE FOR THE SETTLE. See the two-contexts note above: the fold has had
		// its own budget, and the dispatch that turns folded state into a charge gets a whole
		// one rather than the remainder.
		finalCtx, cancelFinal := httpx.TeardownContext(r.Context())
		defer cancelFinal()
		finalAction := s.OutboundPipeline.RunResponseFrame(finalCtx, pctx, nil, true)
		if finalAction.Type == pipeline.Reject {
			httpx.WriteRejection(w, finalAction)
			return
		}
	}
	s.recordOutboundResponseEvent(pctx, resp.StatusCode, nil)
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		slog.Debug("response copy error", "host", r.Host, "error", err)
	}
}

// recordOutboundReject emits a SessionDenied event for outbound
// requests a pipeline plugin rejected. Symmetric to the accept path's
// session recording (above). Lets guardrail plugins (rate-limit,
// intent-based, content policy) show operators what was blocked and
// why via /v1/sessions and agentop, instead of the block appearing only
// as a 4xx/5xx on the agent side.
//
// Skips when no Invocations were appended — the deny came from a
// plugin that didn't contribute diagnostic context, and a content-free
// SessionDenied event would be noise without attribution.
func (s *Server) recordOutboundReject(pctx *pipeline.Context, action pipeline.Action, sid string) {
	s.recordOutboundRejectIn(nil, pctx, action, sid)
}

// recordOutboundRejectIn is recordOutboundReject for a request decrypted from tl, whose
// deferred open — when the denial is the tunnel's first recorded row — goes in with it.
// tl is nil for a plaintext request or a CONNECT.
func (s *Server) recordOutboundRejectIn(tl *tunnelLog, pctx *pipeline.Context, action pipeline.Action, sid string) {
	if s.Sessions == nil || pctx.Extensions.Invocations == nil {
		return
	}
	var status int
	var code, message string
	if action.Violation != nil {
		status = action.Violation.Status
		if status == 0 {
			status = pipeline.StatusFromCode(action.Violation.Code)
		}
		code = action.Violation.Code
		message = action.Violation.Reason
	}
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionDenied,
		RequestID:   pctx.RequestID(),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Client: pctx.ClientInfo(),
	}
	s.appendOutbound(tl, sid, ev)
}

// connectDialTimeout bounds the upstream TCP dial for a CONNECT tunnel.
// Once the tunnel is open the timeout no longer applies — the agent's TLS
// handshake and subsequent traffic flow at their own pace.
const connectDialTimeout = 30 * time.Second

// handleConnect tunnels HTTPS (and any other TLS-wrapped protocol) through
// the forward proxy as raw TCP. Mirrors the TLS-passthrough behavior of
// envoy-sidecar mode: bytes are opaque to the proxy, so token-exchange and
// the protocol parsers (mcp-parser, inference-parser) are no-ops by
// definition. Pipeline gates (ibac, jwt-validation bypass logic, etc.)
// still run on the CONNECT request itself so they can reject based on
// destination host before the tunnel opens.
//
// mTLS is intentionally NOT applied to the upstream dial — the bytes
// flowing through this tunnel ARE the agent's own end-to-end TLS, and
// terminating that with sidecar-to-sidecar mTLS would break the agent's
// trust path. CONNECT targets are opaque externals (LiteMaaS, Bedrock,
// GitHub API, etc.) where the agent's existing TLS is the right answer.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	s.noteTunnel()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    r.Method, // always "CONNECT" here, but populated for parity with handleRequest
		Scheme:    "tcp",    // marker: bytes are opaque, not HTTP
		Host:      r.Host,
		Path:      "",
		Headers:   r.Header.Clone(),
		StartedAt: time.Now(),
	}
	// At construction, as in serveOutbound: the gate plugins run on this CONNECT before
	// recordTunnelOpened builds its event, so the pin is what keeps the tunnel row
	// attributed to the agent that opened it.
	pctx.ResolveClient()

	// SkipHosts short-circuit: open the tunnel without running the
	// pipeline or recording a session event. The pipeline never ran,
	// so there's nothing to RunFinish — defer is suppressed. Mirrors
	// handleRequest's skip path so HTTP and CONNECT-tunnel destinations
	// that match a skip pattern behave identically. Note the gate
	// plugin loss this implies: if your skip-host list includes a
	// destination you'd want IBAC or token-exchange to deny on, that
	// denial does not happen — the SkipHosts list is a "trusted
	// infrastructure" surface, not a generic per-route policy knob.
	//
	// CONNECT is safer-by-construction than the HTTP path: r.Host on
	// CONNECT is the dial target, so a forged Host header cannot
	// skip-match while dialing elsewhere — the proxy dials the same
	// "host:port" it matched. We still emit an audit log so a
	// successful skip leaves a trace.
	pat, skipped := s.SkipHosts.MatchPattern(pctx.Host)
	if skipped {
		slog.Info("forward-proxy: skip_hosts match (CONNECT) — opening tunnel without pipeline + recording",
			"host", pctx.Host, "pattern", pat)
	}

	var sessionID string
	if !skipped {
		defer func() {
			s.OutboundPipeline.RunFinish(r.Context(), pctx, pipeline.OutcomeFromContext(pctx))
		}()

		// Same one-identity rule as the request path, and the same local-variable
		// reason: the plugins that gate this tunnel and the bucket its denial
		// records under must be the same session, and the pipeline runs in
		// between, so the answer cannot live in a field a plugin can write. A
		// CONNECT request carries the client's own headers, so a client that
		// announces its session on CONNECT is honored; most do not, and those
		// fall through to ActiveSession() exactly as before.
		if s.Sessions != nil {
			if sessionID = s.resolvePluginSessionID(r.Header); sessionID != "" {
				pctx.Session = s.sessionViewFor(sessionID)
			}
		}

		// Run the outbound pipeline. Plugins that policy on host/identity
		// (ibac, content gates) still get to allow/deny; plugins that need
		// HTTP body (parsers) see no body, which they handle gracefully.
		action := s.OutboundPipeline.Run(r.Context(), pctx)
		if action.Type == pipeline.Reject {
			s.recordOutboundReject(pctx, action, s.tunnelSessionID(sessionID, r.Header))
			// Render as a JSON-RPC error frame when the rejected
			// request was MCP JSON-RPC, so the agent's MCP client
			// surfaces this as one failed tool call rather than a
			// transport break. Falls through to plain HTTP-level
			// rejection for non-MCP traffic.
			httpx.WriteRejectionForRequest(w, action, pctx)
			return
		}
	}

	// Under client affinity an opaque tunnel's row joins the session this CONNECT was
	// gated under, not ActiveSession() at recording time. A bridged tunnel's open follows
	// its first recorded request instead (tunnelLog.recordWith); only one that records no
	// request files under this pin, when it settles. Pinned only now, after the pipeline,
	// for the reason sessionID is a local; left unpinned where affinity had no answer, for
	// the reason in tunnelSessionID. See recordTunnelOpened.
	if s.ClientAffinity && !skipped && s.Sessions != nil && sessionID != "" {
		pctx.OutboundSessionID = sessionID
	}

	// Verify hijack capability BEFORE dialing upstream. If hijacking
	// isn't supported the failure mode should be a 500 to the client,
	// not a half-opened TCP connection to the upstream. The actual
	// Hijack() call happens after dial succeeds — http.Error needs an
	// un-hijacked ResponseWriter to deliver the dial-failure 502.
	if _, ok := w.(http.Hijacker); !ok {
		slog.Error("forward-proxy: ResponseWriter does not support hijacking", "host", r.Host)
		http.Error(w, `{"error":"connect not supported by listener"}`, http.StatusInternalServerError)
		return
	}

	// tl records this CONNECT's two timeline rows. The open is recorded exactly once, on
	// whichever path the CONNECT takes, carrying the reason the bytes stayed opaque; it
	// is not recorded here because the reason is not known yet — recording before the
	// bridge decision made the two most useful reasons unrepresentable, since both are
	// discovered inside bridgeServe. The close is recorded when the tunnel ends, or at
	// once when it never opened. Neither is recorded for a SkipHosts destination: no
	// plugin ran, so there is nothing to attribute them to.
	tl := s.newTunnelLog(pctx, skipped)

	// Plain TCP dial. See package-level comment on why mTLS doesn't
	// apply here. r.Host on a CONNECT carries "host:port" already.
	upstream, err := net.DialTimeout("tcp", r.Host, connectDialTimeout)
	if err != nil {
		slog.Warn("forward-proxy: CONNECT upstream dial failed", "host", r.Host, "error", err)
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		// Recorded rather than dropped: an unreachable destination used to leave no
		// trace in the timeline at all.
		tl.open(pipeline.TunnelDialFailed)
		tl.close(http.StatusBadGateway, dialError(err), 0, 0)
		return
	}

	clientConn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		_ = upstream.Close()
		slog.Error("forward-proxy: CONNECT hijack failed", "host", r.Host, "error", err)
		return
	}

	// TCP keepalive on both ends. Streaming LLM completions can hold
	// the tunnel open for minutes; without keepalives, a vanished peer
	// (network partition, NAT entry expiry, peer reboot) parks the
	// io.Copy goroutines until the OS finally times the socket out.
	// 30s is loose enough to not perturb idle traffic and tight enough
	// that operators get prompt cleanup on dead connections.
	enableKeepalive(upstream)
	enableKeepalive(clientConn)

	// Tell the agent the tunnel is up. Per RFC 7231 §4.3.6 a 200 to
	// CONNECT signals "tunnel established"; the body is empty and any
	// subsequent bytes from either side are application data.
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		_ = upstream.Close()
		slog.Debug("forward-proxy: CONNECT 200 write failed", "host", r.Host, "error", err)
		return
	}

	reason := pipeline.TunnelBridgeDisabled

	if s.TLSBridge != nil {
		pc := &peekedConn{Conn: clientConn, r: bufio.NewReaderSize(clientConn, sniffBufSize)}
		clientConn = pc // replay peeked bytes into whichever path runs
		first, _ := pc.Peek(5)
		authority := r.Host // CONNECT target is already host:port
		key := hostOnly(r.Host)
		if s.TLSBridge.Skip.Contains(key) {
			// Distinct from client-rejected-ca: this client may trust the CA
			// perfectly well and is being tunnelled because another one did not.
			reason = pipeline.TunnelSkipCached
		} else {
			v, why := s.TLSBridge.Decision.Classify(key, portOf(r.Host), first)
			reason = passthroughReason(why)
			if v == tlsbridge.Terminate {
				s.noteBridgeAttempt()
				_ = upstream.Close() // bridgeServe dials its own verified upstream
				if s.bridgeServe(clientConn, authority, key, tl) {
					return
				}
				// fell open → re-dial for the tunnel. That tunnel is opaque, so it closes
				// like any other opaque one.
				up2, derr := net.DialTimeout("tcp", r.Host, connectDialTimeout)
				if derr != nil {
					// The client already has its 200, so that stays the status, and the
					// error says why the tunnel died. Closing the client is what makes
					// that close row true: the hijacked conn is this handler's to close,
					// and it used to be left open for the client to time out on.
					_ = clientConn.Close()
					tl.close(http.StatusOK, dialError(derr), 0, 0)
					return
				}
				sent, received := tunnel(clientConn, up2)
				_ = up2.Close()
				tl.close(http.StatusOK, nil, sent, received)
				return
			}
		}
	}

	// Every path that reaches here left the bytes opaque without entering
	// bridgeServe, so this is where their reason gets recorded.
	tl.open(reason)

	// Bidirectional copy until either side closes.
	sent, received := tunnel(clientConn, upstream)
	tl.close(http.StatusOK, nil, sent, received)
}

// writeSSEFrame writes one SSE event built from a sseframe-decoded
// frame back to w. The decoder folds multi-line `data:` events with
// `\n` separators; this helper splits on those `\n`s and emits one
// `data: <line>\n` per original line followed by the blank-line
// terminator, so a downstream SSE parser sees the same event
// boundaries the upstream produced. Returns true when every byte
// was written; false on any write error so the caller can stop
// forwarding without re-checking each Write.
//
// The sseframe reader drops the SSE `event:` field (it surfaces only
// data payloads), which is correct for data-only streams (MCP/A2A
// JSON-RPC, OpenAI chat chunks). But Anthropic's Messages streaming
// REQUIRES a typed `event:` line before each `data:` — without it the
// Anthropic client can't finalize the stream and falls back to a
// non-streaming retry, doubling the upstream call. Reconstruct the
// `event:` line from the payload's top-level "type" (which, for an
// Anthropic stream event, is exactly the SSE event name). Frames
// without a top-level string "type" get no event line, preserving the
// data-only shape the other protocols expect.
func writeSSEFrame(w io.Writer, frame []byte) bool {
	if ev := sseEventName(frame); ev != "" {
		if _, err := io.WriteString(w, "event: "+ev+"\n"); err != nil {
			return false
		}
	}
	for len(frame) > 0 {
		nl := bytes.IndexByte(frame, '\n')
		var line []byte
		if nl < 0 {
			line = frame
			frame = nil
		} else {
			line = frame[:nl]
			frame = frame[nl+1:]
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(line); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return false
		}
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		return false
	}
	return true
}

// sseEventName returns the SSE `event:` name to emit for an SSE data
// payload, or "" when none should be emitted. It maps a frame to one of
// the known Anthropic Messages stream events via the payload's top-level
// JSON "type"; reconstructing that `event:` line keeps the relayed stream
// byte-faithful Anthropic SSE (the sseframe reader drops it).
//
// Two deliberate constraints:
//   - Fast path: skip the JSON parse entirely for frames that cannot carry
//     a top-level "type" (data-only JSON-RPC / OpenAI chat chunks with
//     "object" / "[DONE]"), so high-rate token streams stay allocation-free
//     on the proxy's hot data path.
//   - Allowlist: emit only the fixed set of Anthropic stream event names.
//     The "type" comes from an upstream the bridge does not trust, so
//     echoing it verbatim would let a crafted value inject SSE fields (a
//     CRLF in "type") or steer the client's event dispatch with an
//     arbitrary name. Exact-matching a constant set makes both impossible
//     and scopes reconstruction to Anthropic — a future data-only protocol
//     that happens to carry a top-level "type" (e.g. the OpenAI Responses
//     API) is left untouched.
func sseEventName(frame []byte) string {
	if !bytes.Contains(frame, []byte(`"type"`)) {
		return ""
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return ""
	}
	switch probe.Type {
	case "message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop", "ping", "error":
		return probe.Type
	default:
		return ""
	}
}

// idleReader wraps r so each Read enforces an idle deadline. The
// goroutine pattern (timer reset on every Read entry, cancelled on
// every Read exit) is portable across any io.ReadCloser, unlike
// SetReadDeadline which only applies to net.Conn — and for HTTPS
// upstreams the proxy holds the *http.Response.Body, not the
// underlying conn. On idle expiry the reader closes the body, which
// causes the in-flight Read to return an error and unblocks the
// caller. Subsequent Reads return the same close error.
//
// The wrapper does not buffer; bufio's reader inside sseframe.Reader
// continues to do that. The deadline is per-Read, not per-frame, so
// a long-running tool that emits one byte every minute (within the
// idle window) keeps the stream alive. The streamReadIdleTimeout
// constant captures the wall-clock budget.
//
// Race-with-success note: time.AfterFunc + timer.Stop() does NOT
// wait for an already-fired callback. If the timer fires just as a
// Read returns successfully, the close runs after the success and
// would leave the next Read failing under a healthy upstream. The
// closeOnce field makes the close idempotent and Close() also runs
// it, so a stray late timer is harmless: the underlying body is
// closed at most once, and a successful in-flight Read keeps its
// data either way. The wider hazard — closing the body concurrently
// with an active Read — is the documented unblock mechanism the
// stdlib http transport relies on for forced disconnects.
type idleReadCloser struct {
	rc        io.ReadCloser
	timeout   time.Duration
	closeOnce sync.Once
}

func idleReader(rc io.ReadCloser, timeout time.Duration) io.ReadCloser {
	return &idleReadCloser{rc: rc, timeout: timeout}
}

func (i *idleReadCloser) Read(p []byte) (int, error) {
	timer := time.AfterFunc(i.timeout, i.closeIdempotent)
	n, err := i.rc.Read(p)
	timer.Stop()
	return n, err
}

func (i *idleReadCloser) Close() error {
	i.closeIdempotent()
	return nil
}

func (i *idleReadCloser) closeIdempotent() {
	i.closeOnce.Do(func() { _ = i.rc.Close() })
}

// enableKeepalive turns on TCP keepalive with a 30s probe interval on
// the underlying *net.TCPConn, if conn unwraps to one. No-op on other
// connection types (notably *tls.Conn, which doesn't apply on the
// CONNECT path since the bytes through the tunnel are already TLS).
func enableKeepalive(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcp.SetKeepAlive(true)
	_ = tcp.SetKeepAlivePeriod(30 * time.Second)
}

// hostOnly strips the port from an authority ("h:443" → "h"); returns input if no port.
func hostOnly(authority string) string {
	if h, _, err := net.SplitHostPort(authority); err == nil {
		return h
	}
	return authority
}

// portOf returns the port from an authority, defaulting to 443.
func portOf(authority string) int {
	if _, p, err := net.SplitHostPort(authority); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 443
}

// tunnelWarnThreshold is how many tunnels may open with nothing decrypted
// before the listener speaks up. A handful is normal — passthrough hosts, a
// non-HTTPS CONNECT, the first request racing startup — so warning on the
// first one would cry wolf. By this many, with zero bridged requests, the
// client is not trusting the CA.
const tunnelWarnThreshold = 5

// noteTunnel counts a CONNECT tunnel. It deliberately does NOT warn: a CONNECT
// says nothing about bridge health yet, because the destination may be in
// TLSBridge.Skip or classified as passthrough on purpose. Warning here counted
// intentional opaque tunnels as evidence of a broken CA — and because the
// warning is once-only, those false positives then masked the real failure when
// it happened later. The warning lives on the bridge-eligible path instead.
func (s *Server) noteTunnel() { s.tunnelsOpened.Add(1) }

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
		slog.Warn("tls-bridge: enabled but nothing has been decrypted — every request is tunnelling through opaquely, so body-reading plugins (parsers, tool-prune) cannot act",
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

// passthroughReason maps Decision.Classify's own reason string onto the stable
// wire vocabulary. Classify already distinguishes these cases; translating here
// rather than inventing a parallel set keeps one source of truth for WHY the
// bridge declined.
func passthroughReason(why string) pipeline.TunnelReason {
	switch why {
	case tlsbridge.ReasonPort:
		return pipeline.TunnelPassthroughPort
	case tlsbridge.ReasonNonTLS:
		return pipeline.TunnelPassthroughNonTLS
	case tlsbridge.ReasonSkip:
		return pipeline.TunnelPassthroughHost
	case "":
		// Classify pairs "" with Terminate — it is NOT declining. The caller bridges
		// and bridgeServe records that outcome, so there is no passthrough reason to
		// give and "" is right here.
		return ""
	}
	// Any OTHER value is a reason tlsbridge grew that this does not map. Never "":
	// that is how a BRIDGED row is marked, so an unmapped passthrough would render as
	// an em dash and read as "we decrypted this". Go cannot force an exhaustive
	// switch, so the fallback is a value that shows up as itself and sends the reader
	// here; TestPassthroughReasonCoversEveryClassifyReason fails when it happens.
	return pipeline.TunnelPassthroughUnknown
}

// markBridged records a successful bridge: the open row is deferred to the first
// decrypted request that records a row, and carries no reason, which is the signal
// agentop uses to fold the CONNECT row into that request. Named because
// `tl.deferOpen()` at the call site does not say why.
func markBridged(tl *tunnelLog) { tl.deferOpen() }

// clientAddr names the client end of a connection for diagnostics, tolerating a
// nil conn or nil RemoteAddr so a logging path can never panic.
func clientAddr(c net.Conn) string {
	if c == nil {
		return "unknown"
	}
	a := c.RemoteAddr()
	if a == nil {
		return "unknown"
	}
	return a.String()
}

// clientPort is the source port alone, for pasting into `lsof -nP -iTCP:<port>`.
// Returns "<port>" as a literal placeholder when it cannot be determined, so the
// suggested command still reads as a template rather than as something to run.
func clientPort(c net.Conn) string {
	if _, port, err := net.SplitHostPort(clientAddr(c)); err == nil && port != "" {
		return port
	}
	return "<port>"
}

// caNotBefore is the bridge CA's NotBefore, which is the line dividing clients
// that can trust it from clients that cannot: CA files are read once at process
// start, so anything older than this is holding a different CA (or none).
// Parsed once — the value is fixed for the process.
func (s *Server) caNotBefore() string {
	s.caNotBeforeOnce.Do(func() {
		s.caNotBeforeStr = "unknown"
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
		s.caNotBeforeStr = crt.NotBefore.Local().Format(time.RFC3339)
	})
	return s.caNotBeforeStr
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

// tunnelLog records one tunnel's rows in the session timeline: the open, with the
// reason the bytes stayed opaque, and the close, with what the tunnel ended as.
//
// Both rows are recorded at most once. The guard defends against a future exit rather
// than any current one — no path calls either twice today, which is exactly why only a
// direct test can hold it. It lives in the methods rather than in each caller, so a
// branch added later cannot be uncovered by forgetting to re-check a flag.
//
// skipped means the destination matched SkipHosts, where no plugin ran and there is
// nothing to attribute an event to: neither row is recorded.
type tunnelLog struct {
	s       *Server
	pctx    *pipeline.Context
	skipped bool

	mu     sync.Mutex
	opened bool
	closed bool
	// bucket and reason are what the open row was recorded with. The close reuses both, so
	// it lands in the open's session and explains itself with the open's reason.
	bucket *session.Bucket
	reason pipeline.TunnelReason

	// deferred marks a bridged tunnel whose open row is not recorded yet. It waits for the
	// first decrypted request that records a row, so it can land in that row's session
	// directly before it; see recordWith. settleLocked records it on today's rule when no
	// request ever does.
	deferred bool
	// openEv is the deferred open row, built by deferOpen on the CONNECT's goroutine
	// before ServeConn. Whoever records it later runs in a handler, and on h2 a handler
	// can outlive ServeConn while handleConnect runs the CONNECT's finishers, which may
	// write pctx.Extensions; so no handler may build this row from pctx itself.
	openEv pipeline.SessionEvent
	// answered is set once a decrypted request has recorded a row. That row answers the
	// tunnel, and agentop folds the open into it, so no close is recorded.
	answered bool
	// inflight counts admitted handlers still running; done is set once ServeConn has
	// returned. The tunnel is settled only when both say nothing more can record.
	inflight int
	done     bool
}

func (s *Server) newTunnelLog(pctx *pipeline.Context, skipped bool) *tunnelLog {
	return &tunnelLog{s: s, pctx: pctx, skipped: skipped}
}

// open records the tunnel-open row. The first call wins; later ones are no-ops. It also
// cancels a deferred open, so a call after deferOpen cannot lead to a second open row.
func (t *tunnelLog) open(reason pipeline.TunnelReason) {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.opened {
		return
	}
	t.opened, t.reason, t.deferred = true, reason, false
	t.bucket = t.s.recordTunnelOpened(t.pctx, reason)
}

// close records the tunnel's close row: the first call wins, and a close with no open
// records nothing, since it would be a response pairing with no request. See
// recordTunnelClosed for what each argument means.
func (t *tunnelLog) close(status int, fail *pipeline.EventError, up, down int64) {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeLocked(status, fail, up, down)
}

func (t *tunnelLog) closeLocked(status int, fail *pipeline.EventError, up, down int64) {
	if !t.opened || t.closed {
		return
	}
	t.closed = true
	t.s.recordTunnelClosed(t.pctx, t.bucket, t.reason, status, fail, up, down)
}

// admit counts a decrypted request the bridged tunnel is about to serve, and refuses it
// once the close has been recorded. On h2 that can happen: ServeConn returns without
// waiting for a request's handler to start, and by then the connection is closed and the
// request's context cancelled. Serving it would record a request beside a close that says
// the tunnel carried none. Every admit is matched by a release when the handler returns.
func (t *tunnelLog) admit() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.inflight++
	return true
}

// release ends an admitted handler, settling the tunnel if it was the last one running
// after ServeConn returned.
func (t *tunnelLog) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	t.settleLocked()
}

// finish is called once ServeConn has returned. It settles the tunnel unless an admitted
// handler is still running; on h2 ServeConn does not wait for them, and the last release
// settles it instead.
func (t *tunnelLog) finish() {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	t.settleLocked()
}

// settleLocked records what a bridged tunnel still owes once nothing more can record
// under it: its open, if no request took it, and a close. A tunnel some request answered
// owes nothing.
//
// A settled tunnel's open is stamped and filed now — time.Now(), under today's session
// rule as it answers at this moment — since there is no request row to take either from.
func (t *tunnelLog) settleLocked() {
	if !t.done || t.inflight > 0 || t.answered {
		return
	}
	if t.deferred {
		t.deferred, t.opened = false, true
		open := t.openEv
		open.At = time.Now()
		t.bucket = t.s.appendTunnelOpen(t.pctx, open)
	}
	t.closeLocked(http.StatusOK, nil, 0, 0)
}

// deferOpen marks t as a bridged tunnel whose open row waits for recordWith. A no-op once
// the open is recorded: the transparent listener records its own before bridging.
//
// It builds that row now, on the CONNECT's goroutine and before ServeConn starts any
// handler; see openEv. Building it also fills pctx's RequestID memo, so a close row a
// handler later records through settleLocked reads pctx without writing it, and reads
// only fields set before bridging: identity, host, method, path, start time, client.
func (t *tunnelLog) deferOpen() {
	if t.skipped {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.opened {
		t.deferred = true
		t.openEv = t.s.tunnelOpenEvent(t.pctx, "")
	}
}

// recordWith records ev under sid together with the tunnel's deferred open, and reports
// whether it did. The open goes in directly before ev, in one store call, because agentop
// folds a tunnel row only into the event that follows it. It takes ev's timestamp: a row
// stamped later than the one after it reads as out of order to agentop's pager. When
// the open is not deferred it records nothing and reports false, leaving ev to the
// caller.
//
// t.mu is held across the append so that a second request multiplexed onto the same
// tunnel cannot record between this check and the pair.
func (t *tunnelLog) recordWith(sid string, ev pipeline.SessionEvent) bool {
	if t.skipped || t.s.Sessions == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.answered = true
	if !t.deferred {
		return false
	}
	t.deferred, t.opened = false, true
	open := t.openEv
	open.At = ev.At
	t.bucket = t.s.Sessions.AppendPair(sid, open, ev)
	return true
}

// dialError is a close row's error for a destination the proxy could not reach. The
// dial error's text names the address and the failure — refused, timed out, no such
// host — and nothing from the request.
func dialError(err error) *pipeline.EventError {
	return &pipeline.EventError{Kind: "dial_failed", Message: err.Error()}
}

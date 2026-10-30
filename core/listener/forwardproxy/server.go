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
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"errors"
	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/listener/egressallow"
	"github.com/rossoctl/cortex/core/listener/egressguard"
	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/bodycount"
	"github.com/rossoctl/cortex/core/listener/internal/bodyread"
	"github.com/rossoctl/cortex/core/listener/internal/sessionevent"
	"github.com/rossoctl/cortex/core/listener/internal/sseframe"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/spiffe"
	"github.com/rossoctl/cortex/core/tlsbridge"
	authtls "github.com/rossoctl/cortex/core/tlsconfig"
)

// maxRequestBodySize caps a buffered request body. It is separate from
// maxBodySize because requests outgrow responses: an agent resends the whole
// conversation plus its full tool manifest on every turn, so a long session's
// requests keep growing while its responses do not.
//
// 32 MiB follows Anthropic's documented 32 MB limit for the Messages API
// (https://platform.claude.com/docs/en/api/errors#request-size-limits). At
// the previous 10MB the proxy refused requests that limit allows: one laptop
// logged 30 rejected POST /v1/messages in a week, from 10,489,160 to
// 12,284,233 bytes (#1250). The docs do not say whether "32 MB" means 10^6 or
// 2^20 bytes; the larger reading is used so that a body near the line is
// judged by the provider, which knows its own limit, rather than by us.
//
// A body over the cap is rejected with a 413 before the pipeline runs, so the
// limit also decides whether a request is parsed at all: an over-limit one
// records only its request row and the 413 (see recordUnbufferedRequest),
// with no model, tokens or cost. The whole body is
// held while the pipeline runs, so one in-flight request can hold this much
// on the request side, on top of the response-side cost described at
// maxBodySize.
const maxRequestBodySize = 32 << 20 // 32 MiB

// maxBodySize is TWO ceilings, not one: the buffered response body cap, and
// the per-frame cap passed to sseframe.NewReader on both streaming paths (see
// handleStreamingResponse and streamFallbackBuffered). Raising it moves both,
// so worst-case resident bytes on the response side of one in-flight request
// are roughly 2x this value — a body buffer plus a frame scratch buffer — and
// the practical ceiling for a listener is that times the number of concurrent
// requests.
//
// It capped request bodies too until they got maxRequestBodySize, and that is
// where 10MB came from: an earlier value of 1 << 20 (1MB) matched Envoy's
// default per_stream_buffer_limit_bytes, but proved too small for LLM traffic,
// where a Claude Code session against the Anthropic endpoint produced a
// 5,250,133-byte request.
//
// The frame cap is the more generous half: a single SSE event is far smaller
// than a whole response, so 10MB per frame is well above anything observed.
// Splitting the two would let the frame cap stay tight, and is worth doing if
// per-request memory ever matters more than the simplicity of one number.
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

	// EgressAllow, when non-nil, is the deny-by-default destination allow
	// list (#1483, MOCA MI1 §8.5): an outbound destination not on the list
	// is refused with 403 destination_denied naming the host, before the
	// pipeline and before DNS resolution or any dial — the moca egress
	// conformance contract's refusal shape (header X-Moca-Egress-Error,
	// text/plain body "destination_denied: <host>\n"). Applied to CONNECT
	// targets, absolute-form http:// hosts, and the target a plugin's
	// redirect moved a request to. Checked BEFORE SkipHosts: when the list
	// is on it is the outer boundary, and a skip_hosts entry for an off-list
	// host must not turn a bypass into an egress bypass. nil keeps the
	// listener's behavior unchanged; see config.ListenerConfig.EgressAllowHosts.
	EgressAllow *egressallow.Matcher

	TLSBridge *tlsbridge.Engine // nil = disabled; set by caller after NewServer

	// SessionIDHeaders are the request headers consulted, in order, for a
	// client-supplied session id to bucket events under. The first one
	// present and usable wins; when none is, bucketing falls back to
	// ActiveSession() and then the default bucket, exactly as before.
	// Empty disables header-based bucketing. See config.SessionConfig's
	// SessionIDHeaders and SessionIDHeadersFor for the defaults and how to turn it off.
	SessionIDHeaders []string

	// ClientAffinity files a request that carries no session header under the newest
	// session of the SAME coding agent, instead of under ActiveSession() — the one
	// global "most recently updated" id, which with two agents running files each
	// one's header-less calls into the other's session. See config.SessionConfig
	// ClientAffinity; false keeps today's resolution byte for byte.
	ClientAffinity bool

	// DeclineResponsesWebSockets answers a WebSocket upgrade on a Responses API path
	// with 426 instead of forwarding it with Upgrade stripped; see
	// declineResponsesWebSocket. Set from config.ListenerConfig's
	// DeclinesResponsesWebSockets, which defaults on; false keeps the forwarding byte
	// for byte.
	DeclineResponsesWebSockets bool

	// EgressGuard, when non-nil, checks every destination this proxy dials —
	// CONNECT tunnels, plain and bridged requests, transparent redirects, and
	// skip_hosts traffic included, because a bypass must not become an SSRF
	// bypass — against the refusal set in core/listener/egressguard: resolved
	// once, refused with 403 destination_private when the address is loopback,
	// link-local, RFC 1918 / unique-local, or one of this host's own addresses
	// and no permit covers it, and dialed at the checked address so a second
	// resolution can never re-answer (the DNS-rebinding window). nil keeps the
	// listener's dial behavior unchanged; see WithEgressGuard and
	// config.ListenerConfig.EgressGuard.
	EgressGuard *egressguard.Guard

	// Processes, when non-nil, names the process behind each client connection, and a
	// request with no session header is filed under that process's session — or its
	// nearest ancestor's — before client affinity is consulted; see core/attribution. Set only where every client is a process on this
	// host (session.process_attribution) and only from a resolver whose self-test passed
	// (peerproc.New). The http.Server must carry ConnContext. nil keeps today's
	// resolution byte for byte.
	Processes peerproc.Resolver

	// bufferedFallbackOnce keeps the SSE-buffered-path notice to one line per
	// process; the condition is a supported chain shape, not an error.
	bufferedFallbackOnce sync.Once

	// selfTrafficSeen holds the (executable, service) pairs selfTraffic has announced.
	selfTrafficSeen sync.Map

	// chainOverrideLogged holds the session header names hydrateSession has
	// reported naming a session other than the process chain's.
	chainOverrideLogged sync.Map

	// attr is the Server's attribution, built once by attributor.
	attributorOnce sync.Once
	attr           *attribution.Attributor

	// Bridge-health counters. When the TLS bridge is enabled but the client
	// does not trust its CA, every HTTPS request opens a CONNECT tunnel and
	// nothing is ever decrypted: the pipeline sees opaque tunnels, every
	// body-reading plugin no-ops, and the proxy looks configured but inert.
	// Nothing errors, so the only symptom is silence. These count the two
	// outcomes so the listener can say so out loud.
	tunnelsOpened   atomic.Uint64
	bridgeAttempts  atomic.Uint64
	bridgedRequests atomic.Uint64

	// caNotBeforeAt is parsed on first use and never changes for the process.
	caNotBeforeOnce sync.Once
	caNotBeforeAt   time.Time
	// caFingerprint is likewise derived once; the CA is fixed for the process.
	caFingerprintOnce sync.Once
	caFingerprintStr  string
	bridgeWarnOnce    sync.Once
	bridgeWarned      atomic.Bool

	// dialMu guards dialFails, the memo of failed CONNECT pre-dials (dialUpstream).
	// Lazy: the Server can be built as a literal in tests.
	dialMu    sync.Mutex
	dialFails map[string]dialFailMemo
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

// Option configures optional forward-proxy behavior in NewServer. Variadic so
// the core constructor signature — and every existing caller — is unchanged.
type Option func(*options)

type options struct {
	egress *egressguard.Guard
}

// WithEgressGuard arms the SSRF dial guard (#1484): every destination the
// proxy dials is resolved once, refused with 403 destination_private when it
// lands in loopback / link-local / private / this host's own address space
// uncovered by a permit, and dialed at the checked address so DNS rebinding
// cannot re-answer between the check and the connect. Applies to CONNECT
// tunnels (Server.EgressGuard, checked in handleConnect and the transparent
// listener) and to every request the transport dials, mTLS included — under
// mTLS the check runs before the TCP connect, so the TLS handshake never
// reaches a refused address. A nil guard is accepted and keeps the dial
// behavior unchanged.
func WithEgressGuard(g *egressguard.Guard) Option {
	return func(o *options) { o.egress = g }
}

// NewServer creates a forward proxy server with a default HTTP client.
// When mtls is non-nil, every outbound dial does TLS-or-fail using the
// local SVID; see MTLSOptions for semantics.
func NewServer(outbound *pipeline.Holder, sessions *session.Store, mtls *MTLSOptions, opts ...Option) (*Server, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	transport := &http.Transport{
		// Sane Go defaults for everything except DialContext, which we
		// customize when mTLS is on.
		MaxIdleConns: 100,
		// Go's default is 2, which churns connections when more than two
		// callers share an h1-only upstream (LiteLLM, Keycloak).
		MaxIdleConnsPerHost:   100,
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
		// The guard sits INSIDE the mTLS dialer: check first, then connect,
		// then handshake — a refused address never gets a TLS ClientHello.
		transport.DialContext = mtlsDialer(tlsCfg, mtls.Metrics).withGuard(o.egress).DialContext
	} else if o.egress != nil {
		transport.DialContext = o.egress.DialContext
	}

	return &Server{
		OutboundPipeline: outbound,
		Sessions:         sessions,
		EgressGuard:      o.egress,
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
	// egress, when non-nil, checks the destination before the plain dial —
	// the TLS handshake is the step after the connect, so it never reaches an
	// address the guard refused. nil keeps the unguarded dial.
	egress *egressguard.Guard
}

func mtlsDialer(cfg *cryptotls.Config, metrics *authtls.Metrics) *mtlsDialFunc {
	return &mtlsDialFunc{
		plain:   &net.Dialer{Timeout: 10 * time.Second},
		tlsCfg:  cfg,
		metrics: metrics,
	}
}

// withGuard returns d with the egress guard set. A nil guard is fine: the
// dialer then behaves exactly as without it, which is why NewServer can pass
// o.egress unconditionally under mTLS.
func (d *mtlsDialFunc) withGuard(g *egressguard.Guard) *mtlsDialFunc {
	d.egress = g
	return d
}

func (d *mtlsDialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	// SNI below reads the ORIGINAL addr, so the name the request used stays
	// the handshake's ServerName; only the TCP connect follows the checked
	// address. A literal destination is its own checked address.
	dialAddr := addr
	if d.egress != nil {
		checked, err := d.egress.CheckAddr(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		dialAddr = checked
	}
	plain, err := d.plain.DialContext(ctx, network, dialAddr)
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
	// Before anything reads the headers: a client must not assert its own attribution, and
	// no hop in front of this listener is trusted to (attribution.Strip).
	attribution.Strip(r.Header)
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

	// A bridged request is dialed to the tunnel's authority, so policy must judge that
	// host. The client chose both the CONNECT target and the Host header, and a Host
	// naming anywhere else would have every host-keyed plugin, skip_hosts included,
	// judge a destination the bytes never reach. Refuse it before anything runs.
	if isBridge {
		if r.Host == "" {
			r.Host = r.URL.Host
			pctx.Host = r.Host
		}
		if !tunnelAuthorityMatches(r.Host, r.URL.Host) {
			s.refuseMisdirected(w, r, pctx, tl)
			return
		}
	}

	// Egress allow list (#1483), the request-path half: a plain (non-bridged)
	// absolute-form request is judged on its dial target, r.URL.Host, the same
	// boundary the CONNECT gate holds — before SkipHosts (the allowlist is the
	// outer boundary; a skip entry must not turn a bypass into an egress
	// bypass), before the pipeline, and before the transport resolves or dials
	// anything. A bridged request is not re-judged here: its destination was
	// gated at its CONNECT, and the misdirected check above pins its Host to
	// that same authority. Plain WriteRejection for the same reason as there:
	// the named 403 is the contract, not something to re-frame for MCP.
	if !isBridge && s.EgressAllow != nil && r.URL.Host != "" && !s.EgressAllow.Allows(r.URL.Host, r.URL.Scheme) {
		host := hostOnly(r.URL.Host)
		action := egressAllowRefusal(pctx, host)
		chain := s.clientChain(r)
		s.recordOutboundRejectIn(tl, pctx, action, s.recordingSessionID("", r.Header, chain))
		// Sanitized: host (the URL authority), Method and Path are all client-controlled,
		// and a control rune forged into any of them would forge a log entry — the same
		// CWE-150 shape #1539's egress guard was flagged for.
		slog.Info("forward-proxy: destination refused by egress allow list",
			"host", pipeline.SanitizeControlRunes(host),
			"method", pipeline.SanitizeControlRunes(r.Method),
			"path", pipeline.SanitizeControlRunes(r.URL.Path))
		httpx.WriteRejection(w, action)
		return
	}

	// Every request that reaches here is re-originated from pctx below, plain or
	// TLS-bridged, so a plugin may redirect it. The CONNECT and transparent-connection
	// contexts are never marked: both dial the address the client chose, so a redirect
	// there would be recorded and never happen.
	pctx.MarkRedirectable()

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

	// The client's process, looked up once per connection; nil when process attribution
	// is off or the lookup failed, and then resolution is exactly as without it.
	var chain []attribution.Proc
	if !skipped {
		chain = s.clientChain(r)
	}

	// An agent talking to its own service on this host is forwarded the way a skip_hosts
	// destination is: no pipeline, no row. See selfTraffic.
	if !skipped && !isBridge && s.selfTraffic(r, chain) {
		skipped = true
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
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			bodyread.LogError("forward-proxy", r, len(body), maxRequestBodySize, err)
			status, msg := bodyread.Rejection(err)
			http.Error(w, msg, status)
			s.recordUnbufferedRequest(tl, pctx, r, chain, err)
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
		sessionID = s.hydrateSession(pctx, r.Header, chain)
	}

	if !skipped {
		action := s.OutboundPipeline.Run(r.Context(), pctx)

		// Send the request where the pipeline pointed it, before any row is recorded,
		// so the row names the host the bytes went to. The target comes from
		// RedirectTarget, the copy Redirect validated, never from pctx.Scheme and
		// pctx.Host: those are exported, and a later plugin that never declared
		// WritesDestination could otherwise steer a redirected request. They are
		// re-asserted to the same values so the request row, the denied row and the
		// redirect record all agree with the dial. r.URL.Host carries the dial target,
		// SNI and certificate check, and r.Host the Host header: they move together,
		// or the upstream sees one host while the transport dials another. The client
		// choice below does not change — a bridged request still goes through the
		// bridge's upstream client, which now verifies the new host.
		if scheme, host, ok := pctx.RedirectTarget(); ok {
			pctx.Scheme, pctx.Host = scheme, host
			r.URL.Scheme, r.URL.Host, r.Host = scheme, host, host
			// The path, when the plugin gave one, from the copy SetRedirectPath
			// validated, for the reason RedirectTarget is read rather than Host. The
			// query string is the client's and stays.
			if path, ok := pctx.RedirectPath(); ok {
				pctx.Path = path
				r.URL.Path, r.URL.RawPath = path, ""
			}
		}

		if action.Type == pipeline.Reject {
			s.recordOutboundRejectIn(tl, pctx, action, s.recordingSessionID(sessionID, r.Header, chain))
			// Render as a JSON-RPC error frame when the rejected
			// request was MCP JSON-RPC, so the agent's MCP client
			// surfaces this as one failed tool call rather than a
			// transport break. Falls through to plain HTTP-level
			// rejection for non-MCP traffic.
			httpx.WriteRejectionForRequest(w, action, pctx)
			return
		}
	}

	// A WebSocket upgrade is the one hop-by-hop request this proxy forwards
	// (#1488): the CONNECT-host check above has already held a bridged request to
	// its tunnel's destination, the pipeline has run, and what is left is to hand
	// the header across and splice the 101. Every other protocol named in Upgrade
	// keeps the strip, and so does the one WebSocket this proxy declines — the
	// Responses-API case below, which answers 426 itself.
	forwardUpgrade := upgradesToWebSocket(r.Header) &&
		!(s.DeclineResponsesWebSockets && wantsResponsesWebSocket(r))

	// Before the request row is recorded, 20-odd lines below, and NOT down beside
	// stripForUpstream where the strip itself happens — the row is appended first,
	// so a field set after it is set on nothing. (It was, and three unit tests
	// driving this method directly all passed while the feature recorded nothing
	// end to end.) Reading it here is equivalent: an Upgrade present on the
	// inbound request is one the strip below will remove, because the one case it
	// is kept is exactly the case excluded by forwardUpgrade above.
	if !forwardUpgrade {
		pctx.RecordStrippedUpgrade(r.Header)
	}

	// After the pipeline, so a plugin's reject still wins; before any row, since this
	// records its own pair. See declineResponsesWebSocket.
	if !skipped && s.DeclineResponsesWebSockets && wantsResponsesWebSocket(r) {
		s.declineResponsesWebSocket(w, r, pctx, tl, s.recordingSessionID(sessionID, r.Header, chain), chain)
		return
	}

	if !skipped && s.Sessions != nil {
		sid := s.recordingSessionID(sessionID, r.Header, chain)
		// Pin this session so the paired response event records into the
		// same bucket. Without it, recordOutboundResponseEvent re-resolves
		// ActiveSession() at response time, which interleaving traffic (a
		// health probe under "default") can flip mid-stream — mis-filing a
		// streaming inference response away from its request's session.
		pctx.OutboundSessionID = sid
		if chain != nil {
			defer s.touch(sid, chain)
		}
		// Record EVERY message that reaches the pipeline — even when no
		// plugin acted and no parser matched (Invocations/MCP/Inference all
		// nil). The session API is an observability surface; a request the
		// pipeline saw but no plugin touched is still a network message the
		// operator wants to see (it carries Host, and the paired response
		// carries StatusCode). skip_hosts traffic never reaches here (the
		// !skipped guard above), so it stays suppressed by design.
		s.recordOutboundRequestEvent(tl, pctx, sid)
	}

	// Propagate every header mutation the outbound pipeline made to the
	// forwarded request. pctx.Headers started as a clone of r.Header, so
	// plugins' set / replace / delete operations on it are the intended
	// upstream-facing header set. Only Authorization used to be forwarded,
	// silently dropping any other injected header (e.g. static-inject's
	// x-api-key). Content-Length / Content-Encoding are managed by the
	// body-rewrite block below and the transport, so leave them untouched.
	// Mirrors reverseproxy's forwarded-request header sync.
	syncHeaders(r, pctx.Headers)

	// If a WritesRequestBody plugin rewrote pctx.Body, ship the new bytes
	// upstream and clear Content-Encoding (see forwardproxy response
	// path for the rationale).
	if pctx.BodyMutated() {
		setBody(r, pctx.Body)
	}

	// The pipeline this request runs on for the rest of the handler: every
	// capability probe below (encoding, streaming, buffering) must answer
	// for it, not whatever a mid-request reload left in the Holder — the
	// path the listener picks has to match the pipeline the response
	// phases dispatch on. Nil-pinned (skipped requests never ran Run)
	// falls back to the current pipeline (Holder.ForPctx).
	pinned := s.OutboundPipeline.ForPctx(pctx)

	ownEncoding := !skipped && (pinned.NeedsResponseBody() || pinned.HasStreamingResponders())
	stripForUpstream(r, ownEncoding, forwardUpgrade)

	// Clear RequestURI — set by the server but must be empty for client requests
	r.RequestURI = ""

	// Egress allow list, redirect re-check (#1483): a plugin's redirect —
	// inference-router moves requests between model servers — can change the
	// dial target after the gate at the top of this function passed it, and
	// deny-by-default means the DIAL TARGET is what counts, not the host the
	// client named. An off-list redirect target is refused here, before the
	// exchange: the request row has already landed above, and this denial row
	// follows it. Unconditional rather than only-when-redirected, so the
	// invariant "nothing dials off-list" holds by one check at the dial
	// rather than by reasoning about every path that can reach it.
	if s.EgressAllow != nil && !s.EgressAllow.Allows(r.URL.Host, r.URL.Scheme) {
		host := hostOnly(r.URL.Host)
		action := egressAllowRefusal(pctx, host)
		s.recordOutboundRejectIn(tl, pctx, action, s.recordingSessionID(sessionID, r.Header, chain))
		// A delivered answer, not a failed exchange — the 426 rule: the deferred
		// RunFinish reads zero StatusCode as OutcomeError, and the allowlist
		// refusing an off-list redirect is the policy working, not a failure.
		pctx.StatusCode = http.StatusForbidden
		// Sanitized like the gate at the top of this function: both hosts are
		// client-controlled (pctx.Host is the one the client named, pre-redirect).
		slog.Info("forward-proxy: destination refused by egress allow list (redirect re-check)",
			"host", pipeline.SanitizeControlRunes(host),
			"requested", pipeline.SanitizeControlRunes(pctx.Host))
		httpx.WriteRejection(w, action)
		return
	}

	client := s.Client
	if isBridge && s.TLSBridge != nil {
		// No upgrade-specific client, though this one negotiates h2: net/http
		// pins a WebSocket upgrade to HTTP/1.1 on its own (Request.requiresHTTP1
		// matches the Connection/Upgrade pair stripForUpstream just normalized
		// to), so the exchange rides this client as HTTP/1.1.
		client = s.TLSBridge.Upstream
	}
	resp, err := client.Do(r)
	// A Resender reads refused answers, and a 101 is not one — it is the upgrade
	// succeeding. Peeking at its (empty) body and sending the GET again would tear
	// down the connection the origin just switched, so a forwarded upgrade never
	// resends.
	//
	// THE RULE IS KEYED ON THE REQUEST, NOT THE ANSWER, so it also covers an
	// upgrade the origin DECLINED with a non-101 (401/403/429 — those take the
	// ordinary response path, and a Resender does not see them here). That is
	// deliberate, and narrowing this to `resp.StatusCode != 101` would break it:
	// the resend path strips the Upgrade pair (`stripForUpstream(again,
	// ownEncoding, false)`), so the second attempt would reach the origin as a
	// bare GET — not the handshake the client sent, and on an endpoint that only
	// answers an upgrade, not a request worth making. A re-auth that cannot carry
	// the upgrade cannot rescue the upgrade; the client retries the handshake
	// itself, with whatever credential it has now. Left as a request-keyed rule
	// rather than made cleverer (#1549 review).
	if err == nil && !skipped && !forwardUpgrade {
		resp, err = s.resendIfAsked(r, pctx, client, resp, tl, ownEncoding)
	}
	if err != nil {
		// The egress guard's refusal, BEFORE the generic 502: a destination that
		// resolved into refused space is a policy answer (403 destination_private),
		// not a failed exchange. errors.As traverses the *url.Error client.Do wraps
		// every dial error in; see TransportError's note on that wrapping.
		var ref *egressguard.Refusal
		if errors.As(err, &ref) {
			slog.Debug("forward-proxy: destination refused by egress guard",
				// Sanitized: Host and Method are client-controlled, and a newline
				// forged into either would forge a log entry (#1539's CodeQL finding).
				"host", pipeline.SanitizeControlRunes(r.Host),
				"method", pipeline.SanitizeControlRunes(r.Method), "addr", ref.Addr.String())
			// The listener's standard rejection shape, code verbatim from the MOCA
			// egress contract so the shared conformance suite matches.
			httpx.WriteRejection(w, pipeline.DenyStatus(http.StatusForbidden, "destination_private",
				fmt.Sprintf("refusing to reach private destination %s (%s)", ref.Host, ref.Addr)))
			// A delivered answer, not a failed exchange — the 426 rule: the Finisher
			// reads non-zero StatusCode as an allow, and the guard did what it was
			// configured to. usage keys off EventError.Kind, which stays set below.
			pctx.StatusCode = http.StatusForbidden
			if !skipped {
				s.recordOutboundResponseEvent(pctx, http.StatusForbidden, &pipeline.EventError{
					Kind:    pipeline.ErrorKindDestinationPrivate,
					Message: ref.Error(),
				})
			}
			return
		}
		// A 502 with the cause, or a 499 when the client hung up first — which,
		// with no timeout on this client, is how a hung upstream usually ends.
		// See pipeline.ExchangeFailure.
		status, fail := pipeline.ExchangeFailure(r.Context(), err)
		if r.Context().Err() != nil {
			slog.Debug("forward-proxy: client went away before the upstream answered", "host", r.Host, "method", r.Method)
		} else {
			// fail.Message, not err: err's text quotes the request URL, query and all.
			slog.Warn("forward-proxy: upstream request failed", "host", r.Host, "method", r.Method, "kind", fail.Kind, "error", fail.Message)
		}
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		// Without this the request event is the only trace, and a row with no
		// status reads as a request still in flight (#1045). Same reason the
		// CONNECT dial-failure path below records.
		//
		// The status goes on the EVENT ONLY, never onto pctx.StatusCode:
		// OutcomeFromContext reads zero as OutcomeError and any non-zero as
		// OutcomeAllow, so assigning it would tell every Finisher this
		// succeeded and have lineage label the span "ok".
		if !skipped {
			s.recordOutboundResponseEvent(pctx, status, fail)
		}
		return
	}

	// The 101: the origin took the upgrade. Hand both connections to a splice and
	// get out of the way (#1488). The caller's resp.Body.Close() below must NOT
	// run — the body IS the origin's connection now, and the splice owns closing
	// it. Anything else the origin answered (a 200 serving the bare GET, a 4xx
	// from an endpoint that does not upgrade) falls through to the ordinary
	// response path, headers and body relayed as usual.
	if forwardUpgrade && resp.StatusCode == http.StatusSwitchingProtocols {
		pctx.ResponseHeadersAt = time.Now()
		s.relayUpgradedWebSocket(w, r, pctx, resp, !skipped)
		return
	}
	defer resp.Body.Close()

	// Response phase: populate pctx and run plugins in reverse order.
	pctx.StatusCode = resp.StatusCode
	pctx.ResponseHeaders = resp.Header.Clone()
	// Taken HERE and not where client.Do returned, because a resend
	// (resendIfAsked above) discards that response and re-issues the exchange:
	// the figure the response row should carry is for the response the client
	// actually got. Both proxy listeners measure at the same point in their
	// own flow — the moment the delivered response's headers are in hand —
	// so ttfb means one thing wherever a row reports it. Stays zero on the
	// error path above, where no headers ever arrived (#1251).
	pctx.ResponseHeadersAt = time.Now()

	// BytesDown for the response row (#1309), counted HERE and nowhere else. Every
	// arm below reads this one body — buffered whole, relayed SSE block by block, relayed
	// chunk by chunk, or copied straight through at the bottom of this function —
	// so one wrapper covers all of them, and the arms that go on to replace
	// resp.Body with a bytes.Reader over what they already read cannot
	// double-count. Counted at the wire and not per arm so that every arm agrees
	// with what extproc reports for the same response without each having to
	// count: see bodycount.
	//
	// The final io.Copy at the bottom of this function — the arm a response takes
	// when nothing buffered it and it was not streamed — reads through this
	// wrapper, and the row is recorded below that copy, so the count is complete
	// when the figure is read. What the wrapper still does not reach is a SKIPPED
	// response's row: skip_hosts records nothing at all. reverseproxy's
	// unbuffered relay defers its row to the end of its copy the same way
	// (recordOnEOFBody).
	resp.Body = bodycount.Wrap(resp.Body, &pctx.ResponseBytes)

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
		// mislabeled is gated on 2xx: an error from this endpoint (we've seen 401, 405, 429
		// in practice) still carries a plain JSON body like {"detail":"..."}. Routing that
		// into the SSE path finds no "data:" lines, so sseframe.Reader emits nothing and the
		// caller gets an empty body — losing the error detail Codex itself surfaces to the
		// user. Confirmed on live traffic: before this gate, a 405 that used to arrive as
		// `{"detail":"Method Not Allowed"}` arrived as the literal string "Unknown error"
		// once isKnownMislabeledSSE started matching every status, not just success.
		mislabeled := resp.StatusCode/100 == 2 && isKnownMislabeledSSE(r.Host, r.URL.Path)
		if (isEventStream(resp.Header.Get("Content-Type")) || mislabeled) && resp.Body != nil {
			if pinned.WritesResponseBody() {
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
			} else if pinned.HasStreamingResponders() {
				// Streaming-aware plugins (inference-parser, a2a-parser) read each
				// SSE event's payload; handleStreamingResponse forwards the
				// upstream bytes unchanged.
				s.handleStreamingResponse(w, r, resp, pctx)
				return
			} else {
				// No streaming responder: relay the SSE stream byte-for-byte
				// with per-write flushing. Both arms forward the upstream bytes
				// unchanged; this one is chosen when no plugin needs per-event
				// dispatch. Fixes #642.
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
				if pinned.NeedsResponseBody() {
					slog.Warn("forward-proxy: text/event-stream response with a ReadsBody plugin that is not a StreamingResponder — streaming byte-for-byte; its OnResponse will see an empty body (implement StreamingResponder to inspect a streamed body)", "host", r.Host)
				}
				s.streamPassthrough(w, r, resp, pctx)
				return
			}
		}

		if pinned.NeedsResponseBody() && resp.Body != nil {
			respBody, ok := s.bufferResponseBody(w, r, resp, pctx)
			if !ok {
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
			s.rejectResponse(w, pctx, respAction)
			return
		}

		// Streaming-aware plugins use a single code path for both shapes:
		// for the buffered application/json case we deliver the whole body
		// as one last=true frame so plugins finalize their running state.
		// Plugins that didn't migrate — i.e. don't implement
		// StreamingResponder — are unaffected (RunResponseFrame skips them).
		if pinned.HasStreamingResponders() && resp.Body != nil {
			// Its own budget, per the rule above.
			finalCtx, cancelFinal := httpx.TeardownContext(r.Context())
			defer cancelFinal()
			respFrameAction := s.OutboundPipeline.RunResponseFrame(finalCtx, pctx, pctx.ResponseBody, true)
			if respFrameAction.Type == pipeline.Reject {
				s.rejectResponse(w, pctx, respFrameAction)
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

	}

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// A skipped response of unknown length — an event stream, a long poll — is relayed as
	// it arrives, as httputil.ReverseProxy does; buffered, its first bytes would wait for
	// the next 4KB. skip_hosts never sent one here, but self-traffic does: OpenCode's TUI
	// follows its service's event stream through this path.
	var dst io.Writer = w
	if f, ok := w.(http.Flusher); ok && skipped && (resp.ContentLength == -1 || isEventStream(resp.Header.Get("Content-Type"))) {
		f.Flush()
		dst = flushWriter{w: w, f: f}
	}
	var streamFail *pipeline.EventError
	if _, err := io.Copy(dst, resp.Body); err != nil {
		var abort bool
		streamFail, abort = classifyBrokenStream(pctx, r, resp, err)
		if abort {
			// Record before the panic: the recording below this block is
			// unreachable on the panic path, and skip_hosts keeps its
			// promise of no row — the client still gets its connection
			// reset, which is not recording (#1543).
			if !skipped {
				s.recordOutboundResponseEvent(pctx, resp.StatusCode, streamFail)
			}
			// Push what is already relayed — headers and any body bytes
			// still sitting in net/http's buffer — before the reset: the
			// SSE arms flush per chunk, but this arm writes through a plain
			// buffered writer, and ErrAbortHandler discards whatever it
			// holds (#1543).
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
	}
	// Recorded after the copy, not before it: the arm that reaches this io.Copy
	// unbuffered is still receiving its body while the copy runs, and a row filed
	// above it would stop Duration at the headers — reporting a body share of zero
	// for a body the proxy was still relaying, the exact misreading #1251's split
	// exists to prevent. After the copy the figure covers the whole exchange,
	// matching the streamed arms (which record when their relay loop ends), and
	// ResponseBytes is complete for the same reason. Skipped responses stay
	// unrecorded, as skip_hosts promises.
	if !skipped {
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, streamFail)
	}
}

// flushWriter flushes after every write, so a relayed stream reaches the client as the
// upstream sends it.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

// bridgeVerdict applies the TLS bridge's rules to a CONNECT, in the order the design
// gives them (docs/superpowers/specs/2026-10-09-tls-bridge-per-program-design.md): the
// host's own eligibility, then what is known about the client's program, and the host
// memory only for a client whose program cannot be named. It returns the reason the
// bytes stay opaque, or bridge true and no reason. With bridge true and a named
// program it sets tl.program, so bridgeServe records the outcome against the program.
func (s *Server) bridgeVerdict(r *http.Request, tl *tunnelLog, host string, first []byte) (reason pipeline.TunnelReason, bridge bool) {
	prog, named := s.bridgeProgram(r)
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
// false path the caller tunnels and records the close itself. originDead says the
// CONNECT's own pre-dial failed, in which case the origin probe is skipped: see
// step 1.
func (s *Server) bridgeServe(client net.Conn, authority, host string, tl *tunnelLog, originDead bool) bool {
	// 1) Verify upstream reachability + cert via the dedicated client, BEFORE forging.
	//    HEAD avoids GET side-effects; a non-2xx status still returns err==nil (cert
	//    verified), which is all we need. Only a transport/TLS error fails here. The
	//    verify is bounded by its own context timeout so a slow/stalled origin can't
	//    pin the bridging goroutine — the timeout is on this probe ONLY, not on the
	//    relay (which must stay unbounded for streaming responses).
	//
	//    Skipped when the pre-dial already failed (originDead): the probe would only
	//    re-confirm a dead origin at up to upstreamVerifyTimeout more, and its
	//    passthrough fallback dials the same dead origin. Bridging proceeds so a
	//    plugin redirect (inference-router moving a pinned session) can still send
	//    the request to a live server (#1595); the re-originated request is verified
	//    against whatever host it actually goes to.
	if !originDead {
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
	}

	// 2) Forge + terminate downstream.
	tconn, err := s.TLSBridge.Term.Terminate(client, hostOnly(authority))
	if err != nil {
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
		memory, key := s.bridgeMemory(tl, host)
		if reason == pipeline.TunnelClientRejectedCA {
			memory.Fail(key)
		} else {
			memory.FailTransient(key)
		}
		if memory == s.TLSBridge.Programs {
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
			"host", host,
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
	tlsbridge.ServeConn(tconn, s.bridgedHandler(authority, tl))
	// ServeConn returns once the connection has closed. A bridged tunnel's decrypted
	// requests answer it and agentop folds the open into the first of them, so a close row
	// there would render as an orphan response. With no recorded request at all there is
	// nothing to fold into, so finish records the open and a close — now, or when the last
	// handler still running returns.
	tl.finish()
	return true
}

// dialFailMemo is a memoized failed CONNECT pre-dial: the error and when it was
// taken. dialFailMemoTTL bounds how long it is believed.
type dialFailMemo struct {
	err error
	at  time.Time
}

// dialFailMemoTTL is how long a failed pre-dial is remembered, so a destination
// that drops packets — where a dial burns the whole connectDialTimeout — is not
// re-dialed per CONNECT. Bridge-enabled CONNECTs only: there a skipped dial is
// re-attempted wherever its result would have been used (the plain-tunnel path
// dials fresh in tunnelOrDial; the bridge path re-originates to a different host
// and dials that), so the staleness it can buy is one skipped dial, never a
// misrouted request. Without the bridge the 502 is the last word, so a memo hit
// would keep answering 502 for an origin that came back within the TTL. A stale
// hit also skips the origin probe (bridgeServe), not just the dial: an origin
// that comes back within the TTL with a certificate the bridge cannot verify is
// bridged rather than passed through, and the request fails at re-origination —
// bounded by the TTL, and the failure is loud rather than silent.
const dialFailMemoTTL = 30 * time.Second

// dialTCP is net.DialTimeout; a variable so tests can count dials.
var dialTCP = net.DialTimeout

// bridgeable reports whether the TLS bridge could take a connection to
// authority's port. The dead-origin fall-through and its memo are for those
// connections only: anywhere else the bridge would decline by port anyway, so
// a failed pre-dial keeps its immediate 502 instead of a 200 the bridge cannot
// use.
func (s *Server) bridgeable(authority string) bool {
	return s.TLSBridge != nil && s.TLSBridge.Decision.HandlesPort(portOf(authority))
}

// dialUpstream is a CONNECT's pre-dial, memoizing failures per target for
// dialFailMemoTTL (#1595: a client that opens a CONNECT per request must not pay
// a dead destination's dial timeout every time). bridgeable gates the memo: where
// the bridge cannot take the connection the failure is answered with a 502, not
// skipped, so remembering it would only pin that 502 past an origin that came
// back. The final return says whether the failure came from the memo rather than
// a dial this call made — a caller that would re-dial (tunnelOrDial) should not,
// because a failure from its own dial is already the answer.
func (s *Server) dialUpstream(dialTarget string, bridgeable bool) (net.Conn, error, bool) {
	if bridgeable {
		s.dialMu.Lock()
		if s.dialFails == nil {
			s.dialFails = make(map[string]dialFailMemo)
		}
		if m, ok := s.dialFails[dialTarget]; ok {
			if time.Since(m.at) < dialFailMemoTTL {
				s.dialMu.Unlock()
				return nil, m.err, true
			}
			delete(s.dialFails, dialTarget)
		}
		s.dialMu.Unlock()
	}

	conn, err := dialTCP("tcp", dialTarget, connectDialTimeout)
	if err == nil {
		return conn, nil, false
	}
	if bridgeable {
		s.dialMu.Lock()
		// Sweep while inserting: an entry is otherwise deleted only when its own
		// target is CONNECTed again after the TTL, so distinct dead targets — a
		// scanner's stock in trade — would grow the map without bound.
		now := time.Now()
		for k, m := range s.dialFails {
			if now.Sub(m.at) >= dialFailMemoTTL {
				delete(s.dialFails, k)
			}
		}
		s.dialFails[dialTarget] = dialFailMemo{err: err, at: now}
		s.dialMu.Unlock()
	}
	return nil, err, false
}

// tunnelOrDial serves a tunnel whose pre-dialed upstream is gone — the bridge
// declined the connection, or fell open after the origin probe failed — by dialing
// fresh: a working dial tunnels normally, a failing one closes the client with the
// dial error on the close row. The client already has its 200, so that stays the
// status and the error says why the tunnel died. fail, when non-nil, is a failure
// the pre-dial produced on a real dial: re-dialing would only repeat it, so the
// tunnel closes with it instead.
func (s *Server) tunnelOrDial(clientConn net.Conn, dialTarget string, tl *tunnelLog, fail error) {
	if fail != nil {
		_ = clientConn.Close()
		tl.close(http.StatusOK, dialError(fail), 0, 0)
		return
	}
	up2, derr := dialTCP("tcp", dialTarget, connectDialTimeout)
	if derr != nil {
		_ = clientConn.Close()
		tl.close(http.StatusOK, dialError(derr), 0, 0)
		return
	}
	sent, received := tunnel(clientConn, up2)
	_ = up2.Close()
	tl.close(http.StatusOK, nil, sent, received)
}

// misdirectedBody is the 421's body. Static: the Host it refuses is the client's own
// input, and echoing it back would make the proxy a reflector.
const misdirectedBody = "misdirected request: the Host header does not name this tunnel's destination\n"

// refuseMisdirected answers a bridged request whose Host names another authority than
// the tunnel's with 421 Misdirected Request (RFC 9110 §15.5.20), without running the
// pipeline or contacting any upstream. The rows it records name the tunnel's authority
// as the host, since that is where the request would have gone; the Host the client
// sent goes in the error message only, and carries no path or query.
func (s *Server) refuseMisdirected(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog) {
	claimed := r.Host
	if len(claimed) > 255 {
		claimed = claimed[:255]
	}
	slog.Warn("forward-proxy: refused a bridged request whose Host is not the tunnel's",
		"tunnel", r.URL.Host, "host", claimed, "method", r.Method)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusMisdirectedRequest)
	_, _ = io.WriteString(w, misdirectedBody)

	if s.Sessions == nil {
		return
	}
	pctx.Host = r.URL.Host
	chain := s.clientChain(r)
	sid := s.attributeForRecording(pctx, r.Header, chain)
	pctx.OutboundSessionID = sid
	if chain != nil {
		defer s.touch(sid, chain)
	}
	s.recordOutboundRequestEvent(tl, pctx, sid)
	s.recordOutboundResponseEvent(pctx, http.StatusMisdirectedRequest, &pipeline.EventError{
		Kind:    "misdirected_request",
		Message: fmt.Sprintf("Host %q does not name the tunnel's destination %s", claimed, r.URL.Host),
	})
}

// declinedUpgradeBody is the 426's body. Static, like misdirectedBody: nothing the
// client sent is echoed back.
const declinedUpgradeBody = `{"detail":"WebSocket transport is not available through Cortex; use HTTPS"}` + "\n"

// wantsResponsesWebSocket reports whether r asks to open a WebSocket on an OpenAI
// Responses API path — the one shape declineResponsesWebSocket answers.
//
// Scoped twice: to OpenAI's own hosts (isOpenAIResponsesHost), and to the Responses
// suffix inference-parser reads (/v1/responses, and Codex's
// /backend-api/codex/responses). A 426 is an answer only Codex is known to read as
// "use HTTPS", and only against OpenAI is that known: every other WebSocket is
// forwarded (#1488), whatever its origin makes of the upgrade.
func wantsResponsesWebSocket(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		upgradesToWebSocket(r.Header) &&
		isOpenAIResponsesHost(r.URL.Host) &&
		strings.HasSuffix(r.URL.Path, "/responses")
}

// isOpenAIResponsesHost reports whether authority is one of OpenAI's own Responses API
// hosts: chatgpt.com, which serves Codex on a ChatGPT login, and api.openai.com, which
// serves it on an API key and is the public /v1/responses.
//
// OpenAI's only, because what a 426 means to a client is only known for Codex — it is
// read from Codex's own source. Another harness speaking the same API through another
// host (a LiteLLM gateway, OpenCode Zen, a local server) may treat a 426 as a hard
// failure where today it falls back, so those keep the strip and the origin's answer.
// Exact names, compared case-insensitively without a port or a trailing dot: a
// subdomain is not the same service.
//
// authority is r.URL.Host, the address the request is dialed to — on a bridged request
// the tunnel's own, which tunnelAuthorityMatches has already held the Host header to,
// and after a redirect the redirect's — so the judgement follows where the bytes go.
func isOpenAIResponsesHost(authority string) bool {
	switch strings.TrimSuffix(strings.ToLower(hostOnly(authority)), ".") {
	case "chatgpt.com", "api.openai.com":
		return true
	}
	return false
}

// upgradesToWebSocket reports whether h is a WebSocket upgrade REQUEST. RFC 9110
// §7.8 makes Upgrade a comma-separated list, matched case-insensitively and in any
// repeat of the header, so "websocket, h2c" asks for a WebSocket as surely as
// "websocket" does. A token may carry a version ("websocket/13"); only the name
// before it is compared.
//
// CONNECTION IS CHECKED FIRST, and it is not a formality. RFC 9110 §7.8 is
// explicit that a sender MUST send Connection: upgrade with the Upgrade header,
// and that a recipient ignores Upgrade when Connection does not list it — which is
// exactly what net/http's own httpguts-based upgradeType does before
// ReverseProxy honours an upgrade at all.
//
// Keying on Upgrade alone made this proxy SYNTHESIZE an upgrade the client had not
// requested: stripForUpstream sets the normalized Connection/Upgrade pair whenever
// keepUpgrade is true, so a request carrying a bare "Upgrade: websocket" and no
// Connection token reached the origin as a well-formed handshake, and an origin
// that answered 101 got its connection hijacked and spliced — turning a request
// the client expected an ordinary HTTP response to into an opaque byte pipe whose
// frames no plugin inspects. Measured end to end before this check existed
// (TestForwardProxy_UpgradeWithoutConnectionTokenIsStripped) (#1549 review).
func upgradesToWebSocket(h http.Header) bool {
	if !headerListContainsToken(h, "Connection", "upgrade") {
		return false
	}
	for _, line := range h.Values("Upgrade") {
		for _, tok := range strings.Split(line, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(tok), "/")
			if strings.EqualFold(name, "websocket") {
				return true
			}
		}
	}
	return false
}

// headerListContainsToken reports whether the named header — in any repeat, as a
// comma-separated list — carries token, compared case-insensitively. The shape
// RFC 9110 gives Connection, and the check net/http makes with
// httpguts.HeaderValuesContainsToken, which is not importable from here.
func headerListContainsToken(h http.Header, name, token string) bool {
	for _, line := range h.Values(name) {
		for _, tok := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), token) {
				return true
			}
		}
	}
	return false
}

// declineResponsesWebSocket answers a WebSocket upgrade on a Responses API path with
// 426 Upgrade Required, without contacting the origin.
//
// THE EXPENSIVE ANSWER IS THE ONE A STRIPPED HANDSHAKE DREW. Before upgrades were
// forwarded (#1488) the strip turned this handshake into a bare GET on an endpoint
// that takes a POST or an upgrade, and the origin answered 405. Codex reads a 405 as
// transient: it retries five times on a 200ms-doubling backoff, about six seconds,
// printing `Reconnecting… n/5`, before falling back to HTTPS. A 426 is the one status
// it reads as "this endpoint wants
// HTTP": it switches at once, for the rest of the session, with no retry and no warning
// (codex-rs/core/src/client.rs, the UPGRADE_REQUIRED arms). Measured on Codex 0.163.0,
// eight runs each of a one-word `codex exec` turn: 10.2–15.1s (median 11.0s) through the
// 405, 2.4–6.4s (median 3.0s) through this, with one handshake attempt instead of seven.
//
// TODAY THE DECLINE IS A STEER RATHER THAN A LIMIT: a forwarded Responses WebSocket
// works (relayUpgradedWebSocket), but its frames are opaque to every plugin, so a
// Codex session moved onto one stops being parseable — no token counts, no cost. The
// 426 keeps it on HTTPS, where its traffic stays observable.
//
// RFC 9110 gives 426 the opposite sense — the server requires the client to upgrade.
// OpenAI's backend uses it this way and Codex reads it this way, and a client that reads
// neither treats it as an ordinary failed handshake, which is what the 405 already was.
//
// The rows are an ordinary request and response, so the timeline still shows the
// attempt; the request row keeps its upgradeStripped marker, since the Upgrade was not
// forwarded. error.kind "upgrade_declined" says the 426 is ours, as misdirected_request
// does for the 421.
func (s *Server) declineResponsesWebSocket(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog, sid string, chain []attribution.Proc) {
	slog.Debug("forward-proxy: declined a WebSocket upgrade on a Responses path; the client falls back to HTTPS",
		"host", r.Host, "path", r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUpgradeRequired)
	_, _ = io.WriteString(w, declinedUpgradeBody)
	// A delivered answer, not a failed exchange: the deferred RunFinish reads zero as
	// OutcomeError, and nothing went wrong here.
	pctx.StatusCode = http.StatusUpgradeRequired

	if s.Sessions == nil {
		return
	}
	pctx.OutboundSessionID = sid
	if chain != nil {
		defer s.touch(sid, chain)
	}
	s.recordOutboundRequestEvent(tl, pctx, sid)
	s.recordOutboundResponseEvent(pctx, http.StatusUpgradeRequired, &pipeline.EventError{
		Kind:    pipeline.ErrorKindUpgradeDeclined,
		Message: "WebSocket upgrade answered 426 by the proxy; the client falls back to HTTPS",
	})
}

// relayUpgradedWebSocket splices a 101 Switching Protocols answer onto the
// client's connection and blocks until either side closes (#1488). It mirrors
// httputil.ReverseProxy's handleUpgradeResponse — hijack the client connection,
// write the origin's 101 through it, then copy both ways — with two things that
// proxy does not need and this one does: per-direction byte counts for the
// session row, and the row itself.
//
// resp.Body is the origin's connection once the origin answered 101 (the
// transport hands back an io.ReadWriteCloser or it never sent a 101), and this
// function owns it: serveOutbound returns without its resp.Body.Close() defer
// running, because closing the body here would close the WebSocket.
//
// THE ROWS: the request row was recorded before client.Do, as for any request.
// Its pair is recorded here, after the splice ends, carrying the 101 and both
// directions' byte counts — the one response row with bytesUp on it, like a
// tunnel close, because after the 101 the bytes are protocol frames the proxy
// does not parse and there is no body/header split to report. Duration covers
// the whole WebSocket session, not the handshake; ttfb is the handshake, set
// where client.Do returned. That is one audit event per upgrade, the request row
// plus its paired 101 (MOCA MI1 §8.5 item 5).
//
// Response plugins never see the 101 — pctx.ResponseHeaders is not set from it,
// so no RunResponse runs on this answer: the frames after a successful upgrade
// belong to the client and the origin ("streamed, never inspected or buffered",
// the same contract), and a response mutator would have nothing to act on. A
// non-101 answer never reaches here; it takes the ordinary response path.
//
// record=false is skip_hosts: the splice still happens — a skipped request is
// forwarded as a transparent proxy or not at all — but no row is appended, which
// is what skip_hosts promises.
func (s *Server) relayUpgradedWebSocket(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, resp *http.Response, record bool) {
	// The host as this function logs it. r.Host is client-controlled, so it goes
	// through SanitizeControlRunes — every control rune replaced — and then
	// through a strings.ReplaceAll that is a NO-OP by construction, because the
	// sanitizer already took every control rune, "\n" included. The ReplaceAll
	// is not ornament: CodeQL's go/log-injection (this PR's finding) cannot see
	// through a custom function however thorough, and recognizes exactly two
	// sanitizers — this ReplaceAll shape and %q formatting — so the one it knows
	// has to sit in THIS function, on the value's last step before a log call
	// (the query's own test suite proves the shape only in the sink's function;
	// fencing inside SanitizeControlRunes was tried at 6cae5d14 and changed
	// nothing). A helper would put it back inside a callee — do not lift it.
	host := strings.ReplaceAll(pipeline.SanitizeControlRunes(r.Host), "\n", "")
	// BOTH early returns below must close the origin themselves. This function
	// owns resp.Body (serveOutbound skipped its Close defer, see the doc above),
	// and the goroutine that closes it on context-done is only armed further
	// down — so returning before that point without closing here leaves the
	// origin holding a connection it believes is an established WebSocket, until
	// its own idle timeout reaps it. The client meanwhile has a 502, so nothing
	// will ever come back for it (#1549 review).
	// THE ORIGIN MUST BE SWITCHING TO THE PROTOCOL THE CLIENT ASKED FOR. A 101 is
	// not self-describing: it says "switching", and its own Upgrade header says to
	// what. net/http's ReverseProxy compares the two (handleUpgradeResponse refuses
	// when resUpType != reqUpType), and this function is modelled on it, but the
	// comparison was the one part left out — so an origin answering "101 Upgrade:
	// h2c" to a WebSocket handshake got spliced anyway, handing the client an
	// opaque pipe speaking a protocol it never negotiated and cannot parse. The
	// client is the one that would have to detect that, after the proxy already
	// committed its connection. Refuse before the hijack, while a 502 is still
	// writable (#1549 review).
	if !upgradesToWebSocket(resp.Header) {
		got := strings.ReplaceAll(pipeline.SanitizeControlRunes(resp.Header.Get("Upgrade")), "\n", "")
		slog.Error("forward-proxy: origin switched to another protocol than the WebSocket requested — not relaying",
			"host", host, "upgrade", got)
		_ = resp.Body.Close()
		s.failUpgradedWebSocket(w, pctx, record,
			fmt.Sprintf("origin switched to %q, not the websocket requested", got))
		return
	}
	backConn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		// Not reachable through net/http's own transport, which gives a 101 a
		// writable body; a custom RoundTripper could violate it.
		slog.Error("forward-proxy: 101 switching protocols with a non-writable body — cannot relay the upgrade", "host", host)
		// Not an io.ReadWriteCloser, but still an io.Closer: the Body contract
		// guarantees that much, so the origin's end can be released.
		_ = resp.Body.Close()
		s.failUpgradedWebSocket(w, pctx, record, "101 switching protocols with a non-writable body")
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		// The host is fenced above; the hijack error is the runtime's own text.
		slog.Error("forward-proxy: hijack failed relaying a WebSocket upgrade", "host", host, "error", err)
		_ = backConn.Close()
		s.failUpgradedWebSocket(w, pctx, record, fmt.Sprintf("hijack failed relaying the upgrade: %v", err))
		return
	}
	// The origin's end closes when the client's request context is done — which
	// covers a client hang-up the copies have not noticed yet (golang.org/issue
	// 35559, as in ReverseProxy) — or when the splice below ends, whichever is
	// first; the deferred close of conn takes care of the client's end.
	backConnCloseCh := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
		case <-backConnCloseCh:
		}
		backConn.Close()
	}()
	defer close(backConnCloseCh)
	defer conn.Close()

	// The origin's 101, as it wrote it, onto the hijacked connection. Moving
	// resp.Header into w's still-empty header map first is ReverseProxy's trick:
	// resp.Write then serializes status line and headers as one response, and
	// nil'ing the body stops it at the blank line.
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	resp.Header = w.Header()
	resp.Body = nil
	if err := resp.Write(brw); err != nil {
		slog.Error("forward-proxy: writing the 101 failed", "host", host, "error", err)
		s.recordSpliceFailure(pctx, record, fmt.Sprintf("writing the 101 failed: %v", err))
		return
	}
	if err := brw.Flush(); err != nil {
		slog.Error("forward-proxy: flushing the 101 failed", "host", host, "error", err)
		s.recordSpliceFailure(pctx, record, fmt.Sprintf("flushing the 101 failed: %v", err))
		return
	}

	// A delivered answer from here: the deferred RunFinish reads any non-zero
	// status as OutcomeAllow, and 101 is what the client got.
	pctx.StatusCode = http.StatusSwitchingProtocols

	// Both directions, each one io.Copy so its return count needs no second
	// counter. Up is client → origin, read from brw rather than conn so bytes
	// the hijack buffered are not lost; down is origin → client, written
	// straight to conn, which has no write buffer to strand. An EOF propagates
	// as CloseWrite where the connection has it (ReverseProxy's
	// switchProtocolCopier), so a half-closed WebSocket shuts down instead of
	// hanging on the open half.
	var up, down int64
	errc := make(chan error, 2)
	go func() {
		var err error
		if up, err = io.Copy(backConn, brw); err != nil {
			errc <- err
			return
		}
		if wc, ok := backConn.(interface{ CloseWrite() error }); ok {
			errc <- wc.CloseWrite()
			return
		}
		errc <- nil
	}()
	go func() {
		var err error
		if down, err = io.Copy(conn, backConn); err != nil {
			errc <- err
			return
		}
		if wc, ok := conn.(interface{ CloseWrite() error }); ok {
			errc <- wc.CloseWrite()
			return
		}
		errc <- nil
	}()
	// Both, not one: the counts are read below, and a goroutine's send on errc
	// is what publishes its write.
	//
	// But a FAILED direction has to tear both ends down before we wait for the
	// second, or that wait never returns. A clean EOF propagates as CloseWrite
	// above, which is what lets a half-closed WebSocket finish; an error has no
	// such signal, so the peer of the broken direction stays blocked in Read
	// with nothing coming. Measured: an origin that RSTs mid-session ends the
	// origin→client copy with an error while client→origin stays blocked on a
	// silent client, and the 101 row — recorded after both — is never written at
	// all. That is the one-event-per-upgrade promise lost on exactly the
	// connections worth auditing.
	//
	// ReverseProxy avoids this by returning after the FIRST result and letting
	// its deferred closes unblock the other; this function waits for both
	// because it needs their byte counts, so it closes explicitly instead. The
	// counts stay correct: io.Copy returns what it copied before the error, and
	// the closes only release a direction that is already finished or broken
	// (#1549 review).
	spliceErr := <-errc
	if spliceErr != nil {
		_ = conn.Close()
		_ = backConn.Close()
	}
	if err := <-errc; spliceErr == nil {
		spliceErr = err
	}

	if !record || s.Sessions == nil {
		return
	}
	// THE ROW SAYS WHETHER THE SESSION ENDED CLEANLY. Both directions have
	// finished either way and the status is still 101 — the upgrade did succeed,
	// and the client got its 101 — but a relay that ended in an error is not the
	// same event as one that closed cleanly, and without this the only symptom
	// was truncated byte counts: an operator auditing a WebSocket that broke
	// mid-session read a successful upgrade. One event per upgrade is the MOCA
	// contract; one event that cannot distinguish the two outcomes only half
	// keeps it.
	//
	// NOT EVERY ERROR HERE IS A FAILURE, and net.ErrClosed is the one that is not.
	// A WebSocket normally ends with one side closing; the deferred conn.Close()
	// and the context goroutine's backConn.Close() then race the direction still
	// blocked in Read, which returns "use of closed network connection". That is
	// the ordinary shutdown of a healthy session — marking it would flag EVERY
	// WebSocket as broken, which is worse than the gap this closes. Caught by
	// assertUpgradedRows' own "no error on a clean session" assertion, which is
	// why that one exists alongside the broken-direction test: a marker that is
	// always set distinguishes nothing.
	//
	// proxy_error otherwise, the kind the two pre-splice failure paths already
	// use, because a relay that broke is on our side. Sanitized: a network
	// error's text can carry a client-supplied address (#1549 review).
	var fail *pipeline.EventError
	if spliceErr != nil && !errors.Is(spliceErr, net.ErrClosed) {
		fail = &pipeline.EventError{
			Kind:    "proxy_error",
			Message: "websocket relay ended in error: " + pipeline.SanitizeControlRunes(spliceErr.Error()),
		}
	}
	ev := outboundResponseEvent(pctx, http.StatusSwitchingProtocols, fail)
	ev.BytesUp, ev.BytesDown = up, down
	s.Sessions.Append(s.responseSessionID(pctx), ev)
}

// recordSpliceFailure records the response row for a 101 whose relay broke after
// the hijack — the connection is the client's problem now, so there is no status
// to write it, but the request row still exists and a row with no pair reads as a
// request still in flight (#1045). 502 with proxy_error, as failUpgradedWebSocket:
// this failure is ours.
func (s *Server) recordSpliceFailure(pctx *pipeline.Context, record bool, cause string) {
	if !record {
		return
	}
	s.recordOutboundResponseEvent(pctx, http.StatusBadGateway, &pipeline.EventError{
		Kind:    "proxy_error",
		Message: cause,
	})
}

// failUpgradedWebSocket answers a 101 this proxy could not relay and records the
// failure row. The status stays off pctx (zero reads as OutcomeError to the
// deferred RunFinish) for the same reason the client.Do error path in
// serveOutbound keeps its 502 off it: nothing succeeded here.
func (s *Server) failUpgradedWebSocket(w http.ResponseWriter, pctx *pipeline.Context, record bool, cause string) {
	http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
	s.recordSpliceFailure(pctx, record, cause)
}

// tunnelAuthorityMatches reports whether host, a bridged request's Host, names the same
// authority as tunnel, the CONNECT target or transparent destination it was decrypted
// from. Hosts compare case-insensitively without a trailing dot, IP literals by value,
// and a missing port is 443, the tunnel being TLS.
func tunnelAuthorityMatches(host, tunnel string) bool {
	h, hp, ok := splitAuthority(host)
	if !ok {
		return false
	}
	t, tp, ok := splitAuthority(tunnel)
	return ok && h == t && hp == tp
}

func splitAuthority(a string) (host string, port int, ok bool) {
	host, p, err := net.SplitHostPort(a)
	if err != nil {
		// No port: a name, an IPv4 literal, or a bracketed IPv6 literal.
		host, p = a, ""
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
	}
	port = 443
	if p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, false
		}
		port = n
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	return host, port, host != ""
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
func (s *Server) resolveOutboundSessionID(clientHeaders http.Header, chain []attribution.Proc) string {
	if sid := s.resolvePluginSessionID(clientHeaders, chain); sid != "" {
		return sid
	}
	return session.DefaultSessionID
}

// recordingSessionID turns the identity resolved at hydration into the bucket an
// event is recorded under. resolved is the listener's own answer, carried in a
// local so no plugin can rewrite it; "" means hydration found no identity (or ran
// on a path that has none), and recording — unlike a plugin — must still file the
// event somewhere, so the full order including the default bucket applies.
func (s *Server) recordingSessionID(resolved string, clientHeaders http.Header, chain []attribution.Proc) string {
	if resolved != "" {
		return resolved
	}
	return s.resolveOutboundSessionID(clientHeaders, chain)
}

// attributeForRecording resolves a request that never reached hydration — refused before the
// pipeline, or unreadable — pins its attribution on pctx so its rows carry the process, and
// returns the bucket they go in: the attribution's, else ActiveSession(), else default.
func (s *Server) attributeForRecording(pctx *pipeline.Context, clientHeaders http.Header, chain []attribution.Proc) string {
	at := s.attribute(clientHeaders, chain)
	pctx.SetAttribution(at.Proc, at.ClientProc, at.Header)
	if sid := at.RecordID(); sid != "" {
		return sid
	}
	if sid := s.Sessions.ActiveSession(); sid != "" {
		return sid
	}
	return session.DefaultSessionID
}

// recordOutboundRequestEvent records pctx's request row under sid. Its pair is
// recordOutboundResponseEvent, which files under pctx.OutboundSessionID, so the
// caller pins that to sid first.
// syncHeaders makes r's headers the pipeline's: pctx.Headers started as a clone of
// r.Header, so a plugin's set, replace and delete on it are the upstream-facing
// header set. Content-Length and Content-Encoding are left to setBody and the
// transport.
func syncHeaders(r *http.Request, h http.Header) {
	skip := func(k string) bool {
		return strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding")
	}
	for k := range r.Header {
		if skip(k) {
			continue
		}
		if _, ok := h[k]; !ok {
			r.Header.Del(k) // plugin removed it
		}
	}
	for k, vv := range h {
		if skip(k) {
			continue
		}
		if len(vv) == 0 {
			r.Header.Del(k) // pctx.Headers[k] = nil is a delete, same as Del(k)
			continue
		}
		r.Header[k] = append([]string(nil), vv...) // set / overwrite
	}
}

// stripForUpstream removes from r what is the proxy's and not the upstream's: the
// hop-by-hop headers, and, when ownEncoding, the client's Accept-Encoding. It runs on
// every request the proxy sends upstream for a client, a resend included.
//
// keepUpgrade is the WebSocket exception (#1488). RFC 9110 makes Upgrade
// hop-by-hop, and the strip is right everywhere the proxy is a reader between two
// endpoints — but an upgrade exchange makes it an endpoint of the hop instead:
// everything after the 101 is opaque to it, so the header must cross or the
// origin never sees an upgrade request at all. The pair is normalized rather than
// forwarded raw (mirroring httputil.ReverseProxy): Connection becomes "Upgrade"
// and Upgrade "websocket", the one protocol this proxy forwards — a client's
// "Connection: keep-alive, Upgrade" is not the proxy's to pass on, and any other
// token sharing the client's Upgrade list was never going to be forwarded anyway.
func stripForUpstream(r *http.Request, ownEncoding, keepUpgrade bool) {
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
	if keepUpgrade {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}

	// Strip the client's Accept-Encoding so Go's transport negotiates content
	// coding on its own behalf.
	//
	// net/http auto-decompresses a gzip response ONLY when the transport added
	// Accept-Encoding itself. Forwarding the caller's header suppresses that:
	// resp.Body then yields raw compressed bytes. Every body-reading plugin
	// sees binary — the SSE parser finds no "data:" lines and reports a
	// clean EOF on its first ReadFrame, so a gzipped text/event-stream relayed
	// zero frames downstream and finalized aggregating plugins on empty state.
	// The symptom is silent in both directions: the client sees a stream that
	// opens and dies, and token telemetry reads as absent rather than wrong.
	//
	// This proxy parses and inspects bodies, so it cannot be encoding-blind.
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
	if ownEncoding {
		r.Header.Del("Accept-Encoding")
	}
}

// setBody gives r the body a plugin wrote, and clears Content-Encoding, since the
// bytes are the plugin's and carry none.
func setBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	r.Header.Del("Content-Encoding")
}

// resendIfAsked gives the outbound pipeline's Resenders a look at each refused answer,
// at most pipeline.MaxResends times, and sends the request again when one asks. It
// returns the answer to relay: resp itself, or the last send's.
//
// Only a request whose body the pipeline buffered can be sent again, and only a
// Resender's request could need it, so nothing is read from an answer unless a
// Resender is in the chain. A refused answer that is sent again is recorded on its own
// response row, and each further send on a request row of its own under a new request
// id, so the timeline shows both attempts and each pairs with its own answer. A
// refused answer that is not sent again goes to the client byte for byte, the bytes
// read for the Resender included.
func (s *Server) resendIfAsked(r *http.Request, pctx *pipeline.Context, client *http.Client, resp *http.Response, tl *tunnelLog, ownEncoding bool) (*http.Response, error) {
	// Both probes and Resend itself answer for the pipeline this request
	// ran on (Holder.ForPctx): the reload-drain window must not flip the
	// resend decision onto plugins that never saw the request.
	if !s.OutboundPipeline.ForPctx(pctx).HasResenders() || !s.OutboundPipeline.ForPctx(pctx).NeedsRequestBody() {
		return resp, nil
	}
	for round := 0; round < pipeline.MaxResends && resp.StatusCode/100 != 2; round++ {
		head, _ := io.ReadAll(io.LimitReader(resp.Body, pipeline.ResendPeekLimit))
		// The parse as the refused attempt was sent: a Resender may rewrite the model,
		// and the refused attempt's row must name the one the upstream refused.
		sent := pipeline.SnapshotInference(pctx.Extensions.Inference)
		if !s.OutboundPipeline.Resend(r.Context(), pctx, resp.StatusCode, head) {
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
			return resp, nil
		}
		_ = resp.Body.Close()
		if s.Sessions != nil {
			// The refused answer's row: its status and error, from the bytes read. Put
			// back at once, because the status of the answer the client gets is the
			// one OutcomeFromContext must read.
			resent := pctx.Extensions.Inference
			pctx.StatusCode, pctx.ResponseBody, pctx.Extensions.Inference = resp.StatusCode, head, sent
			s.recordOutboundResponseEvent(pctx, resp.StatusCode, nil)
			pctx.StatusCode, pctx.ResponseBody, pctx.Extensions.Inference = 0, nil, resent
			pctx.RenewRequestID()
			s.recordOutboundRequestEvent(tl, pctx, pctx.OutboundSessionID)
		}
		again := r.Clone(r.Context())
		syncHeaders(again, pctx.Headers)
		setBody(again, pctx.Body)
		// No keepUpgrade: a resent request is never a forwarded upgrade — the
		// 101 branch in serveOutbound skips resendIfAsked entirely, and only that
		// branch leaves the headers unstripped.
		stripForUpstream(again, ownEncoding, false)
		var err error
		if resp, err = client.Do(again); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (s *Server) recordOutboundRequestEvent(tl *tunnelLog, pctx *pipeline.Context, sid string) {
	// Snapshot-copy the protocol extension so the request event
	// doesn't see response-phase mutations on the same MCP/Inference
	// struct (e.g. token counts assigned in OnResponse).
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	ev := pipeline.SessionEvent{
		At:              time.Now(),
		Direction:       pipeline.Outbound,
		Phase:           pipeline.SessionRequest,
		RequestID:       pctx.RequestID(),
		BytesUp:         int64(len(pctx.Body)),
		MCP:             pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:       pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations:     pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:         plugins,
		Identity:        pipeline.SnapshotIdentity(pctx),
		Host:            pctx.Host,
		RequestedHost:   pctx.RequestedHost(),
		HTTPMethod:      pctx.Method,
		HTTPPath:        pctx.Path,
		UpgradeStripped: pctx.UpgradeStripped,
		Client:          pctx.ClientInfo(),
		Subagent:        pctx.Subagent(),
		ParentSubagent:  pctx.ParentSubagent(),
		Proc:            pctx.AttributedProc(),
		ClientProc:      pctx.AttributedClientProc(),
		SessionHeader:   pctx.NamingHeader(),
	}
	s.appendOutbound(tl, sid, ev)
}

// recordUnbufferedRequest records the request row and its failure row for a
// request whose body could not be buffered, which therefore never reached the
// pipeline. This return used to record nothing: a request over the size limit
// got its 413 and left no row, so the operator could not see it failed, and on
// a bridged tunnel the only rows were the tunnel's own open and a close saying
// 200. Recording the request through appendOutbound puts the tunnel's open in
// the request's session, as any other recorded request does.
//
// The session is resolved here rather than taken from serveOutbound's
// hydration, which runs after the body is read. Resolution reads only the
// client's headers and process, never the body, so the answer is the one the
// request would have got.
func (s *Server) recordUnbufferedRequest(tl *tunnelLog, pctx *pipeline.Context, r *http.Request, chain []attribution.Proc, err error) {
	if s.Sessions == nil {
		return
	}
	sid := s.attributeForRecording(pctx, r.Header, chain)
	pctx.OutboundSessionID = sid
	if chain != nil {
		defer s.touch(sid, chain)
	}
	status, fail := bodyread.Failure(r, maxRequestBodySize, err)
	s.recordOutboundRequestEvent(tl, pctx, sid)
	s.recordOutboundResponseEvent(pctx, status, fail)
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
// With the client's process known, an empty answer means a process of no agent, and the
// row goes to default like that process's requests.
func (s *Server) tunnelSessionID(resolved string, clientHeaders http.Header, chain []attribution.Proc) string {
	if s.ClientAffinity && resolved == "" && chain == nil && s.Sessions != nil {
		if sid := s.Sessions.ActiveSession(); sid != "" {
			return sid
		}
		return session.DefaultSessionID
	}
	return s.recordingSessionID(resolved, clientHeaders, chain)
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
//
// With the client's process known (chain non-nil), a header claims the session for that
// process as well as for its agent, and a request with no header is filed by process first;
// client affinity and ActiveSession() answer only what it leaves open. See core/attribution.
func (s *Server) resolvePluginSessionID(clientHeaders http.Header, chain []attribution.Proc) string {
	sid, _ := s.resolvePluginSession(clientHeaders, chain)
	return sid
}

// pluginSession is the session handed to plugins and how it was resolved.
type pluginSession struct {
	source pipeline.SessionSource
	header string
}

// hydrateSession resolves the plugin session for a request and, when there is one,
// sets pctx.Session and how it was resolved. It returns the id for the caller's
// local, which recording reads instead of pctx.
func (s *Server) hydrateSession(pctx *pipeline.Context, clientHeaders http.Header, chain []attribution.Proc) string {
	at := s.attribute(clientHeaders, chain)
	pctx.SetAttribution(at.Proc, at.ClientProc, at.Header)
	sid, how := s.pluginSessionOf(at)
	if sid != "" {
		pctx.Session = s.sessionViewFor(sid)
		pctx.SessionSource = how.source
		pctx.SessionHeader = how.header
	}
	if chain != nil && s.processesOn() {
		pctx.ChainSession = at.ChainSession
		if pctx.ChainSession != "" && sid != pctx.ChainSession && how.source == pipeline.SessionSourceHeader {
			if _, seen := s.chainOverrideLogged.LoadOrStore(how.header, struct{}{}); !seen {
				slog.Info("forwardproxy: a process named a session other than its parent agent's; "+
					"policy keyed on the process chain uses the parent's", "header", how.header)
			}
		}
	}
	return sid
}

// resolvePluginSession is resolvePluginSessionID that also says how the id was
// resolved.
func (s *Server) resolvePluginSession(clientHeaders http.Header, chain []attribution.Proc) (string, pluginSession) {
	return s.pluginSessionOf(s.attribute(clientHeaders, chain))
}

// pluginSessionOf is an attribution as plugins hear it: its plugin id, else — for no
// identity at all — the store's ActiveSession(), the in-cluster answer.
func (s *Server) pluginSessionOf(at attribution.Attribution) (string, pluginSession) {
	if sid := at.PluginID(); sid != "" {
		return sid, pluginSession{source: at.Source, header: at.Header}
	}
	if at.Session == "" && at.Source != pipeline.SessionSourcePending && s.Sessions != nil {
		if sid := s.Sessions.ActiveSession(); sid != "" {
			return sid, pluginSession{source: pipeline.SessionSourceActive}
		}
	}
	return "", pluginSession{}
}

// attribute is the attributor's answer for a request, or what an attributor with nothing
// configured gives — no session — when there is none (sessions off, or a test Server built
// without one).
func (s *Server) attribute(clientHeaders http.Header, chain []attribution.Proc) attribution.Attribution {
	a := s.attributor()
	if a == nil {
		return attribution.Attribution{Agent: attribution.AgentOf(clientHeaders)}
	}
	if !s.processesOn() {
		chain = nil
	}
	return a.Resolve(clientHeaders, chain)
}

// attributor is the Server's Attributor, built on first use from SessionIDHeaders and
// ClientAffinity — fields the caller sets after NewServer — and registered with the store,
// which tells it which sessions exist. nil without a store.
func (s *Server) attributor() *attribution.Attributor {
	if s.Sessions == nil {
		return nil
	}
	s.attributorOnce.Do(func() {
		s.attr = attribution.New(attribution.Config{
			SessionIDHeaders: s.SessionIDHeaders,
			ClientAffinity:   s.ClientAffinity,
		})
		s.Sessions.AddRecorder(s.attr)
	})
	return s.attr
}

// touch is attribution's Touch, a no-op without process attribution.
func (s *Server) touch(sid string, chain []attribution.Proc) {
	if a := s.attributor(); a != nil && chain != nil {
		a.Touch(sid, chain)
	}
}

// affinityClient names the coding agent behind a request for session attribution; see
// attribution.AgentOf.
func affinityClient(clientHeaders http.Header) string { return attribution.AgentOf(clientHeaders) }

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

// rejectResponse answers the client with a response-phase rejection and
// records it, before any of the upstream's response is written. The request
// row is already on the timeline; without this row the deny that replaced
// the response left no trace of its own.
func (s *Server) rejectResponse(w http.ResponseWriter, pctx *pipeline.Context, action pipeline.Action) {
	httpx.WriteRejection(w, action)
	if s.Sessions == nil || pctx.Extensions.Invocations == nil {
		return
	}
	s.Sessions.Append(s.responseSessionID(pctx), sessionevent.DenyResponse(pctx, action, pipeline.Outbound))
}

// responseSessionID is the bucket a response row files under: the session
// pinned when the request row was recorded, else the active one, else the
// default.
func (s *Server) responseSessionID(pctx *pipeline.Context) string {
	if sid := pctx.OutboundSessionID; sid != "" {
		return sid
	}
	if sid := s.Sessions.ActiveSession(); sid != "" {
		return sid
	}
	return session.DefaultSessionID
}

// recordOutboundResponseEvent emits the SessionResponse event for a
// completed outbound response. Extracted from handleRequest so the
// streaming path can call it once at end-of-stream and the buffered
// path can call it once after RunResponse — both go through the same
// gate and snapshotting logic.
//
// fail is set where the exchange failed — the client.Do call, or
// bufferResponseBody — and statusCode is then what the client got, not anything
// the upstream sent. Nil when the response was delivered, leaving the error to
// DeriveError.
func (s *Server) recordOutboundResponseEvent(pctx *pipeline.Context, statusCode int, fail *pipeline.EventError) {
	if s.Sessions == nil {
		return
	}
	// Prefer the session pinned when the request event was recorded so the
	// response lands in the same bucket. Fall back to ActiveSession() only
	// when nothing was pinned (defensive — a response-only path), then to
	// the default bucket. See Context.OutboundSessionID.
	sid := s.responseSessionID(pctx)
	// Always record — see the request-phase comment. This is what surfaces
	// responses no plugin acted on (e.g. a generic 404), carrying StatusCode
	// + Error even with empty invocations.
	s.Sessions.Append(sid, outboundResponseEvent(pctx, statusCode, fail))
}

// outboundResponseEvent builds the SessionResponse row every outbound answer is
// recorded through — the shared shape, so the WebSocket splice
// (relayUpgradedWebSocket) can adjust its byte fields without forking the
// snapshotting logic.
func outboundResponseEvent(pctx *pipeline.Context, statusCode int, fail *pipeline.EventError) pipeline.SessionEvent {
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	return pipeline.SessionEvent{
		At:             time.Now(),
		Direction:      pipeline.Outbound,
		Phase:          pipeline.SessionResponse,
		RequestID:      pctx.RequestID(),
		BytesDown:      pctx.ResponseBytes,
		MCP:            pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:      pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations:    pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:        plugins,
		Identity:       pipeline.SnapshotIdentity(pctx),
		Host:           pctx.Host,
		RequestedHost:  pctx.RequestedHost(),
		HTTPMethod:     pctx.Method,
		HTTPPath:       pctx.Path,
		StatusCode:     statusCode,
		Error:          pipeline.EventErrorOr(pctx, fail),
		Duration:       pipeline.DurationSince(pctx.StartedAt),
		Ttfb:           pipeline.DurationBetween(pctx.StartedAt, pctx.ResponseHeadersAt),
		Client:         pctx.ClientInfo(),
		Subagent:       pctx.Subagent(),
		ParentSubagent: pctx.ParentSubagent(),
		Proc:           pctx.AttributedProc(),
	}
}

// bufferResponseBody reads resp's body whole for the response phase. When it
// cannot, it answers the client with a 502, records the response row the request
// is owed, and returns false — these returns used to leave no trace at all, which
// is #1045 on the paths that did reach the upstream.
//
// The row's statusCode is what the CLIENT got, and error.code is what the
// upstream sent. Every consumer keys on statusCode, so keeping the upstream's 200
// there would count a failed exchange as a healthy one in /v1/usage and agentop.
//
// Of the two failures only the size limit is ours, so only it says proxy_error.
// A read that errors usually means the upstream reset or truncated mid-body, and
// is classified like a failed call; less often the client hung up while we read,
// which ExchangeFailure reports as such.
//
// pctx.StatusCode goes back to zero, as on a failed call: the upstream's response
// was never delivered, and OutcomeFromContext would otherwise report its status
// as an allow — lineage labelling "ok" an exchange the client saw fail.
func (s *Server) bufferResponseBody(w http.ResponseWriter, r *http.Request, resp *http.Response, pctx *pipeline.Context) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err == nil && len(body) <= maxBodySize {
		return body, true
	}
	var status int
	var fail *pipeline.EventError
	if err != nil {
		status, fail = pipeline.ExchangeFailure(r.Context(), err)
		if r.Context().Err() == nil {
			slog.Warn("forward-proxy: response body read error", "host", r.Host, "kind", fail.Kind, "error", fail.Message)
		}
		http.Error(w, `{"error":"response body read error"}`, http.StatusBadGateway)
	} else {
		slog.Warn("forward-proxy: response body too large", "host", r.Host, "limit", maxBodySize)
		http.Error(w, `{"error":"response body too large"}`, http.StatusBadGateway)
		status, fail = http.StatusBadGateway, &pipeline.EventError{
			Kind:    "proxy_error",
			Message: fmt.Sprintf("response body over the %d-byte buffer limit", maxBodySize),
		}
	}
	fail.Code = strconv.Itoa(resp.StatusCode)
	pctx.StatusCode = 0
	s.recordOutboundResponseEvent(pctx, status, fail)
	return nil, false
}

// classifyBrokenStream classifies a mid-body read error on a relay path
// (#1543). fail is what the response row should carry — client_canceled
// when the client hung up first, the transport kind otherwise, with the
// upstream's status as error.code exactly as the buffered path records it.
// Two of the errors that reach here are the proxy's own cuts, not the
// upstream's — the idle read cutoff and the per-frame size cap — and both
// record proxy_error, the kind CLAUDE.md reserves for "the one that is
// ours", exactly as the buffered path records its own size limit. An
// operator reading an upstream_error row should be looking at the network,
// never at streamReadIdleTimeout.
//
// abort says the client's connection must be reset — the caller panics with
// http.ErrAbortHandler — instead of letting the handler return normally: a
// normal return makes net/http write the terminating chunk (h1) or
// END_STREAM (h2), handing the client a COMPLETE response off a stream the
// upstream cut off. ErrAbortHandler is the standard library's own abort
// signal: deferred finalization still runs, and net/http does not log it as
// a panic. abort is false only when the client hung up first — there is
// nobody left to abort for.
// pctx.StatusCode goes back to zero, as the buffered path does on a failed
// read: the deferred RunFinish derives the outcome from it after the handler
// returns, and a 200 there would report OutcomeAllow for an exchange the
// client saw reset — lineage labelling "ok" a failed one, the exact
// misreading the buffered path's zero exists to prevent. The row is
// unaffected: it records resp.StatusCode, the status already on the wire.
func classifyBrokenStream(pctx *pipeline.Context, r *http.Request, resp *http.Response, err error) (fail *pipeline.EventError, abort bool) {
	switch {
	case errors.Is(err, errIdleCut):
		fail = &pipeline.EventError{Kind: "proxy_error", Message: fmt.Sprintf("stream idle over the proxy's %s read cutoff", streamReadIdleTimeout)}
	case errors.Is(err, sseframe.ErrFrameTooLarge):
		fail = &pipeline.EventError{Kind: "proxy_error", Message: fmt.Sprintf("single SSE frame over the %d-byte frame cap", maxBodySize)}
	default:
		_, fail = pipeline.ExchangeFailure(r.Context(), err)
	}
	fail.Code = strconv.Itoa(resp.StatusCode)
	pctx.StatusCode = 0
	if r.Context().Err() != nil {
		slog.Debug("forward-proxy: streaming response ended by client hangup", "host", r.Host, "error", err)
		return fail, false
	}
	slog.Warn("forward-proxy: streaming response read error — aborting the client connection",
		"host", r.Host, "kind", fail.Kind, "error", fail.Message)
	return fail, true
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

// isKnownMislabeledSSE reports whether host+path is a known endpoint that sends real SSE
// wire bytes (event:/data: lines) under the wrong Content-Type.
//
// Confirmed on live traffic: Codex's chatgpt.com-hosted Responses API gateway sends
// event-stream content labeled Content-Type: application/json. Content-Type is otherwise
// the single signal this function's caller trusts to decide streaming vs. buffered — see
// isEventStream — and with the wrong header, this response would never reach
// handleStreamingResponse at all, forcing the buffered path (and losing live delivery to
// the client) regardless of what any plugin declares. inference-parser's own dispatch has
// a second, independent content-sniff fallback (carriesSSEFraming) for the case where the
// buffered path is still what runs — this is the other half, letting the response stream
// live in the first place when the one plugin that cares (inference-parser) supports it.
//
// Deliberately a narrow, named-endpoint allowlist rather than sniffing every response's
// body to detect SSE by content: that would mean reading ahead on EVERY response regardless
// of provider, to work around one provider's one wrong header. Add a line here if another
// endpoint turns out to have the same defect.
func isKnownMislabeledSSE(host, path string) bool {
	return host == "chatgpt.com" && path == "/backend-api/codex/responses"
}

// handleStreamingResponse forwards a text/event-stream response to the
// downstream client block by block. Each SSE block carrying data is
// delivered to the pipeline's StreamingResponder plugins (recording-only
// today), and then every block — data or not — is written exactly as the
// upstream sent it and flushed. End-of-stream
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

	// Set by the read-error arm below; the deferred recorder puts it on the
	// response row so a stream the upstream broke mid-body stops showing as
	// a clean 200 in agentop (#1543).
	var streamFail *pipeline.EventError

	// Defer the final last=true dispatch + session-event recording so
	// every exit path (normal EOF, upstream read error, downstream
	// client-write error, the ErrAbortHandler panic below) finalizes
	// aggregating plugins and records the response event. Without this, a
	// client disconnect mid-stream leaves inference/a2a stuck in an
	// unfinalized state and emits no SessionResponse row to agentop.
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
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, streamFail)
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
	for {
		block, err := reader.ReadBlock()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Read error or oversized single frame. We can't promote this to
			// 502 — headers are sent — so the client's only honest signal is a
			// broken connection: returning normally would write the terminating
			// chunk and hand it a COMPLETE stream off a cut-off one (#1543).
			// The deferred finalize + recorder above still run, both on the
			// panic path and the plain break.
			// bytesRead, not the bytesWritten this used to keep in a local: the
			// local was declared after the recording defer and never reached the
			// session event, so the figure is now taken off the counting wrapper
			// around resp.Body — which is bytes received, not frames relayed.
			var abort bool
			streamFail, abort = classifyBrokenStream(pctx, r, resp, err)
			if abort {
				panic(http.ErrAbortHandler)
			}
			break
		}

		// Plugins see the payload BEFORE the block's bytes are written, so a
		// Reject still keeps the rejected block off the wire. Blocks with no
		// data line — heartbeats, a lone retry: — are not shown to plugins,
		// exactly as ReadFrame never returned them.
		if block.HasData {
			respAction := s.OutboundPipeline.RunResponseFrame(r.Context(), pctx, block.Data, false)
			if respAction.Type == pipeline.Reject {
				// Headers + earlier frames already on the wire — log and
				// stop forwarding. The downstream client sees a truncated
				// stream, which is the best we can do without inspect-
				// before-forward semantics.
				slog.Warn("forward-proxy: streaming response rejected mid-stream by plugin",
					"host", r.Host, "violation", respAction.Violation)
				break
			}
		}

		// The block exactly as the upstream sent it. Nothing here needs a
		// rebuilt frame: StreamingResponders only read, and a response-body
		// writer forces the buffered path before this one is chosen. A rebuild
		// used to drop event:, id:, retry: and comment lines, which clients
		// type, resume and keep alive by.
		if _, err := w.Write(block.Raw); err != nil {
			slog.Debug("forward-proxy: streaming write error", "host", r.Host, "error", err)
			break
		}
		flusher.Flush()
	}
}

// streamPassthrough forwards a text/event-stream response to the downstream
// client byte-for-byte with per-write flushing. It is the streaming path when
// no StreamingResponder plugin is configured (a plain proxy pipeline). It
// relays the exact upstream bytes, as handleStreamingResponse does; the two
// differ only in whether StreamingResponders are dispatched. Without this path such a response fell
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
		s.rejectResponse(w, pctx, respAction)
		return
	}

	// Set by the read-error arm below; the deferred recorder puts it on the
	// response row so a stream the upstream broke mid-body stops showing as a
	// clean 200 in agentop (#1543).
	var streamFail *pipeline.EventError

	// Record the response event on every exit path (normal EOF, upstream read
	// error, downstream write error, the ErrAbortHandler panic below) so a
	// SessionResponse row still lands. A closure, not a bare defer call:
	// defer evaluates its arguments up front, so streamFail must be read at
	// exit time, not at entry.
	defer func() {
		s.recordOutboundResponseEvent(pctx, resp.StatusCode, streamFail)
	}()

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
				// Returning normally would write the terminating chunk and
				// hand the client a COMPLETE stream off a cut-off one (#1543);
				// the deferred recorder above still runs on the panic path.
				var abort bool
				streamFail, abort = classifyBrokenStream(pctx, r, resp, readErr)
				if abort {
					panic(http.ErrAbortHandler)
				}
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
	respBody, ok := s.bufferResponseBody(w, r, resp, pctx)
	if !ok {
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
		s.rejectResponse(w, pctx, respAction)
		return
	}
	if s.OutboundPipeline.ForPctx(pctx).HasStreamingResponders() {
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
				s.rejectResponse(w, pctx, frameAction)
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
			s.rejectResponse(w, pctx, finalAction)
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
	s.appendOutbound(tl, sid, sessionevent.Deny(pctx, action, pipeline.Outbound))
}

// egressAllowRefusal builds the destination_denied refusal for an off-list
// destination and records the deny invocation on pctx. The invocation is
// what makes the denied-phase session row land at all — recordOutboundRejectIn
// skips events with no invocations, and a listener gate is not a plugin, so
// the record is ours to make. Phase is stamped explicitly because the gate
// runs outside a pipeline pass, where the framework's currentPhase is empty
// and the row's snapshot filters by it. Details name the host the way the
// moca conformance suite's audit event reads it ({decision:
// destination_denied, host, ...}); #1482's JSON audit channel builds on the
// same row.
func egressAllowRefusal(pctx *pipeline.Context, host string) pipeline.Action {
	pctx.Record(pipeline.Invocation{
		Plugin:  "egress-allowlist",
		Action:  pipeline.ActionDeny,
		Phase:   pipeline.InvocationPhaseRequest,
		Reason:  "destination_denied",
		Details: map[string]string{"host": host},
	})
	return pipeline.MocaRefusal(http.StatusForbidden, "destination_denied", host)
}

// connectDialTimeout bounds the upstream TCP dial for a CONNECT tunnel.
// Once the tunnel is open the timeout no longer applies — the agent's TLS
// handshake and subsequent traffic flow at their own pace.
//
// 10s, not the 30s it long was, because the only dials that wait it out are
// black-holed ones (#1251): a refused or unroutable destination answers a TCP
// SYN with an RST in well under a second, while a host that drops the SYN
// silently — off the VPN that reaches it, behind a dead NAT — consumes the
// whole budget and the agent spends it staring at nothing. By 10s the initial
// SYN and its first four scheduled retransmits (1s, 2s, 4s, 8s) have all gone
// unanswered; a handshake that misses five straight tries is not being saved
// by a sixth at 16s, so the rest of the budget bought no connection, only
// latency before the 502. Matches the budget the bridge already trusts for
// its own upstream probe (upstreamVerifyTimeout) and the mTLS dialer's plain
// dial, which had settled on 10s independently.
const connectDialTimeout = 10 * time.Second

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
	attribution.Strip(r.Header)
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

	// Egress allow list (#1483): when on, an off-list CONNECT target is refused
	// here — before the SkipHosts match, so a skip entry cannot exempt an
	// off-list host (the allowlist is the outer boundary), and before the
	// pipeline and the dial, so the destination is neither resolved nor
	// contacted: the refusal happens at a point Go's resolver has not run
	// (it runs inside net.DialTimeout, which this return never reaches), which
	// is the conformance contract's "no DNS, no connect" by construction. The
	// named 403 is written with plain WriteRejection, never
	// WriteRejectionForRequest: the egress boundary's answer is the contract,
	// and the MCP JSON-RPC 200 rewrite would hide it behind a protocol frame.
	if s.EgressAllow != nil && !s.EgressAllow.Allows(r.Host, "tcp") {
		host := hostOnly(r.Host)
		action := egressAllowRefusal(pctx, host)
		// Session resolution the recordUnbufferedRequest way — pre-pipeline,
		// from the client's headers and process only — so the denied row lands
		// in the bucket the request would have recorded under.
		chain := s.clientChain(r)
		s.recordOutboundReject(pctx, action, s.recordingSessionID("", r.Header, chain))
		// Sanitized like the serveOutbound refusal: host is client-controlled and
		// lands in a log file.
		slog.Info("forward-proxy: CONNECT destination refused by egress allow list",
			"host", pipeline.SanitizeControlRunes(host))
		httpx.WriteRejection(w, action)
		return
	}

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
	var chain []attribution.Proc
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
		chain = s.clientChain(r)
		if s.Sessions != nil {
			sessionID = s.hydrateSession(pctx, r.Header, chain)
		}

		// Run the outbound pipeline. Plugins that policy on host/identity
		// (ibac, content gates) still get to allow/deny; plugins that need
		// HTTP body (parsers) see no body, which they handle gracefully.
		action := s.OutboundPipeline.Run(r.Context(), pctx)
		if action.Type == pipeline.Reject {
			s.recordOutboundReject(pctx, action, s.tunnelSessionID(sessionID, r.Header, chain))
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
	// the reason in tunnelSessionID. See recordTunnelOpened. With process attribution the
	// pin is the process's answer, which is never empty.
	if !skipped && s.Sessions != nil {
		switch {
		case chain != nil && s.processesOn():
			// The client's process is known, so the tunnel's own row goes where that
			// process's requests go — default included, for a process of no agent.
			pctx.OutboundSessionID = s.recordingSessionID(sessionID, r.Header, chain)
		case s.ClientAffinity && sessionID != "":
			pctx.OutboundSessionID = sessionID
		}
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
	tl.conn = connProcOf(r.Context())

	// Plain TCP dial. See package-level comment on why mTLS doesn't
	// apply here. r.Host on a CONNECT carries "host:port" already.
	//
	// dialTarget is what actually gets dialed: r.Host itself when the guard is
	// off, else the address the guard checked — resolved once here, and reused
	// verbatim by the bridge-fall-open re-dial below, so no second resolution
	// can ever re-answer between check and connect (the DNS-rebinding window).
	dialTarget := r.Host
	bridgeable := s.bridgeable(r.Host)
	var resolveErr error // a non-refusal CheckAddr failure: the name would not resolve
	if s.EgressGuard != nil {
		checked, cerr := s.EgressGuard.CheckAddr(r.Context(), "tcp", r.Host)
		if cerr != nil {
			var ref *egressguard.Refusal
			if errors.As(cerr, &ref) {
				slog.Warn("forward-proxy: CONNECT destination refused by egress guard",
					// Sanitized like the Debug line in serveOutbound: r.Host is
					// client-controlled and lands in a log file.
					"host", pipeline.SanitizeControlRunes(r.Host), "addr", ref.Addr.String())
				// A policy answer, not a failed exchange. skip_hosts destinations
				// are refused too — a bypass must not become an SSRF bypass — and
				// tl's rows are no-ops for them, exactly as the dial failure's are.
				httpx.WriteRejection(w, pipeline.DenyStatus(http.StatusForbidden, "destination_private",
					fmt.Sprintf("refusing to reach private destination %s (%s)", ref.Host, ref.Addr)))
				// The 426 rule again: a delivered 403 the Finisher reads as an
				// allow, because the guard did what it was configured to.
				pctx.StatusCode = http.StatusForbidden
				tl.open(pipeline.TunnelDestPrivate)
				tl.close(http.StatusForbidden, &pipeline.EventError{
					Kind:    pipeline.ErrorKindDestinationPrivate,
					Message: ref.Error(),
				}, 0, 0)
				return
			}
			// Not a refusal: the name would not resolve. Same shape as a dial
			// failure — nothing was dialed, and the client gets the 502. Where the
			// bridge could take the connection, fall through instead, carrying the
			// resolution error as the pre-dial's failure: the bridge can still
			// bridge it, and a redirect can move the request to a name that
			// resolves (#1595). The empty checked address is never dialed.
			// Both values sanitized: the name is client-controlled, and the error
			// embeds it (net.DNSError quotes the name it failed to resolve).
			if !bridgeable {
				slog.Warn("forward-proxy: CONNECT destination resolution failed",
					"host", pipeline.SanitizeControlRunes(r.Host),
					"error", pipeline.SanitizeControlRunes(cerr.Error()))
				http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
				tl.open(pipeline.TunnelDialFailed)
				tl.close(http.StatusBadGateway, dialError(cerr), 0, 0)
				return
			}
			resolveErr = cerr
		}
		dialTarget = checked
		// The audit trail against rebinding: "asked for the name, dialed this
		// address" — moca's egress conformance reads it off the tunnel open row.
		pctx.DialedAddr = checked
	}
	var upstream net.Conn
	var memoized bool
	err := resolveErr
	if err == nil {
		upstream, err, memoized = s.dialUpstream(dialTarget, bridgeable)
	}
	// dialFail remembers a pre-dial that failed where the bridge could take the
	// connection: the connection continues with no upstream, because the bridge
	// can still take it — a plugin redirect (inference-router moving a pinned
	// session) sends the decrypted request to a live server, and an opaque tunnel
	// to a dead origin helps nobody (#1595). Every path below that would have used
	// the upstream handles a nil one by closing with this error or re-dialing
	// (tunnelOrDial). freshDialFail is the failure when it came from a dial this
	// connection made itself, not a memo hit: re-dialing would only repeat it
	// behind the 200 the client already has, so tunnelOrDial closes with it.
	var dialFail, freshDialFail error
	if err != nil {
		if !bridgeable {
			slog.Warn("forward-proxy: CONNECT upstream dial failed", "host", r.Host, "error", err)
			http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
			// Recorded rather than dropped: an unreachable destination used to leave no
			// trace in the timeline at all.
			tl.open(pipeline.TunnelDialFailed)
			tl.close(http.StatusBadGateway, dialError(err), 0, 0)
			return
		}
		slog.Warn("forward-proxy: CONNECT upstream dial failed; trying the TLS bridge",
			"host", r.Host, "error", err)
		dialFail = err
		if !memoized {
			freshDialFail = err
		}
	}

	clientConn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		if upstream != nil {
			_ = upstream.Close()
		}
		slog.Error("forward-proxy: CONNECT hijack failed", "host", r.Host, "error", err)
		return
	}

	// TCP keepalive on both ends. Streaming LLM completions can hold
	// the tunnel open for minutes; without keepalives, a vanished peer
	// (network partition, NAT entry expiry, peer reboot) parks the
	// io.Copy goroutines until the OS finally times the socket out.
	// 30s is loose enough to not perturb idle traffic and tight enough
	// that operators get prompt cleanup on dead connections.
	if upstream != nil {
		enableKeepalive(upstream)
	}
	enableKeepalive(clientConn)

	// Tell the agent the tunnel is up. Per RFC 7231 §4.3.6 a 200 to
	// CONNECT signals "tunnel established"; the body is empty and any
	// subsequent bytes from either side are application data.
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		if upstream != nil {
			_ = upstream.Close()
		}
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
		var bridge bool
		reason, bridge = s.bridgeVerdict(r, tl, key, first)
		if bridge {
			s.noteBridgeAttempt()
			if upstream != nil {
				_ = upstream.Close() // bridgeServe dials its own verified upstream
			}
			if s.bridgeServe(clientConn, authority, key, tl, dialFail != nil) {
				return
			}
			// fell open → re-dial for the tunnel. That tunnel is opaque, so it
			// closes like any other opaque one. dialTarget, not r.Host: the address
			// was already checked once, and re-resolving here would reopen the
			// rebinding window the check closed. freshDialFail, when set, skips the
			// re-dial: the pre-dial already failed on a real dial of this target.
			s.tunnelOrDial(clientConn, dialTarget, tl, freshDialFail)
			return
		}
	}

	// Every path that reaches here left the bytes opaque without entering
	// bridgeServe, so this is where their reason gets recorded — except a
	// certainly-dead origin, which keeps the dial-failed reason the docs and
	// dashboards key on, whatever made the bridge decline.
	// The pre-dial failed (or was skipped as memoized-dead) and the bridge
	// declined the connection: dial now — the memo's optimism was for the bridge
	// path, and a plain tunnel still needs a real upstream. A fresh failure needs
	// no second dial; a memo hit is up to dialFailMemoTTL old, and the origin may
	// have come back.
	if upstream == nil {
		if freshDialFail != nil {
			tl.open(pipeline.TunnelDialFailed)
		} else {
			tl.open(reason)
		}
		s.tunnelOrDial(clientConn, dialTarget, tl, freshDialFail)
		return
	}
	tl.open(reason)

	// Bidirectional copy until either side closes.
	sent, received := tunnel(clientConn, upstream)
	tl.close(http.StatusOK, nil, sent, received)
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
//
// When the timer — not the upstream — is what closed the body, Read
// surfaces errIdleCut instead of the bare closed-body error, so
// classifyBrokenStream can record the cut as proxy_error rather than
// blaming a healthy upstream (#1543: on the issue's own log, 68 of
// 103 truncations were this cutoff killing MCP's standing GET).
type idleReadCloser struct {
	rc        io.ReadCloser
	timeout   time.Duration
	closeOnce sync.Once
	cutFired  atomic.Bool
}

// errIdleCut is the read error idleReadCloser returns when its timer closed
// the body. Matched with errors.Is; see classifyBrokenStream.
var errIdleCut = errors.New("stream cut by the proxy's idle read cutoff")

func idleReader(rc io.ReadCloser, timeout time.Duration) io.ReadCloser {
	return &idleReadCloser{rc: rc, timeout: timeout}
}

func (i *idleReadCloser) Read(p []byte) (int, error) {
	timer := time.AfterFunc(i.timeout, i.cut)
	n, err := i.rc.Read(p)
	timer.Stop()
	if err != nil && i.cutFired.Load() {
		return n, errIdleCut
	}
	return n, err
}

// cut is the timer's callback: mark that THIS close was the cutoff's, so a
// concurrent or next Read reports the cut rather than an upstream break.
func (i *idleReadCloser) cut() {
	i.cutFired.Store(true)
	i.closeIdempotent()
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
	// conn is the CONNECT's client process slot, handed to the requests a bridged tunnel
	// decrypts; nil off the CONNECT path.
	conn *connProc
	// program is the client's program when the bridge rules named it, nil when they did
	// not. bridgeServe records a handshake's failure against it when set and against the
	// host when not, which is what the transparent listener and a nameless client get.
	program *tlsbridge.Program

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

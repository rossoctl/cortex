// Package forwardproxy implements an HTTP forward proxy listener.
// Agents set HTTP_PROXY to route outbound traffic through this proxy
// for transparent token exchange.
package forwardproxy

import (
	"bytes"
	"context"
	cryptotls "crypto/tls"
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

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/egressallow"
	"github.com/rossoctl/cortex/core/listener/egressguard"
	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/bodycount"
	"github.com/rossoctl/cortex/core/listener/internal/bodyread"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/peerproc"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/redact"
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

	// BridgedIdleTimeout closes a TLS-bridged tunnel once it has been idle
	// between decrypted requests this long, and on HTTP/1.1 bounds reading
	// each request's headers, the first included. Zero keeps it open until
	// either side closes.
	BridgedIdleTimeout time.Duration

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

	// ResponsesWebSockets is what a WebSocket upgrade on a Responses API path gets:
	// config.ResponsesWebSocketsParse carries and parses it (serveResponsesWebSocket),
	// Decline answers 426 (declineResponsesWebSocket), and Forward forwards it and
	// splices the 101 like every other WebSocket (#1488), its frames opaque — which
	// is also what "" means here, so a Server built without it forwards as one
	// always has. cmd/cortex sets it from
	// config.ListenerConfig.ResponsesWebSocketsMode, which defaults to parse.
	ResponsesWebSockets string

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

	// Hop, when non-nil, sends each recorded request to cortex-gateway at the dial
	// (praxis mode, #1721). nil in direct mode, and then serveOutbound is as before.
	Hop *Hop

	// Processes, when non-nil, names the process behind each client connection, and a
	// request with no session header is filed under that process's session — or its
	// nearest ancestor's — before client affinity is consulted; see core/attribution. Set only where every client is a process on this
	// host (session.process_attribution) and only from a resolver whose self-test passed
	// (peerproc.New). The http.Server must carry ConnContext. nil keeps today's
	// resolution byte for byte.
	Processes peerproc.Resolver

	// ProjectOf, when non-nil, names the project (project.Label of the repo root) of the
	// process with pid, recorded as SessionEvent.Project. Used only while process
	// attribution is on, once per client connection. nil records no project (#1815).
	ProjectOf func(pid int32) string

	// TrustedSessionID, when set, is the listener's own answer to "which
	// session is this request on" — resolved from how the connection arrived
	// (which per-VM socket accepted it, vmcontrol), not from anything the
	// client sent. A non-empty answer wins over every header- and
	// process-derived id, and the client's own session headers never override
	// it. #1476 will generalise this into full connection-derived identity;
	// this is deliberately the narrow seam it needs.
	TrustedSessionID func(r *http.Request) string

	// AdoptBridgedRequest, when set, re-applies a connection-derived identity
	// to each request a TLS-bridged tunnel decrypts, given the CONNECT that
	// opened it. It exists because a bridged request is NOT the CONNECT: the
	// bytes are re-parsed into fresh *http.Requests inside tlsbridge.ServeConn's
	// own http.Server, so neither the CONNECT's context nor any header the
	// accepting listener stripped survives into them. A listener that derives
	// identity from the connection (vmcontrol: which per-VM socket accepted it)
	// must therefore be given a second chance to stamp it, or TrustedSessionID
	// answers "" on the decrypted request and resolution falls through to the
	// guest's own session headers — which is the whole seam inverted.
	//
	// connect is the CONNECT request as the listener's wrapper saw it, so a
	// hook can read the identity it stamped there; r is the decrypted request
	// to decorate. With ok true, adopted is non-nil and replaces r. ok false
	// refuses r: the hook has answered it on w, and nothing else runs for it.
	AdoptBridgedRequest func(w http.ResponseWriter, connect, r *http.Request) (adopted *http.Request, ok bool)

	// bufferedFallbackOnce keeps the SSE-buffered-path notice to one line per
	// process; the condition is a supported chain shape, not an error.
	bufferedFallbackOnce sync.Once

	// selfTrafficSeen holds the (executable, service) pairs selfTraffic has announced.
	selfTrafficSeen sync.Map

	// chainOverrideLogged holds the session header names hydrateSession has
	// reported naming a session other than the process chain's.
	chainOverrideLogged sync.Map

	// upstreamUnverifiedWarned holds the hosts logUpstreamUnverified has warned about.
	upstreamUnverifiedWarned sync.Map

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
	// Hop branch point 1: a client must not send its own hop headers.
	if s.Hop != nil {
		stripHopHeaders(r.Header)
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

	// The connection's own answer to "which session", resolved HERE rather than at
	// hydration, because a request can be refused before it ever reaches hydration
	// — an off-list egress destination, a body too large to buffer, a bridged
	// request whose Host is not its tunnel's. Each of those records a row, and a
	// row filed by header/process/ActiveSession() for a VM whose identity the
	// connection already settled lands in another VM's session or in `default`:
	// the denied-egress event you most want attributed correctly, attributed to
	// someone else. Resolving once up here and threading it into every recording
	// site is what makes "identity from the connection wins over every header- and
	// process-derived id" true for refusals too, not just for requests that make
	// it to the pipeline (#1569 review).
	trusted := s.trustedSessionOf(r)

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
		s.recordOutboundRejectIn(tl, pctx, action, s.attributeForRecording(pctx, trusted, r.Header, chain, connProcOf(r.Context())))
		// Sanitized: host (the URL authority), Method and Path are all client-controlled,
		// and a control rune forged into any of them would forge a log entry — the same
		// CWE-150 shape #1539's egress guard was flagged for.
		slog.Info("forward-proxy: destination refused by egress allow list",
			"host", pipeline.SanitizeLogValue(host),
			"method", pipeline.SanitizeLogValue(r.Method),
			"path", pipeline.SanitizeLogValue(r.URL.Path))
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
			"host", pipeline.SanitizeLogValue(pctx.Host), "pattern", pat, "method", r.Method, "path", pipeline.SanitizeLogValue(r.URL.Path))
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

	// capture is the request as the agent sent it, for the session archive's lossless capture
	// (#1494). Only a buffered body is captured: buffering one for capture alone would delay
	// every request no plugin reads.
	var capture *pipeline.RequestCapture
	captures := s.Sessions.TakesCaptures()
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
		slog.Debug("forward-proxy: buffered request body", "host", pipeline.SanitizeLogValue(r.Host), "bodyLen", len(body))
		// Before any plugin runs, which is the only time the agent's own bytes and headers
		// are still here: a rewrite replaces pctx.Body and keeps no copy. Only where something
		// keeps captures: an in-cluster sidecar has no archive.
		if captures {
			capture = &pipeline.RequestCapture{Original: body, Header: redact.Headers(r.Header), Query: redact.Query(r.URL.RawQuery)}
		}
	}

	// Establish the session identity ONCE, here, and keep it in a local: the
	// identity handed to plugins and the bucket events are recorded under must be
	// the same answer, not two rules that happen to agree. Plugins key on
	// pctx.SessionID (sessionbudget's Redis counters, contextguru's compaction
	// state, sparc), so resolving this from ActiveSession() alone attributed two
	// concurrent coding-agent sessions to whichever spoke last.
	//
	// A local rather than pctx.OutboundSessionID, deliberately. That field is
	// exported and the pipeline runs between here and every recording site, so a
	// plugin could otherwise write it and silently re-file both the request and
	// its paired response — which recordingSessionID's own contract forbids,
	// and which would fail invisibly. The listener's answer wins; the pin is
	// assigned from this local after the pipeline has run.
	// The store's absence gates the header/process resolution, not the trusted
	// answer: that one costs no store (sessionViewFor falls back to an ID-only
	// view) and is the VM's identity, which plugins must see whether or not
	// anyone is recording events. Throwing it away under `session.enabled:
	// false` would quietly un-identify every VM in a MOCA deployment that turned
	// the store off (#1569 review).
	var sessionID string
	if !skipped && (s.Sessions != nil || trusted != "") {
		sessionID = s.hydrateSession(pctx, trusted, r.Header, chain, connProcOf(r.Context()))
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
	// keeps the strip. A Responses API WebSocket is listener.responses_websockets'
	// to decide: only forward splices it like any other, while parse and decline
	// answer it below, with the socket carried or a 426.
	answeredHere := wantsResponsesWebSocket(r) &&
		(s.ResponsesWebSockets == config.ResponsesWebSocketsParse || s.ResponsesWebSockets == config.ResponsesWebSocketsDecline)
	forwardUpgrade := upgradesToWebSocket(r.Header) && !answeredHere

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

	// After the pipeline, so a plugin's reject still wins; before any row, since both
	// arms record their own. A socket that cannot be carried is declined rather than
	// forwarded: forwarded, it would be a session no plugin sees, under a mode that
	// asked for every turn to be parsed.
	if !skipped && answeredHere {
		switch s.ResponsesWebSockets {
		case config.ResponsesWebSocketsParse:
			sid := s.recordingSessionID(sessionID, r.Header, chain)
			if why := s.cannotCarryWebSocket(tl, s.OutboundPipeline.ForPctx(pctx)); why != "" {
				slog.Debug("forward-proxy: declining a Responses WebSocket the proxy cannot carry", "host", pipeline.SanitizeLogValue(r.Host), "reason", why)
				s.declineResponsesWebSocket(w, r, pctx, tl, sid, chain)
				return
			}
			s.serveResponsesWebSocket(w, r, pctx, tl, sid, chain)
			return
		case config.ResponsesWebSocketsDecline:
			s.declineResponsesWebSocket(w, r, pctx, tl, s.recordingSessionID(sessionID, r.Header, chain), chain)
			return
		}
	}

	// Hop branch point 2: the gateway records the rows of a request the hop sends.
	// An origin-form request has no dial target; it stays on the direct path, where client.Do refuses it with a 502.
	hopped := s.Hop != nil && !skipped && !forwardUpgrade && r.URL.Scheme != "" && r.URL.Host != ""
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
		if hopped {
			s.Hop.own(pctx, hopWire(pctx, r.Header, sid))
			defer s.Hop.release(pctx)
		} else {
			s.recordCapturedRequest(tl, pctx, sid, capture)
		}
	}

	// Propagate every header mutation the outbound pipeline made to the
	// forwarded request. pctx.Headers started as a clone of r.Header, so
	// plugins' set / replace / delete operations on it are the intended
	// upstream-facing header set. Only Authorization used to be forwarded,
	// silently dropping any other injected header (e.g. static-inject's
	// x-api-key). Content-Length / Content-Encoding are managed by the
	// body-rewrite block below and the transport, so leave them untouched.
	// Mirrors reverseproxy's forwarded-request header sync.
	httpx.SyncRequestHeaders(r, pctx.Headers)

	// If a WritesRequestBody plugin rewrote pctx.Body, ship the new bytes
	// upstream and clear Content-Encoding (see forwardproxy response
	// path for the rationale).
	if pctx.BodyMutated() {
		httpx.SetRequestBody(r, pctx.Body)
	}

	// The pipeline this request runs on for the rest of the handler: every
	// capability probe below (encoding, streaming, buffering) must answer
	// for it, not whatever a mid-request reload left in the Holder — the
	// path the listener picks has to match the pipeline the response
	// phases dispatch on. Nil-pinned (skipped requests never ran Run)
	// falls back to the current pipeline (Holder.ForPctx).
	pinned := s.OutboundPipeline.ForPctx(pctx)

	// Hop branch point 3: the hop always owns the encoding.
	ownEncoding := hopped || (!skipped && (pinned.NeedsResponseBody() || pinned.HasStreamingResponders()))
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
			"host", pipeline.SanitizeLogValue(host),
			"requested", pipeline.SanitizeLogValue(pctx.Host))
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
	// Hop branch point 4: after the redirect re-check, dial the gateway in place of the origin.
	var resp *http.Response
	var err error
	if hopped {
		var answered bool
		if resp, err, answered = s.hopDo(w, r, pctx, tl, pctx.OutboundSessionID); answered {
			return
		}
	} else {
		resp, err = client.Do(r)
	}
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
		resp, err = s.resendIfAsked(r, pctx, client, resp, tl, ownEncoding, capture)
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
				"host", pipeline.SanitizeLogValue(r.Host),
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
			slog.Debug("forward-proxy: client went away before the upstream answered", "host", pipeline.SanitizeLogValue(r.Host), "method", r.Method)
		} else {
			// fail.Message, not err: err's text quotes the request URL, query and all.
			slog.Warn("forward-proxy: upstream request failed", "host", pipeline.SanitizeLogValue(r.Host), "method", r.Method, "kind", fail.Kind, "error", fail.Message)
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
	//
	// The same wrapper keeps the body for the session archive's capture (#1494): every arm
	// reads through it, so what it keeps is the body as it came off the wire, whatever arm
	// relays it. Only when the request was captured, so a response never outlives its pair: the
	// request row keeps its capture only for inference (recordCapturedRequest).
	if capture != nil && pctx.Extensions.Inference != nil {
		rc := &pipeline.ResponseCapture{Status: resp.StatusCode, Header: redact.Headers(resp.Header), Decoded: resp.Uncompressed}
		pctx.ResponseCapture = rc
		resp.Body = bodycount.WrapCapture(resp.Body, &pctx.ResponseBytes, &rc.Body, &rc.Truncated, &rc.Ended, pipeline.MaxResponseCapture)
	} else {
		resp.Body = bodycount.Wrap(resp.Body, &pctx.ResponseBytes)
	}

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
		if (httpx.IsEventStream(resp.Header.Get("Content-Type")) || mislabeled) && resp.Body != nil {
			if pinned.WritesResponseBody() {
				// A response mutator needs the whole response to rewrite it, so
				// it can't stream — fall back to the buffered path with a warning.
				// A request-only mutator does NOT land here: it never touches
				// these bytes, so the relay stays incremental.
				// Once, not per response: a response mutator on an SSE chain is a
				// supported configuration (cpex, sparc), not a misconfiguration, so
				// warning every request is log spam at request rate.
				s.bufferedFallbackOnce.Do(func() {
					slog.Info("forward-proxy: text/event-stream responses will use the buffered path — a WritesResponseBody plugin is in the chain", "host", pipeline.SanitizeLogValue(r.Host))
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
					slog.Warn("forward-proxy: text/event-stream response with a ReadsBody plugin that is not a StreamingResponder — streaming byte-for-byte; its OnResponse will see an empty body (implement StreamingResponder to inspect a streamed body)", "host", pipeline.SanitizeLogValue(r.Host))
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

	copyResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	// A skipped response of unknown length — an event stream, a long poll — is relayed as
	// it arrives, as httputil.ReverseProxy does; buffered, its first bytes would wait for
	// the next 4KB. skip_hosts never sent one here, but self-traffic does: OpenCode's TUI
	// follows its service's event stream through this path.
	var dst io.Writer = w
	if f, ok := w.(http.Flusher); ok && skipped && (resp.ContentLength == -1 || httpx.IsEventStream(resp.Header.Get("Content-Type"))) {
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

// recordOutboundRequestEvent records pctx's request row under sid. Its pair is
// recordOutboundResponseEvent, which files under pctx.OutboundSessionID, so the
// caller pins that to sid first.

// hopByHopHeaders is the response-side twin of the request-side list stripForUpstream
// deletes. RFC 9110 §7.6.3: these describe one connection, not the resource, and must
// not be forwarded in either direction.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyResponseHeaders forwards resp's end-to-end headers to w, skipping the hop-by-hop
// set — including anything the response's own Connection header names as hop-by-hop for
// this connection (#1567). The four response paths used to copy resp.Header verbatim
// while requests were stripped, so an HTTP/1.1 origin's Keep-Alive reached the client;
// through the TLS bridge that lands inside an HTTP/2 HEADERS frame, which clients like
// Node's http2 reject outright.
//
// Content-Length is NOT skipped here: the buffered paths forward a body that matches it,
// and the streaming paths delete it themselves after this call.
func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	// The Connection header itself is in the skip list; its values are read first
	// because a response can demote a normally end-to-end header to hop-by-hop by
	// naming it ("Connection: X-Hop-Extra" makes X-Hop-Extra per-connection).
	named := map[string]bool{}
	for _, v := range resp.Header.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				named[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	for key, values := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] || named[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
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
func (s *Server) resendIfAsked(r *http.Request, pctx *pipeline.Context, client *http.Client, resp *http.Response, tl *tunnelLog, ownEncoding bool, capture *pipeline.RequestCapture) (*http.Response, error) {
	// Both probes and Resend itself answer for the pipeline this request
	// ran on (Holder.ForPctx): the reload-drain window must not flip the
	// resend decision onto plugins that never saw the request.
	if !s.OutboundPipeline.ForPctx(pctx).HasResenders() || !s.OutboundPipeline.ForPctx(pctx).NeedsRequestBody() {
		return resp, nil
	}
	for round := 0; round < pipeline.MaxResends && resp.StatusCode/100 != 2; round++ {
		head, peekErr := io.ReadAll(io.LimitReader(resp.Body, pipeline.ResendPeekLimit))
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
			if capture != nil && pctx.Extensions.Inference != nil {
				// The refusal as far as it was read: the resend peeks at most ResendPeekLimit bytes.
				pctx.ResponseCapture = &pipeline.ResponseCapture{Status: resp.StatusCode, Header: redact.Headers(resp.Header),
					Body: head, Decoded: resp.Uncompressed, Ended: peekErr == nil && len(head) < pipeline.ResendPeekLimit}
			}
			s.recordOutboundResponseEvent(pctx, resp.StatusCode, nil)
			pctx.StatusCode, pctx.ResponseBody, pctx.Extensions.Inference = 0, nil, resent
			pctx.RenewRequestID()
			s.recordCapturedRequest(tl, pctx, pctx.OutboundSessionID, capture)
		}
		again := r.Clone(r.Context())
		httpx.SyncRequestHeaders(again, pctx.Headers)
		httpx.SetRequestBody(again, pctx.Body)
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
			slog.Warn("forward-proxy: response body read error", "host", pipeline.SanitizeLogValue(r.Host), "kind", fail.Kind, "error", fail.Message)
		}
		http.Error(w, `{"error":"response body read error"}`, http.StatusBadGateway)
	} else {
		slog.Warn("forward-proxy: response body too large", "host", pipeline.SanitizeLogValue(r.Host), "limit", maxBodySize)
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

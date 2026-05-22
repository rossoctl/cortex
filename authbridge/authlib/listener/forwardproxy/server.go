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
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/httpx"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
	authtls "github.com/kagenti/kagenti-extensions/authbridge/authlib/tls"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

// Server is an HTTP forward proxy that performs token exchange on outbound requests.
//
// OutboundPipeline is a holder so the bound pipeline can be hot-swapped
// under the running listener; each handleRequest Loads through it so
// in-flight requests finish on the pipeline they started with.
type Server struct {
	OutboundPipeline *pipeline.Holder
	Sessions         *session.Store // nil when session tracking is disabled
	Client           *http.Client
}

// MTLSOptions configures outbound mTLS. When non-nil, the forward
// proxy's HTTP client uses a custom DialContext that:
//
//  1. opens a plain TCP connection to the destination
//  2. attempts a TLS handshake using the local SVID
//  3. on handshake success → returns the *tls.Conn
//  4. on handshake failure:
//     - Strict=false: closes, redials plain TCP, returns the plain conn
//     (with a one-line WARN log naming the destination)
//     - Strict=true: closes and returns the handshake error
//  5. successful handshake with verification failure is always a hard
//     error in both modes (destination misconfigured its identity)
type MTLSOptions struct {
	Source  spiffe.X509Source
	Strict  bool
	Metrics *authtls.Metrics
}

// NewServer creates a forward proxy server with a default HTTP client.
// When mtls is non-nil, every outbound dial first tries TLS using the
// local SVID; mode-aware fallback is described in MTLSOptions.
func NewServer(outbound *pipeline.Holder, sessions *session.Store, mtls *MTLSOptions) (*Server, error) {
	transport := &http.Transport{
		// Sane Go defaults for everything except DialContext, which we
		// customize when mTLS is on.
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if mtls != nil {
		if mtls.Source == nil {
			return nil, fmt.Errorf("forwardproxy: MTLSOptions.Source is required when mtls is non-nil")
		}
		tlsCfg, err := authtls.ClientConfig(mtls.Source)
		if err != nil {
			return nil, fmt.Errorf("forwardproxy: build client tls config: %w", err)
		}
		transport.DialContext = mtlsDialer(tlsCfg, mtls.Strict, mtls.Metrics).DialContext
	}

	return &Server{
		OutboundPipeline: outbound,
		Sessions:         sessions,
		Client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// mtlsDialer returns a dialer-shaped object whose DialContext does
// the try-TLS-fall-back-to-plain dance. We construct it once per
// Server so the *tls.Config / metrics references are stable across
// connections.
type mtlsDialFunc struct {
	plain   *net.Dialer
	tlsCfg  *cryptotls.Config
	strict  bool
	metrics *authtls.Metrics
}

func mtlsDialer(cfg *cryptotls.Config, strict bool, metrics *authtls.Metrics) *mtlsDialFunc {
	return &mtlsDialFunc{
		plain:   &net.Dialer{Timeout: 10 * time.Second},
		tlsCfg:  cfg,
		strict:  strict,
		metrics: metrics,
	}
}

func (d *mtlsDialFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	plain, err := d.plain.DialContext(ctx, network, addr)
	if err != nil {
		// TCP failure — never retry as TLS, and never count as a TLS
		// fallback. Connection refused / DNS failure is a different
		// kind of bug from "destination doesn't speak TLS."
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

		// Strict mode: handshake failure is terminal. Don't fall back.
		if d.strict {
			if d.metrics != nil {
				d.metrics.OutboundFailed.Add(1)
			}
			return nil, fmt.Errorf("forwardproxy mtls strict: handshake to %s failed: %w", addr, err)
		}

		// Permissive mode: redial plain TCP and return that. Surface
		// the destination so operators can spot persistent fallbacks
		// in their logs and either install authbridge on the target
		// or accept the cost. The mode attribute makes the log easy
		// to grep across mixed-mode rollouts where some sidecars are
		// permissive and others are strict.
		if d.metrics != nil {
			d.metrics.OutboundFellBack.Add(1)
		}
		slog.Warn("mtls fallback to plaintext",
			"mode", "permissive", "addr", addr, "reason", err.Error())
		return d.plain.DialContext(ctx, network, addr)
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

	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Scheme:    r.URL.Scheme,
		Host:      r.Host,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
		StartedAt: time.Now(),
	}

	// Finisher dispatch runs after every exit path. RunFinish is a
	// no-op when pctx.dispatched is empty (pre-pipeline rejects), so
	// this defer is safe on every path including the body-too-large
	// early return.
	defer func() {
		s.OutboundPipeline.RunFinish(r.Context(), pctx, pipeline.OutcomeFromContext(pctx))
	}()

	if s.OutboundPipeline.NeedsBody() && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Warn("forward-proxy: request body too large or unreadable", "host", r.Host, "error", err)
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		pctx.Body = body
		slog.Debug("forward-proxy: buffered request body", "host", r.Host, "bodyLen", len(body))
	}

	if s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.OutboundPipeline.Run(r.Context(), pctx)

	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		httpx.WriteRejection(w, action)
		return
	}

	if s.Sessions != nil {
		sid := s.Sessions.ActiveSession()
		if sid == "" {
			sid = session.DefaultSessionID
		}
		// Snapshot-copy the protocol extension so the request event
		// doesn't see response-phase mutations on the same MCP/Inference
		// struct (e.g. token counts assigned in OnResponse).
		plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
		ev := pipeline.SessionEvent{
			At:          time.Now(),
			Direction:   pipeline.Outbound,
			Phase:       pipeline.SessionRequest,
			MCP:         pipeline.SnapshotMCP(pctx.Extensions.MCP),
			Inference:   pipeline.SnapshotInference(pctx.Extensions.Inference),
			Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
			Plugins:     plugins,
			Identity:    pipeline.SnapshotIdentity(pctx),
			Host:        pctx.Host,
		}
		// Record whenever ANY protocol-or-plugin context is present —
		// MCP/Inference (parser-emitted), Invocations (gate plugins like
		// jwt-validation/token-exchange), or plugin-public Plugins
		// entries. Earlier the gate was just MCP||Inference; widening
		// it ensures auth-only outbound traffic and pure observability
		// events show up in abctl. Don't narrow this back without
		// understanding why each clause is necessary.
		if ev.MCP != nil || ev.Inference != nil || ev.Invocations != nil || plugins != nil {
			s.Sessions.Append(sid, ev)
		}
	}

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		r.Header.Set("Authorization", "Bearer "+auth.ExtractBearer(newAuth))
	}

	// If a WritesBody plugin rewrote pctx.Body, ship the new bytes
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

	// Clear RequestURI — set by the server but must be empty for client requests
	r.RequestURI = ""

	resp, err := s.Client.Do(r)
	if err != nil {
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Response phase: populate pctx and run plugins in reverse order.
	pctx.StatusCode = resp.StatusCode
	pctx.ResponseHeaders = resp.Header.Clone()

	if s.OutboundPipeline.NeedsBody() && resp.Body != nil {
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
		if err != nil {
			slog.Warn("forward-proxy: response body read error", "host", r.Host, "error", err)
			http.Error(w, `{"error":"response body read error"}`, http.StatusBadGateway)
			return
		}
		if len(respBody) > maxBodySize {
			slog.Warn("forward-proxy: response body too large", "host", r.Host, "len", len(respBody))
			http.Error(w, `{"error":"response body too large"}`, http.StatusBadGateway)
			return
		}
		pctx.ResponseBody = respBody
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}

	respAction := s.OutboundPipeline.RunResponse(r.Context(), pctx)
	if respAction.Type == pipeline.Reject {
		httpx.WriteRejection(w, respAction)
		return
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

	if s.Sessions != nil {
		sid := s.Sessions.ActiveSession()
		if sid == "" {
			sid = session.DefaultSessionID
		}
		plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
		ev := pipeline.SessionEvent{
			At:          time.Now(),
			Direction:   pipeline.Outbound,
			Phase:       pipeline.SessionResponse,
			MCP:         pipeline.SnapshotMCP(pctx.Extensions.MCP),
			Inference:   pipeline.SnapshotInference(pctx.Extensions.Inference),
			Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
			Plugins:     plugins,
			Identity:    pipeline.SnapshotIdentity(pctx),
			Host:        pctx.Host,
			StatusCode:  resp.StatusCode,
			Error:       pipeline.DeriveError(pctx),
			Duration:    pipeline.DurationSince(pctx.StartedAt),
		}
		// Same widened gate as the request side — see the request-phase
		// comment for why each clause matters.
		if ev.MCP != nil || ev.Inference != nil || ev.Invocations != nil || plugins != nil {
			s.Sessions.Append(sid, ev)
		}
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

// recordOutboundReject emits a SessionDenied event for outbound
// requests a pipeline plugin rejected. Symmetric to the accept path's
// session recording (above). Lets guardrail plugins (rate-limit,
// intent-based, content policy) show operators what was blocked and
// why via /v1/sessions and abctl, instead of the block appearing only
// as a 4xx/5xx on the agent side.
//
// Skips when no Invocations were appended — the deny came from a
// plugin that didn't contribute diagnostic context, and a content-free
// SessionDenied event would be noise without attribution.
func (s *Server) recordOutboundReject(pctx *pipeline.Context, action pipeline.Action) {
	if s.Sessions == nil || pctx.Extensions.Invocations == nil {
		return
	}
	sid := s.Sessions.ActiveSession()
	if sid == "" {
		sid = session.DefaultSessionID
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
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Host:        pctx.Host,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
	}
	s.Sessions.Append(sid, ev)
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
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Scheme:    "tcp", // marker: bytes are opaque, not HTTP
		Host:      r.Host,
		Path:      "",
		Headers:   r.Header.Clone(),
		StartedAt: time.Now(),
	}
	defer func() {
		s.OutboundPipeline.RunFinish(r.Context(), pctx, pipeline.OutcomeFromContext(pctx))
	}()

	if s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	// Run the outbound pipeline. Plugins that policy on host/identity
	// (ibac, content gates) still get to allow/deny; plugins that need
	// HTTP body (parsers) see no body, which they handle gracefully.
	action := s.OutboundPipeline.Run(r.Context(), pctx)
	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		httpx.WriteRejection(w, action)
		return
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

	// Plain TCP dial. See package-level comment on why mTLS doesn't
	// apply here. r.Host on a CONNECT carries "host:port" already.
	upstream, err := net.DialTimeout("tcp", r.Host, connectDialTimeout)
	if err != nil {
		slog.Warn("forward-proxy: CONNECT upstream dial failed", "host", r.Host, "error", err)
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
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

	// Record a SessionRequest event so /v1/sessions and abctl show that
	// a tunnel was opened. Mirrors the HTTP path's post-Allow recording
	// (see handleRequest above). The MCP / Inference snapshots are nil
	// by definition (CONNECT bytes are opaque), but Invocations from
	// gate plugins (ibac, token-exchange's skip/no_route, etc.) and
	// any plugin-public Plugins entries are still meaningful.
	if s.Sessions != nil {
		sid := s.Sessions.ActiveSession()
		if sid == "" {
			sid = session.DefaultSessionID
		}
		plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
		ev := pipeline.SessionEvent{
			At:          time.Now(),
			Direction:   pipeline.Outbound,
			Phase:       pipeline.SessionRequest,
			Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
			Plugins:     plugins,
			Identity:    pipeline.SnapshotIdentity(pctx),
			Host:        pctx.Host,
		}
		if ev.Invocations != nil || plugins != nil {
			s.Sessions.Append(sid, ev)
		}
	}

	// Bidirectional copy. When either side closes, propagate the close
	// to the other so both io.Copy goroutines exit. Close-on-each-side
	// is idempotent on net.Conn.
	go func() {
		_, _ = io.Copy(upstream, clientConn)
		_ = upstream.Close()
		_ = clientConn.Close()
	}()
	_, _ = io.Copy(clientConn, upstream)
	_ = clientConn.Close()
	_ = upstream.Close()
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

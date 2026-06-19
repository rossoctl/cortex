// Package forwardproxy implements an HTTP forward proxy listener.
// Agents set HTTP_PROXY to route outbound traffic through this proxy
// for transparent token exchange.
package forwardproxy

import (
	"bufio"
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
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/httpx"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/internal/sseframe"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/skiphost"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
	authtls "github.com/kagenti/kagenti-extensions/authbridge/authlib/tls"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/tlsbridge"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

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
	// See authlib/config/config.go ListenerConfig.SkipHosts for
	// motivation.
	SkipHosts *skiphost.Matcher

	TLSBridge *tlsbridge.Engine // nil = disabled; set by caller after NewServer
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
	s.serveOutbound(w, r, false)
}

// serveOutbound runs the outbound pipeline for one decrypted/plaintext request
// and re-originates it. isBridge=true marks requests produced by TLS bridging:
// they are origin-form (the caller sets r.URL.Scheme/Host) and must re-originate
// via the dedicated upstream client, never the mesh-mTLS s.Client.
func (s *Server) serveOutbound(w http.ResponseWriter, r *http.Request, isBridge bool) {
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

	if !skipped && s.OutboundPipeline.NeedsBody() && r.Body != nil {
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

	if !skipped && s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	originalAuth := pctx.Headers.Get("Authorization")
	if !skipped {
		action := s.OutboundPipeline.Run(r.Context(), pctx)

		if action.Type == pipeline.Reject {
			s.recordOutboundReject(pctx, action)
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
		// Record EVERY message that reaches the pipeline — even when no
		// plugin acted and no parser matched (Invocations/MCP/Inference all
		// nil). The session API is an observability surface; a request the
		// pipeline saw but no plugin touched is still a network message the
		// operator wants to see (it carries Host, and the paired response
		// carries StatusCode). skip_hosts traffic never reaches here (the
		// !skipped guard above), so it stays suppressed by design.
		s.Sessions.Append(sid, ev)
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

	client := s.Client
	if isBridge && s.TLSBridge != nil {
		client = s.TLSBridge.Upstream
	}
	resp, err := client.Do(r)
	if err != nil {
		http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
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
		// declares WritesBody (mutating a body we've already started
		// forwarding is incompatible with streaming) — fall back to
		// buffered with a warning log instead.
		if isEventStream(resp.Header.Get("Content-Type")) &&
			s.OutboundPipeline.HasStreamingResponders() &&
			resp.Body != nil {
			if s.OutboundPipeline.WritesBody() {
				slog.Warn("forward-proxy: text/event-stream response with WritesBody plugin — falling back to buffered path", "host", r.Host)
			} else {
				s.handleStreamingResponse(w, r, resp, pctx)
				return
			}
		}

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

		// Streaming-aware plugins use a single code path for both shapes:
		// for the buffered application/json case we deliver the whole body
		// as one last=true frame so plugins finalize their running state.
		// Plugins that didn't migrate — i.e. don't implement
		// StreamingResponder — are unaffected (RunResponseFrame skips them).
		if s.OutboundPipeline.HasStreamingResponders() && resp.Body != nil {
			respFrameAction := s.OutboundPipeline.RunResponseFrame(r.Context(), pctx, pctx.ResponseBody, true)
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

		s.recordOutboundResponseEvent(pctx, resp.StatusCode)
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

// bridgeServe attempts to bridge: verify the upstream origin first (reversibility),
// then forge a leaf + terminate the agent TLS + run the UNCHANGED pipeline via
// serveOutbound. authority is host:port (used to dial+verify upstream and to set
// r.URL.Host); host is the skip/log key. Returns true if it consumed the connection
// (success OR an unrecoverable post-forge failure that was logged); false to fall
// back to a plain tunnel — so no working call is ever broken.
func (s *Server) bridgeServe(client net.Conn, authority, host string) bool {
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
		return false
	}
	resp, err := s.TLSBridge.Upstream.Do(req)
	if err != nil {
		slog.Info("tls-bridge passthrough", "host", host, "reason", "upstream-verify", "error", err)
		return false // fall back to plain tunnel — agent's own e2e TLS still reaches origin
	}
	_ = resp.Body.Close()

	// 2) Forge + terminate downstream.
	tconn, err := s.TLSBridge.Term.Terminate(client, hostOnly(authority))
	if err != nil {
		s.TLSBridge.Skip.Add(host) // pinned client → its retry will passthrough
		slog.Warn("tls-bridge passthrough", "host", host, "reason", "handshake-fail", "error", err)
		return true // conn is dead post-forge; nothing left to tunnel
	}

	// 3) Serve the decrypted conn through the UNCHANGED pipeline.
	tlsbridge.ServeConn(tconn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Scheme = "https"
		r.URL.Host = authority // host:port — preserves non-443 origins
		s.serveOutbound(w, r, true)
	}))
	return true
}

// recordOutboundResponseEvent emits the SessionResponse event for a
// completed outbound response. Extracted from handleRequest so the
// streaming path can call it once at end-of-stream and the buffered
// path can call it once after RunResponse — both go through the same
// gate and snapshotting logic.
func (s *Server) recordOutboundResponseEvent(pctx *pipeline.Context, statusCode int) {
	if s.Sessions == nil {
		return
	}
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
		StatusCode:  statusCode,
		Error:       pipeline.DeriveError(pctx),
		Duration:    pipeline.DurationSince(pctx.StartedAt),
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
	// SessionResponse row to abctl.
	defer func() {
		finalAction := s.OutboundPipeline.RunResponseFrame(r.Context(), pctx, nil, true)
		if finalAction.Type == pipeline.Reject {
			// Headers already sent; we can't promote to 502, but
			// surface the policy violation so operators see it.
			slog.Warn("forward-proxy: streaming response rejected on finalization (headers already sent)",
				"host", r.Host, "violation", finalAction.Violation)
		}
		s.recordOutboundResponseEvent(pctx, resp.StatusCode)
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

	respAction := s.OutboundPipeline.RunResponse(r.Context(), pctx)
	if respAction.Type == pipeline.Reject {
		httpx.WriteRejection(w, respAction)
		return
	}
	if s.OutboundPipeline.HasStreamingResponders() {
		// Re-parse the buffered SSE body frame-by-frame so plugins see the
		// same per-event shape as the real streaming path. A Reject is
		// honored here — headers are not yet on the wire.
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
			frameAction := s.OutboundPipeline.RunResponseFrame(r.Context(), pctx, frame, false)
			if frameAction.Type == pipeline.Reject {
				httpx.WriteRejection(w, frameAction)
				return
			}
		}
		finalAction := s.OutboundPipeline.RunResponseFrame(r.Context(), pctx, nil, true)
		if finalAction.Type == pipeline.Reject {
			httpx.WriteRejection(w, finalAction)
			return
		}
	}
	s.recordOutboundResponseEvent(pctx, resp.StatusCode)
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
		Method:    r.Method, // always "CONNECT" here, but populated for parity with handleRequest
		Scheme:    "tcp",    // marker: bytes are opaque, not HTTP
		Host:      r.Host,
		Path:      "",
		Headers:   r.Header.Clone(),
		StartedAt: time.Now(),
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

	if !skipped {
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
			// Render as a JSON-RPC error frame when the rejected
			// request was MCP JSON-RPC, so the agent's MCP client
			// surfaces this as one failed tool call rather than a
			// transport break. Falls through to plain HTTP-level
			// rejection for non-MCP traffic.
			httpx.WriteRejectionForRequest(w, action, pctx)
			return
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
	// (see handleRequest above). Shared with the transparent-redirect path.
	// Skipped when the destination matched SkipHosts: no plugin ran, so
	// there are no Invocations to attribute the event to.
	if !skipped {
		s.recordTunnelOpened(pctx)
	}

	if s.TLSBridge != nil {
		pc := &peekedConn{Conn: clientConn, r: bufio.NewReaderSize(clientConn, sniffBufSize)}
		clientConn = pc // replay peeked bytes into whichever path runs
		first, _ := pc.Peek(5)
		authority := r.Host // CONNECT target is already host:port
		key := hostOnly(r.Host)
		if !s.TLSBridge.Skip.Contains(key) {
			if v, _ := s.TLSBridge.Decision.Classify(key, portOf(r.Host), first); v == tlsbridge.Terminate {
				_ = upstream.Close() // bridgeServe dials its own verified upstream
				if s.bridgeServe(clientConn, authority, key) {
					return
				}
				// fell open → re-dial for the tunnel
				if up2, derr := net.DialTimeout("tcp", r.Host, connectDialTimeout); derr == nil {
					tunnel(clientConn, up2)
					_ = up2.Close()
				}
				return
			}
		}
	}

	// Bidirectional copy until either side closes.
	tunnel(clientConn, upstream)
}

// writeSSEFrame writes one SSE event built from a sseframe-decoded
// frame back to w. The decoder folds multi-line `data:` events with
// `\n` separators; this helper splits on those `\n`s and emits one
// `data: <line>\n` per original line followed by the blank-line
// terminator, so a downstream SSE parser sees the same event
// boundaries the upstream produced. Returns true when every byte
// was written; false on any write error so the caller can stop
// forwarding without re-checking each Write.
func writeSSEFrame(w io.Writer, frame []byte) bool {
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

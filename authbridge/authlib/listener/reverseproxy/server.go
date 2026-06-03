// Package reverseproxy implements an HTTP reverse proxy listener.
// Inbound requests are validated via the inbound pipeline before being
// forwarded to a fixed backend.
package reverseproxy

import (
	"bytes"
	"context"
	cryptotls "crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/httpx"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/internal/tlssniff"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/spiffe"
	authtls "github.com/kagenti/kagenti-extensions/authbridge/authlib/tls"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

type pctxKey struct{}

// responseRejectedError carries a pipeline Reject from the roundTripper
// back to the error handler, where it's rendered into the
// http.ResponseWriter. The embedded action keeps Violation.Render() and
// helper constructors available at the render site.
type responseRejectedError struct {
	action pipeline.Action
}

func (e *responseRejectedError) Error() string {
	if e.action.Violation != nil {
		return e.action.Violation.Reason
	}
	return "response rejected"
}

// Server is an HTTP reverse proxy with inbound JWT validation.
//
// InboundPipeline is a holder so the bound pipeline can be hot-swapped
// under the running listener; each handleRequest Loads through it so
// in-flight requests finish on the pipeline they started with.
type Server struct {
	InboundPipeline *pipeline.Holder
	Sessions        *session.Store       // nil when session tracking is disabled
	Shared          pipeline.SharedStore // process-scoped store; set by main, may be nil
	proxy           *httputil.ReverseProxy
	backend         string

	// mtlsCfg is the *tls.Config wrapping the local SVID for inbound
	// mTLS, or nil when mTLS is disabled. mtlsMode is consulted by
	// the byte-peek listener (Listen) to decide whether non-TLS
	// connections are passed through (permissive) or closed (strict).
	mtlsCfg     *cryptotls.Config
	mtlsMode    tlssniff.Mode
	mtlsMetrics *authtls.Metrics
}

// MTLSOptions configures inbound mTLS. Pass nil (or a zero-value
// MTLSOptions with Source nil) to construct a server with TLS off.
type MTLSOptions struct {
	// Source supplies the local SVID + trust bundle. Required when
	// MTLSOptions is non-nil; the constructor errors otherwise.
	Source spiffe.X509Source

	// Strict: when true, the listener rejects non-TLS callers. When
	// false (default), it accepts both TLS and plaintext on the same
	// port via byte-peek detection.
	Strict bool

	// Metrics, when non-nil, receives counter increments on TLS
	// accept / plaintext-accept / plaintext-reject paths. The caller
	// owns the *Metrics and exposes its Snapshot via /stats.
	Metrics *authtls.Metrics
}

// NewServer creates a reverse proxy that forwards to the given backend URL.
// When mtls is non-nil, the listener returned by Listen wraps the inbound
// connection in TLS sniffing using the provided X.509 source.
func NewServer(inbound *pipeline.Holder, sessions *session.Store, backendURL string, mtls *MTLSOptions) (*Server, error) {
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	s := &Server{
		InboundPipeline: inbound,
		Sessions:        sessions,
		proxy:           proxy,
		backend:         backendURL,
	}
	if mtls != nil {
		if mtls.Source == nil {
			return nil, fmt.Errorf("reverseproxy: MTLSOptions.Source is required when mtls is non-nil")
		}
		tlsCfg, err := authtls.ServerConfig(mtls.Source)
		if err != nil {
			return nil, fmt.Errorf("reverseproxy: build server tls config: %w", err)
		}
		s.mtlsCfg = tlsCfg
		s.mtlsMode = tlssniff.ModePermissive
		if mtls.Strict {
			s.mtlsMode = tlssniff.ModeStrict
		}
		s.mtlsMetrics = mtls.Metrics
	}
	proxy.ModifyResponse = s.modifyResponse
	proxy.ErrorHandler = s.errorHandler
	return s, nil
}

// Listen returns a net.Listener bound to addr. When mTLS is configured
// the listener is a tlssniff.Listener that dispatches TLS handshakes
// through the local SVID and pass-throughs plain HTTP per the
// configured mode (permissive / strict). When mTLS is disabled the
// returned listener is a plain net.Listen("tcp", addr).
//
// Callers pass the result to http.Server.Serve.
func (s *Server) Listen(addr string) (net.Listener, error) {
	inner, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if s.mtlsCfg == nil {
		return inner, nil
	}
	sniff := tlssniff.New(inner, s.mtlsCfg, s.mtlsMode)
	if s.mtlsMetrics != nil {
		sniff.SetOnPlainRejected(func(_ net.Conn) {
			s.mtlsMetrics.InboundPlainRejected.Add(1)
		})
	}
	return sniff, nil
}

// MTLSEnabled reports whether the listener is wrapping connections
// in TLS-sniffing. Used by the bin's startup-log path to surface a
// clear message about the listener mode.
func (s *Server) MTLSEnabled() bool { return s.mtlsCfg != nil }

// eventTLS builds a *pipeline.EventTLS from the pctx's connection
// state, extracting the peer SPIFFE ID via authlib/tls. Returns nil
// for plaintext or absent TLS state — sites that pass the result
// through to a SessionEvent get the right thing for any caller.
func eventTLS(pctx *pipeline.Context) *pipeline.EventTLS {
	if pctx == nil || pctx.TLS == nil {
		return nil
	}
	return pipeline.NewEventTLS(pctx.TLS, authtls.PeerSPIFFEID(pctx.PeerCertificate()))
}

// Handler returns the HTTP handler for the reverse proxy.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleRequest)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    r.Method,
		Scheme:    requestScheme(r),
		Host:      r.Host,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}

	// Surface connection-level identity to plugins that opt in. r.TLS is
	// non-nil only when the connection went through TLS — for plain HTTP
	// callers (UI, healthchecks), pctx.TLS stays nil and any plugin
	// reading it sees the absence cleanly.
	if r.TLS != nil {
		pctx.TLS = r.TLS
		if s.mtlsMetrics != nil && len(r.TLS.PeerCertificates) > 0 {
			s.mtlsMetrics.InboundTLSAccepted.Add(1)
		}
	} else if s.mtlsMetrics != nil {
		s.mtlsMetrics.InboundPlainAccepted.Add(1)
	}

	// Finisher dispatch runs after every exit path from this handler —
	// allowed requests, plugin denials, upstream errors. RunFinish is
	// a no-op when pctx.dispatched is empty (e.g. body-too-large
	// rejected before Run), so this defer is safe on the pre-pipeline
	// error paths too.
	defer func() {
		s.InboundPipeline.RunFinish(r.Context(), pctx, pipeline.OutcomeFromContext(pctx))
	}()

	if s.InboundPipeline.NeedsBody() && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.Warn("reverse-proxy: request body too large or unreadable", "host", r.Host, "error", err)
			http.Error(w, `{"error":"request body too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		pctx.Body = body
		slog.Debug("reverse-proxy: buffered request body", "host", r.Host, "bodyLen", len(body))
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.InboundPipeline.Run(r.Context(), pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		httpx.WriteRejection(w, action)
		return
	}

	// If a WritesBody plugin rewrote pctx.Body, send the new bytes to
	// the backend and clear Content-Encoding (same rationale as the
	// response path — plugin may have decompressed).
	if pctx.BodyMutated() {
		r.Body = io.NopCloser(bytes.NewReader(pctx.Body))
		r.ContentLength = int64(len(pctx.Body))
		r.Header.Set("Content-Length", fmt.Sprintf("%d", len(pctx.Body)))
		r.Header.Del("Content-Encoding")
	}

	// Propagate an inbound Authorization mutation to the forwarded
	// request. A plugin (e.g. jwt-validation in mint mode) may have
	// replaced the caller's token on pctx.Headers; the proxy forwards
	// r.Header, so without this the backend would still see the original
	// token. Only rewrite when the value actually changed.
	if newAuth := pctx.Headers.Get("Authorization"); newAuth != originalAuth {
		r.Header.Set("Authorization", newAuth)
	}

	// Record the inbound request event whenever there is something
	// observable: an A2A conversation, plugin invocations, or plugin-public
	// Custom entries. Mirrors extproc.recordInboundSession's widened gate so
	// observability does not depend on the a2a-parser being in the pipeline
	// (e.g. a jwt-validation allow on an auth-only agent must still show, just
	// as denials already do via recordInboundReject). The A2A-specific session
	// rekey in modifyResponse stays A2A-gated.
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	if s.Sessions != nil && (pctx.Extensions.A2A != nil || pctx.Extensions.Invocations != nil || plugins != nil) {
		sid := inboundSessionID(pctx)
		// Snapshot-copy the protocol extension and use the shared helpers
		// for plugin invocations / observability / identity. Mirrors what
		// extproc does so request events don't pick up response-phase
		// mutations on the same pctx.Extensions.A2A struct.
		s.Sessions.Append(sid, pipeline.SessionEvent{
			At:          time.Now(),
			Direction:   pipeline.Inbound,
			Phase:       pipeline.SessionRequest,
			A2A:         pipeline.SnapshotA2A(pctx.Extensions.A2A),
			Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
			Plugins:     plugins,
			Identity:    pipeline.SnapshotIdentity(pctx),
			Host:        pctx.Host,
			TLS:         eventTLS(pctx),
		})
	}

	r = r.WithContext(context.WithValue(r.Context(), pctxKey{}, pctx))
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) modifyResponse(resp *http.Response) error {
	pctx, _ := resp.Request.Context().Value(pctxKey{}).(*pipeline.Context)
	if pctx == nil {
		return nil
	}

	pctx.StatusCode = resp.StatusCode
	pctx.ResponseHeaders = resp.Header.Clone()

	if s.InboundPipeline.NeedsBody() && resp.Body != nil {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
		if err != nil {
			return err
		}
		resp.Body.Close()
		if len(body) > maxBodySize {
			return fmt.Errorf("response body too large (%d bytes)", len(body))
		}
		pctx.ResponseBody = body
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}

	action := s.InboundPipeline.RunResponse(resp.Request.Context(), pctx)
	if action.Type == pipeline.Reject {
		return &responseRejectedError{action: action}
	}

	// A plugin that called pctx.SetResponseBody flipped the mutation flag.
	// Use the replaced bytes and rewrite Content-Length so the downstream
	// client gets a consistent response. Content-Encoding is cleared —
	// see the same comment in forwardproxy for the rationale.
	if pctx.ResponseBodyMutated() {
		resp.Body = io.NopCloser(bytes.NewReader(pctx.ResponseBody))
		resp.ContentLength = int64(len(pctx.ResponseBody))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(pctx.ResponseBody)))
		resp.Header.Del("Content-Encoding")
	}

	// Rekey the default bucket → A2A contextId when the response
	// reveals one. The first turn of an A2A conversation arrives
	// without a contextId (the agent assigns it on response), so the
	// inbound request + any outbound MCP/inference calls during
	// processing land in `default`. Without rekey those events stay
	// orphaned while only the response goes to the contextId bucket.
	// Mirrors extproc.rekeyInboundSession.
	//
	// Skip when SessionID is empty (auth-only or non-A2A response —
	// no contextId to merge against) or already "default" (a no-op
	// that would also collide with the source bucket name).
	if s.Sessions != nil && pctx.Extensions.A2A != nil &&
		pctx.Extensions.A2A.SessionID != "" &&
		pctx.Extensions.A2A.SessionID != session.DefaultSessionID {
		s.Sessions.Rekey(session.DefaultSessionID, pctx.Extensions.A2A.SessionID)
	}

	// Mirror forwardproxy's response-phase event so abctl pairs every
	// inbound request with a response row. Without this, A2A
	// `message/stream` requests show up as orphan request events.
	// SSE responses still get recorded — the body is whatever the
	// pipeline saw at this point (may be empty for streamed bodies),
	// but the status code and plugin invocations are always meaningful.
	respPlugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	if s.Sessions != nil && (pctx.Extensions.A2A != nil || pctx.Extensions.Invocations != nil || respPlugins != nil) {
		sid := inboundSessionID(pctx)
		s.Sessions.Append(sid, pipeline.SessionEvent{
			At:          time.Now(),
			Direction:   pipeline.Inbound,
			Phase:       pipeline.SessionResponse,
			A2A:         pipeline.SnapshotA2A(pctx.Extensions.A2A),
			Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
			Plugins:     respPlugins,
			Identity:    pipeline.SnapshotIdentity(pctx),
			Host:        pctx.Host,
			StatusCode:  resp.StatusCode,
			Error:       pipeline.DeriveError(pctx),
			Duration:    pipeline.DurationSince(pctx.StartedAt),
			TLS:         eventTLS(pctx),
		})
	}
	return nil
}

func (s *Server) errorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	if rErr, ok := err.(*responseRejectedError); ok {
		httpx.WriteRejection(w, rErr.action)
		return
	}
	http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
}

// recordInboundReject emits a SessionDenied event for inbound requests
// a pipeline plugin rejected. Lets gate plugins (jwt-validation and
// future inbound guardrails) show operators what was blocked and why
// via /v1/sessions and abctl, instead of the block appearing only as
// a 401/403 on the caller side.
//
// Skips when no Invocations were appended — the deny came from a
// plugin that didn't contribute diagnostic context, and a content-free
// SessionDenied event would be noise without attribution.
func (s *Server) recordInboundReject(pctx *pipeline.Context, action pipeline.Action) {
	if s.Sessions == nil || pctx.Extensions.Invocations == nil {
		return
	}
	// Inbound uses the A2A-stated contextId when available; otherwise
	// the default bucket. Same rule as the accept path's
	// inboundSessionID helper, kept consistent so denial events land
	// in the same bucket the accepted request would have.
	sid := inboundSessionID(pctx)
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
		Direction:   pipeline.Inbound,
		Phase:       pipeline.SessionDenied,
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Host:        pctx.Host,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		TLS: eventTLS(pctx),
	}
	s.Sessions.Append(sid, ev)
}

// requestScheme derives the URL scheme for an incoming server-side
// request. On server requests Go does not populate r.URL.Scheme (it's
// only set for client-side / proxy requests where the full URL is on
// the request line), so we read it from r.TLS instead: TLS present =>
// https, absent => http.
//
// Contract note: this listener intentionally diverges from the
// Context.Scheme godoc's "empty when undetermined" convention — it
// always returns "http" or "https" based on r.TLS. The fallback is
// confidently wrong when reverseproxy sits behind a TLS-terminating
// upstream (LB, ingress): r.TLS is nil on the inner hop even though
// the caller's actual scheme was https. Consumers that need the
// caller's scheme in that topology should plumb X-Forwarded-Proto
// once a trusted-upstream policy exists (not in this PR).
//
// Does not consult X-Forwarded-Proto. Honoring that header is only
// safe when the upstream proxy is trusted; wiring a trust policy is
// deferred until we have a concrete multi-hop deployment story.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// inboundSessionID returns the bucket ID for an inbound event. Mirrors
// extproc's inboundSessionID: trusts the A2A-stated contextId when
// non-empty, otherwise routes to DefaultSessionID. Does NOT fall back
// to ActiveSession() — that fallback was a cross-conversation
// contamination vector (a new conversation's first turn would inherit
// the previous conversation's rekeyed bucket, stranding the current
// turn's events in the prior bucket and creating an orphan one-event
// session for the response). Rekey on response migrates the Default
// bucket into the contextId once the agent reveals it.
func inboundSessionID(pctx *pipeline.Context) string {
	if pctx.Extensions.A2A != nil && pctx.Extensions.A2A.SessionID != "" {
		return pctx.Extensions.A2A.SessionID
	}
	return session.DefaultSessionID
}

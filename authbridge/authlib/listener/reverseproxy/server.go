// Package reverseproxy implements an HTTP reverse proxy listener.
// Inbound requests are validated via the inbound pipeline before being
// forwarded to a fixed backend.
package reverseproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
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
	Sessions        *session.Store // nil when session tracking is disabled
	proxy           *httputil.ReverseProxy
	backend         string
}

// NewServer creates a reverse proxy that forwards to the given backend URL.
func NewServer(inbound *pipeline.Holder, sessions *session.Store, backendURL string) (*Server, error) {
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
	proxy.ModifyResponse = s.modifyResponse
	proxy.ErrorHandler = s.errorHandler
	return s, nil
}

// Handler returns the HTTP handler for the reverse proxy.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleRequest)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Scheme:    requestScheme(r),
		Host:      r.Host,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
		StartedAt: time.Now(),
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

	action := s.InboundPipeline.Run(r.Context(), pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		writeRejection(w, action)
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

	if s.Sessions != nil && pctx.Extensions.A2A != nil {
		sid := pctx.Extensions.A2A.SessionID
		if sid == "" {
			sid = s.Sessions.ActiveSession()
		}
		if sid == "" {
			sid = session.DefaultSessionID
		}
		s.Sessions.Append(sid, pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Inbound,
			Phase:     pipeline.SessionRequest,
			A2A:       pctx.Extensions.A2A,
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
	return nil
}

func (s *Server) errorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	if rErr, ok := err.(*responseRejectedError); ok {
		writeRejection(w, rErr.action)
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
	// falls through to the default bucket. Matches the accept path's
	// bucketing rule (A2A request event at line 112-125).
	sid := ""
	if pctx.Extensions.A2A != nil {
		sid = pctx.Extensions.A2A.SessionID
	}
	if sid == "" {
		sid = s.Sessions.ActiveSession()
	}
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
		Direction:   pipeline.Inbound,
		Phase:       pipeline.SessionDenied,
		Invocations: pctx.Extensions.Invocations.FilteredByPhase(pipeline.InvocationPhaseRequest),
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

// writeRejection renders a pipeline Reject to the http.ResponseWriter,
// preserving the plugin's status, headers, and body.
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

func writeRejection(w http.ResponseWriter, action pipeline.Action) {
	status, headers, body := action.Violation.Render()
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

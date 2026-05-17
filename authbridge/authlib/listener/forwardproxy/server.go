// Package forwardproxy implements an HTTP forward proxy listener.
// Agents set HTTP_PROXY to route outbound traffic through this proxy
// for transparent token exchange.
package forwardproxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/httpx"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
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

// NewServer creates a forward proxy server with a default HTTP client.
func NewServer(outbound *pipeline.Holder, sessions *session.Store) *Server {
	return &Server{
		OutboundPipeline: outbound,
		Sessions:         sessions,
		Client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Handler returns the HTTP handler for the forward proxy.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handleRequest)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, `{"error":"HTTPS CONNECT not supported — only HTTP proxy"}`, http.StatusMethodNotAllowed)
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



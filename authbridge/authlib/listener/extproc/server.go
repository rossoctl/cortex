// Package extproc implements an Envoy ext_proc gRPC streaming listener.
// It translates ext_proc ProcessingRequests into pipeline runs and maps
// the results back to ProcessingResponses.
package extproc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/auth"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/httpx"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/internal/sseframe"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/listener/skiphost"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/session"
)

const maxBodySize = 1 << 20 // 1MB — matches Envoy's default per_stream_buffer_limit_bytes

// Server implements the Envoy ext_proc ExternalProcessor gRPC service.
//
// InboundPipeline / OutboundPipeline are holders so the bound pipeline
// can be hot-swapped under the running listener; each Process stream
// Loads through the holder, so in-flight requests finish on the pipeline
// they started with.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer
	InboundPipeline  *pipeline.Holder
	OutboundPipeline *pipeline.Holder
	Sessions         *session.Store       // nil when session tracking is disabled
	Shared           pipeline.SharedStore // process-scoped store; set by main, may be nil

	// SkipHosts, when non-nil and matching pctx.Host on an outbound
	// request, causes the listener to return passResponse() / nil pctx
	// immediately — bypassing the pipeline AND session recording for
	// that request. Forward the bytes; do nothing else. See
	// authlib/config/config.go ListenerConfig.SkipHosts for the
	// motivating case (OTel-collector traffic evicting the inbound
	// A2A intent from the session buffer's FIFO window).
	SkipHosts *skiphost.Matcher
}

// Process handles the bidirectional ext_proc stream.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()

	// pendingHeaders/pendingDirection hold state between RequestHeaders and
	// RequestBody phases. Envoy guarantees sequential message ordering per
	// stream: RequestBody always follows its RequestHeaders, and each stream
	// is a single request — no interleaving or stale state is possible.
	var pendingHeaders *corev3.HeaderMap
	var pendingDirection string

	// pctx and requestDirection survive from the request phase to the response
	// phase so that RunResponse can see the full request+response context.
	var pctx *pipeline.Context
	var requestDirection string

	// Finisher dispatch runs once when Process returns — stream end is
	// Envoy's signal that the request is finalized (response sent or
	// abandoned). A stream that never reached Run (no RequestHeaders
	// ever arrived) leaves pctx nil, in which case we have no chain
	// to finish on; skip.
	defer func() {
		if pctx == nil {
			return
		}
		p := s.OutboundPipeline
		if requestDirection == "inbound" {
			p = s.InboundPipeline
		}
		p.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := stream.Recv()
		if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		var resp *extprocv3.ProcessingResponse

		switch r := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			headers := r.RequestHeaders.Headers
			direction := getHeader(headers, "x-authbridge-direction")

			p := s.OutboundPipeline
			if direction == "inbound" {
				p = s.InboundPipeline
			}

			if p.NeedsBody() && requestHasBody(headers) {
				slog.Debug("ext_proc: requesting body from Envoy", "direction", direction)
				pendingHeaders = headers
				pendingDirection = direction
				resp = requestBodyResponse()
			} else if direction == "inbound" {
				resp, pctx = s.handleInbound(stream, headers, nil)
				requestDirection = direction
			} else {
				resp, pctx = s.handleOutbound(stream, headers, nil)
				requestDirection = direction
			}

		case *extprocv3.ProcessingRequest_RequestBody:
			body := r.RequestBody.Body
			slog.Debug("ext_proc: received request body", "direction", pendingDirection, "bodyLen", len(body))
			if len(body) > maxBodySize {
				slog.Warn("ext_proc: request body too large", "direction", pendingDirection, "bodyLen", len(body))
				resp = immediateResponse(http.StatusRequestEntityTooLarge, "request body too large")
			} else if pendingDirection == "inbound" {
				resp, pctx = s.handleInboundBody(stream, pendingHeaders, body)
				requestDirection = pendingDirection
			} else {
				resp, pctx = s.handleOutboundBody(stream, pendingHeaders, body)
				requestDirection = pendingDirection
			}
			pendingHeaders = nil
			pendingDirection = ""

		case *extprocv3.ProcessingRequest_ResponseHeaders:
			resp = s.handleResponseHeaders(ctx, r.ResponseHeaders.Headers, pctx, requestDirection)

		case *extprocv3.ProcessingRequest_ResponseBody:
			resp = s.handleResponseBody(ctx, r.ResponseBody.Body, pctx, requestDirection)

		default:
			resp = &extprocv3.ProcessingResponse{}
		}

		if err := stream.Send(resp); err != nil {
			return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
		}
	}
}

func (s *Server) handleInbound(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    getHeader(headers, ":method"),
		Scheme:    getHeader(headers, ":scheme"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		s.InboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)
	if newAuth := pctx.Headers.Get("Authorization"); newAuth != originalAuth {
		return replaceTokenResponse(auth.ExtractBearer(newAuth)), pctx
	}
	return allowResponse(), pctx
}

func (s *Server) handleInboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    getHeader(headers, ":method"),
		Scheme:    getHeader(headers, ":scheme"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		s.InboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)
	if newAuth := pctx.Headers.Get("Authorization"); newAuth != originalAuth {
		return withBodyMutation(replaceTokenBodyResponse(auth.ExtractBearer(newAuth)), pctx), pctx
	}
	return withBodyMutation(allowBodyResponse(), pctx), pctx
}

// inboundSessionID returns the bucket ID for an inbound event. Trusts the
// client's stated contextId (pctx.Extensions.A2A.SessionID) as authoritative
// and bootstraps to DefaultSessionID when empty. Does NOT fall back to
// ActiveSession() — that fallback was a cross-conversation contamination
// vector: a new conversation's first turn (empty SessionID) would inherit
// the previous conversation's rekeyed bucket, stranding the current turn's
// request events in the prior bucket and creating an orphan 1-event session
// for the response.
//
// Auth-only events (no A2A parser match — e.g. a rejected request that
// never reached the parser) route to DefaultSessionID. This is where
// operators will look for unauthorized-access events in abctl.
func inboundSessionID(pctx *pipeline.Context) string {
	if pctx.Extensions.A2A != nil && pctx.Extensions.A2A.SessionID != "" {
		return pctx.Extensions.A2A.SessionID
	}
	return session.DefaultSessionID
}

func (s *Server) recordInboundSession(pctx *pipeline.Context) {
	if s.Sessions == nil {
		return
	}
	// Widened gate (was: A2A == nil). Any of A2A / Auth / plugin-public
	// Custom entries qualify. Keeps traffic with no protocol parser but
	// meaningful auth state visible in the session stream.
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	if pctx.Extensions.A2A == nil && pctx.Extensions.Invocations == nil && plugins == nil {
		return
	}
	sid := inboundSessionID(pctx)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Inbound,
		Phase:       pipeline.SessionRequest,
		A2A:         pipeline.SnapshotA2A(pctx.Extensions.A2A),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
	}
	s.Sessions.Append(sid, ev)
}

// recordInboundReject emits a SessionDenied event for requests a pipeline
// plugin rejected. Called from the Reject path BEFORE rejectFromAction
// returns, so denied requests appear in the session stream rather than
// silently vanishing (which was the pre-Auth-extension behavior — denials
// only surfaced via /stats counters, invisible to abctl). Fires only when
// at least one plugin populated Auth — otherwise we wouldn't have
// diagnostic context worth recording and would just be logging an HTTP
// status.
func (s *Server) recordInboundReject(pctx *pipeline.Context, action pipeline.Action) {
	if s.Sessions == nil || pctx.Extensions.Invocations == nil {
		return
	}
	var status int
	var code, message string
	if action.Violation != nil {
		// Use the structured fields directly — Render() produces the HTTP
		// wire payload (status, headers, JSON body) which is the wrong
		// shape for a session event. We want the semantic Code + Reason.
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
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Duration: pipeline.DurationSince(pctx.StartedAt),
	}
	s.Sessions.Append(inboundSessionID(pctx), ev)
}

// recordOutboundReject emits a SessionDenied event for outbound requests
// a pipeline plugin rejected. Symmetric to recordInboundReject on the
// inbound side. Called BEFORE rejectFromAction returns, so denied
// outbound calls appear in /v1/sessions and abctl rather than vanishing
// with only a 4xx/5xx on the agent side — the observability surface
// that guardrail plugins (rate-limit, policy, intent-based) depend on
// to show operators what they blocked and why.
//
// Uses the same ActiveSession bucketing as recordOutboundSession: an
// outbound call inherits the most-recently-updated session. When no
// active session exists the event lands in DefaultSessionID. Matches
// the correctness envelope of the accept path.
//
// Skips recording when no Invocations were appended — the deny came
// from a plugin that didn't contribute diagnostic context, and a
// content-free SessionDenied event would be noise without attribution.
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
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Duration: pipeline.DurationSince(pctx.StartedAt),
	}
	s.Sessions.Append(sid, ev)
}

// recordInboundResponseSession appends a Phase:SessionResponse event for the
// inbound direction. Called after RunResponse completes so the event carries
// the updated SessionID (from the response body's contextId, when an A2A
// parser ran) or the default bucket (when the pipeline is auth-only).
//
// Recording gate parallels the request-phase gate in recordInboundSession
// and the outbound-response gate in recordOutboundResponseSession: A2A,
// Auth, or plugin-public Custom entries all qualify. The earlier gate that
// required A2A silently dropped response events for auth-only pipelines
// (jwt-validation without any parser) — the request phase recorded, the
// response phase didn't, so operators saw one-sided conversations in abctl.
func (s *Server) recordInboundResponseSession(pctx *pipeline.Context) {
	if s.Sessions == nil {
		return
	}
	plugins := pipeline.SnapshotPlugins(pctx.Extensions.Custom)
	if pctx.Extensions.A2A == nil && pctx.Extensions.Invocations == nil && plugins == nil {
		return
	}
	sid := inboundSessionID(pctx)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Inbound,
		Phase:       pipeline.SessionResponse,
		A2A:         pipeline.SnapshotA2A(pctx.Extensions.A2A),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		StatusCode:  pctx.StatusCode,
		Error:       pipeline.DeriveError(pctx),
		Host:        pctx.Host,
		Duration:    pipeline.DurationSince(pctx.StartedAt),
	}
	s.Sessions.Append(sid, ev)
}

// recordOutboundResponseSession appends a Phase:SessionResponse event for the
// outbound direction, carrying whichever protocol extension the response
// populated (MCP tool result, inference completion + token counts).
func (s *Server) recordOutboundResponseSession(pctx *pipeline.Context) {
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
		StatusCode:  pctx.StatusCode,
		Error:       pipeline.DeriveError(pctx),
		Host:        pctx.Host,
		Duration:    pipeline.DurationSince(pctx.StartedAt),
	}
	// Auth / Plugins alone qualify for recording; matches the widened
	// gate in recordInboundSession so outbound denials and plugin-public
	// observability aren't dropped just because the response carried no
	// MCP/Inference payload.
	if ev.MCP != nil || ev.Inference != nil || ev.Invocations != nil || plugins != nil {
		s.Sessions.Append(sid, ev)
	}
}

// rekeyInboundSession renames the DefaultSessionID bucket to the
// server-assigned A2A contextId when the response reveals one, so events
// from the first turn (recorded under "default" during the request phase)
// merge with subsequent turns that carry the real contextId.
func (s *Server) rekeyInboundSession(pctx *pipeline.Context, direction string) {
	if direction != "inbound" || s.Sessions == nil || pctx.Extensions.A2A == nil {
		return
	}
	sid := pctx.Extensions.A2A.SessionID
	if sid == "" || sid == session.DefaultSessionID {
		return
	}
	s.Sessions.Rekey(session.DefaultSessionID, sid)
}

func (s *Server) recordOutboundSession(pctx *pipeline.Context) {
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
		Phase:       pipeline.SessionRequest,
		MCP:         pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:   pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
	}
	if ev.MCP != nil || ev.Inference != nil || ev.Invocations != nil || plugins != nil {
		s.Sessions.Append(sid, ev)
	}
}

func (s *Server) handleOutbound(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    getHeader(headers, ":method"),
		Scheme:    getHeader(headers, ":scheme"),
		Host:      getHeader(headers, ":authority"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	if pctx.Host == "" {
		pctx.Host = getHeader(headers, "host")
	}

	// SkipHosts short-circuit: forward the request as a transparent
	// proxy without running the pipeline or recording a session event.
	// pctx=nil signals the response handlers (handleResponseHeaders,
	// handleResponseBody) and the deferred RunFinish to no-op as well —
	// all four phases are skipped consistently. See ListenerConfig.SkipHosts.
	if s.SkipHosts.Match(pctx.Host) {
		return passResponse(), nil
	}

	if s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		s.OutboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromActionForRequest(action, pctx), nil
	}

	s.recordOutboundSession(pctx)

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return replaceTokenResponse(auth.ExtractBearer(newAuth)), pctx
	}
	return passResponse(), pctx
}

func (s *Server) handleOutboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    getHeader(headers, ":method"),
		Scheme:    getHeader(headers, ":scheme"),
		Host:      getHeader(headers, ":authority"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	if pctx.Host == "" {
		pctx.Host = getHeader(headers, "host")
	}

	// SkipHosts short-circuit: see handleOutbound for rationale. The
	// body-phase entry point needs the same gate because Envoy may
	// deliver the body in a separate ProcessingRequest message even
	// when the headers were already passed through — without checking
	// here, a skip-listed host whose request carries a body would still
	// run the pipeline on the body phase.
	if pat, matched := s.SkipHosts.MatchPattern(pctx.Host); matched {
		slog.Info("ext_proc: skip_hosts match (body phase) — bypassing pipeline + session recording",
			"host", pctx.Host, "pattern", pat, "path", pctx.Path)
		return allowBodyResponse(), nil
	}

	if s.Sessions != nil {
		if aid := s.Sessions.ActiveSession(); aid != "" {
			pctx.Session = s.Sessions.View(aid)
		}
	}

	originalAuth := pctx.Headers.Get("Authorization")
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		s.OutboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromActionForRequest(action, pctx), nil
	}

	s.recordOutboundSession(pctx)

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return withBodyMutation(replaceTokenBodyResponse(auth.ExtractBearer(newAuth)), pctx), pctx
	}
	return withBodyMutation(passBodyResponse(), pctx), pctx
}

func (s *Server) handleResponseHeaders(ctx context.Context, headers *corev3.HeaderMap, pctx *pipeline.Context, direction string) *extprocv3.ProcessingResponse {
	if pctx == nil {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extprocv3.HeadersResponse{},
			},
		}
	}

	statusStr := getHeader(headers, ":status")
	pctx.StatusCode, _ = strconv.Atoi(statusStr)
	pctx.ResponseHeaders = headerMapToHTTP(headers)

	p := s.OutboundPipeline
	if direction == "inbound" {
		p = s.InboundPipeline
	}

	if p.NeedsBody() {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseHeaders{
				ResponseHeaders: &extprocv3.HeadersResponse{},
			},
			ModeOverride: &extprocfilterv3.ProcessingMode{
				ResponseBodyMode: extprocfilterv3.ProcessingMode_BUFFERED,
			},
		}
	}

	action := p.RunResponse(ctx, pctx)
	if action.Type == pipeline.Reject {
		return rejectFromAction(action)
	}

	// Body-less response: deliver an empty last=true frame so
	// StreamingResponder plugins can finalize (and emit no_response_body
	// Skip rows for pairing). Mirrors the buffered-body path's single
	// last=true dispatch.
	if p.HasStreamingResponders() {
		if frameAction := p.RunResponseFrame(ctx, pctx, nil, true); frameAction.Type == pipeline.Reject {
			return rejectFromAction(frameAction)
		}
	}

	// No body phase will run; record the response event here. A2A responses
	// need the body to extract contextId, so the rekey path is body-only;
	// skip it on this header-only path.
	if direction == "inbound" {
		s.recordInboundResponseSession(pctx)
	} else {
		s.recordOutboundResponseSession(pctx)
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{},
		},
	}
}

func (s *Server) handleResponseBody(ctx context.Context, body []byte, pctx *pipeline.Context, direction string) *extprocv3.ProcessingResponse {
	if pctx == nil {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseBody{
				ResponseBody: &extprocv3.BodyResponse{},
			},
		}
	}

	pctx.ResponseBody = body

	p := s.OutboundPipeline
	if direction == "inbound" {
		p = s.InboundPipeline
	}

	action := p.RunResponse(ctx, pctx)
	if action.Type == pipeline.Reject {
		return rejectFromAction(action)
	}

	// Streaming-aware plugins use a single code path for both shapes
	// (mirrors forwardproxy/reverseproxy). pipeline.RunResponse skips
	// StreamingResponder plugins so they wouldn't get a response-phase
	// dispatch otherwise; deliver the buffered body via RunResponseFrame
	// so mcp/inference/a2a parsers populate their response state and
	// the inbound A2A contextId rekey below sees pctx.Extensions.A2A
	// fully populated. For text/event-stream bodies (Envoy already
	// buffered them at this point), re-parse with sseframe so each
	// event arrives as its own frame; otherwise dispatch the whole
	// body as one last=true frame.
	if p.HasStreamingResponders() {
		if frameAction := dispatchBufferedFrames(ctx, p, pctx); frameAction.Type == pipeline.Reject {
			return rejectFromAction(frameAction)
		}
	}

	// The server's response may carry the server-assigned A2A contextId. If
	// the request phase recorded events under DefaultSessionID (because the
	// client had no contextId yet), migrate them to the real ID so subsequent
	// turns — which will send that contextId — accumulate into one session.
	// Rekey first so the response event we're about to append lands under
	// the real contextId rather than being orphaned in "default".
	s.rekeyInboundSession(pctx, direction)

	if direction == "inbound" {
		s.recordInboundResponseSession(pctx)
	} else {
		s.recordOutboundResponseSession(pctx)
	}

	// A plugin that declared WritesBody: true and called pctx.SetResponseBody
	// flips the ResponseBodyMutated flag. Emit the replacement bytes via
	// BodyMutation so Envoy rewrites the downstream response; otherwise
	// pass through with no mutation. The flag avoids the O(n) string
	// compare the old path did on every response, and lets a no-op rewrite
	// (bytes unchanged but intent was to redact-nothing) still route
	// through the mutation path if a future test needs to observe it.
	if pctx.ResponseBodyMutated() {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseBody{
				ResponseBody: &extprocv3.BodyResponse{
					Response: &extprocv3.CommonResponse{
						BodyMutation: &extprocv3.BodyMutation{
							Mutation: &extprocv3.BodyMutation_Body{
								Body: pctx.ResponseBody,
							},
						},
					},
				},
			},
		}
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocv3.BodyResponse{},
		},
	}
}

func headerMapToHTTP(headers *corev3.HeaderMap) http.Header {
	h := make(http.Header)
	if headers != nil {
		for _, hdr := range headers.Headers {
			h.Set(hdr.Key, string(hdr.RawValue))
		}
	}
	return h
}

func requestBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{},
		},
		ModeOverride: &extprocfilterv3.ProcessingMode{
			RequestBodyMode: extprocfilterv3.ProcessingMode_BUFFERED,
		},
	}
}

func allowResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						RemoveHeaders: []string{"x-authbridge-direction"},
					},
				},
			},
		},
	}
}

func passResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{},
		},
	}
}

func passBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{},
		},
	}
}

// withBodyMutation optionally decorates a RequestBody ProcessingResponse
// with an ext_proc BodyMutation when the pipeline rewrote pctx.Body.
// Envoy replaces the buffered body with the new bytes and recomputes
// Content-Length for the upstream. We also clear content-encoding
// because the plugin may have decompressed + rewritten in plaintext;
// shipping plain bytes without the old encoding header is safer than
// shipping a malformed archive.
//
// No-op when pctx.BodyMutated() is false — the common case of a
// read-only pipeline pays no cost beyond the bool read.
func withBodyMutation(resp *extprocv3.ProcessingResponse, pctx *pipeline.Context) *extprocv3.ProcessingResponse {
	if !pctx.BodyMutated() {
		return resp
	}
	br, ok := resp.Response.(*extprocv3.ProcessingResponse_RequestBody)
	if !ok || br.RequestBody == nil {
		return resp // response is an ImmediateResponse or shaped differently; leave alone.
	}
	if br.RequestBody.Response == nil {
		br.RequestBody.Response = &extprocv3.CommonResponse{}
	}
	cr := br.RequestBody.Response
	cr.BodyMutation = &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_Body{Body: pctx.Body},
	}
	if cr.HeaderMutation == nil {
		cr.HeaderMutation = &extprocv3.HeaderMutation{}
	}
	cr.HeaderMutation.RemoveHeaders = append(cr.HeaderMutation.RemoveHeaders, "content-encoding")
	return resp
}

func allowBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						RemoveHeaders: []string{"x-authbridge-direction"},
					},
				},
			},
		},
	}
}

func replaceTokenBodyResponse(token string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						SetHeaders: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key:      "authorization",
									RawValue: []byte("Bearer " + token),
								},
							},
						},
						// Strip the internal direction header before forwarding,
						// matching allowResponse/allowBodyResponse — otherwise
						// Envoy leaks x-authbridge-direction to the agent/target.
						RemoveHeaders: []string{"x-authbridge-direction"},
					},
				},
			},
		},
	}
}

func replaceTokenResponse(token string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						SetHeaders: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key:      "authorization",
									RawValue: []byte("Bearer " + token),
								},
							},
						},
						// Strip the internal direction header before forwarding,
						// matching allowResponse/allowBodyResponse — otherwise
						// Envoy leaks x-authbridge-direction to the agent/target.
						RemoveHeaders: []string{"x-authbridge-direction"},
					},
				},
			},
		},
	}
}

// rejectFromActionForRequest is the MCP-aware sibling of rejectFromAction.
// When pctx carries an MCP JSON-RPC request shape (Method + non-nil RPCID),
// the response is an HTTP 200 carrying a JSON-RPC 2.0 error frame so the
// caller's MCP client surfaces this as one failed tool call rather than a
// transport break. All other shapes fall through to rejectFromAction.
func rejectFromActionForRequest(action pipeline.Action, pctx *pipeline.Context) *extprocv3.ProcessingResponse {
	if pctx != nil && pctx.Extensions.MCP != nil &&
		pctx.Extensions.MCP.Method != "" && pctx.Extensions.MCP.RPCID != nil {
		body := httpx.MarshalMCPRejectionBody(action, pctx.Extensions.MCP.RPCID)
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extprocv3.ImmediateResponse{
					Status: &typev3.HttpStatus{Code: typev3.StatusCode(http.StatusOK)},
					Body:   body,
					Headers: &extprocv3.HeaderMutation{SetHeaders: []*corev3.HeaderValueOption{{
						Header: &corev3.HeaderValue{Key: "content-type", RawValue: []byte("application/json")},
					}}},
				},
			},
		}
	}
	return rejectFromAction(action)
}

// rejectFromAction turns a pipeline Reject into an Envoy ImmediateResponse,
// preserving the plugin's status/headers/body. Replaces the old
// denyResponse helper which hardcoded {"error":...,"message":...} at each
// call site.
func rejectFromAction(action pipeline.Action) *extprocv3.ProcessingResponse {
	status, headers, body := action.Violation.Render()
	immediate := &extprocv3.ImmediateResponse{
		Status: &typev3.HttpStatus{Code: typev3.StatusCode(status)},
		Body:   body,
	}
	if len(headers) > 0 {
		setHeaders := make([]*corev3.HeaderValueOption, 0, len(headers))
		for k, vs := range headers {
			for _, v := range vs {
				setHeaders = append(setHeaders, &corev3.HeaderValueOption{
					Header: &corev3.HeaderValue{Key: k, RawValue: []byte(v)},
				})
			}
		}
		immediate.Headers = &extprocv3.HeaderMutation{SetHeaders: setHeaders}
	}
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{ImmediateResponse: immediate},
	}
}

func immediateResponse(httpStatus int, reason string) *extprocv3.ProcessingResponse {
	body, _ := json.Marshal(map[string]string{"error": reason})
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status: &typev3.HttpStatus{Code: typev3.StatusCode(httpStatus)},
				Body:   body,
			},
		},
	}
}

func requestHasBody(headers *corev3.HeaderMap) bool {
	method := getHeader(headers, ":method")
	if method == "GET" || method == "HEAD" || method == "OPTIONS" || method == "DELETE" {
		return false
	}
	cl := getHeader(headers, "content-length")
	if cl != "" && cl != "0" {
		return true
	}
	te := getHeader(headers, "transfer-encoding")
	return te != ""
}

func getHeader(headers *corev3.HeaderMap, key string) string {
	if headers == nil {
		return ""
	}
	for _, h := range headers.Headers {
		if strings.EqualFold(h.Key, key) {
			return string(h.RawValue)
		}
	}
	return ""
}

// dispatchBufferedFrames feeds the buffered response body to
// StreamingResponder plugins via RunResponseFrame, mirroring the
// proxy listeners' single-dispatch contract for buffered bodies.
// Envoy's ext_proc delivers response bodies pre-buffered (we requested
// ResponseBodyMode_BUFFERED via ModeOverride), so we get the whole
// body in one shot regardless of upstream framing.
//
// For application/json the entire body is one last=true frame, so
// non-streaming JSON-RPC responses look the same to plugins as on
// the proxy listeners. For text/event-stream we re-parse with
// sseframe so each event arrives as its own non-last frame followed
// by a final last=true — matches the per-message dispatch shape
// streaming-aware plugins expect.
func dispatchBufferedFrames(ctx context.Context, p *pipeline.Holder, pctx *pipeline.Context) pipeline.Action {
	contentType := pctx.ResponseHeaders.Get("Content-Type")
	if isEventStream(contentType) && len(pctx.ResponseBody) > 0 {
		reader := sseframe.NewReader(bytes.NewReader(pctx.ResponseBody), maxBodySize)
		for {
			frame, err := reader.ReadFrame()
			if err == io.EOF {
				break
			}
			if err != nil {
				slog.Warn("extproc: SSE re-parse error", "error", err)
				break
			}
			if action := p.RunResponseFrame(ctx, pctx, frame, false); action.Type == pipeline.Reject {
				return action
			}
		}
		return p.RunResponseFrame(ctx, pctx, nil, true)
	}
	return p.RunResponseFrame(ctx, pctx, pctx.ResponseBody, true)
}

// isEventStream reports whether a Content-Type header value names the
// SSE media type. Tolerates parameters and ASCII case differences.
// Mirrors the helpers in forwardproxy/reverseproxy.
func isEventStream(contentType string) bool {
	if contentType == "" {
		return false
	}
	if idx := strings.IndexByte(contentType, ';'); idx >= 0 {
		contentType = contentType[:idx]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), "text/event-stream")
}

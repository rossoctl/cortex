// Package extproc implements an Envoy ext_proc gRPC streaming listener.
// It translates ext_proc ProcessingRequests into pipeline runs and maps
// the results back to ProcessingResponses.
package extproc

import (
	"context"
	"encoding/json"
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
	Sessions         *session.Store // nil when session tracking is disabled
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
		Scheme:    getHeader(headers, ":scheme"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		StartedAt: time.Now(),
	}

	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)
	return allowResponse(), pctx
}

func (s *Server) handleInboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Scheme:    getHeader(headers, ":scheme"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		StartedAt: time.Now(),
	}

	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)
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
	plugins := snapshotPlugins(pctx.Extensions.Custom)
	if pctx.Extensions.A2A == nil && pctx.Extensions.Invocations == nil && plugins == nil {
		return
	}
	sid := inboundSessionID(pctx)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Inbound,
		Phase:       pipeline.SessionRequest,
		A2A:         snapshotA2A(pctx.Extensions.A2A),
		Invocations: snapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     plugins,
		Identity:    snapshotIdentity(pctx),
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
		Invocations: snapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     snapshotPlugins(pctx.Extensions.Custom),
		Identity:    snapshotIdentity(pctx),
		Host:        pctx.Host,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Duration: durationSince(pctx.StartedAt),
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
		Invocations: snapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     snapshotPlugins(pctx.Extensions.Custom),
		Identity:    snapshotIdentity(pctx),
		Host:        pctx.Host,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Duration: durationSince(pctx.StartedAt),
	}
	s.Sessions.Append(sid, ev)
}

// snapshotInvocations returns a shallow copy of the Invocations extension
// filtered by phase. Plugins append to pctx.Extensions.Invocations as
// both OnRequest and OnResponse fire; the full list lives there for
// cross-phase inspection. At record time each SessionEvent should carry
// only the invocations from its own phase, so request events don't
// double-report request-phase entries AFTER the response phase has
// already added its own. Request events pass InvocationPhaseRequest,
// response events pass InvocationPhaseResponse, denied events pass
// InvocationPhaseRequest (denial terminates the pass before response
// runs). Returns nil when no matching entry exists, so the recording
// gate can check for "no invocations on this phase" cleanly.
//
// Thin wrapper over (*Invocations).FilteredByPhase — kept as a named
// function so recordXxxSession call sites stay grep-friendly.
func snapshotInvocations(ext *pipeline.Invocations, phase pipeline.InvocationPhase) *pipeline.Invocations {
	return ext.FilteredByPhase(phase)
}

// snapshotPlugins collects plugin-public observability events from
// pctx.Extensions.Custom entries whose keys end in PluginEventSuffix.
// Each matching value is json.Marshaled into the wire-form map under
// the plugin name (suffix stripped). Marshal errors downgrade to slog
// Debug and skip the entry rather than aborting recording — that keeps
// a misbehaving plugin from taking out the whole session stream.
func snapshotPlugins(custom map[string]any) map[string]json.RawMessage {
	if len(custom) == 0 {
		return nil
	}
	var out map[string]json.RawMessage
	for k, v := range custom {
		if !strings.HasSuffix(k, pipeline.PluginEventSuffix) {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			slog.Debug("session: skipping non-marshalable plugin event",
				"key", k, "error", err)
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage)
		}
		pluginName := strings.TrimSuffix(k, pipeline.PluginEventSuffix)
		out[pluginName] = raw
	}
	return out
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
	plugins := snapshotPlugins(pctx.Extensions.Custom)
	if pctx.Extensions.A2A == nil && pctx.Extensions.Invocations == nil && plugins == nil {
		return
	}
	sid := inboundSessionID(pctx)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Inbound,
		Phase:       pipeline.SessionResponse,
		A2A:         snapshotA2A(pctx.Extensions.A2A),
		Invocations: snapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:     plugins,
		Identity:    snapshotIdentity(pctx),
		StatusCode:  pctx.StatusCode,
		Error:       deriveError(pctx),
		Host:        pctx.Host,
		Duration:    durationSince(pctx.StartedAt),
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
	plugins := snapshotPlugins(pctx.Extensions.Custom)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionResponse,
		MCP:         snapshotMCP(pctx.Extensions.MCP),
		Inference:   snapshotInference(pctx.Extensions.Inference),
		Invocations: snapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:     plugins,
		Identity:    snapshotIdentity(pctx),
		StatusCode:  pctx.StatusCode,
		Error:       deriveError(pctx),
		Host:        pctx.Host,
		Duration:    durationSince(pctx.StartedAt),
	}
	// Auth / Plugins alone qualify for recording; matches the widened
	// gate in recordInboundSession so outbound denials and plugin-public
	// observability aren't dropped just because the response carried no
	// MCP/Inference payload.
	if ev.MCP != nil || ev.Inference != nil || ev.Invocations != nil || plugins != nil {
		s.Sessions.Append(sid, ev)
	}
}

// snapshotIdentity copies Claims + Agent identity off pctx so the session event
// stays valid after pctx is discarded. Returns nil when no identity information
// is available (e.g., jwt-validation didn't run on this path).
func snapshotIdentity(pctx *pipeline.Context) *pipeline.EventIdentity {
	// Read identity via the pipeline.Identity interface. Whichever auth
	// plugin ran (jwt-validation today; SAML / mTLS / custom later)
	// published an adapter onto pctx.Identity — we don't need to know
	// its concrete type to snapshot the subject/client/scopes.
	if pctx.Identity == nil && pctx.Agent == nil {
		return nil
	}
	id := &pipeline.EventIdentity{}
	if pctx.Identity != nil {
		id.Subject = pctx.Identity.Subject()
		id.ClientID = pctx.Identity.ClientID()
		if scopes := pctx.Identity.Scopes(); len(scopes) > 0 {
			id.Scopes = append([]string(nil), scopes...)
		}
	}
	if pctx.Agent != nil {
		id.AgentID = pctx.Agent.WorkloadID
	}
	return id
}

// durationSince returns the elapsed time since StartedAt, or 0 when StartedAt
// is zero (pctx constructed without wall-clock stamping, e.g. in unit tests).
func durationSince(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

// deriveError constructs an EventError from response-side signals. Returns nil
// for 2xx / no guardrail block / no parser error.
func deriveError(pctx *pipeline.Context) *pipeline.EventError {
	if pctx.Extensions.Security != nil && pctx.Extensions.Security.Blocked {
		return &pipeline.EventError{
			Kind:    "blocked",
			Message: pctx.Extensions.Security.BlockReason,
		}
	}
	if pctx.StatusCode >= 400 {
		return &pipeline.EventError{
			Kind: "backend_error",
			Code: strconv.Itoa(pctx.StatusCode),
		}
	}
	return nil
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
	plugins := snapshotPlugins(pctx.Extensions.Custom)
	ev := pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionRequest,
		MCP:         snapshotMCP(pctx.Extensions.MCP),
		Inference:   snapshotInference(pctx.Extensions.Inference),
		Invocations: snapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     plugins,
		Identity:    snapshotIdentity(pctx),
		Host:        pctx.Host,
	}
	if ev.MCP != nil || ev.Inference != nil || ev.Invocations != nil || plugins != nil {
		s.Sessions.Append(sid, ev)
	}
}

// snapshotA2A returns a shallow copy of ext. The record helpers attach
// the snapshot to the SessionEvent rather than the live pointer so
// response-phase mutations on pctx.Extensions.A2A (e.g. the parser
// stamping the server-assigned contextId onto SessionID during OnResponse)
// don't retroactively rewrite request-phase events that were already
// appended. Slice fields are reused intentionally — they are only
// assigned, never mutated in place, after the parser completes.
func snapshotA2A(ext *pipeline.A2AExtension) *pipeline.A2AExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

// snapshotMCP returns a shallow copy of ext. Important for outbound
// request events: the same pctx.Extensions.MCP pointer receives Result
// or Err on the response side, so without snapshotting, the
// already-recorded request event would display the future response's
// result map.
func snapshotMCP(ext *pipeline.MCPExtension) *pipeline.MCPExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

// snapshotInference returns a shallow copy of ext. Scalar response
// fields (Completion, FinishReason, *Tokens) get assigned on the live
// extension during OnResponse; without snapshotting, the request event's
// view would contain the eventual response's token counts and completion.
func snapshotInference(ext *pipeline.InferenceExtension) *pipeline.InferenceExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

func (s *Server) handleOutbound(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Scheme:    getHeader(headers, ":scheme"),
		Host:      getHeader(headers, ":authority"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		StartedAt: time.Now(),
	}
	if pctx.Host == "" {
		pctx.Host = getHeader(headers, "host")
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
		return rejectFromAction(action), nil
	}

	s.recordOutboundSession(pctx)

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return replaceTokenResponse(extractBearer(newAuth)), pctx
	}
	return passResponse(), pctx
}

func (s *Server) handleOutboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Scheme:    getHeader(headers, ":scheme"),
		Host:      getHeader(headers, ":authority"),
		Path:      getHeader(headers, ":path"),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		StartedAt: time.Now(),
	}
	if pctx.Host == "" {
		pctx.Host = getHeader(headers, "host")
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
		return rejectFromAction(action), nil
	}

	s.recordOutboundSession(pctx)

	newAuth := pctx.Headers.Get("Authorization")
	if newAuth != originalAuth {
		return withBodyMutation(replaceTokenBodyResponse(extractBearer(newAuth)), pctx), pctx
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

func extractBearer(authHeader string) string {
	if len(authHeader) > 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return authHeader[7:]
	}
	return ""
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
					},
				},
			},
		},
	}
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

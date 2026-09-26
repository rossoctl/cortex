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
	"slices"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocfilterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/sseframe"
	"github.com/rossoctl/cortex/core/listener/skiphost"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
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
	// core/config/config.go ListenerConfig.SkipHosts for the
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

	// sawResponseHeaders / sawResponseBody say which response phases Envoy actually delivered,
	// which is what makes the flush below meaningful: they distinguish "the response never
	// finished" from "there was no response at all".
	//
	// BOTH, because they fail differently. A body with no end_of_stream leaves the parsers holding
	// accumulated state; a response that ended after its HEADERS never ran the response phase at
	// all — and the gateway's cost header arrived on those headers.
	var sawResponseHeaders, sawResponseBody bool

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
		// THE FALLBACK FOR A RESPONSE WHOSE LAST BODY MESSAGE NEVER SAID end_of_stream, and the
		// reason gating on that flag is safe at all: Envoy omits it when TRAILERS follow, and this
		// server asks for no trailer phase, so it can legitimately never arrive. Without this such
		// a response never finalizes and is never recorded — its cost reaching no ledger, which is
		// worse than the double-count the gate removes. Stream end is Envoy's own statement that
		// the transaction is over, so it is both the last safe point to finalize and one always
		// reached.
		//
		// A REJECTED RESPONSE IS ALREADY FINISHED. Its ImmediateResponse went back to Envoy, the
		// response never went downstream and the parsers hold nothing, so recording a
		// SessionResponse row would label a denial as an ordinary response. Request-phase rejects
		// never reach this defer — those paths return a nil pctx.
		if pctx.RejectingPlugin() != "" {
			p.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
			return
		}
		if (sawResponseHeaders || sawResponseBody) && !responseWasRecorded(pctx) {
			// ONE PER DISPATCH, the rule at every finalization site: the phase below and the terminal
			// frame each get their own budget, because they run in order and a shared one lets a slow
			// plugin in the phase spend what the settle needs. See httpx.TeardownContext.
			phaseCtx, cancelPhase := httpx.TeardownContext(ctx)
			defer cancelPhase()
			// THE RESPONSE HAS ALREADY GONE DOWNSTREAM, and saying so is what keeps a refusal
			// recorded here from becoming this request's outcome.
			//
			// The flush dispatches plugins, and a plugin can reject in a response phase. The
			// action is dropped — there is nothing left to refuse — but the REJECTION is still
			// recorded on the context, and OutcomeFromContext maps any deny to OutcomeDeny.
			// Every Finisher then reads "this request was denied" for a response Envoy delivered
			// with a 200, which inverts the rule this defer states two blocks up: a rejected
			// response is left alone precisely so a denial and an ordinary response are not
			// confused.
			//
			// MarkResponseDelivered is the pipeline-side half, so the fix holds for anything
			// that re-derives the outcome from the context rather than reading the one passed to
			// RunFinish — which is what a plugin does. The refusal stays on the record, marked
			// Late; what it no longer does is rename the request.
			pctx.MarkResponseDelivered()
			defer func() { p.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx)) }()
			// THE RESPONSE PHASE, WHICH ON THIS PATH HAS DEFINITELY NOT RUN — and that is an
			// invariant of the flush's own gate rather than something to track. Every site that runs
			// the phase also records the response (handleResponseHeaders when it does not defer,
			// handleResponseBody on end_of_stream), so !responseWasRecorded above already means "the
			// phase has not run"; and a site that ran it and then REJECTED returns through the
			// RejectingPlugin branch two blocks up. A separate mark for it was unreachable —
			// deleting both of its call sites left every test in this package green, which is the
			// evidence — and the once-per-response claim that does matter is pinned by
			// TestExtProc_TornStreamRunsTheResponsePhaseOnce and its multi-message sibling.
			//
			// Rejecting is meaningless here (the stream is gone), so the action is dropped, exactly
			// as it is for the terminal frame.
			if action := p.RunResponse(phaseCtx, pctx); action.Type == pipeline.Reject {
				// Said out loud rather than swallowed: a plugin refusing a response that Envoy has
				// already delivered is a policy decision that did not take effect, and an operator
				// reading an allow-shaped row needs to know one was attempted.
				slog.Warn("ext_proc: a plugin rejected during teardown, after the response was delivered",
					"plugin", pctx.RejectingPlugin(), "code", violationCode(action))
			}
			if p.HasStreamingResponders() {
				// DETACHED FROM THE STREAM'S CONTEXT, which is the difference between this flush
				// working and only appearing to: ctx is stream.Context(), and the case this block
				// exists for — a hangup, a filter timeout, a shutdown — is exactly when it is
				// already done. RunResponseFrame refuses a done context before calling any plugin,
				// so the terminal frame would be a no-op and the request's spend would reach no
				// aggregate, ledger or budget. RunFinish needs no wrapper: dispatchFinish detaches
				// internally.
				//
				// Terminal frame, so the parsers settle from whatever state they accumulated.
				// Rejecting here would be pointless — the response has already gone downstream —
				// so the action is deliberately dropped.
				// dispatchBufferedFrames rather than a bare nil frame: on the non-SSE arm
				// the accumulated body has never been dispatched, and a nil terminal frame
				// would finalize the parsers over a body nothing ever read. The SSE arm sends
				// only the terminal frame, since its frames went out per message.
				finalCtx, cancelFinal := httpx.TeardownContext(ctx)
				defer cancelFinal()
				dispatchTerminalFrame(finalCtx, p, pctx)
			}
			s.recordResponseSession(pctx, requestDirection)
			return
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
			// end_of_stream is carried through because it is the only thing that
			// distinguishes "a body is coming" from "this response is over" — the
			// same question requestHasBody answers for the request phase above.
			sawResponseHeaders = true
			resp = s.handleResponseHeaders(ctx, r.ResponseHeaders.Headers, pctx, requestDirection,
				r.ResponseHeaders.GetEndOfStream())

		case *extprocv3.ProcessingRequest_ResponseBody:
			sawResponseBody = true
			// end_of_stream carried through for the same reason the response-header phase
			// above carries it: it is the only thing that distinguishes the last chunk of a
			// body from a middle one, and everything that must happen exactly once per
			// response hangs off that distinction.
			resp = s.handleResponseBody(ctx, r.ResponseBody.Body, pctx, requestDirection,
				r.ResponseBody.GetEndOfStream())

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
		Path:      httpx.PathOnly(getHeader(headers, ":path")),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	// Pin the calling agent HERE, at construction, from the headers as the CLIENT sent
	// them — the same line forwardproxy's serveOutbound carries, for the same reason.
	// pctx.Headers is this listener's own copy of the wire headers and plugins write to
	// it, while ClientInfo's memo fills on first READ — which without this line is
	// recordInboundSession below, downstream of the whole pipeline. A plugin that
	// rewrote User-Agent would re-file this request's spend under a name of its
	// choosing, and this path's request, response and denial events could disagree
	// depending on which asked first. Latent while no plugin touches that header — and
	// silent on the day one does, which is why it is pinned rather than watched.
	pctx.ResolveClient()

	originalHeaders := pctx.Headers.Clone()
	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		s.InboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)
	return withHeaderMutation(allowResponse(), pctx, originalHeaders), pctx
}

func (s *Server) handleInboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Inbound,
		Method:    getHeader(headers, ":method"),
		Scheme:    getHeader(headers, ":scheme"),
		Path:      httpx.PathOnly(getHeader(headers, ":path")),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	// At construction, as in handleInbound: a second Context for the same request, built one
	// message later because a plugin wanted the body, feeding the same recorders.
	pctx.ResolveClient()

	originalHeaders := pctx.Headers.Clone()
	action := s.InboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordInboundReject(pctx, action)
		s.InboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromAction(action), nil
	}

	s.recordInboundSession(pctx)
	resp := withHeaderMutation(allowBodyResponse(), pctx, originalHeaders)
	return withBodyMutation(resp, pctx), pctx
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
		RequestID:   pctx.RequestID(),
		A2A:         pipeline.SnapshotA2A(pctx.Extensions.A2A),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		Client:      pctx.ClientInfo(),
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
		RequestID:   pctx.RequestID(),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Duration: pipeline.DurationSince(pctx.StartedAt),
		Client:   pctx.ClientInfo(),
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
		RequestID:   pctx.RequestID(),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		StatusCode:  status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Duration: pipeline.DurationSince(pctx.StartedAt),
		Client:   pctx.ClientInfo(),
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
		RequestID:   pctx.RequestID(),
		A2A:         pipeline.SnapshotA2A(pctx.Extensions.A2A),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		StatusCode:  pctx.StatusCode,
		Error:       pipeline.DeriveError(pctx),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		Duration:    pipeline.DurationSince(pctx.StartedAt),
		Client:      pctx.ClientInfo(),
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
		RequestID:   pctx.RequestID(),
		MCP:         pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:   pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:     plugins,
		Identity:    pipeline.SnapshotIdentity(pctx),
		StatusCode:  pctx.StatusCode,
		Error:       pipeline.DeriveError(pctx),
		Host:        pctx.Host,
		HTTPMethod:  pctx.Method,
		HTTPPath:    pctx.Path,
		Duration:    pipeline.DurationSince(pctx.StartedAt),
		Client:      pctx.ClientInfo(),
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
		Host:      authorityOf(headers),
		Path:      httpx.PathOnly(getHeader(headers, ":path")),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	// At construction, as in handleInbound, and BEFORE the SkipHosts gate: a pin placed after an
	// early return is one a later short-circuit can step in front of. The cost on a skipped host
	// is one header lookup.
	pctx.ResolveClient()

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

	originalHeaders := pctx.Headers.Clone()
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		s.OutboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromActionForRequest(action, pctx), nil
	}

	s.recordOutboundSession(pctx)

	return withHeaderMutation(passResponse(), pctx, originalHeaders), pctx
}

func (s *Server) handleOutboundBody(stream extprocv3.ExternalProcessor_ProcessServer, headers *corev3.HeaderMap, body []byte) (*extprocv3.ProcessingResponse, *pipeline.Context) {
	ctx := stream.Context()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Method:    getHeader(headers, ":method"),
		Scheme:    getHeader(headers, ":scheme"),
		Host:      authorityOf(headers),
		Path:      httpx.PathOnly(getHeader(headers, ":path")),
		Headers:   headerMapToHTTP(headers),
		Body:      body,
		Shared:    s.Shared,
		StartedAt: time.Now(),
	}
	// At construction, as in handleOutbound: this body-phase entry point builds its own Context
	// and reaches the same outbound recorders.
	pctx.ResolveClient()

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

	originalHeaders := pctx.Headers.Clone()
	action := s.OutboundPipeline.Run(ctx, pctx)
	if action.Type == pipeline.Reject {
		s.recordOutboundReject(pctx, action)
		s.OutboundPipeline.RunFinish(ctx, pctx, pipeline.OutcomeFromContext(pctx))
		return rejectFromActionForRequest(action, pctx), nil
	}

	s.recordOutboundSession(pctx)

	resp := withHeaderMutation(passBodyResponse(), pctx, originalHeaders)
	return withBodyMutation(resp, pctx), pctx
}

// handleResponseHeaders runs the response phase off the headers alone.
//
// endOfStream is Envoy's own statement that no body follows, and it is what
// decides whether this phase defers to the body phase or finishes the response
// here. Without it the deferral below was unconditional for every shipped
// pipeline — see the comment on that branch.
func (s *Server) handleResponseHeaders(ctx context.Context, headers *corev3.HeaderMap, pctx *pipeline.Context, direction string, endOfStream bool) *extprocv3.ProcessingResponse {
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

	// Defer to the body phase — but only when there IS one.
	//
	// AND !endOfStream IS THE FIX, not a refinement. NeedsBody() alone is unconditionally true for
	// every shipped pipeline (it is NeedsRequestBody() || NeedsResponseBody(), and
	// inference-parser's undirected ReadsBody counts toward both), so everything below this return
	// was dead code — and a response with no body at all, a 204 or an error status ended on
	// headers, reached NEITHER branch. Envoy sends no ResponseBody message for those, so nothing
	// settled a cost the gateway had already reported in a header.
	//
	// The early return itself stays: when a body IS coming, this phase must not run the pipeline,
	// because the body phase runs the whole buffered dispatch — terminal frame included — and
	// running both charges twice. requestHasBody makes the same distinction on the request side.
	if p.NeedsBody() && !endOfStream {
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
	s.recordResponseSession(pctx, direction)

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{},
		},
	}
}

// violationCode names a rejected action's code for a log line, tolerating a nil Violation —
// pipeline.Deny always sets one, but an action assembled by hand need not.
func violationCode(a pipeline.Action) string {
	if a.Violation == nil {
		return ""
	}
	return a.Violation.Code
}

// sseCarryKey holds the bytes of a STREAMED SSE body that arrived after its last complete
// event — the front half of an event whose terminator is still in the next ResponseBody
// message.
//
// Kept on the pctx rather than in a local because the two ends are in different calls: the cut
// happens in dispatchBufferedFrames and the join in handleResponseBody, one Envoy message
// apart, and Process holds no per-response state of its own.
const sseCarryKey = "extproc.sse-carry"

type sseCarry struct{ tail []byte }

// carrySSETail remembers the undispatched tail of an SSE body for the next message.
//
// COPIED, because tail points into the protobuf message Envoy's decoder owns and that message
// does not outlive this call.
//
// BOUNDED BY ONE FRAME'S WORTH, and dropped rather than grown past it: a carry only grows while
// no event terminates, so a body past this cap is not a long stream but a peer sending
// something that is not SSE at all — for which the honest outcome is to lose the fragment and
// say so, not to buffer without limit.
func carrySSETail(pctx *pipeline.Context, tail []byte) {
	if len(tail) > maxBodySize {
		slog.Warn("extproc: SSE body has no event boundary within the frame limit; the fragment is dropped",
			"limit", maxBodySize, "have", len(tail))
		tail = nil
	}
	pipeline.SetState(pctx, sseCarryKey, &sseCarry{tail: append([]byte(nil), tail...)})
}

// withCarriedSSETail returns the previous message's unfinished tail joined to this message's
// bytes, and clears the carry. Returns body itself when there is nothing to join, so the common
// case allocates nothing.
func withCarriedSSETail(pctx *pipeline.Context, body []byte) []byte {
	c := pipeline.GetState[sseCarry](pctx, sseCarryKey)
	if c == nil || len(c.tail) == 0 {
		return body
	}
	joined := make([]byte, 0, len(c.tail)+len(body))
	joined = append(joined, c.tail...)
	joined = append(joined, body...)
	c.tail = nil
	return joined
}

// lastSSEFrameBoundary returns the offset just past the last blank line in b — the end of the
// last COMPLETE event — or 0 when b holds no complete event at all.
//
// Its own scan rather than sseframe's reader, because the reader reports frames and this needs
// an OFFSET: the question is where the complete prefix ends, and a frame's payload has had its
// field prefixes and line folding removed, so nothing about it locates a byte. The terminator
// rules are SSE's own and match sseframe.readLine: LF, CR, or CRLF ends a line, and an empty
// line ends an event.
func lastSSEFrameBoundary(b []byte) int {
	end, lineStart := 0, 0
	for i := 0; i < len(b); {
		if c := b[i]; c != '\n' && c != '\r' {
			i++
			continue
		}
		next := i + 1
		if b[i] == '\r' && next < len(b) && b[next] == '\n' {
			next++
		}
		if i == lineStart {
			// An empty line: the event before it is complete through here.
			end = next
		}
		i, lineStart = next, next
	}
	return end
}

// responseRecordedKey and responseRecorded mark this request's response event as appended.
const responseRecordedKey = "extproc.response-recorded"

type responseRecorded struct{}

// responseWasRecorded reports whether the response session event has already been appended.
func responseWasRecorded(pctx *pipeline.Context) bool {
	return pipeline.GetState[responseRecorded](pctx, responseRecordedKey) != nil
}

// recordResponseSession appends the response session event, AT MOST ONCE per request.
//
// THE COUNT IS MONEY, which is why the guard is here rather than at each caller. This
// listener runs the whole buffered dispatch — terminal frame included — once per
// ResponseBody message it receives (handleResponseBody -> dispatchBufferedFrames), and it
// appended a response event at the end of each one. The cost record lives in
// pctx.Extensions.Custom and is never cleared, so SnapshotPlugins serialised the SAME
// figure into every one of those events, and the aggregator and the ledger summed them: one
// request charged N times, where N is however many body messages Envoy chose to send.
//
// Reachable only OFF the shipped configuration — buffered mode delivers one message with
// end_of_stream set — but "reachable only when Envoy is configured differently" is not a
// guarantee this process makes, and a statically configured STREAMED response body mode or
// a filter with allow_mode_override off produces it. See the ModeOverride this server asks
// for in handleResponseHeaders, which is a request and not a contract.
//
// The guard is a pctx state entry rather than a local in Process because the header-only
// path records too: a 204 records from handleResponseHeaders, and the flush at stream end
// must be able to see that it already happened.
func (s *Server) recordResponseSession(pctx *pipeline.Context, direction string) {
	if responseWasRecorded(pctx) {
		return
	}
	pipeline.SetState(pctx, responseRecordedKey, &responseRecorded{})
	if direction == "inbound" {
		s.recordInboundResponseSession(pctx)
	} else {
		s.recordOutboundResponseSession(pctx)
	}
}

func (s *Server) handleResponseBody(ctx context.Context, body []byte, pctx *pipeline.Context, direction string, endOfStream bool) *extprocv3.ProcessingResponse {
	if pctx == nil {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ResponseBody{
				ResponseBody: &extprocv3.BodyResponse{},
			},
		}
	}

	// A ResponseBody message is a chunk of a byte stream and NOT a unit of anything else: nothing
	// aligns Envoy's chunk boundaries with the body's own structure. Both arms exist because of
	// that, and differ only in how much has to be kept.
	//
	// A NON-SSE BODY IS ACCUMULATED WHOLE. Replaced per message, each fragment reached the parser
	// as a complete response — no fragment is valid JSON, so usage never landed and the settle
	// latch pinned the first empty answer.
	//
	// AN SSE BODY KEEPS ONLY WHAT IS UNFINISHED. Its frames are dispatched as each message arrives
	// (a plugin has to be able to reject mid-stream, and buffering a whole SSE body runs into the
	// truncation cap on the long turns that matter), so all that must survive a boundary is the
	// bytes after the last COMPLETE event. Without carrying them, a straddling event becomes two
	// unparseable halves — and on the Anthropic dialect the event at risk is message_delta, so
	// what goes missing is the OUTPUT tally while the response still settles at the prompt-only
	// floor.
	if isEventStream(pctx.ResponseHeaders.Get("Content-Type")) {
		// BOUNDED LIKE THE OTHER ARM. The carry itself is capped at one frame's worth, but the join
		// below is carry + THIS MESSAGE, and nothing else caps the message: the maxBodySize check in
		// Process covers the REQUEST body only. appendBoundedBody puts both under the same ceiling
		// the non-SSE arm has, warning and truncating rather than growing without limit.
		//
		// SO THE PHASE BELOW SEES ONLY THE TRAILING CHUNK HERE, and that is deliberate rather than
		// an oversight in the once-per-response change: this field holds carry + this message, never
		// the whole turn, so a non-streaming plugin reading it gets the end of the stream and not a
		// document. A whole streamed turn is precisely the body that runs into the truncation cap, and
		// the SSE interface is the FRAME dispatch — a plugin that needs every event implements
		// OnResponseFrame, which is how both cost owners read one. Pinned from the plugin's side in
		// server_lifecycle_test.go so a change in either direction is a decision, not a drift.
		//
		// AND IT IS NOT A ONE-WORD CHANGE, if anyone wants the whole turn here: appending to this
		// field instead of replacing it duplicates the straddling bytes, because the joined value
		// already carries them (measured: data: {"seq":2,"ta then data: {"seq":2,"tail":"x"}).
		pctx.ResponseBody = appendBoundedBody(nil, withCarriedSSETail(pctx, body))
	} else {
		pctx.ResponseBody = appendBoundedBody(pctx.ResponseBody, body)
	}

	p := s.OutboundPipeline
	if direction == "inbound" {
		p = s.InboundPipeline
	}

	// ONCE PER RESPONSE, ON THE LAST MESSAGE — not once per ResponseBody message.
	//
	// A statically configured STREAMED body mode delivers N messages, and this ran the response
	// phase on every one of them, each time over a PARTIAL body: opa, cpex, lineage and sparc
	// would decide N times on N prefixes of a document, and their Invocation rows would all land
	// in the recorded snapshot. It is also the same "wait for the whole body" rule the frame
	// dispatch and the session record already follow. No double CHARGE was possible — both cost owners are StreamingResponders,
	// which RunResponse skips — so what this protects is the audit trail and, on the non-SSE arm
	// above, any plugin that reads pctx.ResponseBody expecting a document. On the SSE arm that field
	// is the trailing chunk by design; the arm says why.
	//
	// AND THE FLUSH CANNOT DOUBLE IT. A stream that never says end-of-stream never reaches this
	// branch, so the flush runs the phase once itself; a stream that does reach it is RECORDED
	// here, and the flush skips a recorded response. That is why no separate "phase ran" mark is
	// needed — see the flush.
	if endOfStream {
		if action := p.RunResponse(ctx, pctx); action.Type == pipeline.Reject {
			return rejectFromAction(action)
		}
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
		if frameAction := dispatchBufferedFrames(ctx, p, pctx, endOfStream); frameAction.Type == pipeline.Reject {
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

	// ONLY ON THE LAST BODY MESSAGE. Envoy sends one per chunk in a streamed response body
	// mode, so appending a session event at the end of every one would emit N events each
	// carrying the SAME cost record — that record sits in pctx.Extensions.Custom and is never
	// cleared, so the aggregator and the ledger would sum it once per chunk. See
	// recordResponseSession, and the flush in Process for the case where end_of_stream never
	// arrives.
	if endOfStream {
		s.recordResponseSession(pctx, direction)
	}

	// A plugin that declared WritesResponseBody: true and called pctx.SetResponseBody
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
						HeaderMutation: &extprocv3.HeaderMutation{
							SetHeaders:    []*corev3.HeaderValueOption{contentLength(pctx.ResponseBody)},
							RemoveHeaders: []string{"content-encoding"},
						},
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

// withHeaderMutation emits every header mutation the request pipeline made to
// pctx.Headers — including the Authorization replacement. ext_proc forwards no header change
// it does not explicitly emit, so emitting Authorization alone silently drops every other
// injected header (e.g. static-inject's x-api-key). Symmetric to withBodyMutation, and to reverseproxy's
// forwarded-request header sync. Skipped: HTTP/2 pseudo-headers, which
// headerMapToHTTP copies into pctx.Headers and whose :authority governs routing;
// and Content-Length / Content-Encoding, managed by withBodyMutation and the
// transport.
func withHeaderMutation(resp *extprocv3.ProcessingResponse, pctx *pipeline.Context, orig http.Header) *extprocv3.ProcessingResponse {
	skip := func(k string) bool {
		return strings.HasPrefix(k, ":") ||
			strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding")
	}
	var set []*corev3.HeaderValueOption
	var del []string
	for k, vv := range pctx.Headers {
		if skip(k) || slices.Equal(orig[k], vv) {
			continue
		}
		if len(vv) == 0 {
			// pctx.Headers[k] = nil is a delete, same as Del(k). Emitting
			// an empty SetHeaders value instead would leave the outcome to
			// Envoy's keep_empty_value setting.
			del = append(del, strings.ToLower(k))
			continue
		}
		// Wire header names are lowercase; pctx.Headers keys were
		// canonicalised by headerMapToHTTP. That helper uses http.Header.Set,
		// not Add, so a header that arrived with several wire entries is
		// already collapsed to its last value in pctx.Headers — a lossiness
		// bug one layer down, not a property to rely on here. Before this PR
		// the collapse had no wire-facing consequence (mutations were never
		// emitted); now a mutated header is emitted as a single SetHeaders,
		// which overwrites every wire entry, so a multi-valued header a plugin
		// touches loses all but the last. Reachable, not theoretical: cpex's
		// applyExtensionChanges (plugins/cpex/manager_cpex.go:492) does
		// pctx.Headers.Set(k, v) for arbitrary CPEX-supplied keys, so a policy
		// naming a repeated header (X-Forwarded-For in a proxy chain) gets
		// here. The one-line root fix is Add-not-Set in headerMapToHTTP, which
		// would make pctx.Headers faithful to the wire and let the join below
		// produce the full value — a follow-up, not part of this header-
		// propagation PR.
		//
		// Multi-value join uses ",": correct per RFC 9110 for every header a
		// plugin realistically rewrites, and known-wrong only for Cookie
		// (whose separator is "; ") — no plugin rewrites Cookie today, and one
		// that does must split this out rather than discover it here.
		set = append(set, &corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{Key: strings.ToLower(k), RawValue: []byte(strings.Join(vv, ","))},
		})
	}
	for k := range orig {
		if _, ok := pctx.Headers[k]; !ok && !skip(k) {
			del = append(del, strings.ToLower(k)) // plugin removed it
		}
	}
	if len(set) == 0 && len(del) == 0 {
		return resp
	}
	var cr *extprocv3.CommonResponse
	switch r := resp.Response.(type) {
	case *extprocv3.ProcessingResponse_RequestHeaders:
		if r.RequestHeaders.Response == nil {
			r.RequestHeaders.Response = &extprocv3.CommonResponse{}
		}
		cr = r.RequestHeaders.Response
	case *extprocv3.ProcessingResponse_RequestBody:
		if r.RequestBody.Response == nil {
			r.RequestBody.Response = &extprocv3.CommonResponse{}
		}
		cr = r.RequestBody.Response
	default:
		return resp // ImmediateResponse or response-phase; nothing to forward.
	}
	if cr.HeaderMutation == nil {
		cr.HeaderMutation = &extprocv3.HeaderMutation{}
	}
	// Append, never assign: composes with allowResponse's
	// x-authbridge-direction removal.
	cr.HeaderMutation.SetHeaders = append(cr.HeaderMutation.SetHeaders, set...)
	cr.HeaderMutation.RemoveHeaders = append(cr.HeaderMutation.RemoveHeaders, del...)
	return resp
}

// authorityOf returns the request's authority: the HTTP/2 :authority
// pseudo-header, falling back to the HTTP/1 Host header. Outbound only —
// there it names the service being called (pipeline.SessionEvent.Host).
// The inbound handlers deliberately leave pctx.Host empty: the inbound
// authority is caller-controlled and pctx.Host feeds enforcement decisions
// (ibac's host-bypass skip, opa's policy input, per-host JWT audiences),
// so a spoofed Host header must not reach them. See cpex's outbound-only
// host-bypass guard for the same rule stated plugin-side.
func authorityOf(headers *corev3.HeaderMap) string {
	if a := getHeader(headers, ":authority"); a != "" {
		return a
	}
	return getHeader(headers, "host")
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
// Envoy replaces the buffered body with the new bytes but, in BUFFERED +
// SEND mode, leaves content-length to the processor (processing_mode.proto,
// BodySendMode) and rejects a mismatch. We also clear content-encoding
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
	cr.HeaderMutation.SetHeaders = append(cr.HeaderMutation.SetHeaders, contentLength(pctx.Body))
	return resp
}

// contentLength is the SetHeaders entry a body-mutation reply must carry in
// BUFFERED + SEND mode (processing_mode.proto, BodySendMode).
func contentLength(body []byte) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: "content-length", RawValue: []byte(strconv.Itoa(len(body)))}}
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

// appendBoundedBody appends src to dst, stopping at maxBodySize.
//
// This is the ONLY ceiling a response body has: Process's per-message maxBodySize check is in the
// request-body case, and both callers here are on the response path. So this bounds a single
// message AND the sum, the latter being what a STREAMED response mode can make arbitrarily large.
// Truncating rather than refusing keeps the prefix a parser may still be able to read — and says
// so, because a JSON body cut short parses as nothing and the silence would otherwise look like a
// response that carried no usage.
func appendBoundedBody(dst, src []byte) []byte {
	// COPIED, NOT ALIASED, even for the first message. Returning src hands back the protobuf
	// message's own slice, so a later append into its spare capacity would write into memory
	// Envoy's decoder owns — harmless today, because that message is discarded after this
	// call, and not a property worth resting on. An oversized first message falls through to
	// the room logic below, which truncates it to the cap and warns; that path is reachable
	// rather than defence in depth, because nothing upstream of it bounds a response message.
	if len(dst) == 0 && len(src) <= maxBodySize {
		return append([]byte(nil), src...)
	}
	room := maxBodySize - len(dst)
	if room <= 0 {
		slog.Warn("extproc: response body past the buffer limit; the rest is dropped",
			"limit", maxBodySize, "have", len(dst), "dropped", len(src))
		return dst
	}
	if len(src) > room {
		slog.Warn("extproc: response body reached the buffer limit; truncating",
			"limit", maxBodySize, "dropped", len(src)-room)
		src = src[:room]
	}
	return append(dst, src...)
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

// dispatchBufferedFrames feeds the response body to StreamingResponder plugins via
// RunResponseFrame, mirroring the proxy listeners' single-dispatch contract: application/json
// arrives as one last=true frame, text/event-stream is re-parsed with sseframe so each event is
// its own non-last frame followed by a terminal one.
//
// BOTH ARMS HONOUR `last`, which says this body message is the final one and the terminal frame
// belongs on it. Ending EVERY message with a terminal frame makes each parser finalize per
// message, and on the Anthropic dialect the first message holds message_start alone — prompt
// counted, output not — so that finalizes into a floor, the settle latch pins it, and the pass
// carrying message_delta is short-circuited by the very guard that stops a double charge. The
// output tokens, at 10x burndown on Bedrock, go unbilled.
//
// The non-SSE arm needs it for a different reason: without a gateway cost header the modelled
// figure is the only one there is, and finalizing each fragment loses it completely — measured at
// TotalTokens 0 and no cost record at all for a JSON body split across two messages.
// dispatchTerminalFrame finalizes the parsers at teardown, sending whatever has not been sent.
//
// The two arms differ because the body path treats them differently. An SSE body is dispatched
// frame by frame as each message arrives, so only the terminal marker is left; a non-SSE body
// is accumulated and dispatched once, so at teardown it has not been dispatched at all and the
// terminal call has to carry it.
//
// A CARRIED SSE TAIL IS DROPPED HERE, DELIBERATELY. What the carry holds is the fragment of an
// event whose terminator never arrived, so its payload is a truncated document: dispatching it
// would hand every parser a JSON fragment to fail on, which is noise, not a lost count. The
// counts of every event that DID complete were dispatched when their message arrived, and the
// terminal marker below is what turns them into a settled figure.
func dispatchTerminalFrame(ctx context.Context, p *pipeline.Holder, pctx *pipeline.Context) {
	if isEventStream(pctx.ResponseHeaders.Get("Content-Type")) {
		_ = p.RunResponseFrame(ctx, pctx, nil, true)
		return
	}
	_ = p.RunResponseFrame(ctx, pctx, pctx.ResponseBody, true)
}

func dispatchBufferedFrames(ctx context.Context, p *pipeline.Holder, pctx *pipeline.Context, last bool) pipeline.Action {
	contentType := pctx.ResponseHeaders.Get("Content-Type")
	if isEventStream(contentType) && len(pctx.ResponseBody) > 0 {
		// PARSE ONLY WHAT IS COMPLETE, AND KEEP THE REST FOR THE NEXT MESSAGE. sseframe
		// delivers an unterminated trailing event at EOF — correct for a stream that really
		// ended, wrong for one that merely ran out of THIS chunk — so a straddling event would
		// otherwise be split into two halves that parse to nothing. On the final message there
		// is no next one, so everything left is genuinely the end and the cut is skipped.
		buf := pctx.ResponseBody
		if !last {
			cut := lastSSEFrameBoundary(buf)
			carrySSETail(pctx, buf[cut:])
			buf = buf[:cut]
		}
		reader := sseframe.NewReader(bytes.NewReader(buf), maxBodySize)
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
		if !last {
			// More body to come. The scratch a StreamingResponder allocated on those frames
			// survives on pctx, so the next message continues the same stream.
			return pipeline.Action{Type: pipeline.Continue}
		}
		return p.RunResponseFrame(ctx, pctx, nil, true)
	}
	if !last {
		// NOTHING IS DISPATCHED UNTIL A NON-SSE BODY IS WHOLE. The parser's contract for this
		// arm is one call carrying the entire body, so handing it a fragment would ask it to
		// parse an incomplete document — and its settle latch would pin that answer. The bytes
		// are accumulating on pctx (see handleResponseBody); the terminal call below gets all
		// of them, whether it comes from the last body message or from Process's teardown flush.
		return pipeline.Action{Type: pipeline.Continue}
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

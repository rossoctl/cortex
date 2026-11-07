package forwardproxy

import (
	"net/http"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/listener/httpx"
	"github.com/rossoctl/cortex/core/listener/internal/bodyread"
	"github.com/rossoctl/cortex/core/listener/internal/sessionevent"
	"github.com/rossoctl/cortex/core/pipeline"
)

func (s *Server) recordOutboundRequestEvent(tl *tunnelLog, pctx *pipeline.Context, sid string) {
	s.recordCapturedRequest(tl, pctx, sid, nil)
}

// recordCapturedRequest is recordOutboundRequestEvent for a request captured as the agent sent
// it, c, which may be nil. The row carries c with the body as it goes upstream: pctx.Body,
// when a plugin rewrote it.
func (s *Server) recordCapturedRequest(tl *tunnelLog, pctx *pipeline.Context, sid string, c *pipeline.RequestCapture) {
	// Only inference is kept: the capture redacts headers and the query, not the body, and the
	// body of any other request, such as an OAuth token refresh, can be a credential.
	if pctx.Extensions.Inference == nil {
		c = nil
	}
	if c != nil && pctx.BodyMutated() {
		cp := *c
		cp.Forwarded = pctx.Body
		c = &cp
	}
	s.appendCaptured(tl, sid, sessionevent.Request(pctx, pipeline.Outbound), c)
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
	sid := s.attributeForRecording(pctx, s.trustedSessionOf(r), r.Header, chain)
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
	s.appendCaptured(tl, sid, ev, nil)
}

// appendCaptured is appendOutbound for a request row with its capture c, which may be nil.
func (s *Server) appendCaptured(tl *tunnelLog, sid string, ev pipeline.SessionEvent, c *pipeline.RequestCapture) {
	if tl != nil && tl.recordWith(sid, ev, c) {
		return
	}
	s.sink().AppendCapture(sid, ev, c)
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
	// With the capture: what a plugin refused is still the origin's answer.
	s.sink().AppendCapture(attribution.FiledSession(pctx.OutboundSessionID, s.sink().ActiveSession), sessionevent.DenyResponse(pctx, action, pipeline.Outbound), takeResponseCapture(pctx))
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
	sid := attribution.FiledSession(pctx.OutboundSessionID, s.sink().ActiveSession)
	// Always record — see the request-phase comment. This is what surfaces
	// responses no plugin acted on (e.g. a generic 404), carrying StatusCode
	// + Error even with empty invocations.
	s.sink().AppendCapture(sid, outboundResponseEvent(pctx, statusCode, fail), takeResponseCapture(pctx))
}

// takeResponseCapture is the capture of the response pctx's row records, nil when there is
// none, and clears it so a resent request's next row does not carry it again. A body the proxy
// did not read to its end is incomplete — the origin broke it off, or the proxy, a plugin or the
// client cut it short — whatever the row's error says.
func takeResponseCapture(pctx *pipeline.Context) *pipeline.RequestCapture {
	rc := pctx.ResponseCapture
	if rc == nil {
		return nil
	}
	pctx.ResponseCapture = nil
	rc.Incomplete = !rc.Ended
	return &pipeline.RequestCapture{Response: rc}
}

// outboundResponseEvent builds the SessionResponse row every outbound answer is
// recorded through — the shared shape, so the WebSocket splice
// (relayUpgradedWebSocket) can adjust its byte fields without forking the
// snapshotting logic.
func outboundResponseEvent(pctx *pipeline.Context, statusCode int, fail *pipeline.EventError) pipeline.SessionEvent {
	return sessionevent.Response(pctx, pipeline.Outbound, statusCode, fail)
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

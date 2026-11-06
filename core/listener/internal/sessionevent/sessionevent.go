// Package sessionevent builds the session rows every listener records for a
// request: Request and Response for one the pipeline let through, Deny and
// DenyResponse for one it rejected. TunnelOpen and TunnelClose build the rows
// of a tunnel, which only the forward proxy has.
//
// It exists because the four recorders that emit a phase:"denied" event — one
// per direction per listener — grew apart from the same copy-pasted body.
// extproc recorded Plugins, Identity and Duration on both directions; the
// reverse proxy recorded TLS and none of those three; the forward proxy
// recorded none of the four. So an operator reading /v1/sessions saw a
// different event shape depending on which listener served the request, and
// which fields were populated was a property of the serving listener rather
// than of the request — the one thing a parity suite exists to prevent.
//
// Every field is derived from pctx and is absent when pctx does not carry it,
// so a listener that cannot know something records nothing for it rather than
// a zero that reads as a real value.
package sessionevent

import (
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	authtls "github.com/rossoctl/cortex/core/tlsconfig"
)

// Deny builds the phase:"denied" event for a request a pipeline plugin
// rejected on OnRequest.
//
// dir is a parameter rather than read from pctx.Direction because the
// recorders are already one-per-direction and several of their unit tests
// construct a bare pctx with no Direction set. Reading it from pctx would
// relabel those events silently — Inbound is the zero value — which is a
// worse failure than passing the constant the caller already knows.
//
// The caller keeps two decisions. Whether to record at all: a deny from a
// plugin that appended no Invocation carries no attribution, and a
// content-free denial event is noise. And which session bucket to append to:
// that genuinely differs per listener, inbound keying on the A2A contextId
// while outbound inherits the most-recently-updated session.
func Deny(pctx *pipeline.Context, action pipeline.Action, dir pipeline.Direction) pipeline.SessionEvent {
	var status int
	var code, message string
	if action.Violation != nil {
		// Read the structured fields directly. Render() produces the HTTP wire
		// payload — status, headers, JSON body — which is the wrong shape for a
		// session event; what belongs here is the semantic Code and Reason.
		status = action.Violation.Status
		if status == 0 {
			status = pipeline.StatusFromCode(action.Violation.Code)
		}
		code = action.Violation.Code
		message = action.Violation.Reason
	}
	return pipeline.SessionEvent{
		At:            time.Now(),
		Direction:     dir,
		Phase:         pipeline.SessionDenied,
		RequestID:     pctx.RequestID(),
		Invocations:   pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:       pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:      pipeline.SnapshotIdentity(pctx),
		Host:          pctx.Host,
		RequestedHost: pctx.RequestedHost(),
		HTTPMethod:    pctx.Method,
		HTTPPath:      pctx.Path,
		StatusCode:    status,
		Error: &pipeline.EventError{
			Kind:    "policy",
			Code:    code,
			Message: message,
		},
		Duration:       pipeline.DurationSince(pctx.StartedAt),
		Client:         pctx.ClientInfo(),
		Subagent:       pctx.Subagent(),
		ParentSubagent: pctx.ParentSubagent(),
		Proc:           pctx.AttributedProc(),
		TLS:            TLS(pctx),
	}
}

// Request builds the SessionRequest row for a request the pipeline let
// through, on any listener and in either direction.
//
// Every listener records through this one builder (#1575). When each built its
// own literal, the copies drifted by direction: outbound rows never copied
// A2A and inbound rows never copied MCP, but the parsers do not check the
// direction. So an outbound a2a-parser match, which the session-budget demo
// configures, recorded its invocation and lost its method, contextId and parts.
// Every field here is empty when pctx does not carry it, so one builder serves
// the listeners that cannot know a field as well as the ones that can.
//
// The extensions are snapshots, so a response-phase mutation of the same
// struct (token counts set in OnResponse) does not reach this row.
//
// dir is a parameter for the reason Deny gives. The caller still decides
// whether to record and in which session bucket.
func Request(pctx *pipeline.Context, dir pipeline.Direction) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:              time.Now(),
		Direction:       dir,
		Phase:           pipeline.SessionRequest,
		RequestID:       pctx.RequestID(),
		BytesUp:         int64(len(pctx.Body)),
		A2A:             pipeline.SnapshotA2A(pctx.Extensions.A2A),
		MCP:             pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:       pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations:     pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:         pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:        pipeline.SnapshotIdentity(pctx),
		Host:            pctx.Host,
		RequestedHost:   pctx.RequestedHost(),
		HTTPMethod:      pctx.Method,
		HTTPPath:        pctx.Path,
		UpgradeStripped: pctx.UpgradeStripped,
		Client:          pctx.ClientInfo(),
		Subagent:        pctx.Subagent(),
		ParentSubagent:  pctx.ParentSubagent(),
		Proc:            pctx.AttributedProc(),
		ClientProc:      pctx.AttributedClientProc(),
		SessionHeader:   pctx.NamingHeader(),
		TLS:             TLS(pctx),
	}
}

// Response builds the SessionResponse row that pairs with Request.
//
// status is what the client got, and fail is an explicit failure the caller
// has to report (nil when it has none). pipeline.EventErrorOr says why fail
// wins over what the response implies.
//
// ClientProc, SessionHeader and UpgradeStripped are on the request row only.
// They describe the request, and the response row pairs with it by RequestID.
func Response(pctx *pipeline.Context, dir pipeline.Direction, status int, fail *pipeline.EventError) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:             time.Now(),
		Direction:      dir,
		Phase:          pipeline.SessionResponse,
		RequestID:      pctx.RequestID(),
		BytesDown:      pctx.ResponseBytes,
		A2A:            pipeline.SnapshotA2A(pctx.Extensions.A2A),
		MCP:            pipeline.SnapshotMCP(pctx.Extensions.MCP),
		Inference:      pipeline.SnapshotInference(pctx.Extensions.Inference),
		Invocations:    pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse),
		Plugins:        pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:       pipeline.SnapshotIdentity(pctx),
		Host:           pctx.Host,
		RequestedHost:  pctx.RequestedHost(),
		HTTPMethod:     pctx.Method,
		HTTPPath:       pctx.Path,
		StatusCode:     status,
		Error:          pipeline.EventErrorOr(pctx, fail),
		Duration:       pipeline.DurationSince(pctx.StartedAt),
		Ttfb:           pipeline.DurationBetween(pctx.StartedAt, pctx.ResponseHeadersAt),
		Client:         pctx.ClientInfo(),
		Subagent:       pctx.Subagent(),
		ParentSubagent: pctx.ParentSubagent(),
		Proc:           pctx.AttributedProc(),
		TLS:            TLS(pctx),
	}
}

// DenyResponse is Deny for a rejection on the response pass, carrying that
// pass's invocations.
func DenyResponse(pctx *pipeline.Context, action pipeline.Action, dir pipeline.Direction) pipeline.SessionEvent {
	ev := Deny(pctx, action, dir)
	ev.Invocations = pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse)
	return ev
}

// TunnelOpen builds a tunnel's open row. reason says why the bytes stayed
// opaque. The caller knows it, so it is a parameter and not derived here from
// the host shape.
func TunnelOpen(pctx *pipeline.Context, reason pipeline.TunnelReason) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:          time.Now(),
		Direction:   pipeline.Outbound,
		Phase:       pipeline.SessionRequest,
		RequestID:   pctx.RequestID(),
		Invocations: pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseRequest),
		Plugins:     pipeline.SnapshotPlugins(pctx.Extensions.Custom),
		Identity:    pipeline.SnapshotIdentity(pctx),
		Host:        pctx.Host,
		// Method is CONNECT — real on a proxied CONNECT, synthetic on a
		// transparently redirected connection — and Path is empty: the bytes
		// are opaque, so there is no request line to read one from. A blank
		// path on this row is the accurate answer, not a gap.
		HTTPMethod: pctx.Method,
		HTTPPath:   pctx.Path,
		// Explicit opaque-tunnel marker so agentop can fold this CONNECT into
		// the decrypted inner request without inferring "tunnel" from shape.
		Tunnel:       true,
		TunnelReason: reason,
		// The address the guard checked and the tunnel dialed — set only when
		// the egress guard is on, else absent (SessionEvent.DialAddr). On the
		// open row, which is where a reader pairs it with the Host the client
		// named. The close row pairs by RequestID and needs no copy.
		DialAddr:       pctx.DialedAddr,
		Client:         pctx.ClientInfo(),
		Subagent:       pctx.Subagent(),
		ParentSubagent: pctx.ParentSubagent(),
		Proc:           pctx.AttributedProc(),
	}
}

// TunnelClose builds the row for a tunnel that ended, or that never opened.
// It shares the open's RequestID, so agentop pairs the two rows.
//
// status is what the proxy answered the tunnel with. fail says why the tunnel
// failed, nil when it ran until one side closed. up and down are the bytes it
// carried, zero where nothing was counted.
//
// No Invocations and no Plugins: nothing runs on a tunnel's response, so there
// is nothing to snapshot that the open row does not already carry.
func TunnelClose(pctx *pipeline.Context, reason pipeline.TunnelReason, status int, fail *pipeline.EventError, up, down int64) pipeline.SessionEvent {
	return pipeline.SessionEvent{
		At:             time.Now(),
		Direction:      pipeline.Outbound,
		Phase:          pipeline.SessionResponse,
		RequestID:      pctx.RequestID(),
		Identity:       pipeline.SnapshotIdentity(pctx),
		Host:           pctx.Host,
		HTTPMethod:     pctx.Method,
		HTTPPath:       pctx.Path,
		StatusCode:     status,
		Error:          fail,
		Duration:       pipeline.DurationSince(pctx.StartedAt),
		Tunnel:         true,
		TunnelReason:   reason,
		BytesUp:        up,
		BytesDown:      down,
		Client:         pctx.ClientInfo(),
		Subagent:       pctx.Subagent(),
		ParentSubagent: pctx.ParentSubagent(),
		Proc:           pctx.AttributedProc(),
	}
}

// TLS summarizes the connection the request arrived on, and is nil whenever
// there is nothing true to say: a plaintext caller, or a listener that never
// sees the client's connection at all. In envoy-sidecar mode Envoy terminates
// the TLS and ext_proc receives the request over gRPC, so pctx.TLS is unset
// there and this records nothing rather than inventing a handshake.
func TLS(pctx *pipeline.Context) *pipeline.EventTLS {
	if pctx == nil || pctx.TLS == nil {
		return nil
	}
	return pipeline.NewEventTLS(pctx.TLS, authtls.PeerSPIFFEID(pctx.PeerCertificate()))
}

// Package sessionevent builds the parts of a session event that every
// listener should record identically.
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

// DenyResponse is Deny for a rejection on the response pass, carrying that
// pass's invocations.
func DenyResponse(pctx *pipeline.Context, action pipeline.Action, dir pipeline.Direction) pipeline.SessionEvent {
	ev := Deny(pctx, action, dir)
	ev.Invocations = pipeline.SnapshotInvocations(pctx.Extensions.Invocations, pipeline.InvocationPhaseResponse)
	return ev
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

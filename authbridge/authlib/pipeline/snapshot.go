package pipeline

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// SnapshotA2A returns a shallow copy of ext. The record helpers attach
// the snapshot to the SessionEvent rather than the live pointer so
// response-phase mutations on pctx.Extensions.A2A (e.g. the parser
// stamping the server-assigned contextId onto SessionID during
// OnResponse) don't retroactively rewrite request-phase events that
// were already appended. Slice fields are reused intentionally — they
// are only assigned, never mutated in place, after the parser
// completes.
func SnapshotA2A(ext *A2AExtension) *A2AExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

// SnapshotMCP returns a shallow copy of ext. Important for outbound
// request events: the same pctx.Extensions.MCP pointer receives Result
// or Err on the response side, so without snapshotting, the
// already-recorded request event would display the future response's
// result map.
func SnapshotMCP(ext *MCPExtension) *MCPExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

// SnapshotInference returns a shallow copy of ext. Scalar response
// fields (Completion, FinishReason, *Tokens) get assigned on the live
// extension during OnResponse; without snapshotting, the request event's
// view would contain the eventual response's token counts and completion.
func SnapshotInference(ext *InferenceExtension) *InferenceExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

// SnapshotInvocations is an alias for FilteredByPhase that participates
// in the Snapshot* family for symmetry with the other shallow-copy
// helpers. The underlying call already returns a fresh slice.
func SnapshotInvocations(ext *Invocations, phase InvocationPhase) *Invocations {
	return ext.FilteredByPhase(phase)
}

// SnapshotPlugins collects plugin-public observability events from
// pctx.Extensions.Custom entries whose keys end in PluginEventSuffix.
// Each matching value is json.Marshaled into the wire-form map under
// the plugin name (suffix stripped). Marshal errors downgrade to slog
// Debug and skip the entry rather than aborting recording — that keeps
// a misbehaving plugin from taking out the whole session stream.
func SnapshotPlugins(custom map[string]any) map[string]json.RawMessage {
	if len(custom) == 0 {
		return nil
	}
	var out map[string]json.RawMessage
	for k, v := range custom {
		if !strings.HasSuffix(k, PluginEventSuffix) {
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
		pluginName := strings.TrimSuffix(k, PluginEventSuffix)
		out[pluginName] = raw
	}
	return out
}

// SnapshotIdentity copies the caller identity off pctx so the session
// event stays valid after pctx is discarded. Returns nil when no
// identity information is available (e.g., jwt-validation didn't run
// on this path and no agent identity was attached).
func SnapshotIdentity(pctx *Context) *EventIdentity {
	if pctx.Identity == nil && pctx.Agent == nil {
		return nil
	}
	id := &EventIdentity{}
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

// DurationSince returns the elapsed time since start, or 0 when start
// is zero (pctx constructed without wall-clock stamping, e.g. in unit
// tests).
func DurationSince(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

// DeriveError constructs an EventError from response-side signals.
// Returns nil for 2xx / no guardrail block / no parser error.
func DeriveError(pctx *Context) *EventError {
	if pctx.Extensions.Security != nil && pctx.Extensions.Security.Blocked {
		return &EventError{
			Kind:    "blocked",
			Message: pctx.Extensions.Security.BlockReason,
		}
	}
	if pctx.StatusCode >= 400 {
		return &EventError{
			Kind: "backend_error",
			Code: strconv.Itoa(pctx.StatusCode),
		}
	}
	return nil
}

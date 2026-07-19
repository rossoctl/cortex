package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
)

// showDetail loads the row's event into the detail viewport as colorized
// JSON and remembers the focused row so yank (y) can find the event and
// layout() can re-render on resize.
//
// Marshal with SessionEvent.MarshalJSON first (readable wire form — string
// enums, durationMs), then filter inference/mcp extensions so request
// events show only request-side fields and response events show only
// response-side fields (TUI readability only — wire format is unchanged,
// and yank still writes the full JSON).
//
// The whole event is rendered — all plugin invocations, both directions —
// because the timeline is now one row per message; the per-plugin breakdown
// lives here in the detail, not in separate rows.
//
// When a CONNECT tunnel was folded into this row (TLS bridge), a one-block
// summary of the tunnel is prepended so the operator sees the bridged
// origin and the connection-level decision without a separate row. When the
// event arrived over TLS (SessionEvent.TLS non-nil), a small TLS header
// block is prepended too. Both absent for plaintext, non-bridged events.
func (m *model) showDetail(r eventRow) {
	e := r.event
	m.detailRow = r
	m.detailEvent = e
	data, err := json.Marshal(e)
	if err != nil {
		m.detailVp.SetContent("error marshaling event: " + err.Error())
		return
	}
	content := ColorizeJSONBytes(filterForDetail(data, e.Phase))
	if w := m.detailVp.Width; w > 0 {
		// Word-wrap on spaces/hyphens, fall back to hard break for long tokens.
		// ansi.Wrap preserves the JSON colorizer's escape codes so wrapped
		// content keeps its highlighting.
		content = ansi.Wrap(content, w, " -")
	}
	if r.tunnel != nil {
		content = tunnelHeader(r.tunnel) + "\n\n" + content
	}
	if header := tlsHeader(e.TLS); header != "" {
		content = header + "\n\n" + content
	}
	m.detailVp.SetContent(content)
	m.detailVp.GotoTop()
}

// tunnelHeader summarizes the CONNECT tunnel folded into a TLS-bridged
// request row: the bridged origin (host:port) and any gate invocations that
// ran on the tunnel-open before the bytes were decrypted. Lets an operator
// see the connection-level decision without a separate row.
//
//	tunnel:  CONNECT example.com:443 (TLS bridge)
//	  jwt-validation skip (no_inbound_identity)
func tunnelHeader(tunnel *pipeline.SessionEvent) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("tunnel:  CONNECT %s (TLS bridge)", tunnel.Host))
	for _, iv := range allInvocations(tunnel) {
		b.WriteString(fmt.Sprintf("\n  %s %s", iv.Plugin, iv.Action))
		if iv.Reason != "" {
			b.WriteString(" (" + iv.Reason + ")")
		}
	}
	return b.String()
}

// tlsHeader builds a one-block summary of the TLS connection state.
// Returns the empty string when tls is nil (plaintext events) so the
// caller can prepend unconditionally.
//
// The block stays on three lines so it fits in the detail pane
// without pushing the JSON off-screen on small terminals:
//
//	TLS:
//	  version: TLS 1.3 · cipher: TLS_AES_128_GCM_SHA256
//	  peer:    spiffe://rossoctl.local/ns/team1/sa/caller-agent
//
// Empty fields are skipped so the block stays terse on partial data.
func tlsHeader(tls *pipeline.EventTLS) string {
	if tls == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("TLS:")
	if tls.Version != "" || tls.CipherSuite != "" {
		parts := []string{}
		if tls.Version != "" {
			parts = append(parts, fmt.Sprintf("version: %s", tls.Version))
		}
		if tls.CipherSuite != "" {
			parts = append(parts, fmt.Sprintf("cipher: %s", tls.CipherSuite))
		}
		b.WriteString("\n  " + strings.Join(parts, " · "))
	}
	if tls.PeerSPIFFEID != "" {
		b.WriteString(fmt.Sprintf("\n  peer:    %s", tls.PeerSPIFFEID))
	}
	return b.String()
}

// filterForDetail rewrites the TUI-side view of a SessionEvent so the
// inference and mcp extensions only expose the fields relevant to the
// event's phase. Request events drop response-side fields (completion,
// tokens, toolCalls, mcp result/error); response events drop request-side
// fields (messages, tools, mcp params). The underlying event is unchanged.
func filterForDetail(data []byte, phase pipeline.SessionPhase) []byte {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	keep := inferenceReqKeys
	mcpKeep := mcpReqKeys
	if phase == pipeline.SessionResponse {
		keep = inferenceRespKeys
		mcpKeep = mcpRespKeys
	}
	a2aKeep := a2aReqKeys
	if phase == pipeline.SessionResponse {
		a2aKeep = a2aRespKeys
	}
	if inf, ok := m["inference"].(map[string]any); ok {
		m["inference"] = filterFields(inf, keep)
	}
	if mcp, ok := m["mcp"].(map[string]any); ok {
		m["mcp"] = filterFields(mcp, mcpKeep)
	}
	if a2a, ok := m["a2a"].(map[string]any); ok {
		m["a2a"] = filterFields(a2a, a2aKeep)
	}
	// Identity is summarized at the session level (events pane banner).
	// Drop it from per-event detail rows to reduce repetition — the full
	// value is still in the wire JSON that yank writes out.
	delete(m, "identity")
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// Field classifications for phase-based filtering. Order is not significant —
// ColorizeJSONBytes sorts keys alphabetically for stable display.
var (
	inferenceReqKeys = []string{
		"model", "messages", "temperature", "maxTokens", "topP",
		"stream", "tools", "toolChoice",
	}
	inferenceRespKeys = []string{
		"model", "completion", "finishReason", "promptTokens",
		"completionTokens", "totalTokens", "toolCalls",
	}
	mcpReqKeys  = []string{"method", "rpcId", "params"}
	mcpRespKeys = []string{"method", "rpcId", "result", "error"}
	// A2A: OnResponse captures the server-assigned contextId plus a summary
	// of the final result (finalStatus / artifact / errorMessage / taskId).
	// Drop the request-side message fields (messageId, role, parts) on
	// response rows so the detail view reflects what the response phase
	// actually contributed.
	a2aReqKeys  = []string{"method", "rpcId", "sessionId", "messageId", "taskId", "role", "parts"}
	a2aRespKeys = []string{"method", "rpcId", "sessionId", "taskId", "finalStatus", "artifact", "errorMessage"}
)

// filterFields returns a new map containing only the keys in `keep` that are
// present in obj. Keys not listed are dropped. This is strict filtering —
// unlike a partition, fields absent from the allow-list do not pass through.
func filterFields(obj map[string]any, keep []string) map[string]any {
	out := map[string]any{}
	for _, k := range keep {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	return out
}

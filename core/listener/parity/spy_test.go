package parity

import (
	"context"
	"encoding/json"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
)

// spyPlugin is the single fixture-mate for parity tests. Knobs (deny at
// OnRequest, emit at OnResponse, declare RequiresLater on another spy)
// arrive via Configure so BuildWithDeps can construct one through the
// registry like any other plugin.
type spyPlugin struct {
	name string
	cfg  spyConfig
}

// spyConfig is the wire-shape the registry factory decodes from Configure.
// Keep JSON tags stable — fixture builders in parity_test.go marshal this.
type spyConfig struct {
	DenyOnRequest bool              `json:"deny_on_request"`
	DenyStatus    int               `json:"deny_status"`
	DenyReason    string            `json:"deny_reason"`
	DenyDetails   map[string]string `json:"deny_details"`

	EmitOnResponse bool      `json:"emit_on_response"`
	ResponseEvent  *spyEvent `json:"response_event"`

	// RequiresLater names peer plugins that must appear at a HIGHER
	// index in the same pipeline. Populates PluginCapabilities so the
	// registry's dependency validator can reject wrong-order pipelines.
	RequiresLater []string `json:"requires_later"`

	// ReadsBody flips PluginCapabilities.ReadsBody, engaging each
	// listener's body-handling path.
	ReadsBody bool `json:"reads_body"`

	// RecordRequestBody publishes the OnRequest body at
	// SessionEvent.Plugins[<name>/req-body].
	RecordRequestBody bool `json:"record_request_body"`

	// RecordResponseFrames publishes accumulated frame bytes + terminal
	// count at SessionEvent.Plugins[<name>/resp-body]. Only meaningful
	// on the spyStreamingPlugin variant.
	RecordResponseFrames bool `json:"record_response_frames"`
}

// bodyObservation is what the spy publishes about the bodies it saw.
type bodyObservation struct {
	Body           string `json:"body"`
	TerminalFrames int    `json:"terminal_frames,omitempty"`
}

// spyEvent is the payload emitted at OnResponse. Trivial and JSON-
// stable so parity assertions compare raw JSON directly.
type spyEvent struct {
	Marker string `json:"marker"`
	Count  int    `json:"count"`
}

func (s *spyPlugin) Name() string { return s.name }

func (s *spyPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		RequiresLater: s.cfg.RequiresLater,
		ReadsBody:     s.cfg.ReadsBody,
	}
}

func (s *spyPlugin) Configure(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &s.cfg)
}

func (s *spyPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if s.cfg.RecordRequestBody {
		s.publish(pctx, bodyReqStrippedSuffix+pipeline.PluginEventSuffix, bodyObservation{Body: string(pctx.Body)})
	}
	if !s.cfg.DenyOnRequest {
		return pipeline.Action{Type: pipeline.Continue}
	}
	pctx.Record(pipeline.Invocation{
		Action:  pipeline.ActionDeny,
		Reason:  s.cfg.DenyReason,
		Details: s.cfg.DenyDetails,
	})
	return pipeline.DenyStatus(s.cfg.DenyStatus, s.cfg.DenyReason, s.cfg.DenyReason)
}

func (s *spyPlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if s.cfg.EmitOnResponse && s.cfg.ResponseEvent != nil {
		s.publish(pctx, pipeline.PluginEventSuffix, *s.cfg.ResponseEvent)
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// publish writes to pctx.Extensions.Custom under the spy's name plus the
// given suffix, so the listener's SnapshotPlugins promotes it onto
// SessionEvent.Plugins.
func (s *spyPlugin) publish(pctx *pipeline.Context, suffix string, v any) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[s.name+suffix] = v
}

// spyStreamingPlugin wraps spyPlugin and adds OnResponseFrame, making it
// a pipeline.StreamingResponder. Pipeline.RunResponse skips streaming
// responders and dispatches only via OnResponseFrame; body-recording
// fixtures use this variant, other fixtures use the base spyPlugin.
type spyStreamingPlugin struct {
	spyPlugin
}

// OnResponseFrame accumulates frame bytes and counts terminal frames so
// fixtures can assert exactly-once semantics and byte-for-byte reassembly
// without depending on how many frames each listener produced.
func (s *spyStreamingPlugin) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if !s.cfg.RecordResponseFrames {
		return pipeline.Action{Type: pipeline.Continue}
	}
	st := pipeline.GetState[frameState](pctx, s.name+"/frames")
	if st == nil {
		st = &frameState{}
		pipeline.SetState(pctx, s.name+"/frames", st)
	}
	st.bytes = append(st.bytes, frame...)
	if last {
		st.terminals++
		s.publish(pctx, bodyRespStrippedSuffix+pipeline.PluginEventSuffix, bodyObservation{
			Body:           string(st.bytes),
			TerminalFrames: st.terminals,
		})
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// frameState is the per-request scratch OnResponseFrame accumulates into.
type frameState struct {
	bytes     []byte
	terminals int
}

// Compile-time interface checks.
var (
	_ pipeline.Plugin             = (*spyPlugin)(nil)
	_ pipeline.Configurable       = (*spyPlugin)(nil)
	_ pipeline.Plugin             = (*spyStreamingPlugin)(nil)
	_ pipeline.Configurable       = (*spyStreamingPlugin)(nil)
	_ pipeline.StreamingResponder = (*spyStreamingPlugin)(nil)
)

// Registered names. The -streaming variant qualifies as a
// pipeline.StreamingResponder; body-recording fixtures use it.
const (
	spyPluginA          = "parity-spy-a"
	spyPluginB          = "parity-spy-b"
	spyPluginAStreaming = "parity-spy-a-streaming"
)

// Body-observation event keys in stripped form — the shape
// SessionEvent.Plugins uses after SnapshotPlugins removes PluginEventSuffix.
const (
	bodyReqStrippedSuffix  = "/req-body"
	bodyRespStrippedSuffix = "/resp-body"
)

func init() {
	plugins.RegisterPlugin(spyPluginA, func() pipeline.Plugin { return &spyPlugin{name: spyPluginA} })
	plugins.RegisterPlugin(spyPluginB, func() pipeline.Plugin { return &spyPlugin{name: spyPluginB} })
	plugins.RegisterPlugin(spyPluginAStreaming, func() pipeline.Plugin {
		return &spyStreamingPlugin{spyPlugin: spyPlugin{name: spyPluginAStreaming}}
	})
}

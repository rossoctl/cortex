package parity

import (
	"context"
	"encoding/json"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
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
}

// spyEvent is the payload emitted at OnResponse. Trivial and JSON-
// stable so parity assertions compare raw JSON directly.
type spyEvent struct {
	Marker string `json:"marker"`
	Count  int    `json:"count"`
}

func (s *spyPlugin) Name() string { return s.name }

func (s *spyPlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{RequiresLater: s.cfg.RequiresLater}
}

func (s *spyPlugin) Configure(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &s.cfg)
}

func (s *spyPlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
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
	if !s.cfg.EmitOnResponse || s.cfg.ResponseEvent == nil {
		return pipeline.Action{Type: pipeline.Continue}
	}
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[s.name+pipeline.PluginEventSuffix] = *s.cfg.ResponseEvent
	return pipeline.Action{Type: pipeline.Continue}
}

// Compile-time interface checks.
var (
	_ pipeline.Plugin       = (*spyPlugin)(nil)
	_ pipeline.Configurable = (*spyPlugin)(nil)
)

// Register two spy factories so RequiresLater fixtures can pair two
// distinct plugins in one pipeline. Names are scoped to avoid clashing
// with any real plugin registered in the same process.
const (
	spyPluginA = "parity-spy-a"
	spyPluginB = "parity-spy-b"
)

func init() {
	plugins.RegisterPlugin(spyPluginA, func() pipeline.Plugin { return &spyPlugin{name: spyPluginA} })
	plugins.RegisterPlugin(spyPluginB, func() pipeline.Plugin { return &spyPlugin{name: spyPluginB} })
}

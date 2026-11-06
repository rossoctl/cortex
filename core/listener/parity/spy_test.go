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

	// DenyOnResponse rejects at OnResponse with DenyStatus, DenyReason and
	// DenyDetails, and records the deny first, as DenyOnRequest does. The
	// response-deny fixtures in divergence_test.go use it.
	DenyOnResponse bool `json:"deny_on_response"`

	// EmitOnRequest publishes RequestEvent at OnRequest, BEFORE any deny.
	// Without it a deny fixture's Plugins map is empty on every listener,
	// so the PluginKeys comparison below comes down to empty-vs-empty and
	// a listener that never snapshots Plugins at all compares equal to one
	// that does. That is how the drift in #936 survived this suite.
	EmitOnRequest bool      `json:"emit_on_request"`
	RequestEvent  *spyEvent `json:"request_event"`

	// Subject / ClientID / Scopes make the spy stand in for an auth
	// plugin, which is the only thing that ever populates pctx.Identity
	// (jwtvalidation/plugin.go:439 does exactly this assignment). A
	// fixture that cannot authenticate cannot tell a listener that drops
	// Identity from one that had none to record.
	Subject  string   `json:"subject"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`

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

	// RecordRequestBodyDigest is RecordRequestBody for a body too big to put in
	// a fixture literal — same bytes, reported as length + SHA-256 at
	// SessionEvent.Plugins[<name>/req-body-digest]. See
	// RecordResponseBodyDigest for the reasoning; the request side needs it
	// because the request caps diverge too (1 MiB vs 32 MiB), and a failure
	// message is readable at this size while two megabytes of expected body is
	// not.
	RecordRequestBodyDigest bool `json:"record_request_body_digest"`

	// MutateRequestBody, when non-nil, is the exact byte string the spy
	// hands to pctx.SetBody at OnRequest — making it stand in for
	// tool-prune, which is the in-tree plugin whose whole job is this and
	// whose cross-shape behaviour had no fixture here.
	//
	// Non-nil ALSO flips PluginCapabilities.WritesRequestBody (which
	// Normalize promotes to ReadsBody, so the body gets buffered without
	// ReadsBody being set too). Deriving the capability from the knob
	// rather than giving it its own bool keeps the two from drifting
	// apart in a fixture: a spy that mutates always declares it.
	//
	// An EMPTY-but-non-nil value is meaningful and reachable — []byte{}
	// rewrites the request to a zero-length body, which is a different
	// wire shape from a body-less request and the one a truncating
	// plugin produces at its limit.
	MutateRequestBody []byte `json:"mutate_request_body"`

	// RecordResponseFrames publishes accumulated frame bytes + terminal
	// count at SessionEvent.Plugins[<name>/resp-body]. Only meaningful
	// on the spyStreamingPlugin variant.
	RecordResponseFrames bool `json:"record_response_frames"`

	// RecordResponseBodyDigest publishes the length and SHA-256 of the
	// BUFFERED response body at SessionEvent.Plugins[<name>/resp-body-digest].
	//
	// The gap it fills: RecordRequestBody covers the request and
	// RecordResponseFrames covers the streamed response, so nothing reported
	// what a plugin was handed on the buffered response path — which is where
	// the listeners' response-body caps differ. Given the body, extproc
	// TRUNCATES an oversized response to its 1 MiB cap and runs the pipeline on
	// the prefix (appendBoundedBody), so the divergence is visible ONLY in the
	// plugin's view of the body: the client still gets the whole thing and
	// neither the event's status nor its error says a byte went missing.
	//
	// "Given the body" is load-bearing. On the shipped envoy-sidecar config
	// Envoy's own 1 MiB default buffer limit refuses the response first and
	// ext_proc receives no body message at all, so what this knob reports there
	// is a length of ZERO — the stream-end flush having run the response phase
	// on an empty buffer. That is why it publishes unconditionally: both the
	// truncated prefix and the empty buffer are claims a fixture needs to make,
	// and they are different claims from OnResponse not running.
	// divergence_test.go's file comment lays out which config produces which.
	//
	// A digest rather than the bytes for two reasons. A multi-megabyte
	// expectation literal is unreadable, and the digest is what makes the
	// assertion sharp: length alone would pass for any 1 MiB of the body,
	// while the digest pins it to the PREFIX. Truncation that kept the tail
	// would be just as wrong and just as silent.
	RecordResponseBodyDigest bool `json:"record_response_body_digest"`

	// RequestInvocation is recorded via pctx.Record at OnRequest when
	// non-nil, which is the only way a spy that does NOT deny appears in
	// SessionEvent.Invocations at all: the deny branch below used to be
	// this plugin's sole Record site. Without it no fixture could put two
	// plugins on the record at once, and ordering is unobservable with
	// one — a one-element slice compares equal under every ordering rule,
	// which is why invocationsEqual's set comparison went unquestioned.
	RequestInvocation *invocationRecord `json:"request_invocation"`
}

// invocationRecord is a fixture-specified pipeline.Invocation. Action is
// a plain string rather than pipeline.InvocationAction so a fixture can
// hand the spy any of the five vocabulary values through the JSON config
// round-trip without the knob caring which.
//
// Plugin, Phase and Path are deliberately absent: Context.Record
// back-fills all three (context.go:534-543), and a fixture that supplied
// them would be asserting its own literals rather than the framework's
// attribution.
type invocationRecord struct {
	Action  string            `json:"action"`
	Reason  string            `json:"reason"`
	Details map[string]string `json:"details,omitempty"`
}

// bodyObservation is what the spy publishes about the bodies it saw.
type bodyObservation struct {
	Body           string `json:"body"`
	TerminalFrames int    `json:"terminal_frames,omitempty"`
}

// bodyDigest is what the spy publishes about a buffered response body: its
// length and the SHA-256 of exactly those bytes. See
// spyConfig.RecordResponseBodyDigest for why it is a digest and not the body.
type bodyDigest struct {
	Len    int    `json:"len"`
	SHA256 string `json:"sha256"`
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
		RequiresLater:     s.cfg.RequiresLater,
		ReadsBody:         s.cfg.ReadsBody,
		WritesRequestBody: s.cfg.MutateRequestBody != nil,
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
	if s.cfg.RecordRequestBodyDigest {
		s.publish(pctx, bodyReqDigestStrippedSuffix+pipeline.PluginEventSuffix, bodyDigest{
			Len:    len(pctx.Body),
			SHA256: sha256Hex(pctx.Body),
		})
	}
	// Rewrite AFTER recording, so a fixture can pin both halves of the
	// mutation independently: /req-body is what the listener handed the
	// plugin, and the upstream capture is what the listener sent on. Read
	// the other way round the two would be the same assertion twice, and a
	// listener that forwarded the ORIGINAL bytes would still look right.
	if s.cfg.MutateRequestBody != nil {
		pctx.SetBody(s.cfg.MutateRequestBody)
	}
	// Authenticate and emit before deciding to deny: both must land on the
	// denial event, since a denial is exactly when an operator needs to
	// know who was denied and which plugin said so.
	if s.cfg.Subject != "" || s.cfg.ClientID != "" {
		pctx.Identity = spyIdentity{
			subject:  s.cfg.Subject,
			clientID: s.cfg.ClientID,
			scopes:   s.cfg.Scopes,
		}
	}
	if s.cfg.EmitOnRequest && s.cfg.RequestEvent != nil {
		s.publish(pctx, pipeline.PluginEventSuffix, *s.cfg.RequestEvent)
	}
	// Recorded before the deny branch so a spy configured with both lands
	// its own observation first, in the order a real gate would: observe
	// what you saw, then refuse.
	if s.cfg.RequestInvocation != nil {
		pctx.Record(pipeline.Invocation{
			Action:  pipeline.InvocationAction(s.cfg.RequestInvocation.Action),
			Reason:  s.cfg.RequestInvocation.Reason,
			Details: s.cfg.RequestInvocation.Details,
		})
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
	// Published unconditionally when the knob is set, zero length included, so
	// a fixture can tell "OnResponse ran and was handed nothing" from
	// "OnResponse never ran" — which is exactly the difference between
	// extproc's truncate-and-continue and reverseproxy's 502 refusal.
	if s.cfg.RecordResponseBodyDigest {
		s.publish(pctx, bodyRespDigestStrippedSuffix+pipeline.PluginEventSuffix, bodyDigest{
			Len:    len(pctx.ResponseBody),
			SHA256: sha256Hex(pctx.ResponseBody),
		})
	}
	if s.cfg.EmitOnResponse && s.cfg.ResponseEvent != nil {
		s.publish(pctx, pipeline.PluginEventSuffix, *s.cfg.ResponseEvent)
	}
	if !s.cfg.DenyOnResponse {
		return pipeline.Action{Type: pipeline.Continue}
	}
	pctx.Record(pipeline.Invocation{
		Action:  pipeline.ActionDeny,
		Reason:  s.cfg.DenyReason,
		Details: s.cfg.DenyDetails,
	})
	return pipeline.DenyStatus(s.cfg.DenyStatus, s.cfg.DenyReason, s.cfg.DenyReason)
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

// spyIdentity is the pipeline.Identity an authenticating plugin leaves on
// pctx. Kept local to this package because every test file that needs one
// defines its own; there is no shared stub to borrow.
type spyIdentity struct {
	subject  string
	clientID string
	scopes   []string
}

func (s spyIdentity) Subject() string  { return s.subject }
func (s spyIdentity) ClientID() string { return s.clientID }
func (s spyIdentity) Scopes() []string { return s.scopes }

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
	bodyReqStrippedSuffix        = "/req-body"
	bodyReqDigestStrippedSuffix  = "/req-body-digest"
	bodyRespStrippedSuffix       = "/resp-body"
	bodyRespDigestStrippedSuffix = "/resp-body-digest"
)

func init() {
	plugins.RegisterPlugin(spyPluginA, func() pipeline.Plugin { return &spyPlugin{name: spyPluginA} })
	plugins.RegisterPlugin(spyPluginB, func() pipeline.Plugin { return &spyPlugin{name: spyPluginB} })
	plugins.RegisterPlugin(spyPluginAStreaming, func() pipeline.Plugin {
		return &spyStreamingPlugin{spyPlugin: spyPlugin{name: spyPluginAStreaming}}
	})
}

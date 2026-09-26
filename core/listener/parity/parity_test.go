// Package parity runs the same fixture through both the extproc and
// HTTP-proxy listeners and asserts the resulting session event is
// identical, catching silent drift between the two deployment shapes.
package parity

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

// listenerRun pairs a driver with a name so failure messages point at
// the listener that drifted.
type listenerRun struct {
	name string
	run  func(*testing.T, fixture, pipeline.SessionPhase) *observation
}

var inboundListeners = []listenerRun{
	{name: "extproc", run: runExtproc},
	{name: "reverseproxy", run: runReverseProxy},
}

var outboundListeners = []listenerRun{
	{name: "extproc", run: runExtproc},
	{name: "forwardproxy", run: runForwardProxy},
}

// TestParity_DenyOnRequest: pctx.Record before Reject must produce the
// same phase:"denied" event on both listeners.
func TestParity_DenyOnRequest(t *testing.T) {
	f := fixture{
		name:      "deny-on-request",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			DenyOnRequest: true,
			DenyStatus:    429,
			DenyReason:    "spy.denied",
			DenyDetails:   map[string]string{"cutoff_reason": "quota"},
		})},
		method: "GET",
		path:   "/parity/deny",
	}
	assertParity(t, f, pipeline.SessionDenied, inboundListeners)
}

// TestParity_ResponseEventEmission: an OnResponse emit to Extensions.
// Custom must surface identically on SessionEvent.Plugins from both
// listeners.
func TestParity_ResponseEventEmission(t *testing.T) {
	f := fixture{
		name:      "priced-response-headers-only",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			EmitOnResponse: true,
			ResponseEvent:  &spyEvent{Marker: "priced", Count: 3},
		})},
		method:         "GET",
		path:           "/parity/priced",
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
	}
	assertParity(t, f, pipeline.SessionResponse, inboundListeners)
}

// TestParity_OutboundDenyOnRequest: same deny-on-request contract on the
// outbound side — extproc and forwardproxy must record the same
// phase:"denied" event when a plugin rejects at OnRequest.
func TestParity_OutboundDenyOnRequest(t *testing.T) {
	f := fixture{
		name:      "outbound-deny-on-request",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			DenyOnRequest: true,
			DenyStatus:    403,
			DenyReason:    "spy.blocked",
			DenyDetails:   map[string]string{"cutoff_reason": "egress-policy"},
		})},
		method: "GET",
		path:   "/parity/egress",
	}
	assertParity(t, f, pipeline.SessionDenied, outboundListeners)
}

// TestParity_ReadsBodyBufferedJSON: with ReadsBody set, both listeners
// present the plugin with the same request and response body bytes,
// despite extproc's two-phase handshake vs. the proxies' in-process
// buffering.
func TestParity_ReadsBodyBufferedJSON(t *testing.T) {
	reqBody := []byte(`{"prompt":"hello"}`)
	respBody := []byte(`{"reply":"ok"}`)
	f := fixture{
		name:      "reads-body-buffered-json",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:            true,
			RecordRequestBody:    true,
			RecordResponseFrames: true,
		})},
		method:         "POST",
		path:           "/parity/echo",
		reqBody:        reqBody,
		upstreamStatus: 200,
		upstreamBody:   respBody,
		expectedPluginEvents: map[string]string{
			spyPluginAStreaming + bodyReqStrippedSuffix:  jsonOf(bodyObservation{Body: string(reqBody)}),
			spyPluginAStreaming + bodyRespStrippedSuffix: jsonOf(bodyObservation{Body: string(respBody), TerminalFrames: 1}),
		},
	}
	assertParity(t, f, pipeline.SessionResponse, inboundListeners)
}

// TestParity_OutboundReadsBodyBufferedJSON: the outbound-side mirror of
// TestParity_ReadsBodyBufferedJSON. Exercises forwardproxy's body path
// (agent egress in proxy-sidecar mode) against extproc.
func TestParity_OutboundReadsBodyBufferedJSON(t *testing.T) {
	reqBody := []byte(`{"prompt":"hello"}`)
	respBody := []byte(`{"reply":"ok"}`)
	f := fixture{
		name:      "outbound-reads-body-buffered-json",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:            true,
			RecordRequestBody:    true,
			RecordResponseFrames: true,
		})},
		method:         "POST",
		path:           "/parity/echo",
		reqBody:        reqBody,
		upstreamStatus: 200,
		upstreamBody:   respBody,
		expectedPluginEvents: map[string]string{
			spyPluginAStreaming + bodyReqStrippedSuffix:  jsonOf(bodyObservation{Body: string(reqBody)}),
			spyPluginAStreaming + bodyRespStrippedSuffix: jsonOf(bodyObservation{Body: string(respBody), TerminalFrames: 1}),
		},
	}
	assertParity(t, f, pipeline.SessionResponse, outboundListeners)
}

// TestParity_ReadsBodySSE: an SSE upstream yields the same reassembled
// payload bytes and exactly one terminal-frame dispatch on every
// listener. Frame counts legitimately vary (extproc buffered, proxies
// streamed) and are intentionally NOT asserted — the anchors are
// reassembled-content parity and exactly-once terminal semantics.
func TestParity_ReadsBodySSE(t *testing.T) {
	// The listener framework's sseframe reader strips `data: ` and the
	// `\n\n` separators before dispatching frames, so the plugin sees
	// the concatenated event payloads, not the raw wire bytes.
	events := []string{
		`{"type":"message_start"}`,
		`{"type":"message_delta","usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
		`[DONE]`,
	}
	var sse, payloads strings.Builder
	for _, e := range events {
		sse.WriteString("data: ")
		sse.WriteString(e)
		sse.WriteString("\n\n")
		payloads.WriteString(e)
	}
	f := fixture{
		name:      "reads-body-sse",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:            true,
			RecordResponseFrames: true,
		})},
		method:              "POST",
		path:                "/parity/sse",
		upstreamStatus:      200,
		upstreamBody:        []byte(sse.String()),
		upstreamContentType: "text/event-stream",
		expectedPluginEvents: map[string]string{
			spyPluginAStreaming + bodyRespStrippedSuffix: jsonOf(bodyObservation{Body: payloads.String(), TerminalFrames: 1}),
		},
	}
	assertParity(t, f, pipeline.SessionResponse, inboundListeners)
}

// TestParity_HeaderOnlyResponse is the shape this suite could not express
// until the synthetic-body gate in runExtproc was fixed, and the one it
// existed to catch.
//
// A response that ends on its headers — a 204, a 304, an error status with no
// body — must still reach the terminal RunResponseFrame(nil, true) on every
// listener, because that dispatch is where a streaming-aware plugin finalizes:
// where inference-parser settles the cost off the gateway's response header and
// records its no_response_body Skip. On extproc that dispatch hangs off a gate
// that has to consider end_of_stream: keyed on NeedsBody() alone its header phase
// defers to a body phase Envoy never opens, and a body-less response then settles
// nothing and records no response row at all. The proxies got it right, which is exactly the divergence this suite is
// for and exactly what it could not see: the old harness handed extproc a
// zero-length ResponseBody message no Envoy would send, papering over the gap.
//
// The anchor is TerminalFrames: 1 with empty content — one finalization, on a
// response that genuinely carried nothing. Exactly-once matters as much as
// at-least-once: two terminal dispatches means two charges for one request.
func TestParity_HeaderOnlyResponse(t *testing.T) {
	f := fixture{
		name:      "header-only-response",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:            true,
			RecordResponseFrames: true,
		})},
		method:         "GET",
		path:           "/parity/no-content",
		upstreamStatus: 204,
		upstreamBody:   nil,
		expectedPluginEvents: map[string]string{
			spyPluginAStreaming + bodyRespStrippedSuffix: jsonOf(bodyObservation{TerminalFrames: 1}),
		},
	}
	assertParity(t, f, pipeline.SessionResponse, inboundListeners)
}

// TestParity_OutboundHeaderOnlyResponse mirrors the above on the egress side,
// which is where the dropped cost actually cost money: agents reach LiteLLM
// through the outbound pipeline, and a rate-limited or errored turn that ends on
// its headers still carries the gateway's own charge in a response header.
func TestParity_OutboundHeaderOnlyResponse(t *testing.T) {
	f := fixture{
		name:      "outbound-header-only-response",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:            true,
			RecordResponseFrames: true,
		})},
		method:         "GET",
		path:           "/parity/no-content",
		upstreamStatus: 204,
		upstreamBody:   nil,
		expectedPluginEvents: map[string]string{
			spyPluginAStreaming + bodyRespStrippedSuffix: jsonOf(bodyObservation{TerminalFrames: 1}),
		},
	}
	assertParity(t, f, pipeline.SessionResponse, outboundListeners)
}

// TestParity_InboundRequestBodyOverflow: a body exceeding both
// listeners' 1 MiB cap must be rejected before the pipeline runs, with
// the same wire status.
func TestParity_InboundRequestBodyOverflow(t *testing.T) {
	big := make([]byte, (1<<20)+1) // 1 MiB + 1 byte — one over the cap
	for i := range big {
		big[i] = 'x'
	}
	f := fixture{
		name:                  "inbound-request-body-overflow",
		direction:             pipeline.Inbound,
		pipelineRefusedPreRun: true,
		expectedWireStatus:    413,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody: true,
		})},
		method:  "POST",
		path:    "/parity/big",
		reqBody: big,
	}
	// wantPhase is load-bearing: SessionRequest is where all three
	// listeners record on the request path, so a regression that ran
	// the pipeline anyway would surface here and trip the
	// pipelineRefusedPreRun check.
	assertParity(t, f, pipeline.SessionRequest, inboundListeners)
}

// TestParity_RequiresLaterOrderingRejected: each listener's
// construction must reject a RequiresLater violation (dependency at a
// LOWER index than the plugin naming it; contract requires HIGHER).
func TestParity_RequiresLaterOrderingRejected(t *testing.T) {
	entriesWrong := []config.PluginEntry{
		spyEntry(spyPluginB, spyConfig{}),
		spyEntry(spyPluginA, spyConfig{RequiresLater: []string{spyPluginB}}),
	}
	entriesOK := []config.PluginEntry{
		spyEntry(spyPluginA, spyConfig{RequiresLater: []string{spyPluginB}}),
		spyEntry(spyPluginB, spyConfig{}),
	}

	builders := []struct {
		name string
		fn   func([]config.PluginEntry) error
	}{
		{"extproc", tryBuildExtproc},
		{"reverseproxy", tryBuildReverseProxy},
		{"forwardproxy", tryBuildForwardProxy},
	}
	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			if err := b.fn(entriesWrong); err == nil {
				t.Errorf("%s accepted a wrong-order pipeline; RequiresLater is not enforced", b.name)
			}
			if err := b.fn(entriesOK); err != nil {
				t.Errorf("%s rejected a well-ordered pipeline: %v", b.name, err)
			}
		})
	}
}

// assertParity runs the fixture through every listener and fails on
// any observation mismatch. Partial presence (one listener records, the
// others don't) is itself drift and reported at the parent-test level.
func assertParity(t *testing.T, f fixture, wantPhase pipeline.SessionPhase, listeners []listenerRun) {
	t.Helper()

	// Refuse a single-listener call: parity needs a pair to compare.
	if len(listeners) < 2 {
		t.Fatalf("assertParity: fixture %q was given %d listener(s); need at least 2", f.name, len(listeners))
	}

	type namedObs struct {
		listener string
		observed *observation
	}
	// COLLECTED OUTSIDE t.Run, WHICH IS NOT A STYLE CHOICE. Appending inside the subtest closure
	// makes this whole suite FAIL OPEN the moment anyone adds t.Parallel to it: the parent
	// continues past the loop before any subtest body has run, `got` is empty, the `len(got) < 2`
	// return below fires, and every comparison below it is skipped — verified, exit 0 with zero
	// drift errors and nothing compared. A suite whose entire purpose is catching per-listener
	// divergence would then pass having compared nothing, silently.
	//
	// This is the hazard cost/settle's TestNoTestInThisPackageRunsInParallel guards against in that
	// package; the fix here is structural instead, so it holds however this file is run.
	got := make([]namedObs, 0, len(listeners))
	for _, l := range listeners {
		var obs *observation
		t.Run(f.name+"/"+l.name, func(t *testing.T) {
			obs = l.run(t, f, wantPhase)
			if obs == nil {
				t.Fatalf("listener %q produced no matching event for fixture %q (phase=%v)", l.name, f.name, wantPhase)
			}
		})
		if obs != nil {
			got = append(got, namedObs{listener: l.name, observed: obs})
		}
	}

	// Partial-presence drift: some listeners produced an event, others
	// didn't. Report at the parent level so a failing subtest doesn't
	// mask the parity gap.
	if len(got) > 0 && len(got) < len(listeners) {
		present := make([]string, 0, len(got))
		for _, g := range got {
			present = append(present, g.listener)
		}
		t.Errorf("parity presence drift on fixture %q: only these listeners produced an event: %v", f.name, present)
	}
	// AND A FIXTURE THAT COMPARED NOTHING IS A BROKEN SUITE, not a pass. Without this, any future
	// change that stops the loop from collecting — a t.Parallel, an early return, a driver that
	// silently skips — turns every comparison below into a no-op that reports success.
	if len(got) == 0 {
		t.Fatalf("fixture %q collected no observations from %d listeners: nothing was compared", f.name, len(listeners))
	}
	if len(got) < 2 {
		return // one or more legs failed in the subtest; presence drift already reported.
	}

	// Wire-status anchor: pin the transport code when the fixture
	// declares one, so a shared regression (both listeners stop
	// enforcing the cap and return 200) fails against the expectation
	// rather than passing parity.
	if f.expectedWireStatus != 0 {
		for _, g := range got {
			if g.observed.WireStatus != f.expectedWireStatus {
				t.Errorf("fixture %q listener %s: WireStatus = %d, want %d", f.name, g.listener, g.observed.WireStatus, f.expectedWireStatus)
			}
		}
	}

	// Correctness anchors: validate each listener against the fixture's
	// expected plugin events before the pairwise diff, so a shared-drop
	// bug (both listeners omit the event) fails against the fixture
	// rather than passing parity.
	for _, g := range got {
		for key, wantJSON := range f.expectedPluginEvents {
			gotJSON, ok := g.observed.PluginEventJSON[key]
			if !ok {
				t.Errorf("fixture %q listener %s: missing expected plugin event %q", f.name, g.listener, key)
				continue
			}
			if !jsonEqual(gotJSON, wantJSON) {
				t.Errorf("fixture %q listener %s: plugin event %q\n  got:  %s\n  want: %s", f.name, g.listener, key, gotJSON, wantJSON)
			}
		}
	}

	// Pairwise compare against the first listener. All observations must
	// agree; a diff names both sides so operators see which drifted.
	base := got[0]
	for _, other := range got[1:] {
		if diff := observationDiff(base.observed, other.observed); diff != "" {
			t.Errorf("parity drift on fixture %q between %s and %s:\n%s",
				f.name, base.listener, other.listener, diff)
		}
	}
}

// observationDiff returns the first field-level disagreement, or ""
// when both agree on every parity-comparable field.
func observationDiff(a, b *observation) string {
	if a.PipelineRan != b.PipelineRan {
		return fmt.Sprintf("PipelineRan: %v vs %v", a.PipelineRan, b.PipelineRan)
	}
	// WireStatus is meaningful only when the pipeline refused pre-run;
	// on the success path extproc has no HTTP transport to report from.
	// Session-event fields cover parity for the run-and-recorded case.
	if !a.PipelineRan {
		if a.WireStatus != b.WireStatus {
			return fmt.Sprintf("WireStatus: %d vs %d", a.WireStatus, b.WireStatus)
		}
		return ""
	}
	if a.Phase != b.Phase {
		return "Phase: " + a.Phase + " vs " + b.Phase
	}
	if a.StatusCode != b.StatusCode {
		return fmt.Sprintf("StatusCode: %d vs %d", a.StatusCode, b.StatusCode)
	}
	if !reflect.DeepEqual(a.Error, b.Error) {
		return "Error: " + jsonPretty(a.Error) + " vs " + jsonPretty(b.Error)
	}
	if !invocationsEqual(a.Invocations, b.Invocations) {
		return "Invocations differ:\n  a=" + jsonPretty(a.Invocations) + "\n  b=" + jsonPretty(b.Invocations)
	}
	if !reflect.DeepEqual(a.PluginKeys, b.PluginKeys) {
		return "PluginKeys: " + jsonPretty(a.PluginKeys) + " vs " + jsonPretty(b.PluginKeys)
	}
	// Compare per-plugin JSON payloads structurally to tolerate
	// whitespace and map-order differences between listeners.
	keys := make([]string, 0, len(a.PluginEventJSON))
	for k := range a.PluginEventJSON {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !jsonEqual(a.PluginEventJSON[k], b.PluginEventJSON[k]) {
			return "PluginEventJSON[" + k + "]: " + a.PluginEventJSON[k] + " vs " + b.PluginEventJSON[k]
		}
	}
	return ""
}

// invocationsEqual compares two slices as sets so multi-plugin
// fixtures tolerate independent-gate ordering.
func invocationsEqual(a, b []invocationSummary) bool {
	if len(a) != len(b) {
		return false
	}
	byKey := func(s []invocationSummary) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].Plugin != s[j].Plugin {
				return s[i].Plugin < s[j].Plugin
			}
			if s[i].Action != s[j].Action {
				return s[i].Action < s[j].Action
			}
			return s[i].Reason < s[j].Reason
		})
	}
	as := append([]invocationSummary(nil), a...)
	bs := append([]invocationSummary(nil), b...)
	byKey(as)
	byKey(bs)
	return reflect.DeepEqual(as, bs)
}

func jsonPretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}

// jsonOf marshals fixture-time data (bodyObservation etc.) to its
// SessionEvent.Plugins JSON. Matches spyEntry's swallow-error pattern.
func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// jsonEqual compares two raw JSON strings structurally so map-order
// and whitespace differences between listeners register as equal.
func jsonEqual(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

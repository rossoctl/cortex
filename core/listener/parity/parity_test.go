// Package parity runs the same fixture through both the extproc and
// HTTP-proxy listeners and asserts the resulting session event is
// identical, catching silent drift between the two deployment shapes.
package parity

import (
	"crypto/sha256"
	"encoding/hex"
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
//
// The fixture authenticates and emits a plugin event before it denies,
// which is not decoration. Until it did, the denial event under test was
// the emptiest one this suite could build — no identity, no plugin
// payload, no asserted duration — so the comparison reduced to the fields
// every listener happened to agree on, and #936's three dropped fields
// (Plugins, Identity, Duration on the reverse proxy) compared nil to nil.
// A parity fixture only covers what it populates.
func TestParity_DenyOnRequest(t *testing.T) {
	f := fixture{
		name:      "deny-on-request",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			DenyOnRequest: true,
			DenyStatus:    429,
			DenyReason:    "spy.denied",
			DenyDetails:   map[string]string{"cutoff_reason": "quota"},
			Subject:       "alice@example.org",
			ClientID:      "weather-agent",
			Scopes:        []string{"openid", "weather.read"},
			EmitOnRequest: true,
			RequestEvent:  &spyEvent{Marker: "denied", Count: 1},
		})},
		method: "GET",
		path:   "/parity/deny",
		expectedPluginEvents: map[string]string{
			spyPluginA: jsonOf(spyEvent{Marker: "denied", Count: 1}),
		},
		expectedIdentity: &identitySummary{
			Subject:  "alice@example.org",
			ClientID: "weather-agent",
			Scopes:   []string{"openid", "weather.read"},
		},
		expectDuration: true,
	}
	assertParity(t, f, pipeline.SessionDenied, inboundListeners)
}

// TestParity_ResponseEventEmission: an OnResponse emit to Extensions.
// Custom must surface identically on SessionEvent.Plugins from both
// listeners.
//
// Bodyless deliberately. The fixture's claim is about the plugin event, and a
// headers-only plugin sees the same OnResponse either way — but with a body
// present and no plugin reading it, the listeners no longer agree on
// BytesDown: the proxies relay that body through their own process and count
// it (their row is recorded after the relay, #1251's fix), while extproc asked
// Envoy for body mode NONE and never received the bytes at all. That split is
// structural — closing it would mean making extproc request bodies no plugin
// needs — so it is pinned with its reason in divergence_test.go
// (TestDivergence_InboundResponseBodyCountWhenNoPluginReadsIt) rather than
// silently failing here. Dropping the body drops only the divergent figure;
// the emit fires in OnResponse either way, and the absolute anchor below keeps
// the bodyless zero meaning "no bytes", not "no reader".
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
		expectedBytes:  &bytesSummary{},
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
			Subject:       "alice@example.org",
			ClientID:      "weather-agent",
			Scopes:        []string{"openid", "llm.invoke"},
			EmitOnRequest: true,
			RequestEvent:  &spyEvent{Marker: "blocked", Count: 1},
		})},
		method: "GET",
		path:   "/parity/egress",
		expectedPluginEvents: map[string]string{
			spyPluginA: jsonOf(spyEvent{Marker: "blocked", Count: 1}),
		},
		expectedIdentity: &identitySummary{
			Subject:  "alice@example.org",
			ClientID: "weather-agent",
			Scopes:   []string{"openid", "llm.invoke"},
		},
		expectDuration: true,
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

// TestBytesParity_CountsAreTheWholeBody pins what the BYTES column says, as an
// absolute figure on every listener (#1309). The pairwise diff is a weak check for
// this one field: zero is the default, so three listeners that all count nothing
// agree — which is exactly the state the column was in before this change.
//
// What each case pins:
//
//   - the buffered pair, both directions: the request row reports the body as
//     FORWARDED and the response row the body as RECEIVED, each on its own row and
//     neither on the other's. A listener that set both on one row would read as a
//     round trip that never happened.
//   - SSE: the WIRE bytes, so 137 and not the 105 bytes of payload the pipeline
//     sees after sseframe strips the framing — and not the trailing chunk either,
//     which is all extproc's response buffer holds by the time the row is
//     recorded. The two wrong answers available here differ from the right one by
//     specific numbers, which is why this is stated absolutely.
//   - the body-less GET: zero on both sides, which agentop renders blank. Pinned
//     because "nothing was counted" and "the body was empty" have to stay the same
//     state as far as the event is concerned — there is no third value for
//     "unknown", and a listener inventing a length for a request that carried none
//     would be worse than the blank.
func TestBytesParity_CountsAreTheWholeBody(t *testing.T) {
	reqBody := []byte(`{"prompt":"hello"}`)
	respBody := []byte(`{"reply":"ok"}`)

	// One shape, run four times: each direction × each phase. The counts are
	// per-phase, so a fixture cannot assert both halves at once.
	//
	// The two Record flags are load-bearing and not boilerplate: extproc appends a
	// session event only when the pipeline left something worth recording on pctx
	// (A2A, an Invocation, or a plugin-public Custom entry — recordInboundSession's
	// gate), while the proxies record unconditionally. A spy with ReadsBody alone
	// publishes nothing, so the extproc leg produces no row at all and the fixture
	// fails on presence rather than on the figures it is here to pin. Giving the
	// spy something to publish is the cheapest way to get both legs recording; it
	// does not touch the byte counts, which the listener takes off the wire.
	buffered := func(name string, dir pipeline.Direction, want *bytesSummary) fixture {
		return fixture{
			name:      "bytes-buffered-" + name,
			direction: dir,
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
			expectedBytes:  want,
		}
	}
	for _, tc := range []struct {
		name      string
		dir       pipeline.Direction
		listeners []listenerRun
	}{
		{"inbound", pipeline.Inbound, inboundListeners},
		{"outbound", pipeline.Outbound, outboundListeners},
	} {
		assertParity(t, buffered(tc.name+"-request", tc.dir,
			&bytesSummary{Up: int64(len(reqBody))}), pipeline.SessionRequest, tc.listeners)
		assertParity(t, buffered(tc.name+"-response", tc.dir,
			&bytesSummary{Down: int64(len(respBody))}), pipeline.SessionResponse, tc.listeners)
	}

	// Same four events as TestParity_ReadsBodySSE, re-framed here rather than
	// shared: that fixture's anchor is the payload the PLUGIN sees, this one's is
	// the wire, and the whole point of the case is that the two differ.
	var sse strings.Builder
	for _, e := range []string{
		`{"type":"message_start"}`,
		`{"type":"message_delta","usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
		`[DONE]`,
	} {
		sse.WriteString("data: ")
		sse.WriteString(e)
		sse.WriteString("\n\n")
	}
	assertParity(t, fixture{
		name:      "bytes-sse",
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
		expectedBytes:       &bytesSummary{Down: int64(sse.Len())},
	}, pipeline.SessionResponse, inboundListeners)

	// A GET with no body on either side. ReadsBody still set, so the zero is the
	// absence of bytes and not the absence of a reader.
	assertParity(t, fixture{
		name:      "bytes-none",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginAStreaming, spyConfig{
			ReadsBody:         true,
			RecordRequestBody: true,
		})},
		method:         "GET",
		path:           "/parity/empty",
		upstreamStatus: 204,
		expectedBytes:  &bytesSummary{},
	}, pipeline.SessionRequest, inboundListeners)
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

// TestParity_RequestBodyMutation: a plugin that rewrites the request body
// must put ITS bytes — not the client's — on the wire to the upstream, with a
// Content-Length that agrees with them, on every deployment shape.
//
// The suite could compare plugin-visible bodies before this (ReadsBody) but
// never the bytes a listener forwarded, and the forwarding paths have nothing
// in common: the proxies swap Request.Body and re-stamp the header in Go,
// extproc returns an Envoy BodyMutation proto with a content-length
// SetHeaders entry beside it. A rewrite dropped on one of them is silent in
// every direction — the plugin's SetBody returns, the modify Invocation and
// the body-mutation event are recorded identically on both listeners, and the
// upstream simply acts on the original request. tool-prune ships on this
// contract today, so "the oversized tool manifest was pruned" and "the
// upstream received the pruned manifest" were two different claims and only
// the first one was tested here.
//
// Three things are pinned together, and the combination is the point:
//
//   - /req-body is the ORIGINAL body, proving the plugin was handed the
//     client's bytes rather than its own.
//   - body-mutation/event is the framework's own account of the rewrite,
//     asserted as exact JSON including both digests, so a listener that
//     publishes a mutation it did not perform disagrees with the wire.
//   - expectedUpstream is the rewrite as the upstream actually received it.
//
// Asserting the wire alone would pass for a listener that forwarded correct
// bytes under a stale length; asserting the event alone is what already
// passed while the question was open.
func TestParity_RequestBodyMutation(t *testing.T) {
	reqBody := []byte(`{"prompt":"hello","tools":["alpha","beta","gamma"]}`)
	mutated := []byte(`{"prompt":"hello","tools":["alpha"]}`)
	f := fixture{
		name:      "request-body-mutation",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			RecordRequestBody: true,
			MutateRequestBody: mutated,
		})},
		method:         "POST",
		path:           "/parity/mutate",
		reqBody:        reqBody,
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
		expectedPluginEvents: map[string]string{
			spyPluginA + bodyReqStrippedSuffix: jsonOf(bodyObservation{Body: string(reqBody)}),
			bodyMutationKey:                    bodyMutationJSON(spyPluginA, reqBody, mutated),
		},
		expectedUpstream: &upstreamSummary{
			Body:          string(mutated),
			ContentLength: fmt.Sprintf("%d", len(mutated)),
		},
	}
	// SessionRequest, not SessionResponse: the rewrite is a request-phase
	// event, so this is the phase that carries both the modify Invocation and
	// the body-mutation payload. On the response event they are out of scope
	// and the fixture would assert the rewrite against the half of the
	// timeline that never saw it.
	assertParity(t, f, pipeline.SessionRequest, inboundListeners)
}

// TestParity_OutboundRequestBodyMutation is the egress mirror, and the shape
// that actually runs in production: tool-prune and context-guru both sit on
// the outbound pipeline, rewriting an agent's request on its way to the model
// gateway. extproc vs forwardproxy here is envoy-sidecar vs proxy-sidecar —
// the latter being the laptop install and the Kubernetes default both, so these
// are the two shapes a prune has to behave the same in.
func TestParity_OutboundRequestBodyMutation(t *testing.T) {
	reqBody := []byte(`{"model":"claude","tools":["alpha","beta","gamma"]}`)
	mutated := []byte(`{"model":"claude","tools":["alpha"]}`)
	f := fixture{
		name:      "outbound-request-body-mutation",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			RecordRequestBody: true,
			MutateRequestBody: mutated,
		})},
		method:         "POST",
		path:           "/parity/mutate",
		reqBody:        reqBody,
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
		expectedPluginEvents: map[string]string{
			spyPluginA + bodyReqStrippedSuffix: jsonOf(bodyObservation{Body: string(reqBody)}),
			bodyMutationKey:                    bodyMutationJSON(spyPluginA, reqBody, mutated),
		},
		expectedUpstream: &upstreamSummary{
			Body:          string(mutated),
			ContentLength: fmt.Sprintf("%d", len(mutated)),
		},
	}
	assertParity(t, f, pipeline.SessionRequest, outboundListeners)
}

// TestParity_OutboundChainedRequestBodyMutation: two request-body writers in one
// chain, as tool-prune and the inference router are on the laptop. The second is
// handed the first's bytes, the upstream receives the second's, and the
// framework's record keeps the client's bytes as before and names both writers —
// on both outbound deployment shapes.
func TestParity_OutboundChainedRequestBodyMutation(t *testing.T) {
	reqBody := []byte(`{"model":"claude","tools":["alpha","beta","gamma"]}`)
	pruned := []byte(`{"model":"claude","tools":["alpha"]}`)
	remodelled := []byte(`{"model":"glm","tools":["alpha"]}`)
	f := fixture{
		name:      "outbound-chained-request-body-mutation",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{
			spyEntry(spyPluginA, spyConfig{RecordRequestBody: true, MutateRequestBody: pruned}),
			spyEntry(spyPluginB, spyConfig{RecordRequestBody: true, MutateRequestBody: remodelled}),
		},
		method:         "POST",
		path:           "/parity/mutate-chained",
		reqBody:        reqBody,
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
		expectedPluginEvents: map[string]string{
			spyPluginA + bodyReqStrippedSuffix: jsonOf(bodyObservation{Body: string(reqBody)}),
			spyPluginB + bodyReqStrippedSuffix: jsonOf(bodyObservation{Body: string(pruned)}),
			bodyMutationKey:                    chainedBodyMutationJSON([]string{spyPluginA, spyPluginB}, reqBody, remodelled),
		},
		expectedUpstream: &upstreamSummary{
			Body:          string(remodelled),
			ContentLength: fmt.Sprintf("%d", len(remodelled)),
		},
	}
	assertParity(t, f, pipeline.SessionRequest, outboundListeners)
}

// TestParity_InboundChainedRequestBodyMutation: the same two chained writers on
// the inbound chain, through ext_proc and the reverse proxy. Chaining is the
// framework's, not one listener's, so the inbound shapes must hand the second
// writer the first's bytes, send the second's, and keep the same record.
func TestParity_InboundChainedRequestBodyMutation(t *testing.T) {
	reqBody := []byte(`{"model":"claude","tools":["alpha","beta","gamma"]}`)
	pruned := []byte(`{"model":"claude","tools":["alpha"]}`)
	remodelled := []byte(`{"model":"glm","tools":["alpha"]}`)
	f := fixture{
		name:      "inbound-chained-request-body-mutation",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{
			spyEntry(spyPluginA, spyConfig{RecordRequestBody: true, MutateRequestBody: pruned}),
			spyEntry(spyPluginB, spyConfig{RecordRequestBody: true, MutateRequestBody: remodelled}),
		},
		method:         "POST",
		path:           "/parity/mutate-chained",
		reqBody:        reqBody,
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
		expectedPluginEvents: map[string]string{
			spyPluginA + bodyReqStrippedSuffix: jsonOf(bodyObservation{Body: string(reqBody)}),
			spyPluginB + bodyReqStrippedSuffix: jsonOf(bodyObservation{Body: string(pruned)}),
			bodyMutationKey:                    chainedBodyMutationJSON([]string{spyPluginA, spyPluginB}, reqBody, remodelled),
		},
		expectedUpstream: &upstreamSummary{
			Body:          string(remodelled),
			ContentLength: fmt.Sprintf("%d", len(remodelled)),
		},
	}
	assertParity(t, f, pipeline.SessionRequest, inboundListeners)
}

// TestParity_RequestBodyMutationToEmpty: a rewrite DOWN TO ZERO BYTES, which
// is a distinct wire shape rather than an edge case for its own sake — it is
// what a truncating plugin emits at its limit, and it is the one mutation that
// cannot be told apart from "no mutation happened" by looking at the body
// alone. Both halves have to carry it: zero bytes AND a Content-Length of 0,
// against a request that arrived with a body. A listener that treats an empty
// replacement as nothing to do leaves the original body and its original
// length on the wire, and every other assertion in this suite still passes.
func TestParity_RequestBodyMutationToEmpty(t *testing.T) {
	reqBody := []byte(`{"prompt":"hello"}`)
	mutated := []byte{}
	f := fixture{
		name:      "request-body-mutation-to-empty",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			RecordRequestBody: true,
			MutateRequestBody: mutated,
		})},
		method:         "POST",
		path:           "/parity/mutate-empty",
		reqBody:        reqBody,
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
		expectedPluginEvents: map[string]string{
			spyPluginA + bodyReqStrippedSuffix: jsonOf(bodyObservation{Body: string(reqBody)}),
			bodyMutationKey:                    bodyMutationJSON(spyPluginA, reqBody, mutated),
		},
		expectedUpstream: &upstreamSummary{Body: "", ContentLength: "0"},
	}
	assertParity(t, f, pipeline.SessionRequest, inboundListeners)
}

// bodyMutationKey is the SessionEvent.Plugins key the framework publishes a
// body rewrite under. Not a plugin name: pipeline.Context.emitBodyMutation
// uses a fixed synthetic prefix precisely so the key survives plugin renames,
// and spelling it once here keeps the fixtures honest about that.
const bodyMutationKey = "body-mutation"

// bodyMutationJSON is the exact payload the framework publishes for one
// rewrite. Built here rather than copied from pipeline's unexported
// bodyMutationEvent struct, which means the field NAMES are asserted too — an
// operator dashboard reads these keys, so a rename is a breaking change and
// should fail something.
//
// The digests are recomputed from the fixture's own bytes, so they assert the
// framework hashed the bodies it claims to have hashed. The point of carrying
// them at all is that the raw bodies must NOT travel here: the session API has
// no auth, so a before/after digest is how a rewrite stays auditable without
// publishing whatever credential the body held.
func bodyMutationJSON(plugin string, before, after []byte) string {
	return chainedBodyMutationJSON([]string{plugin}, before, after)
}

// chainedBodyMutationJSON is the payload for a chain of rewrites: before is the
// body the client sent, after the body sent upstream, plugins every writer in
// order, and plugin the last of them.
func chainedBodyMutationJSON(plugins []string, before, after []byte) string {
	names, _ := json.Marshal(plugins)
	return fmt.Sprintf(`{"phase":"request","plugin":%q,"plugins":%s,"length_before":%d,"length_after":%d,"sha256_before":%q,"sha256_after":%q}`,
		plugins[len(plugins)-1], names, len(before), len(after), sha256Hex(before), sha256Hex(after))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

// --- multi-plugin pipelines ---------------------------------------------
//
// Every fixture above this point runs ONE plugin, and a one-plugin
// pipeline cannot exercise the three framework contracts that only exist
// between plugins: invocation order, event merging across keys, and
// error-policy composition. Production pipelines are never one plugin —
// jwt-validation then a parser then a cost plugin is the shape the
// operator actually runs — so each of these was asserted only in
// per-listener unit tests, never across listeners.

// TestParity_MultiPluginInvocationOrder is #948's fixture: two plugins on
// one request, one observing and one denying, with the invocation list
// pinned exactly and in order on every listener.
//
// B runs first and observes; A then denies, which stops the pipeline. The
// recorded order must therefore be [B observe, A deny] — the order the
// plugins ran, not the order they are named and not alphabetical. Both
// spies also emit their own Extensions.Custom event, so the fixture pins
// a two-key SessionEvent.Plugins map: with a single plugin the map has one
// key, and a listener that overwrote the map instead of merging into it
// would look identical.
func TestParity_MultiPluginInvocationOrder(t *testing.T) {
	f := fixture{
		name:      "multi-plugin-invocation-order",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{
			spyEntry(spyPluginB, spyConfig{
				EmitOnRequest:     true,
				RequestEvent:      &spyEvent{Marker: "observed", Count: 1},
				RequestInvocation: &invocationRecord{Action: "observe", Reason: "spy.observed"},
			}),
			spyEntry(spyPluginA, spyConfig{
				DenyOnRequest: true,
				DenyStatus:    403,
				DenyReason:    "spy.denied",
				DenyDetails:   map[string]string{"cutoff_reason": "policy"},
				Subject:       "alice@example.org",
				ClientID:      "weather-agent",
				EmitOnRequest: true,
				RequestEvent:  &spyEvent{Marker: "denied", Count: 2},
			}),
		},
		method: "GET",
		path:   "/parity/multi",
		expectedInvocations: []invocationSummary{
			{Plugin: spyPluginB, Action: "observe", Reason: "spy.observed"},
			{Plugin: spyPluginA, Action: "deny", Reason: "spy.denied", Details: map[string]string{"cutoff_reason": "policy"}},
		},
		expectedPluginEvents: map[string]string{
			spyPluginB: jsonOf(spyEvent{Marker: "observed", Count: 1}),
			spyPluginA: jsonOf(spyEvent{Marker: "denied", Count: 2}),
		},
		expectedIdentity: &identitySummary{
			Subject:  "alice@example.org",
			ClientID: "weather-agent",
		},
		expectDuration: true,
	}
	assertParity(t, f, pipeline.SessionDenied, inboundListeners)
}

// TestParity_OutboundMultiPluginInvocationOrder mirrors the above on the
// egress side. Not redundant: forwardproxy is a separate recorder from
// reverseproxy — #936's dropped fields were on the proxies and had to be
// found twice — and the outbound bucket is a different slice
// (Invocations.Outbound), filled by the same append but snapshotted by
// different code.
func TestParity_OutboundMultiPluginInvocationOrder(t *testing.T) {
	f := fixture{
		name:      "outbound-multi-plugin-invocation-order",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{
			spyEntry(spyPluginB, spyConfig{
				EmitOnRequest:     true,
				RequestEvent:      &spyEvent{Marker: "observed", Count: 1},
				RequestInvocation: &invocationRecord{Action: "observe", Reason: "spy.observed"},
			}),
			spyEntry(spyPluginA, spyConfig{
				DenyOnRequest: true,
				DenyStatus:    403,
				DenyReason:    "spy.blocked",
				DenyDetails:   map[string]string{"cutoff_reason": "egress-policy"},
				Subject:       "alice@example.org",
				ClientID:      "weather-agent",
				EmitOnRequest: true,
				RequestEvent:  &spyEvent{Marker: "blocked", Count: 2},
			}),
		},
		method: "GET",
		path:   "/parity/multi-egress",
		expectedInvocations: []invocationSummary{
			{Plugin: spyPluginB, Action: "observe", Reason: "spy.observed"},
			{Plugin: spyPluginA, Action: "deny", Reason: "spy.blocked", Details: map[string]string{"cutoff_reason": "egress-policy"}},
		},
		expectedPluginEvents: map[string]string{
			spyPluginB: jsonOf(spyEvent{Marker: "observed", Count: 1}),
			spyPluginA: jsonOf(spyEvent{Marker: "blocked", Count: 2}),
		},
		expectedIdentity: &identitySummary{
			Subject:  "alice@example.org",
			ClientID: "weather-agent",
		},
		expectDuration: true,
	}
	assertParity(t, f, pipeline.SessionDenied, outboundListeners)
}

// TestParity_MultiPluginRequiresLaterRuntimeOrder pins the RUNTIME half of
// the dependency contract. TestParity_RequiresLaterOrderingRejected proves
// construction rejects a wrong-order pipeline; it says nothing about what
// a well-ordered one then does, because it never runs one. That gap
// matters because the whole point of RequiresLater is that the dependent
// plugin observes state the earlier one left behind — a pipeline that
// validated the declaration at build time and then dispatched in some
// other order would satisfy the existing test completely.
//
// Neither spy denies, so this is the success path: the request reaches the
// upstream and both invocations land on the request event, in declaration
// order [A, B].
func TestParity_MultiPluginRequiresLaterRuntimeOrder(t *testing.T) {
	f := fixture{
		name:      "multi-plugin-requires-later-runtime-order",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{
			spyEntry(spyPluginA, spyConfig{
				RequiresLater:     []string{spyPluginB},
				RequestInvocation: &invocationRecord{Action: "allow", Reason: "spy.a.ran"},
			}),
			spyEntry(spyPluginB, spyConfig{
				RequestInvocation: &invocationRecord{Action: "allow", Reason: "spy.b.ran"},
			}),
		},
		method:         "GET",
		path:           "/parity/ordered",
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
		expectedInvocations: []invocationSummary{
			{Plugin: spyPluginA, Action: "allow", Reason: "spy.a.ran"},
			{Plugin: spyPluginB, Action: "allow", Reason: "spy.b.ran"},
		},
	}
	// SessionRequest, not SessionResponse: both spies record at OnRequest,
	// and Invocation.Phase makes the listener put request-phase entries on
	// the request event only. Asserting on the response event would
	// compare two empty slices and pass whatever the order was.
	assertParity(t, f, pipeline.SessionRequest, inboundListeners)
}

// TestParity_MultiPluginShadowComposition covers the fourth contract: a
// plugin under on_error: observe sitting in front of an enforcing one.
//
// B's Reject is downgraded — Pipeline.Run flips its deny record's Shadow
// flag and continues — and A then denies for real. So one request produces
// two deny records that differ ONLY in Shadow, and the request is refused
// with A's status, not B's. Both facts must survive identically on both
// listeners: a listener that dropped Shadow would report a shadow-mode
// rollout as an outage, and one that let B's downgraded Reject terminate
// the pipeline would never reach A at all.
func TestParity_MultiPluginShadowComposition(t *testing.T) {
	f := fixture{
		name:      "multi-plugin-shadow-composition",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{
			spyEntryShadow(spyPluginB, spyConfig{
				DenyOnRequest: true,
				DenyStatus:    429,
				DenyReason:    "spy.would.deny",
				DenyDetails:   map[string]string{"cutoff_reason": "rollout"},
				EmitOnRequest: true,
				RequestEvent:  &spyEvent{Marker: "shadow", Count: 1},
			}),
			spyEntry(spyPluginA, spyConfig{
				DenyOnRequest: true,
				DenyStatus:    403,
				DenyReason:    "spy.denied",
				EmitOnRequest: true,
				RequestEvent:  &spyEvent{Marker: "enforced", Count: 2},
			}),
		},
		method:             "GET",
		path:               "/parity/shadow",
		expectedWireStatus: 403, // A's status — proof B's shadow deny did not terminate the request
		expectedInvocations: []invocationSummary{
			{Plugin: spyPluginB, Action: "deny", Reason: "spy.would.deny", Details: map[string]string{"cutoff_reason": "rollout"}, Shadow: true},
			{Plugin: spyPluginA, Action: "deny", Reason: "spy.denied"},
		},
		expectedPluginEvents: map[string]string{
			spyPluginB: jsonOf(spyEvent{Marker: "shadow", Count: 1}),
			spyPluginA: jsonOf(spyEvent{Marker: "enforced", Count: 2}),
		},
		expectDuration: true,
	}
	assertParity(t, f, pipeline.SessionDenied, inboundListeners)
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
		if f.expectedIdentity != nil {
			if g.observed.Identity == nil {
				t.Errorf("fixture %q listener %s: event carried no Identity; the caller authenticated as %q and the event cannot be attributed to them", f.name, g.listener, f.expectedIdentity.Subject)
			} else if !reflect.DeepEqual(g.observed.Identity, f.expectedIdentity) {
				t.Errorf("fixture %q listener %s: Identity\n  got:  %s\n  want: %s", f.name, g.listener, jsonPretty(g.observed.Identity), jsonPretty(f.expectedIdentity))
			}
		}
		if f.expectDuration && !g.observed.HasDuration {
			t.Errorf("fixture %q listener %s: event carried no Duration; the request completed, so it has one", f.name, g.listener)
		}
		if f.expectedInvocations != nil && !reflect.DeepEqual(g.observed.Invocations, f.expectedInvocations) {
			t.Errorf("fixture %q listener %s: Invocations (exact, ordered)\n  got:  %s\n  want: %s", f.name, g.listener, jsonPretty(g.observed.Invocations), jsonPretty(f.expectedInvocations))
		}
		if f.expectedBytes != nil {
			got := bytesSummary{Up: g.observed.BytesUp, Down: g.observed.BytesDown}
			if got != *f.expectedBytes {
				t.Errorf("fixture %q listener %s: bytes counted\n  got:  %+v\n  want: %+v", f.name, g.listener, got, *f.expectedBytes)
			}
		}
		if f.expectedUpstream != nil {
			switch {
			case g.observed.Upstream == nil:
				t.Errorf("fixture %q listener %s: nothing reached the upstream, so the request the fixture pins was never forwarded", f.name, g.listener)
			case !reflect.DeepEqual(g.observed.Upstream, f.expectedUpstream):
				t.Errorf("fixture %q listener %s: upstream received\n  got:  %s\n  want: %s", f.name, g.listener, jsonPretty(g.observed.Upstream), jsonPretty(f.expectedUpstream))
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
	// One listener attributing the request while another publishes an
	// anonymous event for the same authenticated caller. Same shared-gap
	// blind spot as Inference below, covered by fixture.expectedIdentity.
	if !reflect.DeepEqual(a.Identity, b.Identity) {
		return "Identity: " + jsonPretty(a.Identity) + " vs " + jsonPretty(b.Identity)
	}
	if a.HasDuration != b.HasDuration {
		return fmt.Sprintf("HasDuration: %v vs %v", a.HasDuration, b.HasDuration)
	}
	// One listener reporting a body size the next one disagrees with. Values, not
	// presence — see the observation fields. The same caveat as Inference below
	// applies and is why TestBytesParity_CountsAreTheWholeBody states the figures
	// absolutely as well: two listeners that both count nothing agree here, so a gap
	// shared by every leg of a fixture passes this check. What it catches is the
	// split, which is the likelier failure given that each listener counts from
	// different machinery.
	if a.BytesUp != b.BytesUp {
		return fmt.Sprintf("BytesUp: %d vs %d", a.BytesUp, b.BytesUp)
	}
	if a.BytesDown != b.BytesDown {
		return fmt.Sprintf("BytesDown: %d vs %d", a.BytesDown, b.BytesDown)
	}
	// One listener putting a plugin's rewrite on the wire while the other
	// forwards the original bytes — the split case of the gap
	// fixture.expectedUpstream covers absolutely, and the one no session-event
	// field can show, since both listeners record the same successful modify
	// Invocation either way. Nil on both legs when a fixture made no upstream
	// claim, so this is inert for every fixture that did not opt in.
	if !reflect.DeepEqual(a.Upstream, b.Upstream) {
		return "Upstream: " + jsonPretty(a.Upstream) + " vs " + jsonPretty(b.Upstream)
	}
	// One listener reporting token counts while another does not. Worth saying what
	// this check CANNOT do, because that is how the gap it was added for survived: two
	// listeners that both record nothing agree, so a gap shared by a direction's whole
	// listener set passes here. The inbound pair did exactly that. The absolute
	// expectations in cost_parity_test.go cover the shared case on BOTH phases — the
	// response event's counts, and the request event's deliberate zeros — while this
	// covers the split. Neither is sufficient alone, and the phase qualifier is
	// load-bearing: while the absolute check ran on the response event only, the two
	// request-phase recorders rode on this comparison alone and could be deleted
	// without a failure anywhere.
	if !reflect.DeepEqual(a.Inference, b.Inference) {
		return "Inference: " + jsonPretty(a.Inference) + " vs " + jsonPretty(b.Inference)
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

// invocationsEqual compares two slices IN ORDER.
//
// It used to sort both sides first, on the stated grounds that
// "multi-plugin fixtures tolerate independent-gate ordering". There were
// no multi-plugin runtime fixtures when that was written, and now that
// there are, the tolerance has nothing to justify it: every listener runs
// the same Pipeline over the same plugin slice, and both the request pass
// (declaration order) and the response pass (reverse) are deterministic,
// so two legs of one fixture have no licence to disagree about order.
// Sorting first meant [B, A] compared equal to [A, B] — the exact
// divergence #948 asked this suite to catch, discarded before the
// comparison.
//
// Length is still checked separately so the message distinguishes a
// listener that dropped a record from one that reordered them.
func invocationsEqual(a, b []invocationSummary) bool {
	return len(a) == len(b) && reflect.DeepEqual(a, b)
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

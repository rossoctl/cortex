package parity

// Divergence fixtures: the same request through both shapes of one direction,
// where the shapes are expected to answer DIFFERENTLY.
//
// Everything else in this package asserts the opposite, and that is the point
// of keeping these apart. The body-size caps are not the same number on every
// listener:
//
//	direction      extproc    reverseproxy   forwardproxy
//	request body   1 MiB      1 MiB          32 MiB
//	response body  1 MiB      1 MiB          10 MiB
//
// WHICH DEPLOYMENT SHAPE EACH LISTENER IS, because the axis is not the obvious
// one. extproc is envoy-sidecar mode. BOTH proxies are proxy-sidecar mode —
// which is the laptop install AND the Kubernetes default: the operator falls
// back to proxy-sidecar when nothing sets a mode (its AgentRuntime CRD
// documents "proxy-sidecar ... Default mode." for Spec.AuthBridgeMode), and the
// chart's authbridge-runtime-config template deliberately leaves `mode:` unset
// so that cluster default wins. So everything below is envoy-sidecar against
// proxy-sidecar, NOT laptop against Kubernetes: an agent on a default
// Kubernetes install egresses through the same forward proxy a laptop does, and
// only a workload that OPTS IN to envoy-sidecar meets extproc's caps at all.
//
// So a body between 1 MiB and 32 MiB is served in proxy-sidecar mode and
// refused in envoy-sidecar mode, and an Anthropic Claude Code session produces
// 5 MiB request bodies (the 5,250,133-byte request measured in #864) — the band
// is where real traffic lives, not a corner. Until these fixtures existed
// nothing stated which way each shape goes, so neither answer was a decision
// and either could change silently.
//
// TWO ENVOY CONFIGS, AND EVERY extproc ROW BELOW SAYS WHICH ONE IT DESCRIBES.
// The caps in that table are the LISTENER's, and in envoy-sidecar mode they are
// not the first limit a large body meets. ext_proc asks Envoy for BUFFERED
// bodies in both directions, and neither the chart's envoy-config nor the
// operator's envoy.yaml.tmpl raises per_connection_buffer_limit_bytes or
// per_request_buffer_limit_bytes — so Envoy's own 1 MiB default fires first,
// and the listener is handed no body to apply its cap to:
//
//	                   shipped (Envoy's 1 MiB default)     buffer limit raised past the body
//	request over cap   413 from Envoy; ext_proc is never   413 from the listener's own check
//	                   called and records NO row           (Process's RequestBody case)
//	response over cap  500 from Envoy; ext_proc sees       appendBoundedBody truncates to
//	                   ResponseHeaders without             1 MiB, logs a WARN, and the
//	                   end_of_stream then EOF, and         pipeline runs on the prefix
//	                   records 200 with an EMPTY body
//
// A THIRD CEILING BOUNDS THE RIGHT-HAND COLUMN ONLY, and getting that wrong is
// the obvious mistake: it sits BEHIND Envoy's buffer limit, not in front of it.
// cmd/cortex-envoy's newExtProcServer builds its ext_proc server with a bare
// grpc.NewServer() and passes no options, so grpc-go's 4 MiB default
// MaxRecvMsgSize applies to every ProcessingRequest Envoy sends. On the SHIPPED
// config nothing can reach it: Envoy buffers at most 1 MiB, so the message it
// sends cannot approach 4 MiB, and #864's 5 MiB request gets Envoy's own 413
// exactly as the left column says. What the gRPC limit changes is the right
// column — once Envoy's buffer limit is raised past the body, the wall moves
// from 1 MiB to 4 MiB rather than away. A body over 4 MiB is then refused by
// the gRPC transport itself: the stream dies ResourceExhausted, the listener's
// own check never runs, and Envoy answers the client an EMPTY 500. So the
// operator who raises the buffer limit for #864's measured request does not get
// the 413 this file's first fixture pins — and does not get the
// ResourceExhausted either, which only ext_proc ever sees.
//
// Which direction the body was in decides what the store is left holding, and
// the answer is not symmetric. An over-limit REQUEST leaves no row at all, the
// same shape the 413 produces and for the same reason — both refuse before a
// pipeline.Context exists. An over-limit RESPONSE books an ordinary 200 with
// the body reported as zero bytes, because by then the request phase has run
// and Process's teardown flush fires. The client got the empty 500 either way,
// so that 200 is not merely incomplete: it is a THIRD mechanism producing
// exactly the contradiction the last two fixtures below pin for Envoy's own
// refusal — the event says the call succeeded while the caller was told it
// failed — and on the outbound leg it is again a request whose spend silently
// becomes nothing.
//
// TestExtprocGRPCReceiveLimit pins all of that — both ceilings, both
// directions, and the rows each leaves behind. The fixtures below cannot,
// because the parity drivers call Server.Process directly (see mockStream) and
// there is no gRPC transport in them to have a limit. The 2 MiB bodies they use
// sit under it, which is what keeps the listener's own cap the thing being
// exercised. The "no options" claim about newExtProcServer is not pinned from
// this module at all — core cannot import package main — so it is pinned beside
// the call site instead, by cmd/cortex-envoy's
// TestStartGRPCExtProcKeepsDefaultRecvLimit, with
// TestExtProcServerIsTheOnlyGRPCServerConstruction keeping that the only
// construction site there is to pin.
//
// The first three fixtures pin the RIGHT column — the only config in which the
// listener's own cap is reachable at all, and therefore the only one that can
// pin appendBoundedBody. The fourth and fifth pin the shipped response shape,
// via envoyAbortsResponseBody, one per direction. Both columns are worth
// having, and the left one is the worse failure: the session event claims a
// clean 200 while the client got a 500, so the event contradicts what the
// caller saw rather than merely omitting something.
//
// One gap has no fixture, because only one shape can express it: on the shipped
// envoy-sidecar config an over-cap REQUEST leaves no session row anywhere,
// Envoy having refused before ext_proc was called, while the forward proxy
// records a 413 proxy_error row of its own (recordUnbufferedRequest). An
// operator comparing the shapes sees a vanished request on one and a logged
// refusal on the other.
//
// Why a sibling of assertParity rather than a tolerance inside it. #987
// suggested a fixture field that makes assertParity accept per-listener
// answers. A field like that is an escape hatch: the next fixture that fails on
// real drift can be made green by filling it in, in a harness whose own
// comments exist because it once passed having compared nothing.
// assertDivergence instead never calls observationDiff, so there is no pairwise
// check to weaken — each leg is pinned absolutely against its own declared row
// — and four guards stop it being used for anything else: every listener needs
// a row, no row may name a listener outside the set being run, the rows must
// actually disagree, and each row has to say why.

import (
	"fmt"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

// divergentExpectation is one listener's whole expected answer. Every field is
// asserted on every row — there is no "unset means don't check", because a
// divergence fixture that left a field open would be hiding the field most
// likely to have moved.
type divergentExpectation struct {
	// why is required: one line on why THIS listener differs from its peer.
	// The complaint in #987 is that the gap is undocumented, so the
	// documentation is a run-time requirement rather than a comment someone
	// may forget to update.
	why string

	// pipelineRan is false when the listener refused before the pipeline. It is
	// also the SOLE declaration of that fact: assertDivergence derives the
	// fixture's refusedPreRunBy from these rows, so the driver is told to expect
	// no session event from this leg without a second place to keep in step.
	pipelineRan bool

	// wireStatus is the transport's code. Compared ALWAYS, including the 0 that
	// extproc reports on a success path for want of an HTTP transport of its
	// own — an explicit 0 in a fixture says "this shape has no wire answer
	// here", which is itself a divergence worth stating.
	wireStatus int

	// eventStatusCode is SessionEvent.StatusCode: what the session API, agentop
	// and /v1/usage will show an operator. It is NOT always wireStatus — on the
	// reverse proxy's buffer refusal the event carries the 502 while the
	// upstream's own 200 moves to error.code.
	eventStatusCode int

	// errorKind and errorCode are SessionEvent.Error, with "" asserting the
	// event carried no error at all. That empty case is the sharp one: the
	// silent truncation this file pins records a clean 200 with no error, so a
	// fixture that only looked at errors would see two legs agreeing.
	//
	// Message is deliberately not here. It is prose with byte counts in it, so
	// pinning it would make these fixtures fail on a reworded log line, and
	// Kind is the field consumers actually branch on.
	errorKind string
	errorCode string

	// pluginEvents is an EXACT set of SessionEvent.Plugins: every key listed
	// must be present and byte-equal, and any key not listed is a failure. nil
	// therefore asserts the event carried no plugin payload — which is the
	// whole claim on the reverse proxy's leg below, where OnResponse never ran.
	pluginEvents map[string]string

	// bytes, when non-nil, pins BytesUp/BytesDown on this listener's row — the
	// one field here that has to be optional against this struct's "every field
	// is asserted on every row" rule, because zero is itself a value a row may
	// want to pin (the extproc leg of the unread-body fixtures below claims
	// exactly ↓0), so a plain pair could not tell "assert nothing" from "assert
	// zero". A pointer, for the same reason fixture.expectedBytes is one.
	bytes *bytesSummary
}

// assertDivergence runs the fixture through every listener and checks each one
// against its own row. No pairwise comparison, by design — see the file comment.
func assertDivergence(t *testing.T, f fixture, wantPhase pipeline.SessionPhase, listeners []listenerRun, want map[string]divergentExpectation) {
	t.Helper()

	if len(listeners) < 2 {
		t.Fatalf("assertDivergence: fixture %q was given %d listener(s); a divergence needs at least two shapes to diverge between", f.name, len(listeners))
	}

	// Guard: a row for every listener, each with a reason. A missing row must
	// be fatal rather than defaulted, or the cheapest way to silence a failing
	// leg would be to delete its expectation.
	inSet := make(map[string]bool, len(listeners))
	for _, l := range listeners {
		inSet[l.name] = true
		e, ok := want[l.name]
		if !ok {
			t.Fatalf("assertDivergence: fixture %q declares nothing for listener %q; every listener in the set needs a row", f.name, l.name)
		}
		if e.why == "" {
			t.Fatalf("assertDivergence: fixture %q listener %q has an empty why; an unexplained divergence is the problem this file exists to fix", f.name, l.name)
		}
		// Guard: errorCode without errorKind checks NOTHING. The empty-kind arm
		// below asserts only that the event carried no error at all, so a row
		// saying {errorCode: "200"} reads like a claim about the upstream's
		// status and is never compared against anything — the same silent-pass
		// shape the guards here exist to close.
		if e.errorKind == "" && e.errorCode != "" {
			t.Fatalf("assertDivergence: fixture %q listener %q sets errorCode %q with an empty errorKind; nothing would compare it, because an empty kind asserts the event carried no error at all", f.name, l.name, e.errorCode)
		}
	}
	// Guard: a row naming a listener that is not being run would look like
	// coverage and check nothing. Typos in a map key are otherwise invisible.
	for name := range want {
		if !inSet[name] {
			t.Fatalf("assertDivergence: fixture %q declares listener %q, which is not in the set being run; nothing would have checked it", f.name, name)
		}
	}
	// Guard: the rows must actually disagree. Without this, assertDivergence
	// would be a strictly weaker assertParity that any fixture could be moved
	// to in order to drop the pairwise diff.
	if !divergentRows(listeners, want) {
		t.Fatalf("assertDivergence: fixture %q expects identical behaviour from every listener; that is parity — use assertParity, which also runs the pairwise diff", f.name)
	}

	// The driver needs to know which legs refuse pre-run, and the rows have
	// already said so. Deriving it keeps ONE declaration: a fixture that also
	// wrote refusedPreRunBy by hand could drift from its own rows, and while
	// that disagreement does fail loudly (finalizeObservation's "no session
	// event" check, then the PipelineRan comparison below), a reader would have
	// two places to reconcile to learn what the fixture claims.
	if f.refusedPreRunBy != nil {
		t.Fatalf("assertDivergence: fixture %q sets refusedPreRunBy; assertDivergence derives it from the rows' pipelineRan, so declare it there only", f.name)
	}
	refused := make(map[string]bool, len(listeners))
	for _, l := range listeners {
		refused[l.name] = !want[l.name].pipelineRan
	}
	f.refusedPreRunBy = refused

	type namedObs struct {
		listener string
		observed *observation
	}
	// Collected OUTSIDE t.Run for the reason assertParity spells out in full
	// above its own namedObs loop: appending inside the closure makes the whole
	// thing fail open under t.Parallel, with nothing compared and exit 0.
	got := make([]namedObs, 0, len(listeners))
	for _, l := range listeners {
		var obs *observation
		t.Run(f.name+"/"+l.name, func(t *testing.T) {
			obs = l.run(t, f, wantPhase)
			if obs == nil {
				t.Fatalf("listener %q produced no observation for fixture %q (phase=%v)", l.name, f.name, wantPhase)
			}
		})
		if obs != nil {
			got = append(got, namedObs{listener: l.name, observed: obs})
		}
	}
	// And a fixture that checked nothing is a broken test, not a pass.
	if len(got) == 0 {
		t.Fatalf("fixture %q collected no observations from %d listeners: nothing was checked", f.name, len(listeners))
	}

	for _, g := range got {
		e := want[g.listener]
		obs := g.observed
		where := fmt.Sprintf("fixture %q listener %s (%s)", f.name, g.listener, e.why)

		if obs.PipelineRan != e.pipelineRan {
			t.Errorf("%s: PipelineRan = %v, want %v", where, obs.PipelineRan, e.pipelineRan)
		}
		if obs.WireStatus != e.wireStatus {
			t.Errorf("%s: WireStatus = %d, want %d", where, obs.WireStatus, e.wireStatus)
		}
		if obs.StatusCode != e.eventStatusCode {
			t.Errorf("%s: event StatusCode = %d, want %d", where, obs.StatusCode, e.eventStatusCode)
		}
		switch {
		case e.errorKind == "" && obs.Error != nil:
			t.Errorf("%s: event carried an error it should not have: %s", where, jsonPretty(obs.Error))
		case e.errorKind != "" && obs.Error == nil:
			t.Errorf("%s: event carried no error; want kind %q code %q", where, e.errorKind, e.errorCode)
		case e.errorKind != "":
			if obs.Error.Kind != e.errorKind {
				t.Errorf("%s: error.Kind = %q, want %q", where, obs.Error.Kind, e.errorKind)
			}
			if obs.Error.Code != e.errorCode {
				t.Errorf("%s: error.Code = %q, want %q", where, obs.Error.Code, e.errorCode)
			}
		}
		for key, wantJSON := range e.pluginEvents {
			gotJSON, ok := obs.PluginEventJSON[key]
			if !ok {
				t.Errorf("%s: missing expected plugin event %q", where, key)
				continue
			}
			if !jsonEqual(gotJSON, wantJSON) {
				t.Errorf("%s: plugin event %q\n  got:  %s\n  want: %s", where, key, gotJSON, wantJSON)
			}
		}
		for key, gotJSON := range obs.PluginEventJSON {
			if _, ok := e.pluginEvents[key]; !ok {
				t.Errorf("%s: unexpected plugin event %q: %s", where, key, gotJSON)
			}
		}
		if e.bytes != nil {
			got := bytesSummary{Up: obs.BytesUp, Down: obs.BytesDown}
			if got != *e.bytes {
				t.Errorf("%s: bytes counted\n  got:  %+v\n  want: %+v", where, got, *e.bytes)
			}
		}
	}
}

// divergentRows reports whether at least two rows differ in something other
// than their prose.
func divergentRows(listeners []listenerRun, want map[string]divergentExpectation) bool {
	first := want[listeners[0].name]
	first.why = ""
	for _, l := range listeners[1:] {
		other := want[l.name]
		other.why = ""
		if !sameRow(first, other) {
			return true
		}
	}
	return false
}

// sameRow compares two rows field by field. Hand-written rather than
// reflect.DeepEqual so that nil and empty pluginEvents maps count as the same
// claim — both mean "no plugin payload" — and a fixture cannot manufacture a
// fake divergence out of that distinction to get past the guard above.
func sameRow(a, b divergentExpectation) bool {
	if a.why != b.why || a.pipelineRan != b.pipelineRan || a.wireStatus != b.wireStatus ||
		a.eventStatusCode != b.eventStatusCode || a.errorKind != b.errorKind || a.errorCode != b.errorCode {
		return false
	}
	// bytes participates too, with nil and an empty summary kept distinct for
	// the reason the field's own comment gives: ↓0 is a claim, so a row pinning
	// it diverges from a row making no claim about bytes at all.
	if (a.bytes == nil) != (b.bytes == nil) {
		return false
	}
	if a.bytes != nil && *a.bytes != *b.bytes {
		return false
	}
	if len(a.pluginEvents) != len(b.pluginEvents) {
		return false
	}
	for k, v := range a.pluginEvents {
		if other, ok := b.pluginEvents[k]; !ok || !jsonEqual(v, other) {
			return false
		}
	}
	return true
}

// digestJSON is the exact payload the spy publishes for a body digest. Written
// out here rather than marshalled from bodyDigest so the JSON FIELD NAMES are
// asserted too, matching what bodyMutationJSON does for the framework's own
// event.
func digestJSON(b []byte) string {
	return fmt.Sprintf(`{"len":%d,"sha256":%q}`, len(b), sha256Hex(b))
}

// divergenceBody returns n bytes whose every 26-byte window is distinct enough
// that a PREFIX and a SUFFIX of the same length hash differently.
//
// That matters here and does not in #937's overflow fixture, which fills with a
// single repeated 'x'. These fixtures assert which 1 MiB of a 2 MiB body
// survived truncation; with a uniform fill, keeping the tail instead of the
// head would produce an identical digest and the assertion would be blind to
// the difference it is for.
func divergenceBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

// maxBufferedBody is the 1 MiB cap extproc and reverseproxy share — each
// spells it maxBodySize in its own package. Written out here so the fixtures
// below read as "one over" / "the first cap's worth" rather than as bit-shift
// arithmetic.
//
// It coincides with Envoy's default buffer limit, which is where it came from
// and is why the shipped envoy-sidecar config refuses at the same number by a
// different mechanism. The two are independent settings, so a deployment that
// raises one does not move this.
const maxBufferedBody = 1 << 20

// TestDivergence_OutboundRequestBodyCap: a 2 MiB outbound request is refused
// before the pipeline by extproc (413) and carried in full by the forward
// proxy, whose request cap is 32 MiB.
//
// This is the band #864 measured real Claude Code traffic in, so the
// consequence is concrete: the same agent that works in proxy-sidecar mode —
// the laptop install and the Kubernetes default both — loses every request over
// 1 MiB the moment it is switched to envoy-sidecar, and loses it as a 413 from
// the sidecar, which is not a status the upstream ever sent.
//
// The 413 holds on BOTH Envoy configs, which is why this is the one cap fixture
// whose observables need no qualifying. On the shipped config it is Envoy's own
// Payload Too Large, refused over the 1 MiB default buffer limit before
// ext_proc is called; raise that limit and the listener's own check answers the
// same 413 instead. What differs is invisible from here and is recorded in the
// file comment: on the shipped config no session row is written at all, whereas
// the forward proxy logs a 413 proxy_error row over its own cap.
//
// 2 MiB rather than #864's measured 5 MiB because of the gRPC ceiling the file
// comment describes, and the size is exactly what keeps the paragraph above
// true. At 5 MiB the SHIPPED config still answers this same 413, Envoy having
// refused over its 1 MiB default long before gRPC is involved; the RAISED one
// no longer does, because past 4 MiB the stream dies ResourceExhausted and the
// client gets an empty 500 instead. So at 5 MiB the two columns stop agreeing
// and "the 413 holds on both configs" would be half wrong. 2 MiB is the size at
// which this fixture pins one answer for both.
// TestExtprocGRPCReceiveLimit pins the 5 MiB case.
func TestDivergence_OutboundRequestBodyCap(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:      "outbound-request-body-cap",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:               true,
			RecordRequestBodyDigest: true,
		})},
		method: "POST",
		path:   "/divergence/big-request",
		// An upstream is needed for the leg that does NOT refuse. The extproc
		// driver skips the response phase for any leg whose row says
		// pipelineRan: false, because Envoy stops calling ext_proc after an
		// ImmediateResponse.
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"ok":true}`),
		reqBody:        big,
	}
	assertDivergence(t, f, pipeline.SessionRequest, outboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:         "1 MiB request cap, refused before any plugin ran: Envoy's own default buffer limit on the shipped config, and the listener's own check in Process's RequestBody case as the backstop once that limit is raised",
			pipelineRan: false,
			wireStatus:  413,
		},
		"forwardproxy": {
			why:             "32 MiB request cap (maxRequestBodySize) and no Envoy in front of it, so 2 MiB is ordinary traffic and the plugin sees all of it",
			pipelineRan:     true,
			wireStatus:      200,
			eventStatusCode: 0, // request-phase event; no status has been chosen yet
			pluginEvents: map[string]string{
				spyPluginA + bodyReqDigestStrippedSuffix: digestJSON(big),
			},
		},
	})
}

// TestDivergence_InboundResponseBodyCap: a 2 MiB upstream response on the
// inbound path, WITH ENVOY'S BUFFER LIMIT RAISED ABOVE THE BODY. extproc
// TRUNCATES to 1 MiB and reports success; the reverse proxy refuses with a 502
// it generated itself.
//
// Both halves contradict #987's prescription, which expected the two to agree
// on 502 — but only in this config, and that qualification is the whole reason
// it is stated here. appendBoundedBody cuts the body, logs a WARN and lets the
// pipeline run on the prefix, so the client gets its whole response and only
// the plugin's view is short. The function's own comment names the
// consequence: "a JSON body cut short parses as nothing and the silence would
// otherwise look like a response that carried no usage."
//
// On the SHIPPED config the extproc row is not this. Envoy refuses the
// response over its 1 MiB default, answers the client 500, and
// appendBoundedBody is never reached — so the two shapes agree that the client
// gets an error (500 against the reverse proxy's 502) and disagree about the
// event, extproc's saying 200 with no error where the reverse proxy's says 502
// proxy_error. TestDivergence_InboundResponseBodyCapOnShippedEnvoy pins that
// pair; this one pins what the listener does when it is given the body.
func TestDivergence_InboundResponseBodyCap(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:      "inbound-response-body-cap",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:                true,
			RecordResponseBodyDigest: true,
		})},
		method:         "GET",
		path:           "/divergence/big-response",
		upstreamStatus: 200,
		upstreamBody:   big,
	}
	assertDivergence(t, f, pipeline.SessionResponse, inboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "1 MiB response cap enforced by truncation (appendBoundedBody), reachable only once Envoy's buffer limit is raised past the body: the pipeline runs on the first 1 MiB and the event reports no problem",
			pipelineRan:     true,
			wireStatus:      0, // no HTTP transport of its own; Envoy owns the connection
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(big[:maxBufferedBody]),
			},
		},
		"reverseproxy": {
			why:             "same 1 MiB cap enforced by refusal, and no Envoy in front to pre-empt it: modifyResponse returns responseBufferError{overLimit} and the request ends 502",
			pipelineRan:     true,
			wireStatus:      502,
			eventStatusCode: 502,
			errorKind:       "proxy_error",
			// The upstream's own status survives here rather than being
			// overwritten: the 502 is ours, and an operator still needs to know
			// the backend answered 200 before we dropped its response.
			errorCode: "200",
			// Nothing: the buffer check returns before RunResponse, so
			// OnResponse never ran and published no digest. The absence IS the
			// assertion — it is what distinguishes this refusal from extproc's
			// truncate-and-continue, where OnResponse runs on a short body.
			pluginEvents: nil,
		},
	})
}

// TestDivergence_OutboundResponseBodyCap: the quiet one, and the reason the
// digest knob had to exist before this fixture could be written. AGAIN WITH
// ENVOY'S BUFFER LIMIT RAISED ABOVE THE BODY — see the row's why.
//
// Both legs report 200 with no error. The ONLY observable difference is how
// much of the response the plugin was handed — 1 MiB on extproc, all 2 MiB on
// the forward proxy — so an LLM response over 1 MiB reaches the client intact
// while its token counts, and therefore its cost, silently become zero.
// Nothing in the session event or the wire status says a byte went missing.
// The LOGS do: appendBoundedBody warns "extproc: response body reached the
// buffer limit; truncating". That WARN is the only signal, which is why it is
// worth naming — an operator reading the session API, /v1/usage or agentop
// sees a healthy request and would have to already suspect this to go looking
// for it.
//
// On the SHIPPED config nothing is truncated here either, for the reason the
// file comment gives: Envoy refuses the response over its own 1 MiB default
// and answers the client 500, so the cost is lost to an error rather than to a
// short body. TestDivergence_OutboundResponseBodyCapOnShippedEnvoy pins that
// half. The outbound pair is worth keeping separate from the inbound one above
// because the forward proxy, this leg's peer, is the shape that gets it right
// in both configs: a 10 MiB cap and no Envoy ahead of it.
func TestDivergence_OutboundResponseBodyCap(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:      "outbound-response-body-cap",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:                true,
			RecordResponseBodyDigest: true,
		})},
		method:         "GET",
		path:           "/divergence/big-response",
		upstreamStatus: 200,
		upstreamBody:   big,
	}
	assertDivergence(t, f, pipeline.SessionResponse, outboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "1 MiB response cap, truncated by appendBoundedBody once Envoy's buffer limit is raised past the body: the plugin sees the first 1 MiB and the event looks entirely healthy, the WARN in the log being the only sign",
			pipelineRan:     true,
			wireStatus:      0,
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(big[:maxBufferedBody]),
			},
		},
		"forwardproxy": {
			why:             "10 MiB response cap (its own maxBodySize) and no Envoy in front of it, so the plugin is handed the whole 2 MiB and can count it",
			pipelineRan:     true,
			wireStatus:      200,
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(big),
			},
		},
	})
}

// TestDivergence_InboundResponseBodyCapOnShippedEnvoy: the same 2 MiB inbound
// response as TestDivergence_InboundResponseBodyCap, this time through the
// SHIPPED envoy-sidecar config — Envoy's 1 MiB default buffer limit left where
// the chart and the operator template leave it.
//
// This is the pair operators actually run, and the worse of the two. Envoy
// refuses the over-limit response and answers the CLIENT 500, never delivering
// the body to ext_proc: the listener is handed ResponseHeaders carrying the
// upstream's own 200 without end_of_stream, and then the stream ends. Its
// stream-end flush runs the response phase on an EMPTY buffer and records a
// 200 with no error — so the session event, /v1/usage and agentop all show a
// clean request that the caller watched fail. appendBoundedBody is never
// reached, so there is not even the WARN that the raised-limit fixtures get.
//
// The empty digest is what makes that visible rather than merely absent: the
// plugin RAN and was handed nothing. A row asserting no plugin event at all
// would also pass if the response phase had been skipped, which is the
// opposite bug — and the benign one, since a phase that never ran could not
// have recorded a misleading 200.
//
// The client's 500 is Envoy's own and is deliberately absent from these rows;
// see envoyAbortsResponseBody. What diverges here, and is pinned, is the half
// the listeners own: extproc's event says 200 with no error where the reverse
// proxy's says 502 proxy_error, even though both callers got an error.
func TestDivergence_InboundResponseBodyCapOnShippedEnvoy(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:                    "inbound-response-body-cap-shipped-envoy",
		direction:               pipeline.Inbound,
		envoyAbortsResponseBody: true,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:                true,
			RecordResponseBodyDigest: true,
		})},
		method:         "GET",
		path:           "/divergence/big-response",
		upstreamStatus: 200,
		upstreamBody:   big,
	}
	assertDivergence(t, f, pipeline.SessionResponse, inboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "Envoy's own 1 MiB default buffer limit refuses the response before the listener sees a byte of it; the stream-end flush then runs the response phase on an empty buffer and records a healthy-looking 200",
			pipelineRan:     true,
			wireStatus:      0, // the 500 is Envoy's; this listener has no transport to report one
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				// Length zero, and the digest of empty: OnResponse ran and was
				// handed nothing. Not the same claim as pluginEvents: nil.
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(nil),
			},
		},
		"reverseproxy": {
			why:             "no Envoy in proxy-sidecar mode to pre-empt anything, so the body arrives and the listener's own 1 MiB cap refuses it: responseBufferError{overLimit}, and the request ends 502",
			pipelineRan:     true,
			wireStatus:      502,
			eventStatusCode: 502,
			errorKind:       "proxy_error",
			errorCode:       "200",
			pluginEvents:    nil,
		},
	})
}

// TestDivergence_OutboundResponseBodyCapOnShippedEnvoy is the egress twin of
// the fixture above, and the one that costs money. Same 2 MiB response, same
// shipped envoy-sidecar config, outbound.
//
// Not a duplicate of the inbound one, for two reasons. The code is different:
// extproc's stream-end flush routes through recordResponseSession(pctx,
// "outbound") into recordOutboundResponseSession, a separate recorder from the
// inbound one, so the misleading 200 is reached by its own path and could stop
// being recorded on one side only. And the consequence is different: outbound
// is agent-to-model, so this is the direction where a response Envoy refused
// still books a healthy 200 with no tokens — a request whose cost silently
// becomes zero in /v1/usage and the ledger, where the inbound twin loses an
// answer to a caller instead.
//
// The forward proxy is the peer worth comparing against precisely because it
// has nothing to go wrong here: a 10 MiB cap, no Envoy ahead of it, and the
// whole 2 MiB handed to the plugin. The divergence is one-sided, which is the
// finding.
func TestDivergence_OutboundResponseBodyCapOnShippedEnvoy(t *testing.T) {
	big := divergenceBody(2 << 20)
	f := fixture{
		name:                    "outbound-response-body-cap-shipped-envoy",
		direction:               pipeline.Outbound,
		envoyAbortsResponseBody: true,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			ReadsBody:                true,
			RecordResponseBodyDigest: true,
		})},
		method:         "GET",
		path:           "/divergence/big-response",
		upstreamStatus: 200,
		upstreamBody:   big,
	}
	assertDivergence(t, f, pipeline.SessionResponse, outboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "Envoy's 1 MiB default buffer limit refuses the response before the listener sees a byte; recordOutboundResponseSession then books a 200 with no error and no tokens, so the spend for this request is lost rather than mispriced",
			pipelineRan:     true,
			wireStatus:      0, // the 500 is Envoy's; this listener has no transport to report one
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				// Length zero with the digest of empty, as inbound: OnResponse
				// ran and was handed nothing. Distinct from pluginEvents: nil,
				// which would also pass if the response phase had been skipped.
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(nil),
			},
		},
		"forwardproxy": {
			why:             "10 MiB response cap and no Envoy in proxy-sidecar mode to refuse anything first, so the plugin is handed the whole 2 MiB and the request can be counted and priced",
			pipelineRan:     true,
			wireStatus:      200,
			eventStatusCode: 200,
			pluginEvents: map[string]string{
				spyPluginA + bodyRespDigestStrippedSuffix: digestJSON(big),
			},
		},
	})
}

// pricedEvents is the emit both unread-body fixtures declare on every row: the
// spy's OnResponse publish, which must surface identically regardless of what
// the listener did with the bytes.
var pricedEvents = map[string]string{
	spyPluginA: jsonOf(spyEvent{Marker: "priced", Count: 3}),
}

// TestDivergence_InboundResponseBodyCountWhenNoPluginReadsIt: a response body
// no plugin asked to see, and the one field the listeners now honestly
// disagree on — BytesDown.
//
// The split is #1251's fix making it visible. Before it, the reverse proxy
// appended its response row inside modifyResponse, BEFORE httputil.ReverseProxy
// copies the body to the client, so this arm reported ↓0; extproc reported ↓0
// too, because with no body-reading plugin it asks Envoy for body mode NONE and
// Envoy never sends it the bytes. Three zeros agreed — the shared gap
// observationDiff's BytesDown comment describes — so parity held for the wrong
// reason. The fix moved the proxies' row to after the relay, where the body they
// relay through their own process is counted, and the agreement broke into the
// split this file exists to document.
//
// It cannot be closed on the extproc side. Matching the proxies would mean
// requesting bodies no plugin needs — defeating body mode NONE, which is what
// keeps unobserved responses unbuffered — so the honest options were restoring
// the shared zero (un-fixing the misread duration #1251 exists to fix) or
// pinning the divergence with its reason. Pinned.
//
// Narrower than it looks: every shipped pipeline includes inference-parser,
// whose undirected ReadsBody counts on both directions, so NeedsBody() is true
// there and extproc buffers, counts and records at body end like the proxies.
// This divergence is the custom body-less pipeline — an auth-only gate, say —
// not any shape a shipped profile runs.
//
// Duration has the same shape — extproc's stops at the headers, the proxies'
// now covers the relay — and is deliberately not pinned here: a timing figure
// needs sleeps to assert against, and the proxies' own packages pin their side
// (TestReverseProxy_ResponseEventDurationIncludesUnbufferedBody and the
// forwardproxy twin). The parity suite's HasDuration check is presence-only.
//
// The spy emits — the same priced emit the parity fixture this arm used to live
// in carried — for two reasons: the row must exist on every leg (extproc's
// recorders write nothing for a pipeline whose plugins published no A2A, no
// Invocation and no Custom entry), and the emit surfacing identically in both
// rows keeps a slice of that fixture's old claim, TestParity_
// ResponseEventEmission's, pinned here rather than dropped with the move.
func TestDivergence_InboundResponseBodyCountWhenNoPluginReadsIt(t *testing.T) {
	f := fixture{
		name:      "inbound-unread-response-body",
		direction: pipeline.Inbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			EmitOnResponse: true,
			ResponseEvent:  &spyEvent{Marker: "priced", Count: 3},
		})},
		method:         "GET",
		path:           "/divergence/unread-response",
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
	}
	assertDivergence(t, f, pipeline.SessionResponse, inboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "no plugin reads bodies, so the listener asks Envoy for body mode NONE and Envoy never sends it the response body; the row is recorded at the headers with ↓0 — not bytes it saw and failed to count, but bytes it asked not to see",
			pipelineRan:     true,
			wireStatus:      0, // success path; this listener has no HTTP transport to report from
			eventStatusCode: 200,
			bytes:           &bytesSummary{},
			pluginEvents:    pricedEvents,
		},
		"reverseproxy": {
			why:             "the unbuffered relay copies this body through the listener's own process, so it is counted at the wire and the row recorded after the copy (#1251); before that fix this leg reported ↓0 too and the two shapes agreed for the wrong reason",
			pipelineRan:     true,
			wireStatus:      200,
			eventStatusCode: 200,
			bytes:           &bytesSummary{Down: int64(len(`{"reply":"ok"}`))},
			pluginEvents:    pricedEvents,
		},
	})
}

// TestDivergence_OutboundResponseBodyCountWhenNoPluginReadsIt is the egress
// twin, and exists for the same reason the cap fixtures come in pairs: the
// forward proxy's unbuffered relay is separate code from the reverse proxy's
// (serveOutbound's io.Copy against recordOnEOFBody), so the post-copy recording
// could regress on one side only. Same body, same split, different recorder.
func TestDivergence_OutboundResponseBodyCountWhenNoPluginReadsIt(t *testing.T) {
	f := fixture{
		name:      "outbound-unread-response-body",
		direction: pipeline.Outbound,
		entries: []config.PluginEntry{spyEntry(spyPluginA, spyConfig{
			EmitOnResponse: true,
			ResponseEvent:  &spyEvent{Marker: "priced", Count: 3},
		})},
		method:         "GET",
		path:           "/divergence/unread-response",
		upstreamStatus: 200,
		upstreamBody:   []byte(`{"reply":"ok"}`),
	}
	assertDivergence(t, f, pipeline.SessionResponse, outboundListeners, map[string]divergentExpectation{
		"extproc": {
			why:             "same mechanism as the inbound twin: body mode NONE means Envoy never sends the bytes, so the row is recorded at the headers with ↓0",
			pipelineRan:     true,
			wireStatus:      0, // success path; this listener has no HTTP transport to report from
			eventStatusCode: 200,
			bytes:           &bytesSummary{},
			pluginEvents:    pricedEvents,
		},
		"forwardproxy": {
			why:             "serveOutbound relays the body through this process and records the row after its io.Copy (#1251); the count is complete for the same reason the duration is",
			pipelineRan:     true,
			wireStatus:      200,
			eventStatusCode: 200,
			bytes:           &bytesSummary{Down: int64(len(`{"reply":"ok"}`))},
			pluginEvents:    pricedEvents,
		},
	})
}

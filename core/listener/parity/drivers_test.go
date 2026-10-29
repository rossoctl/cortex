package parity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/metadata"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/extproc"
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/listener/reverseproxy"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	"github.com/rossoctl/cortex/core/session"
)

// fixture describes one parity scenario. Direction selects the listener
// pair to compare — Inbound (extproc + reverseproxy) or Outbound
// (extproc + forwardproxy). Entries flow through plugins.BuildWithDeps
// so Requires / RequiresLater / RequiresAny fire at build time.
type fixture struct {
	name      string
	direction pipeline.Direction
	entries   []config.PluginEntry
	method    string
	path      string
	reqBody   []byte

	// upstreamStatus / upstreamBody / upstreamContentType are what the
	// httptest backend serves on the proxy path, and what the parity
	// harness synthesizes into the extproc ResponseHeaders/ResponseBody
	// messages. Zero status skips the response phase entirely (deny-at-
	// request scenarios). Empty content-type defaults to application/json.
	upstreamStatus      int
	upstreamBody        []byte
	upstreamContentType string

	// upstreamHeaders are extra RESPONSE headers, applied identically by all three
	// drivers. A gateway's cost header lives here: it is the input half of every
	// precedence rule in cost/settle, so a fixture that cannot set one cannot compare what
	// the listeners do with it.
	upstreamHeaders map[string]string

	// splitResponseBodyAt makes the ext_proc driver deliver the response body as TWO
	// ResponseBody messages, cut at this byte offset, which is what a statically configured
	// STREAMED body mode produces. Zero means one message.
	//
	// ext_proc ONLY, deliberately: on the HTTP listeners the transport decides its own chunk
	// boundaries and nothing in a fixture can pin them. That asymmetry is the point — a chunk
	// boundary is a shape only this listener can be handed, and the last undercount found in
	// review lived exactly there.
	splitResponseBodyAt int

	// envoyAbortsResponseBody makes the ext_proc driver send ResponseHeaders with
	// end_of_stream FALSE and then no ResponseBody at all, ending the stream.
	//
	// That is not a malformed stream — it is what the SHIPPED envoy-sidecar
	// config produces for a response over Envoy's buffer limit. ext_proc asks
	// for ResponseBodyMode BUFFERED (set inline in handleResponseHeaders, the
	// response-side counterpart to the request mode requestBodyResponse
	// returns), and neither the chart's envoy-config nor the operator's
	// envoy.yaml.tmpl sets per_connection_buffer_limit_bytes or
	// per_request_buffer_limit_bytes, so Envoy's 1 MiB default applies; its docs
	// for BUFFERED say the downstream system receives an error when the body
	// exceeds it. Envoy sends the headers it had already seen, fails the stream,
	// and answers the client 500 itself.
	//
	// ext_proc ONLY, like splitResponseBodyAt, and for a stronger reason than
	// that one: proxy-sidecar mode — the laptop install and the Kubernetes
	// default alike — has no Envoy at all, so this models a filter the other
	// legs genuinely do not have. A fixture setting it is therefore comparing
	// two REAL DEPLOYMENT SHAPES of the same request, which is the divergence,
	// not an asymmetry in the harness.
	//
	// The 500 itself is not representable here and must not be faked: Envoy
	// generates it, the listener never sees it, and extproc's wire status stays 0
	// for want of a transport of its own. What this knob pins is the half the
	// listener DOES own — the session event it records with no body in hand.
	envoyAbortsResponseBody bool

	// deps are the dependencies the pipeline is built with — the same injection production
	// uses (plugins.BuildWithDeps). Zero value for the spy fixtures, a pricing registry for
	// the cost fixtures, which is what lets this suite run the REAL cost owner instead of a
	// plugin that only records that it was called.
	deps plugins.Deps

	// pipelineRefusedPreRun asserts the listener refused before the
	// pipeline (e.g. body overflow). Default false: every listener must
	// record. Combine with expectedWireStatus to pin the wire code.
	pipelineRefusedPreRun bool

	// refusedPreRunBy says the same thing PER LISTENER, keyed by the name in
	// listenerRun. Non-nil REPLACES pipelineRefusedPreRun; a missing key reads
	// as false.
	//
	// It exists because the request-body caps are not the same number on every
	// shape — 1 MiB on extproc and reverseproxy, 32 MiB on forwardproxy
	// (its maxRequestBodySize) — so a body in between is refused pre-run on
	// one leg of a fixture and runs the whole pipeline on the other. The
	// fixture-level flag cannot say that: it errors on whichever leg disagrees
	// with it, so the shape was unrepresentable and the gap untestable.
	//
	// This is a DRIVER-SHAPE hint, not a comparison weakener. assertParity
	// still compares PipelineRan pairwise, so a fixture that set this and then
	// asked for parity would fail on the very divergence it declared — which is
	// why divergence_test.go exists and this does not loosen anything here.
	//
	// A DIVERGENCE FIXTURE MUST NOT SET THIS. assertDivergence derives the map
	// from its rows' pipelineRan and rejects a fixture that also filled it in by
	// hand, so that the refusal is declared once. Say pipelineRan: false on the
	// row instead.
	refusedPreRunBy map[string]bool

	// expectedWireStatus, when non-zero, is asserted against every
	// listener's wire status. Meaningful whenever the listener produced a
	// status of its own: a pre-pipeline refusal (pipelineRefusedPreRun) or
	// a pipeline denial, where extproc reports the ImmediateResponse code.
	// NOT meaningful on the success path — there extproc has no HTTP
	// transport to report from and returns 0.
	expectedWireStatus int

	// expectedPluginEvents anchors correctness — maps each expected
	// SessionEvent.Plugins key to its exact JSON. Empty means "don't
	// assert content beyond the pairwise diff." Fixtures that want
	// bug-catching (not just drift-catching) fill this in.
	expectedPluginEvents map[string]string

	// expectedIdentity, when non-nil, is asserted against EVERY listener
	// rather than only compared between them. The pairwise diff cannot see
	// a gap both legs share — two listeners that each record nothing agree
	// perfectly — and for Identity the shared case is the likely one,
	// since the two recorders that dropped it were copy-pasted from each
	// other. Same argument the Inference comment in observationDiff makes.
	expectedIdentity *identitySummary

	// expectDuration asserts every listener reported a non-zero duration.
	// Absolute for the same reason: a denial is a completed request, so
	// "how long did it take" has an answer on every listener, and all four
	// already stamp pctx.StartedAt at entry.
	expectDuration bool

	// expectedInvocations, when non-nil, pins each listener's invocation
	// list EXACTLY and IN ORDER — length, order and every field.
	//
	// Absolute rather than pairwise for the usual reason: the pairwise diff
	// cannot see a gap both legs share, and an invocation list is one of
	// the easier things for every listener to get wrong together, since all
	// of them snapshot it through the same SnapshotInvocations call. The
	// pairwise check does now compare order (invocationsEqual stopped
	// sorting), so the two are complementary — this one catches a shared
	// drop or reorder, that one catches a split.
	//
	// Order is the contract #948 is about: SnapshotInvocations walks the
	// whole slice and agentop renders one row per entry in slice order, so
	// a listener that reordered them would tell an operator a different
	// story about which gate spoke first.
	expectedInvocations []invocationSummary

	// expectedUpstream, when non-nil, pins the request AS THE UPSTREAM
	// RECEIVED IT on every listener — the bytes and the Content-Length that
	// travelled, not what the plugin believed it set. It is the only field
	// here that looks past the session event at the wire.
	//
	// This exists because a plugin-driven body rewrite reaches the upstream
	// through completely unrelated code on each shape: the proxies replace
	// Request.Body and re-stamp Content-Length in Go, while extproc emits
	// Envoy's BodyMutation proto with a content-length SetHeaders entry
	// beside it (BUFFERED + SEND leaves the length to the processor and
	// Envoy rejects a mismatch). Nothing in the session event distinguishes
	// a rewrite that landed on the wire from one that was computed, emitted
	// as a modify Invocation, and then dropped — the pipeline reports
	// success either way — so without this the suite could only have
	// compared the two listeners' accounts of their own intent.
	//
	// Non-nil ALSO switches the capture on; left nil, a fixture makes no
	// claim about upstream and the drivers record nothing. Deliberate: the
	// extproc leg is DERIVED (see extprocUpstream) and is only faithful
	// where the fixture controls the request shape, so fixtures opt in
	// rather than inheriting a comparison they were not written for.
	expectedUpstream *upstreamSummary

	// expectedBytes, when non-nil, pins BytesUp and BytesDown EXACTLY on every
	// listener rather than only comparing them (#1309).
	//
	// Absolute as well as pairwise because zero is the default and the pairwise
	// diff cannot see a gap both legs share: three listeners that all count nothing
	// agree perfectly, which is what BYTES looked like before this change and what
	// a regression would look like after it. A pointer and not a plain pair of
	// int64s for the same reason — zero is a value a fixture may want to pin (a
	// body-less GET must report zero, not a length), so "assert nothing" needs to
	// be a distinct state.
	expectedBytes *bytesSummary
}

// bytesSummary is a fixture's claim about the two byte counts on one row.
//
// Up and Down are populated per PHASE, not per row: a request row counts what was
// forwarded and a response row what came back, so a fixture asserting both runs
// the same shape twice with different wantPhase values.
type bytesSummary struct {
	Up   int64
	Down int64
}

// refusedPreRun reports whether the named listener is expected to refuse the
// request before the pipeline runs. refusedPreRunBy wins when set, so a fixture
// declares either one global answer or one per listener, never a mix.
func (f fixture) refusedPreRun(listener string) bool {
	if f.refusedPreRunBy != nil {
		return f.refusedPreRunBy[listener]
	}
	return f.pipelineRefusedPreRun
}

// contentType returns the fixture's response content-type or a sensible
// default. Kept as a helper so both driver paths stay compact.
func (f fixture) contentType() string {
	if f.upstreamContentType != "" {
		return f.upstreamContentType
	}
	return "application/json"
}

// buildParityPipeline routes construction through plugins.BuildWithDeps
// so Requires / RequiresLater fire at build time on every driver, and so a fixture's
// dependencies are injected exactly as production injects them.
func buildParityPipeline(entries []config.PluginEntry, deps plugins.Deps) (*pipeline.Pipeline, error) {
	return plugins.BuildWithDeps(entries, deps)
}

// spyEntry builds a config.PluginEntry for spyPluginA/B with the given
// knobs. Marshals spyConfig once so fixture literals stay compact.
func spyEntry(name string, cfg spyConfig) config.PluginEntry {
	raw, _ := json.Marshal(cfg)
	return config.PluginEntry{Name: name, Config: raw}
}

// spyEntryShadow is spyEntry wrapped in on_error: observe — the shadow
// mode an operator rolls a new gate out under. The plugin is dispatched
// normally and its Reject is downgraded: Pipeline.Run flips the deny
// record's Shadow flag and CONTINUES to the next plugin
// (pipeline/pipeline.go:120), so a fixture pairing one of these with an
// enforcing spy gets both plugins on the record from one request.
func spyEntryShadow(name string, cfg spyConfig) config.PluginEntry {
	e := spyEntry(name, cfg)
	e.OnError = pipeline.ErrorPolicyObserve
	return e
}

// observation is the parity-comparable snapshot of a listener's session
// event. Covers the operator-facing wire surface; per-listener locals
// (Host casing, timestamps, RequestID) are excluded.
//
// Identity and Duration used to be excluded too, as "a follow-up fixture
// pass". They were the follow-up: #936 found Identity, Duration and Plugins
// recorded by extproc and dropped by both HTTP listeners on the deny path,
// invisible here precisely because this struct did not carry them.
//
// TLS stays out, and not as a deferral. extproc legitimately has no client
// TLS to report — Envoy terminates the handshake and ext_proc receives the
// request over gRPC — so a cross-listener equality check on TLS would be
// asserting something false, and it would pass here for the wrong reason
// anyway: all three drivers run plaintext httptest servers, so every leg
// records nil. TLS derivation is pinned by unit test in
// listener/internal/sessionevent instead, which is where it can be given a
// real tls.ConnectionState.
type observation struct {
	// PipelineRan is false when the listener rejected the request before
	// the pipeline (e.g. request body too large). Overflow fixtures then
	// assert wire status only and skip session-event comparisons.
	PipelineRan bool
	WireStatus  int // captured from the transport, not from the session event

	Phase       string
	StatusCode  int
	Error       *errorSummary
	Invocations []invocationSummary
	PluginKeys  []string
	// PluginEventJSON pins the raw JSON per plugin key so a snapshot
	// difference between listeners surfaces as a value diff.
	PluginEventJSON map[string]string
	// Identity is who the request authenticated as, nil when the event
	// carried none. Compared in full: a listener that drops the subject or
	// the scopes publishes an event an operator cannot attribute, and an
	// unattributable denial is the one denial that matters most.
	Identity *identitySummary
	// HasDuration is presence, not value. Every listener computes it as a
	// live time.Since(pctx.StartedAt), so the figures legitimately differ
	// between two legs of the same fixture and an equality check here
	// would flake. What is comparable — and what was actually wrong — is
	// whether a listener reports a duration at all.
	HasDuration bool
	// Upstream is the request as it left the listener, nil when the fixture
	// did not ask (expectedUpstream) or when nothing reached the upstream at
	// all — a denial, where nil on every leg is the correct answer.
	Upstream *upstreamSummary
	// BytesUp and BytesDown are the body sizes the event reported (#1309). A
	// cross-listener obligation with nothing behind it until now: all three
	// listeners count, each from its own machinery — extproc sums the chunks Envoy
	// hands it, the proxies count off the upstream body — and the same body has to
	// come out the same figure whichever shape the operator deployed.
	//
	// Compared as VALUES, unlike HasDuration above, and that is the point: a
	// duration legitimately differs between two legs of one fixture because it is
	// measured, while a body length is counted and must agree exactly.
	//
	// It earned its keep on the first run. The proxies originally tallied inside
	// each response arm, where the SSE arm has sseframe PAYLOADS in hand — `data: `
	// prefixes and blank-line separators already stripped — so on the four-event
	// reads-body-sse fixture extproc reported 137 and reverseproxy 105. Both
	// numbers are defensible in isolation; only one of them can be what BYTES
	// means. See listener/internal/bodycount for where the proxies count now.
	BytesUp   int64
	BytesDown int64
	// Inference is the token report the event carried, nil when it carried none.
	//
	// IT IS NOT A RESTATEMENT OF THE COST RECORD, which travels separately in
	// PluginEventJSON: the record is written into pctx.Extensions.Custom and every
	// listener snapshots that, while the counts live on the inference extension and
	// were snapshotted only by the outbound recorders. So a listener could — and did
	// — publish a whole, correctly priced cost on an event whose counts were absent,
	// with this suite green: cost/usage reads every token figure off
	// SessionEvent.Inference and nothing else (usage.go's foldInto), so the consumer
	// saw money for zero tokens. Comparing the money without the counts is what let
	// that through.
	Inference *inferenceSummary
}

// inferenceSummary is the token report, flattened to the fields a cost or usage
// consumer reads. Model included because a count with no model cannot be priced,
// and PresentKinds because it is what separates "used no cache" from "reported no
// cache" downstream — a zero and an absence that a bare count conflates.
type inferenceSummary struct {
	Model            string
	TotalTokens      int
	InputTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
	OutputTokens     int
	ReasoningTokens  int
	PresentKinds     uint8
}

// upstreamSummary is the forwarded request reduced to the two things a body
// rewrite has to get right together.
//
// ContentLength is the HEADER AS A STRING rather than a parsed length, because
// the failure worth catching is a header that disagrees with the bytes — and
// "absent" has to stay distinguishable from "0". Either listener can get one
// half right and the other wrong: the proxies set the header and
// Request.ContentLength from the same len() but the transport may re-derive it,
// and extproc's two halves are separate fields of separate proto messages that
// nothing cross-checks.
type upstreamSummary struct {
	Body          string
	ContentLength string
}

// identitySummary mirrors pipeline.EventIdentity. Scopes compared as an
// ordered slice because SnapshotIdentity copies them in the order the auth
// plugin supplied, and nothing between there and the event reorders them.
type identitySummary struct {
	Subject  string
	Scopes   []string
	ClientID string
	AgentID  string
}

// errorSummary mirrors pipeline.EventError so listener drift in the
// deny reason fails parity instead of remaining invisible.
type errorSummary struct {
	Kind    string
	Code    string
	Message string
}

type invocationSummary struct {
	Plugin  string
	Action  string
	Reason  string
	Details map[string]string
	// Shadow is whether the plugin ran under on_error: observe and its
	// deny was downgraded to a report. Compared because it is the ONLY
	// thing separating "this request was refused" from "this request
	// would have been refused once we enforce": drop the flag and a
	// shadow-mode rollout reads as an outage. The framework stamps it in
	// Pipeline.Run, so both listeners must carry it through unchanged.
	Shadow bool
}

// observe returns the sole event matching (direction, phase) in the
// DefaultSessionID bucket, folded into the parity-comparable shape.
// Returns nil when the bucket is empty or the phase is absent. Fails
// on more than one match so duplicate-record drift surfaces here.
// flattenHeaders turns a fixture's header map into makeHeaders' key/value sequence, sorted so
// the extproc driver's header order is stable across runs — an unstable order would make a
// fixture pass or fail on map iteration.
func flattenHeaders(h map[string]string) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		out = append(out, k, h[k])
	}
	return out
}

// waitForPhase polls the default bucket, for up to 5s, until it holds an event of wantDir
// and wantPhase. It reports nothing: observe, which runs next, says what is missing.
func waitForPhase(store *session.Store, wantDir pipeline.Direction, wantPhase pipeline.SessionPhase) {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if v := store.View(session.DefaultSessionID); v != nil {
			for i := range v.Events {
				if v.Events[i].Phase == wantPhase && v.Events[i].Direction == wantDir {
					return
				}
			}
		}
	}
}

func observe(t *testing.T, store *session.Store, wantDir pipeline.Direction, wantPhase pipeline.SessionPhase) *observation {
	t.Helper()
	v := store.View(session.DefaultSessionID)
	if v == nil || len(v.Events) == 0 {
		return nil
	}
	var (
		ev      *pipeline.SessionEvent
		matches int
	)
	for i := range v.Events {
		if v.Events[i].Phase == wantPhase && v.Events[i].Direction == wantDir {
			ev = &v.Events[i]
			matches++
		}
	}
	if matches > 1 {
		t.Fatalf("observe: %d events matched (direction=%v phase=%v); listener must record exactly once", matches, wantDir, wantPhase)
	}
	if ev == nil {
		return nil
	}

	obs := &observation{
		Phase:           ev.Phase.String(),
		StatusCode:      ev.StatusCode,
		HasDuration:     ev.Duration > 0,
		BytesUp:         ev.BytesUp,
		BytesDown:       ev.BytesDown,
		PluginEventJSON: map[string]string{},
	}
	if ev.Identity != nil {
		obs.Identity = &identitySummary{
			Subject:  ev.Identity.Subject,
			Scopes:   ev.Identity.Scopes,
			ClientID: ev.Identity.ClientID,
			AgentID:  ev.Identity.AgentID,
		}
	}
	if ev.Error != nil {
		obs.Error = &errorSummary{Kind: ev.Error.Kind, Code: ev.Error.Code, Message: ev.Error.Message}
	}
	if ev.Invocations != nil {
		invs := ev.Invocations.Inbound
		if wantDir == pipeline.Outbound {
			invs = ev.Invocations.Outbound
		}
		for _, inv := range invs {
			obs.Invocations = append(obs.Invocations, invocationSummary{
				Plugin:  inv.Plugin,
				Action:  string(inv.Action),
				Reason:  inv.Reason,
				Details: inv.Details,
				Shadow:  inv.Shadow,
			})
		}
	}
	if ev.Inference != nil {
		obs.Inference = &inferenceSummary{
			Model:            ev.Inference.Model,
			TotalTokens:      ev.Inference.TotalTokens,
			InputTokens:      ev.Inference.InputTokens,
			CacheReadTokens:  ev.Inference.CacheReadTokens,
			CacheWriteTokens: ev.Inference.CacheWriteTokens,
			OutputTokens:     ev.Inference.OutputTokens,
			ReasoningTokens:  ev.Inference.ReasoningTokens,
			PresentKinds:     ev.Inference.PresentKinds,
		}
	}
	for k, raw := range ev.Plugins {
		obs.PluginKeys = append(obs.PluginKeys, k)
		obs.PluginEventJSON[k] = string(raw)
	}
	sort.Strings(obs.PluginKeys)
	return obs
}

// --- extproc driver ------------------------------------------------------

// mockStream is a minimal ExternalProcessor_ProcessServer that pre-seeds
// requests and captures responses. Same shape as the mockStream in
// extproc/server_test.go:29; copied because that file is _test.go.
//
// TODO(parity): promote to plugintesting/ or listener/testutil/ once a
// third caller appears, per the shared-testing-utils audit follow-up.
type mockStream struct {
	extprocv3.ExternalProcessor_ProcessServer
	ctx       context.Context
	requests  []*extprocv3.ProcessingRequest
	responses []*extprocv3.ProcessingResponse
	recvIdx   int
}

func (m *mockStream) Context() context.Context { return m.ctx }
func (m *mockStream) Send(resp *extprocv3.ProcessingResponse) error {
	m.responses = append(m.responses, resp)
	return nil
}
func (m *mockStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if m.recvIdx >= len(m.requests) {
		return nil, io.EOF
	}
	req := m.requests[m.recvIdx]
	m.recvIdx++
	return req, nil
}
func (m *mockStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockStream) SendHeader(metadata.MD) error { return nil }
func (m *mockStream) SetTrailer(metadata.MD)       {}
func (m *mockStream) SendMsg(any) error            { return nil }
func (m *mockStream) RecvMsg(any) error            { return nil }

func makeHeaders(kvs ...string) *corev3.HeaderMap {
	hm := &corev3.HeaderMap{}
	for i := 0; i < len(kvs); i += 2 {
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{
			Key:      kvs[i],
			RawValue: []byte(kvs[i+1]),
		})
	}
	return hm
}

// runExtproc drives the fixture through the extproc listener in-process
// (no gRPC, no Envoy binary — Server.Process is called directly).
// Puts the spy pipeline on the slot matching fixture.direction; the
// other slot gets an empty pipeline.
func runExtproc(t *testing.T, f fixture, wantPhase pipeline.SessionPhase) *observation {
	t.Helper()

	spyPipe, err := buildParityPipeline(f.entries, f.deps)
	if err != nil {
		t.Fatalf("extproc: BuildWithDeps: %v", err)
	}
	emptyPipe, err := plugins.BuildWithDeps(nil, plugins.Deps{})
	if err != nil {
		t.Fatalf("extproc: BuildWithDeps(nil): %v", err)
	}

	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })

	srv := &extproc.Server{Sessions: store}
	dirHeader := "inbound"
	if f.direction == pipeline.Outbound {
		srv.OutboundPipeline = pipeline.NewHolder(spyPipe)
		srv.InboundPipeline = pipeline.NewHolder(emptyPipe)
		dirHeader = "outbound"
	} else {
		srv.InboundPipeline = pipeline.NewHolder(spyPipe)
		srv.OutboundPipeline = pipeline.NewHolder(emptyPipe)
	}

	reqs := []*extprocv3.ProcessingRequest{
		{
			Request: &extprocv3.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extprocv3.HttpHeaders{
					Headers: makeHeaders(
						"x-authbridge-direction", dirHeader,
						":method", f.method,
						":path", f.path,
						":authority", "parity.local",
						"content-length", fmt.Sprintf("%d", len(f.reqBody)),
					),
				},
			},
		},
	}
	// Send RequestBody only when the plugin declared ReadsBody, matching
	// what Envoy does at the ext_proc filter — the server asks for the body
	// from its RequestHeaders handler (NeedsBody() && requestHasBody) and
	// Envoy sends one only in reply to that.
	if len(f.reqBody) > 0 && spyPipe.NeedsBody() {
		reqs = append(reqs, &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_RequestBody{
				RequestBody: &extprocv3.HttpBody{Body: f.reqBody, EndOfStream: true},
			},
		})
	}
	// The response phase is skipped when this leg is expected to refuse the
	// request pre-run, and that is transport fidelity rather than test
	// convenience: an ImmediateResponse terminates Envoy's filter chain, so
	// Envoy never calls ext_proc again for that request. Sending response
	// callbacks after one would hand the listener a shape production cannot
	// produce. No fixture hit this before — a refusal meant upstreamStatus: 0 —
	// but a cap that only ONE shape enforces needs an upstream for the other leg.
	if f.upstreamStatus > 0 && !f.refusedPreRun("extproc") {
		reqs = append(reqs, &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_ResponseHeaders{
				ResponseHeaders: &extprocv3.HttpHeaders{
					Headers: makeHeaders(append([]string{
						":status", fmt.Sprintf("%d", f.upstreamStatus),
						"content-type", f.contentType(),
						"content-length", fmt.Sprintf("%d", len(f.upstreamBody)),
					}, flattenHeaders(f.upstreamHeaders)...)...),
					// Envoy sets end_of_stream on the header callback when the
					// response is over at its headers, and this harness has to say
					// so too: it is the only signal that distinguishes a body-less
					// response from one whose body has not arrived yet, and the
					// listener now keys its dispatch on it.
					EndOfStream: len(f.upstreamBody) == 0,
				},
			},
		})
		// Send ResponseBody only when the plugin asked for body buffering AND
		// THERE IS A BODY.
		//
		// The second half is not a tidy-up. Firing this append on NeedsBody()
		// alone gives every fixture with an empty upstream body a synthetic
		// zero-length ResponseBody message that real Envoy never sends — which
		// makes the body-less shape unrepresentable here and lets
		// extproc drop the cost and the response row for every 204, 304 and
		// error-status-on-headers while a suite whose entire purpose is catching
		// per-listener divergence stayed green. A harness that cannot express a
		// shape cannot notice a listener mishandling it.
		//
		// And not at all when Envoy aborted over its own buffer limit: the
		// headers above are the last message the listener gets, so what runs is
		// the stream-end flush with an empty buffer. See envoyAbortsResponseBody.
		if spyPipe.NeedsBody() && len(f.upstreamBody) > 0 && !f.envoyAbortsResponseBody {
			if cut := f.splitResponseBodyAt; cut > 0 && cut < len(f.upstreamBody) {
				reqs = append(reqs, &extprocv3.ProcessingRequest{
					Request: &extprocv3.ProcessingRequest_ResponseBody{
						ResponseBody: &extprocv3.HttpBody{Body: f.upstreamBody[:cut], EndOfStream: false},
					},
				})
				reqs = append(reqs, &extprocv3.ProcessingRequest{
					Request: &extprocv3.ProcessingRequest_ResponseBody{
						ResponseBody: &extprocv3.HttpBody{Body: f.upstreamBody[cut:], EndOfStream: true},
					},
				})
			} else {
				reqs = append(reqs, &extprocv3.ProcessingRequest{
					Request: &extprocv3.ProcessingRequest_ResponseBody{
						ResponseBody: &extprocv3.HttpBody{Body: f.upstreamBody, EndOfStream: true},
					},
				})
			}
		}
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)

	// Wire guards matching the HTTP drivers' upstreamHit check: consume
	// every message, and reject fixtures must send an ImmediateResponse.
	if stream.recvIdx != len(reqs) {
		t.Errorf("extproc: consumed %d of %d messages; listener bailed", stream.recvIdx, len(reqs))
	}
	// A pre-run refusal is checked here too, not only by the missing session
	// event: "no event" is also what a listener that quietly forwarded the
	// request and recorded nothing would produce, and those are opposite bugs.
	if f.upstreamStatus == 0 || f.refusedPreRun("extproc") {
		if n := len(stream.responses); n == 0 {
			t.Errorf("extproc: fixture %q asked for deny but no response was sent", f.name)
		} else if last := stream.responses[n-1]; last.GetImmediateResponse() == nil {
			t.Errorf("extproc: fixture %q asked for deny but last response was %T, want ImmediateResponse", f.name, last.Response)
		}
	}

	wireStatus := extprocWireStatus(stream)
	obs := finalizeObservation(t, f, "extproc", observe(t, store, f.direction, wantPhase), wireStatus)
	if obs != nil && f.expectedUpstream != nil {
		obs.Upstream = extprocUpstream(f, stream, wireStatus)
	}
	return obs
}

// extprocUpstream derives the request Envoy would forward from the protos this
// listener actually emitted. There is no upstream on this leg to ask — Envoy
// owns the connection and ext_proc only returns instructions about it — so this
// applies those instructions the way Envoy would and reports the result, which
// is what makes the answer comparable with the proxies' real httptest upstream.
//
// The starting point is the request as the driver sent it, because a reply that
// carries no mutation means "forward what you buffered". A content-length
// SetHeaders entry overrides the header, a BodyMutation overrides the bytes,
// and the two are read INDEPENDENTLY on purpose: emitting one without the other
// is precisely the drift this is here to catch, and Envoy would reject the
// result in BUFFERED + SEND mode rather than quietly fix it up.
//
// nil when an ImmediateResponse was sent — the request never reached an
// upstream, which is the same answer the proxy drivers give for a denial.
func extprocUpstream(f fixture, stream *mockStream, wireStatus int) *upstreamSummary {
	if wireStatus != 0 {
		return nil
	}
	out := &upstreamSummary{
		Body:          string(f.reqBody),
		ContentLength: fmt.Sprintf("%d", len(f.reqBody)),
	}
	for _, r := range stream.responses {
		br, ok := r.Response.(*extprocv3.ProcessingResponse_RequestBody)
		if !ok {
			continue
		}
		cr := br.RequestBody.GetResponse()
		if m := cr.GetBodyMutation(); m != nil {
			out.Body = string(m.GetBody())
		}
		for _, sh := range cr.GetHeaderMutation().GetSetHeaders() {
			if strings.EqualFold(sh.GetHeader().GetKey(), "content-length") {
				out.ContentLength = headerValueString(sh.GetHeader())
			}
		}
	}
	return out
}

// headerValueString reads a corev3.HeaderValue written either way round.
// RawValue is what this repo's listeners emit and what Envoy prefers, but Value
// is still legal and a reader that only checked one would silently see "".
func headerValueString(h *corev3.HeaderValue) string {
	if len(h.GetRawValue()) > 0 {
		return string(h.GetRawValue())
	}
	return h.GetValue()
}

// captureUpstream records a forwarded request from inside an httptest upstream
// handler. Shared by both proxy drivers so the two legs cannot disagree about
// how they looked, and called BEFORE the handler writes anything so the
// client's read of the response body orders the write against the test
// goroutine's read — the same ordering upstreamHit already relies on.
func captureUpstream(t *testing.T, listener string, r *http.Request) *upstreamSummary {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("%s: reading forwarded request body: %v", listener, err)
	}
	return &upstreamSummary{Body: string(body), ContentLength: r.Header.Get("Content-Length")}
}

// extprocWireStatus reads the HTTP status from an ImmediateResponse if
// one was sent, else 0. Pipeline-ran is inferred from observe(): a
// session event exists iff the pipeline reached the recording site.
func extprocWireStatus(stream *mockStream) int {
	for _, r := range stream.responses {
		if imm := r.GetImmediateResponse(); imm != nil && imm.Status != nil {
			return int(imm.Status.Code)
		}
	}
	return 0
}

// finalizeObservation stamps PipelineRan + WireStatus onto an
// observation. The refusal expectation — pipelineRefusedPreRun, or
// refusedPreRunBy[listener] where the caps differ per shape — is strict in
// BOTH directions: a missing event where the fixture didn't opt in is a bug,
// and an event present where it did is also a bug (the listener silently
// stopped enforcing the cap).
//
// listener is the name from listenerRun, needed both to read refusedPreRunBy
// and to say which leg failed — with three drivers calling this, "the listener"
// in an error message named none of them.
func finalizeObservation(t *testing.T, f fixture, listener string, obs *observation, wireStatus int) *observation {
	t.Helper()
	refused := f.refusedPreRun(listener)
	if obs == nil {
		if !refused {
			// Two fixture shapes reach here and the remedy is NOT the same one.
			// A parity fixture declares a pre-run refusal with
			// pipelineRefusedPreRun or refusedPreRunBy; a divergence fixture
			// must set neither — assertDivergence derives refusedPreRunBy from
			// the rows' pipelineRan and rejects a hand-written one outright — so
			// its remedy is pipelineRan: false on this listener's row. Naming
			// only the fixture fields sent half the callers to a knob
			// assertDivergence ignores or refuses.
			t.Errorf("no session event recorded for fixture %q on %s; if that is expected: an assertParity fixture sets pipelineRefusedPreRun or refusedPreRunBy[%q], an assertDivergence fixture sets pipelineRan: false on the %q row", f.name, listener, listener, listener)
			return nil
		}
		return &observation{PipelineRan: false, WireStatus: wireStatus}
	}
	if refused {
		t.Errorf("fixture %q expected %s to refuse before the pipeline, but an event was recorded (wireStatus=%d)", f.name, listener, wireStatus)
	}
	obs.PipelineRan = true
	obs.WireStatus = wireStatus
	return obs
}

// --- reverseproxy driver -------------------------------------------------

// runReverseProxy drives the fixture through the reverse-proxy listener
// against an httptest.Server upstream. The upstream stub fails loudly
// when a deny fixture reaches it, guarding OnRequest deny correctness.
func runReverseProxy(t *testing.T, f fixture, wantPhase pipeline.SessionPhase) *observation {
	t.Helper()
	if f.direction != pipeline.Inbound {
		t.Fatalf("reverseproxy handles Inbound only, got fixture %q direction=%v", f.name, f.direction)
	}

	var (
		upstreamHit  bool
		upstreamSeen *upstreamSummary
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		if f.expectedUpstream != nil {
			upstreamSeen = captureUpstream(t, "reverseproxy", r)
		}
		if f.upstreamStatus == 0 {
			t.Errorf("reverseproxy: upstream unexpectedly reached on deny fixture %q", f.name)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", f.contentType())
		for k, v := range f.upstreamHeaders {
			w.Header().Set(k, v)
		}
		w.WriteHeader(f.upstreamStatus)
		_, _ = w.Write(f.upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	p, err := buildParityPipeline(f.entries, f.deps)
	if err != nil {
		t.Fatalf("reverseproxy: BuildWithDeps: %v", err)
	}

	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })

	srv, err := reverseproxy.NewServer(pipeline.NewHolder(p), store, upstream.URL, nil)
	if err != nil {
		t.Fatalf("reverseproxy: NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	var bodyReader io.Reader
	if len(f.reqBody) > 0 {
		bodyReader = bytes.NewReader(f.reqBody)
	}
	req, err := http.NewRequestWithContext(context.Background(), f.method, proxy.URL+f.path, bodyReader)
	if err != nil {
		t.Fatalf("reverseproxy: NewRequest: %v", err)
	}
	if len(f.reqBody) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reverseproxy: Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Errorf("reverseproxy: Body.Close: %v", err)
	}

	// Deny-fixture sanity: the upstream stub must stay untouched.
	if f.upstreamStatus == 0 && upstreamHit {
		t.Errorf("reverseproxy: fixture %q asked for deny but upstream was reached", f.name)
	}

	// The unbuffered relay records its response row when ReverseProxy finishes copying the
	// body, which can be after the client has its response: wait for it rather than race it.
	if wantPhase == pipeline.SessionResponse && !f.refusedPreRun("reverseproxy") {
		waitForPhase(store, pipeline.Inbound, wantPhase)
	}
	obs := finalizeObservation(t, f, "reverseproxy", observe(t, store, pipeline.Inbound, wantPhase), resp.StatusCode)
	if obs != nil {
		obs.Upstream = upstreamSeen
	}
	return obs
}

// --- forwardproxy driver -------------------------------------------------

// runForwardProxy drives the fixture through the forward-proxy listener
// via a proxy-configured http.Client. Outbound-only; this is the listener
// agents egress through in proxy-sidecar mode — the laptop install and the
// Kubernetes default both.
func runForwardProxy(t *testing.T, f fixture, wantPhase pipeline.SessionPhase) *observation {
	t.Helper()
	if f.direction != pipeline.Outbound {
		t.Fatalf("forwardproxy handles Outbound only, got fixture %q direction=%v", f.name, f.direction)
	}

	var (
		upstreamHit  bool
		upstreamSeen *upstreamSummary
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		if f.expectedUpstream != nil {
			upstreamSeen = captureUpstream(t, "forwardproxy", r)
		}
		if f.upstreamStatus == 0 {
			t.Errorf("forwardproxy: upstream unexpectedly reached on deny fixture %q", f.name)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", f.contentType())
		for k, v := range f.upstreamHeaders {
			w.Header().Set(k, v)
		}
		w.WriteHeader(f.upstreamStatus)
		_, _ = w.Write(f.upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	p, err := buildParityPipeline(f.entries, f.deps)
	if err != nil {
		t.Fatalf("forwardproxy: BuildWithDeps: %v", err)
	}

	store := session.New(0, 100, 100)
	t.Cleanup(func() { store.Close() })

	srv, err := forwardproxy.NewServer(pipeline.NewHolder(p), store, nil)
	if err != nil {
		t.Fatalf("forwardproxy: NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("forwardproxy: parse proxy URL: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	var bodyReader io.Reader
	if len(f.reqBody) > 0 {
		bodyReader = bytes.NewReader(f.reqBody)
	}
	req, err := http.NewRequestWithContext(context.Background(), f.method, upstream.URL+f.path, bodyReader)
	if err != nil {
		t.Fatalf("forwardproxy: NewRequest: %v", err)
	}
	if len(f.reqBody) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("forwardproxy: Do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Errorf("forwardproxy: Body.Close: %v", err)
	}

	if f.upstreamStatus == 0 && upstreamHit {
		t.Errorf("forwardproxy: fixture %q asked for deny but upstream was reached", f.name)
	}

	obs := finalizeObservation(t, f, "forwardproxy", observe(t, store, pipeline.Outbound, wantPhase), resp.StatusCode)
	if obs != nil {
		obs.Upstream = upstreamSeen
	}
	return obs
}

// --- construction-only helpers -------------------------------------------

// tryBuild* replay each driver's construction (pipeline + listener
// NewServer / Server init) and return the first error.

func tryBuildExtproc(entries []config.PluginEntry) error {
	spyPipe, err := buildParityPipeline(entries, plugins.Deps{})
	if err != nil {
		return err
	}
	emptyPipe, err := plugins.BuildWithDeps(nil, plugins.Deps{})
	if err != nil {
		return err
	}
	_ = &extproc.Server{
		InboundPipeline:  pipeline.NewHolder(spyPipe),
		OutboundPipeline: pipeline.NewHolder(emptyPipe),
	}
	return nil
}

func tryBuildReverseProxy(entries []config.PluginEntry) error {
	p, err := buildParityPipeline(entries, plugins.Deps{})
	if err != nil {
		return err
	}
	_, err = reverseproxy.NewServer(pipeline.NewHolder(p), nil, "http://parity.local", nil)
	return err
}

func tryBuildForwardProxy(entries []config.PluginEntry) error {
	p, err := buildParityPipeline(entries, plugins.Deps{})
	if err != nil {
		return err
	}
	_, err = forwardproxy.NewServer(pipeline.NewHolder(p), nil, nil)
	return err
}

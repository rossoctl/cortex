package lineage

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// newTestPlugin creates a LineageTelemetry wired to an in-memory span exporter
// (synchronous, so a span appears the instant it is ended) and marks it ready
// so Init is not needed.
func newTestPlugin(t *testing.T) (*LineageTelemetry, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	p := NewLineageTelemetry()
	p.cfg = defaultConfig() // the shipped defaults, so a test sees what a deployment sees
	p.tp = tp
	p.tracer = tp.Tracer("test")
	p.selfID = "weather-service"
	p.ready.Store(true)
	return p, exp
}

// run drives a full exchange (request pass + finish) through a single-plugin
// pipeline. Spans are read from the caller's exporter.
func run(t *testing.T, p *LineageTelemetry, pctx *pipeline.Context, outcome pipeline.Outcome) {
	t.Helper()
	pl, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	pl.Run(context.Background(), pctx)
	pl.RunFinish(context.Background(), pctx, outcome)
}

// allow is the ordinary success outcome.
func allow(status int) pipeline.Outcome {
	return pipeline.Outcome{FinalAction: pipeline.OutcomeAllow, StatusCode: status}
}

// fakeContext mirrors what the real listeners supply. Method is populated
// because every listener now supplies it (reverseproxy/forwardproxy from
// r.Method, ext_proc from the :method pseudo-header) — if a listener stops,
// the fixture must change with it rather than keep asserting a fiction.
func fakeContext(dir pipeline.Direction, headers http.Header) *pipeline.Context {
	return &pipeline.Context{
		Direction: dir,
		Method:    "POST",
		Host:      "test-service:8000",
		Path:      "/test",
		Headers:   headers,
	}
}

// traceparent builds a header carrier naming traceID/spanID as the wire parent.
func traceparent(traceID, spanID string) http.Header {
	h := http.Header{}
	h.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	return h
}

// extractParent decodes the span context named by the headers' traceparent.
func extractParent(h http.Header) trace.SpanContext {
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h))
	return trace.SpanContextFromContext(ctx)
}

// roleSplit returns the request and response spans from an exported set,
// asserting exactly one of each.
func roleSplit(t *testing.T, spans tracetest.SpanStubs) (req, resp tracetest.SpanStub) {
	t.Helper()
	var gotReq, gotResp bool
	for _, s := range spans {
		switch attrStr(s, "lineage.role") {
		case "request":
			if gotReq {
				t.Fatal("more than one request span")
			}
			req, gotReq = s, true
		case "response":
			if gotResp {
				t.Fatal("more than one response span")
			}
			resp, gotResp = s, true
		default:
			t.Fatalf("span %q has no lineage.role", s.Name)
		}
	}
	if !gotReq || !gotResp {
		t.Fatalf("want one request + one response span, got %d spans (req=%v resp=%v)", len(spans), gotReq, gotResp)
	}
	return req, resp
}

// ---- identifiers, pairing, parenting ----

func TestExchange_TwoSpansPairedAndParented(t *testing.T) {
	p, exp := newTestPlugin(t)
	pctx := fakeContext(pipeline.Inbound, http.Header{})

	pl, err := pipeline.New([]pipeline.Plugin{p})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	// Emit on sight: the request span exists after the request pass, before finish.
	pl.Run(context.Background(), pctx)
	if got := len(exp.GetSpans()); got != 1 {
		t.Fatalf("after request pass: want 1 span (request), got %d", got)
	}

	pl.RunFinish(context.Background(), pctx, allow(200))
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("after finish: want 2 spans, got %d", len(spans))
	}
	req, resp := roleSplit(t, spans)

	// exchange.id == request span id, echoed on both.
	wantID := req.SpanContext.SpanID().String()
	if got := attrStr(req, "lineage.exchange.id"); got != wantID {
		t.Errorf("request exchange.id = %q, want %q", got, wantID)
	}
	if got := attrStr(resp, "lineage.exchange.id"); got != wantID {
		t.Errorf("response exchange.id = %q, want %q", got, wantID)
	}

	// Response span's parent is the request span (same trace).
	if resp.Parent.SpanID() != req.SpanContext.SpanID() {
		t.Errorf("response parent span = %s, want request span %s", resp.Parent.SpanID(), req.SpanContext.SpanID())
	}
	if resp.SpanContext.TraceID() != req.SpanContext.TraceID() {
		t.Errorf("response trace = %s, want request trace %s", resp.SpanContext.TraceID(), req.SpanContext.TraceID())
	}

	// Both spans share the same SpanKind (SERVER for inbound).
	if req.SpanKind != trace.SpanKindServer || resp.SpanKind != trace.SpanKindServer {
		t.Errorf("span kinds = %v/%v, want server/server", req.SpanKind, resp.SpanKind)
	}
}

// ---- the stamp (single-channel parenting, wire contract v1.5) ----

// TestStamp_OutboundRewritesStampNotTraceparent: the outbound reads its
// parent from the inbound's stamp, then re-stamps the forwarded tracestate
// with its OWN request span id for the peer sidecar's inbound to read. The
// forwarded traceparent is NOT modified (v1.5 removed the splice) — an app
// chain riding traceparent toward its own backend stays intact.
func TestStamp_OutboundRewritesStampNotTraceparent(t *testing.T) {
	p, exp := newTestPlugin(t)
	const traceID, wireParent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	const inboundID = "1111111111111111"
	h := traceparent(traceID, wireParent)
	h.Set("tracestate", tracestateStampKey+"="+inboundID)
	pctx := fakeContext(pipeline.Outbound, h)
	pctx.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/call", Params: map[string]any{"name": "get_weather"}}

	run(t, p, pctx, allow(200))

	req, _ := roleSplit(t, exp.GetSpans())
	// Parent comes from the inbound's stamp.
	if got := req.Parent.SpanID().String(); got != inboundID {
		t.Errorf("parent = %s, want stamped inbound %s", got, inboundID)
	}
	// The forwarded traceparent is untouched — still the wire parent.
	forwarded := extractParent(pctx.Headers)
	if got := forwarded.SpanID().String(); got != wireParent {
		t.Errorf("forwarded traceparent parent = %s, want untouched wire parent %s", got, wireParent)
	}
	if got := forwarded.TraceID().String(); got != traceID {
		t.Errorf("forwarded trace = %s, want %s", got, traceID)
	}
	// The forwarded tracestate now stamps THIS outbound request span,
	// replacing the inbound's stamp it consumed.
	want := tracestateStampKey + "=" + req.SpanContext.SpanID().String()
	if got := pctx.Headers.Get("tracestate"); got != want {
		t.Errorf("tracestate = %q, want re-stamp %q", got, want)
	}
}

// TestStamp_InboundParentsOnPeerStamp is the cross-pod link: the caller
// sidecar's outbound stamped tracestate with its request span id, and this
// inbound must parent on that stamp — not on the wire traceparent, whose
// span id may belong to an app chain this pipeline never exports.
func TestStamp_InboundParentsOnPeerStamp(t *testing.T) {
	p, exp := newTestPlugin(t)
	const traceID, wireParent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	const peerOutbound = "2222222222222222"
	h := traceparent(traceID, wireParent)
	h.Set("tracestate", tracestateStampKey+"="+peerOutbound)
	pctx := fakeContext(pipeline.Inbound, h)

	run(t, p, pctx, allow(200))

	req, _ := roleSplit(t, exp.GetSpans())
	if got := req.Parent.SpanID().String(); got != peerOutbound {
		t.Errorf("parent = %s, want peer outbound stamp %s", got, peerOutbound)
	}
	if got := attrStr(req, "lineage.parent.source"); got != "tracestate" {
		t.Errorf("lineage.parent.source = %q, want tracestate", got)
	}
	// The forwarded stamp now names THIS inbound request span — the app's
	// shim couriers it to exactly the outbound calls this inbound causes.
	want := tracestateStampKey + "=" + req.SpanContext.SpanID().String()
	if got := pctx.Headers.Get("tracestate"); got != want {
		t.Errorf("tracestate = %q, want re-stamp %q", got, want)
	}
}

func TestStamp_InboundHeadersUntouchedExceptStamp(t *testing.T) {
	p, exp := newTestPlugin(t)
	h := traceparent("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	before := http.Header{}
	maps.Copy(before, h)
	pctx := fakeContext(pipeline.Inbound, h)

	run(t, p, pctx, allow(200))

	// The ONLY inbound mutation is the tracestate stamp; traceparent and
	// everything else are forwarded as they arrived.
	req, _ := roleSplit(t, exp.GetSpans())
	want := tracestateStampKey + "=" + req.SpanContext.SpanID().String()
	if got := pctx.Headers.Get("tracestate"); got != want {
		t.Errorf("tracestate = %q, want stamp %q", got, want)
	}
	after := http.Header{}
	maps.Copy(after, pctx.Headers)
	after.Del("tracestate")
	if !headersEqual(before, after) {
		t.Errorf("inbound headers beyond tracestate mutated: before=%v after=%v", before, after)
	}
	// No stamp arrived, so the parent is the wire traceparent — recorded as such.
	if got := attrStr(req, "lineage.parent.source"); got != "wire" {
		t.Errorf("lineage.parent.source = %q, want wire", got)
	}
}

func TestStamp_PreservesForeignTracestateMembers(t *testing.T) {
	p, exp := newTestPlugin(t)
	h := traceparent("4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")
	h.Set("tracestate", "vendor=abc")
	pctx := fakeContext(pipeline.Inbound, h)

	run(t, p, pctx, allow(200))

	req, _ := roleSplit(t, exp.GetSpans())
	got := pctx.Headers.Get("tracestate")
	wantStamp := tracestateStampKey + "=" + req.SpanContext.SpanID().String()
	if !strings.Contains(got, wantStamp) || !strings.Contains(got, "vendor=abc") {
		t.Errorf("tracestate = %q, want both %q and vendor=abc", got, wantStamp)
	}
}

// TestMint_NoTraceparentForwardsOwn is the traceparent-less entry, both
// directions: the request span roots a fresh trace, and the forwarded request
// now carries a traceparent naming that span PLUS the tracestate stamp — so
// the next element (the app's shim inbound, the peer's sidecar outbound) has
// a context to extract and the stamp has a header to ride on.
func TestMint_NoTraceparentForwardsOwn(t *testing.T) {
	for _, dir := range []pipeline.Direction{pipeline.Inbound, pipeline.Outbound} {
		t.Run(dir.String(), func(t *testing.T) {
			p, exp := newTestPlugin(t)
			pctx := fakeContext(dir, http.Header{})

			run(t, p, pctx, allow(200))

			req, _ := roleSplit(t, exp.GetSpans())
			if req.Parent.IsValid() {
				t.Errorf("request span has parent %s, want a root", req.Parent.SpanID())
			}
			if got := attrStr(req, "lineage.parent.source"); got != "none" {
				t.Errorf("lineage.parent.source = %q, want none", got)
			}
			want := "00-" + req.SpanContext.TraceID().String() + "-" + req.SpanContext.SpanID().String() + "-01"
			if got := pctx.Headers.Get("traceparent"); got != want {
				t.Errorf("forwarded traceparent = %q, want minted %q", got, want)
			}
			wantStamp := tracestateStampKey + "=" + req.SpanContext.SpanID().String()
			if got := pctx.Headers.Get("tracestate"); got != wantStamp {
				t.Errorf("tracestate = %q, want stamp %q", got, wantStamp)
			}
		})
	}
}

// TestMint_Disabled is the pure-observer posture: with mint_traceparent off a
// traceparent-less request is forwarded exactly as it arrived — no
// traceparent, and therefore no stamp either.
func TestMint_Disabled(t *testing.T) {
	p, _ := newTestPlugin(t)
	p.cfg.MintTraceparent = false
	pctx := fakeContext(pipeline.Inbound, http.Header{})

	run(t, p, pctx, allow(200))

	if got := pctx.Headers.Get("traceparent"); got != "" {
		t.Errorf("traceparent minted with mint_traceparent off: %q", got)
	}
	if got := pctx.Headers.Get("tracestate"); got != "" {
		t.Errorf("tracestate stamped without a wire traceparent: %q", got)
	}
}

// TestMint_RestartsInvalidTraceparent: W3C's processing model for an
// unparseable traceparent is to restart the trace — a new traceparent, the
// tracestate dropped. The propagator's verdict decides: a malformed value, an
// empty one, and a version-ff one all extract as no context, so all three are
// restarted exactly like an absent header, and a foreign tracestate that rode
// in with them does not survive. (A VALID traceparent is never touched:
// TestStamp_InboundHeadersUntouchedExceptStamp.)
func TestMint_RestartsInvalidTraceparent(t *testing.T) {
	for _, sent := range []string{"not-a-traceparent", "", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"} {
		t.Run(fmt.Sprintf("traceparent=%q", sent), func(t *testing.T) {
			p, exp := newTestPlugin(t)
			h := http.Header{}
			h.Set("traceparent", sent)
			h.Set("tracestate", "vendor=abc")
			pctx := fakeContext(pipeline.Inbound, h)

			run(t, p, pctx, allow(200))

			req, _ := roleSplit(t, exp.GetSpans())
			if req.Parent.IsValid() {
				t.Errorf("request span has parent %s, want a root", req.Parent.SpanID())
			}
			if got := attrStr(req, "lineage.parent.source"); got != "none" {
				t.Errorf("lineage.parent.source = %q, want none", got)
			}
			want := "00-" + req.SpanContext.TraceID().String() + "-" + req.SpanContext.SpanID().String() + "-01"
			if got := pctx.Headers.Values("traceparent"); len(got) != 1 || got[0] != want {
				t.Errorf("forwarded traceparent = %v, want restarted %q", got, want)
			}
			wantStamp := tracestateStampKey + "=" + req.SpanContext.SpanID().String()
			if got := pctx.Headers.Get("tracestate"); got != wantStamp {
				t.Errorf("tracestate = %q, want the stamp alone (caller's dropped on restart) %q", got, wantStamp)
			}
		})
	}
}

// TestMint_OutboundChainsIntoPeerInbound is the cross-pod twin of
// TestMint_ChainsThroughEntry: an app with no context of its own calls out
// bare, this pod's outbound mints and stamps, and the peer's inbound —
// receiving exactly the forwarded headers — parents on that outbound via the
// stamp, in the outbound's trace. Same code path, both directions, proven
// rather than assumed.
func TestMint_OutboundChainsIntoPeerInbound(t *testing.T) {
	p, exp := newTestPlugin(t)
	out := fakeContext(pipeline.Outbound, http.Header{})
	run(t, p, out, allow(200))
	outReq, _ := roleSplit(t, exp.GetSpans())
	exp.Reset()

	peer, peerExp := newTestPlugin(t)
	peer.selfID = "weather-tool"
	in := fakeContext(pipeline.Inbound, out.Headers.Clone())
	run(t, peer, in, allow(200))
	inReq, _ := roleSplit(t, peerExp.GetSpans())

	if inReq.SpanContext.TraceID() != outReq.SpanContext.TraceID() {
		t.Errorf("peer inbound trace %s, want the outbound's %s", inReq.SpanContext.TraceID(), outReq.SpanContext.TraceID())
	}
	if inReq.Parent.SpanID() != outReq.SpanContext.SpanID() {
		t.Errorf("peer inbound parent %s, want the outbound request span %s", inReq.Parent.SpanID(), outReq.SpanContext.SpanID())
	}
	if got := attrStr(inReq, "lineage.parent.source"); got != "tracestate" {
		t.Errorf("lineage.parent.source = %q, want tracestate", got)
	}
}

// TestMint_ChainsThroughEntry is the whole entry mechanism end to end in
// miniature: a traceparent-less inbound (the entry), then an outbound that
// arrives carrying exactly the headers the inbound forwarded — as a
// propagate-only shim would courier them — parents on the entry's request
// span via the stamp, in the entry's own trace. One tree, one root.
func TestMint_ChainsThroughEntry(t *testing.T) {
	p, exp := newTestPlugin(t)
	entry := fakeContext(pipeline.Inbound, http.Header{})
	run(t, p, entry, allow(200))
	entryReq, _ := roleSplit(t, exp.GetSpans())
	exp.Reset()

	out := fakeContext(pipeline.Outbound, entry.Headers.Clone())
	run(t, p, out, allow(200))
	outReq, _ := roleSplit(t, exp.GetSpans())

	if outReq.SpanContext.TraceID() != entryReq.SpanContext.TraceID() {
		t.Errorf("outbound trace %s, want the entry's %s", outReq.SpanContext.TraceID(), entryReq.SpanContext.TraceID())
	}
	if outReq.Parent.SpanID() != entryReq.SpanContext.SpanID() {
		t.Errorf("outbound parent %s, want the entry request span %s", outReq.Parent.SpanID(), entryReq.SpanContext.SpanID())
	}
	if got := attrStr(outReq, "lineage.parent.source"); got != "tracestate" {
		t.Errorf("lineage.parent.source = %q, want tracestate", got)
	}
}

func TestConfig_MintTraceparent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"default on", `{"self_id":"x"}`, true},
		{"explicit true", `{"mint_traceparent":true,"self_id":"x"}`, true},
		{"explicit false", `{"mint_traceparent":false,"self_id":"x"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := decodeConfig([]byte(tc.raw))
			if err != nil {
				t.Fatalf("decodeConfig: %v", err)
			}
			if cfg.MintTraceparent != tc.want {
				t.Errorf("MintTraceparent = %v, want %v", cfg.MintTraceparent, tc.want)
			}
		})
	}
}

// TestStamp_OutboundPrefersStampOverMap is the same-trace fan-in case in
// miniature: two concurrent inbound exchanges on ONE trace (the trace-keyed
// map can only hold the later one), then an outbound whose tracestate stamp
// names the EARLIER inbound. Without the stamp this outbound would collapse
// onto the map entry — the 1/N misattribution the fanin-test.sh e2e proves.
func TestStamp_OutboundUsesTheStampedInbound(t *testing.T) {
	p, exp := newTestPlugin(t)
	const traceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Two concurrent inbounds on the SAME trace — the case no trace-keyed
	// structure can disambiguate, and the reason the stamp exists.
	run(t, p, fakeContext(pipeline.Inbound, traceparent(traceID, "1111111111111111")), allow(200))
	in1, _ := roleSplit(t, exp.GetSpans())
	exp.Reset()
	run(t, p, fakeContext(pipeline.Inbound, traceparent(traceID, "2222222222222222")), allow(200))
	in2, _ := roleSplit(t, exp.GetSpans())

	// Outbound couriered in1's stamp back through the app. It must parent
	// under in1 specifically — not in2, not the wire parent.
	exp.Reset()
	h := traceparent(traceID, "3333333333333333")
	h.Set("tracestate", tracestateStampKey+"="+in1.SpanContext.SpanID().String())
	run(t, p, fakeContext(pipeline.Outbound, h), allow(200))
	outReq, _ := roleSplit(t, exp.GetSpans())

	if outReq.Parent.SpanID() != in1.SpanContext.SpanID() {
		t.Errorf("parent = %s, want stamped inbound %s (the other in-flight inbound was %s)",
			outReq.Parent.SpanID(), in1.SpanContext.SpanID(), in2.SpanContext.SpanID())
	}
	if got := attrStr(outReq, "lineage.parent.source"); got != "tracestate" {
		t.Errorf("lineage.parent.source = %q, want tracestate", got)
	}
}

func TestStamp_MalformedFallsBackToWire(t *testing.T) {
	p, exp := newTestPlugin(t)
	const traceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const wireParent = "3333333333333333"

	// An inbound on this trace exists — and must NOT be used, because a
	// malformed stamp means "unknown", not "guess for me".
	run(t, p, fakeContext(pipeline.Inbound, traceparent(traceID, "1111111111111111")), allow(200))
	in1, _ := roleSplit(t, exp.GetSpans())

	exp.Reset()
	h := traceparent(traceID, wireParent)
	h.Set("tracestate", tracestateStampKey+"=nothex")
	run(t, p, fakeContext(pipeline.Outbound, h), allow(200))
	outReq, _ := roleSplit(t, exp.GetSpans())

	if got := outReq.Parent.SpanID().String(); got != wireParent {
		t.Errorf("parent = %s, want wire parent %s", got, wireParent)
	}
	if outReq.Parent.SpanID() == in1.SpanContext.SpanID() {
		t.Error("malformed stamp silently inherited this pod's inbound span")
	}
	if got := attrStr(outReq, "lineage.parent.source"); got != "wire" {
		t.Errorf("lineage.parent.source = %q, want wire", got)
	}
}

func TestStamp_ParentSourceWireWhenUnstamped(t *testing.T) {
	p, exp := newTestPlugin(t)
	out := fakeContext(pipeline.Outbound, traceparent("cccccccccccccccccccccccccccccccc", "1111111111111111"))
	run(t, p, out, allow(200))
	outReq, _ := roleSplit(t, exp.GetSpans())
	if got := attrStr(outReq, "lineage.parent.source"); got != "wire" {
		t.Errorf("lineage.parent.source = %q, want wire", got)
	}
}

// TestStamp_UnstampedOutboundNeverInheritsInbound is the regression guard for
// the removal of the trace-keyed map. An outbound with no stamp must fall to the
// wire parent EVEN WHEN this pod has an inbound span for the same trace. The old
// map answered such cases from "the last inbound seen", which is correct only
// while exactly one inbound is in flight — a precondition it never checked. A
// missing edge is recoverable; a confidently wrong one is not.
func TestStamp_UnstampedOutboundNeverInheritsInbound(t *testing.T) {
	p, exp := newTestPlugin(t)
	const traceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const wireParent = "3333333333333333"

	run(t, p, fakeContext(pipeline.Inbound, traceparent(traceID, "2222222222222222")), allow(200))
	inReq, _ := roleSplit(t, exp.GetSpans())

	exp.Reset()
	run(t, p, fakeContext(pipeline.Outbound, traceparent(traceID, wireParent)), allow(200))
	outReq, _ := roleSplit(t, exp.GetSpans())

	if outReq.Parent.SpanID() == inReq.SpanContext.SpanID() {
		t.Fatal("un-stamped outbound inherited this pod's inbound span — the map is back")
	}
	if got := outReq.Parent.SpanID().String(); got != wireParent {
		t.Errorf("parent = %s, want wire parent %s", got, wireParent)
	}
	if got := attrStr(outReq, "lineage.parent.source"); got != "wire" {
		t.Errorf("lineage.parent.source = %q, want wire", got)
	}
}

func TestStamp_ConcurrentTracesNeverCross(t *testing.T) {
	p, exp := newTestPlugin(t)
	const traceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const traceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// One inbound on each trace.
	run(t, p, fakeContext(pipeline.Inbound, traceparent(traceA, "1111111111111111")), allow(200))
	inA, _ := roleSplit(t, exp.GetSpans())
	exp.Reset()
	run(t, p, fakeContext(pipeline.Inbound, traceparent(traceB, "2222222222222222")), allow(200))
	inB, _ := roleSplit(t, exp.GetSpans())

	// Each outbound couriers its own trace's stamp back.
	exp.Reset()
	hA := traceparent(traceA, "3333333333333333")
	hA.Set("tracestate", tracestateStampKey+"="+inA.SpanContext.SpanID().String())
	run(t, p, fakeContext(pipeline.Outbound, hA), allow(200))
	outA, _ := roleSplit(t, exp.GetSpans())
	if outA.Parent.SpanID() != inA.SpanContext.SpanID() {
		t.Errorf("outbound A parent = %s, want inbound A %s", outA.Parent.SpanID(), inA.SpanContext.SpanID())
	}
	if outA.Parent.SpanID() == inB.SpanContext.SpanID() {
		t.Error("outbound A crossed into inbound B's span")
	}
	if outA.SpanContext.TraceID().String() != traceA {
		t.Errorf("outbound A trace = %s, want %s", outA.SpanContext.TraceID(), traceA)
	}

	exp.Reset()
	hB := traceparent(traceB, "4444444444444444")
	hB.Set("tracestate", tracestateStampKey+"="+inB.SpanContext.SpanID().String())
	run(t, p, fakeContext(pipeline.Outbound, hB), allow(200))
	outB, _ := roleSplit(t, exp.GetSpans())
	if outB.Parent.SpanID() != inB.SpanContext.SpanID() {
		t.Errorf("outbound B parent = %s, want inbound B %s", outB.Parent.SpanID(), inB.SpanContext.SpanID())
	}
}

// ---- bodyless / unparsed completeness ----

func TestBodyless_UnparsedNoCaptureStillEmitsBothSpans(t *testing.T) {
	p, exp := newTestPlugin(t)
	// capture_io defaults false; no parser extensions → protocol http.
	pctx := fakeContext(pipeline.Outbound, http.Header{})

	run(t, p, pctx, allow(200))

	req, resp := roleSplit(t, exp.GetSpans())
	if got := attrStr(req, "lineage.protocol"); got != "http" {
		t.Errorf("protocol = %q, want http", got)
	}
	// Complete: both carry the shared facts and the exchange is paired.
	if attrStr(req, "lineage.exchange.id") == "" || attrStr(resp, "lineage.exchange.id") == "" {
		t.Error("exchange.id missing on a bodyless span")
	}
	if got := attrStr(resp, "lineage.outcome"); got != "ok" {
		t.Errorf("outcome = %q, want ok", got)
	}
	// Deliberately no capture_io assertion here: protocol http has no payload
	// extractor (ioInputValue/ioOutputValue dispatch on a2a/mcp/inference
	// only), so this fixture yields "" whatever the flag says. The default is
	// proven in TestCaptureIO_OffByDefaultEmitsNoPayload, on a fixture where
	// the flag is the only thing that can suppress the payload.
}

// TestCaptureIO_OffByDefaultEmitsNoPayload proves the privacy default: with
// capture_io unset, no parsed content leaves the pod. It runs on an MCP
// tools/call fixture whose arguments and result both yield a non-empty value,
// so the absence of input.value/output.value can only be the flag — the same
// assertion on an http fixture cannot fail. The second half flips the flag on
// and asserts both appear, which is what makes the first half non-vacuous.
//
// The two halves need separate fixtures: a pipeline.Context carries its own
// finished state, so a second RunFinish on the same one is dropped.
func TestCaptureIO_OffByDefaultEmitsNoPayload(t *testing.T) {
	mcpContext := func() *pipeline.Context {
		pctx := fakeContext(pipeline.Outbound, http.Header{})
		pctx.Host = "weather-tool-mcp.team1.svc:8000"
		pctx.Path = "/mcp"
		pctx.Extensions.MCP = &pipeline.MCPExtension{
			Method: "tools/call",
			Params: map[string]any{"name": "get_weather", "arguments": map[string]any{"city": "Tokyo"}},
			Result: map[string]any{"content": []any{map[string]any{"type": "text", "text": "sunny"}}},
		}
		return pctx
	}

	off, offExp := newTestPlugin(t)
	if off.cfg.CaptureIO {
		t.Fatal("capture_io must default to false")
	}
	run(t, off, mcpContext(), allow(200))
	req, resp := roleSplit(t, offExp.GetSpans())
	if v, ok := findAttr(req, "input.value"); ok {
		t.Errorf("input.value = %v present with capture_io off", v)
	}
	if v, ok := findAttr(resp, "output.value"); ok {
		t.Errorf("output.value = %v present with capture_io off", v)
	}

	// Same fixture, flag on: both values appear, so the assertions above are
	// measuring the flag rather than an absent extractor.
	on, onExp := newTestPlugin(t)
	on.cfg.CaptureIO = true
	run(t, on, mcpContext(), allow(200))
	req, resp = roleSplit(t, onExp.GetSpans())
	checkAttr(t, req, "input.value", `{"city":"Tokyo"}`)
	checkAttr(t, resp, "output.value", "sunny")
}

// ---- outcomes ----

func TestOutcome_Denied(t *testing.T) {
	p, exp := newTestPlugin(t)
	pctx := fakeContext(pipeline.Inbound, http.Header{})

	run(t, p, pctx, pipeline.Outcome{
		FinalAction:   pipeline.OutcomeDeny,
		StatusCode:    401,
		DenyingPlugin: "jwt-validation",
	})

	_, resp := roleSplit(t, exp.GetSpans())
	if got := attrStr(resp, "lineage.outcome"); got != "denied" {
		t.Errorf("outcome = %q, want denied", got)
	}
	if got := attrStr(resp, "lineage.denied_by"); got != "jwt-validation" {
		t.Errorf("denied_by = %q, want jwt-validation", got)
	}
	if got, ok := intAttr(resp, "http.status_code"); !ok || got != 401 {
		t.Errorf("http.status_code = %d (ok=%v), want 401", got, ok)
	}
}

func TestOutcome_AbandonedHasNoStatus(t *testing.T) {
	p, exp := newTestPlugin(t)
	pctx := fakeContext(pipeline.Outbound, http.Header{})

	// Terminal error with no response written (upstream reset / disconnect).
	run(t, p, pctx, pipeline.Outcome{FinalAction: pipeline.OutcomeError, StatusCode: 0})

	_, resp := roleSplit(t, exp.GetSpans())
	if got := attrStr(resp, "lineage.outcome"); got != "abandoned" {
		t.Errorf("outcome = %q, want abandoned", got)
	}
	if _, ok := findAttr(resp, "http.status_code"); ok {
		t.Error("http.status_code present on an abandoned exchange (none was produced)")
	}
}

// ---- request facts + capture_io + span names ----

func TestRequestFacts_MCPWithCapture(t *testing.T) {
	p, exp := newTestPlugin(t)
	p.cfg.CaptureIO = true
	pctx := fakeContext(pipeline.Outbound, http.Header{})
	pctx.Host = "weather-tool-mcp.team1.svc:8000"
	pctx.Path = "/mcp"
	pctx.Scheme = "http"
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "tools/call",
		Params: map[string]any{"name": "get_weather", "arguments": map[string]any{"city": "Tokyo"}},
		Result: map[string]any{"content": []any{map[string]any{"type": "text", "text": "sunny"}}},
	}

	run(t, p, pctx, allow(200))
	req, resp := roleSplit(t, exp.GetSpans())

	checkAttr(t, req, "lineage.protocol", "mcp")
	checkAttr(t, req, "mcp.method", "tools/call")
	checkAttr(t, req, "mcp.tool", "get_weather")
	checkAttr(t, req, "http.method", "POST")
	checkAttr(t, req, "url.path", "/mcp")
	checkAttr(t, req, "url.scheme", "http")
	checkAttr(t, req, "lineage.self.id", "weather-service")
	checkAttr(t, req, "lineage.peer.host", "weather-tool-mcp.team1.svc:8000")
	checkAttr(t, req, "lineage.direction", "outbound")
	checkAttr(t, req, "input.value", `{"city":"Tokyo"}`)
	checkAttr(t, resp, "output.value", "sunny")

	if req.Name != "weather-service mcp get_weather" {
		t.Errorf("request span name = %q", req.Name)
	}
	if resp.Name != "weather-service mcp get_weather response" {
		t.Errorf("response span name = %q", resp.Name)
	}
	if req.SpanKind != trace.SpanKindClient {
		t.Errorf("outbound request kind = %v, want client", req.SpanKind)
	}
}

func TestRequestFacts_A2AAndInference(t *testing.T) {
	p, exp := newTestPlugin(t)
	// A2A.
	a := fakeContext(pipeline.Outbound, http.Header{})
	a.Extensions.A2A = &pipeline.A2AExtension{Method: "message/send", SessionID: "sess-123"}
	run(t, p, a, allow(200))
	areq, _ := roleSplit(t, exp.GetSpans())
	checkAttr(t, areq, "lineage.protocol", "a2a")
	checkAttr(t, areq, "a2a.method", "message/send")
	checkAttr(t, areq, "a2a.session_id", "sess-123")
	if _, ok := findAttr(areq, "url.scheme"); ok {
		t.Error("url.scheme present although the context carried no scheme")
	}
	if areq.Name != "weather-service a2a message/send" {
		t.Errorf("a2a span name = %q", areq.Name)
	}

	// Inference.
	exp.Reset()
	i := fakeContext(pipeline.Outbound, http.Header{})
	i.Extensions.Inference = &pipeline.InferenceExtension{Model: "qwen2.5:7b"}
	run(t, p, i, allow(200))
	ireq, _ := roleSplit(t, exp.GetSpans())
	checkAttr(t, ireq, "lineage.protocol", "inference")
	checkAttr(t, ireq, "inference.model", "qwen2.5:7b")
	if ireq.Name != "weather-service inference qwen2.5:7b" {
		t.Errorf("inference span name = %q", ireq.Name)
	}
}

// mcp-parser attaches to ANY JSON-RPC body — including every a2a exchange —
// so on an a2a hop both extensions are populated. The payload read is keyed by
// the protocol fact: when the a2a parser yields nothing (no text parts, a
// protocol-event artifact), the payload stays ABSENT rather than falling
// through to the co-populated MCP parse of the same bytes (which would emit
// the raw JSON-RPC envelope on an lineage.protocol=a2a span).
func TestCaptureIO_A2ANeverFallsThroughToCoPopulatedMCP(t *testing.T) {
	p, exp := newTestPlugin(t)
	p.cfg.CaptureIO = true
	pctx := fakeContext(pipeline.Outbound, http.Header{})
	pctx.Extensions.A2A = &pipeline.A2AExtension{
		Method: "message/send",
		// A status-update captured as the artifact — a protocol event, filtered.
		Artifact: `{"kind":"status-update","taskId":"t-1"}`,
	}
	pctx.Extensions.MCP = &pipeline.MCPExtension{
		Method: "message/send",
		Params: map[string]any{"message": map[string]any{"role": "user"}},
		Result: map[string]any{"artifacts": []any{map[string]any{"artifactId": "a-1"}}},
	}

	run(t, p, pctx, allow(200))
	req, resp := roleSplit(t, exp.GetSpans())

	checkAttr(t, req, "lineage.protocol", "a2a")
	if v, ok := findAttr(req, "input.value"); ok {
		t.Errorf("input.value = %q on an a2a hop with no a2a parts — leaked from the co-populated MCP parse", v.String())
	}
	if v, ok := findAttr(resp, "output.value"); ok {
		t.Errorf("output.value = %q on an a2a hop whose artifact is a protocol event — leaked from the co-populated MCP parse", v.String())
	}
	// mcp.* facts belong to mcp hops only; the a2a label must keep them off.
	if v, ok := findAttr(req, "mcp.method"); ok {
		t.Errorf("mcp.method = %q emitted on an a2a hop", v.String())
	}
}

func TestPrincipalFacts_InboundRequestOnly(t *testing.T) {
	p, exp := newTestPlugin(t)
	pctx := fakeContext(pipeline.Inbound, http.Header{})
	pctx.Identity = fakeIdentity{sub: "alice", client: "weather-ui"}

	run(t, p, pctx, allow(200))
	req, resp := roleSplit(t, exp.GetSpans())

	checkAttr(t, req, "lineage.principal.sub", "alice")
	checkAttr(t, req, "lineage.principal.client", "weather-ui")
	// Principal facts are request-only.
	if _, ok := findAttr(resp, "lineage.principal.sub"); ok {
		t.Error("lineage.principal.sub leaked onto the response span")
	}
}

func TestPrincipalFacts_OutboundNeverEmitsPrincipal(t *testing.T) {
	p, exp := newTestPlugin(t)
	pctx := fakeContext(pipeline.Outbound, http.Header{})
	pctx.Identity = fakeIdentity{sub: "alice", client: "weather-ui"}

	run(t, p, pctx, allow(200))
	req, _ := roleSplit(t, exp.GetSpans())
	if _, ok := findAttr(req, "lineage.principal.sub"); ok {
		t.Error("outbound span carried a principal fact (inbound-only)")
	}
}

// ---- the forbidden-keys guard ----

// TestForbiddenKeysNeverEmitted scans every attribute of every span emitted
// across a spread of exchange shapes and asserts none carries a key from a
// removed vocabulary. The contract deleted these; this test is the tripwire
// that keeps them deleted.
func TestForbiddenKeysNeverEmitted(t *testing.T) {
	forbidden := []string{"trust.", "lineage.hop.kind", "enduser.id", "openinference.", "source", "authbridge.proxy"}

	shapes := []func() *pipeline.Context{
		func() *pipeline.Context {
			c := fakeContext(pipeline.Inbound, http.Header{})
			c.Identity = fakeIdentity{sub: "alice", client: "weather-ui"}
			return c
		},
		func() *pipeline.Context {
			c := fakeContext(pipeline.Outbound, http.Header{})
			c.Extensions.MCP = &pipeline.MCPExtension{Method: "tools/call", Params: map[string]any{"name": "get_weather"}}
			return c
		},
		func() *pipeline.Context {
			c := fakeContext(pipeline.Outbound, http.Header{})
			c.Extensions.A2A = &pipeline.A2AExtension{Method: "message/send"}
			return c
		},
		func() *pipeline.Context {
			c := fakeContext(pipeline.Outbound, http.Header{})
			c.Extensions.Inference = &pipeline.InferenceExtension{Model: "qwen2.5:7b"}
			return c
		},
	}

	for _, mk := range shapes {
		p, exp := newTestPlugin(t)
		p.cfg.CaptureIO = true
		run(t, p, mk(), allow(200))
		// roleSplit fatals unless the exchange produced exactly one request
		// and one response span, so the scan below can never run on an empty
		// set and report green on a plugin that emitted nothing.
		req, resp := roleSplit(t, exp.GetSpans())
		for _, s := range []tracetest.SpanStub{req, resp} {
			for _, kv := range s.Attributes {
				key := string(kv.Key)
				for _, bad := range forbidden {
					if key == bad || strings.HasPrefix(key, bad) {
						t.Errorf("span %q emitted forbidden attribute %q", s.Name, key)
					}
				}
			}
		}
	}
}

// ---- robustness ----

func TestOnFinish_NoStateDoesNotPanicOrEmit(t *testing.T) {
	p, exp := newTestPlugin(t)
	pctx := fakeContext(pipeline.Inbound, http.Header{})
	// OnFinish without OnRequest having run — no exchangeState stored.
	p.OnFinish(context.Background(), pctx)
	if got := len(exp.GetSpans()); got != 0 {
		t.Errorf("OnFinish with no state emitted %d spans, want 0", got)
	}
}

func TestNotReady_SkipsSpan(t *testing.T) {
	p := NewLineageTelemetry()
	// Do NOT set ready — Init never called.
	pctx := fakeContext(pipeline.Inbound, http.Header{})
	action := p.OnRequest(context.Background(), pctx)
	if action.Type != pipeline.Continue {
		t.Fatalf("expected Continue, got %v", action.Type)
	}
	if pipeline.GetState[exchangeState](pctx, pluginName) != nil {
		t.Error("exchangeState should not be set when plugin is not ready")
	}
}

// ---- config ----

func TestConfigure_Defaults(t *testing.T) {
	p := NewLineageTelemetry()
	if err := p.Configure(nil); err != nil {
		t.Fatalf("Configure(nil): %v", err)
	}
	if p.cfg.OTelEndpoint != "localhost:4317" {
		t.Errorf("default endpoint = %q, want localhost:4317", p.cfg.OTelEndpoint)
	}
	if p.cfg.SelfIDFile != "/shared/client-id.txt" {
		t.Errorf("default self_id_file = %q", p.cfg.SelfIDFile)
	}
}

func TestConfigure_DecodesKeptKeys(t *testing.T) {
	p := NewLineageTelemetry()
	raw := json.RawMessage(`{"otel_endpoint":"http://collector:4317","capture_io":true,"self_id":"weather-service"}`)
	if err := p.Configure(raw); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if p.cfg.OTelEndpoint != "collector:4317" {
		t.Errorf("endpoint = %q, want collector:4317 (scheme stripped)", p.cfg.OTelEndpoint)
	}
	if !p.cfg.CaptureIO {
		t.Error("capture_io should be true")
	}
	if p.cfg.SelfID != "weather-service" {
		t.Errorf("self_id = %q", p.cfg.SelfID)
	}
}

// ---- helpers ----

type fakeIdentity struct {
	sub, client string
	scopes      []string
}

func (f fakeIdentity) Subject() string  { return f.sub }
func (f fakeIdentity) ClientID() string { return f.client }
func (f fakeIdentity) Scopes() []string { return f.scopes }

func findAttr(span tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func attrStr(span tracetest.SpanStub, key string) string {
	if v, ok := findAttr(span, key); ok {
		return v.AsString()
	}
	return ""
}

func intAttr(span tracetest.SpanStub, key string) (int64, bool) {
	if v, ok := findAttr(span, key); ok {
		return v.AsInt64(), true
	}
	return 0, false
}

// checkAttr asserts a span contains attribute key with the given string value.
func checkAttr(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	got, ok := findAttr(span, key)
	if !ok {
		t.Errorf("attribute %q not found in span %q", key, span.Name)
		return
	}
	if got.AsString() != want {
		t.Errorf("attr %q = %q, want %q", key, got.AsString(), want)
	}
}

func headersEqual(a, b http.Header) bool {
	return maps.EqualFunc(a, b, slices.Equal[[]string])
}

// TestInit_RefusesToStartWithoutIdentity locks the v1.3 rule at the identity
// boundary: a pod whose self identity cannot be resolved must fail at boot,
// never serve traffic under a plausible-but-wrong label (the old behavior
// emitted lineage.self.id="agent" from the empty-string serviceLabel).
func TestInit_RefusesToStartWithoutIdentity(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"inline self_id starts", Config{OTelEndpoint: "localhost:4317", SelfID: "weather-service"}, false},
		{"missing self_id_file refuses", Config{OTelEndpoint: "localhost:4317", SelfIDFile: t.TempDir() + "/absent.txt"}, true},
		{"no identity source refuses", Config{OTelEndpoint: "localhost:4317"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewLineageTelemetry()
			p.cfg = tc.cfg
			err := p.Init(context.Background())
			if p.tp != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				_ = p.tp.Shutdown(ctx)
				cancel()
			}
			if tc.wantErr && err == nil {
				t.Fatal("Init succeeded without a resolvable identity")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Init failed with a valid inline self_id: %v", err)
			}
			if tc.wantErr && p.Ready() {
				t.Error("plugin reports Ready after a refused Init")
			}
		})
	}
}

// TestInit_ReadsSelfIDFile covers the operator-injected path (file, not inline).
func TestInit_ReadsSelfIDFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/client-id.txt"
	if err := os.WriteFile(path, []byte("weather-service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewLineageTelemetry()
	p.cfg = Config{OTelEndpoint: "localhost:4317", SelfIDFile: path}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_ = p.tp.Shutdown(ctx)
	cancel()
	if p.selfID != "weather-service" {
		t.Errorf("selfID = %q, want trimmed file content", p.selfID)
	}
}

// TestConfig_UnknownKeysRefused: a typo'd knob must be a boot error, not a
// silent run-with-defaults.
func TestConfig_UnknownKeysRefused(t *testing.T) {
	if _, err := decodeConfig([]byte(`{"capture-io": true}`)); err == nil {
		t.Fatal("unknown config key accepted silently")
	}
	if _, err := decodeConfig([]byte(`{"capture_io": true, "self_id": "x"}`)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// ---- bypass config ----
// The one failure mode of bypass_paths / bypass_hosts produces NO signal
// anywhere: a matched hop is simply absent from the graph. So both directions
// are pinned — a match emits nothing, a near-miss emits the full pair.

func TestBypassPaths_PrefixMatchEmitsNothing(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		spans int // spans expected from the exchange
	}{
		{"prefix match skipped", "/health/live", 0},
		{"exact prefix skipped", "/health", 0},
		{"non-matching path emits", "/api/health-report", 2},
		{"prefix is anchored, not substring", "/v1/health", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, exp := newTestPlugin(t)
			p.cfg.BypassPaths = []string{"/health"}
			pctx := fakeContext(pipeline.Inbound, http.Header{})
			pctx.Path = tc.path
			run(t, p, pctx, allow(200))
			if got := len(exp.GetSpans()); got != tc.spans {
				t.Fatalf("path %q: got %d spans, want %d", tc.path, got, tc.spans)
			}
		})
	}
}

// TestBypassHosts_GlobMatchEmitsNothing pins the anchored matcher. The
// unanchored strings.Contains it replaced skipped every host in the third and
// fourth rows: a real workload leaving the graph, and a tenant opting out of
// being graphed by choosing its own name.
func TestBypassHosts_GlobMatchEmitsNothing(t *testing.T) {
	cases := []struct {
		name  string
		host  string
		spans int
	}{
		{"bare name skipped", "otel-collector:4317", 0},
		{"fqdn skipped by the .* form", "otel-collector.rossoctl-system.svc:4317", 0},
		{"prefixed workload is NOT skipped", "prometheus-metrics-agent.team1.svc:9090", 2},
		{"suffixed workload is NOT skipped", "my-otel-collector:4317", 2},
		{"unrelated host emits", "weather-tool:8000", 2},
		{"case is folded", "OTel-Collector:4317", 0},
		{"port is optional", "otel-collector", 0},
		{"ipv6 literal keeps its host", "[::1]:4317", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, exp := newTestPlugin(t)
			p.cfg = defaultConfig()
			pctx := fakeContext(pipeline.Outbound, http.Header{})
			pctx.Host = tc.host
			run(t, p, pctx, allow(200))
			if got := len(exp.GetSpans()); got != tc.spans {
				t.Fatalf("host %q: got %d spans, want %d", tc.host, got, tc.spans)
			}
		})
	}
}

// TestBypassHosts_InboundIgnoresHost: an inbound Host is the caller's own
// header. Honouring bypass_hosts there would let any caller suppress the
// record of its own request by naming the target it claims to be calling.
func TestBypassHosts_InboundIgnoresHost(t *testing.T) {
	p, exp := newTestPlugin(t)
	p.cfg.BypassHosts = []string{"otel-collector"}
	pctx := fakeContext(pipeline.Inbound, http.Header{})
	pctx.Host = "otel-collector:4317"
	run(t, p, pctx, allow(200))
	if got := len(exp.GetSpans()); got != 2 {
		t.Fatalf("inbound Host bypass honoured: got %d spans, want 2", got)
	}
}

// TestConfig_BypassEntriesValidated: an entry that matches everything turns
// the plugin off with no signal at all — Ready() stays true and no span is
// ever emitted — so it has to fail at boot.
func TestConfig_BypassEntriesValidated(t *testing.T) {
	refused := []struct {
		name string
		raw  string
	}{
		{"empty path", `{"bypass_paths": ["/healthz", ""]}`},
		{"whitespace-only path", `{"bypass_paths": ["  "]}`},
		{"root path", `{"bypass_paths": ["/"]}`},
		{"empty host", `{"bypass_hosts": ["jaeger", ""]}`},
		{"whitespace-only host", `{"bypass_hosts": [" "]}`},
		{"star host", `{"bypass_hosts": ["*"]}`},
		{"invalid glob", `{"bypass_hosts": ["[unclosed"]}`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeConfig([]byte(tc.raw)); err == nil {
				t.Fatalf("%s accepted; it disables the plugin silently", tc.raw)
			}
		})
	}

	cfg, err := decodeConfig([]byte(`{"bypass_hosts": [" jaeger.* "], "bypass_paths": [" /healthz "]}`))
	if err != nil {
		t.Fatalf("valid bypass config rejected: %v", err)
	}
	// Surrounding whitespace is trimmed rather than silently never matching.
	if got := cfg.BypassHosts; len(got) != 1 || got[0] != "jaeger.*" {
		t.Fatalf("bypass_hosts = %q, want [\"jaeger.*\"]", got)
	}
	if got := cfg.BypassPaths; len(got) != 1 || got[0] != "/healthz" {
		t.Fatalf("bypass_paths = %q, want [\"/healthz\"]", got)
	}
}

// TestConfig_BypassReplacesDefaults: setting either key replaces the default
// list rather than extending it — the convention ibac, sparc and cpex share.
// Undocumented until now, and the reason it is now stated on both keys.
func TestConfig_BypassReplacesDefaults(t *testing.T) {
	cfg, err := decodeConfig([]byte(`{"bypass_hosts": ["my-metrics-thing"]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.BypassHosts) != 1 || cfg.BypassHosts[0] != "my-metrics-thing" {
		t.Fatalf("bypass_hosts = %q, want the operator list alone", cfg.BypassHosts)
	}
	// The untouched key keeps its defaults.
	if len(cfg.BypassPaths) != len(defaultConfig().BypassPaths) {
		t.Fatalf("bypass_paths = %q, want the defaults", cfg.BypassPaths)
	}
}

// ---- lifecycle: gRPC connection ownership ----
// WithGRPCConn leaves the conn for the caller to close; the exporter's Shutdown
// does not. A real Init dials a (never-answered) localhost target, stores the
// conn, and Shutdown must both stop the provider and close that conn without
// error. We assert the observable contract — conn stored after Init, Shutdown
// returns nil — since the closed socket itself is not introspectable here.
func TestShutdown_ClosesConn(t *testing.T) {
	p := NewLineageTelemetry()
	p.cfg = Config{OTelEndpoint: "localhost:4317", SelfID: "weather-service"}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.conn == nil {
		t.Fatal("Init did not store the gRPC conn for Shutdown to close")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned an error: %v", err)
	}
	// A second Shutdown after conn is already closed must not panic; it may
	// return an error from re-closing, which the caller can ignore.
	_ = p.Shutdown(ctx)
}

// TestShutdown_NoInitIsSafe: Shutdown on a plugin that never Init'd (tp and
// conn both nil) is a no-op, not a nil-deref — the host may call it after a
// failed Init.
func TestShutdown_NoInitIsSafe(t *testing.T) {
	p := NewLineageTelemetry()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on an uninitialized plugin: %v", err)
	}
}

// TestShutdown_ClearsReady: a successful Init makes the plugin ready; Shutdown
// clears readiness, so a pipeline orchestrator polling Ready() before routing
// sees the plugin fall out of rotation rather than keep receiving traffic into
// a torn-down tracer provider.
func TestShutdown_ClearsReady(t *testing.T) {
	p := NewLineageTelemetry()
	p.cfg = Config{OTelEndpoint: "localhost:4317", SelfID: "weather-service"}
	if err := p.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !p.Ready() {
		t.Fatal("Ready() is false after a successful Init")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned an error: %v", err)
	}
	if p.Ready() {
		t.Fatal("Ready() is still true after Shutdown")
	}
}

// TestShutdown_ClearsReadyAfterFailedInit: readiness is cleared unconditionally,
// even when Init never set it — Shutdown flips it false regardless of whether
// tp/conn were ever built, keeping the false→false transition idempotent.
func TestShutdown_ClearsReadyAfterFailedInit(t *testing.T) {
	p := NewLineageTelemetry()
	if p.Ready() {
		t.Fatal("a freshly constructed plugin should not be ready")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on an uninitialized plugin: %v", err)
	}
	if p.Ready() {
		t.Fatal("Ready() is true after Shutdown on an uninitialized plugin")
	}
}

// ---- OTLP transport selection ----
// The export defaults to plaintext (in-pod loopback) but must honour a request
// for TLS rather than silently downgrading it: an https:// endpoint turns TLS
// on, and the one contradiction (https:// with an explicit otel_tls:false)
// fails closed rather than sending principal facts / payloads in the clear. A
// non-http(s) scheme (ftp://, ftps://) is rejected outright, not stripped and
// dialed insecure.
func TestConfig_TLSFromScheme(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantErr  bool
		wantTLS  bool
		wantHost string
	}{
		{"bare host:port stays insecure", `{"otel_endpoint":"collector:4317","self_id":"x"}`, false, false, "collector:4317"},
		{"http:// stays insecure, host reduced", `{"otel_endpoint":"http://collector:4317/v1/traces","self_id":"x"}`, false, false, "collector:4317"},
		{"https:// turns TLS on, host reduced", `{"otel_endpoint":"https://collector.example.com:4317","self_id":"x"}`, false, true, "collector.example.com:4317"},
		{"explicit otel_tls on a plaintext host is honoured", `{"otel_endpoint":"collector:4317","otel_tls":true,"self_id":"x"}`, false, true, "collector:4317"},
		{"https:// with otel_tls:false is a rejected contradiction", `{"otel_endpoint":"https://collector:4317","otel_tls":false,"self_id":"x"}`, true, false, ""},
		{"https:// with otel_tls:true is consistent", `{"otel_endpoint":"https://collector:4317","otel_tls":true,"self_id":"x"}`, false, true, "collector:4317"},
		{"ftp:// scheme is rejected", `{"otel_endpoint":"ftp://collector:4317","self_id":"x"}`, true, false, ""},
		{"ftps:// scheme is rejected", `{"otel_endpoint":"ftps://collector:4317","self_id":"x"}`, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := decodeConfig([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a config error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeConfig: %v", err)
			}
			if cfg.OTelTLS != tc.wantTLS {
				t.Errorf("OTelTLS = %v, want %v", cfg.OTelTLS, tc.wantTLS)
			}
			if cfg.OTelEndpoint != tc.wantHost {
				t.Errorf("OTelEndpoint = %q, want %q", cfg.OTelEndpoint, tc.wantHost)
			}
		})
	}
}

// ---- payload truncation ----
// With capture_io on, a payload larger than max_payload_bytes must be cut at
// the producer with an explicit marker, not left whole to be silently dropped
// by the OTLP exporter's attribute-length limit. The cut is byte-bounded and
// UTF-8-safe.
func TestCaptureIO_TruncatesOversizePayload(t *testing.T) {
	p, exp := newTestPlugin(t)
	p.cfg.CaptureIO = true
	p.cfg.MaxPayloadBytes = 64
	big := strings.Repeat("x", 500)
	pctx := fakeContext(pipeline.Outbound, http.Header{})
	pctx.Extensions.A2A = &pipeline.A2AExtension{
		Method: "message/send",
		Parts:  []pipeline.A2APart{{Kind: "text", Content: big}},
	}
	run(t, p, pctx, allow(200))
	req, _ := roleSplit(t, exp.GetSpans())
	v, ok := findAttr(req, "input.value")
	if !ok {
		t.Fatal("input.value absent on a captured a2a hop with text parts")
	}
	got := v.AsString()
	if len(got) > p.cfg.MaxPayloadBytes {
		t.Errorf("input.value is %d bytes, exceeds cap %d", len(got), p.cfg.MaxPayloadBytes)
	}
	if !strings.HasSuffix(got, truncatedSuffix) {
		t.Errorf("truncated input.value %q missing %q suffix", got, truncatedSuffix)
	}
}

// TestTruncate covers the helper's boundaries directly: within-cap passthrough,
// the opt-out, and a UTF-8-safe cut that never splits a multi-byte rune.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 64); got != "short" {
		t.Errorf("within cap mutated: %q", got)
	}
	if got := truncate("anything", -1); got != "anything" {
		t.Errorf("negative cap should disable truncation, got %q", got)
	}
	// A run of 3-byte runes ("世") cut to a byte budget that lands mid-rune:
	// the result must be valid UTF-8 and within the cap.
	s := strings.Repeat("世", 100) // 300 bytes
	got := truncate(s, 40)
	if len(got) > 40 {
		t.Errorf("truncated to %d bytes, exceeds cap 40", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation split a multi-byte rune: %q is not valid UTF-8", got)
	}
	if !strings.HasSuffix(got, truncatedSuffix) {
		t.Errorf("missing truncation marker: %q", got)
	}
	// The suffix-can't-fit fallback: max below len(truncatedSuffix) with a
	// multi-byte payload must still return valid UTF-8 within the cap (the
	// marker is dropped, but a mid-rune byte cut is not). Guards the budget<=0
	// branch, which previously did a hard s[:max] byte cut.
	tiny := truncate(strings.Repeat("世", 100), 4) // 4 < len(truncatedSuffix)
	if len(tiny) > 4 {
		t.Errorf("suffix-can't-fit cut to %d bytes, exceeds cap 4", len(tiny))
	}
	if !utf8.ValidString(tiny) {
		t.Errorf("suffix-can't-fit cut split a rune: %q is not valid UTF-8", tiny)
	}
}

// ---- otel_ca_file: a private CA for the collector ----

// TestConfig_CAFileImpliesTLS: a CA bundle is only meaningful for a TLS dial,
// so otel_ca_file turns otel_tls on. The contradictions are refused at decode
// like https:// + otel_tls:false: an explicit otel_tls:false beside the file,
// and an http:// endpoint beside either TLS knob.
func TestConfig_CAFileImpliesTLS(t *testing.T) {
	cfg, err := decodeConfig([]byte(`{"self_id":"x","otel_endpoint":"collector.ns:4317","otel_ca_file":"/etc/lineage/ca.pem"}`))
	if err != nil {
		t.Fatalf("decodeConfig: %v", err)
	}
	if !cfg.OTelTLS {
		t.Error("otel_ca_file did not imply otel_tls")
	}
	if cfg.OTelCAFile != "/etc/lineage/ca.pem" {
		t.Errorf("OTelCAFile = %q", cfg.OTelCAFile)
	}
	for name, raw := range map[string]string{
		"otel_ca_file with an explicit otel_tls:false": `{"self_id":"x","otel_ca_file":"/etc/lineage/ca.pem","otel_tls":false}`,
		"http:// endpoint with otel_ca_file":           `{"self_id":"x","otel_endpoint":"http://collector:4317","otel_ca_file":"/etc/lineage/ca.pem"}`,
		"http:// endpoint with otel_tls:true":          `{"self_id":"x","otel_endpoint":"http://collector:4317","otel_tls":true}`,
	} {
		if _, err := decodeConfig([]byte(raw)); err == nil {
			t.Errorf("%s: accepted, want a refused contradiction", name)
		}
	}
}

// selfSignedCAPEM returns a PEM-encoded self-signed CA certificate, enough
// for Init to build a cert pool from.
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "lineage-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestInit_CAFile: a private-CA bundle is loaded at Init and a bad one refuses
// to start (fail closed — never a silent fallback to the system roots). The
// dial is lazy, so a valid bundle lets Init succeed without a collector. The
// Config is built directly, with OTelTLS left false, to pin that Init keys
// on the file alone.
func TestInit_CAFile(t *testing.T) {
	dir := t.TempDir()
	good := dir + "/ca.pem"
	if err := os.WriteFile(good, selfSignedCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	garbage := dir + "/garbage.pem"
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		caFile  string
		wantErr bool
	}{
		{"valid bundle starts", good, false},
		{"missing file refuses", dir + "/absent.pem", true},
		{"file with no certificate refuses", garbage, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewLineageTelemetry()
			p.cfg = Config{OTelEndpoint: "collector.ns:4317", OTelCAFile: tc.caFile, SelfID: "x"}
			err := p.Init(context.Background())
			if tc.wantErr {
				if err == nil {
					_ = p.Shutdown(context.Background())
					t.Fatal("Init succeeded with an unusable otel_ca_file")
				}
				if p.Ready() {
					t.Error("plugin ready after a refused Init")
				}
				return
			}
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			if !p.Ready() {
				t.Error("plugin not ready after a successful Init")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = p.Shutdown(ctx)
		})
	}
}

// ---- export failure visibility ----

type failingExporter struct{ calls atomic.Int32 }

func (f *failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	f.calls.Add(1)
	return errors.New("collector unreachable")
}
func (f *failingExporter) Shutdown(context.Context) error { return nil }

// TestExportObserver_CountsFailures: a refused batch is counted (and logged
// under the plugin's name) and the error still reaches the SDK unchanged, so
// the batch processor's own handling is not altered.
func TestExportObserver_CountsFailures(t *testing.T) {
	fe := &failingExporter{}
	p := NewLineageTelemetry()
	obs := &exportObserver{SpanExporter: fe, failures: &p.exportFailures}
	if err := obs.ExportSpans(context.Background(), nil); err == nil {
		t.Fatal("observer swallowed the export error")
	}
	if got := p.exportFailures.Load(); got != 1 {
		t.Fatalf("exportFailures = %d after one refused batch, want 1", got)
	}
	// Reachable through the SDK: a span ended on a provider that exports
	// through the observer increments the counter again.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(obs))
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	span.End()
	_ = tp.Shutdown(context.Background())
	if got := p.exportFailures.Load(); got != 2 {
		t.Errorf("exportFailures = %d after a span through the SDK, want 2", got)
	}
	if fe.calls.Load() != 2 {
		t.Errorf("underlying exporter called %d times, want 2", fe.calls.Load())
	}
}

// TestLogExportFailure pins the throttle: powers of two are logged, nothing
// else is, so a long outage costs log lines in proportion to log2(length).
func TestLogExportFailure(t *testing.T) {
	var logged []uint64
	for n := uint64(1); n <= 40; n++ {
		if logExportFailure(n) {
			logged = append(logged, n)
		}
	}
	if !slices.Equal(logged, []uint64{1, 2, 4, 8, 16, 32}) {
		t.Errorf("logged failures = %v, want the powers of two up to 32", logged)
	}
}

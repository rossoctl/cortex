// Package lineage provides the lineage-telemetry authbridge plugin.
//
// Two-span model (see authbridge/docs/lineage-wire-contract.md — the wire
// contract this file implements, kept byte-identical with the consumer's copy
// in the lab-data-governance repo). Each HTTP exchange through the sidecar
// produces TWO OTLP spans:
//
//   - a request span, emitted as soon as the request has been seen and
//     forwarded, carrying caller-side facts + input.value; and
//   - a response span, emitted at stream end (even when no response was
//     produced), carrying status/outcome facts + output.value.
//
// Both spans are ended immediately at emission — no span is held open across
// the wait. lineage.exchange.id (= the request span's own span id) is echoed
// on both so the consumer pairs them. The plugin emits FACTS ONLY (direction,
// protocol, endpoints, parsed payloads); all vocabulary — hop kinds, entity
// kinds, caller/callee — lives in the consumer's classify(). See the "removed
// vs today" migration map in the contract for the attrs this no longer emits.
//
// The plugin implements Finisher so the response span is emitted at stream
// end whatever the outcome — including denials that happen AFTER the request
// span was recorded (a response-phase deny, or a request-phase deny by a
// plugin ordered after this one); pctx.Outcome() is available at that point
// and maps to lineage.outcome=denied + lineage.denied_by. LIMITATION: the
// pipeline YAML places this plugin after the gate plugins (ordering is not
// soft-declared under this capabilities model — position in the list is the
// contract), and the pipeline short-circuits on a request-phase Reject,
// so an exchange denied by a gate BEFORE OnRequest ran emits NO spans at all —
// it is invisible to lineage. Moving lineage ahead of the gates (spans for
// denied traffic too) is a named follow-up, not current behavior.
//
// What this producer writes onto forwarded requests — two things, both
// directions. Always: one tracestate member (tracestateStampKey) so the next
// lineage element can parent its exchange on this one. Only when the request
// carried no VALID traceparent — absent, empty or malformed, as the W3C
// propagator judges it: a traceparent naming this request span
// (mintTraceparent, config mint_traceparent, default on), because without one
// the next element has nothing to extract and the stamp has nothing to ride
// on. A valid traceparent is never modified; an invalid one is replaced, which
// is W3C's processing model for it (restart the trace, drop tracestate).
//
// Read-only variant ("Option 4"). A deployment that wants a pure observer —
// no header written, parenting on the wire context alone — sets
// mint_traceparent: false and deletes exactly the selectParent and
// restampTracestate calls in OnRequest (and the lineage.parent.source fact);
// the span emit itself stays. The trade-off: without the stamp, two sidecarred
// pods can only be joined through the app's own propagation, so cross-pod
// parenting degrades to whatever the wire parent carries. The call site is
// marked so the choice stays locatable; the variant is not built here.
package lineage

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
)

const pluginName = "lineage-telemetry"

// tracestateStampKey is the W3C tracestate member that carries the sidecar
// parent chain — the single channel every lineage element reads its parent
// from and writes its own request span id into (wire contract v1.5). Inbound
// stamps the request it forwards to its own app; the app's propagate-only shim
// carries tracestate through its per-request causal chain (contextvars), so
// the member surfaces on exactly the outbound calls that inbound caused.
// Outbound re-stamps the request it forwards to the peer, whose inbound
// sidecar reads it as its parent. Parent precedence is stamp > wire parent >
// none (nothing valid on the wire: the span roots a trace) in BOTH
// directions, and the chosen source is recorded as the
// lineage.parent.source fact. The forwarded traceparent is never modified:
// the sidecar chain lives entirely in this member, so an app that emits its
// own spans keeps an intact traceparent chain toward its own backend while
// the sidecar chain stays self-consistent in ours. (Until v1.4 the outbound
// instead rewrote the forwarded traceparent — the splice; v1.5 removed it.)
// The one exception: a request that arrived with no valid traceparent is
// forwarded with one naming this request span (mintTraceparent), so this
// member has a header to ride on — W3C reads tracestate only alongside a
// valid traceparent.
//
// The key is producer-owned and names the lineage domain (W3C convention: the
// key identifies the owner of the entry): the member carries the sidecar
// chain's own parent link and names no consumer. It was `kglin` until
// 2026-08-04 and `dg-parent` until 2026-09-03; the name never lands in stored
// data, so renaming is wire-only — but every sidecar on a hop must run the
// same key, so it changes in one release.
//
// A trace-keyed map (one entry per trace, "the last inbound seen") used to sit
// between the two. It was removed: its answer is correct only while exactly one
// inbound of that trace is in flight — a precondition it never checked and could
// not verify — and when it was wrong it produced a real, exported, walkable
// parent that was simply untrue. Un-stamped traffic falls to the wire
// parent, which is an app-internal span this pipeline never exported: the
// interaction still derives in full, but as a trace entry rather than a child.
// A visibly missing edge is recoverable; a silently wrong one is not.
const tracestateStampKey = "lineage-parent"

// truncatedSuffix marks a captured payload that MaxPayloadBytes cut short.
const truncatedSuffix = "…[truncated]"

func init() {
	plugins.RegisterPlugin(pluginName, func() pipeline.Plugin { return NewLineageTelemetry() })
}

// exchangeState carries what OnFinish needs to emit the response span as the
// twin of the request span emitted in OnRequest.
type exchangeState struct {
	// reqCtx is the (already-ended) request span's context — the parent of
	// the response span. An ended span's SpanContext is a valid parent.
	reqCtx trace.SpanContext
	// common holds the attributes shared by both spans (lineage.direction,
	// self.id, peer.*, protocol, exchange.id) — NOT lineage.role, which
	// differs per span. Computed once so both spans agree byte-for-byte.
	common   []attribute.KeyValue
	spanKind trace.SpanKind
	// spanName is the request span's name; the response span appends " response".
	spanName string
	// protocol is the request span's lineage.protocol fact; the response
	// span's output.value must be read through the SAME protocol's parser
	// (parsers are precedence-ordered, not mutually exclusive — mcp-parser
	// also matches any JSON-RPC body, including every a2a exchange).
	protocol string
}

// LineageTelemetry emits OTel spans for each request hop observed by authbridge.
type LineageTelemetry struct {
	cfg        Config
	tp         *sdktrace.TracerProvider
	tracer     trace.Tracer
	conn       *grpc.ClientConn // OTLP gRPC connection; owned by us, closed on Shutdown
	ready      atomic.Bool
	propagator propagation.TextMapPropagator
	selfID     string // agent's own client ID for the lineage.self.id fact
	// exportFailures counts batches the collector refused or never received.
	// Export is asynchronous and the dial is lazy, so this — with the WARN
	// exportObserver logs — is how an unreachable collector or a TLS chain
	// that does not verify becomes visible; see Ready for why readiness
	// deliberately does not follow it.
	exportFailures atomic.Uint64
}

// NewLineageTelemetry constructs an unconfigured plugin. Configure + Init must
// run before it serves traffic (guarded by Ready()).
func NewLineageTelemetry() *LineageTelemetry {
	return &LineageTelemetry{
		propagator: propagation.TraceContext{},
	}
}

func (p *LineageTelemetry) Name() string { return pluginName }

func (p *LineageTelemetry) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		// At least one protocol parser must be present and earlier in the
		// chain: the protocol fact and both payload reductions read the
		// parsers' Extensions. A chain that misorders lineage before its
		// parsers (or has none) fails at startup instead of silently
		// emitting lineage.protocol="http" on everything. jwt-validation
		// ordering (for the principal facts) cannot be soft-declared under
		// this capabilities model — list it before lineage in the YAML.
		RequiresAny: []string{"a2a-parser", "mcp-parser", "inference-parser"},
		// The contract is cited major.minor only, deliberately: patch
		// revisions (v1.5.x) clarify prose and never change span semantics,
		// so a patch bump must not imply a producer change.
		Description: "Emits two facts-only lineage spans per HTTP exchange (wire contract v1.6).",
	}
}

func (p *LineageTelemetry) Configure(raw json.RawMessage) error {
	cfg, err := decodeConfig(raw)
	if err != nil {
		return err
	}
	p.cfg = cfg
	return nil
}

func (p *LineageTelemetry) Init(ctx context.Context) error {
	// Resolve self identity for the lineage.self.id fact FIRST, before any
	// exporter or tracer resource is allocated. Every span this plugin emits is
	// a claim of the form "X did Y"; with no X there is no claim to make, so an
	// unresolvable identity refuses to start rather than serving traffic under a
	// plausible-but-wrong label ("no mechanism may guess", contract v1.3). Note
	// the asymmetry with this file's other unknowns: a missing status, payload
	// or parent anchor is a missing PART of a fact and degrades honestly
	// (abandoned / NULL / parent.source=wire or none). Identity is the fact's subject —
	// it has no degraded form, and a shared placeholder would collapse every
	// unidentified pod onto one entity row (entity id = uuid5("{kind}:{self.id}"),
	// and entities is upsert-only). Resolving it up front also means a refused
	// identity leaks nothing: the gRPC client and batch-span-processor goroutine
	// below are never created on that path.
	if p.cfg.SelfID != "" {
		p.selfID = p.cfg.SelfID
	} else if p.cfg.SelfIDFile != "" {
		raw, err := os.ReadFile(p.cfg.SelfIDFile)
		if err != nil {
			return fmt.Errorf("lineage-telemetry: no inline self_id and self_id_file unreadable: %w", err)
		}
		p.selfID = strings.TrimSpace(string(raw))
	}
	if p.selfID == "" {
		return fmt.Errorf("lineage-telemetry: self identity unresolved (empty self_id and self_id_file %q)", p.cfg.SelfIDFile)
	}

	endpoint := p.cfg.OTelEndpoint
	// Transport credentials: plaintext by default (the loopback in-pod
	// collector), TLS when otel_tls is set — an https:// endpoint or an
	// otel_ca_file sets it in decodeConfig. Spans carry principal facts on
	// every inbound request and, under capture_io, user messages and model
	// output, so a remote collector must not receive them in cleartext.
	creds, plaintext := insecure.NewCredentials(), true
	switch {
	case p.cfg.OTelCAFile != "":
		// A private CA (a cert-manager issued in-cluster collector, typically):
		// the dial verifies against this bundle only. Keyed on the file alone,
		// not on OTelTLS, so a Config built without the decoder still cannot
		// dial cleartext with a CA configured. Fails closed — an unreadable
		// file or one with no certificate refuses to start rather than falling
		// back to the system roots. Empty serverName = derive from endpoint.
		pemBytes, err := os.ReadFile(p.cfg.OTelCAFile)
		if err != nil {
			return fmt.Errorf("lineage-telemetry: otel_ca_file %q: %w", p.cfg.OTelCAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return fmt.Errorf("lineage-telemetry: otel_ca_file %q: no CA certificate found in PEM", p.cfg.OTelCAFile)
		}
		creds, plaintext = credentials.NewClientTLSFromCert(pool, ""), false
	case p.cfg.OTelTLS:
		// nil cert pool = system roots; empty serverName = derive from endpoint.
		creds, plaintext = credentials.NewClientTLSFromCert(nil, ""), false
	}
	// A plaintext dial to anything but loopback puts principal facts, and under
	// capture_io whole prompts and tool output, on the network in the clear.
	// Whether that hop leaves the cluster is not knowable from a host:port, and
	// the platform's own collector is plaintext gRPC, so this is the operator's
	// call to make rather than ours to refuse — but they get told at the one
	// moment they can act on it.
	if plaintext && !isLoopback(endpoint) {
		slog.Warn("lineage-telemetry: exporting spans in cleartext to a non-loopback collector; "+
			"set otel_tls (or otel_ca_file for a private CA) to encrypt them",
			"endpoint", endpoint)
	}

	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return fmt.Errorf("lineage-telemetry: gRPC dial %s: %w", endpoint, err)
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn),
	)
	if err != nil {
		// The dial succeeded but the exporter did not adopt the conn, so close
		// it here rather than leaking the gRPC client on this error path.
		_ = conn.Close()
		return fmt.Errorf("lineage-telemetry: OTLP exporter: %w", err)
	}
	// WithGRPCConn leaves connection ownership with the caller: the exporter's
	// Shutdown will not close conn, so we retain it and close it ourselves in
	// Shutdown. Store it only now, past the error path above.
	p.conn = conn

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("authbridge"),
			attribute.String("authbridge.component", pluginName),
		),
	)
	if err != nil {
		slog.Warn("lineage-telemetry: resource detection failed, using default", "error", err)
		res = resource.Default()
	}

	// No sampler is set, so the SDK default applies: ParentBased(AlwaysSample),
	// overridable through OTEL_TRACES_SAMPLER. The sampling flag of a root
	// span is what a minted traceparent carries downstream (mintTraceparent),
	// and every peer sidecar is ParentBased too — a ratio sampler here would
	// silently un-sample whole chains, not just this pod's spans.
	p.tp = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(&exportObserver{SpanExporter: exporter, failures: &p.exportFailures}),
		sdktrace.WithResource(res),
	)
	p.tracer = p.tp.Tracer("authbridge/" + pluginName)

	p.ready.Store(true)
	slog.Info("lineage-telemetry: initialized", "endpoint", endpoint, "self_id", p.selfID)
	return nil
}

// isLoopback reports whether endpoint (host:port, already reduced from any URL
// form by decodeConfig) names this pod. Loopback traffic never leaves the
// network namespace, so cleartext there carries no exposure and earns no WARN.
func isLoopback(endpoint string) bool {
	host := endpoint
	if h, _, err := net.SplitHostPort(endpoint); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Shutdown flushes and stops the tracer provider and then closes the OTLP gRPC
// connection. The exporter created with WithGRPCConn does not own conn, so
// closing it here is what actually releases the socket; both errors are joined
// so neither is lost. Safe to call after a failed Init (tp/conn may be nil).
//
// Readiness is cleared first, unconditionally: after Shutdown the plugin is no
// longer ready even if tp/conn are nil (post-failed-Init) or their shutdown
// errors, so a pipeline orchestrator checking Ready() before routing sees the
// lifecycle transition. This mirrors the p.ready.Store(true) in Init.
func (p *LineageTelemetry) Shutdown(ctx context.Context) error {
	p.ready.Store(false)
	var tpErr error
	if p.tp != nil {
		tpErr = p.tp.Shutdown(ctx)
	}
	var connErr error
	if p.conn != nil {
		connErr = p.conn.Close()
	}
	return errors.Join(tpErr, connErr)
}

// Ready reports that the plugin is configured, has an identity and can
// record spans — not that the collector is reachable. grpc.NewClient dials
// lazily and the batch processor exports asynchronously, so no point in Init
// can prove the collector or its TLS chain; and readiness must not follow the
// collector anyway: an unready plugin skips OnRequest, which is where the
// tracestate stamp and the minted traceparent are written, so a collector
// outage would fragment every trace on the wire instead of merely delaying
// its export. A collector that cannot be reached surfaces through
// exportObserver instead: a plugin-namespaced WARN and the exportFailures
// counter.
func (p *LineageTelemetry) Ready() bool { return p.ready.Load() }

// exportObserver wraps the OTLP exporter so a failed export is visible from
// this plugin — a plugin-namespaced WARN and a counter — instead of only
// through the OTel SDK's default error handler on stderr. The error is
// returned unchanged; the batch processor's retry/drop behaviour is untouched.
// The WARN is throttled to the 1st, 2nd, 4th, 8th… failure: a dead collector
// fails a batch every export interval, and one line per failure would bury
// the logs exactly when they matter. The running total is in every line.
type exportObserver struct {
	sdktrace.SpanExporter
	failures *atomic.Uint64
}

func (e *exportObserver) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	if err != nil {
		if n := e.failures.Add(1); logExportFailure(n) {
			slog.Warn("lineage-telemetry: span export failed; the collector is unreachable or refused the batch",
				"spans", len(spans), "failures", n, "error", err)
		}
	}
	return err
}

// logExportFailure reports whether the n-th consecutive-count failure is one
// of the logged ones: powers of two, so the volume grows with the log of the
// outage length rather than with the outage length.
func logExportFailure(n uint64) bool { return n&(n-1) == 0 }

func (p *LineageTelemetry) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
	if !p.ready.Load() {
		pctx.Skip("not_ready")
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Skip infrastructure paths (health checks, agent-card discovery, etc.)
	for _, prefix := range p.cfg.BypassPaths {
		if strings.HasPrefix(pctx.Path, prefix) {
			pctx.Skip("bypass_path")
			return pipeline.Action{Type: pipeline.Continue}
		}
	}

	// Skip infrastructure outbound targets (OTel exporters, metrics scrapers, etc.).
	// Outbound only: on the inbound phase Host is the caller's own header, so
	// honouring the list there would let any caller suppress its own exchange
	// by sending "Host: otel-collector". cpex reached the same conclusion for
	// the same reason (its bypass_hosts is outbound-only for an attacker-
	// controlled inbound Host).
	if pctx.Direction == pipeline.Outbound && matchesAnyHost(p.cfg.BypassHosts, pctx.Host) {
		pctx.Skip("bypass_host")
		return pipeline.Action{Type: pipeline.Continue}
	}

	// Extract remote trace context from the incoming W3C traceparent header.
	// HeaderCarrier wraps http.Header and uses case-insensitive Get/Keys so
	// canonical-form keys ("Traceparent") match the propagator's lowercase
	// lookups.
	remoteCtx := p.propagator.Extract(ctx, propagation.HeaderCarrier(pctx.Headers))

	protocol := protocolOf(pctx)
	self := serviceLabel(p.selfID)
	spanKind := spanKindFor(pctx.Direction)
	spanName := requestSpanName(self, protocol, spanOp(pctx, protocol))

	// Facts shared by both spans (exchange.id is appended once the request
	// span exists, since it IS the request span id).
	base := baseAttrs(pctx, self, protocol)

	// Request-span attributes: role + shared facts + request-only facts.
	reqAttrs := make([]attribute.KeyValue, 0, len(base)+8)
	reqAttrs = append(reqAttrs, attribute.String("lineage.role", "request"))
	reqAttrs = append(reqAttrs, base...)
	reqAttrs = p.appendRequestFacts(reqAttrs, pctx, protocol)

	// (3) parent · (4) emit · (4b) mint · (5) re-stamp — wire contract v1.6.
	// The emit is unconditional; the calls around it are the header
	// machinery, and (4b) only acts when no valid traceparent arrived. The
	// read-only "Option 4" variant deletes selectParent and restampTracestate
	// (and the parent.source fact) and sets mint_traceparent: false — see the
	// package doc for the trade-off.
	parent, parentSource := selectParent(ctx, remoteCtx)
	reqAttrs = append(reqAttrs, attribute.String("lineage.parent.source", parentSource))
	reqCtx := p.emitRequestSpan(parent, spanName, spanKind, reqAttrs)
	exchangeID := reqCtx.SpanID().String()
	remoteCtx = p.mintTraceparent(ctx, pctx, remoteCtx, reqCtx)
	restampTracestate(pctx, remoteCtx, exchangeID)

	common := make([]attribute.KeyValue, 0, len(base)+1)
	common = append(common, base...)
	common = append(common, attribute.String("lineage.exchange.id", exchangeID))

	pipeline.SetState(pctx, pluginName, &exchangeState{
		reqCtx:   reqCtx,
		common:   common,
		spanKind: spanKind,
		spanName: spanName,
		protocol: protocol,
	})
	pctx.Observe("recorded_request")
	return pipeline.Action{Type: pipeline.Continue}
}

// selectParent is step (3) of the single-channel parenting mechanism (wire
// contract v1.6): the parent is the tracestate stamp — the previous sidecar
// element in the chain (the caller's outbound for an inbound, this pod's
// inbound for an outbound) — else the wire parent; and when nothing valid is
// on the wire at all, no parent: the request span roots a trace and the fact
// says so ("none") rather than claiming a wire parent that was never there.
// Same precedence in both directions. Guessing an attribution is deliberately
// not among the options: it is worse than declining to give one. Returns the
// parent context and the source label the caller emits as the
// lineage.parent.source fact.
func selectParent(ctx, remoteCtx context.Context) (context.Context, string) {
	rsc := trace.SpanContextFromContext(remoteCtx)
	if !rsc.IsValid() {
		return remoteCtx, "none"
	}
	if psc, ok := stampedParent(rsc); ok {
		return trace.ContextWithRemoteSpanContext(ctx, psc), "tracestate"
	}
	return remoteCtx, "wire"
}

// emitRequestSpan is step (4): emit the request span under parent and end it
// immediately — no span is held open across the exchange. lineage.exchange.id
// is the span's OWN id, so it can only be set after Start. Returns the span's
// context; an ended span's SpanContext remains a valid parent for the response
// span, and its span id is the exchange id.
func (p *LineageTelemetry) emitRequestSpan(
	parent context.Context,
	spanName string,
	spanKind trace.SpanKind,
	reqAttrs []attribute.KeyValue,
) trace.SpanContext {
	_, span := p.tracer.Start(parent, spanName,
		trace.WithSpanKind(spanKind),
		trace.WithAttributes(reqAttrs...),
	)
	sc := span.SpanContext()
	span.SetAttributes(attribute.String("lineage.exchange.id", sc.SpanID().String()))
	span.End()
	return sc
}

// mintTraceparent is step (4b), both directions: when the request arrived
// with NO VALID traceparent, forward one naming this request span, and return
// that context for the re-stamp to build on. Without it the next element has
// nothing to extract — an app's propagate-only shim roots a fresh trace of
// its own and the tracestate stamp never leaves this pod, because W3C reads
// tracestate only alongside a valid traceparent — so the entry exchange lands
// alone in its own trace and every call it caused derives as a parentless
// root (measured live 2026-09-02: one traceparent-less turn, 32 spans, 9
// derived roots instead of 1). Validity is the propagator's verdict, the same
// one selectParent used to record parent.source=none: absent, empty and
// malformed all extract as no context, and all three are restarted here —
// exactly W3C's processing model for an unparseable traceparent (new
// traceparent, tracestate dropped; the re-stamp then writes ours alone). A
// valid traceparent is never modified. Disabled by mint_traceparent: false,
// for a pure observer.
func (p *LineageTelemetry) mintTraceparent(ctx context.Context, pctx *pipeline.Context, remoteCtx context.Context, reqCtx trace.SpanContext) context.Context {
	if !p.cfg.MintTraceparent || trace.SpanContextFromContext(remoteCtx).IsValid() {
		return remoteCtx
	}
	minted := trace.ContextWithRemoteSpanContext(ctx, reqCtx)
	// TraceContext.Inject writes traceparent only; tracestate follows in
	// restampTracestate (reqCtx carries an empty TraceState).
	p.propagator.Inject(minted, propagation.HeaderCarrier(pctx.Headers))
	return minted
}

// restampTracestate is step (5): rewrite the forwarded request's tracestate
// member with this exchange id — both directions. Inbound: the app's
// propagate-only shim couriers it to exactly the outbound calls this inbound
// caused. Outbound: the peer sidecar's inbound reads it as its parent. The
// forwarded traceparent is never modified (see tracestateStampKey). A valid
// traceparent to ride on is required — the wire's, or the one mintTraceparent
// just forwarded; with neither (mint_traceparent off) the shim would root a
// fresh trace and drop the tracestate anyway, so there is nothing to stamp. On
// a minted context the base TraceState is empty, so the caller's tracestate,
// if any rode in with an invalid traceparent, is dropped — W3C's restart
// semantics, not an accident. The listener is
// responsible for propagating these header mutations (ext_proc emits a
// SetHeaders diff).
func restampTracestate(pctx *pipeline.Context, remoteCtx context.Context, exchangeID string) {
	rsc := trace.SpanContextFromContext(remoteCtx)
	if !rsc.IsValid() {
		return
	}
	ts, err := rsc.TraceState().Insert(tracestateStampKey, exchangeID)
	if err != nil {
		// Stamp attempted and refused (tracestate full or a member malformed,
		// W3C caps at 32 members / 512 bytes). Without this line the outcome
		// is indistinguishable from "app has no shim".
		slog.Warn("lineage-telemetry: tracestate stamp rejected; the next element will attribute as wire",
			"exchange_id", exchangeID, "error", err)
		return
	}
	pctx.Headers.Set("tracestate", ts.String())
}

// matchesAnyHost reports whether host matches any configured bypass_hosts
// glob. Semantics follow the convention ibac, sparc and cpex already share
// for this key: path.Match against the host with its port stripped, so
// "otel-collector.*" matches otel-collector.rossoctl-system.svc:4317.
//
// The anchoring is the point. The earlier strings.Contains match was
// unanchored against bare-word defaults, so a legitimate workload at
// prometheus-metrics-agent.team1.svc silently left the lineage graph, and a
// tenant could opt out of being graphed at all by naming a service to contain
// one of the default words. A glob has to match from the first character.
//
// Two deliberate differences from the siblings, both strict improvements:
// the port is split with net.SplitHostPort so an IPv6 literal ([::1]:4317)
// survives, and matching is case-folded because an authority is
// case-insensitive (RFC 3986) — a target spelled OTel-Collector would
// otherwise be recorded while otel-collector was skipped.
func matchesAnyHost(patterns []string, host string) bool {
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, pattern := range patterns {
		if matched, _ := path.Match(strings.ToLower(pattern), host); matched {
			return true
		}
	}
	return false
}

// stampedParent resolves the tracestate stamp on an outbound wire context:
// the inbound exchange id this pod's sidecar wrote into tracestate on the
// forwarded request, carried back by the app's shim. Returns ok=false when
// the member is absent or malformed (caller falls back to the wire parent).
func stampedParent(rsc trace.SpanContext) (trace.SpanContext, bool) {
	raw := rsc.TraceState().Get(tracestateStampKey)
	if raw == "" {
		return trace.SpanContext{}, false
	}
	sid, err := trace.SpanIDFromHex(raw)
	if err != nil {
		return trace.SpanContext{}, false
	}
	psc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    rsc.TraceID(),
		SpanID:     sid,
		TraceFlags: rsc.TraceFlags(),
		Remote:     true,
	})
	return psc, psc.IsValid()
}

// OnResponse is a no-op. The response span is emitted in OnFinish (which fires
// on every finished exchange, including denials and abandonments), not here.
// The method exists only to satisfy the base pipeline.Plugin interface, which
// mandates OnResponse; it carries no logic in the two-span model.
func (p *LineageTelemetry) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// OnFinish emits the response span — the twin of the request span, parented
// under it and echoing the same exchange.id — carrying outcome/status/output.
// Always fires at stream end, so a bodyless or failed exchange still completes
// as a first-class pair. No recover here: the Finisher contract states that
// OnFinish runs best-effort and that panics are recovered and logged, and
// dispatchFinish scopes that recover to one plugin, so a second net would only
// hide the same panic under a different logger.
func (p *LineageTelemetry) OnFinish(ctx context.Context, pctx *pipeline.Context) {
	state := pipeline.GetState[exchangeState](pctx, pluginName)
	if state == nil || !state.reqCtx.IsValid() {
		return
	}

	outcome, status, hasStatus, deniedBy := lineageOutcome(pctx.Outcome())

	attrs := make([]attribute.KeyValue, 0, len(state.common)+5)
	attrs = append(attrs, attribute.String("lineage.role", "response"))
	attrs = append(attrs, state.common...)
	attrs = append(attrs, attribute.String("lineage.outcome", outcome))
	if hasStatus {
		// "http.status_code" (like "http.method" on the request span) is the
		// pre-v1.21 OTel semconv key, kept deliberately: this producer's
		// contract vocabulary is lineage.* + these two well-known HTTP keys,
		// pinned to the wire contract, not the stable OTel names
		// (http.response.status_code / http.request.method). Interop with
		// generic OTel tooling is a non-goal here.
		attrs = append(attrs, attribute.Int("http.status_code", status))
	}
	if deniedBy != "" {
		attrs = append(attrs, attribute.String("lineage.denied_by", deniedBy))
	}
	if p.cfg.CaptureIO {
		if v := ioOutputValue(pctx, state.protocol); v != "" {
			attrs = append(attrs, attribute.String("output.value", truncate(v, p.cfg.MaxPayloadBytes)))
		}
	}

	parent := trace.ContextWithRemoteSpanContext(ctx, state.reqCtx)
	_, span := p.tracer.Start(parent, state.spanName+" response",
		trace.WithSpanKind(state.spanKind),
		trace.WithAttributes(attrs...),
	)
	span.End()
}

// lineageOutcome maps the pipeline's 3-value Outcome (allow/deny/error, nil
// outside OnFinish) onto the contract's lineage.outcome vocabulary
// (ok|denied|error|abandoned) plus the http.status_code fact. "ok" and "denied"
// are the pipeline's own verdicts and carry a status only if one was written —
// an allow that produced none is still an allow. "abandoned" is a nil Outcome,
// or an error that never wrote a status (upstream reset, client disconnect,
// listener death) — the row completes as in-flight-turned-failed rather than
// dangling. hasStatus is false when no status code was produced.
func lineageOutcome(o *pipeline.Outcome) (outcome string, status int, hasStatus bool, deniedBy string) {
	if o == nil {
		return "abandoned", 0, false, ""
	}
	switch o.FinalAction {
	case pipeline.OutcomeAllow:
		return "ok", o.StatusCode, o.StatusCode > 0, ""
	case pipeline.OutcomeDeny:
		return "denied", o.StatusCode, o.StatusCode > 0, o.DenyingPlugin
	case pipeline.OutcomeError:
		if o.StatusCode > 0 {
			return "error", o.StatusCode, true, ""
		}
		return "abandoned", 0, false, ""
	default:
		return "error", o.StatusCode, o.StatusCode > 0, ""
	}
}

// protocolOf reports which parser populated Extensions — the lineage.protocol
// fact. "http" means no parser matched.
func protocolOf(pctx *pipeline.Context) string {
	switch {
	case pctx.Extensions.A2A != nil:
		return "a2a"
	case pctx.Extensions.MCP != nil:
		return "mcp"
	case pctx.Extensions.Inference != nil:
		return "inference"
	default:
		return "http"
	}
}

// spanKindFor maps direction to OTel SpanKind: inbound is SERVER, outbound is
// CLIENT. The response span reuses its request span's kind.
func spanKindFor(dir pipeline.Direction) trace.SpanKind {
	if dir == pipeline.Inbound {
		return trace.SpanKindServer
	}
	return trace.SpanKindClient
}

// baseAttrs returns the facts carried on BOTH spans except exchange.id (added
// once the request span id is known) and role (differs per span).
func baseAttrs(pctx *pipeline.Context, self, protocol string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("lineage.direction", pctx.Direction.String()),
		attribute.String("lineage.self.id", self),
		attribute.String("lineage.protocol", protocol),
	}
	if pctx.Host != "" {
		attrs = append(attrs, attribute.String("lineage.peer.host", pctx.Host))
	}
	return attrs
}

// appendRequestFacts adds the request-only facts: HTTP method/path/scheme, the
// protocol-specific parsed facts, validated-JWT principal (inbound only), and
// input.value when capture_io is on. protocolOf guarantees the matching
// extension pointer is non-nil.
func (p *LineageTelemetry) appendRequestFacts(attrs []attribute.KeyValue, pctx *pipeline.Context, protocol string) []attribute.KeyValue {
	if pctx.Method != "" {
		// "http.method" is the pre-v1.21 OTel semconv key, kept deliberately —
		// see the http.status_code note on the response span.
		attrs = append(attrs, attribute.String("http.method", pctx.Method))
	}
	if pctx.Path != "" {
		attrs = append(attrs, attribute.String("url.path", pctx.Path))
	}
	if pctx.Scheme != "" {
		attrs = append(attrs, attribute.String("url.scheme", pctx.Scheme))
	}
	switch protocol {
	case "a2a":
		a := pctx.Extensions.A2A
		if a.Method != "" {
			attrs = append(attrs, attribute.String("a2a.method", a.Method))
		}
		if a.SessionID != "" {
			attrs = append(attrs, attribute.String("a2a.session_id", a.SessionID))
		}
	case "mcp":
		m := pctx.Extensions.MCP
		if m.Method != "" {
			attrs = append(attrs, attribute.String("mcp.method", m.Method))
		}
		if t := mcpTool(pctx); t != "" {
			attrs = append(attrs, attribute.String("mcp.tool", t))
		}
	case "inference":
		if model := pctx.Extensions.Inference.Model; model != "" {
			attrs = append(attrs, attribute.String("inference.model", model))
		}
	}
	// Principal facts: request span, inbound only, and only from a validated
	// JWT (pctx.Identity non-nil).
	if pctx.Direction == pipeline.Inbound && pctx.Identity != nil {
		if s := pctx.Identity.Subject(); s != "" {
			attrs = append(attrs, attribute.String("lineage.principal.sub", s))
		}
		if c := pctx.Identity.ClientID(); c != "" {
			attrs = append(attrs, attribute.String("lineage.principal.client", c))
		}
	}
	if p.cfg.CaptureIO {
		if v := ioInputValue(pctx, protocol); v != "" {
			attrs = append(attrs, attribute.String("input.value", truncate(v, p.cfg.MaxPayloadBytes)))
		}
	}
	return attrs
}

// truncate bounds a captured payload to max bytes, cutting on a UTF-8
// rune boundary and appending truncatedSuffix so the loss is explicit in the
// span rather than a silent drop at the OTLP exporter's attribute-length limit.
// A non-positive max disables the cap; decode narrows that to exactly -1. The
// returned string, suffix included, never exceeds max bytes.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// Reserve room for the marker; if the marker alone would not fit, drop it
	// and return the prefix. Still back up to a rune boundary so the fallback
	// never emits invalid UTF-8, and never exceed max.
	budget := max - len(truncatedSuffix)
	if budget <= 0 {
		budget = max
		for budget > 0 && !utf8.RuneStart(s[budget]) {
			budget--
		}
		return s[:budget]
	}
	// Back up to a rune boundary so we never split a multi-byte character.
	for budget > 0 && !utf8.RuneStart(s[budget]) {
		budget--
	}
	return s[:budget] + truncatedSuffix
}

// requestSpanName builds "{self} {protocol} {op}", dropping the trailing op
// when it is empty. The response span appends " response".
func requestSpanName(self, protocol, op string) string {
	if op == "" {
		return self + " " + protocol
	}
	return self + " " + protocol + " " + op
}

// spanOp picks the operation label for the span name per protocol:
// mcp.tool / a2a.method / inference.model, falling back to url.path.
func spanOp(pctx *pipeline.Context, protocol string) string {
	var op string
	switch protocol {
	case "a2a":
		if pctx.Extensions.A2A != nil {
			op = pctx.Extensions.A2A.Method
		}
	case "mcp":
		op = mcpTool(pctx)
		if op == "" && pctx.Extensions.MCP != nil {
			op = pctx.Extensions.MCP.Method
		}
	case "inference":
		if pctx.Extensions.Inference != nil {
			op = pctx.Extensions.Inference.Model
		}
	}
	if op == "" {
		op = pctx.Path
	}
	return op
}

// mcpTool returns the tool name for an MCP tools/call, or "" otherwise.
func mcpTool(pctx *pipeline.Context) string {
	m := pctx.Extensions.MCP
	if m == nil || m.Method != "tools/call" || m.Params == nil {
		return ""
	}
	if name, ok := m.Params["name"].(string); ok {
		return name
	}
	return ""
}

// serviceLabel reduces a SPIFFE ID to its last path segment, or returns
// selfID as-is if it is not a SPIFFE URI. Used for the lineage.self.id fact
// and span names.
//
//	"spiffe://trust-domain/ns/team1/sa/weather-service" → "weather-service"
//	"weather-service" → "weather-service"
//
// selfID is never empty at the only call site: Init refuses to start without
// a resolved identity (v1.3). There is deliberately no empty-string fallback —
// inventing a label is the guess that rule exists to forbid.
func serviceLabel(selfID string) string {
	parts := strings.Split(selfID, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return selfID
}

// ioInputValue returns the input.value for a request span: the parsed request
// content for *protocol* — the hop's lineage.protocol fact — or "" if that
// parser produced nothing meaningful. Only that protocol's extension is read:
// parsers are precedence-ordered, not mutually exclusive (mcp-parser matches
// any JSON-RPC body, including every a2a exchange), so falling through to
// another parser's output would attach a mislabeled protocol envelope. A hop
// whose own parser yields nothing keeps a NULL payload — the contract's
// "interactions are independent of payloads".
func ioInputValue(pctx *pipeline.Context, protocol string) string {
	ext := pctx.Extensions
	switch {
	case protocol == "a2a" && ext.A2A != nil && len(ext.A2A.Parts) > 0:
		// Collect all text parts; fall back to JSON if non-text parts present.
		var texts []string
		for _, p := range ext.A2A.Parts {
			if p.Content != "" {
				texts = append(texts, p.Content)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
		if b, err := json.Marshal(ext.A2A.Parts); err == nil {
			return string(b)
		}
	case protocol == "inference" && ext.Inference != nil && len(ext.Inference.Messages) > 0:
		if b, err := json.Marshal(ext.Inference.Messages); err == nil {
			return string(b)
		}
	case protocol == "mcp" && ext.MCP != nil && ext.MCP.Params != nil:
		// For tools/call, surface just the arguments (the semantically
		// meaningful part) rather than the full {"name":…,"arguments":…} wrapper.
		if ext.MCP.Method == "tools/call" {
			if args, ok := ext.MCP.Params["arguments"]; ok {
				if b, err := json.Marshal(args); err == nil {
					return string(b)
				}
			}
		}
		if b, err := json.Marshal(ext.MCP.Params); err == nil {
			return string(b)
		}
	}
	return ""
}

// isA2AProtocolEvent returns true when s is a JSON object carrying an A2A
// transport-level "kind" field (status-update, task-status-update, etc.)
// rather than actual content. Used to avoid surfacing protocol metadata
// as output.value when the a2a-parser captures a protocol event as the
// artifact instead of the real agent response text.
func isA2AProtocolEvent(s string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &obj) != nil {
		return false
	}
	var kind string
	if raw, ok := obj["kind"]; ok {
		_ = json.Unmarshal(raw, &kind)
	}
	// A2A protocol event kinds are enumerated and stable, so match exactly:
	// a substring test would suppress a legitimate agent-defined artifact whose
	// kind merely contains one of these words (e.g. "final-status-report").
	switch kind {
	case "status-update", "task-status-update", "artifact-update", "working", "canceled":
		return true
	default:
		return false
	}
}

// ioOutputValue returns the output.value for a response span: the parsed
// response content for *protocol* — the REQUEST span's lineage.protocol fact —
// or "" if that parser produced nothing. Only that protocol's extension is
// read, for the same reason as ioInputValue: mcp-parser also parses every a2a
// response (any JSON-RPC body), and falling through to it would emit the raw
// JSON-RPC envelope — including the protocol events isA2AProtocolEvent exists
// to suppress — as an a2a hop's payload.
func ioOutputValue(pctx *pipeline.Context, protocol string) string {
	ext := pctx.Extensions
	switch {
	case protocol == "a2a" && ext.A2A != nil && ext.A2A.Artifact != "" && !isA2AProtocolEvent(ext.A2A.Artifact):
		return ext.A2A.Artifact
	case protocol == "a2a" && ext.A2A != nil && ext.A2A.ErrorMessage != "":
		return ext.A2A.ErrorMessage
	case protocol == "inference" && ext.Inference != nil && ext.Inference.Completion != "":
		return ext.Inference.Completion
	case protocol == "inference" && ext.Inference != nil && len(ext.Inference.ToolCalls) > 0:
		if b, err := json.Marshal(ext.Inference.ToolCalls); err == nil {
			return string(b)
		}
	case protocol == "mcp" && ext.MCP != nil && ext.MCP.Result != nil:
		// For tools/call results, extract the text content from the MCP
		// content array rather than returning the full {"content":[…],"_meta":…}
		// envelope, so the output matches what Phoenix shows for the tool span.
		if ext.MCP.Method == "tools/call" {
			if content, ok := ext.MCP.Result["content"]; ok {
				if items, ok := content.([]any); ok {
					var texts []string
					for _, item := range items {
						if m, ok := item.(map[string]any); ok {
							if m["type"] == "text" {
								if t, ok := m["text"].(string); ok && t != "" {
									texts = append(texts, t)
								}
							}
						}
					}
					if len(texts) > 0 {
						return strings.Join(texts, "\n")
					}
				}
			}
		}
		if b, err := json.Marshal(ext.MCP.Result); err == nil {
			return string(b)
		}
	case protocol == "mcp" && ext.MCP != nil && ext.MCP.Err != nil:
		if b, err := json.Marshal(ext.MCP.Err); err == nil {
			return string(b)
		}
	}
	return ""
}

// Compile-time interface assertions.
var (
	_ pipeline.Plugin       = (*LineageTelemetry)(nil)
	_ pipeline.Configurable = (*LineageTelemetry)(nil)
	_ pipeline.Initializer  = (*LineageTelemetry)(nil)
	_ pipeline.Shutdowner   = (*LineageTelemetry)(nil)
	_ pipeline.Finisher     = (*LineageTelemetry)(nil)
	_ pipeline.Readier      = (*LineageTelemetry)(nil)
)

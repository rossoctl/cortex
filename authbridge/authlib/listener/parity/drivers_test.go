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
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/metadata"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/extproc"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/forwardproxy"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/reverseproxy"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
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

	// pipelineRefusedPreRun asserts the listener refused before the
	// pipeline (e.g. body overflow). Default false: every listener must
	// record. Combine with expectedWireStatus to pin the wire code.
	pipelineRefusedPreRun bool

	// expectedWireStatus, when non-zero, is asserted against every
	// listener's wire status. Only meaningful together with
	// pipelineRefusedPreRun — on the success path extproc has no HTTP
	// transport and reports 0.
	expectedWireStatus int

	// expectedPluginEvents anchors correctness — maps each expected
	// SessionEvent.Plugins key to its exact JSON. Empty means "don't
	// assert content beyond the pairwise diff." Fixtures that want
	// bug-catching (not just drift-catching) fill this in.
	expectedPluginEvents map[string]string
}

// contentType returns the fixture's response content-type or a sensible
// default. Kept as a helper so both driver paths stay compact.
func (f fixture) contentType() string {
	if f.upstreamContentType != "" {
		return f.upstreamContentType
	}
	return "application/json"
}

// buildSpyPipeline routes construction through plugins.BuildWithDeps
// so Requires / RequiresLater fire at build time on every driver.
func buildSpyPipeline(entries []config.PluginEntry) (*pipeline.Pipeline, error) {
	return plugins.BuildWithDeps(entries, plugins.Deps{})
}

// spyEntry builds a config.PluginEntry for spyPluginA/B with the given
// knobs. Marshals spyConfig once so fixture literals stay compact.
func spyEntry(name string, cfg spyConfig) config.PluginEntry {
	raw, _ := json.Marshal(cfg)
	return config.PluginEntry{Name: name, Config: raw}
}

// observation is the parity-comparable snapshot of a listener's session
// event. Covers the operator-facing wire surface; per-listener locals
// (Host casing, timestamps, RequestID, Duration, TLS, Identity) are
// excluded — expanding coverage there is a follow-up fixture pass.
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
}

// observe returns the sole event matching (direction, phase) in the
// DefaultSessionID bucket, folded into the parity-comparable shape.
// Returns nil when the bucket is empty or the phase is absent. Fails
// on more than one match so duplicate-record drift surfaces here.
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
		PluginEventJSON: map[string]string{},
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
			})
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

	spyPipe, err := buildSpyPipeline(f.entries)
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
	// what Envoy does at the ext_proc filter (server.go:113).
	if len(f.reqBody) > 0 && spyPipe.NeedsBody() {
		reqs = append(reqs, &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_RequestBody{
				RequestBody: &extprocv3.HttpBody{Body: f.reqBody, EndOfStream: true},
			},
		})
	}
	if f.upstreamStatus > 0 {
		reqs = append(reqs, &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_ResponseHeaders{
				ResponseHeaders: &extprocv3.HttpHeaders{
					Headers: makeHeaders(
						":status", fmt.Sprintf("%d", f.upstreamStatus),
						"content-type", f.contentType(),
						"content-length", fmt.Sprintf("%d", len(f.upstreamBody)),
					),
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
		// The second half is not a tidy-up. This append used to fire on
		// NeedsBody() alone, so every fixture with an empty upstream body got a
		// synthetic zero-length ResponseBody message that real Envoy never
		// sends — which made the body-less shape unrepresentable here and let
		// extproc drop the cost and the response row for every 204, 304 and
		// error-status-on-headers while a suite whose entire purpose is catching
		// per-listener divergence stayed green. A harness that cannot express a
		// shape cannot notice a listener mishandling it.
		if spyPipe.NeedsBody() && len(f.upstreamBody) > 0 {
			reqs = append(reqs, &extprocv3.ProcessingRequest{
				Request: &extprocv3.ProcessingRequest_ResponseBody{
					ResponseBody: &extprocv3.HttpBody{Body: f.upstreamBody, EndOfStream: true},
				},
			})
		}
	}

	stream := &mockStream{ctx: context.Background(), requests: reqs}
	_ = srv.Process(stream)

	// Wire guards matching the HTTP drivers' upstreamHit check: consume
	// every message, and reject fixtures must send an ImmediateResponse.
	if stream.recvIdx != len(reqs) {
		t.Errorf("extproc: consumed %d of %d messages; listener bailed", stream.recvIdx, len(reqs))
	}
	if f.upstreamStatus == 0 {
		if n := len(stream.responses); n == 0 {
			t.Errorf("extproc: fixture %q asked for deny but no response was sent", f.name)
		} else if last := stream.responses[n-1]; last.GetImmediateResponse() == nil {
			t.Errorf("extproc: fixture %q asked for deny but last response was %T, want ImmediateResponse", f.name, last.Response)
		}
	}

	return finalizeObservation(t, f, observe(t, store, f.direction, wantPhase), extprocWireStatus(stream))
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
// observation. pipelineRefusedPreRun is a strict expectation: the
// listener MUST refuse before the pipeline. A missing event when the
// fixture didn't opt in is a bug; an event present when it did is
// also a bug (the listener silently stopped enforcing the cap).
func finalizeObservation(t *testing.T, f fixture, obs *observation, wireStatus int) *observation {
	t.Helper()
	if obs == nil {
		if !f.pipelineRefusedPreRun {
			t.Errorf("no session event recorded for fixture %q; set pipelineRefusedPreRun=true if expected", f.name)
			return nil
		}
		return &observation{PipelineRan: false, WireStatus: wireStatus}
	}
	if f.pipelineRefusedPreRun {
		t.Errorf("fixture %q expected the listener to refuse before the pipeline, but an event was recorded (wireStatus=%d)", f.name, wireStatus)
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

	var upstreamHit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		if f.upstreamStatus == 0 {
			t.Errorf("reverseproxy: upstream unexpectedly reached on deny fixture %q", f.name)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", f.contentType())
		w.WriteHeader(f.upstreamStatus)
		_, _ = w.Write(f.upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	p, err := buildSpyPipeline(f.entries)
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

	return finalizeObservation(t, f, observe(t, store, pipeline.Inbound, wantPhase), resp.StatusCode)
}

// --- forwardproxy driver -------------------------------------------------

// runForwardProxy drives the fixture through the forward-proxy listener
// via a proxy-configured http.Client. Outbound-only; this is the
// listener agents egress through in the laptop (proxy-sidecar) shape.
func runForwardProxy(t *testing.T, f fixture, wantPhase pipeline.SessionPhase) *observation {
	t.Helper()
	if f.direction != pipeline.Outbound {
		t.Fatalf("forwardproxy handles Outbound only, got fixture %q direction=%v", f.name, f.direction)
	}

	var upstreamHit bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		if f.upstreamStatus == 0 {
			t.Errorf("forwardproxy: upstream unexpectedly reached on deny fixture %q", f.name)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", f.contentType())
		w.WriteHeader(f.upstreamStatus)
		_, _ = w.Write(f.upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	p, err := buildSpyPipeline(f.entries)
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

	return finalizeObservation(t, f, observe(t, store, pipeline.Outbound, wantPhase), resp.StatusCode)
}

// --- construction-only helpers -------------------------------------------

// tryBuild* replay each driver's construction (pipeline + listener
// NewServer / Server init) and return the first error.

func tryBuildExtproc(entries []config.PluginEntry) error {
	spyPipe, err := buildSpyPipeline(entries)
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
	p, err := buildSpyPipeline(entries)
	if err != nil {
		return err
	}
	_, err = reverseproxy.NewServer(pipeline.NewHolder(p), nil, "http://parity.local", nil)
	return err
}

func tryBuildForwardProxy(entries []config.PluginEntry) error {
	p, err := buildSpyPipeline(entries)
	if err != nil {
		return err
	}
	_, err = forwardproxy.NewServer(pipeline.NewHolder(p), nil, nil)
	return err
}

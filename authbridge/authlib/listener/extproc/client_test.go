package extproc

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
)

// TestExtProcRecordersCarryTheClient is a POPULATION guard, the same shape and for
// the same reason as TestExtProcRecordersCarryMethodAndPath: the reflection guards
// in pipeline catch "the field never reaches the wire", but a recorder that simply
// forgets Client serializes a clean nil and every other test stays green. That is
// the defect this file exists to make impossible — an unwired call site has already
// shipped once on this branch.
//
// One subtest per recorder, so a failure names the site rather than the file.
func TestExtProcRecordersCarryTheClient(t *testing.T) {
	// Fixture mirrors newPctx in httppath_test.go — each recorder needs whatever
	// makes its own gate open, and the response recorders filter invocations on
	// InvocationPhaseResponse, so the fixture carries an entry in both phases.
	// Headers is the addition: it is where ClientInfo reads the User-Agent from.
	newPctx := func(dir pipeline.Direction, phase pipeline.InvocationPhase, ua string) *pipeline.Context {
		h := http.Header{}
		if ua != "" {
			h.Set("User-Agent", ua)
		}
		return &pipeline.Context{
			Direction: dir,
			Method:    "POST",
			Host:      "api.openai.com",
			Path:      "/v1/chat/completions",
			Headers:   h,
			StartedAt: time.Now(),
			Extensions: pipeline.Extensions{
				Invocations: &pipeline.Invocations{
					Inbound: []pipeline.Invocation{{
						Plugin: "jwt-validation", Phase: phase,
						Action: pipeline.ActionAllow, Reason: "authorized",
					}},
					Outbound: []pipeline.Invocation{{
						Plugin: "token-exchange", Phase: phase,
						Action: pipeline.ActionAllow, Reason: "exchanged",
					}},
				},
			},
		}
	}

	recorders := []struct {
		name   string
		record func(s *Server, pctx *pipeline.Context)
		dir    pipeline.Direction
		phase  pipeline.InvocationPhase
	}{
		{"recordInboundSession", func(s *Server, p *pipeline.Context) { s.recordInboundSession(p) },
			pipeline.Inbound, pipeline.InvocationPhaseRequest},
		{"recordOutboundSession", func(s *Server, p *pipeline.Context) { s.recordOutboundSession(p) },
			pipeline.Outbound, pipeline.InvocationPhaseRequest},
		{"recordInboundResponseSession", func(s *Server, p *pipeline.Context) { s.recordInboundResponseSession(p) },
			pipeline.Inbound, pipeline.InvocationPhaseResponse},
		{"recordOutboundResponseSession", func(s *Server, p *pipeline.Context) { s.recordOutboundResponseSession(p) },
			pipeline.Outbound, pipeline.InvocationPhaseResponse},
		{"recordInboundReject", func(s *Server, p *pipeline.Context) {
			s.recordInboundReject(p, pipeline.Action{Type: pipeline.Reject})
		}, pipeline.Inbound, pipeline.InvocationPhaseRequest},
		{"recordOutboundReject", func(s *Server, p *pipeline.Context) {
			s.recordOutboundReject(p, pipeline.Action{Type: pipeline.Reject})
		}, pipeline.Outbound, pipeline.InvocationPhaseRequest},
	}

	firstEvent := func(t *testing.T, name string, rec func(*Server, *pipeline.Context), pctx *pipeline.Context) pipeline.SessionEvent {
		t.Helper()
		store := session.New(5*time.Minute, 100, 0)
		defer store.Close()
		rec(&Server{Sessions: store}, pctx)
		v := store.View(session.DefaultSessionID)
		if v == nil || len(v.Events) == 0 {
			t.Fatalf("%s recorded no event; cannot assert population", name)
		}
		return v.Events[0]
	}

	// BOTH DIRECTIONS IN ONE SUBTEST, per recorder. They used to be two loops, and the
	// absence half asserted only "want nil" — which a recorder that never populates Client
	// at all satisfies, so deleting the `Client:` assignment left that half green while it
	// read as coverage of the same contract. Present-then-absent through the same recorder
	// is what makes each subtest able to fail on its own: the first assertion catches an
	// unwired call site, the second catches one that fabricates a label for traffic that
	// named no agent.
	//
	// The nil's Label() is deliberately NOT asserted. Label is nil-safe by construction, so
	// `ev.Client.Label() == "unknown"` restates pipeline.EventClient.Label's own contract —
	// pinned by its "nil is unknown" table row in that package — and cannot fail for
	// anything a recorder here does or omits.
	for _, tc := range recorders {
		t.Run(tc.name, func(t *testing.T) {
			ev := firstEvent(t, tc.name, tc.record, newPctx(tc.dir, tc.phase, "claude-cli/2.1.14 (external, cli)"))
			if ev.Client == nil {
				t.Fatalf("%s: Client is nil; the recorder does not copy pctx.ClientInfo()", tc.name)
			}
			if ev.Client.Name != "claude-code" || ev.Client.Version != "2.1.14" {
				t.Errorf("%s: Client = %+v, want claude-code/2.1.14", tc.name, ev.Client)
			}

			absent := firstEvent(t, tc.name, tc.record, newPctx(tc.dir, tc.phase, ""))
			if absent.Client != nil {
				t.Errorf("%s: Client = %+v, want nil for a request with no User-Agent; an invented agent in a cost table reads as a real program that spent real money",
					tc.name, absent.Client)
			}
		})
	}
}

// uaRewritePlugin rewrites the User-Agent on pctx.Headers during OnRequest — what every
// header-mutating plugin does, and what decides attribution if the client label is
// resolved after the pipeline rather than before it. Its counterpart in forwardproxy's
// client_test.go is the model. It is the plugin that does not exist yet: nothing in the
// tree rewrites this header today, which is exactly why the ordering below has to be
// pinned by a test rather than left to hold by luck.
//
// set == "" DELETES the header instead, which is the same defect run backwards.
//
// It records an Invocation in BOTH phases because every extproc recorder is gated on
// having something to report, and the response recorders filter invocations on
// InvocationPhaseResponse — without the OnResponse row the response event is never
// appended and half of what these tests claim to assert would not exist.
//
// readsBody routes the request through handleInboundBody / handleOutboundBody instead of
// handleInbound / handleOutbound. That is the only way to reach the other two
// construction sites: Envoy's separate body message is what creates their Context.
type uaRewritePlugin struct {
	set       string
	readsBody bool
}

func (p *uaRewritePlugin) Name() string { return "ua-rewrite" }
func (p *uaRewritePlugin) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{ReadsBody: p.readsBody}
}
func (p *uaRewritePlugin) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	if p.set == "" {
		pctx.Headers.Del("User-Agent")
	} else {
		pctx.Headers.Set("User-Agent", p.set)
	}
	pctx.Modify("rewrote user-agent")
	return pipeline.Action{Type: pipeline.Continue}
}
func (p *uaRewritePlugin) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	pctx.Observe("saw response")
	return pipeline.Action{Type: pipeline.Continue}
}

// uaShape is one of the four Context-construction sites in server.go, addressed the only
// way a test can address them: by the stream shape that makes Process route to it. All
// four build their Context inside a handler with no callable seam, so a direct unit call
// is not available — TestExtProcRecordersCarryTheClient above calls the recorders
// directly and therefore cannot see the ordering at all.
type uaShape struct {
	name      string
	direction string
	readsBody bool
}

var uaShapes = []uaShape{
	{"handleInbound", "inbound", false},
	{"handleInboundBody", "inbound", true},
	{"handleOutbound", "outbound", false},
	{"handleOutboundBody", "outbound", true},
}

// runUAStream drives one full ext_proc stream — request headers, optional request body,
// response headers, optional response body — through Process, and returns the events it
// recorded. sentUA is what the CLIENT put on the wire; pluginUA is what the plugin
// rewrites it to ("" deletes it).
func runUAStream(t *testing.T, shape uaShape, sentUA, pluginUA string) []pipeline.SessionEvent {
	t.Helper()

	p, err := pipeline.New([]pipeline.Plugin{&uaRewritePlugin{set: pluginUA, readsBody: shape.readsBody}})
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	// One pipeline bound to both holders: a stream exercises exactly one direction, and
	// binding both keeps the fixture from depending on which holder the listener picks.
	srv := &Server{
		InboundPipeline:  pipeline.NewHolder(p),
		OutboundPipeline: pipeline.NewHolder(p),
		Sessions:         store,
	}

	reqBody := []byte(`{"model":"claude-opus-4"}`)
	kvs := []string{":method", "POST", ":path", "/v1/messages", ":authority", "api.anthropic.com"}
	if shape.direction == "inbound" {
		kvs = append(kvs, "x-authbridge-direction", "inbound")
	}
	if sentUA != "" {
		// Omitted entirely rather than sent blank when there is no client, which is the
		// shape net/http produces and the "no client" case the recorders must keep as
		// absence.
		kvs = append(kvs, "user-agent", sentUA)
	}
	if shape.readsBody {
		// requestHasBody gates the body handshake on this; without it Envoy's body
		// message is never requested and the headers-only handler runs instead.
		kvs = append(kvs, "content-length", strconv.Itoa(len(reqBody)))
	}

	reqs := []*extprocv3.ProcessingRequest{{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: makeHeaders(kvs...)},
		},
	}}
	if shape.readsBody {
		reqs = append(reqs, &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_RequestBody{
				RequestBody: &extprocv3.HttpBody{Body: reqBody},
			},
		})
	}
	reqs = append(reqs, &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{Headers: makeHeaders(":status", "200")},
		},
	})
	if shape.readsBody {
		// With a body-reading pipeline the response event is recorded in
		// handleResponseBody, not handleResponseHeaders — that phase returns a
		// ModeOverride and defers. Without this message the body shapes would assert
		// only the request half.
		reqs = append(reqs, &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_ResponseBody{
				ResponseBody: &extprocv3.HttpBody{Body: []byte(`{"ok":true}`)},
			},
		})
	}

	_ = srv.Process(&mockStream{ctx: context.Background(), requests: reqs}) // EOF from Recv ends the stream

	v := store.View(session.DefaultSessionID)
	if v == nil {
		t.Fatalf("%s: no session recorded, so nothing here is asserted", shape.name)
	}
	return v.Events
}

// assertBothPhasesName checks the client on EVERY event the stream recorded, and that
// both phases are present to be checked.
//
// Both halves matter. A stream that recorded nothing, or only its request phase, would
// make a per-event assertion vacuously true — and per-phase is not cosmetic here: the
// request and response events are two separate ClientInfo() calls on one Context, so a
// pin that covered only the first would halve every per-agent figure.
//
// wantRaw == "" means "want no client at all".
func assertBothPhasesName(t *testing.T, shape uaShape, evs []pipeline.SessionEvent, wantRaw string) {
	t.Helper()

	var sawRequest, sawResponse bool
	for _, ev := range evs {
		switch ev.Phase {
		case pipeline.SessionRequest:
			sawRequest = true
		case pipeline.SessionResponse:
			sawResponse = true
		}
		if wantRaw == "" {
			if ev.Client != nil {
				t.Errorf("%s phase %s: Client = %+v, want nil — the client sent no User-Agent and a "+
					"plugin wrote one into pctx.Headers, so this row credits the spend to an agent that "+
					"never made the call", shape.name, ev.Phase, ev.Client)
			}
			continue
		}
		if ev.Client == nil {
			t.Errorf("%s phase %s: Client is nil; a plugin rewrote the header and the agent that "+
				"actually made this call became untagged traffic", shape.name, ev.Phase)
			continue
		}
		if ev.Client.Raw != wantRaw {
			t.Errorf("%s phase %s: Client.Raw = %q, want the User-Agent the CLIENT sent (%q); the "+
				"attribution followed a plugin's rewrite, so this request's spend is filed under a "+
				"program that never made it", shape.name, ev.Phase, ev.Client.Raw, wantRaw)
		}
		if ev.Client.Name != "claude-code" {
			t.Errorf("%s phase %s: Client.Name = %q, want claude-code", shape.name, ev.Phase, ev.Client.Name)
		}
	}
	if !sawRequest {
		t.Errorf("%s: no request-phase event recorded; the construction site under test is unasserted", shape.name)
	}
	if !sawResponse {
		t.Errorf("%s: no response-phase event recorded; a pin that covered only the request phase would pass", shape.name)
	}
}

// TestExtProc_ClientIsResolvedAtConstructionNotOnFirstUse is the ordering guard extproc
// lacked while forwardproxy had it, over all four of this listener's Context-construction
// sites — six of the ten `Client:` recording sites in the tree live behind them.
//
// Context.ClientInfo memoizes on FIRST CALL, and every call site in server.go is an
// event-construction site downstream of the pipeline. pctx.Headers is the listener's own
// copy of the wire headers and plugins write to it, so without the pin at construction the
// attribution of a request is decided by whichever recorder asks first, AFTER the pipeline
// has had its way with the header.
//
// This distinguishes the two: the plugin rewrites User-Agent to an agent that never made
// the call, and every event must still name the one that did. It fails if the pin is
// removed and equally if it is merely MOVED after Run — which is the whole difference
// between "resolved once" and "resolved before anything can change it".
//
// The delete row is the same defect in the other direction: a plugin that removes the
// header cannot turn a named agent into untagged traffic. Kept separate because a
// "resolve later" bug that happens to preserve names could still lose them.
//
// Latent today (no plugin in the tree touches this header) and asserted anyway, because
// the failure mode is silent: spend re-filed under another program's name, with nothing
// in the event to say it happened.
func TestExtProc_ClientIsResolvedAtConstructionNotOnFirstUse(t *testing.T) {
	const sentUA = "claude-cli/2.1.14 (external, cli)"

	for _, mut := range []struct{ name, pluginUA string }{
		{"plugin_rewrites_ua", "impostor/9.9"},
		{"plugin_deletes_ua", ""},
	} {
		for _, shape := range uaShapes {
			t.Run(mut.name+"/"+shape.name, func(t *testing.T) {
				evs := runUAStream(t, shape, sentUA, mut.pluginUA)
				assertBothPhasesName(t, shape, evs, sentUA)

				// Both events agree. Without the pin the two recorders parse the same
				// mutated header and agree on the WRONG answer, so this is the weaker
				// half — kept because a future change that pinned only one phase would
				// halve every per-agent figure.
				for i := 1; i < len(evs); i++ {
					if a, b := evs[0].Client.Label(), evs[i].Client.Label(); a != b {
						t.Errorf("%s: events disagree about the client (%q vs %q); the answer depends on "+
							"which recorder asked first", shape.name, a, b)
					}
				}
			})
		}
	}
}

// TestExtProc_APluginCannotInventAClient is the absence case, and the direction that puts
// a fictional line item on a cost table: the client sent NO User-Agent, a plugin writes
// one, and every event must still record no client.
//
// Separate from the test above because it is the assertion a pin-less implementation can
// pass by accident — nil is what an unwired recorder produces too — and because absence is
// the answer that has to survive a plugin, not just be preserved by one. An invented agent
// in a cost table reads as a real program that spent real money; TestExtProcRecordersCarryTheClient
// makes the same claim about the recorders, this one makes it about the ORDER.
func TestExtProc_APluginCannotInventAClient(t *testing.T) {
	for _, shape := range uaShapes {
		t.Run(shape.name, func(t *testing.T) {
			assertBothPhasesName(t, shape, runUAStream(t, shape, "", "impostor/9.9"), "")
		})
	}
}

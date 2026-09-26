package parity

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/costing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins"
	// REGISTERED BY IMPORTING IT, which is how a deployment gets it too: the plugin's init
	// calls plugins.RegisterPlugin, and a binary that never imports the package cannot build a
	// pipeline naming it. Blank because nothing here calls the package directly — the point is
	// to reach the real plugin through the same registry and the same builder production uses,
	// rather than constructing it and hand-wiring a resolver.
	_ "github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/pricing"
)

// THE COST RECORD, COMPARED ACROSS ALL THREE LISTENERS, WITH THE REAL COST OWNER WIRED.
//
// The suite next door compares the dispatch SHAPE using spy plugins: how many frames, which one
// carries last, what order. Nothing compared what a real cost owner DID with those frames, so each
// listener's cost behaviour was pinned only by its own tests, written at different times against
// different fixtures — and every cost defect found in review of this PR was of exactly one shape:
// a fold or a finalization that works on one listener and not on another.
//
//	the reverse proxy's finalization deadline started when response headers arrived, so any
//	stream longer than ten seconds settled nothing. No reverseproxy test drove a stream past ~20ms.
//	ext_proc lost the output tally for an SSE event split across two ResponseBody messages.
//	the forward proxy's buffered fallback — and then its PRIMARY buffered path — finalized on the
//	request context, so a hangup produced no settled cost and no response row.
//
// Each was found by reading, one per round. A table like this one fails on all of them at once.
//
// WHAT IT CATCHES, MEASURED, not assumed. Reverting the SSE reassembly fails the split fixture
// three ways at once — the figure ($0.0076 against $0.0191), a false "output-uncounted" disclosure,
// and ext_proc disagreeing with the forward proxy. Flipping costing's precedence rule fails the
// header fixture on ALL THREE listeners while they still agree with each other, which is the case
// the absolute expectations exist for.
//
// WHAT IT CANNOT CATCH, so nobody reads it as more than it is: every defect that needs an ADVERSE
// CONDITION rather than a shape. A client hanging up mid-response, a stream that outlives the
// finalization deadline, an Envoy stream torn down without end_of_stream — none of those can be
// expressed in a fixture that all three drivers can run, because each is a property of one
// transport. They stay pinned by per-listener tests, and the structural guard against the whole
// class is the dispatch-site table in dispatch_audit_test.go.
//
// ABSOLUTE, THEN PAIRWISE, and that order is the point. Every listener is checked against an
// EXPECTED record first, because two listeners agreeing on a wrong figure is the failure mode a
// pure parity assertion cannot see — and "they agree" is exactly what a shared bug in costing
// would produce. The pairwise check then catches the drift no single expectation would: a field
// nobody thought to assert.

// costRates is the rate table these fixtures price against: DISTINCT per tier, so an expectation
// says which tier each count was charged at rather than only how many tokens there were.
//
//	input 7 micros/token   cache-write 11   cache-read 3   output 23
func costRates(t *testing.T) *pricing.Registry {
	t.Helper()
	var r pricing.Rates
	for tier, perToken := range map[pricing.Tier]float64{
		pricing.TierInput:      7e-6,
		pricing.TierCacheWrite: 11e-6,
		pricing.TierCacheRead:  3e-6,
		pricing.TierOutput:     23e-6,
	} {
		r.Base[tier], r.Set[tier] = perToken, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatalf("pricing.NewTable: %v", err)
	}
	return pricing.NewRegistry(tab)
}

// inferenceParserEntry names the real plugin, built through the same registry production builds
// from. No config: the rate table arrives as a dependency, exactly as it does in a deployment.
var inferenceParserEntry = config.PluginEntry{Name: "inference-parser"}

const (
	anthropicRequest = `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`
	streamRequest    = `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// A buffered Anthropic response: 1,000 uncached input, 200 cache reads, 500 output.
	anthropicBuffered = `{"model":"claude-opus-5","usage":{"input_tokens":1000,` +
		`"cache_read_input_tokens":200,"output_tokens":500},"stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"reply"}]}`

	// The same turn, streamed: the prompt split lands on message_start and the output tally only
	// on message_delta, which is what makes a premature finalization cost money.
	anthropicStreamed = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_read_input_tokens":200,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"reply"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
)

// splitInsideOutputTally is a byte offset inside message_delta's usage object — after
// `"usage":{"outp` and before the count — so a body cut there leaves two fragments that each parse
// to nothing. Computed rather than written as a literal, so editing the fixture cannot move the cut
// somewhere harmless while this test keeps claiming to make the point.
var splitInsideOutputTally = func() int {
	const marker = `"usage":{"outp`
	i := strings.Index(anthropicStreamed, marker)
	if i < 0 {
		panic("the streamed fixture no longer contains the output tally this cut is supposed to split")
	}
	return i + len(marker)
}()

// modelledWholeUSD is the figure the table above produces for either shape of that turn:
// 1000 input at 7 + 200 cache reads at 3 + 500 output at 23 micros.
const modelledWholeUSD = (1000*7 + 200*3 + 500*23) / 1e6

// costFixture pairs a parity fixture with the record every listener must produce for it.
type costFixture struct {
	fixture
	want costevent.Event
}

func costFixtures(t *testing.T, direction pipeline.Direction) []costFixture {
	t.Helper()
	deps := plugins.Deps{Pricing: costRates(t)}
	base := func(name string) fixture {
		return fixture{
			name:      name,
			direction: direction,
			entries:   []config.PluginEntry{inferenceParserEntry},
			method:    "POST",
			path:      "/v1/messages",
			deps:      deps,
		}
	}
	return []costFixture{{
		// THE MODELLED FIGURE. No cost header at all, which is the Anthropic shape: the token
		// counters and the rate table are the only figure there is.
		fixture: func() fixture {
			f := base("buffered, modelled from the counters")
			f.reqBody = []byte(anthropicRequest)
			f.upstreamStatus = 200
			f.upstreamBody = []byte(anthropicBuffered)
			return f
		}(),
		want: costevent.Event{
			CostUSD:    modelledWholeUSD,
			Source:     costevent.SourceUsageFallback,
			Provenance: pricing.ProvConfigured.String(),
			Settled:    true,
			PromptUSD:  (1000*7 + 200*3) / 1e6,
			OutputUSD:  500 * 23 / 1e6,
		},
	}, {
		// THE GATEWAY'S OWN FIGURE WINS, and the modelled one is computed alongside for drift.
		// The precedence rule is the reason costing exists, so every listener has to apply it.
		fixture: func() fixture {
			f := base("buffered, gateway header wins")
			f.reqBody = []byte(anthropicRequest)
			f.upstreamStatus = 200
			f.upstreamBody = []byte(anthropicBuffered)
			f.upstreamHeaders = map[string]string{costing.ResponseCostHeader: "0.0075"}
			return f
		}(),
		want: costevent.Event{
			CostUSD:    0.0075,
			Source:     costevent.SourceGatewayHeader,
			Provenance: pricing.ProvAuthoritative.String(),
			Settled:    true,
			PromptUSD:  (1000*7 + 200*3) / 1e6,
			OutputUSD:  500 * 23 / 1e6,
		},
	}, {
		// THE STREAMED TURN. Same money as the buffered one, arriving as frames — which is where
		// the listeners differ most: per-frame dispatch, a terminal frame, and a finalization
		// context. A premature finalization prices the prompt and loses the output.
		fixture: func() fixture {
			f := base("streamed, folded across frames")
			f.reqBody = []byte(streamRequest)
			f.upstreamStatus = 200
			f.upstreamContentType = "text/event-stream"
			f.upstreamBody = []byte(anthropicStreamed)
			// LiteLLM's placeholder zero on a stream: it means nothing, and treating it as a
			// declared-free call would publish $0 for a turn that cost money.
			f.upstreamHeaders = map[string]string{costing.ResponseCostHeader: "0"}
			return f
		}(),
		want: costevent.Event{
			CostUSD:    modelledWholeUSD,
			Source:     costevent.SourceUsageFallback,
			Provenance: pricing.ProvConfigured.String(),
			Settled:    true,
			PromptUSD:  (1000*7 + 200*3) / 1e6,
			OutputUSD:  500 * 23 / 1e6,
		},
	}, {
		// A REFUSED HEADER on a path the parser cannot read. The figure is declined and the
		// refusal published, so the coverage gap stays nameable instead of looking like a
		// response that reported no cost.
		fixture: func() fixture {
			f := base("an implausible header on an unparsed path is refused")
			f.path = "/v1/embeddings"
			f.reqBody = []byte(`{"input":"hi"}`)
			f.upstreamStatus = 200
			f.upstreamBody = []byte(`{"data":[]}`)
			f.upstreamHeaders = map[string]string{costing.ResponseCostHeader: "1000000000"}
			return f
		}(),
		want: costevent.Event{
			RejectedReason: costevent.RejectedImplausible,
			// "none" rather than empty: NewRecord stringifies the provenance whatever it is, and
			// a refused figure has none. Worth pinning — a consumer rendering provenance beside
			// the money needs the refusal to read as "no rates were involved" and not as a gap
			// in the table.
			Provenance: pricing.ProvNone.String(),
		},
	}, {
		// THE SPEND THIS SERIES NEWLY ADMITS: an endpoint the parser cannot read, priced from the
		// gateway's own header. Before cost was settled on every proxied response this figure
		// reached nothing, and it is also the path the plausibility cap guards — so all three
		// listeners have to agree on both halves, the believed figure here and the refused one
		// above.
		fixture: func() fixture {
			f := base("an unparsed path is priced from its header")
			f.path = "/v1/embeddings"
			f.reqBody = []byte(`{"input":"hi"}`)
			f.upstreamStatus = 200
			f.upstreamBody = []byte(`{"data":[]}`)
			f.upstreamHeaders = map[string]string{costing.ResponseCostHeader: "0.25"}
			return f
		}(),
		want: costevent.Event{
			CostUSD:    0.25,
			Source:     costevent.SourceGatewayHeader,
			Provenance: pricing.ProvAuthoritative.String(),
			Settled:    true,
			// No halves: there is no usage to model on a path the parser could not read, and
			// inventing them from the header would attribute a figure nobody split.
		},
	}, {
		// THE SAME STREAMED TURN, CUT MID-EVENT. Envoy's body messages are chunks of a byte
		// stream, so nothing aligns them with SSE framing, and the event carrying the output
		// tally is the one that gets split in practice. The cut below lands inside
		// message_delta's usage object, so neither half parses on its own: before the
		// reassembly fix this settled at the prompt-only floor and looked like an ordinary
		// priced response.
		//
		// The expectation is the SAME record as the un-split fixture, which is the whole claim:
		// how a body was chunked must not change what a turn cost.
		fixture: func() fixture {
			f := base("streamed and cut inside the output tally")
			f.reqBody = []byte(streamRequest)
			f.upstreamStatus = 200
			f.upstreamContentType = "text/event-stream"
			f.upstreamBody = []byte(anthropicStreamed)
			f.upstreamHeaders = map[string]string{costing.ResponseCostHeader: "0"}
			f.splitResponseBodyAt = splitInsideOutputTally
			return f
		}(),
		want: costevent.Event{
			CostUSD:    modelledWholeUSD,
			Source:     costevent.SourceUsageFallback,
			Provenance: pricing.ProvConfigured.String(),
			Settled:    true,
			PromptUSD:  (1000*7 + 200*3) / 1e6,
			OutputUSD:  500 * 23 / 1e6,
		},
	}}
}

// A CLAIM THIS TABLE CANNOT MAKE, AND WHY, because dropping it silently would be worse than
// stating it: "an unparsed path with NO cost header publishes nothing" belongs here by rights, and
// ext_proc cannot express it. Its recorder appends a response row only when something
// plugin-shaped happened — an extension, an invocation, a plugin event — while both proxies append
// a row unconditionally, carrying the status. So on that fixture ext_proc produces no session event
// at all, which is a divergence in TELEMETRY rather than in cost, and asserting it here would make
// this suite fail for a reason that has nothing to do with money.
//
// The claim itself is pinned where it can be: TestUnparsedEndpoint_NoCostHeaderPublishesNothing in
// plugins/inferenceparser covers it across four paths and every dispatch site. The divergence is
// worth its own fixture in the spy suite next door, which is about exactly that question.
func TestCostRecordParity(t *testing.T) {
	for _, direction := range []pipeline.Direction{pipeline.Outbound, pipeline.Inbound} {
		listeners := outboundListeners
		if direction == pipeline.Inbound {
			listeners = inboundListeners
		}
		for _, cf := range costFixtures(t, direction) {
			t.Run(fmt.Sprintf("%s/%s", direction, cf.name), func(t *testing.T) {
				// Collected in this scope for the reason parity_test.go's loop explains: a
				// comparison assembled inside a subtest closure compares nothing the moment the
				// subtests run in parallel, and passes.
				records := map[string]*costevent.Event{}
				for _, l := range listeners {
					obs := l.run(t, cf.fixture, pipeline.SessionResponse)
					if obs == nil {
						t.Fatalf("%s: no response event recorded", l.name)
					}
					rec := costRecord(t, l.name, obs)
					if rec == nil {
						t.Fatalf("%s published NO cost record: this turn's spend reaches no aggregate, no ledger and no budget", l.name)
					}
					// ABSOLUTE FIRST. Two listeners agreeing on a wrong figure is what a shared
					// bug in costing looks like, and a pairwise check cannot see it.
					assertRecord(t, l.name, *rec, cf.want)
					records[l.name] = rec
				}
				// THEN PAIRWISE, over the whole record rather than the fields named above, so a
				// field nobody thought to assert cannot drift between listeners unnoticed.
				var first string
				for name := range records {
					if first == "" || name < first {
						first = name
					}
				}
				for name, rec := range records {
					if name == first {
						continue
					}
					if a, b := mustJSON(t, *records[first]), mustJSON(t, *rec); a != b {
						t.Errorf("cost records differ between listeners\n  %s: %s\n  %s: %s",
							first, a, name, b)
					}
				}
			})
		}
	}
}

// assertRecord compares the fields that carry money or qualify it, naming each one, because a
// reflect.DeepEqual failure over the whole struct says "these differ" and not which claim broke.
func assertRecord(t *testing.T, listener string, got, want costevent.Event) {
	t.Helper()
	if got.CostUSD != want.CostUSD {
		t.Errorf("%s: CostUSD = %v, want %v", listener, got.CostUSD, want.CostUSD)
	}
	if got.Settled != want.Settled {
		t.Errorf("%s: Settled = %v, want %v (Priced() = %v)", listener, got.Settled, want.Settled, got.Priced())
	}
	if got.Source != want.Source {
		t.Errorf("%s: Source = %q, want %q — the precedence rule is the reason costing exists",
			listener, got.Source, want.Source)
	}
	if got.Provenance != want.Provenance {
		t.Errorf("%s: Provenance = %q, want %q", listener, got.Provenance, want.Provenance)
	}
	if got.RejectedReason != want.RejectedReason {
		t.Errorf("%s: RejectedReason = %q, want %q", listener, got.RejectedReason, want.RejectedReason)
	}
	// INCOMPLETE IS NEVER EXPECTED HERE, on any fixture: every turn in this table reports its
	// counters in full. A floor appearing on one listener and not another is precisely the
	// premature-finalization bug this suite exists to catch, so it is asserted rather than
	// tolerated.
	if got.Incomplete {
		t.Errorf("%s: Incomplete = true (%q) on a turn that reported every count: the listener finalized before the tally arrived",
			listener, got.IncompleteReason)
	}
	if got.PromptUSD != want.PromptUSD {
		t.Errorf("%s: PromptUSD = %v, want %v", listener, got.PromptUSD, want.PromptUSD)
	}
	if got.OutputUSD != want.OutputUSD {
		t.Errorf("%s: OutputUSD = %v, want %v", listener, got.OutputUSD, want.OutputUSD)
	}
}

// costRecord decodes the cost record off an observation, or nil when the turn published none.
func costRecord(t *testing.T, listener string, obs *observation) *costevent.Event {
	t.Helper()
	// THE WIRE KEY, NOT THE CONTEXT KEY. A plugin publishes under "<key>.event" on the pctx and
	// SnapshotPlugins strips that suffix when it writes the session event, so looking for the
	// context key here finds nothing and reads as "no record published" — which is a false
	// FAILURE, but the same mistake in a consumer is a false zero. costevent.Record accepts
	// either the concern name or the producer's, so both are tried.
	raw, ok := obs.PluginEventJSON[costevent.Key]
	if !ok {
		// costevent.PluginName is deprecated in favour of the concern-named key, and reading it
		// here is the point: Record accepts EITHER, so a consumer written against the old key must
		// keep working. Nothing else in this repo should reach for it.
		if raw, ok = obs.PluginEventJSON[costevent.PluginName]; !ok { //nolint:staticcheck // deliberate: the compatibility key Record still accepts
			return nil
		}
	}
	var ev costevent.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("%s: decoding the cost record: %v (raw %s)", listener, err, raw)
	}
	return &ev
}

func mustJSON(t *testing.T, ev costevent.Event) string {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

package costing

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/costevent"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
)

// THE TRUST BOUNDARY. This header is an unauthenticated string on a response and nothing in
// the pipeline considers the HOST it came from — inference-parser dispatches on the path
// alone. Since cost is settled on every proxied response, including the ones with no
// inference extension, any path on any host an agent is proxied to can name its own figure.
//
// Verified consequence of one accepted forgery: the figure is published, aggregated,
// written to the thirty-day ledger, and fed to litellm_budgettrack, which denies with HTTP
// 429 once the daily total passes MaxBudget. One hostile response could lock an agent out
// of all further inference and poison durable cost reporting, and inference-parser is in the
// default local pipeline.
//
// The cap bounds what an UNPARSED endpoint may contribute. Three properties are pinned
// here, and the third matters as much as the first two: a forged figure contributes nothing
// and is disclosed; a PARSED endpoint is untouched, because that narrower hole predates this
// and needs a host allowlist rather than a cap; and a PLAUSIBLE figure from an unparsed
// endpoint still settles, because gateway-priced /v1/embeddings spend is the feature that
// opened the hole and breaking it is not a fix.

// unparsedCostCtx builds a response context for a request the parser never parsed:
// Extensions.Inference is nil, exactly as OnRequest leaves it for an endpoint off the
// dialect list.
func unparsedCostCtx(costHeader string) *pipeline.Context {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(ResponseCostHeader, costHeader)
	return &pipeline.Context{
		Host:            "evil.example",
		Path:            "/v1/embeddings",
		ResponseHeaders: h,
		// NO Inference extension. This is the state the cap keys on.
	}
}

// capUSD is the plausibility cap in dollars: $10,000.
func capUSD() float64 { return float64(pricing.MaxPlausibleRequestCostMicros) / 1e6 }

// TestSettle_UnparsedEndpointRefusesAnImplausibleCost is the fix proper.
//
// Every field a consumer could read money out of is asserted, because "not charged" has to
// hold on all of them at once: CostUSD is the figure, Priced is the admission predicate the
// ledger and the aggregator both ask, and HasReported is what the drift check compares
// against — leaving the figure in ReportedUSD would have the rate table measured against a
// forgery and reported as stale.
func TestSettle_UnparsedEndpointRefusesAnImplausibleCost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"a figure that exhausts any daily budget", "1000000000"},
		{"a micro over the cap", "10000.000001"},
		{"ten times the cap", "100000"},
		{"past the micros unit entirely", "1e13"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Settle(unparsedCostCtx(tc.header), rates(t))

			if got.Priced {
				t.Errorf("Priced = true for a header of %s on an endpoint nothing parsed; this figure reaches the ledger, the daily total and the 429", tc.header)
			}
			if got.CostUSD != 0 {
				t.Errorf("CostUSD = %v, want 0 — a refused figure is not clamped to the cap, because a wrong number wearing a right label is worse than a named gap", got.CostUSD)
			}
			if got.HasReported || got.ReportedUSD != 0 {
				t.Errorf("HasReported = %v / ReportedUSD = %v; a refused figure must not reach the drift check, which would report the rate table stale against a forgery", got.HasReported, got.ReportedUSD)
			}
			if got.DeclaredFree {
				t.Error("DeclaredFree = true; refusing a figure is not the gateway declaring the call free")
			}
			// DISCLOSED, which is the half that keeps the coverage gap honest. Silence
			// here would make this response identical to one that reported no cost.
			if got.RejectedReason != costevent.RejectedImplausible {
				t.Errorf("RejectedReason = %q, want %q; without it the refusal reaches no record and nobody can tell a forged figure from an absent one", got.RejectedReason, costevent.RejectedImplausible)
			}
			// And the record a consumer actually reads.
			rec := NewRecord(got, nil)
			if rec.Priced() {
				t.Error("the published record reads as priced; costevent.Priced is the guard the ledger writer and the aggregator both use")
			}
			if rec.Micros() != 0 {
				t.Errorf("record Micros() = %d, want 0", rec.Micros())
			}
			if rec.RejectedReason != costevent.RejectedImplausible {
				t.Errorf("record RejectedReason = %q, want %q", rec.RejectedReason, costevent.RejectedImplausible)
			}
		})
	}
}

// TestSettle_UnparsedEndpointStillSettlesAPlausibleCost is the feature the hole was opened
// by, and it must survive the fix.
//
// LiteLLM prices /v1/embeddings and stamps the same cost header on it as on a completion.
// That spend has no inference extension behind it — the parser has no dialect for the
// endpoint — so it is settled from the header alone, and a cap that refused it would trade
// one silent money bug for another.
//
// The cap boundary is walked from both sides in the same test, because the interesting
// question is not whether $0.0042 works but where the line falls: exactly at the cap
// settles (the derivation reads "the most a request could plausibly cost", and that figure
// is still plausible), a micro past it does not.
func TestSettle_UnparsedEndpointStillSettlesAPlausibleCost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   float64
	}{
		{"an embeddings call", "0.0042", 0.0042},
		{"an expensive but real call", "20", 20},
		{"exactly at the cap", "10000", 10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Settle(unparsedCostCtx(tc.header), rates(t))

			if !got.Priced {
				t.Fatalf("Priced = false for a plausible header of %s; gateway-priced spend on an endpoint the parser cannot read reaches neither /v1/usage nor the ledger nor the budget", tc.header)
			}
			if got.CostUSD != tc.want {
				t.Errorf("CostUSD = %v, want %v", got.CostUSD, tc.want)
			}
			if got.Source != costevent.SourceGatewayHeader {
				t.Errorf("Source = %q, want %q", got.Source, costevent.SourceGatewayHeader)
			}
			if got.RejectedReason != "" {
				t.Errorf("RejectedReason = %q for a figure that settled; a record cannot both carry a figure and disclose that it refused one", got.RejectedReason)
			}
			if !got.HasReported {
				t.Error("HasReported = false; the drift check has nothing to compare and a stale rate table becomes undetectable on this path")
			}
		})
	}
}

// TestSettle_ParsedEndpointIsUnaffectedByTheCap keeps the fix inside its stated scope.
//
// Where the extension is present the request WAS parsed: an inference-shaped body went to an
// inference-shaped path and a model came back out of it. A hostile host serving
// /v1/chat/completions could always forge a figure there, that hole predates this PR, and
// the cap is deliberately not applied to it — a cap is not authentication, and the fix for
// that one is an allowlist of hosts whose cost headers are believed at all.
//
// Pinned as a test rather than left as a comment for two reasons: so the boundary is
// checked instead of asserted, and so the residual exposure is visible in the suite rather
// than only in a disclosure document. If a later change DOES cap the parsed path, this test
// fails and the scope decision gets made deliberately.
func TestSettle_ParsedEndpointIsUnaffectedByTheCap(t *testing.T) {
	// A billion dollars: far past the plausibility cap, still inside the micros unit, so
	// nothing but the cap could refuse it.
	const forged = 1e9
	if pricing.PlausibleRequestCostUSD(forged) {
		t.Fatal("the fixture is under the plausibility cap; this test would then prove nothing about the parsed path")
	}
	pctx := ctx(map[string]string{ResponseCostHeader: "1000000000", "Content-Type": "application/json"}, 1000, 500)

	got := Settle(pctx, rates(t))

	if !got.Priced || got.CostUSD != forged {
		t.Errorf("Priced = %v / CostUSD = %v, want true / %v — a parsed endpoint keeps today's behaviour; changing it here is a scope decision, not a bug fix",
			got.Priced, got.CostUSD, forged)
	}
	if got.RejectedReason != "" {
		t.Errorf("RejectedReason = %q on a parsed endpoint; the cap must key on the nil extension, which is exactly the traffic whose spend this PR newly admitted", got.RejectedReason)
	}
	if !got.HasReported {
		t.Error("HasReported = false on a parsed endpoint; the drift check is the one thing that would notice this figure disagreeing with the table")
	}
}

// TestHeaderCost_ImplausibleIsItsOwnState covers the state machine the fix extends, all
// five arms in one table.
//
// A separate state rather than reusing headerUnusable, because the two say different things
// and the difference is the disclosure: an unparseable header is noise, while a refused one
// is a figure this process declined and an operator needs to know about. Collapsing them
// would publish nothing and make a forged $9,000,000,000 identical to a garbage string.
//
// The last two rows are the fix in two lines: the same header value, the same path, opposite
// states, separated only by whether an extension was parsed.
func TestHeaderCost_ImplausibleIsItsOwnState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		header    string
		parsed    bool
		wantState headerCostState
		wantCost  float64
	}{
		{"no header", "", false, headerAbsent, 0},
		{"garbage", "not-a-number", false, headerUnusable, 0},
		{"negative", "-1", false, headerUnusable, 0},
		{"a zero", "0", false, headerZero, 0},
		{"a real figure", "0.0042", false, headerPositive, 0.0042},
		{"exactly at the cap", "10000", false, headerPositive, capUSD()},
		{"past the cap, unparsed", "1000000000", false, headerImplausible, 0},
		{"past the cap, parsed", "1000000000", true, headerPositive, 1e9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pctx := unparsedCostCtx(tc.header)
			if tc.header == "" {
				pctx.ResponseHeaders.Del(ResponseCostHeader)
			}
			if tc.parsed {
				pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-opus-5"}
			}
			cost, state := headerCost(pctx)
			if state != tc.wantState {
				t.Errorf("state = %d, want %d for header %q (parsed=%v)", state, tc.wantState, tc.header, tc.parsed)
			}
			if cost != tc.wantCost {
				t.Errorf("cost = %v, want %v", cost, tc.wantCost)
			}
		})
	}
}

// TestSettle_RefusedHeaderIsNotAnAnswer pins that the refusal declines a figure without
// asserting one.
//
// A refused header must behave like an unusable one and NOT like a declared-free zero: no
// source, no provenance, nothing settled. The distinction is load-bearing elsewhere in this
// package — a declared-free zero is published as PRICED so no consumer re-prices it — and a
// refusal that borrowed that shape would publish a settled $0 for hostile traffic and count
// it as covered.
func TestSettle_RefusedHeaderIsNotAnAnswer(t *testing.T) {
	got := Settle(unparsedCostCtx("1000000000"), rates(t))

	if got.Source != "" {
		t.Errorf("Source = %q, want empty; nothing priced this request", got.Source)
	}
	if got.Provenance != pricing.ProvNone {
		t.Errorf("Provenance = %v, want ProvNone", got.Provenance)
	}
	if got.HasModelled || got.HasPrompt || got.HasOutput {
		t.Errorf("a modelled figure appeared from a request with no extension: modelled=%v prompt=%v output=%v",
			got.HasModelled, got.HasPrompt, got.HasOutput)
	}
	if got.Incomplete {
		t.Error("Incomplete = true; there is no figure here to qualify as inexact")
	}
}

// TestWarnImplausibleCost_NamesTheHost pins the operator's only lead.
//
// A warning saying "implausible cost rejected" leaves the one question that matters
// unanswerable. Host, path, the figure and the bound all have to be on it, or an operator
// cannot find the site that sent it.
//
// The Once is reset first because it is process-scoped and another test in this package may
// have consumed it. Legitimate here and nowhere else: the test is in-package, and the
// alternative — injecting a logger through headerCost — would widen a production signature
// for a test's convenience.
func TestWarnImplausibleCost_NamesTheHost(t *testing.T) {
	capture := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
	implausibleWarnOnce = sync.Once{}
	t.Cleanup(func() { implausibleWarnOnce = sync.Once{} })

	Settle(unparsedCostCtx("1000000000"), rates(t))

	// THE WARN RECORD SPECIFICALLY, not every record. Asserting over all of them passes when
	// the detail is only on the debug line beside it — a real mutation that survived until this
	// was tightened, and one that would leave a default deployment (debug off) with a warning
	// that names no host.
	warn := capture.at(slog.LevelWarn)
	if len(warn) != 1 {
		t.Fatalf("want exactly one WARN record, got %d; a debug-only line is invisible in a default deployment", len(warn))
	}
	got := attrs(warn[0])
	for key, want := range map[string]any{
		"host":              "evil.example",
		"path":              "/v1/embeddings",
		"reported_usd":      1e9,
		"max_plausible_usd": 10000.0,
	} {
		if got[key] != want {
			t.Errorf("WARN attr %q = %#v, want %#v; an operator cannot find the host that sent this. all attrs: %#v",
				key, got[key], want, got)
		}
	}

	// ONCE at warn level, so a hostile host cannot use this path as a log-flood
	// amplifier. Every occurrence stays on the record as RejectedImplausible and at debug
	// level for whoever is investigating.
	capture.reset()
	Settle(unparsedCostCtx("2000000000"), rates(t))
	if n := len(capture.at(slog.LevelWarn)); n != 0 {
		t.Errorf("a second refusal warned again (%d WARN records); this fires on a path an attacker chooses, so it must be once per process", n)
	}
	debug := capture.at(slog.LevelDebug)
	if len(debug) != 1 {
		t.Fatalf("DEBUG records for the second refusal = %d, want 1: every occurrence must be recoverable by an operator who turns debug on", len(debug))
	}
	if d := attrs(debug[0]); d["reported_usd"] != 2e9 || d["host"] != "evil.example" {
		t.Errorf("the second refusal's debug trail = %#v, want cost 2e9 on evil.example", d)
	}
}

// captureHandler collects slog records as VALUES, which is what the assertions in this file
// actually want to ask about.
//
// The earlier version of these tests matched substrings of a text-handler dump — "level=WARN",
// "1e+09" — so they were asserting slog's OUTPUT FORMAT alongside the behaviour: a switch to a
// JSON handler, or a change in how a float is rendered, breaks them while nothing about the
// signal has changed. Worse for the float, which is the load-bearing detail here: "1e+09" is
// Go's default float formatting, not a fact about the warning.
//
// Records are appended under a mutex because a slog.Handler may be called from any goroutine;
// nothing in these tests does, and depending on that would be the kind of assumption that turns
// into a flake once something else in the package logs asynchronously.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// at returns the records at one level, most recent last.
func (h *captureHandler) at(level slog.Level) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if r.Level == level {
			out = append(out, r)
		}
	}
	return out
}

func (h *captureHandler) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = nil
}

// attrs flattens a record's attributes into a map, so an assertion can name the KEY it depends
// on — "host" — instead of hoping a value appears somewhere in a rendered line.
//
// Numbers are normalised to float64, deliberately. slog keeps the Go type it was handed, and
// these attrs are compared through an `any`, so an expectation written as 10000 fails against a
// value that IS 10000 the moment a call site passes an int instead of a float — a test failing on
// a literal's type rather than on the signal. Every number here is a dollar figure.
func attrs(r slog.Record) map[string]any {
	out := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		switch a.Value.Kind() {
		case slog.KindInt64:
			out[a.Key] = float64(a.Value.Int64())
		case slog.KindUint64:
			out[a.Key] = float64(a.Value.Uint64())
		default:
			out[a.Key] = a.Value.Any()
		}
		return true
	})
	return out
}

package tui

import (
	"math"
	"testing"

	"github.com/rossoctl/cortex/core/cost/event"
)

// The events table shows every saving the proxy attributed to a request — every operator the
// accountant priced, not a list agentop keeps — summed into one figure.
func TestRowSaving_AnyComponent(t *testing.T) {
	e := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "tool-prune", Basis: event.BasisUSD, TokensAvoided: 1_000, USD: 0.01, Estimated: true},
		{Component: "context-guru.mask", Basis: event.BasisUSD, TokensAvoided: 4_000, USD: 0.03},
		{Component: "keeper", Basis: event.BasisUSD, TokensAvoided: 20_000, USD: 0.05},
		{Component: "a-future-operator", Basis: event.BasisUSD, TokensAvoided: 1, USD: 0.001},
	}})
	s, ok := rowSavingFor(e)
	if !ok {
		t.Fatal("no saving for a record carrying four")
	}
	if s.TokensAvoided != 25_001 || math.Abs(s.USD-0.091) > 1e-12 || s.Projected || !s.Estimated || s.Basis != event.BasisUSD {
		t.Errorf("saving = %+v, want 25001 tokens, $0.091, applied, estimated, usd", s)
	}
	if _, ok := rowSavingFor(nil); ok {
		t.Error("a nil event reported a saving")
	}
	if _, ok := rowSavingFor(recordEvent(t, event.Event{CostUSD: 1, Settled: true})); ok {
		t.Error("a record with no saving reported one")
	}
}

// The dollar figure is net of the penalty each operator caused, and NOT floored: a compaction
// that rewrote more of the cached prefix than it saved shows a negative net.
func TestRowSaving_NetCanBeNegative(t *testing.T) {
	e := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "context-guru.mask", Basis: event.BasisUSD, TokensAvoided: 4_000, USD: 0.03, PenaltyUSD: 0.5},
		{Component: "tool-prune", Basis: event.BasisUSD, TokensAvoided: 100, USD: 0.002},
	}})
	s, ok := rowSavingFor(e)
	if !ok || math.Abs(s.USD-(-0.468)) > 1e-12 || s.TokensAvoided != 4_100 {
		t.Errorf("saving = %+v ok=%v, want USD -0.468 (0.03-0.5+0.002) and the tokens kept", s, ok)
	}
	if got := formatUSDWithSaving(1.0, s.USD, false); got != "$1.0000(+$0.4680)" {
		t.Errorf("cell = %q, want the net cost shown as an addition", got)
	}
}

// Only a usd saving is money: a subscription saving's dollars are what the API would have
// charged, so they never enter the row's dollar figure, while its tokens are shown.
func TestRowSaving_OnlyUSDBasisIsMoney(t *testing.T) {
	e := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "tool-prune", Basis: event.BasisSubscriptionEquivalent, TokensAvoided: 1_000, USD: 0.5, Units: 100},
		{Component: "context-guru.mask", Basis: event.BasisUnknown, TokensAvoided: 10, USD: 0.2},
	}})
	s, ok := rowSavingFor(e)
	if !ok || s.USD != 0 || s.TokensAvoided != 1_010 || s.Basis == event.BasisUSD {
		t.Errorf("saving = %+v ok=%v, want the tokens and no dollars", s, ok)
	}
}

// Applied and projected savings are different claims — money not spent against money that was
// spent — so they are never added together.
func TestRowSaving_NeverMixesAppliedAndProjected(t *testing.T) {
	mixed := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "tool-prune", Basis: event.BasisUSD, TokensAvoided: 1_000, USD: 0.01},
		{Component: "context-guru.mask", Basis: event.BasisUSD, TokensAvoided: 4_000, USD: 0.03, Projected: true},
	}})
	if s, _ := rowSavingFor(mixed); s.TokensAvoided != 1_000 || s.Projected {
		t.Errorf("mixed = %+v, want only the applied tool-prune saving", s)
	}
	projected := recordEvent(t, event.Event{Avoided: []event.Saving{
		{Component: "context-guru.mask", Basis: event.BasisUSD, TokensAvoided: 4_000, USD: 0.03, Projected: true},
	}})
	if s, _ := rowSavingFor(projected); s.TokensAvoided != 4_000 || !s.Projected {
		t.Errorf("projected = %+v, want the projected saving, marked", s)
	}
}

// A saving from a proxy that predates the accountant has no basis, and is dollars: the row shows
// it as the ledger counts it (event.Saving.EffectiveBasis).
func TestRowSaving_ALegacySavingWithNoBasisIsMoney(t *testing.T) {
	e := recordEvent(t, event.Event{Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 500, USD: 0.004}}})
	if s, ok := rowSavingFor(e); !ok || s.USD != 0.004 || s.Basis != event.BasisUSD {
		t.Errorf("saving = %+v ok=%v, want $0.004 as usd", s, ok)
	}
}

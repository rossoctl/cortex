package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pricing"
)

// End to end through the real handler: a real Registry, the real JSON, the real
// renderer. A canned fixture would let the wire shape drift from the producer, which is
// the failure this whole area keeps having.
func realStatServer(t *testing.T) string {
	t.Helper()
	tab, err := pricing.Build(nil) // bundled rates + the shipped gateway discount
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(pricing.NewRegistry(tab).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunPricing_HostViewShowsTheDiscountApplied(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPricing([]string{
		"--stats-url", realStatServer(t),
		"--host", "ete-litellm.ai-models.vpc-int.res.ibm.com",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb.String())
	}
	got := out.String()
	t.Logf("\n%s", got)

	// The measured gateway rates for opus-5: 3.8 in, 19 out.
	for _, want := range []string{"claude-opus-5", "3.8", "19", "multiplier 0.76", "ete-litellm"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// It must say the figures are scaled, or a reader compares them against a
	// published price list and concludes the tool is broken.
	if !strings.Contains(got, "vendor list scaled") {
		t.Errorf("output does not explain that rates are scaled: %s", got)
	}
	// The bundled table gives sonnet-4-5 a 200k long-context tier, and the host view
	// resolves at prompt size 0 — so its row MUST carry the marker and the footnote
	// must give the breakpoint. Quoting a below-threshold rate as if it were the only
	// rate is the exact failure this package exists to remove.
	if !strings.Contains(got, "long-context rates apply above 200,000 prompt tokens") {
		t.Errorf("no long-context note with a breakpoint: %s", got)
	}
	// The above-threshold rates must be present AND scaled: sonnet-4-5's 200k input
	// premium is 6.00 at list, so at 0.76 it is 4.56. Quoting 6.00 here would be the
	// unscaled figure; quoting nothing would leave the operator doing the arithmetic
	// this command exists to do.
	af := overrideAfter(t, got, "claude-sonnet-4-5")
	if len(af) != 7 {
		t.Fatalf("override line has %d columns, want 7: %q", len(af), af)
	}
	if af[3] != "4.56" {
		t.Errorf("above-threshold input = %q, want the scaled 4.56 (6.00 means unscaled)", af[3])
	}
	// A model without a tier gets no continuation line.
	if _, ok := overrideLineAfter(got, "claude-opus-5"); ok {
		t.Error("opus-5 has a long-context line, but it has no long-context tier")
	}
}

func TestRunPricing_UnscaledEndpointSaysSo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t), "--host", "api.anthropic.com"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "no gateway discount") {
		t.Errorf("expected the no-discount note: %s", got)
	}
	// UNSCALED vendor list for opus-5: 5 in, 25 out. Asserted by COLUMN, not by
	// Contains: "5" is a substring of the model name "claude-opus-5" and of "25", so a
	// containment check on either the output or the row proves nothing at all.
	//
	// Columns are: model, input, cache-write, cache-read, output, provenance.
	f := strings.Fields(modelRow(t, got, "claude-opus-5"))
	if len(f) < 6 {
		t.Fatalf("opus-5 row has %d columns, want 6: %q", len(f), f)
	}
	if f[1] != "5" {
		t.Errorf("input column = %q, want the list rate 5", f[1])
	}
	if f[4] != "25" {
		t.Errorf("output column = %q, want the list rate 25", f[4])
	}
	// No separate "and not 3.8/19" check: the two equalities above already exclude every
	// other value, discounted ones included.
}

// modelRow returns one model's line, so a rate assertion is scoped to that model rather
// than matching any digit anywhere in the table.
func modelRow(t *testing.T, out, model string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		// Exact first field, not Contains: "claude-opus-5" is a prefix of
		// "claude-opus-5-1", and a substring match would silently accept the wrong row.
		if f := strings.Fields(line); len(f) > 0 && f[0] == model {
			return line
		}
	}
	t.Fatalf("no %s row in:\n%s", model, out)
	return ""
}

func TestRunPricing_TableViewListsRowsAndDiscounts(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t)}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{"Pricing table", "generated from litellm", "Gateway discounts", "res.ibm.com", "UNSCALED"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

// A tier with no rate must render "-", never 0.00: pricing.Cost refuses to price a
// request that used such a tier, so a zero would misrepresent a coverage gap as free.
func TestRunPricing_UnsetTierRendersAsAbsent(t *testing.T) {
	if got := rate(0); got != "-" {
		t.Errorf("rate(0) = %q, want %q", got, "-")
	}
	if got := rate(3.8); got != "3.8" {
		t.Errorf("rate(3.8) = %q", got)
	}
}

func TestRunPricing_ProxyDownIsActionable(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPricing([]string{"--stats-url", "http://127.0.0.1:1"}, &out, &errb)
	if code == 0 {
		t.Fatal("expected a non-zero exit when the proxy is unreachable")
	}
	if !strings.Contains(errb.String(), "abctl service status") {
		t.Errorf("error does not tell the operator what to check: %s", errb.String())
	}
}

// The raw view must not present a two-tier row as a one-tier answer. Four bundled models
// carry a 200k override; the wire has always had them and the renderer used to drop them.
func TestRunPricing_TableViewShowsLongContextOverrides(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPricing([]string{"--stats-url", realStatServer(t)}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	// By column, not by Contains: a Contains on "        6 " would depend on the %9s
	// padding and would fail on a width change while the rate itself was correct.
	//
	// Override columns: "above", <breakpoint>, "tok:", then the four rates.
	f := overrideAfter(t, out.String(), "claude-sonnet-4-5")
	if len(f) != 7 {
		t.Fatalf("override line has %d columns, want 7: %q", len(f), f)
	}
	if f[1] != "200,000" {
		t.Errorf("breakpoint = %q, want 200,000", f[1])
	}
	// sonnet-4-5's 200k premium at list: 6.00 input, 22.50 output. The raw view is
	// unscaled, so these are list and not the discounted figures.
	if f[3] != "6" {
		t.Errorf("override input = %q, want the premium 6", f[3])
	}
	if f[6] != "22.5" {
		t.Errorf("override output = %q, want the premium 22.5", f[6])
	}
}

// overrideAfter returns the long-context continuation line that follows a model's row in
// the raw view, as columns.
//
// Positional on purpose: the continuation line only means anything attached to the row
// above it, so finding it by scanning for "above" anywhere would not prove it landed on
// the right model.
func overrideAfter(t *testing.T, out, model string) []string {
	t.Helper()
	f, ok := overrideLineAfter(out, model)
	if !ok {
		t.Fatalf("no long-context line after the %s row in:\n%s", model, tail(out))
	}
	return f
}

// overrideLineAfter returns the long-context continuation line following a model's row.
//
// Handles both views: the raw table leads with the endpoint (model is the second column),
// the host view leads with the model. Matching either position keeps one helper for both
// rather than two that can disagree about what "the line after" means.
func overrideLineAfter(out, model string) ([]string, bool) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		onRow := (len(f) > 0 && f[0] == model) || (len(f) > 1 && f[1] == model)
		if !onRow || i+1 >= len(lines) {
			continue
		}
		if nf := strings.Fields(lines[i+1]); len(nf) > 0 && nf[0] == "above" {
			return nf, true
		}
		return nil, false
	}
	return nil, false
}

func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return strings.Join(lines, "\n")
}

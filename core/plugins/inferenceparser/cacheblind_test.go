package inferenceparser

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/pipeline"
)

// blindCtx is a non-streamed response whose header prices the uncached tiers only.
//
// bodylessRates charges 1 micro-dollar in every tier, so the arithmetic is the token counts:
// 100 input + 100 output is $0.0002 uncached, and 10,000 cache reads is $0.01 — 98% of the
// request, which is the shape of real agent traffic and what makes the omission material.
func blindCtx(header string, cacheRead int) *pipeline.Context {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if header != "" {
		h.Set(settle.ResponseCostHeader, header)
	}
	return &pipeline.Context{
		Host:            "gw.internal",
		ResponseHeaders: h,
		Extensions: pipeline.Extensions{Inference: &pipeline.InferenceExtension{
			Model:           "claude-opus-5",
			InputTokens:     100,
			OutputTokens:    100,
			CacheReadTokens: cacheRead,
		}},
	}
}

func blindParser(t *testing.T, buf *bytes.Buffer) *InferenceParser {
	t.Helper()
	p := NewInferenceParser()
	p.SetPricingResolver(bodylessRates(t))
	p.setCacheBlindLogger(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return p
}

// THE SIGNAL THAT WAS MISSING. The header figure was on the wire, the substitution now happens
// in settle.Settle, and without a line here an operator sees neither: the money is right and
// the reason is invisible.
func TestCacheBlind_WarnsOnceWithBothFigures(t *testing.T) {
	var buf bytes.Buffer
	p := blindParser(t, &buf)

	p.settleCost(blindCtx("0.0002", 10_000))

	got := buf.String()
	if got == "" {
		t.Fatal("no warning for a header that priced only input and output")
	}
	for _, want := range []string{
		"gw.internal", "claude-opus-5",
		"header_usd=0.000200",  // what the gateway said
		"charged_usd=0.010200", // what the table charged instead
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning omits %q:\n%s", want, got)
		}
	}
	// NOT the neighbouring drift warning's advice. "set pricing.endpoints[].multiplier" is
	// exactly wrong here — the table is the figure that matches the gateway's own billing, and
	// an operator who changed it would break the majority of responses whose headers agree.
	if strings.Contains(got, "multiplier") {
		t.Errorf("warning advises a rate change, which would break a correct table:\n%s", got)
	}

	// Once per endpoint and model: an agent makes thousands of these.
	buf.Reset()
	for i := 0; i < 5; i++ {
		p.settleCost(blindCtx("0.0002", 10_000))
	}
	if buf.String() != "" {
		t.Errorf("repeated for the same endpoint and model:\n%s", buf.String())
	}
}

// Silence is the correct output for every other shape, and each of these reaches settleCost on
// the same path. A reporter that fired on any of them would train an operator to filter it out.
func TestCacheBlind_SilentWhenNothingWasSubstituted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		header    string
		cacheRead int
	}{
		{"a header that prices every tier", "0.010200", 10_000},
		{"a cache-free request whose header is therefore the whole figure", "0.000200", 0},
		{"no header at all — the table was always going to be charged", "", 10_000},
		{"an unexplained low header, which costing keeps rather than second-guesses", "0.005", 10_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := blindParser(t, &buf)

			p.settleCost(blindCtx(tc.header, tc.cacheRead))

			if buf.String() != "" {
				t.Errorf("warned when no substitution happened:\n%s", buf.String())
			}
		})
	}
}

// A distinct model on the same endpoint is a distinct fact — gateways have been seen pricing one
// deployment's cache tokens and not another's, which is how this was found.
func TestCacheBlind_WarnsPerModel(t *testing.T) {
	var buf bytes.Buffer
	p := blindParser(t, &buf)

	p.settleCost(blindCtx("0.0002", 10_000))
	second := blindCtx("0.0002", 10_000)
	second.Extensions.Inference.Model = "claude-sonnet-5"
	p.settleCost(second)

	if n := strings.Count(buf.String(), "omits cache cost"); n != 2 {
		t.Errorf("warned %d times for two models, want 2:\n%s", n, buf.String())
	}
}

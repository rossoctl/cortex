package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func demoFixture() Fixture {
	return Fixture{Sessions: []FixtureSession{
		{ID: "api-7f3c", Title: "fix the retry handler", Model: "claude-opus-5", Turns: []Turn{
			{Messages: 4, Tools: 15, Input: 1800, CacheRead: 46000, CacheWrite: 9000, Output: 620, PromptUSD: 0.098, OutputUSD: 0.047,
				ToolCall: "github-tool-mcp", ToolHost: "github-tool-mcp"},
			{Messages: 10, Tools: 15, Input: 900, CacheRead: 93000, CacheWrite: 2400, Output: 810, PromptUSD: 0.121, OutputUSD: 0.061},
			{Messages: 16, Tools: 15, Input: 1100, CacheRead: 141000, CacheWrite: 3100, Output: 940, PromptUSD: 0.174, OutputUSD: 0.071},
		}},
		{ID: "web-2a91", Title: "add dark mode toggle", Model: "claude-opus-5", Turns: []Turn{
			{Messages: 4, Tools: 15, Input: 1200, CacheRead: 21000, CacheWrite: 6400, Output: 380, PromptUSD: 0.061, OutputUSD: 0.029},
			{Messages: 8, Tools: 15, Input: 700, CacheRead: 44000, CacheWrite: 1800, Output: 520, PromptUSD: 0.074, OutputUSD: 0.039},
		}},
		{ID: "infra-55de", Title: "debug the helm chart", Model: "claude-sonnet-5", Turns: []Turn{
			{Messages: 4, Tools: 15, Input: 800, CacheRead: 12000, CacheWrite: 3200, Output: 240, PromptUSD: 0.012, OutputUSD: 0.004},
		}},
	}}
}

func newProbe(t *testing.T) *Capturer {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := demoFixture()
	if err := WriteTitles(home, f); err != nil {
		t.Fatal(err)
	}
	c, err := NewCapturer(f, 120, 34, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestCapture_SessionsTableShowsMoneyAndTitles(t *testing.T) {
	c := newProbe(t)
	got := ansi.Strip(c.Screen())

	for _, want := range []string{
		"SESSION", "TITLE", "TOKENS", "COST", "SAVED~", "CTX(1M)",
		"fix the retry handler", "add dark mode toggle", "debug the helm chart",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sessions table missing %q", want)
		}
	}
	if strings.Count(got, "$") < 2 {
		t.Errorf("expected COST figures in the table, got none")
	}
	if !strings.Contains(got, ":"+localSessionPort) {
		t.Errorf("header should show the real local session port, not the harness's ephemeral one:\n%s", got)
	}
	// A gauge glyph proves CTX(1M) folded: it is a dash whenever the fixture
	// forgets the tool manifest or the main-agent role.
	if !strings.Contains(got, "▏") {
		t.Errorf("CTX(1M) rendered no gauge:\n%s", got)
	}
}

// TestCapture_EveryBeatShowsWhatTheStoryboardClaims walks the same keys demo.yaml
// walks and asserts each pane still says what the animation is built to show. This
// is the drift guard with teeth: the staleness check can only say "something
// changed", while these name the thing that must not vanish.
func TestCapture_EveryBeatShowsWhatTheStoryboardClaims(t *testing.T) {
	c := newProbe(t)

	c.Press("$")
	tiers := ansi.Strip(c.Screen())
	for _, want := range []string{"WHERE IT WENT", "cache-read", "cache-write", "input", "output", "BY MODEL"} {
		if !strings.Contains(tiers, want) {
			t.Errorf("spend tiers missing %q", want)
		}
	}

	// The usage pane is excluded from the byte-level staleness check because its
	// bars slide with the wall clock, so these assertions are the only thing
	// guarding it. Keep them specific.
	c.Press("$", "u")
	usage := ansi.Strip(c.Screen())
	if strings.Contains(usage, "not available on this proxy") {
		t.Fatalf("usage charts unavailable — the capturer must attach a usage aggregator:\n%s", usage)
	}
	for _, want := range []string{"USAGE", "REQUESTS", "ERRORS", "TOKENS", "LATENCY", "COST"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage pane missing %q", want)
		}
	}
	if !strings.Contains(usage, "█") {
		t.Errorf("usage pane drew no bars:\n%s", usage)
	}

	c.Press("esc", "G", "enter")
	events := ansi.Strip(c.Screen())
	for _, want := range []string{"ACTION", "PLUGIN", "HOST", "inference-parser", "api.anthropic.com"} {
		if !strings.Contains(events, want) {
			t.Errorf("events table missing %q", want)
		}
	}
	if strings.Contains(events, "fix the retry handler") == false {
		t.Error("expected to drill into the busiest session")
	}

	c.Press("up", "enter")
	detail := ansi.Strip(c.Screen())
	if !strings.Contains(detail, `"messages"`) {
		t.Error("detail pane should open on a request carrying the conversation")
	}

	c.Press("f", "f")
	manifest := ansi.Strip(c.Screen())
	for _, want := range []string{`"tools"`, `"parameters"`} {
		if !strings.Contains(manifest, want) {
			t.Errorf("two pages down should reach the tool manifest; missing %q", want)
		}
	}
}

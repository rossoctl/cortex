package main

import (
	"strings"
	"testing"
	"time"

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
			{Messages: 2, Tools: 15, Input: 1200, CacheWrite: 6400, Output: 120, PromptUSD: 0.031, OutputUSD: 0.009},
			{Messages: 4, Tools: 15, Input: 600, CacheRead: 7600, CacheWrite: 900, Output: 140, PromptUSD: 0.012, OutputUSD: 0.011},
			{Messages: 6, Tools: 15, Input: 500, CacheRead: 8500, CacheWrite: 700, Output: 260, PromptUSD: 0.011, OutputUSD: 0.020},
			{Messages: 8, Tools: 15, Input: 500, CacheRead: 9200, CacheWrite: 800, Output: 110, PromptUSD: 0.012, OutputUSD: 0.008},
			{Messages: 10, Tools: 15, Input: 700, CacheRead: 10000, CacheWrite: 900, Output: 180, PromptUSD: 0.014, OutputUSD: 0.014},
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
	c, err := NewCapturer(f, 120, 34, t.TempDir(), 0)
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := demoFixture()
	f.Sessions = f.Sessions[1:] // the replay takes api-7f3c's place, as in demo.yaml
	f.Replay = &Replay{File: "testdata/timeline-replay.json.gz", ID: "api-7f3c", Title: "audit the repo before release"}
	if err := WriteTitles(home, f); err != nil {
		t.Fatal(err)
	}
	c, err := NewCapturer(f, 120, 34, t.TempDir(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	c.Press("T")
	early := ansi.Strip(c.Screen())
	for _, want := range []string{"timeline", "main agent", "tok/s"} {
		if !strings.Contains(early, want) {
			t.Errorf("timeline at 1m missing %q:\n%s", want, early)
		}
	}
	if strings.Contains(early, "Review docs and CI") {
		t.Errorf("the last subagent starts after 1m, so the replay leaked ahead:\n%s", early)
	}

	c.advance(12*time.Minute + 30*time.Second)
	late := ansi.Strip(c.Screen())
	if !strings.Contains(late, "Review docs and CI") {
		t.Errorf("timeline at 12m30s should show every subagent:\n%s", late)
	}

	c.Press("esc", "down", "enter", "G", "up")
	rows := ansi.Strip(c.Screen())
	for _, want := range []string{"add dark mode toggle", "Read", "Grep", "Edit", "Bash"} {
		if !strings.Contains(rows, want) {
			t.Errorf("events pane should list each tool call as a row; missing %q:\n%s", want, rows)
		}
	}

	c.Press("enter")
	call := ansi.Strip(c.Screen())
	for _, want := range []string{`"toolCalls"`, "npm test -- Settings"} {
		if !strings.Contains(call, want) {
			t.Errorf("Enter on the Bash row should open its call; missing %q:\n%s", want, call)
		}
	}
	for i := 0; i < 16; i++ {
		c.Press("down")
	}
	if out := ansi.Strip(c.Screen()); !strings.Contains(out, "toggles dark mode") {
		t.Errorf("the Bash call's output should be on screen below it:\n%s", out)
	}
}

package wire

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/accountant"
)

// want is the golden shape of one fixture: the positions of each section (by their wire byte
// size, in hash order) and the breakpoints Parse must find.
type want struct {
	Tools       []int       `json:"tools"`
	System      []int       `json:"system"`
	Messages    []int       `json:"messages"`
	Breakpoints []wantBreak `json:"breakpoints"`
}

type wantBreak struct {
	Section string `json:"section"`
	Index   int    `json:"index"`
	TTL     string `json:"ttl"`
}

// loadWant decodes a fixture's sibling .want.json by hand, because the Tools/System/Messages
// fields above do not share one array under one json tag.
func loadWant(t *testing.T, path string) want {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var w struct {
		Tools       []int       `json:"tools"`
		System      []int       `json:"system"`
		Messages    []int       `json:"messages"`
		Breakpoints []wantBreak `json:"breakpoints"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return want{Tools: w.Tools, System: w.System, Messages: w.Messages, Breakpoints: w.Breakpoints}
}

func TestParse_Golden(t *testing.T) {
	cases := []struct {
		file string
		fmt  Format
	}{
		{"anthropic_basic", FormatAnthropic},
		{"anthropic_cache_control", FormatAnthropic},
		{"anthropic_system_array", FormatAnthropic},
		{"chat_basic", FormatChat},
		{"chat_system_msg", FormatChat},
		{"responses_basic", FormatResponses},
		// system is preferred over instructions when a body states both.
		{"responses_system_over_instructions", FormatResponses},
		// A body shaped like Responses (input, no messages) read with every OTHER format too:
		// Parse goes by the body's own shape, not by f, exactly like the proxy's accScan.
		{"responses_shape_ignores_path", FormatResponses},
		{"responses_shape_ignores_path", FormatAnthropic},
		{"responses_shape_ignores_path", FormatChat},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/fmt=%d", c.file, c.fmt), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", c.file+".json"))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			w := loadWant(t, filepath.Join("testdata", c.file+".want.json"))

			body, bps, err := Parse(c.fmt, raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			gotTools := sectionSizes(body, accountant.SectionTools)
			gotSystem := sectionSizes(body, accountant.SectionSystem)
			gotMessages := sectionSizes(body, accountant.SectionMessages)
			assertSizes(t, "tools", gotTools, w.Tools)
			assertSizes(t, "system", gotSystem, w.System)
			assertSizes(t, "messages", gotMessages, w.Messages)

			if len(bps) != len(w.Breakpoints) {
				t.Fatalf("got %d breakpoints, want %d: %+v", len(bps), len(w.Breakpoints), bps)
			}
			for i, wb := range w.Breakpoints {
				got := bps[i]
				if string(got.Pos.Section) != wb.Section || got.Pos.Index != wb.Index || got.TTL != wb.TTL {
					t.Errorf("breakpoint %d: got %+v, want section=%s index=%d ttl=%s", i, got, wb.Section, wb.Index, wb.TTL)
				}
			}
		})
	}
}

// sectionSizes reads the wire byte size of every position of one section, in position order.
func sectionSizes(b accountant.Body, s accountant.Section) []int {
	start, end := sectionBounds(b, s)
	var out []int
	for i := start; i < end; i++ {
		out = append(out, int(b.Pos[i].Bytes))
	}
	return out
}

func sectionBounds(b accountant.Body, s accountant.Section) (int, int) {
	idx := map[accountant.Section]int{accountant.SectionTools: 0, accountant.SectionSystem: 1, accountant.SectionMessages: 2}[s]
	start := b.Start[idx]
	end := len(b.Pos)
	if idx < 2 {
		end = b.Start[idx+1]
	}
	return start, end
}

func assertSizes(t *testing.T, name string, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d positions %v, want %d %v", name, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d]: got %d bytes, want %d", name, i, got[i], want[i])
		}
	}
}

// TestParse_CacheControlStrippedFromHash pins W6.3: the hash leaves out cache_control, so two
// bodies that differ only by a cache_control mark on an otherwise identical block hash equal at
// that position.
func TestParse_CacheControlStrippedFromHash(t *testing.T) {
	plain, err := os.ReadFile(filepath.Join("testdata", "anthropic_basic.json"))
	if err != nil {
		t.Fatal(err)
	}
	marked, err := os.ReadFile(filepath.Join("testdata", "anthropic_cache_control.json"))
	if err != nil {
		t.Fatal(err)
	}
	bodyA, _, err := Parse(FormatAnthropic, plain)
	if err != nil {
		t.Fatal(err)
	}
	bodyB, _, err := Parse(FormatAnthropic, marked)
	if err != nil {
		t.Fatal(err)
	}
	startA, _ := sectionBounds(bodyA, accountant.SectionSystem)
	startB, _ := sectionBounds(bodyB, accountant.SectionSystem)
	if bodyA.Pos[startA].Hash != bodyB.Pos[startB].Hash {
		t.Errorf("system position hash differs with and without cache_control: %d != %d", bodyA.Pos[startA].Hash, bodyB.Pos[startB].Hash)
	}
	// The sizes must still differ: the size keeps the cache_control bytes.
	if bodyA.Pos[startA].Bytes == bodyB.Pos[startB].Bytes {
		t.Errorf("sizes should differ (marks count toward wire size), both are %d", bodyA.Pos[startA].Bytes)
	}
}

func TestParse_RejectsNonJSON(t *testing.T) {
	_, _, err := Parse(FormatAnthropic, []byte("not json"))
	if err == nil {
		t.Fatal("Parse of non-JSON: want an error, got nil")
	}
}

func TestFormatOf(t *testing.T) {
	cases := []struct {
		path string
		want Format
		ok   bool
	}{
		{"/v1/messages", FormatAnthropic, true},
		{"/zen/v1/messages", FormatAnthropic, true},
		{"/v1/chat/completions", FormatChat, true},
		{"/openai/deployments/d1/v1/chat/completions", FormatChat, true},
		{"/v1/responses", FormatResponses, true},
		{"/backend-api/codex/responses", 0, false},
		{"/v1/embeddings", 0, false},
	}
	for _, c := range cases {
		got, ok := FormatOf(c.path)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("FormatOf(%q) = %v, %v; want %v, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}

// TestAllMarksAsk1h pins the W6.6/W6.4 "every raw mark, not merged positions" rule: a position
// whose merged Breakpoint would read "1h" (because one of its marks does) must still make this
// false when another mark at that same position asked "5m" — the question
// apply.AllBreakpointsAskExtendedTTL answers for the proxy's keeper.
func TestAllMarksAsk1h(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			"every mark asks 1h",
			`{"system":[{"type":"text","text":"a","cache_control":{"ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`,
			true,
		},
		{
			"one of two marks asks 1h",
			`{"system":[{"type":"text","text":"a","cache_control":{"ttl":"5m"}},{"type":"text","text":"b","cache_control":{"ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`,
			false,
		},
		{
			"no marks at all",
			`{"system":"a","messages":[{"role":"user","content":"hi"}]}`,
			false,
		},
		{
			// One message carries both its own 5m mark and a nested content-block 1h mark: the
			// merged Breakpoint for this position would read "1h" (breakpointsOf's "1h wins"
			// rule), but AllMarksAsk1h must still see the 5m mark and answer false.
			"element mark and nested content mark disagree on one message",
			`{"system":"a","messages":[{"role":"user","cache_control":{"ttl":"5m"},"content":[{"type":"text","text":"hi","cache_control":{"ttl":"1h"}}]}]}`,
			false,
		},
		{
			"only a top-level automatic-caching mark, asking 1h",
			`{"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"hi"}]}`,
			true,
		},
		{
			"a top-level 5m mark next to a 1h block mark",
			`{"cache_control":{"type":"ephemeral"},"system":[{"type":"text","text":"a","cache_control":{"ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`,
			false,
		},
		{
			"an OpenAI-shaped body has no cache_control fields to find",
			`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AllMarksAsk1h([]byte(c.raw)); got != c.want {
				t.Errorf("AllMarksAsk1h(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestParse_InputAfterNonArrayMessages: a messages value that is not an array must not stop the
// scan before a later Responses input, so field order does not change the body.
func TestParse_InputAfterNonArrayMessages(t *testing.T) {
	for _, raw := range []string{
		`{"tools":[],"system":"s","messages":null,"input":"hi"}`,
		`{"input":"hi","tools":[],"system":"s","messages":null}`,
	} {
		body, _, err := Parse(FormatResponses, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		assertSizes(t, raw+" system", sectionSizes(body, accountant.SectionSystem), []int{3})
		assertSizes(t, raw+" messages", sectionSizes(body, accountant.SectionMessages), []int{4})
	}
}

// TestParse_ToolParamNamedCacheControlStaysInHash: only cache-metadata locations are marks. A
// tool parameter named cache_control is part of the tool definition, so a change to it changes
// the hash, while the tool's own mark still leaves it.
func TestParse_ToolParamNamedCacheControlStaysInHash(t *testing.T) {
	tool := func(typ, mark string) []byte {
		return []byte(`{"tools":[{"name":"t","input_schema":{"type":"object","properties":{"cache_control":{"type":"` +
			typ + `"}}}` + mark + `}],"messages":[{"role":"user","content":"hi"}]}`)
	}
	hash := func(raw []byte) uint64 {
		t.Helper()
		body, _, err := Parse(FormatAnthropic, raw)
		if err != nil {
			t.Fatal(err)
		}
		return uint64(body.Pos[0].Hash)
	}
	if hash(tool("string", "")) == hash(tool("number", "")) {
		t.Error("a tool parameter named cache_control was stripped from the hash")
	}
	if hash(tool("string", "")) != hash(tool("string", `,"cache_control":{"type":"ephemeral"}`)) {
		t.Error("the tool's own cache_control mark is still in the hash")
	}
	// A tool_use input value named cache_control is data too.
	use := func(v string) []byte {
		return []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"x","name":"t","input":{"cache_control":"` + v + `"}}]}]}`)
	}
	if hash(use("a")) == hash(use("b")) {
		t.Error("a tool_use input value named cache_control was stripped from the hash")
	}
	// A mark on a block nested in a tool_result's content still leaves the hash.
	nested := func(mark string) []byte {
		return []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"r"` + mark + `}]}]}]}`)
	}
	if hash(nested("")) != hash(nested(`,"cache_control":{"type":"ephemeral"}`)) {
		t.Error("a nested tool_result block's mark is still in the hash")
	}
}

// TestParse_AutomaticCachingBreakpoint: a top-level cache_control puts one breakpoint on the
// body's last position, merged with an explicit mark already there.
func TestParse_AutomaticCachingBreakpoint(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []wantBreak
	}{
		{
			"one user message",
			`{"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":"hi"}]}`,
			[]wantBreak{{"messages", 0, "5m"}},
		},
		{
			"1h, after messages",
			`{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}],"cache_control":{"type":"ephemeral","ttl":"1h"}}`,
			[]wantBreak{{"messages", 1, "1h"}},
		},
		{
			"explicit system mark plus automatic",
			`{"cache_control":{"type":"ephemeral"},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`,
			[]wantBreak{{"system", 0, "5m"}, {"messages", 0, "5m"}},
		},
		{
			"explicit mark on the last message is not listed twice",
			`{"cache_control":{"type":"ephemeral"},"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
			[]wantBreak{{"messages", 0, "5m"}},
		},
		{
			"no messages positions",
			`{"cache_control":{"type":"ephemeral"},"system":"s","messages":[]}`,
			[]wantBreak{{"system", 0, "5m"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, bps, err := Parse(FormatAnthropic, []byte(c.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(bps) != len(c.want) {
				t.Fatalf("got %+v, want %+v", bps, c.want)
			}
			for i, w := range c.want {
				if g := bps[i]; string(g.Pos.Section) != w.Section || g.Pos.Index != w.Index || g.TTL != w.TTL {
					t.Errorf("breakpoint %d: got %+v, want %+v", i, g, w)
				}
			}
		})
	}
}

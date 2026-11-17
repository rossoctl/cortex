package wire

import (
	"encoding/json"
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
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
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

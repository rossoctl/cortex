package translate

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/anthropic"
	"github.com/rossoctl/cortex/core/translate/pivot"
)

// validToolID is the pattern of rule S7.
var validToolID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func roundTripToolID(id string) string {
	if validToolID.MatchString(id) {
		return id
	}
	return "cx_" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// roundTripExpected changes want by only the differences that rules S3-S12
// allow, so that it equals the decoded Anthropic body.
func roundTripExpected(want pivot.Request, dropped []string) pivot.Request {
	isDropped := func(k string) bool { return slices.Contains(dropped, k) }
	dropExtra := func(x pivot.Extra) pivot.Extra {
		for k := range x {
			if _, field, ok := strings.Cut(k, "."); ok && isDropped(field) {
				delete(x, k)
			}
		}
		if len(x) == 0 {
			return nil
		}
		return x
	}
	want.Extra = dropExtra(want.Extra)
	if want.MaxTokens == 0 { // S4
		want.MaxTokens = 16000
		if want.Stream {
			want.MaxTokens = 64000
		}
	}
	if isDropped("temperature") {
		want.Temperature = nil
	}
	if isDropped("top_p") {
		want.TopP = nil
	}
	want.Thinking = nil // S11
	if want.Effort != "" {
		switch want.Effort { // S11
		case "none", "minimal", "low":
			want.Effort = "low"
		case "medium", "high", "xhigh", "max":
		default:
			want.Effort = ""
		}
	}

	var system []pivot.Block
	for _, b := range want.System {
		if b.Kind == pivot.BlockText {
			b.Extra = dropExtra(b.Extra)
			system = append(system, b)
		}
	}
	want.System = system

	var messages []pivot.Message
	for _, m := range want.Messages {
		m.Extra = dropExtra(m.Extra)
		if m.Role == "system" { // S5
			m.Role = "user"
		}
		var content []pivot.Block
		for _, b := range m.Content {
			b.Extra = dropExtra(b.Extra)
			switch b.Kind {
			case pivot.BlockThinking, pivot.BlockRedactedThinking:
				if b.Kind == pivot.BlockThinking && b.Signature == "" {
					continue
				}
				b.CacheControl = nil
			case pivot.BlockToolUse:
				b.ID = roundTripToolID(b.ID)
			case pivot.BlockToolResult:
				b.ToolUseID = roundTripToolID(b.ToolUseID)
				var inner []pivot.Block
				for _, c := range b.Content {
					if c.Kind == pivot.BlockText || c.Kind == pivot.BlockImage {
						c.Extra = dropExtra(c.Extra)
						inner = append(inner, c)
					}
				}
				b.Content = inner
			}
			content = append(content, b)
		}
		if len(content) > 0 {
			m.Content = content
			messages = append(messages, m)
		}
	}
	want.Messages = messages

	var tools []pivot.Tool
	for _, tool := range want.Tools {
		if tool.Type == "" { // S8: a server tool is not sent
			tool.Extra = dropExtra(tool.Extra)
			tools = append(tools, tool)
		}
	}
	want.Tools = tools
	return want
}

func indented(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// TestAnthropicRoundTrip checks rule S17: each Anthropic request decodes,
// encodes and decodes again to the same pivot request, apart from the
// differences that the rules allow.
func TestAnthropicRoundTrip(t *testing.T) {
	inputs, _ := filepath.Glob("testdata/request/*.json")
	if len(inputs) == 0 {
		t.Fatal("no request inputs")
	}
	p := anthropic.Protocol{}
	for _, in := range inputs {
		t.Run(filepath.Base(in), func(t *testing.T) {
			f, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			want, err := p.DecodeRequest(f)
			if err != nil {
				t.Fatal(err)
			}
			body, dropped, err := p.EncodeRequest(want, nil)
			if err != nil {
				t.Fatalf("EncodeRequest refused the request: %v", err)
			}
			got, err := p.DecodeRequest(body)
			if err != nil {
				t.Fatalf("the encoded body does not decode: %v\n%s", err, body)
			}
			want = roundTripExpected(want, dropped)
			delete(got.Extra, "anthropic.cache_control") // S12
			if len(got.Extra) == 0 {
				got.Extra = nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip differs (dropped %v):\n got %s\nwant %s", dropped, indented(got), indented(want))
			}
		})
	}
}

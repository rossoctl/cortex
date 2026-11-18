package translate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/chat"
	"github.com/rossoctl/cortex/core/translate/pivot"
)

// dropChatExtra removes the keys of the chat-completions prefix. The Chat
// encoder neither sends nor names them (decision 24).
func dropChatExtra(x pivot.Extra) pivot.Extra {
	for k := range x {
		if strings.HasPrefix(k, "chat-completions.") {
			delete(x, k)
		}
	}
	if len(x) == 0 {
		return nil
	}
	return x
}

// dropDocuments removes the document blocks, which the Chat encoder
// refuses (#1911 rule 14), and each message that has no blocks then.
func dropDocuments(req pivot.Request) pivot.Request {
	keep := func(blocks []pivot.Block) []pivot.Block {
		var out []pivot.Block
		for _, b := range blocks {
			if b.Kind == pivot.BlockDocument {
				continue
			}
			if b.Kind == pivot.BlockToolResult {
				b.Content = slices.DeleteFunc(slices.Clone(b.Content), func(c pivot.Block) bool { return c.Kind == pivot.BlockDocument })
			}
			out = append(out, b)
		}
		return out
	}
	var msgs []pivot.Message
	for _, m := range req.Messages {
		if m.Content = keep(m.Content); len(m.Content) > 0 {
			msgs = append(msgs, m)
		}
	}
	req.Messages = msgs
	return req
}

// chatRoundTripExpected changes want by only the differences that rule K10
// and decision 24 allow, so that it equals the decoded Chat body.
func chatRoundTripExpected(want pivot.Request, dropped []string) pivot.Request {
	want.Extra = dropChatExtra(want.Extra)
	switch want.Effort { // #1911 rule 19
	case "", "low", "medium", "high":
	case "none", "minimal":
		want.Effort = "low"
	default:
		want.Effort = "high"
	}
	for i := range want.System {
		want.System[i].Extra = dropChatExtra(want.System[i].Extra)
	}
	for i := range want.Tools {
		want.Tools[i].Extra = dropChatExtra(want.Tools[i].Extra)
	}

	var messages []pivot.Message
	for _, m := range want.Messages {
		m.Extra = dropChatExtra(m.Extra)
		if m.Role == "system" { // #1911 rule 13a
			m.Role = "user"
		}
		var content, toolImages, rest []pivot.Block
		var text *pivot.Block // the joined assistant text (#1911 rule 15)
		for _, b := range m.Content {
			b.Extra = dropChatExtra(b.Extra)
			switch b.Kind {
			case pivot.BlockThinking:
				if b.Signature == "" && slices.Contains(dropped, "thinking") {
					continue
				}
			case pivot.BlockText:
				if m.Role == "assistant" {
					if text == nil {
						content = append(content, pivot.Block{Kind: pivot.BlockText})
						text = &content[len(content)-1]
					}
					text.Text += b.Text
					continue
				}
			case pivot.BlockToolUse:
				if b.Input == nil {
					b.Input = json.RawMessage("{}")
				}
			case pivot.BlockToolResult:
				var inner []pivot.Block
				for _, c := range b.Content {
					c.Extra = dropChatExtra(c.Extra)
					if c.Kind == pivot.BlockImage { // #1911 rule 14
						toolImages = append(toolImages, c)
					} else {
						inner = append(inner, c)
					}
				}
				if !slices.ContainsFunc(inner, func(c pivot.Block) bool { return c.Kind == pivot.BlockText }) {
					inner = append(inner, pivot.Block{Kind: pivot.BlockText})
				}
				b.Content = inner
				content = append(content, b)
				continue
			}
			if b.Kind != pivot.BlockToolResult && slices.ContainsFunc(content, func(c pivot.Block) bool { return c.Kind == pivot.BlockToolResult }) {
				rest = append(rest, b)
				continue
			}
			content = append(content, b)
		}
		if text != nil && text.Text == "" { // the Chat encoder sends no empty content
			content = slices.DeleteFunc(content, func(c pivot.Block) bool { return c.Kind == pivot.BlockText && c.Text == "" })
		}
		// The images of the tool results go in a user message after the
		// tool messages, and the decoder joins it after the results (K3).
		// The other blocks then come back in a message of their own.
		if len(toolImages) > 0 {
			content = append(content, toolImages...)
			if len(rest) > 0 {
				messages = appendMessage(messages, pivot.Message{Role: m.Role, Content: content, Extra: m.Extra})
				messages = append(messages, pivot.Message{Role: m.Role, Content: rest})
				continue
			}
		} else {
			content = append(content, rest...)
		}
		messages = appendMessage(messages, pivot.Message{Role: m.Role, Content: content, Extra: m.Extra})
	}
	want.Messages = messages
	return want
}

// appendMessage appends m, or joins it to the last message when that is a
// user message that ends with a tool_result (K3). A message with no blocks
// is left out.
func appendMessage(msgs []pivot.Message, m pivot.Message) []pivot.Message {
	if len(m.Content) == 0 {
		return msgs
	}
	if n := len(msgs); n > 0 && m.Role == "user" && msgs[n-1].Role == "user" {
		last := msgs[n-1].Content
		if last[len(last)-1].Kind == pivot.BlockToolResult {
			msgs[n-1].Content = append(last, m.Content...)
			return msgs
		}
	}
	return append(msgs, m)
}

// TestChatRoundTrip checks rule K10: each Chat request decodes, encodes
// and decodes again to the same pivot request, apart from the differences
// that the rules allow.
func TestChatRoundTrip(t *testing.T) {
	inputs, _ := filepath.Glob(chatAnthropicDir + "/request/*.json")
	if len(inputs) == 0 {
		t.Fatal("no request inputs")
	}
	p := chat.Protocol{}
	for _, in := range inputs {
		t.Run(filepath.Base(in), func(t *testing.T) {
			f, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			want, err := p.DecodeRequest(f)
			var re *pivot.RequestError
			if errors.As(err, &re) {
				t.Skipf("a refusal fixture: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			want = dropDocuments(want)
			body, dropped, err := p.EncodeRequest(want, nil)
			if err != nil {
				t.Fatalf("EncodeRequest refused the request: %v", err)
			}
			got, err := p.DecodeRequest(body)
			if err != nil {
				t.Fatalf("the encoded body does not decode: %v\n%s", err, body)
			}
			want = chatRoundTripExpected(want, dropped)
			got.Extra = dropChatExtra(got.Extra)
			for i := range got.Tools {
				got.Tools[i].Extra = dropChatExtra(got.Tools[i].Extra)
			}
			for i := range got.Messages {
				m := &got.Messages[i]
				m.Extra = dropChatExtra(m.Extra)
				for j := range m.Content {
					m.Content[j].Extra = dropChatExtra(m.Content[j].Extra)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip differs (dropped %v):\n got %s\nwant %s", dropped, indented(got), indented(want))
			}
		})
	}
}

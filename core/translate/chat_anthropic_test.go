package translate

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"testing/iotest"

	"github.com/rossoctl/cortex/core/translate/anthropic"
	"github.com/rossoctl/cortex/core/translate/chat"
	"github.com/rossoctl/cortex/core/translate/pivot"
)

const chatAnthropicDir = "testdata/chat-anthropic"

var createdRE = regexp.MustCompile(`"created":[0-9]+`)

// normCreated sets each "created" time to 0, so that a golden has fixed
// bytes (decision 2).
func normCreated(b []byte) []byte {
	return createdRE.ReplaceAll(b, []byte(`"created":0`))
}

// chatGoldenName gives the golden file of one Anthropic reply input
// converted to a Chat reply (decision 3).
func chatGoldenName(c replyCase, agentStream, includeUsage bool) string {
	name := filepath.Join(chatAnthropicDir, "reply", filepath.Base(c.path))
	if includeUsage {
		name += ".include-usage"
	}
	if agentStream {
		return name + ".to-sse.golden"
	}
	return name + ".to-json.golden"
}

// TestChatToAnthropicReplyGolden converts each Anthropic reply to the Chat
// reply that a Chat agent reads (rules K5-K8).
func TestChatToAnthropicReplyGolden(t *testing.T) {
	withUsage := pivot.Request{Model: "m", Extra: pivot.Extra{"chat-completions.include_usage": json.RawMessage("true")}}
	for _, c := range replyCases(t, responsesAnthropicDir+"/reply") {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, agentStream := range c.agentForms() {
			name := chatGoldenName(c, agentStream, false)
			t.Run(filepath.Base(name), func(t *testing.T) {
				_, out := Reply(Chat, Anthropic, pivot.Request{Model: "m"}, c.status, c.stream, agentStream, body)
				checkGolden(t, name, normCreated(out))
			})
		}
		if c.stream && c.status >= 200 && c.status < 300 {
			name := chatGoldenName(c, true, true)
			t.Run(filepath.Base(name), func(t *testing.T) {
				_, out := Reply(Chat, Anthropic, withUsage, c.status, c.stream, true, body)
				checkGolden(t, name, normCreated(out))
			})
		}
	}
}

// TestChatToAnthropicReplyEveryByte reads each Anthropic reply one byte
// per Read. The Chat output must equal the golden.
func TestChatToAnthropicReplyEveryByte(t *testing.T) {
	for _, c := range replyCases(t, responsesAnthropicDir+"/reply") {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, agentStream := range c.agentForms() {
			name := chatGoldenName(c, agentStream, false)
			t.Run(filepath.Base(name), func(t *testing.T) {
				want, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				r := NewReplyReader(io.NopCloser(iotest.OneByteReader(bytes.NewReader(body))),
					anthropic.Protocol{}.NewReplyDecoder(c.status, c.stream),
					chat.Protocol{}.NewReplyEncoder(pivot.Request{Model: "m"}, agentStream))
				got, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				if got = normCreated(got); !bytes.Equal(got, want) {
					t.Errorf("got %s\nwant %s", got, want)
				}
			})
		}
	}
}

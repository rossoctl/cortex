package translate

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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

// chatAnthropicGolden gives the golden form of one Chat request sent to an
// Anthropic server: the first line names the dropped fields, the second
// line is the body. A refusal gives one "error:" line.
func chatAnthropicGolden(in []byte) []byte {
	c, err := Request(Chat, Anthropic, in, nil)
	if err != nil {
		return []byte("error: " + err.Error() + "\n")
	}
	return []byte("dropped: " + strings.Join(c.Dropped, ",") + "\n" + string(c.Body) + "\n")
}

// TestChatToAnthropicRequestGolden converts each hand-written Chat request
// to an Anthropic Messages body (rules K3 and K4).
func TestChatToAnthropicRequestGolden(t *testing.T) {
	inputs, _ := filepath.Glob(chatAnthropicDir + "/request/*.json")
	if len(inputs) == 0 {
		t.Fatal("no request inputs")
	}
	for _, in := range inputs {
		t.Run(filepath.Base(in), func(t *testing.T) {
			body, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, strings.TrimSuffix(in, ".json")+".golden", chatAnthropicGolden(body))
		})
	}
}

func encodeChatToAnthropic(t *testing.T, in []byte) string {
	t.Helper()
	c, err := Request(Chat, Anthropic, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(c.Body)
}

// TestChatToAnthropicDeterministic checks rule 9a of #1911 for the Chat
// decoder: the same request gives the same bytes, and one more turn keeps
// the old bytes.
func TestChatToAnthropicDeterministic(t *testing.T) {
	in, err := os.ReadFile(chatAnthropicDir + "/request/full_multiturn.json")
	if err != nil {
		t.Fatal(err)
	}
	first := encodeChatToAnthropic(t, in)
	if again := encodeChatToAnthropic(t, in); again != first {
		t.Fatalf("two encodings differ:\n%s\n%s", first, again)
	}

	const lastOut = `{"role":"user","content":[{"type":"text","text":"Thanks. Now run the tests."}]}`
	end := strings.Index(first, lastOut)
	if end < 0 {
		t.Fatalf("the last old message is not in the output:\n%s", first)
	}
	end += len(lastOut)
	const lastIn = `{"role":"user","content":"Thanks. Now run the tests."}`
	longer := bytes.Replace(in, []byte(lastIn), []byte(lastIn+
		`,{"role":"assistant","content":"Done."},{"role":"user","content":"And the linter."}`), 1)
	if bytes.Equal(longer, in) {
		t.Fatal("the fixture does not hold the last user turn")
	}
	grown := encodeChatToAnthropic(t, longer)
	if grown == first {
		t.Fatal("the new turn is not in the output")
	}
	if !strings.HasPrefix(grown, first[:end]) {
		t.Errorf("one more turn changed the old bytes:\n old %s\n new %s", first[:end], grown)
	}
}

// TestChatToAnthropicHeaders checks rule K2 and S2: only the headers that
// an Anthropic server reads stay.
func TestChatToAnthropicHeaders(t *testing.T) {
	in, err := os.ReadFile(chatAnthropicDir + "/request/k3_user_text.json")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	for _, k := range []string{"OpenAI-Beta", "OpenAI-Organization", "OpenAI-Project", "Anthropic-Beta", "X-Keep"} {
		h.Set(k, "v")
	}
	if _, err := Request(Chat, Anthropic, in, h); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range h {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"Anthropic-Version", "X-Keep"}) ||
		h.Get("Anthropic-Version") != "2023-06-01" || h.Get("X-Keep") != "v" {
		t.Errorf("headers after = %v, want only Anthropic-Version: 2023-06-01 and X-Keep", h)
	}
}

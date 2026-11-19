package translate

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

const responsesAnthropicDir = "testdata/responses-anthropic"

// topCache is the end of a body with the top-level cache marker (S12).
const topCache = `,"cache_control":{"type":"ephemeral"}}`

// responsesAnthropicGolden gives the golden form of one Responses request
// sent to an Anthropic server: the first line names the dropped fields,
// the second line is the body. A refusal gives one "error:" line.
func responsesAnthropicGolden(in []byte) []byte {
	c, err := Request(Responses, Anthropic, in, nil)
	if err != nil {
		return []byte("error: " + err.Error() + "\n")
	}
	return []byte("dropped: " + strings.Join(c.Dropped, ",") + "\n" + string(c.Body) + "\n")
}

// TestResponsesToAnthropicRequestGolden converts each hand-written Codex
// request to an Anthropic Messages body (rules S3-S12).
func TestResponsesToAnthropicRequestGolden(t *testing.T) {
	inputs, _ := filepath.Glob(responsesAnthropicDir + "/request/*.json")
	if len(inputs) == 0 {
		t.Fatal("no request inputs")
	}
	for _, in := range inputs {
		t.Run(filepath.Base(in), func(t *testing.T) {
			body, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			got := responsesAnthropicGolden(body)
			checkGolden(t, strings.TrimSuffix(in, ".json")+".golden", got)
			if !bytes.HasPrefix(got, []byte("error: ")) && !bytes.HasSuffix(got, []byte(topCache+"\n")) {
				t.Errorf("the body does not end with the top-level cache marker (S12):\n%s", got)
			}
		})
	}
}

// TestResponsesToAnthropicReplyGolden converts each Anthropic reply to
// the Responses reply that Codex reads (rules S13-S15).
func TestResponsesToAnthropicReplyGolden(t *testing.T) {
	for _, c := range replyCases(t, responsesAnthropicDir+"/reply") {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, agentStream := range c.agentForms() {
			name := goldenName(c, agentStream)
			t.Run(filepath.Base(name), func(t *testing.T) {
				_, out := Reply(Responses, Anthropic, pivot.Request{}, c.status, c.stream, agentStream, body)
				checkGolden(t, name, out)
			})
		}
	}
}

func encodeAnthropic(t *testing.T, in []byte) string {
	t.Helper()
	c, err := Request(Responses, Anthropic, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	return string(c.Body)
}

// TestResponsesToAnthropicDeterministic checks rule 9a of #1911 and S12:
// the same request gives the same bytes, and one more turn keeps the old
// bytes and the same top-level cache marker.
func TestResponsesToAnthropicDeterministic(t *testing.T) {
	in, err := os.ReadFile(responsesAnthropicDir + "/request/full_multiturn.json")
	if err != nil {
		t.Fatal(err)
	}
	first := encodeAnthropic(t, in)
	if again := encodeAnthropic(t, in); again != first {
		t.Fatalf("two encodings differ:\n%s\n%s", first, again)
	}

	const lastOut = `{"role":"user","content":[{"type":"text","text":"Thanks. Now run the tests."}]}`
	end := strings.Index(first, lastOut)
	if end < 0 {
		t.Fatalf("the last old message is not in the output:\n%s", first)
	}
	end += len(lastOut)
	const lastIn = `"text": "Thanks. Now run the tests."}]}`
	longer := bytes.Replace(in, []byte(lastIn), []byte(lastIn+
		`, {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "And the linter."}]}`), 1)
	if bytes.Equal(longer, in) {
		t.Fatal("the fixture does not hold the last user turn")
	}
	grown := encodeAnthropic(t, longer)
	if grown == first {
		t.Fatal("the new turn is not in the output")
	}
	if !strings.HasPrefix(grown, first[:end]) {
		t.Errorf("one more turn changed the old bytes:\n old %s\n new %s", first[:end], grown)
	}
	if !strings.HasSuffix(first, topCache) || !strings.HasSuffix(grown, topCache) {
		t.Errorf("an output does not end with %s:\n%s\n%s", topCache, first, grown)
	}
}

// TestHeaders checks R3 and S2: only the headers that an Anthropic server
// reads stay.
func TestHeaders(t *testing.T) {
	in, err := os.ReadFile(responsesAnthropicDir + "/request/s11_no_effort.json")
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	for _, k := range []string{"OpenAI-Beta", "Anthropic-Beta", "X-Keep"} {
		h.Set(k, "v")
	}
	if _, err := Request(Responses, Anthropic, in, h); err != nil {
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

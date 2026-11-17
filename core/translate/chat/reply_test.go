package chat

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// sse writes each chunk as one data line, as Bob does, with a padded
// ping comment line first.
func sse(chunks ...string) string {
	var b strings.Builder
	b.WriteString(":                ping\n\n")
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

func decodeAll(status int, stream bool, body string) []pivot.Event {
	d := Protocol{}.NewReplyDecoder(status, stream)
	evs := d.Feed([]byte(body))
	return append(evs, d.End()...)
}

// decodeBytewise feeds the body one byte at a time.
func decodeBytewise(status int, stream bool, body string) []pivot.Event {
	d := Protocol{}.NewReplyDecoder(status, stream)
	var evs []pivot.Event
	for i := 0; i < len(body); i++ {
		evs = append(evs, d.Feed([]byte{body[i]})...)
	}
	return append(evs, d.End()...)
}

func checkEvents(t *testing.T, status int, stream bool, body string, want []pivot.Event) {
	t.Helper()
	if got := decodeAll(status, stream, body); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n got %+v\nwant %+v", got, want)
	}
	if got := decodeBytewise(status, stream, body); !reflect.DeepEqual(got, want) {
		t.Errorf("events, fed one byte at a time:\n got %+v\nwant %+v", got, want)
	}
}

func chunk(delta string) string {
	return `{"id":"chatcmpl-1","model":"claude","choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}]}`
}

func finish(reason string) string {
	return `{"id":"chatcmpl-1","model":"claude","choices":[{"index":0,"delta":{},"finish_reason":"` + reason + `"}]}`
}

func usageChunk(u string) string {
	return `{"id":"chatcmpl-1","model":"claude","choices":[],"usage":` + u + `}`
}

func start() pivot.Event {
	return pivot.Event{Kind: pivot.EventStart, ID: "chatcmpl-1", Model: "claude"}
}
func open(i int, k pivot.BlockKind) pivot.Event {
	return pivot.Event{Kind: pivot.EventBlockStart, Index: i, Block: k}
}
func openTool(i int, id, name string) pivot.Event {
	return pivot.Event{Kind: pivot.EventBlockStart, Index: i, Block: pivot.BlockToolUse, ToolID: id, ToolName: name}
}
func delta(i int, k pivot.DeltaKind, v string) pivot.Event {
	return pivot.Event{Kind: pivot.EventDelta, Index: i, Delta: k, Value: v}
}
func closeBlock(i int) pivot.Event    { return pivot.Event{Kind: pivot.EventBlockStop, Index: i} }
func usage(u pivot.Usage) pivot.Event { return pivot.Event{Kind: pivot.EventUsage, Usage: u} }
func stop(r string) pivot.Event       { return pivot.Event{Kind: pivot.EventStop, StopReason: r} }
func apiError(status int, typ, msg string) pivot.Event {
	return pivot.Event{Kind: pivot.EventError, Status: status, ErrorType: typ, Message: msg}
}

func TestReplyDecoder_BobShape1(t *testing.T) {
	body := sse(
		chunk(`{"reasoning":"391","thinking_blocks":[{"type":"thinking","thinking":"391"}],"role":"assistant"}`),
		chunk(`{"reasoning":" Not","thinking_blocks":[{"type":"thinking","thinking":" Not"}]}`),
		chunk(`{"thinking_blocks":[{"type":"thinking","thinking":"","signature":"EvkB"}]}`),
		chunk(`{"content":"391 is not prime"}`),
		chunk(`{"tool_calls":[{"id":"tooluse_9nf","function":{"arguments":"","name":"Bash"},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"function":{"arguments":"{\"comma"},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"function":{"arguments":"nd\": \"echo done\"}"},"type":"function","index":0}]}`),
		finish("tool_calls"),
		usageChunk(`{"prompt_tokens":578,"completion_tokens":55,"total_tokens":633,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}`),
		"[DONE]",
	)
	checkEvents(t, 200, true, body, []pivot.Event{
		start(),
		open(0, pivot.BlockThinking),
		delta(0, pivot.DeltaThinking, "391"),
		delta(0, pivot.DeltaThinking, " Not"),
		delta(0, pivot.DeltaSignature, "EvkB"),
		closeBlock(0),
		open(1, pivot.BlockText),
		delta(1, pivot.DeltaText, "391 is not prime"),
		closeBlock(1),
		openTool(2, "tooluse_9nf", "Bash"),
		delta(2, pivot.DeltaInputJSON, `{"comma`),
		delta(2, pivot.DeltaInputJSON, `nd": "echo done"}`),
		closeBlock(2),
		usage(pivot.Usage{Input: 578, Output: 55}),
		stop("tool_use"),
	})
}

func TestReplyDecoder_BobShape2(t *testing.T) {
	body := sse(
		chunk(`{"reasoning":"","thinking_blocks":[{"type":"thinking","thinking":""}],"content":"","role":"assistant"}`),
		chunk(`{"reasoning":"","thinking_blocks":[{"type":"thinking","thinking":"","signature":"CAQS"}],"content":""}`),
		chunk(`{"content":"hello"}`),
		chunk(`{"content":"","tool_calls":[{"id":"tooluse_Z","function":{"arguments":"","name":"execute_command"},"type":"function","index":0}]}`),
		chunk(`{"content":"","tool_calls":[{"function":{"arguments":"{}"},"type":"function","index":0}]}`),
		finish("tool_calls"),
		usageChunk(`{"prompt_tokens":22333,"completion_tokens":216,"cache_read_input_tokens":21649,"cache_creation_input_tokens":682,"completion_tokens_details":{"reasoning_tokens":0}}`),
		"[DONE]",
	)
	checkEvents(t, 200, true, body, []pivot.Event{
		start(),
		open(0, pivot.BlockThinking),
		delta(0, pivot.DeltaSignature, "CAQS"),
		closeBlock(0),
		open(1, pivot.BlockText),
		delta(1, pivot.DeltaText, "hello"),
		closeBlock(1),
		openTool(2, "tooluse_Z", "execute_command"),
		delta(2, pivot.DeltaInputJSON, "{}"),
		closeBlock(2),
		usage(pivot.Usage{Input: 2, Output: 216, CacheRead: 21649, CacheCreation: 682}),
		stop("tool_use"),
	})
}

func TestReplyDecoder_BobShape3(t *testing.T) {
	const id = "call_3833__thought__AY89a1+8zd/x=="
	body := sse(
		chunk(`{"provider_specific_fields":{"thought_signatures":["AY89"]},"role":"assistant","tool_calls":[{"id":"`+id+`","function":{"arguments":"{\"task\":\"x\"}","name":"spawn_subagent"},"type":"function","index":0}]}`),
		finish("tool_calls"),
		usageChunk(`{"prompt_tokens":13314,"completion_tokens":296,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"completion_tokens_details":{"reasoning_tokens":249}}`),
		"[DONE]",
	)
	checkEvents(t, 200, true, body, []pivot.Event{
		start(),
		openTool(0, id, "spawn_subagent"),
		delta(0, pivot.DeltaInputJSON, `{"task":"x"}`),
		closeBlock(0),
		usage(pivot.Usage{Input: 13314, Output: 296}),
		stop("tool_use"),
	})
}

func TestReplyDecoder_FinishReasons(t *testing.T) {
	for reason, want := range map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
		"content_filter": "refusal", "something_new": "end_turn",
	} {
		evs := decodeAll(200, true, sse(chunk(`{"content":"a"}`), finish(reason), "[DONE]"))
		if got := evs[len(evs)-1]; got != stop(want) {
			t.Errorf("finish_reason %q: last event %+v, want stop %q", reason, got, want)
		}
	}
}

// Usage falls back to prompt_tokens_details.cached_tokens (rule 22).
func TestReplyDecoder_CachedTokens(t *testing.T) {
	evs := decodeAll(200, true, sse(chunk(`{"content":"a"}`), finish("stop"),
		usageChunk(`{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60}}`), "[DONE]"))
	if got := evs[len(evs)-2]; got != usage(pivot.Usage{Input: 40, Output: 5, CacheRead: 60}) {
		t.Errorf("usage event = %+v", got)
	}
}

// A body that ends after finish_reason with no [DONE] still ends the reply.
func TestReplyDecoder_NoDone(t *testing.T) {
	checkEvents(t, 200, true, sse(chunk(`{"content":"a"}`), finish("stop")), []pivot.Event{
		start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "a"), closeBlock(0),
		usage(pivot.Usage{}), stop("end_turn"),
	})
}

const endedEarly = "the inference server's stream ended before the reply was complete"

func TestReplyDecoder_EndsEarly(t *testing.T) {
	want := []pivot.Event{
		start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "half"),
		apiError(502, "api_error", endedEarly),
	}
	checkEvents(t, 200, true, sse(chunk(`{"content":"half"}`)), want)
	checkEvents(t, 200, true, sse(chunk(`{"content":"half"}`), "[DONE]"), want)
}

func TestReplyDecoder_ClosedIndexPiece(t *testing.T) {
	body := sse(
		chunk(`{"tool_calls":[{"id":"a","function":{"arguments":"{}","name":"A"},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"id":"b","function":{"arguments":"{","name":"B"},"type":"function","index":1}]}`),
		chunk(`{"tool_calls":[{"function":{"arguments":"\"late\""},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"function":{"arguments":"}"},"type":"function","index":1}]}`),
		finish("tool_calls"),
		"[DONE]",
	)
	checkEvents(t, 200, true, body, []pivot.Event{
		start(),
		openTool(0, "a", "A"), delta(0, pivot.DeltaInputJSON, "{}"), closeBlock(0),
		openTool(1, "b", "B"), delta(1, pivot.DeltaInputJSON, "{"),
		apiError(502, "api_error", "the inference server sent arguments for tool call 0 after that tool call ended"),
	})
}

func TestReplyDecoder_ErrorChunk(t *testing.T) {
	checkEvents(t, 200, true, sse(chunk(`{"content":"a"}`), `{"error":{"message":"overloaded","type":"overloaded_error"}}`), []pivot.Event{
		start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "a"),
		apiError(500, "overloaded_error", "overloaded"),
	})
	checkEvents(t, 200, true, sse(`{"error":{"message":"boom","type":"litellm_error","code":429}}`), []pivot.Event{
		apiError(429, "api_error", "boom"),
	})
}

func TestReplyDecoder_HTMLErrorPage(t *testing.T) {
	checkEvents(t, 502, true, "<html>bad gateway</html>\n", []pivot.Event{
		apiError(502, "api_error", "<html>bad gateway</html>"),
	})
	long := strings.Repeat("x", 600)
	if got := decodeAll(503, false, long); got[0].Message != long[:512] {
		t.Errorf("long body message has %d bytes, want 512", len(got[0].Message))
	}
	checkEvents(t, 503, false, "", []pivot.Event{apiError(503, "api_error", "Service Unavailable")})
}

func TestReplyDecoder_JSONError(t *testing.T) {
	checkEvents(t, 404, false, `{"error":{"message":"model \"claude-opus-5-5\" not found or not accessible","type":"not_found_error"}}`, []pivot.Event{
		apiError(404, "not_found_error", `model "claude-opus-5-5" not found or not accessible`),
	})
	checkEvents(t, 403, false, `{"error":{"message":"no access","type":"team_model_access_denied"}}`, []pivot.Event{
		apiError(403, "permission_error", "no access"),
	})
}

func TestReplyDecoder_NotStream(t *testing.T) {
	body := `{"id":"chatcmpl-1","model":"claude","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant",` +
		`"content":"hi","thinking_blocks":[{"type":"thinking","thinking":"hmm","signature":"SIG"}],"reasoning_content":"hmm",` +
		`"tool_calls":[{"id":"call_1__thought__a+b=","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":3,"cache_read_input_tokens":4}}`
	checkEvents(t, 200, false, body, []pivot.Event{
		start(),
		open(0, pivot.BlockThinking), delta(0, pivot.DeltaThinking, "hmm"), delta(0, pivot.DeltaSignature, "SIG"), closeBlock(0),
		open(1, pivot.BlockText), delta(1, pivot.DeltaText, "hi"), closeBlock(1),
		openTool(2, "call_1__thought__a+b=", "Bash"), delta(2, pivot.DeltaInputJSON, `{"command":"ls"}`), closeBlock(2),
		usage(pivot.Usage{Input: 6, Output: 3, CacheRead: 4}),
		stop("tool_use"),
	})

	// No thinking_blocks: one unsigned thinking block from reasoning.
	checkEvents(t, 200, false, `{"id":"chatcmpl-1","model":"claude","choices":[{"finish_reason":"stop","message":{"content":"a","reasoning":"r"}}]}`, []pivot.Event{
		start(),
		open(0, pivot.BlockThinking), delta(0, pivot.DeltaThinking, "r"), closeBlock(0),
		open(1, pivot.BlockText), delta(1, pivot.DeltaText, "a"), closeBlock(1),
		usage(pivot.Usage{}), stop("end_turn"),
	})
}

func TestReplyDecoder_InvalidArgumentsNotStream(t *testing.T) {
	body := `{"id":"chatcmpl-1","model":"claude","choices":[{"finish_reason":"tool_calls","message":{"content":"x",` +
		`"tool_calls":[{"id":"c","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}`
	checkEvents(t, 200, false, body, []pivot.Event{
		apiError(502, "api_error", "the inference server sent tool Bash arguments that are not valid JSON"),
	})
}

func TestEncodeError(t *testing.T) {
	got := string(Protocol{}.EncodeError(400, "invalid_request_error", "bad <x>"))
	if want := `{"error":{"message":"bad <x>","type":"invalid_request_error","code":null}}`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// Two whole tool calls at one index are two tool_use blocks (rule 20a).
func TestReplyDecoder_NewIDOnOpenIndex(t *testing.T) {
	body := sse(
		chunk(`{"tool_calls":[{"id":"a","function":{"arguments":"{\"p\":1}","name":"A"},"type":"function","index":0},{"id":"b","function":{"arguments":"{\"q\":2}","name":"B"},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"id":"b","function":{"arguments":""},"type":"function","index":0}]}`),
		finish("tool_calls"),
		"[DONE]",
	)
	checkEvents(t, 200, true, body, []pivot.Event{
		start(),
		openTool(0, "a", "A"), delta(0, pivot.DeltaInputJSON, `{"p":1}`), closeBlock(0),
		openTool(1, "b", "B"), delta(1, pivot.DeltaInputJSON, `{"q":2}`), closeBlock(1),
		usage(pivot.Usage{}), stop("tool_use"),
	})
}

// Three calls at index 0, each in its own chunk: closing a and b must not
// refuse c.
func TestReplyDecoder_ThreeNewIDsOnIndexZero(t *testing.T) {
	body := sse(
		chunk(`{"tool_calls":[{"id":"a","function":{"arguments":"{}","name":"A"},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"id":"b","function":{"arguments":"{}","name":"B"},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"id":"c","function":{"arguments":"{}","name":"C"},"type":"function","index":0}]}`),
		finish("tool_calls"),
		"[DONE]",
	)
	checkEvents(t, 200, true, body, []pivot.Event{
		start(),
		openTool(0, "a", "A"), delta(0, pivot.DeltaInputJSON, "{}"), closeBlock(0),
		openTool(1, "b", "B"), delta(1, pivot.DeltaInputJSON, "{}"), closeBlock(1),
		openTool(2, "c", "C"), delta(2, pivot.DeltaInputJSON, "{}"), closeBlock(2),
		usage(pivot.Usage{}), stop("tool_use"),
	})
}

// An id whose block ended comes back: refuse it.
func TestReplyDecoder_ClosedIDAgain(t *testing.T) {
	body := sse(
		chunk(`{"tool_calls":[{"id":"a","function":{"arguments":"{}","name":"A"},"type":"function","index":0}]}`),
		chunk(`{"tool_calls":[{"id":"b","function":{"arguments":"{}","name":"B"},"type":"function","index":1}]}`),
		chunk(`{"tool_calls":[{"id":"a","function":{"arguments":"{}","name":"A"},"type":"function","index":0}]}`),
		finish("tool_calls"),
		"[DONE]",
	)
	checkEvents(t, 200, true, body, []pivot.Event{
		start(),
		openTool(0, "a", "A"), delta(0, pivot.DeltaInputJSON, "{}"), closeBlock(0),
		openTool(1, "b", "B"), delta(1, pivot.DeltaInputJSON, "{}"),
		apiError(502, "api_error", "the inference server sent tool call a again after that tool call ended"),
	})
}

// A reply with a tool call stops with tool_use when finish_reason is stop:
// vLLM and OpenAI send stop for a forced named tool. A length cut
// stays max_tokens.
func TestReplyDecoder_ToolCallWithStop(t *testing.T) {
	call := chunk(`{"tool_calls":[{"id":"a","function":{"arguments":"{}","name":"A"},"type":"function","index":0}]}`)
	for reason, want := range map[string]string{"stop": "tool_use", "length": "max_tokens"} {
		checkEvents(t, 200, true, sse(call, finish(reason), "[DONE]"), []pivot.Event{
			start(),
			openTool(0, "a", "A"), delta(0, pivot.DeltaInputJSON, `{}`), closeBlock(0),
			usage(pivot.Usage{}), stop(want),
		})
		body := `{"id":"chatcmpl-1","model":"claude","choices":[{"finish_reason":"` + reason + `","message":{` +
			`"tool_calls":[{"id":"a","type":"function","function":{"name":"A","arguments":"{}"}}]}}]}`
		checkEvents(t, 200, false, body, []pivot.Event{
			start(),
			openTool(0, "a", "A"), delta(0, pivot.DeltaInputJSON, `{}`), closeBlock(0),
			usage(pivot.Usage{}), stop(want),
		})
	}
}

// A not-streamed reply refuses a tool call with no id or a repeated id
// (rule 20a).
func TestReplyDecoder_ToolCallIDsNotStream(t *testing.T) {
	for calls, msg := range map[string]string{
		`{"type":"function","function":{"name":"A","arguments":"{}"}}`:                                                                                "the inference server sent tool A with no id",
		`{"id":"a","type":"function","function":{"name":"A","arguments":"{}"}},{"id":"a","type":"function","function":{"name":"B","arguments":"{}"}}`: "the inference server sent tool call a twice",
	} {
		body := `{"id":"chatcmpl-1","model":"claude","choices":[{"finish_reason":"tool_calls","message":{"content":"x","tool_calls":[` + calls + `]}}]}`
		checkEvents(t, 200, false, body, []pivot.Event{apiError(502, "api_error", msg)})
	}
}

func TestReplyDecoder_OverLimit(t *testing.T) {
	// Valid JSON, so only the limit makes it an error.
	body := `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}` + strings.Repeat(" ", maxReplyBuffer)
	d := Protocol{}.NewReplyDecoder(200, false)
	evs := append(d.Feed([]byte(body)), d.End()...)
	if len(evs) != 1 || evs[0].Kind != pivot.EventError || evs[0].Status != 502 {
		t.Errorf("not streamed: events = %+v, want one 502 error", evs)
	}
	// Bytes after [DONE] are not read, so they give no error.
	d = Protocol{}.NewReplyDecoder(200, true)
	evs = d.Feed([]byte(sse(chunk(`{"content":"a"}`), finish("stop"), "[DONE]") + strings.Repeat("x", maxReplyBuffer+1)))
	for _, ev := range evs {
		if ev.Kind == pivot.EventError {
			t.Errorf("stream: events = %+v, want no error after [DONE]", evs)
		}
	}
	d = Protocol{}.NewReplyDecoder(200, true)
	evs = d.Feed([]byte("data: " + strings.Repeat("x", maxReplyBuffer)))
	if len(evs) != 1 || evs[0].Kind != pivot.EventError || evs[0].Status != 502 {
		t.Errorf("stream line: events = %+v, want one 502 error", evs)
	}
}

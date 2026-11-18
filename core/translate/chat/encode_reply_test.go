package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// fixNow sets a fixed time for the created field.
func fixNow(t *testing.T) {
	t.Helper()
	now = func() time.Time { return time.Unix(1700000000, 0) }
	t.Cleanup(func() { now = time.Now })
}

// encodeEvents gives the bytes of each Encode call and of End, joined.
func encodeEvents(req pivot.Request, stream bool, evs ...pivot.Event) string {
	enc := Protocol{}.NewReplyEncoder(req, stream)
	var b strings.Builder
	for _, ev := range evs {
		b.Write(enc.Encode(ev))
	}
	b.Write(enc.End())
	return b.String()
}

const (
	chunkHead = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"claude","choices":[`
	done      = "data: [DONE]\n\n"
	usage60   = `{"prompt_tokens":60,"completion_tokens":5,"total_tokens":65,"prompt_tokens_details":{"cached_tokens":20},"cache_read_input_tokens":20,"cache_creation_input_tokens":30}`
	zeroUsage = `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0},"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
	roleChunk = chunkHead + `{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
)

func wantChunk(delta string) string {
	return chunkHead + `{"index":0,"delta":` + delta + `,"finish_reason":null}]}` + "\n\n"
}

// wantStop gives the finish_reason chunk, with usage when u is not "".
func wantStop(reason, u string) string {
	s := chunkHead + `{"index":0,"delta":{},"finish_reason":"` + reason + `"}]`
	if u != "" {
		s += `,"usage":` + u
	}
	return s + "}\n\n"
}

func wantUsageChunk(u string) string {
	return `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"claude","choices":[],"usage":` + u + "}\n\n"
}

func checkOut(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n got %s\nwant %s", name, got, want)
	}
}

func textEvents() []pivot.Event {
	return []pivot.Event{
		start(), open(0, pivot.BlockText),
		delta(0, pivot.DeltaText, "Hel"), delta(0, pivot.DeltaText, "lo"),
		closeBlock(0),
		usage(pivot.Usage{Input: 10, Output: 5, CacheRead: 20, CacheCreation: 30}),
		stop("end_turn"),
	}
}

func TestReplyEncoder_Text(t *testing.T) {
	fixNow(t)
	got := encodeEvents(pivot.Request{}, true, textEvents()...)
	want := roleChunk + wantChunk(`{"content":"Hel"}`) + wantChunk(`{"content":"lo"}`) + wantStop("stop", usage60) + done
	checkOut(t, "stream", got, want)
}

func TestReplyEncoder_IncludeUsage(t *testing.T) {
	fixNow(t)
	on := pivot.Request{Extra: pivot.Extra{"chat-completions.include_usage": json.RawMessage("true")}}
	got := encodeEvents(on, true, textEvents()...)
	want := roleChunk + wantChunk(`{"content":"Hel"}`) + wantChunk(`{"content":"lo"}`) +
		wantStop("stop", "") + wantUsageChunk(usage60) + done
	checkOut(t, "include_usage true", got, want)

	off := pivot.Request{Extra: pivot.Extra{"chat-completions.include_usage": json.RawMessage("false")}}
	got = encodeEvents(off, true, textEvents()...)
	want = roleChunk + wantChunk(`{"content":"Hel"}`) + wantChunk(`{"content":"lo"}`) + wantStop("stop", usage60) + done
	checkOut(t, "include_usage false", got, want)
}

func TestReplyEncoder_ThinkingSignature(t *testing.T) {
	fixNow(t)
	got := encodeEvents(pivot.Request{}, true,
		start(), open(0, pivot.BlockThinking),
		delta(0, pivot.DeltaThinking, "a"), delta(0, pivot.DeltaThinking, "b"),
		delta(0, pivot.DeltaSignature, "SIG"),
		closeBlock(0),
		open(1, pivot.BlockThinking), delta(1, pivot.DeltaThinking, "c"), closeBlock(1),
		stop("end_turn"))
	want := roleChunk + wantChunk(`{"reasoning_content":"a"}`) + wantChunk(`{"reasoning_content":"b"}`) +
		wantChunk(`{"thinking_blocks":[{"type":"thinking","thinking":"ab","signature":"SIG"}]}`) +
		wantChunk(`{"reasoning_content":"c"}`) + wantStop("stop", zeroUsage) + done
	checkOut(t, "stream", got, want)
}

func TestReplyEncoder_Redacted(t *testing.T) {
	fixNow(t)
	got := encodeEvents(pivot.Request{}, true,
		start(), pivot.Event{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockRedactedThinking, Data: "RD"},
		closeBlock(0), stop("end_turn"))
	want := roleChunk + wantChunk(`{"thinking_blocks":[{"type":"redacted_thinking","data":"RD"}]}`) + wantStop("stop", zeroUsage) + done
	checkOut(t, "stream", got, want)
}

func TestReplyEncoder_TwoTools(t *testing.T) {
	fixNow(t)
	got := encodeEvents(pivot.Request{}, true,
		start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "x"), closeBlock(0),
		openTool(1, "call_a", "Read"), delta(1, pivot.DeltaInputJSON, `{"p":`), delta(1, pivot.DeltaInputJSON, `1}`), closeBlock(1),
		openTool(2, "call_b", "Bash"), delta(2, pivot.DeltaInputJSON, `{}`), closeBlock(2),
		stop("tool_use"))
	want := roleChunk + wantChunk(`{"content":"x"}`) +
		wantChunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Read","arguments":""}}]}`) +
		wantChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]}`) +
		wantChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}`) +
		wantChunk(`{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"Bash","arguments":""}}]}`) +
		wantChunk(`{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}`) +
		wantStop("tool_calls", zeroUsage) + done
	checkOut(t, "stream", got, want)
}

func TestReplyEncoder_EmptyToolInput(t *testing.T) {
	fixNow(t)
	evs := []pivot.Event{start(), openTool(0, "call_a", "Now"), closeBlock(0), stop("tool_use")}
	got := encodeEvents(pivot.Request{}, true, evs...)
	want := roleChunk +
		wantChunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Now","arguments":""}}]}`) +
		wantChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`) +
		wantStop("tool_calls", zeroUsage) + done
	checkOut(t, "stream", got, want)

	got = encodeEvents(pivot.Request{}, false, evs...)
	want = `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"claude","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_a","type":"function","function":{"name":"Now","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":` + zeroUsage + `}`
	checkOut(t, "not streamed", got, want)
}

func TestReplyEncoder_FinishReasons(t *testing.T) {
	fixNow(t)
	for reason, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length",
		"tool_use": "tool_calls", "refusal": "content_filter", "pause_turn": "stop", "other": "stop",
	} {
		got := encodeEvents(pivot.Request{}, true, start(), stop(reason))
		checkOut(t, reason, got, roleChunk+wantStop(want, zeroUsage)+done)
	}
}

func TestReplyEncoder_StreamError(t *testing.T) {
	fixNow(t)
	enc := Protocol{}.NewReplyEncoder(pivot.Request{}, true)
	var b strings.Builder
	for _, ev := range []pivot.Event{start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "x"),
		apiError(529, "overloaded_error", "m")} {
		b.Write(enc.Encode(ev))
	}
	want := roleChunk + wantChunk(`{"content":"x"}`) + `data: {"error":{"message":"m","type":"overloaded_error","code":null}}` + "\n\n"
	checkOut(t, "stream", b.String(), want)
	for _, ev := range []pivot.Event{delta(0, pivot.DeltaText, "y"), closeBlock(0), stop("end_turn")} {
		if out := enc.Encode(ev); out != nil {
			t.Errorf("an event after the error gave %q", out)
		}
	}
	if out := enc.End(); out != nil {
		t.Errorf("End after the error gave %q", out)
	}
}

func TestReplyEncoder_EndsEarly(t *testing.T) {
	fixNow(t)
	evs := []pivot.Event{start(), open(0, pivot.BlockText), delta(0, pivot.DeltaText, "x")}
	got := encodeEvents(pivot.Request{}, true, evs...)
	want := roleChunk + wantChunk(`{"content":"x"}`) + "data: " + string(Protocol{}.EncodeError(502, "api_error", msgEndedEarly)) + "\n\n"
	checkOut(t, "stream", got, want)

	got = encodeEvents(pivot.Request{}, false, evs...)
	checkOut(t, "not streamed", got, string(Protocol{}.EncodeError(502, "api_error", msgEndedEarly)))
}

func TestReplyEncoder_Whole(t *testing.T) {
	fixNow(t)
	enc := Protocol{}.NewReplyEncoder(pivot.Request{}, false)
	for _, ev := range []pivot.Event{
		start(),
		open(0, pivot.BlockThinking), delta(0, pivot.DeltaThinking, "th"), delta(0, pivot.DeltaSignature, "SIG"), closeBlock(0),
		pivot.Event{Kind: pivot.EventBlockStart, Index: 1, Block: pivot.BlockRedactedThinking, Data: "RD"}, closeBlock(1),
		open(2, pivot.BlockThinking), delta(2, pivot.DeltaThinking, "un"), closeBlock(2),
		open(3, pivot.BlockText), delta(3, pivot.DeltaText, "Hel"), delta(3, pivot.DeltaText, "lo"), closeBlock(3),
		open(4, pivot.BlockText), delta(4, pivot.DeltaText, "!"), closeBlock(4),
		openTool(5, "call_a", "Read"), delta(5, pivot.DeltaInputJSON, `{"p":`), delta(5, pivot.DeltaInputJSON, `1}`), closeBlock(5),
		openTool(6, "call_b", "Bash"), delta(6, pivot.DeltaInputJSON, `{}`), closeBlock(6),
		usage(pivot.Usage{Input: 10, Output: 5, CacheRead: 20, CacheCreation: 30}),
		stop("tool_use"),
	} {
		if out := enc.Encode(ev); out != nil {
			t.Fatalf("Encode gave %q for a not-streamed reply", out)
		}
	}
	want := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"claude","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"Hello!","reasoning_content":"thun",` +
		`"thinking_blocks":[{"type":"thinking","thinking":"th","signature":"SIG"},{"type":"redacted_thinking","data":"RD"}],` +
		`"tool_calls":[{"id":"call_a","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}},` +
		`{"id":"call_b","type":"function","function":{"name":"Bash","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":` + usage60 + `}`
	checkOut(t, "whole", string(enc.End()), want)

	got := encodeEvents(pivot.Request{}, false, start(), apiError(429, "rate_limit_error", "slow down"))
	checkOut(t, "error", got, string(Protocol{}.EncodeError(429, "rate_limit_error", "slow down")))
}

func TestReplyEncoder_ModelFromRequest(t *testing.T) {
	fixNow(t)
	got := encodeEvents(pivot.Request{Model: "m"}, true, pivot.Event{Kind: pivot.EventStart, ID: "chatcmpl-1"}, stop("end_turn"))
	want := strings.ReplaceAll(roleChunk+wantStop("stop", zeroUsage), `"model":"claude"`, `"model":"m"`) + done
	checkOut(t, "stream", got, want)
}

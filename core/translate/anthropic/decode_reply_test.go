package anthropic

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

const replyFixtures = "../testdata/responses-anthropic/reply"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(replyFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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

func checkDecode(t *testing.T, status int, stream bool, body string, want []pivot.Event) {
	t.Helper()
	if got := decodeAll(status, stream, body); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n got %+v\nwant %+v", got, want)
	}
	if got := decodeBytewise(status, stream, body); !reflect.DeepEqual(got, want) {
		t.Errorf("events, fed one byte at a time:\n got %+v\nwant %+v", got, want)
	}
}

func startEv(id string) pivot.Event {
	return pivot.Event{Kind: pivot.EventStart, ID: id, Model: "claude-opus-5-5"}
}
func openEv(i int, k pivot.BlockKind) pivot.Event {
	return pivot.Event{Kind: pivot.EventBlockStart, Index: i, Block: k}
}
func toolEv(i int, id, name string) pivot.Event {
	return pivot.Event{Kind: pivot.EventBlockStart, Index: i, Block: pivot.BlockToolUse, ToolID: id, ToolName: name}
}
func deltaEv(i int, k pivot.DeltaKind, v string) pivot.Event {
	return pivot.Event{Kind: pivot.EventDelta, Index: i, Delta: k, Value: v}
}
func closeEv(i int) pivot.Event { return pivot.Event{Kind: pivot.EventBlockStop, Index: i} }
func usageEv(in, out, read, create int) pivot.Event {
	return pivot.Event{Kind: pivot.EventUsage, Usage: pivot.Usage{Input: in, Output: out, CacheRead: read, CacheCreation: create}}
}
func stopEv(r string) pivot.Event { return pivot.Event{Kind: pivot.EventStop, StopReason: r} }
func errorEv(status int, typ, msg string) pivot.Event {
	return pivot.Event{Kind: pivot.EventError, Status: status, ErrorType: typ, Message: msg}
}

var twoToolsEvents = []pivot.Event{
	startEv("msg_01Tools"),
	openEv(0, pivot.BlockText),
	deltaEv(0, pivot.DeltaText, "I will read two files."),
	closeEv(0),
	toolEv(1, "toolu_01A", "Read"),
	deltaEv(1, pivot.DeltaInputJSON, `{"file_path":`),
	deltaEv(1, pivot.DeltaInputJSON, `"/a.txt"}`),
	closeEv(1),
	toolEv(2, "toolu_01B", "Read"),
	deltaEv(2, pivot.DeltaInputJSON, `{"file_path":"/b.txt"}`),
	closeEv(2),
	usageEv(7, 61, 2000, 100),
	stopEv("tool_use"),
}

func TestDecodeReply_Stream(t *testing.T) {
	body := fixture(t, "two_tools.sse")
	checkDecode(t, 200, true, body, twoToolsEvents)
	// Decision 11: \r\n line ends give the same events.
	checkDecode(t, 200, true, strings.ReplaceAll(body, "\n", "\r\n"), twoToolsEvents)
}

func TestDecodeReply_ThinkingSignature(t *testing.T) {
	checkDecode(t, 200, true, fixture(t, "thinking_signature.sse"), []pivot.Event{
		startEv("msg_01Think"),
		openEv(0, pivot.BlockThinking),
		deltaEv(0, pivot.DeltaSignature, "EqQBCkYIBRgCKkBx+/Sig=="),
		closeEv(0),
		openEv(1, pivot.BlockText),
		deltaEv(1, pivot.DeltaText, "391 is 17 times 23."),
		closeEv(1),
		usageEv(20, 90, 0, 0),
		stopEv("end_turn"),
	})
}

func TestDecodeReply_Redacted(t *testing.T) {
	checkDecode(t, 200, true, fixture(t, "redacted_thinking.sse"), []pivot.Event{
		startEv("msg_01Redact"),
		{Kind: pivot.EventBlockStart, Index: 0, Block: pivot.BlockRedactedThinking, Data: "EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cr3qpP"},
		closeEv(0),
		openEv(1, pivot.BlockText),
		deltaEv(1, pivot.DeltaText, "Done."),
		closeEv(1),
		usageEv(30, 44, 10, 0),
		stopEv("end_turn"),
	})
}

// Review Focus 3, decision 6.
func TestDecodeReply_SkippedBlockUsesNoIndex(t *testing.T) {
	checkDecode(t, 200, true, fixture(t, "server_tool.sse"), []pivot.Event{
		startEv("msg_01Search"),
		openEv(0, pivot.BlockText),
		deltaEv(0, pivot.DeltaText, "Let me search."),
		closeEv(0),
		openEv(1, pivot.BlockText),
		deltaEv(1, pivot.DeltaText, "Found it."),
		closeEv(1),
		usageEv(50, 30, 0, 0),
		stopEv("end_turn"),
	})
}

// joinDeltas joins the consecutive deltas of one block and kind, so that
// a stream and a whole body can be compared.
func joinDeltas(evs []pivot.Event) []pivot.Event {
	var out []pivot.Event
	for _, ev := range evs {
		if n := len(out); n > 0 && ev.Kind == pivot.EventDelta {
			if last := &out[n-1]; last.Kind == pivot.EventDelta && last.Index == ev.Index && last.Delta == ev.Delta {
				last.Value += ev.Value
				continue
			}
		}
		out = append(out, ev)
	}
	return out
}

func TestDecodeReply_Whole(t *testing.T) {
	sses, _ := filepath.Glob(filepath.Join(replyFixtures, "*.sse"))
	n := 0
	for _, s := range sses {
		name := strings.TrimSuffix(filepath.Base(s), ".sse")
		if _, err := os.Stat(filepath.Join(replyFixtures, name+".json")); err != nil {
			continue
		}
		n++
		t.Run(name, func(t *testing.T) {
			want := joinDeltas(decodeAll(200, true, fixture(t, name+".sse")))
			got := decodeAll(200, false, fixture(t, name+".json"))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("events:\n got %+v\nwant %+v", got, want)
			}
		})
	}
	if n < 7 {
		t.Errorf("found %d .sse and .json pairs, want 7", n)
	}
}

func TestDecodeReply_EveryByteBoundary(t *testing.T) {
	sses, _ := filepath.Glob(filepath.Join(replyFixtures, "*.sse"))
	if len(sses) < 8 {
		t.Fatalf("found %d .sse fixtures, want 8", len(sses))
	}
	for _, s := range sses {
		name := filepath.Base(s)
		t.Run(name, func(t *testing.T) {
			body := fixture(t, name)
			want := decodeAll(200, true, body)
			for i := 0; i <= len(body); i++ {
				d := Protocol{}.NewReplyDecoder(200, true)
				got := append(d.Feed([]byte(body[:i])), d.Feed([]byte(body[i:]))...)
				got = append(got, d.End()...)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("split at %d:\n got %+v\nwant %+v", i, got, want)
				}
			}
			if got := decodeBytewise(200, true, body); !reflect.DeepEqual(got, want) {
				t.Errorf("fed one byte at a time:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestDecodeReply_StreamError(t *testing.T) {
	checkDecode(t, 200, true, fixture(t, "stream_error.sse"), []pivot.Event{
		startEv("msg_01Err"),
		openEv(0, pivot.BlockText),
		deltaEv(0, pivot.DeltaText, "Partial"),
		errorEv(529, "overloaded_error", "Overloaded"),
	})
	// Nothing after the error.
	body := fixture(t, "stream_error.sse") + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	if evs := decodeAll(200, true, body); evs[len(evs)-1].Kind != pivot.EventError {
		t.Errorf("events after the error: %+v", evs)
	}
}

// Decision 9.
func TestDecodeReply_StreamErrorStatus(t *testing.T) {
	for typ, status := range map[string]int{
		"invalid_request_error": 400, "authentication_error": 401, "permission_error": 403,
		"not_found_error": 404, "request_too_large": 413, "rate_limit_error": 429,
		"overloaded_error": 529, "api_error": 500, "new_error": 500,
	} {
		body := `data: {"type":"error","error":{"type":"` + typ + `","message":"m"}}` + "\n\n"
		checkDecode(t, 200, true, body, []pivot.Event{errorEv(status, typ, "m")})
	}
}

func TestDecodeReply_HTTPError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		checkDecode(t, 429, stream, fixture(t, "error_429_rate_limit.json"), []pivot.Event{
			errorEv(429, "rate_limit_error", "This request would exceed the rate limit for your organization of 50 requests per minute."),
		})
		checkDecode(t, 529, stream, fixture(t, "error_529_overloaded.json"), []pivot.Event{
			errorEv(529, "overloaded_error", "Overloaded"),
		})
		checkDecode(t, 404, stream, `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"}}`, []pivot.Event{
			errorEv(404, "not_found_error", "model: claude-nope"),
		})
	}
}

// Review Focus 4.
func TestDecodeReply_NonJSONErrorCut(t *testing.T) {
	body := "<html>" + strings.Repeat("x", 193) + "€" + strings.Repeat("y", 98)
	if len(body) != 300 || body[199:202] != "€" {
		t.Fatalf("bad test body: %d bytes", len(body))
	}
	checkDecode(t, 502, false, body, []pivot.Event{errorEv(502, "api_error", body[:199])})
	checkDecode(t, 502, false, "", []pivot.Event{errorEv(502, "api_error", "Bad Gateway")})
	// JSON that is not Anthropic's error shape is a body like any other.
	checkDecode(t, 500, false, `{"error":"boom"}`, []pivot.Event{errorEv(500, "api_error", `{"error":"boom"}`)})
}

// Review Focus 1, decision 8.
func TestDecodeReply_StreamEndsEarly(t *testing.T) {
	body := fixture(t, "two_tools.sse")
	body = body[:strings.Index(body, "event: message_stop")]
	evs := decodeAll(200, true, body)
	want := errorEv(502, "api_error", "the inference server's stream ended before the reply was complete")
	if len(evs) == 0 || !reflect.DeepEqual(evs[len(evs)-1], want) {
		t.Errorf("events = %+v, want the last to be %+v", evs, want)
	}
	for _, ev := range evs {
		if ev.Kind == pivot.EventStop || ev.Kind == pivot.EventUsage {
			t.Errorf("events = %+v, want no usage or stop", evs)
		}
	}
	checkDecode(t, 200, true, "", []pivot.Event{want})
}

// Decision 10.
func TestDecodeReply_OverLimit(t *testing.T) {
	want := []pivot.Event{errorEv(502, "api_error", "the inference server's reply is over the 10485760-byte buffer limit")}
	// Valid JSON, so only the limit makes it an error.
	body := fixture(t, "text.json") + strings.Repeat(" ", maxReplyBuffer)
	if got := decodeAll(200, false, body); !reflect.DeepEqual(got, want) {
		t.Errorf("not streamed: events = %+v", got)
	}
	if got := decodeAll(200, true, "data: "+strings.Repeat("x", maxReplyBuffer)); !reflect.DeepEqual(got, want) {
		t.Errorf("stream line: events = %+v", got)
	}
}

func TestDecodeReply_StopReasons(t *testing.T) {
	for name, want := range map[string]string{"max_tokens": "max_tokens", "refusal": "refusal"} {
		evs := decodeAll(200, true, fixture(t, name+".sse"))
		if got := evs[len(evs)-1]; !reflect.DeepEqual(got, stopEv(want)) {
			t.Errorf("%s: last event = %+v, want stop %s", name, got, want)
		}
	}
}

// Decision 12 and 13: a start block with text, and a message_delta usage
// that replaces only the members it has.
func TestDecodeReply_StartTextAndUsage(t *testing.T) {
	body := `data: {"type":"message_start","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":4,"cache_read_input_tokens":2,"output_tokens":1}}}
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Hi"}}
data: {"type":"content_block_stop","index":0}
data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"hmm","signature":""}}
data: {"type":"content_block_stop","index":1}
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":6,"output_tokens":9}}
data: {"type":"message_stop"}
`
	checkDecode(t, 200, true, body, []pivot.Event{
		startEv("m"),
		openEv(0, pivot.BlockText),
		deltaEv(0, pivot.DeltaText, "Hi"),
		closeEv(0),
		openEv(1, pivot.BlockThinking),
		deltaEv(1, pivot.DeltaThinking, "hmm"),
		closeEv(1),
		usageEv(6, 9, 2, 0),
		stopEv("end_turn"),
	})
}

func TestDecodeReply_BadJSON(t *testing.T) {
	checkDecode(t, 200, true, "data: {nope\n\n", []pivot.Event{
		errorEv(502, "api_error", "the inference server sent a stream event that is not valid JSON"),
	})
	checkDecode(t, 200, false, "{nope", []pivot.Event{
		errorEv(502, "api_error", "the inference server sent a reply that is not valid JSON"),
	})
}

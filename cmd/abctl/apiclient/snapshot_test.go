package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/rossoctl/cortex/core/pipeline"
)

// decodeSessionView walks the object by hand, so it can drift from what a plain Decode
// would have produced — the same hazard writeSessionView has on the server, answered the
// same way: compare the two directly on every shape this can see.
func TestDecodeSessionView_MatchesTheBufferedDecode(t *testing.T) {
	at := time.Date(2026, 9, 17, 10, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		doc  string
	}{
		{"empty object", `{}`},
		{"null events", `{"id":"s1","events":null}`},
		{"empty events", `{"id":"s1","events":[]}`},
		{"one event", fmt.Sprintf(
			`{"id":"s1","events":[{"seq":1,"at":%q,"direction":"outbound","phase":"request","host":"h"}]}`,
			at.Format(time.RFC3339Nano))},
		{"truncated view", fmt.Sprintf(
			`{"id":"s1","events":[{"seq":9,"at":%q,"direction":"inbound","phase":"response","statusCode":200}],`+
				`"totalEvents":42,"oldestSeq":7}`,
			at.Format(time.RFC3339Nano))},
		{"inference extension", fmt.Sprintf(
			`{"id":"s1","events":[{"seq":1,"at":%q,"direction":"outbound","phase":"request",`+
				`"inference":{"model":"m","messages":[{"role":"user","content":"hi"}],`+
				`"tools":[{"name":"t","description":"d","parameters":{"type":"object"}}]}}]}`,
			at.Format(time.RFC3339Nano))},
		// A field this abctl does not know must be skipped, not fatal: a newer proxy is a
		// normal thing to point an older abctl at.
		{"unknown field", fmt.Sprintf(
			`{"id":"s1","somethingNew":{"a":[1,2,{"b":null}]},"events":[{"seq":1,"at":%q,`+
				`"direction":"outbound","phase":"request"}],"alsoNew":7}`,
			at.Format(time.RFC3339Nano))},
		// Key order is the server's business, not ours; the walk must not depend on it.
		{"events first", fmt.Sprintf(
			`{"events":[{"seq":3,"at":%q,"direction":"outbound","phase":"request"}],"oldestSeq":2,"id":"s1"}`,
			at.Format(time.RFC3339Nano))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want pipeline.SessionView
			if err := json.Unmarshal([]byte(tc.doc), &want); err != nil {
				t.Fatalf("buffered decode of the fixture failed: %v", err)
			}
			got, err := decodeSessionView(strings.NewReader(tc.doc))
			if err != nil {
				t.Fatalf("decodeSessionView: %v", err)
			}
			if !reflect.DeepEqual(*got, want) {
				// %#v, not %+v: the difference that actually shows up here is a nil
				// slice against an empty one, and %+v prints both as "[]".
				t.Errorf("streamed decode differs from buffered\n got: %#v\nwant: %#v", *got, want)
			}
		})
	}
}

// Malformed input has to come back as an error rather than a partial view, because the
// caller replaces its timeline with what it gets.
func TestDecodeSessionView_RejectsMalformedInput(t *testing.T) {
	for _, doc := range []string{
		``,
		`[]`,                             // an array, not the expected object
		`{"events":42}`,                  // events is neither array nor null
		`{"events":[{"at":"nonsense"}]}`, // an event that cannot decode
		`{"id":"s1","events":[{"seq":1}`, // truncated mid-document
		`{"totalEvents":"not-a-number"}`,
	} {
		if _, err := decodeSessionView(strings.NewReader(doc)); err == nil {
			t.Errorf("decodeSessionView(%q) = nil error, want failure", doc)
		}
	}
}

// The point of decoding through an Interner: consecutive events re-send the same messages,
// and without this each event keeps its own copy of every one of them.
//
// Asserted on the string's data POINTER, not on equality. Equal strings prove nothing here
// — the whole question is whether the two events reference one allocation or two.
func TestDecodeSessionView_SharesRepeatedMessageContent(t *testing.T) {
	shared := strings.Repeat("the conversation so far ", 8) // past internMinLen (64)
	event := func(seq int, extra string) string {
		msgs := fmt.Sprintf(`{"role":"user","content":%q}`, shared)
		if extra != "" {
			msgs += fmt.Sprintf(`,{"role":"user","content":%q}`, extra)
		}
		return fmt.Sprintf(`{"seq":%d,"at":"2026-09-17T10:30:00Z","direction":"outbound",`+
			`"phase":"request","inference":{"model":"m","messages":[%s]}}`, seq, msgs)
	}
	doc := fmt.Sprintf(`{"id":"s1","events":[%s,%s]}`,
		event(1, ""), event(2, strings.Repeat("and one more turn ", 8)))

	v, err := decodeSessionView(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("decodeSessionView: %v", err)
	}
	if len(v.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(v.Events))
	}

	first := v.Events[0].Inference.Messages[0].Content
	second := v.Events[1].Inference.Messages[0].Content
	if first != second {
		t.Fatalf("fixture broken: contents differ\n%q\n%q", first, second)
	}
	if dataPtr(first) != dataPtr(second) {
		t.Error("repeated message content was decoded into two allocations; the Interner did not run")
	}
}

// dataPtr is the address of a string's bytes, which is what tells a shared string from an
// equal one.
func dataPtr(s string) uintptr {
	return uintptr(unsafe.Pointer(unsafe.StringData(s)))
}

// --- heap benchmark ---

// The fixture's shape, scaled down from the live session BenchmarkSnapshotHeap is built
// around (226 events x ~948 messages) so this runs in a couple of seconds.
const (
	benchDecodeEvents      = 120
	benchDecodeMsgPerEvent = 400
	benchDecodeMsgBytes    = 476
)

// benchDecodeDoc builds the JSON one snapshot response carries: every event re-sends the
// whole conversation, which is what the wire actually looks like and what makes the
// document large relative to the distinct content in it.
func benchDecodeDoc(b *testing.B) []byte {
	b.Helper()
	body := strings.Repeat("prompt text ", benchDecodeMsgBytes/12)
	events := make([]pipeline.SessionEvent, 0, benchDecodeEvents)
	for e := 0; e < benchDecodeEvents; e++ {
		msgs := make([]pipeline.InferenceMessage, benchDecodeMsgPerEvent)
		for i := range msgs {
			msgs[i] = pipeline.InferenceMessage{
				Role:    "user",
				Content: fmt.Sprintf("message %06d: %s", i, body),
			}
		}
		events = append(events, pipeline.SessionEvent{
			Seq:       uint64(e + 1),
			Phase:     pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{Model: "m", Messages: msgs},
		})
	}
	doc, err := json.Marshal(&pipeline.SessionView{ID: "s1", Events: events})
	if err != nil {
		b.Fatal(err)
	}
	return doc
}

// BenchmarkDecodeSessionViewHeap reports what receiving one snapshot costs abctl, buffered
// against streamed-and-interned. It is the client-side counterpart of
// sessionapi.BenchmarkSnapshotHeap, and the number this decode path exists to move: abctl
// was measured at 1.64GB against the proxy's 1.03GB, and was the only one of the two still
// growing.
//
// Two metrics, because the buffered path was expensive in two different ways:
//
//   - MB-HeapSys is the peak the allocator had to reach, which Go keeps as idle heap
//     rather than returning promptly, so it is what RSS reflects. Decoding a document
//     whole has to hold all of it at once.
//   - MB-retained is what is still live with the view in hand, which is what the interner
//     moves: the encoder on the server expanded every shared message back into its own
//     bytes, and without interning the decode keeps every one of those copies.
//
// RUN THE TWO SEPARATELY, for the reason BenchmarkSnapshotHeap gives: HeapSys does not
// shrink within a process, so whichever runs second reports a flattering figure against a
// heap the first already grew.
//
//	go test ./apiclient/ -bench 'DecodeSessionViewHeap/buffered' -run '^$' -benchtime 1x
//	go test ./apiclient/ -bench 'DecodeSessionViewHeap/streamed' -run '^$' -benchtime 1x
//
// Reported rather than asserted, like both benchmarks it mirrors. The deterministic
// properties are pinned by the tests above.
//
// Measured on an M-series laptop, each in its own process, on a 23.5MB document:
//
//	buffered   +32.0 MB HeapSys   26.49 MB retained
//	streamed    +0.0 MB HeapSys    2.31 MB retained
//
// Both columns matter and they are not the same win. HeapSys is the document no longer
// being held whole; retained is the interner, 11.5x less kept for the same events.
func BenchmarkDecodeSessionViewHeap(b *testing.B) {
	b.Run("buffered", func(b *testing.B) {
		benchDecode(b, func(doc []byte) (*pipeline.SessionView, error) {
			var v pipeline.SessionView
			err := json.NewDecoder(bytes.NewReader(doc)).Decode(&v)
			return &v, err
		})
	})
	b.Run("streamed", func(b *testing.B) {
		benchDecode(b, func(doc []byte) (*pipeline.SessionView, error) {
			return decodeSessionView(bytes.NewReader(doc))
		})
	})
}

// heapSysTaken records that a HeapSys figure has already been measured in this process.
//
// Splitting the sub-benchmarks into two top-level functions would NOT fix the ordering trap
// — one `-bench DecodeSessionViewHeap` still matches both and runs them in one process — so
// the second measurement refuses to report instead. Without this, running both together
// gives the streamed case a flattering ≈+0.0 MB against a heap the buffered case already
// grew, and +0.0 MB is exactly the headline figure.
var heapSysTaken bool

func benchDecode(b *testing.B, decode func([]byte) (*pipeline.SessionView, error)) {
	if heapSysTaken {
		b.Skip("HeapSys does not shrink within a process, so this would measure a heap the " +
			"previous sub-benchmark already grew. Run this one on its own: " +
			"go test ./apiclient/ -bench 'DecodeSessionViewHeap/<buffered|streamed>' -run '^$' -benchtime 1x")
	}
	heapSysTaken = true

	doc := benchDecodeDoc(b)

	// The document is allocated before the baseline so it cancels out of both figures;
	// what is measured is the decode. Two GCs, as BenchmarkSnapshotHeap does, because the
	// first can leave finalizer-reachable memory the second collects.
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ResetTimer()

	var held *pipeline.SessionView
	for i := 0; i < b.N; i++ {
		v, err := decode(doc)
		if err != nil {
			b.Fatal(err)
		}
		held = v
	}

	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	b.ReportMetric(float64(int64(after.HeapSys)-int64(before.HeapSys))/1048576, "MB-HeapSys")
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/1048576, "MB-retained")
	b.ReportMetric(float64(len(doc))/1048576, "MB-document")
	// The view must still be live when HeapAlloc is read — that is the retained figure.
	runtime.KeepAlive(held)
	runtime.KeepAlive(doc)
}

package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// realBodiesEnv names a directory of real request bodies, one per file, for the tests and the
// benchmark below. Real bodies carry a user's system prompt and files, so they are never in tree:
// the committed fixture is synthBodies, and this is how a change is checked against live traffic.
const realBodiesEnv = "CORTEX_ARCHIVE_REAL_BODIES"

func realBodies(tb testing.TB) [][]byte {
	tb.Helper()
	dir := os.Getenv(realBodiesEnv)
	if dir == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		tb.Fatal(err)
	}
	var out [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		tb.Fatalf("%s=%s holds no files", realBodiesEnv, dir)
	}
	return out
}

// synthBodies builds the requests of one Anthropic Messages conversation, shaped like what
// Claude Code sends: a system prompt in blocks, a tool manifest, a conversation that grows on
// every request with text, thinking with a signature, tool calls, tool results and an image,
// and a cache_control marker that moves to the newest message each turn. The text is from a
// seeded vocabulary, as synthSession's is.
func synthBodies(seed int64, turns, promptBytes int) [][]byte {
	r := rand.New(rand.NewSource(seed))
	vocab := make([]string, 2000)
	for i := range vocab {
		b := make([]byte, 3+r.Intn(8))
		for j := range b {
			b[j] = byte('a' + r.Intn(26))
		}
		vocab[i] = string(b)
	}
	word := func() string { return vocab[r.Intn(len(vocab))] }
	text := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(word())
			b.WriteByte(' ')
		}
		return b.String()
	}
	q := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	system := fmt.Sprintf(`[{"type":"text","text":%s},{"type":"text","text":%s,"cache_control":{"type":"ephemeral"}}]`,
		q("You are a coding agent. "+text(64)), q(text(promptBytes/2)))
	var tools []string
	for i := range 8 {
		tools = append(tools, fmt.Sprintf(`{"name":"tool_%d","description":%s,"input_schema":{"type":"object","properties":{"arg":{"type":"string","description":%s}}}}`,
			i, q(text(promptBytes/32)), q(text(promptBytes/64))))
	}
	var msgs []string
	var out [][]byte
	for i := range turns {
		msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%s}]}`, q(text(80+r.Intn(400))+" é ✓ <tag> & \"quoted\" \\ back")))
		marked := append([]string(nil), msgs...)
		last := marked[len(marked)-1]
		marked[len(marked)-1] = last[:len(last)-3] + `,"cache_control":{"type":"ephemeral"}}]}`
		out = append(out, fmt.Appendf(nil,
			`{"model":"claude-opus-4-6","max_tokens":32000,"thinking":{"type":"adaptive"},"system":%s,"tools":[%s],"messages":[%s],"stream":true}`,
			system, strings.Join(tools, ","), strings.Join(marked, ",")))
		id := fmt.Sprintf("toolu_%02d", i)
		msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":[{"type":"thinking","thinking":%s,"signature":%s},{"type":"tool_use","id":%q,"name":"tool_1","input":{"arg":%s}}]}`,
			q(text(200+r.Intn(600))), q(text(300)), id, q(text(40))))
		result := fmt.Sprintf(`{"type":"tool_result","tool_use_id":%q,"content":%s}`, id, q(text(60+r.Intn(900))))
		if i%4 == 3 {
			result = fmt.Sprintf(`{"type":"tool_result","tool_use_id":%q,"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}`,
				id, strings.Repeat("iVBORw0KGgo", 400+r.Intn(400)))
		}
		msgs = append(msgs, `{"role":"user","content":[`+result+`]}`)
	}
	return out
}

// edgeBodies are the bodies synthBodies does not produce: empty, not JSON, escapes at the edge
// of a cut, an unterminated string, pretty-printed JSON, and bytes that are not valid UTF-8.
func edgeBodies() [][]byte {
	long := strings.Repeat("x", 80)
	return [][]byte{
		nil,
		{},
		[]byte("not json at all, but long enough that a careless splitter might try something with it"),
		[]byte(`{"a":"` + long + `\\","b":"` + long + `\"still inside\"","c":"` + long + `é"}`),
		[]byte(`{"open":"` + long + `... and no closing quote`),
		[]byte("{\n  \"messages\": [\n    {\n      \"content\": \"" + long + "\"\n    }\n  ]\n}\n"),
		[]byte(`{"bad":"` + long + "\xff\xfe" + `"}`),
		[]byte(`{"nul":"` + long + "\x00" + `"}`),
	}
}

func captureOf(i int, body []byte) *pipeline.RequestCapture {
	c := &pipeline.RequestCapture{
		Original: body,
		Header:   http.Header{"Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"a", "b"}},
		Query:    "beta=true",
	}
	if i%3 == 1 && len(body) > 0 {
		// What a rewriting plugin forwards: most of the original, one block changed.
		c.Forwarded = bytes.Replace(body, []byte(`"stream":true`), []byte(`"stream":false`), 1)
	}
	return c
}

// writeCaptures writes one request event per body, each with its capture, into a fresh closed
// segment, and returns the segment's path and the captures in seq order.
func writeCaptures(t *testing.T, bodies [][]byte) (string, []*pipeline.RequestCapture) {
	t.Helper()
	w, err := createSegment(filepath.Join(t.TempDir(), "sess"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var caps []*pipeline.RequestCapture
	for i, b := range bodies {
		c := captureOf(i, b)
		e := pipeline.SessionEvent{Seq: uint64(i + 1), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest}
		if err := w.appendCapture(e, c, time.Now()); err != nil {
			t.Fatal(err)
		}
		caps = append(caps, c)
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(w.f.Name()), w.info().File), caps
}

// THE POINT OF LOSSLESS CAPTURE: what a segment hands back is the request byte for byte — the
// body the agent sent, the body Cortex forwarded, its headers and its query — or nothing.
// Re-encoded JSON would not do: the prompt cache and thinking signatures are sensitive to bytes.
func TestCapture_RoundTripsByteIdentical(t *testing.T) {
	bodies := append(synthBodies(1, 12, 64<<10), edgeBodies()...)
	bodies = append(bodies, realBodies(t)...)
	path, caps := writeCaptures(t, bodies)
	for i, want := range caps {
		got, err := readCapture(path, uint64(i+1))
		if err != nil {
			t.Fatalf("seq %d: %v", i+1, err)
		}
		if got == nil {
			t.Fatalf("seq %d: no capture read back", i+1)
		}
		if !bytes.Equal(got.Original, want.Original) {
			t.Fatalf("seq %d: original differs (%d bytes back, want %d)", i+1, len(got.Original), len(want.Original))
		}
		if !bytes.Equal(got.Forwarded, want.Forwarded) || (got.Forwarded == nil) != (want.Forwarded == nil) {
			t.Fatalf("seq %d: forwarded differs (%d bytes back, want %d)", i+1, len(got.Forwarded), len(want.Forwarded))
		}
		if !reflect.DeepEqual(got.Header, want.Header) || got.Query != want.Query {
			t.Fatalf("seq %d: header %v query %q, want %v %q", i+1, got.Header, got.Query, want.Header, want.Query)
		}
	}
}

// Every request re-sends the conversation, so a block must be written once per segment, or
// lossless capture costs what storing each body whole does.
func TestCapture_WritesEachBlockOncePerSegment(t *testing.T) {
	body := synthBodies(2, 1, 8<<10)[0]
	path, _ := writeCaptures(t, [][]byte{body, body, body})
	blocks := map[string]int{}
	for _, rec := range rawLines(t, path) {
		if rec.K == kindBlock {
			blocks[rec.H]++
		}
	}
	if len(blocks) == 0 {
		t.Fatal("no block records: the body was not split")
	}
	for h, n := range blocks {
		if n != 1 {
			t.Fatalf("block %s written %d times, want once", h, n)
		}
	}
}

// A reader that predates captures — and every reader of events here — must see the same events
// whether or not they carry one.
func TestCapture_EventsReadTheSameWithIt(t *testing.T) {
	path, _ := writeCaptures(t, synthBodies(3, 4, 8<<10))
	got, truncated := readAll(t, path)
	if truncated || len(got) != 4 {
		t.Fatalf("read %d events (truncated=%v), want 4", len(got), truncated)
	}
	for i, e := range got {
		if e.Seq != uint64(i+1) || e.Phase != pipeline.SessionRequest {
			t.Fatalf("event %d is %+v", i, e)
		}
	}
}

// An event written without a capture — every event before this format, and every one that is
// not a request — has none, and says so without an error.
func TestCapture_AbsentIsNotAnError(t *testing.T) {
	path, _ := writeSegment(t, synthSession(4, 2, 2048), true)
	got, err := readCapture(path, 1)
	if got != nil || err != nil {
		t.Fatalf("capture %v, err %v, want none and no error", got, err)
	}
}

// A rebuild that does not hash to what was captured is corruption, and must say so rather than
// hand back bytes that are not the request.
func TestCapture_RebuildRefusesWhatDoesNotMatch(t *testing.T) {
	body := synthBodies(5, 2, 8<<10)[1]
	seen := map[string]struct{}{}
	sb, fresh := splitBody(body, seen)
	blocks := map[string]string{}
	for _, s := range fresh {
		blocks[s.h] = s.v
	}
	if got, err := sb.rebuild(blocks); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("clean rebuild: err %v, equal %v", err, bytes.Equal(got, body))
	}
	for h := range blocks {
		blocks[h] += "!"
		break
	}
	if _, err := sb.rebuild(blocks); err == nil {
		t.Fatal("rebuild accepted a changed block")
	}
	if _, err := sb.rebuild(map[string]string{}); err == nil {
		t.Fatal("rebuild accepted missing blocks")
	}
}

// THE DEDUP MUST KEEP A RE-SENT CONVERSATION ONCE, for captures as for events: a split that
// stops sharing blocks still round-trips perfectly and only shows as size. The prompt is past
// the 1 MiB zstd window on purpose, as in TestSegment_SizeStaysFarUnderNaive: below it, zstd
// finds the repeats on its own and hides a lost dedup.
func TestCapture_SizeStaysFarUnderRaw(t *testing.T) {
	bodies := synthBodies(6, 10, 1536<<10)
	var raw int64
	for _, b := range bodies {
		raw += int64(len(b))
	}
	path, _ := writeCaptures(t, bodies)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if ratio := float64(raw) / float64(fi.Size()); ratio < 20 {
		t.Fatalf("segment is %d bytes against %d raw: %.1fx, want at least 20x", fi.Size(), raw, ratio)
	}
}

// BenchmarkCapture_BytesPerRequest is the disk cost of a captured request. Set
// CORTEX_ARCHIVE_REAL_BODIES to measure a real session instead of the synthetic one.
func BenchmarkCapture_BytesPerRequest(b *testing.B) {
	bodies := realBodies(b)
	if bodies == nil {
		bodies = synthBodies(7, 40, 800<<10)
	}
	var raw int64
	for _, x := range bodies {
		raw += int64(len(x))
	}
	var onDisk int64
	for b.Loop() {
		w, err := createSegment(filepath.Join(b.TempDir(), "sess"), time.Now())
		if err != nil {
			b.Fatal(err)
		}
		for i, x := range bodies {
			e := pipeline.SessionEvent{Seq: uint64(i + 1), Phase: pipeline.SessionRequest}
			if err := w.appendCapture(e, &pipeline.RequestCapture{Original: x}, time.Now()); err != nil {
				b.Fatal(err)
			}
		}
		if err := w.close(); err != nil {
			b.Fatal(err)
		}
		onDisk = w.info().Bytes
	}
	b.ReportMetric(float64(onDisk)/float64(len(bodies)), "B/request")
	b.ReportMetric(float64(raw)/float64(len(bodies)), "raw-B/request")
	b.ReportMetric(float64(raw)/float64(onDisk), "x-smaller")
}

// BenchmarkCapture_Split is the writer goroutine's CPU per request: the split and the hashes.
func BenchmarkCapture_Split(b *testing.B) {
	bodies := realBodies(b)
	if bodies == nil {
		bodies = synthBodies(8, 40, 800<<10)
	}
	body := bodies[len(bodies)-1]
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		splitBody(body, map[string]struct{}{})
	}
}

// Archive.Capture finds the segment that holds the event and rebuilds its request, and reports
// no capture for an event that has none.
func TestArchive_CaptureReadsBackWhatTheWriterWrote(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	body := synthBodies(9, 3, 8<<10)[2]
	want := captureOf(1, body)
	a.inWriter(func() {
		a.write("s", &pipeline.SessionEvent{Seq: 1, SessionID: "s", Phase: pipeline.SessionRequest}, want)
		a.write("s", &pipeline.SessionEvent{Seq: 2, SessionID: "s", Phase: pipeline.SessionResponse}, nil)
		a.tick()
	})
	got, ok, err := a.Capture("s", 1)
	if err != nil || !ok {
		t.Fatalf("Capture(s, 1): ok %v, err %v", ok, err)
	}
	if !bytes.Equal(got.Original, want.Original) || !bytes.Equal(got.Forwarded, want.Forwarded) {
		t.Fatal("Capture(s, 1) differs from what was written")
	}
	if _, ok, err := a.Capture("s", 2); ok || err != nil {
		t.Fatalf("Capture(s, 2): ok %v, err %v, want no capture", ok, err)
	}
	if _, ok, err := a.Capture("other", 1); ok || err != nil {
		t.Fatalf("Capture(other, 1): ok %v, err %v, want no capture", ok, err)
	}
}

// A segment written by this version reads in a reader that knows only strings and events: the
// block records are skipped and every event comes back. Pinned on the raw lines, which is all an
// older reader has.
func TestCapture_OldReaderSkipsBlocks(t *testing.T) {
	path, _ := writeCaptures(t, synthBodies(10, 3, 8<<10))
	events, blocks := 0, 0
	for _, rec := range rawLines(t, path) {
		switch rec.K {
		case kindEvent:
			events++
		case kindString:
		case kindBlock:
			blocks++
		default:
			t.Fatalf("unexpected record kind %q", rec.K)
		}
	}
	if events != 3 || blocks == 0 {
		t.Fatalf("%d events, %d blocks, want 3 and some", events, blocks)
	}
}

// A block that is not valid UTF-8 cannot be a JSON string, and must still come back exact.
func TestCapture_InvalidUTF8BlockIsExact(t *testing.T) {
	body := []byte(strings.Repeat("\xff\xfe binary-ish ", 10))
	path, _ := writeCaptures(t, [][]byte{body})
	got, err := readCapture(path, 1)
	if err != nil || got == nil || !bytes.Equal(got.Original, body) {
		t.Fatalf("err %v, got %+v", err, got)
	}
}

// A body nested deeper than any validator's recursion can take must not crash the writer: it
// is one block, still exact.
func TestCapture_DeepNestingIsOneBlock(t *testing.T) {
	body := []byte(`{"m":` + strings.Repeat("[", 8<<20) + strings.Repeat("]", 8<<20) + `}`)
	sb, fresh := splitBody(body, map[string]struct{}{})
	if len(fresh) != 1 || len(sb.P) != 3 {
		t.Fatalf("%d blocks, %d pieces, want the whole body as one block", len(fresh), len(sb.P))
	}
}

// Headers and the query are not hashed, so they must come back exact by construction, also
// when they hold bytes that are not valid UTF-8 — which Go's server accepts in a header value.
func TestCapture_HeadersAndQueryAreExact(t *testing.T) {
	c := &pipeline.RequestCapture{Original: []byte("{}"),
		Header: http.Header{"X-Name": {"caf\xe9", "ok"}}, Query: "q=\xff&r=1"}
	w, err := createSegment(filepath.Join(t.TempDir(), "sess"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.appendCapture(pipeline.SessionEvent{Seq: 1}, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.close()
	got, err := readCapture(filepath.Join(filepath.Dir(w.f.Name()), w.info().File), 1)
	if err != nil || got == nil {
		t.Fatalf("got %v, err %v", got, err)
	}
	if !reflect.DeepEqual(got.Header, c.Header) || got.Query != c.Query {
		t.Fatalf("header %q query %q, want %q %q", got.Header, got.Query, c.Header, c.Query)
	}
}

// An empty forwarded body is a rewrite to nothing, not "no rewrite".
func TestCapture_EmptyForwardedStaysEmpty(t *testing.T) {
	c := &pipeline.RequestCapture{Original: []byte(`{"a":1}`), Forwarded: []byte{}}
	w, err := createSegment(filepath.Join(t.TempDir(), "sess"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.appendCapture(pipeline.SessionEvent{Seq: 1}, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.close()
	got, err := readCapture(filepath.Join(filepath.Dir(w.f.Name()), w.info().File), 1)
	if err != nil || got == nil || got.Forwarded == nil || len(got.Forwarded) != 0 {
		t.Fatalf("got %+v, err %v, want an empty, non-nil forwarded body", got, err)
	}
}

// An event read does not decode block records: Page, Event and the startup usage replay read
// segments that are mostly blocks, and decoding them cost 37% more time on a 40-request
// segment. Pinned with a damaged block line, which an event read never looks inside, so it is
// not a truncation there.
func TestCapture_EventReadsSkipBlocks(t *testing.T) {
	path := zstdFile(t, 1<<20,
		`{"k":"b",this block line is damaged}`,
		`{"k":"e","e":{"seq":1,"at":"2026-10-01T00:00:00Z","direction":"outbound","phase":"request"}}`)
	got, truncated := readAll(t, path)
	if len(got) != 1 || truncated {
		t.Fatalf("read %d events (truncated=%v), want 1, not truncated", len(got), truncated)
	}
}

// A block's undoing on a failed encode: if the event cannot be written, its blocks are not
// recorded as written either, and the next event that uses them writes them.
func TestCapture_FailedEventForgetsItsBlocks(t *testing.T) {
	body := synthBodies(13, 1, 8<<10)[0]
	w, err := createSegment(filepath.Join(t.TempDir(), "sess"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bad := pipeline.SessionEvent{Seq: 1, Plugins: map[string]json.RawMessage{"p": json.RawMessage("{not json")}}
	if err := w.appendCapture(bad, &pipeline.RequestCapture{Original: body}, time.Now()); err == nil {
		t.Fatal("an unencodable event was written")
	}
	if err := w.appendCapture(pipeline.SessionEvent{Seq: 2}, &pipeline.RequestCapture{Original: body}, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.close()
	got, err := readCapture(filepath.Join(filepath.Dir(w.f.Name()), w.info().File), 2)
	if err != nil || got == nil || !bytes.Equal(got.Original, body) {
		t.Fatalf("got %v, err %v: the second event's blocks were never written", got != nil, err)
	}
}

// A segment torn after a block and before its event holds no capture for that event: no
// error, and nothing rebuilt from half a request.
func TestCapture_TornBeforeTheEventIsNoCapture(t *testing.T) {
	path := zstdFile(t, 1<<20,
		`{"k":"e","e":{"seq":1,"at":"2026-10-01T00:00:00Z","direction":"outbound","phase":"request"}}`,
		`{"k":"b","h":"AAAAAAAAAAAAAAAAAAAAAA","v":"a block whose event never made it to the disk"}`)
	if got, err := readCapture(path, 2); got != nil || err != nil {
		t.Fatalf("got %v, err %v, want no capture and no error", got, err)
	}
}

// A block written after the event is not one the event can use: blocks precede their first
// use, so a reader that took it would be reading a later request's data.
func TestCapture_ABlockAfterItsEventIsNotUsed(t *testing.T) {
	v := strings.Repeat("a block long enough to be cut out of the body ", 2)
	h := hashOf(v)
	body := `{"x":"` + v + `"}`
	sum := sha256.Sum256([]byte(body))
	f := fmt.Sprintf(`{"o":{"s":%q,"p":["{\"x\":",%q,"}"]}}`, hex.EncodeToString(sum[:]), h)
	path := zstdFile(t, 1<<20,
		`{"k":"e","e":{"seq":1,"at":"2026-10-01T00:00:00Z","direction":"outbound","phase":"request"},"f":`+f+`}`,
		fmt.Sprintf(`{"k":"b","h":%q,"v":%q}`, h, `"`+v+`"`))
	if _, err := readCapture(path, 1); err == nil {
		t.Fatal("a block after its event was used to rebuild it")
	}
}

// The archive takes captures from the store: a captured request read back through Capture is
// the request that was appended.
func TestArchive_RecordCaptureReachesDisk(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock())
	defer a.Close()
	want := captureOf(1, synthBodies(14, 2, 8<<10)[1])
	a.RecordCapture("s", &pipeline.SessionEvent{Seq: 1, SessionID: "s", Phase: pipeline.SessionRequest}, want)
	tick(a)
	got, ok, err := a.Capture("s", 1)
	if err != nil || !ok || !bytes.Equal(got.Original, want.Original) || !bytes.Equal(got.Forwarded, want.Forwarded) {
		t.Fatalf("Capture: ok %v err %v", ok, err)
	}
}

// Captures waiting for the writer are bounded in bytes, not only in count: 4,096 queued
// requests of 2 MB would pin 8 GB. Past the budget the capture is dropped and counted — never
// the event, which still reaches disk without it — and never blocks the request.
func TestArchive_CaptureQueueHasAByteBudget(t *testing.T) {
	a := openTest(t, t.TempDir(), newClock(), WithCaptureBudget(10<<10))
	defer a.Close()
	gate := make(chan struct{})
	a.gate = gate
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	defer open() // before Close, which waits for the writer
	body := bytes.Repeat([]byte("x"), 4<<10)
	for i := 1; i <= 5; i++ {
		a.RecordCapture("s", &pipeline.SessionEvent{Seq: uint64(i), SessionID: "s", Phase: pipeline.SessionRequest},
			&pipeline.RequestCapture{Original: body, Forwarded: body[:1<<10]})
	}
	if got := a.Stats().DroppedCaptures; got != 3 {
		t.Fatalf("DroppedCaptures = %d, want 3: two 5 KB captures fit a 10 KB budget", got)
	}
	open()
	tick(a) // flushes, so the open segment reads back
	for seq, want := range map[uint64]bool{1: true, 2: true, 3: false, 5: false} {
		if _, ok, err := a.Capture("s", seq); ok != want || err != nil {
			t.Errorf("Capture(s, %d): ok %v err %v, want ok %v", seq, ok, err, want)
		}
		if _, ok, _ := a.Event("s", seq); !ok {
			t.Errorf("event %d was dropped with its capture", seq)
		}
	}
	// The budget is given back as the writer takes captures: a later one fits again.
	a.RecordCapture("s", &pipeline.SessionEvent{Seq: 6, SessionID: "s", Phase: pipeline.SessionRequest},
		&pipeline.RequestCapture{Original: body})
	tick(a)
	if _, ok, _ := a.Capture("s", 6); !ok {
		t.Error("a capture after the queue drained was dropped")
	}
}

// An element with a content array is cut into its content blocks AND its other large fields:
// a Responses reasoning item's encrypted_content and a Chat Completions message's tool_calls
// are re-sent on every request as much as its content is.
func TestCapture_ElementFieldsBesideContentAreBlocks(t *testing.T) {
	enc, args := strings.Repeat("Z", 4096), strings.Repeat("a", 2048)
	for name, body := range map[string][]byte{
		"responses reasoning": []byte(`{"input":[{"type":"reasoning","encrypted_content":"` + enc +
			`","content":[{"type":"reasoning_text","text":"` + strings.Repeat("r", 100) + `"}]}]}`),
		"chat tool_calls": []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"` +
			strings.Repeat("t", 100) + `"}],"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"` + args + `"}}]}]}`),
	} {
		sb, fresh := splitBody(body, map[string]struct{}{})
		literal := 0
		for i := 0; i < len(sb.P); i += 2 {
			literal += len(sb.P[i])
		}
		if literal > 200 {
			t.Errorf("%s: %d of %d bytes stay literal in the skeleton, want under 200", name, literal, len(body))
		}
		blocks := map[string]string{}
		for _, s := range fresh {
			blocks[s.h] = s.v
		}
		if got, err := sb.rebuild(blocks); err != nil || !bytes.Equal(got, body) {
			t.Errorf("%s: rebuild err %v, equal %v", name, err, bytes.Equal(got, body))
		}
	}
}

// The byte path keeps every header key, as the string path does: one with an empty value list
// and one with a nil list come back as they went in.
func TestCapture_ByteHeadersKeepEmptyKeys(t *testing.T) {
	c := &pipeline.RequestCapture{Original: []byte("{}"),
		Header: http.Header{"X-Empty": {}, "X-Nil": nil, "X-Name": {"caf\xe9"}}}
	w, err := createSegment(filepath.Join(t.TempDir(), "sess"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.appendCapture(pipeline.SessionEvent{Seq: 1}, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.close()
	got, err := readCapture(filepath.Join(filepath.Dir(w.f.Name()), w.info().File), 1)
	if err != nil || got == nil {
		t.Fatalf("got %v, err %v", got, err)
	}
	if !reflect.DeepEqual(got.Header, c.Header) {
		t.Fatalf("header %#v, want %#v", got.Header, c.Header)
	}
}

// A response is captured whole, as one record with no block split: it is not re-sent from one
// request to the next. Status, headers, encoding and the two flags come back as written.
func TestCapture_ResponseRoundTrips(t *testing.T) {
	body := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n" + strings.Repeat("data: x\n\n", 50) + "\xff")
	c := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{
		Status: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"},
			"X-Odd": {"caf\xe9"}, "X-Empty": {}, "X-Nil": nil},
		Body: body, Truncated: true, Incomplete: true, Decoded: true,
	}}
	w, err := createSegment(filepath.Join(t.TempDir(), "sess"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.appendCapture(pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionResponse}, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.close()
	path := filepath.Join(filepath.Dir(w.f.Name()), w.info().File)
	var blocks int
	for _, rec := range rawLines(t, path) {
		if rec.K == kindBlock {
			blocks++
		}
	}
	if blocks != 1 {
		t.Fatalf("%d block records, want the response body whole, as 1", blocks)
	}
	got, err := readCapture(path, 1)
	if err != nil || got == nil || got.Response == nil {
		t.Fatalf("got %+v, err %v", got, err)
	}
	r := got.Response
	if r.Status != 200 || !bytes.Equal(r.Body, body) || !r.Truncated || !r.Incomplete || !r.Decoded ||
		!reflect.DeepEqual(r.Header, c.Response.Header) || got.Original != nil {
		t.Fatalf("response %+v, want what was written", r)
	}
}

// An event read skips response bodies unread, as it skips blocks: Page, Event and the startup
// usage replay read segments that hold them. Measured before this rule: 40 events with 270 KB
// responses took 60 ms a read against 0.1 ms without them.
func TestCapture_EventReadsSkipResponseBodies(t *testing.T) {
	c := &pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200, Body: []byte("a body")}}
	w, err := createSegment(filepath.Join(t.TempDir(), "sess"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.appendCapture(pipeline.SessionEvent{Seq: 1, Phase: pipeline.SessionResponse}, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.close()
	path := filepath.Join(filepath.Dir(w.f.Name()), w.info().File)
	var bodies int
	for _, rec := range rawLines(t, path) {
		if rec.K == kindBlock {
			bodies++
		}
		if rec.K == kindEvent && bytes.Contains(rec.F, []byte(base64.StdEncoding.EncodeToString(c.Response.Body))) {
			t.Fatal("the response body is on the event line")
		}
	}
	if bodies != 1 {
		t.Fatalf("%d block records, want the body as 1", bodies)
	}
	got, err := readCapture(path, 1)
	if err != nil || got == nil || string(got.Response.Body) != "a body" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

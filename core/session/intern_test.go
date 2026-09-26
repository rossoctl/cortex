package session

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"github.com/rossoctl/cortex/core/pipeline"
)

// backing returns the address of a string's bytes.
//
// Pointer identity is the only honest assertion here. A RSS or allocation measurement is
// what the change is FOR, but it is noisy enough to pass while the code does nothing;
// two strings sharing a backing array is exactly the claim, and it is deterministic.
func backing(s string) uintptr {
	return uintptr(unsafe.Pointer(unsafe.StringData(s)))
}

// convo builds a turn's worth of messages: the whole conversation so far, as an LLM
// request actually carries it, with one new message on the end.
func convo(turns int) []pipeline.InferenceMessage {
	out := make([]pipeline.InferenceMessage, 0, turns)
	for i := 0; i < turns; i++ {
		out = append(out, pipeline.InferenceMessage{
			Role: "user",
			// Distinct per index, long enough to be worth interning, and rebuilt from
			// scratch on every call — so a test that sees sharing is seeing the store
			// share it, not the fixture handing out one string twice.
			Content: fmt.Sprintf("message %04d: %s", i, strings.Repeat("prompt text ", 8)),
		})
	}
	return out
}

// The claim: appending N turns of a growing conversation keeps ONE copy of each message,
// not one per turn.
func TestAppend_SharesRepeatedMessageContent(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	const turns = 25
	for i := 1; i <= turns; i++ {
		s.Append("s1", pipeline.SessionEvent{
			Phase:     pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{Model: "m", Messages: convo(i)},
		})
	}

	v := s.View("s1")
	if len(v.Events) != turns {
		t.Fatalf("stored %d events, want %d", len(v.Events), turns)
	}

	// Message 0 appears in all 25 events. Every occurrence must be the same bytes.
	first := backing(v.Events[0].Inference.Messages[0].Content)
	shared := 0
	for i := range v.Events {
		got := backing(v.Events[i].Inference.Messages[0].Content)
		if got == first {
			shared++
			continue
		}
		t.Errorf("event %d holds its own copy of message 0", i)
	}
	if shared != turns {
		t.Errorf("message 0 shared by %d of %d events", shared, turns)
	}

	// And the content still reads correctly — sharing must not have crossed any wires.
	want := convo(1)[0].Content
	if got := v.Events[turns-1].Inference.Messages[0].Content; got != want {
		t.Errorf("message 0 content changed: %q", trunc(got))
	}
}

// An event with nothing to intern must not break the chain between the turns around it.
//
// This is the live traffic shape, not a contrived one: every bridged HTTPS request records
// a CONNECT tunnel-open, and it lands between the inference request and its response — 165
// of 500 events on a real session. Rolling an empty table forward on those cleared it
// before the next turn could match, which defeated interning almost entirely: the same
// benchmark fixture measures 31.33MB/session without InternEvent's guard against 2.79MB
// with it.
func TestAppend_ContentlessEventDoesNotBreakSharing(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	const turns = 25
	for i := 1; i <= turns; i++ {
		// A tunnel-open before each turn: no Inference, no A2A, nothing to intern.
		s.Append("s1", pipeline.SessionEvent{Tunnel: true, Host: "gateway:443"})
		s.Append("s1", pipeline.SessionEvent{
			Phase:     pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{Model: "m", Messages: convo(i)},
		})
	}

	v := s.View("s1")
	if len(v.Events) != turns*2 {
		t.Fatalf("stored %d events, want %d", len(v.Events), turns*2)
	}

	// Message 0 appears in every inference event, and all of them must be the same bytes
	// despite a contentless event sitting between each pair.
	var first uintptr
	inference := 0
	for i := range v.Events {
		inf := v.Events[i].Inference
		if inf == nil {
			continue
		}
		inference++
		got := backing(inf.Messages[0].Content)
		if first == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("event %d holds its own copy of message 0; a contentless event cleared the table", i)
		}
	}
	if inference != turns {
		t.Fatalf("found %d inference events, want %d", inference, turns)
	}
}

// An event that HAS an extension but interns nothing must not break the chain either.
//
// The other door into the same bug. The contentless-event guard tests for a missing
// extension, but "no extension" and "nothing interned" are different things: an inbound A2A
// intent whose parts are all shorter than internMinLen — "continue", "yes", "do it", the
// most ordinary messages in an agent conversation — has a non-nil extension, produces an
// empty table, and rolling that forward would clear the conversation just as a tunnel-open
// did. What the code keys on is therefore whether anything was interned.
func TestAppend_ShortContentEventDoesNotBreakSharing(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	const turns = 25
	for i := 1; i <= turns; i++ {
		// A short user intent between the turns. Well under internMinLen (64), so it
		// interns nothing at all, and it carries a REAL extension.
		s.Append("s1", pipeline.SessionEvent{
			Direction: pipeline.Inbound,
			Phase:     pipeline.SessionRequest,
			A2A: &pipeline.A2AExtension{
				Method: "message/send",
				Parts:  []pipeline.A2APart{{Kind: "text", Content: "continue"}},
			},
		})
		s.Append("s1", pipeline.SessionEvent{
			Phase:     pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{Model: "m", Messages: convo(i)},
		})
	}

	v := s.View("s1")
	var first uintptr
	inference := 0
	for i := range v.Events {
		inf := v.Events[i].Inference
		if inf == nil {
			continue
		}
		inference++
		got := backing(inf.Messages[0].Content)
		if first == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("event %d holds its own copy of message 0; a short-content event cleared the table", i)
		}
	}
	if inference != turns {
		t.Fatalf("found %d inference events, want %d", inference, turns)
	}
}

// Distinct content is left distinct: interning must not collapse two different messages.
func TestAppend_DoesNotMergeDifferentContent(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	msgs := convo(3)
	s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Messages: msgs}})

	v := s.View("s1")
	got := v.Events[0].Inference.Messages
	for i := range got {
		if got[i].Content != msgs[i].Content {
			t.Errorf("message %d = %q, want %q", i, trunc(got[i].Content), trunc(msgs[i].Content))
		}
		for j := range got {
			if i != j && backing(got[i].Content) == backing(got[j].Content) {
				t.Errorf("messages %d and %d were collapsed into one string", i, j)
			}
		}
	}
}

// Two sessions must not share a table. The content is identical here, and it still has
// to be two copies: one session's eviction cannot be allowed to free another's messages,
// and a table shared across conversations would outlive whichever ended first.
func TestAppend_DoesNotShareAcrossSessions(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	s.Append("s1", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Messages: convo(2)}})
	s.Append("s2", pipeline.SessionEvent{Inference: &pipeline.InferenceExtension{Messages: convo(2)}})

	a := backing(s.View("s1").Events[0].Inference.Messages[0].Content)
	b := backing(s.View("s2").Events[0].Inference.Messages[0].Content)
	if a == b {
		t.Error("two sessions share one copy of the same message")
	}
}

// Short strings skip the table: a role or a finish reason costs less to duplicate than
// to hash, and the savings are in bodies.
func TestIntern_SkipsShortStrings(t *testing.T) {
	var in Interner
	next := map[string]string{}
	short := strings.Repeat("x", internMinLen-1)
	if got := in.intern(short, next); backing(got) != backing(short) {
		t.Error("a short string was interned")
	}
	if len(next) != 0 {
		t.Errorf("table grew by %d for a short string", len(next))
	}

	long := strings.Repeat("y", internMinLen)
	in.intern(long, next)
	if len(next) != 1 {
		t.Errorf("table holds %d entries after one long string, want 1", len(next))
	}
}

// A2A part content interns too — the other place a conversation repeats itself.
func TestAppend_SharesA2APartContent(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	// Built fresh inside the loop, not hoisted into a variable. A single string handed
	// to four events is shared by every event whether or not the store interns
	// anything, which is a test that cannot fail — the first version of this one did
	// exactly that and passed with interning disabled.
	body := func() string { return strings.Repeat("intent text ", 16) }
	for i := 0; i < 4; i++ {
		s.Append("s1", pipeline.SessionEvent{
			Direction: pipeline.Inbound, Phase: pipeline.SessionRequest,
			A2A: &pipeline.A2AExtension{Parts: []pipeline.A2APart{{Content: body()}}},
		})
	}

	v := s.View("s1")
	first := backing(v.Events[0].A2A.Parts[0].Content)
	for i := range v.Events {
		if backing(v.Events[i].A2A.Parts[0].Content) != first {
			t.Errorf("event %d holds its own copy of the A2A part", i)
		}
	}
}

// The table holds the LAST event's strings — one event's worth, not an accumulation
// across events.
//
// Named for that rather than for "does not grow with the session", which the first
// version of this claimed and the assertion contradicts: on this workload one event
// carries the whole conversation, so 30 turns means 30 entries and the table does grow
// with it. What the rolling table buys is that every key is a string some event still
// references, so it pins nothing of its own and never needs pruning when events are
// evicted.
func TestIntern_TableHoldsTheLastEventsStrings(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	for i := 1; i <= 30; i++ {
		s.Append("s1", pipeline.SessionEvent{
			Inference: &pipeline.InferenceExtension{Messages: convo(i)},
		})
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got, want := len(s.sessions["s1"].intern.prev), 30; got != want {
		t.Errorf("table holds %d entries after 30 turns, want %d (the last event's messages)", got, want)
	}
}

func trunc(s string) string {
	if len(s) <= 48 {
		return s
	}
	return s[:48] + "…"
}

// The store must not mutate what it was handed, and this is the sequence that catches it.
//
// The obvious version of this test cannot fail. Append the request- and response-phase
// events of one request — which alias one backing array — and the canonical string IS the
// pointer already in that array: the first Append misses the table and mints the array's
// own string as canonical, the second hits and gets the identical pointer back. In-place
// interning writes nothing but what was already there.
//
// The failure needs the canonical pointer to have MOVED between the two passes over the
// same array. Four appends do that:
//
//  1. the request-phase event, which mints canon from array A's own strings;
//  2. an unrelated event, which rolls the table past them;
//  3. an event whose content EQUALS A's but is a separate allocation, which mints a fresh
//     canonical pointer into array B;
//  4. the response-phase event, which aliases array A again and now resolves to B's
//     pointer — a genuinely different one, written over memory the store published in
//     step 1.
//
// That is the race the clone exists to prevent, in a form a test can see.
func TestAppend_DoesNotMutateTheCallersSlice(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	live := &pipeline.InferenceExtension{Model: "m", Messages: convo(3), Tools: manifest()} // array A
	before := make([]uintptr, len(live.Messages))
	for i := range live.Messages {
		before[i] = backing(live.Messages[i].Content)
	}
	beforeTools := make([]uintptr, len(live.Tools))
	for i := range live.Tools {
		beforeTools[i] = backing(live.Tools[i].Description)
	}

	reqEv := pipeline.SessionEvent{Phase: pipeline.SessionRequest, Inference: pipeline.SnapshotInference(live)}
	s.Append("s1", reqEv)

	// Roll the table past A's strings.
	s.Append("s1", pipeline.SessionEvent{
		Inference: &pipeline.InferenceExtension{Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: strings.Repeat("unrelated filler ", 8)},
		}},
	})

	// Equal content, separate allocation: convo and manifest rebuild their strings every
	// call, so this mints canonical pointers that are NOT the ones in array A.
	s.Append("s1", pipeline.SessionEvent{
		Inference: &pipeline.InferenceExtension{Messages: convo(3), Tools: manifest()}, // array B
	})

	// The response-phase event of the first request: aliases array A again.
	respEv := pipeline.SessionEvent{Phase: pipeline.SessionResponse, Inference: pipeline.SnapshotInference(live)}
	s.Append("s1", respEv)

	for i := range live.Messages {
		if backing(live.Messages[i].Content) != before[i] {
			t.Errorf("message %d of the caller's live extension was rewritten by the store", i)
		}
	}
	for i := range reqEv.Inference.Messages {
		if backing(reqEv.Inference.Messages[i].Content) != before[i] {
			t.Errorf("message %d of the caller's already-published snapshot was rewritten", i)
		}
	}
	// The tool manifest gets the same treatment, and needs it for the same reason: both
	// phases of one request alias the array it lives in.
	for i := range live.Tools {
		if backing(live.Tools[i].Description) != beforeTools[i] {
			t.Errorf("tool %d of the caller's live extension was rewritten by the store", i)
		}
	}
	for i := range reqEv.Inference.Tools {
		if backing(reqEv.Inference.Tools[i].Description) != beforeTools[i] {
			t.Errorf("tool %d of the caller's already-published snapshot was rewritten", i)
		}
	}
}

// manifest is the tool list a client re-sends on every request, rebuilt on every call for
// the reason TestAppend_SharesA2APartContent gives: one string handed to four events is
// shared by all of them whether or not the store interns anything.
func manifest() []pipeline.InferenceTool {
	return []pipeline.InferenceTool{
		{
			Name:        "get_weather",
			Description: "Look up the forecast for a place. " + strings.Repeat("schema detail ", 8),
			// Past internMinLen (64), or intern returns it untouched and a test asserting
			// that schemas are shared would pass without any sharing having happened.
			Parameters: pipeline.RawJSON(`{"type":"object","properties":{"city":{"type":"string","description":"` +
				strings.Repeat("where to look ", 6) + `"}}}`),
		},
		{
			Name:        "send_email",
			Description: "Send a message to a recipient. " + strings.Repeat("schema detail ", 8),
			Parameters: pipeline.RawJSON(`{"type":"object","properties":{"to":{"type":"string","description":"` +
				strings.Repeat("who to send it to ", 6) + `"}}}`),
		},
	}
}

// The tool manifest interns too, and it is the field that duplicates hardest: a client
// re-sends the whole manifest on every request, so a live 272-event session held 10.3MB of
// tool JSON against 0.1MB distinct — 154x, against 6.5x for the conversation itself.
func TestAppend_SharesRepeatedToolDescriptions(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	const turns = 4
	for i := 0; i < turns; i++ {
		s.Append("s1", pipeline.SessionEvent{
			Inference: &pipeline.InferenceExtension{Model: "m", Tools: manifest()},
		})
	}

	v := s.View("s1")
	for tool := range manifest() {
		first := backing(v.Events[0].Inference.Tools[tool].Description)
		for i := range v.Events {
			if backing(v.Events[i].Inference.Tools[tool].Description) != first {
				t.Errorf("event %d holds its own copy of tool %d's description", i, tool)
			}
		}
	}

	// Two tools must not be collapsed into one string.
	if backing(v.Events[0].Inference.Tools[0].Description) ==
		backing(v.Events[0].Inference.Tools[1].Description) {
		t.Error("two tools' descriptions were collapsed into one string")
	}
	// And the text still reads correctly.
	if got, want := v.Events[turns-1].Inference.Tools[0].Description, manifest()[0].Description; got != want {
		t.Errorf("tool description changed: %q", trunc(got))
	}
}

// The SCHEMAS share too, which is the part the field's type change exists for and the
// largest term measured on a live session: 84KB per event as a map, 4.1x its JSON text.
//
// Asserted separately from the descriptions above because it is a different mechanism —
// interning a string field the parser used to widen into a map[string]any — and because it
// rested on a benchmark alone, which CI does not run.
func TestAppend_SharesRepeatedToolSchemas(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	const turns = 4
	for i := 0; i < turns; i++ {
		s.Append("s1", pipeline.SessionEvent{
			Inference: &pipeline.InferenceExtension{Model: "m", Tools: manifest()},
		})
	}

	v := s.View("s1")
	for tool := range manifest() {
		// The fixture has to be long enough to intern at all, or this passes vacuously.
		if got := len(v.Events[0].Inference.Tools[tool].Parameters); got < internMinLen {
			t.Fatalf("tool %d's schema is %d bytes, under internMinLen %d — it cannot be interned",
				tool, got, internMinLen)
		}
		first := backing(string(v.Events[0].Inference.Tools[tool].Parameters))
		for i := range v.Events {
			if backing(string(v.Events[i].Inference.Tools[tool].Parameters)) != first {
				t.Errorf("event %d holds its own copy of tool %d's schema", i, tool)
			}
		}
	}

	// Two different schemas must stay two strings.
	if backing(string(v.Events[0].Inference.Tools[0].Parameters)) ==
		backing(string(v.Events[0].Inference.Tools[1].Parameters)) {
		t.Error("two tools' schemas were collapsed into one string")
	}
	// And the JSON still reads correctly.
	if got, want := v.Events[turns-1].Inference.Tools[0].Parameters, manifest()[0].Parameters; got != want {
		t.Errorf("tool schema changed: %q", trunc(string(got)))
	}
}

// The event the store keeps must be its own, so that a second Append cannot reach the
// first event's memory through a shared array.
func TestAppend_StoresItsOwnExtension(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	live := &pipeline.InferenceExtension{Messages: convo(3), Tools: manifest()}
	ev := pipeline.SessionEvent{Inference: pipeline.SnapshotInference(live)}
	s.Append("s1", ev)

	stored := s.View("s1").Events[0].Inference
	if stored == ev.Inference {
		t.Error("the store kept the caller's extension pointer")
	}
	if len(stored.Messages) > 0 && len(ev.Inference.Messages) > 0 &&
		&stored.Messages[0] == &ev.Inference.Messages[0] {
		t.Error("the store kept the caller's message array")
	}
	if len(stored.Tools) > 0 && len(ev.Inference.Tools) > 0 &&
		&stored.Tools[0] == &ev.Inference.Tools[0] {
		t.Error("the store kept the caller's tool array")
	}
}

// The table is keyed on the canonical string, not on the duplicate that was looked up.
//
// Keying on the duplicate reads identically — equal strings hash equally — while pinning
// the copy the event just stopped referencing, so the table would hold a second full
// copy of the conversation and undo most of the saving. Asserting on POINTERS is what
// catches that; a heap measurement would absorb it as noise.
func TestIntern_TableKeysAreTheStringsTheEventsReference(t *testing.T) {
	s := New(0, 0, 100)
	defer s.Close()

	for turn := 1; turn <= 6; turn++ {
		s.Append("s1", pipeline.SessionEvent{
			Inference: &pipeline.InferenceExtension{Messages: convo(turn)},
		})
	}

	stored := map[uintptr]bool{}
	for _, e := range s.View("s1").Events {
		for i := range e.Inference.Messages {
			stored[backing(e.Inference.Messages[i].Content)] = true
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for key := range s.sessions["s1"].intern.prev {
		if !stored[backing(key)] {
			t.Errorf("table key %q is a copy no event references — it pins a duplicate", trunc(key))
		}
	}
}

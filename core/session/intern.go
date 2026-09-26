package session

import (
	"slices"

	"github.com/rossoctl/cortex/core/pipeline"
)

// Repeated message content is stored once per session, not once per event.
//
// WHY: an LLM request carries the whole conversation, so every turn re-sends every
// earlier message. The store keeps one event per request, each holding its own copy of
// that array, which makes retention quadratic in turns while the conversation itself
// grows linearly. Measured on a real 96-turn session: 27.7MB of message text held, 7.0MB
// distinct — 3.9x, and the factor rises with turn count because the duplicated prefix
// gets longer. The proxy reached 3.37GB resident on a laptop in 18 hours, against
// ~833MB for every Claude Code transcript on the same disk. The transcripts are the same
// conversations, appended once each; the difference was all duplication.
//
// The fix costs nothing at read time and changes nothing observable: Go strings are
// immutable, so two events sharing one backing array cannot tell, and a consumer sees
// the same bytes it always did.
//
// SCOPE: string fields only — inference messages and completions, A2A part content, and
// the tool manifest's descriptions AND schemas. The manifest earns its place: a client
// re-sends it on every request, so it duplicates harder than the conversation does.
// Measured on a live 272-event session, 10.3MB of tool JSON held against 0.1MB distinct —
// 154x, where the conversation itself was 6.5x. Interning the descriptions took that
// session's tool retention from 7.12MB to 0.37MB.
//
// The schemas (InferenceTool.Parameters) were the last big duplicate and are now interned
// too. They used to be map[string]any, which is what put them out of reach: a walk that
// rewrites map values cannot lean on string immutability the way this does, so it needs
// its own reasoning about aliasing. Rather than write that walk, the FIELD changed — it is
// a pipeline.RawJSON, a named string type, so it interns here like any other string and
// the aliasing question never arises. Two measurements motivated it: the schemas were
// 34.7% of that 10.3MB, and as maps they cost 4.1x their JSON text to hold — 84KB per
// event on a live session, ~172MB across one 2050-event session.
//
// What is still duplicated: MCP Params/Result, which remain map[string]any and would need
// the recursive walk. Left alone deliberately — they are unmeasured on this workload (zero
// MCP events across every live session inspected), so there is no evidence yet about what
// they cost, and the same field-type change is available to them if there ever is.
const (
	// internMinLen is the shortest string worth a map lookup.
	//
	// A role ("user"), a finish reason ("end_turn") and an empty completion all repeat
	// constantly and all cost less to duplicate than to hash. The savings live in
	// message bodies, which are orders of magnitude past this.
	internMinLen = 64
)

// Interner maps a string to the one copy the session keeps of it.
//
// It holds only the strings of the LAST event interned, which is enough to collapse the
// whole history: turn N's array is turn N-1's array plus a message or two, so interning N
// against N-1 makes them share; N-1 already shares with N-2, and so on back to the first
// turn that carried the string.
//
// That is one event's worth of keys, which on this workload is NOT small — an LLM request
// carries the whole conversation, so the last event's strings are roughly the whole
// distinct set. What the rolling table buys is not a small table but a table that costs
// nothing extra: every key is a string some event already references, so it pins no
// content of its own and needs no pruning in step with event eviction. A table
// accumulating across events would hold strings whose events had been evicted.
//
// The tradeoff is deliberate: content that disappears from the conversation and comes
// back later (a compaction that rewrites history, say) misses the table and gets a second
// copy. Best-effort dedup with a bounded table beats exact dedup with a table that
// outlives what it describes.
//
// Exported for readers outside this package, because the store is not the only place these
// events pile up: abctl decodes the same events off the session API and held more memory
// than the proxy it was watching. Feed events through one Interner in the order they were
// recorded — the rolling table depends on consecutive events being neighbours, so shuffled
// input still returns correct strings but shares almost nothing. The zero value is ready
// to use, and one Interner is for one goroutine.
type Interner struct {
	prev map[string]string
}

// intern returns the session's copy of s, recording s as canonical if it is new.
//
// The table is keyed on the CANONICAL string, never on the duplicate that was looked
// up. Keying on the duplicate would work identically for lookups — equal strings hash
// equally — while pinning the copy the event just stopped referencing, so the table
// would hold a copy of every message the last interned event carried.
//
// Bounded to one event's worth, because the table is rolled forward on every Append — so
// on BenchmarkRetainedHeap it is 2.358MB keyed on the duplicate against 2.205MB keyed on
// the canonical, a 0.15MB difference and not the multiplier it first looks like. Worth
// fixing because it is free, and worth measuring before claiming more than that.
func (in *Interner) intern(s string, next map[string]string) string {
	if len(s) < internMinLen {
		return s
	}
	canon, ok := in.prev[s]
	if !ok {
		canon = s
	}
	next[canon] = canon
	return canon
}

// InternEvent gives the event its own extension and message slice, with content
// pointing at the session's existing copies, then rolls the table forward.
//
// It CLONES rather than rewriting in place, and that is not defensive habit — writing in
// place is unsound here for three separate reasons:
//
//   - pipeline.SnapshotInference and SnapshotA2A are shallow copies, and their contract
//     says so: "Slice fields are reused intentionally — they are only assigned, never
//     mutated in place, after the parser completes." Interning in place breaks the
//     invariant the rest of the pipeline is written against.
//   - the request-phase and response-phase events of one request alias the same backing
//     array, because both snapshot the same live pctx.Extensions.Inference
//     (forwardproxy/server.go:377 and :880). So the second Append would rewrite an
//     array the first event already published.
//   - publishLocked hands the event — extension pointers included — to subscriber
//     channels, and the SSE goroutine encodes it outside the store's mutex. Mutating
//     those arrays is a straight data race against an in-flight encode.
//
// "Every replacement is a string equal to the one it replaced" does not rescue it
// either. With two requests interleaved in one session bucket, the second Append rolls
// prev forward before the first request's response-phase event is interned, so that
// pass writes a genuinely different pointer over memory the store has already handed
// out.
//
// Cloning costs one slice copy per event and nothing in steady state: the original array
// becomes garbage when the request completes, and the store then owns everything it
// mutates.
func (in *Interner) InternEvent(e *pipeline.SessionEvent) {
	// An event with nothing to intern must leave the table ALONE rather than roll an empty
	// one forward. Rolling is what makes turn N share with turn N-1, and a session's events
	// are not all turns: a CONNECT tunnel-open, a denial, an MCP call all carry no interned
	// content, and one of them landing between two inference events used to reset the table
	// and force the second to keep its own copy of the whole conversation.
	//
	// That interleaving is the normal case, not an edge case. Every bridged HTTPS request
	// records a tunnel-open, and it lands between the inference request and its response —
	// measured on a live session, 165 of 500 events. Skipping the roll took that window's
	// retention from 108.6MB to 89.2MB.
	//
	// This early return is only the cheap path — it skips the map allocation for the third
	// of events that carry no extension at all. The predicate that actually MATTERS is at
	// the bottom of this function: "produced nothing to intern", not "had no extension". An
	// inbound A2A intent whose parts are all shorter than internMinLen — "continue", "yes",
	// "do it" — has a non-nil extension, interns nothing, and would roll an empty table
	// forward exactly as a tunnel-open used to. Same bug, different door.
	//
	// Safe for the reason the roll was bounded in the first place: prev then holds the last
	// CONTENT event's strings, which that event still references, so the table pins nothing
	// of its own. The one-event bound is unchanged — it is the same table, just not cleared
	// by traffic that had nothing to say.
	if e.Inference == nil && e.A2A == nil {
		return
	}
	next := make(map[string]string, len(in.prev))

	if e.Inference != nil {
		cp := *e.Inference
		cp.Messages = slices.Clone(e.Inference.Messages)
		for i := range cp.Messages {
			cp.Messages[i].Content = in.intern(cp.Messages[i].Content, next)
		}
		cp.Completion = in.intern(cp.Completion, next)
		// Tools, like Messages, must be cloned before any field is rewritten: the same
		// array is aliased by this request's response-phase event.
		cp.Tools = slices.Clone(e.Inference.Tools)
		for i := range cp.Tools {
			cp.Tools[i].Description = in.intern(cp.Tools[i].Description, next)
			// Both conversions are free — pipeline.RawJSON is a string underneath — so
			// the interned schema is shared with the previous event rather than copied
			// into this one. That is the whole reason the field is a named string type
			// and not a json.RawMessage; see pipeline.RawJSON.
			cp.Tools[i].Parameters = pipeline.RawJSON(
				in.intern(string(cp.Tools[i].Parameters), next))
		}
		e.Inference = &cp
	}
	if e.A2A != nil {
		cp := *e.A2A
		cp.Parts = slices.Clone(e.A2A.Parts)
		for i := range cp.Parts {
			cp.Parts[i].Content = in.intern(cp.Parts[i].Content, next)
		}
		e.A2A = &cp
	}

	// Nothing was interned, so there is nothing to roll: keep the table the last event that
	// DID intern something left behind. Strictly stronger than the early return at the top,
	// which it subsumes — that one is an allocation shortcut, this one is the invariant.
	if len(next) == 0 {
		return
	}
	in.prev = next
}

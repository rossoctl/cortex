package session

import (
	"hash/fnv"
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
// SCOPE: string fields only — inference messages and completions, tool results (content
// by the threshold, ToolUseID below it via internID), A2A part content, and the tool
// manifest's descriptions AND schemas. Tool results repeat exactly as
// the conversation does: a client re-sends every earlier one on every request. The manifest
// earns its place: a client re-sends it on every request, so it duplicates harder than
// the conversation does.
// Measured on a live 272-event session, 10.3MB of tool JSON held against 0.1MB distinct —
// 154x, where the conversation itself was 6.5x. Interning the descriptions took that
// session's tool retention from 7.12MB to 0.37MB.
//
// The schemas (InferenceTool.Parameters) were the last big duplicate and are interned
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
	// InternMinLen is the shortest string worth a map lookup.
	//
	// A role ("user"), a finish reason ("end_turn") and an empty completion all repeat
	// constantly and all cost less to duplicate than to hash. The savings live in
	// message bodies, which are orders of magnitude past this.
	//
	// Exported for the session archive, which deduplicates the same six fields on disk
	// and must agree with the store on what is worth sharing — one threshold, not two
	// numbers kept equal by hand.
	InternMinLen = 64

	// maxConversations caps the per-session conversation states (#1541). Real
	// sessions measured 3 conversations on average and 80 at the extreme; 32
	// covers all but 4 of 708 live sessions, and an evicted conversation only
	// loses sharing — its next event interns fresh, exactly as a session does
	// against a cold interner.
	maxConversations = 32
)

// Interner maps a string to the one copy the session keeps of it, and one
// conversation's growing message arrays to the events that re-send them.
//
// CONVERSATIONS, NOT EVENTS. The table holds the strings of the last event of
// EACH conversation in the session, keyed by conversationKey (model + the
// first two messages — the system prompt, which differs between a subagent and
// its parent), LRU-capped at maxConversations. A single rolling table served
// single-conversation sessions: turn N's array is turn N-1's plus a message or
// two, so interning N against N-1 makes them share. But a session that runs
// subagents carries several conversations taking turns, and every switch threw
// the table away: one live session switched between its 14 conversations 1,307
// times in 2,436 requests, re-retaining a full copy of the conversation on
// every switch back (#1541 — 60.5x the distinct content on a two-conversation
// fixture, against 1.9x for one).
//
// PREFIX-SHARED ARRAYS. Beyond the strings, each conversation state holds the
// growing []InferenceMessage / []InferenceTool / []InferenceToolResult backing
// arrays, appended the way a transcript is: a turn that extends the
// conversation appends its tail to the shared array, and every event's slice
// points into it — so the per-event message HEADERS (48 bytes each, 15.4
// million of them across one measured 30-day archive = 738MB) are shared the
// same way the strings are, instead of cloned per event. A turn whose prefix
// stops matching (a compaction that rewrote history) shares only the verified
// prefix and copies the rest — graceful degradation, the same trade the string
// table already makes.
//
// Sharing an ARRAY needs a stronger invariant than sharing a string: strings
// are immutable, slices are not. The pipeline's snapshot contract supplies it —
// "Slice fields are reused intentionally — they are only assigned, never
// mutated in place, after the parser completes" — and nothing in this package
// or its readers writes an event's slice elements after Append. The interner
// itself only ever APPENDS to the shared arrays; it never writes an index
// below the length a published event already references.
//
// The table pins no content of its own: every key is a string some event still
// references, and every array element belongs to some event's slice. Eviction
// (whole-session or LRU) therefore needs no pruning — the state is garbage
// with its events. The tradeoff is deliberate: content that disappears and
// comes back (compaction, an evicted conversation) misses the table and gets a
// second copy. Best-effort dedup with a bounded table beats exact dedup with a
// table that outlives what it describes.
//
// Exported for readers outside this package, because the store is not the only
// place these events pile up: agentop decodes the same events off the session
// API and held more memory than the proxy it was watching. Feed events through
// one Interner in the order they were recorded — the tables depend on
// consecutive events of a conversation being neighbours, so shuffled input
// still returns correct strings but shares almost nothing. The zero value is
// ready to use, and one Interner is for one goroutine.
type Interner struct {
	convs map[string]*convState
	lru   []string // conversation keys, least recently used first
}

// convState is one conversation's share of a session: the strings of its last
// content event, and its growing message arrays. The zero value is ready.
type convState struct {
	strings map[string]string
	msgs    []pipeline.InferenceMessage
	tools   []pipeline.InferenceTool
	results []pipeline.InferenceToolResult
}

// conversationKeys separates the conversations sharing one session bucket:
// the BASE key is a 64-bit FNV of the model and the first message (usually
// the system prompt, which differs between a subagent and its parent); the
// FULL key adds the second message (usually the task, which differs between
// two subagents of the same type). Measured on a 30-day archive, the pair
// held 2,125 distinct conversations across 102,767 content events, stable
// across a conversation's turns (99% of turn N's bytes also in turn N-1).
//
// TWO keys because a conversation's first event cannot know its second
// message: it registers under the base key, and its later turns — which carry
// the second message — find it there by falling back from a full-key miss.
// The first event therefore shares with the rest, which a full-key-only
// scheme silently orphaned. The reverse collision — two conversations with
// the same model AND first message — is handled by stateForEvent splitting
// the arriving one off under its full key the moment their second messages
// disagree.
//
// The keys are fresh 8-byte strings: hashing reads the contents, it does not
// retain them.
func conversationKeys(inf *pipeline.InferenceExtension) (full, base string) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(inf.Model))
	_, _ = h.Write([]byte{0})
	if len(inf.Messages) > 0 {
		_, _ = h.Write([]byte(inf.Messages[0].Content))
	}
	base = string(h.Sum(nil))
	if len(inf.Messages) > 1 {
		_, _ = h.Write([]byte(inf.Messages[1].Content))
	}
	return string(h.Sum(nil)), base
}

// stateForEvent resolves the conversation state an inference event belongs
// to: the full key directly, the base key on a full-key miss when the event
// extends what that state holds, and a fresh split state when it does not —
// a sibling conversation sharing the model and system prompt but carrying a
// different task, which must not thrash the original's arrays.
func (in *Interner) stateForEvent(inf *pipeline.InferenceExtension) *convState {
	full, base := conversationKeys(inf)
	if st, ok := in.convs[full]; ok {
		in.touchLRU(full)
		return st
	}
	if st, ok := in.convs[base]; ok {
		if len(st.msgs) >= 2 && len(inf.Messages) >= 2 && inf.Messages[1] != st.msgs[1] {
			// A sibling: same prompt, different task. Split it off under its
			// full key; the original keeps the base key its own turns fall
			// back to.
			return in.newState(full)
		}
		in.touchLRU(base)
		return st
	}
	// First sighting: under the full key when the conversation already shows
	// its second message, else the base key its later turns fall back to.
	if len(inf.Messages) >= 2 {
		return in.newState(full)
	}
	return in.newState(base)
}

// stateFor returns the state for a fixed key — the "" bucket A2A-only events
// share, which today share one table anyway.
func (in *Interner) stateFor(key string) *convState {
	if st, ok := in.convs[key]; ok {
		in.touchLRU(key)
		return st
	}
	return in.newState(key)
}

// newState registers a fresh state under key, evicting the least recently
// used conversation past the cap.
func (in *Interner) newState(key string) *convState {
	if in.convs == nil {
		in.convs = make(map[string]*convState)
	}
	if len(in.convs) >= maxConversations {
		delete(in.convs, in.lru[0])
		in.lru = in.lru[1:]
	}
	st := &convState{}
	in.convs[key] = st
	in.lru = append(in.lru, key)
	return st
}

// touchLRU moves key to the most-recently-used end.
func (in *Interner) touchLRU(key string) {
	for i, k := range in.lru {
		if k == key {
			in.lru = append(in.lru[:i], in.lru[i+1:]...)
			in.lru = append(in.lru, key)
			return
		}
	}
}

// intern returns the conversation's copy of s, recording s as canonical if it
// is new.
//
// The table is keyed on the CANONICAL string, never on the duplicate that was
// looked up. Keying on the duplicate would work identically for lookups —
// equal strings hash equally — while pinning the copy the event just stopped
// referencing, so the table would hold a copy of every message the last
// interned event carried.
//
// Bounded to one event's worth, because the table is rolled forward on every
// Append — so on BenchmarkRetainedHeap it is 2.358MB keyed on the duplicate
// against 2.205MB keyed on the canonical, a 0.15MB difference and not the
// multiplier it first looks like. Worth fixing because it is free, and worth
// measuring before claiming more than that.
func (st *convState) intern(s string, next map[string]string) string {
	if len(s) < InternMinLen {
		return s
	}
	canon, ok := st.strings[s]
	if !ok {
		canon = s
	}
	next[canon] = canon
	return canon
}

// internID is intern without the length floor, for identifiers that repeat
// exactly as message content does while being far shorter than InternMinLen.
// ToolUseID and the message Role are the two today: a "toolu_…" id is ~20
// bytes and a role ~5, but a client re-sends every earlier tool result and
// message on every request, so both duplicate as hard as the conversation
// does — the Role duplication alone was ~15 million strings on one measured
// 30-day archive (#1541). Only the empty string is beneath sharing, because
// it is the one value every absent id shares for no reason.
//
// Same canonical-keying contract as intern; see that function for why.
func (st *convState) internID(s string, next map[string]string) string {
	if s == "" {
		return s
	}
	canon, ok := st.strings[s]
	if !ok {
		canon = s
	}
	next[canon] = canon
	return canon
}

// InternEvent gives the event its own extension, with content pointing at the
// session's existing copies and message slices prefix-shared into the
// conversation's growing arrays, then rolls the string table forward.
//
// It CLONES or prefix-shares rather than rewriting in place, and that is not
// defensive habit — writing in place is unsound here for three separate
// reasons:
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
	// An event with nothing to intern must leave the tables ALONE rather than roll an
	// empty one forward. Rolling is what makes turn N share with turn N-1, and a session's
	// events are not all turns: a CONNECT tunnel-open, a denial, an MCP call all carry
	// no interned content, and one of them landing between two inference events used to
	// reset the table and force the second to keep its own copy of the whole conversation.
	//
	// That interleaving is the normal case, not an edge case. Every bridged HTTPS request
	// records a tunnel-open, and it lands between the inference request and its response —
	// measured on a live session, 165 of 500 events. Skipping the roll took that window's
	// retention from 108.6MB to 89.2MB.
	//
	// With per-conversation tables this early return is also what keeps a tunnel-open
	// from disturbing ANY conversation's state, not just the one that happened to be
	// last.
	if e.Inference == nil && e.A2A == nil {
		return
	}
	var st *convState
	if e.Inference != nil {
		st = in.stateForEvent(e.Inference)
	} else {
		st = in.stateFor("")
	}
	next := make(map[string]string, len(st.strings))

	if e.Inference != nil {
		cp := *e.Inference
		cp.Messages = shareMessages(st, e.Inference.Messages, next)
		cp.Completion = st.intern(cp.Completion, next)
		// The tool manifest re-sends wholesale on every request, so its array
		// prefix-shares the same way the conversation does; a client that
		// reorders or edits its manifest shares only the verified prefix.
		cp.Tools = shareTools(st, e.Inference.Tools, next)
		// Tool results repeat like the conversation — every request re-sends all
		// of them — and alias across both phases of a request the same way.
		cp.ToolResults = shareToolResults(st, e.Inference.ToolResults, next)
		e.Inference = &cp
	}
	if e.A2A != nil {
		cp := *e.A2A
		cp.Parts = slices.Clone(e.A2A.Parts)
		for i := range cp.Parts {
			cp.Parts[i].Content = st.intern(cp.Parts[i].Content, next)
		}
		e.A2A = &cp
	}

	// Nothing was interned, so there is nothing to roll: keep the table the last event that
	// DID intern something left behind. Strictly stronger than the early return at the top,
	// which it subsumes — that one is an allocation shortcut, this one is the invariant.
	// The shared arrays may still have grown; they are self-consistent without the
	// string table, which only exists to find canonical strings for the NEXT event.
	if len(next) == 0 {
		return
	}
	st.strings = next
}

// shareMessages returns the event's message slice, sharing the conversation's
// growing backing array for the prefix that matches its last event and
// appending the canonicalized tail. Equal after canonicalization means
// struct equality, so a shared prefix element is byte-identical to the one
// the event arrived with.
//
// The three shapes:
//
//   - prefix match, no tail (the response-phase event of the request just
//     interned, or a truncated re-send): the shared array's prefix itself,
//     zero allocation.
//   - full match, with tail (the next turn): append to the shared array,
//     reallocating as a transcript does — amortized one header copy per new
//     message, none per event.
//   - partial match (a compaction rewrote history): share only the verified
//     prefix, taking its elements from the state's array so the event holds
//     the canonical strings earlier events already hold; the rest is
//     canonicalized into a fresh tail and the state's array is rebased to it.
//     The event's slice and the state's diverge from here on, exactly as they
//     do for strings that disappear and come back.
func shareMessages(st *convState, msgs []pipeline.InferenceMessage, next map[string]string) []pipeline.InferenceMessage {
	k := 0
	// Struct ==, not a field list: a field added to InferenceMessage joins the
	// comparison by itself, and a non-comparable one breaks the build rather
	// than being silently dropped from it.
	for k < len(msgs) && k < len(st.msgs) && msgs[k] == st.msgs[k] {
		// Record the matched element so the table stays "the last event's
		// strings" — a later verbatim re-send of an old message (a compaction)
		// must still find its canonical. Recorded from the ARRAY's copy, never
		// the arriving one, so a miss cannot pin a duplicate no event holds.
		st.intern(st.msgs[k].Content, next)
		st.internID(st.msgs[k].Role, next)
		k++
	}
	if k == len(msgs) {
		// The whole arriving history is the shared array's prefix — a verbatim
		// repeat or a truncation of it. The prefix itself: canonical elements,
		// zero copy.
		return st.msgs[:k]
	}
	if k == len(st.msgs) {
		tail := make([]pipeline.InferenceMessage, len(msgs)-k)
		for i := range tail {
			tail[i] = msgs[k+i]
			tail[i].Role = st.internID(tail[i].Role, next)
			tail[i].Content = st.intern(tail[i].Content, next)
		}
		st.msgs = append(st.msgs, tail...)
		return st.msgs
	}
	// Partial match: rebuild from the verified prefix, CANONICAL side first —
	// copying the arriving elements instead would retain the event's own wire
	// copy of every prefix string alongside the canonical one.
	out := make([]pipeline.InferenceMessage, len(msgs))
	copy(out, st.msgs[:k])
	for i := k; i < len(msgs); i++ {
		out[i] = msgs[i]
		out[i].Role = st.internID(out[i].Role, next)
		out[i].Content = st.intern(out[i].Content, next)
	}
	st.msgs = out
	return out
}

// shareTools is shareMessages for the tool manifest.
func shareTools(st *convState, tools []pipeline.InferenceTool, next map[string]string) []pipeline.InferenceTool {
	k := 0
	for k < len(tools) && k < len(st.tools) && tools[k] == st.tools[k] {
		st.intern(st.tools[k].Description, next)
		st.internID(st.tools[k].Name, next)
		k++
	}
	if k == len(tools) {
		return st.tools[:k]
	}
	if k == len(st.tools) {
		tail := make([]pipeline.InferenceTool, len(tools)-k)
		for i := range tail {
			tail[i] = tools[k+i]
			tail[i].Name = st.internID(tail[i].Name, next)
			tail[i].Description = st.intern(tail[i].Description, next)
			tail[i].Parameters = pipeline.RawJSON(st.intern(string(tail[i].Parameters), next))
		}
		st.tools = append(st.tools, tail...)
		return st.tools
	}
	out := make([]pipeline.InferenceTool, len(tools))
	copy(out, st.tools[:k])
	for i := k; i < len(tools); i++ {
		out[i] = tools[i]
		out[i].Name = st.internID(out[i].Name, next)
		out[i].Description = st.intern(out[i].Description, next)
		out[i].Parameters = pipeline.RawJSON(st.intern(string(out[i].Parameters), next))
	}
	st.tools = out
	return out
}

// shareToolResults is shareMessages for tool results.
func shareToolResults(st *convState, results []pipeline.InferenceToolResult, next map[string]string) []pipeline.InferenceToolResult {
	k := 0
	for k < len(results) && k < len(st.results) && results[k] == st.results[k] {
		st.intern(st.results[k].Content, next)
		st.internID(st.results[k].ToolUseID, next)
		k++
	}
	if k == len(results) {
		return st.results[:k]
	}
	if k == len(st.results) {
		tail := make([]pipeline.InferenceToolResult, len(results)-k)
		for i := range tail {
			tail[i] = results[k+i]
			tail[i].ToolUseID = st.internID(tail[i].ToolUseID, next)
			tail[i].Content = st.intern(tail[i].Content, next)
		}
		st.results = append(st.results, tail...)
		return st.results
	}
	out := make([]pipeline.InferenceToolResult, len(results))
	copy(out, st.results[:k])
	for i := k; i < len(results); i++ {
		out[i] = results[i]
		out[i].ToolUseID = st.internID(out[i].ToolUseID, next)
		out[i].Content = st.intern(out[i].Content, next)
	}
	st.results = out
	return out
}

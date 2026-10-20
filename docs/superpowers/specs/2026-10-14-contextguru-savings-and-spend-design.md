# context-guru in Cortex: savings and the plugin's own spend

Status: draft. Part 2 (`CostRecorder`) is defined and consumed on `feat/contextguru-keepalive`,
and `cmd/cortex` and `cmd/cortex-envoy` inject `spend.LogRecorder`, which only logs each charge
(`plugin spend`). Part 1 is implemented through §1.4, with the representation change in §1.4a.
The recorder that writes the ledger (§2.5) is a proposal.

Follows PR rossoctl/context-guru#381 §9.3, which proposed `CostRecorder`.

## 0. The problem

context-guru changes money in two directions, and Cortex records neither.

| Direction | What happens | What Cortex records today |
|---|---|---|
| Saving | The plugin shrinks the request body, so the provider bills fewer prompt tokens. | Only the lower `costMicros`. No row says context-guru caused it. `settle.Avoided` prices savings for `tool-prune` only (`savingComponents`), and context-guru publishes no plugin event. |
| Spend | The plugin calls a model itself: keep-alive pings, and the engine's LLM components (`extract_llm`, `summarize`, `cache_aware_summarizer`). | Nothing. Pings go out on a client with `Proxy: nil`. Model calls go out on `llmclient`'s own `http.Client`. Neither passes through the pipeline, so neither reaches `inference-parser`, the session store or the cost ledger. Pings are only logged. |

So an operator cannot answer "does context-guru pay for itself?" from Cortex. The saving has no
name, and the cost of producing it is missing. A net figure computed from what Cortex has today
always leans toward "it pays", which is the one direction a savings figure must not lean.

Two facts found while designing this (not fixed here):

- `llmclient.ChatResponse` decoded only `choices`. The usage of every plugin model call was
  discarded. This change adds `Usage` (additive).
- The engine's own usage accounting (`cheapmodel.Sink`) is fed only by the engine's own model
  client. Cortex passes its own `components.Model` (`cgModel`), so under Cortex the token and
  cost fields of `components.ModelCall` are probably zero. The engine's economic gates
  (`cheapmodel.AvgCallCost`) then see no calls. That is an engine-integration gap to track
  separately; this design does not depend on it, because it measures at Cortex's transport.

## Part 1 — Savings

### 1.1 Goal

Each request that context-guru changed carries a saving attributed to `context-guru`, priced by
`settle` from the response, through the same path as tool-prune's. The figure must be able to be
zero or negative when compaction cost more than it saved on that turn.

### 1.2 Why tool-prune's shape is not enough

tool-prune publishes `{bytesRemoved, bodyBytesAfter, projected}`. `settle.Avoided` turns bytes
into tokens with the response's own tokens-per-byte, then prices them at the tier the prompt
landed in. That is correct for tool-prune, which removes declarations from the head of the
prompt and never changes a message. It is wrong for context-guru for three reasons:

1. **A smaller body can cost more.** When context-guru rewrites a message inside the cached
   prefix, the provider re-writes the rest of the prefix at the cache-write rate (1.25x input)
   instead of reading it (0.1x). The engine pins its `cg:*` state to make this rare, but
   "bytes removed × rate" is always positive and cannot show it.
2. **The counterfactual tier is not the tier the response shows.** Removed tokens would have
   been billed at whatever tier they occupied *without* compaction. Tokens removed from the
   uncached tail would have been fresh input or cache-write; the response's own tier split
   (after compaction) says nothing about that.
3. **One figure hides which component did the work.** The engine runs several components
   (mask, extract, summarize, …). The operator needs per-component figures to tune the preset.

### 1.3 What the plugin publishes

A facts-only event under `context-guru/event` (`pctx.Extensions.Custom`, as tool-prune does),
built from `apply.Result.Trace` and `Trace.Run` — the engine already returns both per request:

```go
type savingEvent struct {
	// Applied bytes, the same meaning as tool-prune's, so settle's calibration code is shared.
	BytesRemoved   int  `json:"bytesRemoved"`
	BodyBytesAfter int  `json:"bodyBytesAfter"`
	Projected      bool `json:"projected,omitempty"` // SetBody returned false (on_error: observe)

	// Where the removed tokens sat, so settle can pick the counterfactual tier (§1.4).
	TokensBefore    int `json:"tokensBefore"`    // Trace.Run.TokensBefore
	TokensAfter     int `json:"tokensAfter"`     // Trace.Run.TokensAfter
	FrozenTokens    int `json:"frozenTokens"`    // Trace.FrozenTokens: the cached prefix left alone
	PrefixRewritten bool `json:"prefixRewritten"` // a change landed at or below Trace.MaxCachedIdx

	// The components' savings split into all of it and the part this session has not had
	// removed before (§1.4 rule 2). Always sent, so 0 unique reads as "all repeat".
	SavedGross  int `json:"savedGross"`
	SavedUnique int `json:"savedUnique"`

	// Per component, engine token counts, no content.
	Components []componentSaving `json:"components,omitempty"`
}

type componentSaving struct {
	Component string `json:"component"`
	Saved     int    `json:"saved"`             // Report.Saved()
	Replay    bool   `json:"replay,omitempty"`  // Report.Replays > 0: a free replay, not a fresh act
	Unique    int    `json:"unique"`            // part of Saved new to this session (see below)
}
```

`Unique` uses the engine's `dash.Recorder.MarkUnique` rule, so Cortex and the context-guru proxy
agree: the plugin keeps a set of seen `Report.CacheKeys` per session (and component), the unique
part is `saved × newKeys / len(keys)`, and a component with no keys counts as unique. Scoped by
session rather than by tenant, because a provider cache is per conversation. The set is bounded
(200,000 hashed entries, cleared when full, as `MarkUnique` is).

Keys exist for Reformats and for Offloads that drop content without stashing it, which is how
this plugin runs every Offload (it caps `marker_mode` at `summary`): the engine derives those keys
from the changed messages' text (context-guru `components/pipeline.go`). Before it did, `dedup`
and `extract_llm` had no keys here and counted as unique on every turn — 933k of 934k saved tokens
in a 24-turn Sonnet 4.6 run, against 48k unique for the same engine in the proxy.

No message text: the session API is unauthenticated, and `Trace.Changes` carries clipped
before/after text, so it is never published.

`PrefixRewritten` needs one engine fact Cortex cannot derive: whether any changed index is at or
below the cached boundary. `Report.ChangedIdx` and `Trace.MaxCachedIdx` give it directly.

### 1.4 How settle prices it

`settle.Avoided` gains a second branch for `context-guru`, added to `savingComponents` with a
test (the list stays explicit):

1. Tokens: `TokensBefore − TokensAfter` from the engine. Fall back to the byte ratio only when
   the engine counts are absent. Mark `Estimated` either way: the engine's counts are its own
   tokenizer's, not the provider's.
2. Tier: split the tokens by `SavedUnique / SavedGross` (1 when the event does not split).
   - The **unique** part is content removed for the first time. It came from the uncached tail
     (the engine compacts only above the frozen prefix when cache-aware), so price it at the input
     rate, or at the cache-write rate when the response shows a cache write on this turn.
   - The **repeated** part is content removed on an earlier turn and removed again. Without
     compaction it would sit in the cached prefix by now, so price it at the cache-read rate when
     this request's cache hit; on a miss, at the cache-write rate when the write covers the prompt
     (cache write ≥ fresh input), else at the input rate. This is the proxy's `Event.repeatRate`.
   - `Saving.Tier` names the larger part; `USD` prices both.

   **Correction (2026-10-15).** The first version of this rule priced every removed token as
   unique, on every turn. On a long session most of a turn's saving is repeated content, so the
   rule reported about 5× the measured saving (fill-end comparison: $1.358 reported against
   $0.271 measured, arm A). `componentSaving.Replay` was published but no rule read it.
3. Penalty: when `PrefixRewritten`, subtract the extra cache-write the rewrite caused —
   the response's `cache_write` tokens times `(cache-write − cache-read)` rate. This is the
   only way the saving goes negative, and it uses the response's own counts.
4. `Projected` is carried through. `TotalAvoidedUSD` already skips projected savings.

### 1.4a As built: a separate penalty, not a negative saving

A negative `Saving.USD` would be refused by `pricing.MicrosFromUSD`, which rejects negative input
on purpose, so it would become 0 in every total without a sign. Signed savings would change the
money layer's bounds and every consumer in §1.6. So, for now:

- `Saving.USD` stays the gross saving, non-negative.
- `Saving.PenaltyUSD` (new, non-negative) is the cache-rewrite cost the same component caused.
- `Event.TotalAvoidedUSD` (and so `TotalAvoidedMicros`, the ledger, the usage aggregator and the
  session totals) counts `max(0, USD − PenaltyUSD)` per saving.

The floor under-states a turn whose penalty exceeds its saving: the totals show 0, not the loss.
The loss is on the record (`USD`, `PenaltyUSD`). Signed totals stay a decision for later.

`settle` reads context-guru's event with its own type (`compactionFacts`, `compactionDraft`),
separate from tool-prune's `pruneFacts`; each component on `savingComponents` names its rule.
The penalty is an upper bound (part of a turn's cache write is its new tail), so it leans toward
"compaction did not pay".

### 1.5 Net figure

Net = applied saving (part 1) − plugin spend (part 2), per session. Neither is ever added to
spend totals — `event.Event.Avoided`'s invariant stands. The net figure is a separate read, for
agentop's session row and `/v1/usage`, computed from two ledger columns (`AvoidedMicros`, and a
spend column keyed by purpose, §2.6).

### 1.6 Changes needed

| Where | Change |
|---|---|
| `core/plugins/contextguru` | publish `context-guru/event` after `SetBody` |
| `core/cost/settle/avoided.go` | context-guru branch; add to `savingComponents`; test pinning the wire field names, as for tool-prune |
| `core/cost/event` | done: `Saving.PenaltyUSD`; `TotalAvoidedUSD` nets it, floored at 0 (§1.4a) |
| `core/cost/ledger`, `core/cost/usage` | none while savings stay unsigned; all read `TotalAvoidedMicros` |
| agentop | done: the events pane's TOKENS and COST cells show tool-prune and context-guru combined (`rowSavingFor`), net of penalty and floored at 0 per saving; applied and projected savings never mixed |
| engine (context-guru) | none required; `Trace` already carries the facts |

### 1.7 Open questions

- Is the input-rate tier (§1.4 step 2) good enough, or does the engine need to report which of
  the removed tokens were in a region the provider had cached on the previous turn?
- Should the penalty use the previous turn's billed input (`cg:bin`, gap B in the state
  analysis) instead of this turn's cache write? That needs `RecordBilledInput` wired first.

## Part 2 — The plugin's own spend: `CostRecorder`

### 2.1 Goal

Every model call the plugin makes outside the pipeline reaches the cost ledger, attributed to the
session, agent, endpoint and model it served, priced by Cortex's own rate table, and labelled so
it is never mistaken for the agent's own traffic.

### 2.2 The interface (defined on this branch)

`core/cost/spend`:

```go
type Charge struct {
	Plugin, Purpose string           // "context-guru", PurposeKeepAlive | PurposeCompaction
	Session         string           // the session the call served
	Client          *pipeline.EventClient
	Host, Model     string           // the endpoint billed and the model it ran
	Usage           pricing.Usage    // tokens by tier, as the provider reported them
	Status          int
	At              time.Time
	Duration        time.Duration
}

type CostRecorder interface{ RecordCost(Charge) }
type RecorderConsumer interface{ SetCostRecorder(CostRecorder) }
```

Injected through `plugins.Deps.Costs`, before `Configure`, never as a typed nil — the same
contract as `pricing.ResolverConsumer`.

Changes from PR #381 §9.3's sketch, and why:

| §9.3 | Here | Why |
|---|---|---|
| `RecordCost(sessionID, client, host string, ev event.Event)` | `RecordCost(Charge)` | A struct can grow without breaking callers. It also needs `Model`: the ledger row is labelled by model, and §9.3's signature had no way to pass it. |
| The plugin prices and passes `event.Event` | The plugin passes tokens; the recorder prices | One pricing site. The recorder uses the same `pricing.Registry` as `settle`, so provenance, currency, plausibility checks and long-context thresholds come out exactly as they do for a real request. A plugin assembling `event.Event` would have to reproduce `settle`'s rules, and would drift. |
| `client string` | `*pipeline.EventClient` | That is what the ledger's agent label is built from (`ledger.agentLabel`). |
| — | `Purpose` | A ping and a compaction call answer different questions ("is keep-alive worth it?", "is compaction worth it?"). |

Contract for implementers, stated on the interface: safe for concurrent use; does not block on
I/O (it is called from the keep-alive sweep and from model-call paths, some of which hold a
request); never makes the session active or refreshes it; may drop a charge for a session that
no longer exists, rather than re-create the session.

### 2.3 Where the plugin calls it (implemented on this branch)

- **Keep-alive pings.** Each `keepalive.Record` carries its own emitter. The plugin now passes a
  per-record `pingEmitter` that knows the session, endpoint and client of the request that
  established the entry. On `KeepAlivePing` it logs, as before, and records a `Charge` with
  `PurposeKeepAlive`. The ping's `Usage` is `FreshInput / CacheWrite / CacheRead / Output` from
  `KeepAliveReport`. A report with no tokens records nothing.
- **Engine model calls.** `cgModel` is the only path any engine component has to a model.
  The plugin now builds a per-request `cgModel` for both the incoming and the static client,
  carrying the session, client and endpoint, and records a `Charge` with `PurposeCompaction`
  after each successful call that reported usage. A detached summarizer call made after the
  request ended is still attributed correctly, because it holds the per-request client.

  This is measured at Cortex's transport, not read from `RunReport.Calls`, for two reasons:
  `ModelCall`'s token fields are probably empty under Cortex (§0), and the transport sees every
  call exactly once, including detached ones the engine attributes one turn late.

With no recorder injected, both paths only log, as today. `spend.LogRecorder` (the one the
binaries inject now) adds one `plugin spend` line per charge, with the tokens but no price. A
ping therefore logs twice: the keeper's `context-guru keep-alive ping` line, which carries the
keeper's own `costUSD`, and the recorder's line.

### 2.4 Keep-alive gate

PR #381 §6.4 asked the adapter to refuse keep-alive when nothing records its spend. This branch
keeps the startup WARN instead, worded on whether a recorder was injected. The binaries inject
`LogRecorder`, so the WARN does not fire there, though the spend still does not reach the ledger.
Once §2.5 replaces `LogRecorder`, the WARN should become a refusal.

### 2.5 The recorder that writes the ledger (proposal, not built)

The ledger and the usage aggregator are `session.Recorder`s: they see every event the session
store appends. So the simplest correct recorder appends one event to the session store, and
everything downstream — ledger row, usage ring, archive, agentop — follows with no change.

```go
// cmd/cortex wiring, sketch
type sessionCostRecorder struct {
	store *session.Store
	rates *pricing.Registry
}

func (r sessionCostRecorder) RecordCost(c spend.Charge) {
	ev := settle.NewSpendRecord(r.rates, c) // prices c.Usage like Settle does; Source: SourcePluginSpend
	r.store.AppendToExisting(c.Session, pipeline.SessionEvent{
		At: c.At, Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: c.Host, StatusCode: c.Status, Duration: c.Duration, Client: c.Client,
		Inference: &pipeline.InferenceExtension{Model: c.Model, /* token counts from c.Usage */},
		Plugins:   map[string]json.RawMessage{event.Key: mustJSON(ev)},
		SpendOf:   c.Plugin + "/" + c.Purpose, // new field, see below
	})
}
```

What it needs:

1. **`session.Store.AppendToExisting(id, ev) bool`.** Like `AppendTrailing`, but found by id: it
   appends only to a session that exists, never creates one, never makes it active, never
   refreshes `UpdatedAt`. `AppendTrailing` needs a `*Bucket` the plugin does not have. This is
   the reason §9.3 named `AppendTrailing`; this is its by-id form.
2. **`settle.NewSpendRecord`.** Prices a `pricing.Usage` through `modelledCost`, with the same
   plausibility checks as `Settle`, and stamps a new `event.SourcePluginSpend`. It is never
   `"authoritative"`: no gateway header priced it.
3. **A spend label on the event and the ledger row.** Without one, a ping is counted as one of
   the agent's own inference requests, which inflates its request count and mixes ping latency
   into its latency series. Proposed: `SessionEvent.SpendOf` and `ledger.Row.Purpose`, with
   usage totals that include the money but keep a separate request counter. This is the part
   that most needs a decision (§2.7).
4. A `cmd/cortex` (and envoy, cpex) injection, guarded by a test like
   `TestEveryBinaryInjectsPricing`.

### 2.6 What an operator then sees

- The session's `costMicros` includes the plugin's spend. That is right: it is money the
  session's credential paid.
- A per-purpose split (`keep-alive`, `compaction`) in `/v1/usage` and agentop, so keep-alive and
  compaction can each be judged against what they bought.
- With part 1: net saving per session = `avoided(context-guru) − spend(context-guru/*)`.

### 2.7 Decisions needed

1. Does plugin spend count as a *request* in usage totals, or only as money? (Proposal: money
   yes, request no, with its own counter.)
2. Is `AppendToExisting` acceptable in `core/session`, or should the recorder write the ledger
   and the usage aggregator directly, bypassing the store? Direct writes skip the archive and
   agentop's live stream, so the store is preferred.
3. A ping for a session the store has already evicted: drop (proposal) or write a ledger-only
   row?

## Phases

1. **Done on this branch:** `spend.CostRecorder`, `Deps.Costs` injection, `llmclient` usage,
   plugin calls for pings and model calls, `spend.LogRecorder` injected by `cmd/cortex` and
   `cmd/cortex-envoy`.
2. §2.5: the session-store recorder, `SourcePluginSpend`, purpose label, binary injection; then
   turn the keep-alive WARN into a refusal.
3. Part 1: done — `context-guru/event`, settle's context-guru rule, `PenaltyUSD`. agentop shows it per
   row. Open: signed totals.

# Cost as a first-class citizen

<!-- Commit references in this document point into TWO pull requests, not one. The work was
     reviewed as a single branch and then split: authlib and the proxy binary into the
     aggregate/ledger PR, the abctl surfaces into the PR stacked on it, and these documents
     into a third. Every SHA below was repointed after that split and verified to resolve.
     Four commits were split in half and are cited as `<one-half>` / `<other-half>`, labelled
     (core) and (abctl), so either PR can be reached from here.

     THREE SHAs ARE DELIBERATELY UNREACHABLE: cbe34bbc, c5b1f596 and 4765644f appear in text
     explaining that an earlier banner pointed at them wrongly. They are commits from an
     abandoned branch, and rewriting them would delete the correction they exist to record.

     "VERIFIED TO RESOLVE" IS NOW A CHECKABLE CLAIM, AND IT USED TO BE FALSE. Four
     "SUPERSEDED by" citations in the ledger plan still pointed at the abandoned branch,
     so they resolved for nobody but the author while this banner asserted the opposite —
     the exact failure the paragraph above was written about, one screen below it. They now
     point at their equivalents on the aggregate/ledger PR, each confirmed three ways:
     reachable from that branch, a unique subject match, and an IDENTICAL patch-id, since
     the split cherry-picked them rather than rewriting them.

     Re-check the whole set after any rebase, rather than trusting this paragraph:

       grep -rhoE '\b[0-9a-f]{8}\b' authbridge/docs/superpowers/{plans,specs}/2026-09-13-*.md |
         sort -u | while read s; do git merge-base --is-ancestor $s <core-branch> 2>/dev/null ||
         git merge-base --is-ancestor $s <abctl-branch> 2>/dev/null || echo "UNREACHABLE $s"; done

     Expect exactly the three named above. Anything else is a citation a reviewer cannot
     follow. Note the pattern also matches hex-looking prose (0b11001, 218100000); those are
     figures, not SHAs. -->

**Date:** 2026-09-13
**Issues:** closes [#950](https://github.com/rossoctl/cortex/issues/950),
[#952](https://github.com/rossoctl/cortex/issues/952),
[#972](https://github.com/rossoctl/cortex/issues/972); satisfies
[#953](https://github.com/rossoctl/cortex/issues/953) except latency percentiles
([#951](https://github.com/rossoctl/cortex/issues/951)); feeds
[#963](https://github.com/rossoctl/cortex/issues/963)
**Release:** Cortex v0.9.0 ([#962](https://github.com/rossoctl/cortex/issues/962)), exit-bar item 3
**Delivery:** one PR, seven signed commits

> **STATUS — read before relying on this.** This design was written against
> `767cbb63`. Upstream has since implemented several of its commits
> independently and with a better factoring, and this work was rebased onto
> current `main` rather than merged from that base. Superseded upstream:
> `costevent.Key` and the presence-versus-pricedness split; the single-costing-owner
> refactor, which upstream extracted into an `authlib/costing` package rather than
> leaving in the parser; the `Avoided` container and `EstimateTokensFromBytes`; and
> the abctl user-settings file.
>
> What survived, and is what this branch delivers: the aggregate token split with
> `presentKinds`, the model/endpoint/session/agent groupings, the spend strip and
> per-session cost, the durable cost ledger, and the Cost pane. Upstream also has
> pricing multipliers this design did not know about, without which every figure
> here would have been vendor list price.
>
> The last two of those arrived late, and a reader of an earlier revision of this
> banner was told they were missing: the Cost pane landed in `c86507ca` and the
> `agent` grouping — `usage.GroupAgent`, `pipeline.EventClient`, the ledger's
> `agent` column — in `6870ad3b`. Both are in the tree. A banner that understates
> what shipped defeats this convention exactly as thoroughly as one that overstates
> it, so this is recorded rather than quietly corrected.
>
> **The one rule this design is built around is NOT achieved on this branch.**
> "Exactly one component turns tokens into dollars" (see *Architecture*) remains an
> aspiration, and the code says so in its own godoc rather than leaving a reader to
> find out by comparing two totals. `usage.Aggregator.costOf` still resolves a rate
> itself for any request that carries a model and tokens but arrives with no settled
> record. Three places say it in as many words: `authlib/usage/usage.go`'s
> `eventCost` doc ("This package is NOT rate-free, which this comment previously
> claimed"), `authlib/sessionapi/usage.go`'s handler doc ("cost is NOT
> single-sourced … do not write here that cost is computed in exactly one place,
> because it is not") and `authlib/costledger/row.go`'s divergence paragraph, which
> enumerates the three differences that follow. So there are two sources — a
> published figure preferred, a modelled one as fallback — and they can answer
> differently about the same request. Of the four sites #972 names, `costOf` is
> still one of them.
>
> Two of its findings turned out to be bugs upstream still had, and are fixed on
> this branch: a positive gateway cost header on a body-less response was charged
> nowhere, and a truncated stream's floor was published as an exact total.
>
> Kept rather than rewritten, because the reasoning is why the code looks the way
> it does — including the parts that were wrong. Where it disagrees with the code,
> the code is right.
>
> **Line numbers drift.** Every `file.go:NN` citation below was accurate against the
> tree it was written against, and many have since moved — `fold` alone has travelled
> from `snapshot.go:159` to `:345` to `:438`. Read them as "roughly here"; find the
> symbol by name.
>
> ---
>
> **SECOND SWEEP, 2026-09-15.** Two further review rounds landed fourteen-plus fixes
> after the banner above was written. Eleven of its claims stopped being true; they are
> collected here rather than rewritten in place, because in most cases the reasoning is
> still why the code looks the way it does.
>
> 1. **Costing has its own package, `authlib/costing`.** The *Layers* table names
>    `inference-parser` as the costing site, and *Architecture* gives three reasons the
>    parser should own the rule. The parser now **calls** `costing.Settle` at the point
>    the token counters are final; it does not own the rule. `costing`'s package doc
>    gives the reason this design missed: a gateway's cost-header semantics are
>    vendor-specific knowledge with no place in a provider-shaped body parser. The
>    "only the parser knows when usage is final" argument survives intact — it is
>    exactly why the parser is the *caller* — so the three reasons are left standing.
> 2. **The history is "two places", not four.** `costing`'s own account is that cost was
>    decided inside `litellm-budget-track` and again inside the usage aggregator. The
>    four sites *Architecture* lists are #972's enumeration, which counted two client-side
>    renderers of a published figure alongside two producers of one. And budget-track now
>    **amends** the settled record to enforce a budget rather than consuming it.
> 3. **Delivery row 7 did land — upstream, not here.** `ToolPrune` has no rate table
>    (its struct says so where the field used to be) and `pruneEvent` "carries FACTS
>    ONLY"; `pricing.EstimateTokensFromBytes` lives in `authlib/pricing/estimate.go`;
>    and `costing.Avoided`, called from `inference-parser`, publishes priced savings into
>    `costevent.Event.Avoided`. What did **not** land is the aggregation of that container
>    into `usage.Counts` — so the four missing `avoided*`/`prunable*` schema rows and the
>    pane's missing `AVOIDED` section are still right, but "did not land at all" is not.
>    A consequence for *Savings, honestly*: its `tui/prune_saving.go:97` and `:104-112`
>    citations now point at a file whose own header says it does none of that arithmetic.
> 4. **The counterfactual-invariant test is written**, in `usage/pricing_test.go` as
>    `TestAggregator_TotalsAreInvariantToAvoidedCost`. It drives a real `Avoided` payload
>    through `Record` twice and pins every money field across the pair. Not vacuous
>    either, since `costevent` carries the container.
> 5. **One client is recognised, not three.** `pipeline.knownClients` maps `claude-cli`
>    → `claude-code` and nothing else; `opencode` and `codex` are deliberately absent,
>    because a wrong guess files one agent under another's name where an unrecognised
>    one still reports under its raw User-Agent and can be identified from the breakdown.
>    The `EventClient` sketch's comment lists all three as if they were implemented.
> 6. **The ledger stores an absent client as `""`, not `"unknown"`.** `"unknown"` is
>    `pipeline.UnknownClientLabel`, applied at the query boundary by
>    `costledger.labelFor` — a durable file must not bake a display value into a column
>    where it becomes permanently indistinguishable from an agent that really called
>    itself that. Corrected inline under *Storage*.
> 7. **Three `/v1/usage` additions the *API* section does not list**, all additive:
>    `Counts.IncompleteRequests` (how many of a total's figures are a floor rather than
>    an exact number), `Snapshot.Degraded` (a pointer, so absent ≠ zero: how many ledger
>    lines and day files a read lost) and `Snapshot.IncompleteBy` (which *way* a figure
>    is inexact). **All three now reach abctl** — `IncompleteBy` was producer-side only when
>    the line above was written, and `20022a75` closed it in `--json`, the human summary and
>    the Cost pane. Closing it exposed a worse defect than the missing field: the pane had
>    been asserting "N of M priced figures are lower bounds — so the real total is higher"
>    over EVERY inexact figure, which is false for an approximation and false on the pane's
>    own default window, where a ledger row carries no reason at all. It now names a
>    direction only where the response named one, and says so plainly when it cannot.
>
>    A fourth disclosure joined them: `Snapshot.UngroupedCostMicros`, the cost no series
>    entry carries, so a breakdown that does not add up says so instead of just being short.
> 8. **The Cost pane mock-up is a picture of the design, not of the pane.** Shipped
>    headings are `TOTAL`, `BY <group>`, `WHERE IT WENT (tokens, not dollars)` — the
>    parenthetical is *in the heading* — and `COVERAGE`. There is no `AVOIDED` section
>    and no per-tier dollar column. The group cycle is model → endpoint → session →
>    agent.
> 9. **The strip's width table is illustrative, not a threshold list.** No width
>    constant exists: `fitStripFigures` searches figure count × verbosity and drops the
>    `SPEND` label before it drops a number. Every money figure now wears up to three
>    one-column claims, composed as `!~$4.1700+` — `!` the ledger read lost rows, `~` at
>    least one figure in this total is inexact, `+` the figure covers only part of the
>    traffic. Markers are never what gets dropped. A fifth figure was added: how long
>    ago the strip last polled, when that answer is stale.
> 10. **Retention is configurable but floored, and the config is restart-only.** A
>     non-zero `cost_ledger.retention_days` below **8** is refused at load, because
>     `window=7d` is served from these files and a rolling 7×24h touches EIGHT local day
>     files unless it happens to begin at midnight. The floor is derived
>     (`usage.Window7dLocalDays`) rather than written as its own number — as a literal it
>     said 7 and admitted the partial week it existed to refuse; `0` still means 30. And
>     `CostLedgerConfig`
>     is **not** hot-reloadable — the reloader has no reference to it and the writer is
>     opened once at startup, so every key takes effect on restart.
> 11. **Persisted UI state is a `cost:` section in `~/.cortex/abctl-config.yaml`**, the
>     YAML file main already writes — not the `~/.cortex/abctl-ui.json` this design
>     proposed. The banner records that file as superseded upstream; this names what
>     replaced it.

## Why

Cortex measures tokens well and reports money badly.

On a default laptop install the pipeline is `inference-parser`, `mcp-parser`, `a2a-parser` and
`tool-prune` with an empty remove list (`cmd/authbridge-proxy/local.go:117-140`). In that
configuration:

- `toolprune/plugin.go:429-433` returns early when nothing shrank, so no prune event is
  published.
- `tui/cost_event.go:62` needs that event's rates: `if resp == nil || ps.RateSource == "none"`.
- So the events pane's `COST` column is **empty on every row out of the box**, while
  `/v1/usage` prices the same requests server-side through
  `usage.WithPricing(pricingRegistry)` (`cmd/authbridge-proxy/main.go:404`).

Tokens render; money does not. A new user's first impression of an observability tool is a
blank money column.

Nothing aggregates either. `usage.Counts` carries a single scalar `Tokens`
(`authlib/usage/usage.go:49-81`) even though the session event has published the full split
since #811. Cost cannot be charted at all — `usageMetric` is tokens / requests / errors /
latency (`tui/usage_render.go:45-48`). There is no grouping by model or endpoint
(`authlib/usage/snapshot.go:13-18`). Tool-prune savings exist only as a per-row parenthetical
computed client-side.

## Scope

In:

- One costing owner, per #972's design, phases 1-3.
- Per-model, per-endpoint and per-agent aggregation of the four-way token split and cost (#950).
- Tool-prune savings attributed, aggregated and quarantined from spend (#952).
- A durable cost ledger so "today" survives a restart.
- An always-on spend strip and a Cost pane in abctl.

Out:

- **Per-session aggregation is designed but not lit.** Laptop traffic all lands under session
  `"default"` (`authlib/session/store.go:20`); rekeying is driven by an A2A `contextId` that
  Claude Code never sends. `GroupSession` and the session field names land here; they become
  meaningful when [#949](https://github.com/rossoctl/cortex/issues/949) does. No schema work is
  deferred.
- Latency percentiles (#951). The strip and Cost pane show the mean the aggregator already
  computes.
- The central collector (#898). The ledger's field names are chosen to be its wire shape, but
  no collector is built.
- Off-allowlist inference blindness: traffic the parser does not recognise produces no facts,
  so no cost, and is not counted as a gap. Filed separately; phase 2's header-only path narrows
  it.

## Architecture

One rule: **exactly one component turns tokens into dollars, and it is the one that already
knows when the token counts are final.** Everything downstream reads a settled figure. The
strip, the row and `/v1/usage` then agree by construction rather than by discipline.

This is #972's design, adopted whole. The four sites it names — `litellm_budgettrack`,
`usage.costOf`, `tui/cost_event.go`, `tui/prune_saving.go` — collapse to one.

**They collapsed to two.** `usage.Aggregator.costOf` survives and still resolves rates: it
prices any request that carries a model and tokens but arrives with no settled record. The
rule above is the design this work aimed at and the reason the rest of the shape is what it
is, so it stays stated as a rule rather than softened — but a reader must not take it as a
description of the branch. `authlib/usage/usage.go`'s `eventCost` doc, `sessionapi/usage.go`'s
handler doc and `costledger/row.go`'s divergence paragraph each say so where the code lives,
and `row.go` enumerates the three ways the two sources can disagree about the same window.
Closing it properly means teaching `costOf` to stop pricing, which is a change to what a
deployment without `inference-parser` reports and therefore its own piece of work.

### Layers

| layer | job | where |
|---|---|---|
| parsers | wire → facts: tokens, model, **and the gateway's reported cost** | `inference-parser` |
| costing | facts × rates → one settled figure with provenance, once | `inference-parser`, response pass |
| fact producers | what changed, no rates | `tool-prune`, pipeline core, listener |
| consumers | act on the number | budget-track enforces; usage records; ledger persists; abctl displays |

`inference-parser` is the costing owner rather than the pipeline runtime for three reasons:

- **The enablement failure mode collapses.** No parser → no usage → nothing to price. A row
  with tokens and no money becomes impossible, because one component produces both.
- **The ordering machinery already exists and is already used this way.**
  `litellm-budget-track` declares `RequiresLater: []string{"inference-parser"}`
  (`plugin.go:157`), and `plugins/registry.go:342-360` documents `Requires` / `RequiresLater`
  as a hard AND with ordering — the pipeline refuses to build if the named plugin is absent.
  "Budget-track without a parser" is already a config error, not a case to design around.
- **Only the parser knows when usage is final.** It owns
  `OnResponseFrame(…, last bool)` and already hosts the #811 `message_delta` prompt-token
  correction. Costing anywhere else duplicates or races that assembly.

### Fact producers lose their rates

`tool-prune`'s job is reducing tokens, not accounting for money. It currently resolves rates and
publishes them so a consumer can finish the arithmetic (`plugins/toolprune/event.go:12-17`),
because the dollar amount depends on which prompt-cache tier the saving came out of — 1×, 1.25×
or 0.1× of the same rate — and that is only known from the response.

That is an argument for moving the money step, not for keeping rates in the plugin. After this
change tool-prune publishes facts only — tools removed, byte delta, `Projected` — and loses its
`pricing.Resolver` and the nil-guard that resolver required. (That nil interface is what
silently stopped pruning during #968.)

The pipeline core already records `bodyMutationEvent{Phase, Plugin, LengthBefore, LengthAfter}`
for every rewrite, attributed to `c.currentPlugin` (`pipeline/context.go:630-637`). So "how many
bytes did this plugin remove" is framework-owned data. The one case it cannot cover is
`on_error: observe`, where `SetBody` is never called and the saving is projected — that stays a
tool-prune fact.

This generalises savings attribution beyond tool-prune: any future body-shrinking plugin —
compaction, redaction, a context pruner — gets tokens and dollars from the core fact it already
emits, with no pricing dependency of its own.

### The event key names the concern, not the producer

`costevent.PluginName = "litellm-budget-track"` (`costevent/costevent.go:23`) is pinned by a test
asserting it equals `New().Name()` (`litellm_budgettrack/plugin_test.go:540`). That constant is
what makes moving the producer a breaking wire change — for live consumers and for events already
in session stores.

The codebase already has the better pattern and says why: `pipeline/context.go:637` publishes
`"body-mutation"+PluginEventSuffix` from the framework, which is not a plugin, because "a switch
of plugin names in a future refactor shouldn't break operators' dashboards."

**Decision:** `costevent.Key = "cost"`, with the legacy `"litellm-budget-track"` key read as a
fallback. The producer then moves with zero consumer changes.

This also fixes a conflation: `Decode` returns `false` — "no record" — when
`CostUSD == 0 && !Settled` (`costevent.go:102`). Presence and pricedness are different questions,
and once one record also carries savings, that behaviour would silently discard a saving on any
request it could not price — exactly the traffic where a savings figure matters most.

### The counterfactual is quarantined by type

```go
type Event struct {
    CostUSD                    float64  // real money
    Source, Provenance         string
    Settled                    bool
    DailyTotalUSD, DailyMaxUSD float64

    // NOT money. Nothing in here is spend.
    Avoided []Saving `json:"avoided,omitempty"`
}

type Saving struct {
    Component     string // attributed from the core body-mutation fact
    TokensAvoided int
    USD           float64
    Tier          string
    Projected     bool // observe mode: measured, never applied
    Estimated     bool // tokens from EstimateTokensFromBytes, not a counter
}
```

A nested, category-named container rather than well-named float siblings of `CostUSD`. More
counterfactuals are coming — compaction, cache-hit savings, "what a cheaper model would have
cost". As siblings the record becomes half-real and half-hypothetical and someone eventually
adds two fields that must never be added.

## Schema

#950 asks for "one documented field-name schema, shared by the TUI metrics view, unattended
workloads and the central collector." The rule: **reuse the names already on the wire event**, so
there is one vocabulary from parser to ledger to collector, rather than aggregate-side synonyms.

`usage.Counts` gains, all `omitempty`, all summed by the existing `Counts.Add`:

| field | meaning |
|---|---|
| `inputTokens` | uncached prompt tokens |
| `cacheReadTokens` | served from cache — priced ~0.1× |
| `cacheWriteTokens` | written to cache — priced ~1.25× |
| `outputTokens` | generated tokens |
| `reasoningTokens` | **a subset of `outputTokens`**, never added to it |
| `presentKinds` | OR-folded across the window |
| `avoidedTokens` | counterfactual tokens — **designed, not implemented** |
| `avoidedMicros` | counterfactual dollars, in millionths — **designed, not implemented** |
| `avoidedRequests` | requests that avoided something — **designed, not implemented** |
| `prunableRequests` | requests that **had a tool inventory to prune** — **designed, not implemented** |

**The last four do not exist.** They are in neither `usage.Counts` nor `Counts.Add` on this
branch, and no on-disk ledger row carries them — including the example row further down, which
still shows all four and is therefore a picture of the design rather than of a real line. The
savings work they belong to (commit 7 / #952) did not land, and the Cost pane records their
absence where a reader would look for it rather than drawing an `AVOIDED` section over nothing.
Nothing here needs revising when they arrive: `Counts.Add` sums whatever `Counts` holds, and the
ledger's `Row` embeds `Counts`, so the storage side follows for free. What is missing is the
aggregation, not the schema.

`tokens` stays as the sum — **except that it does not always, and the doc comment on `Counts`'
split block is where that is stated** (`Tokens` itself carries no godoc; the sentence "Tokens is
NOT always the sum of the fields below" sits on the four fields that follow it). `parsercommon.Fill` prefers the provider's `total_tokens` when one was
reported and falls back to summing the split otherwise, so a gateway reporting only a total
yields a non-zero `tokens` with every split field at 0. Old consumers do keep working, which was
the point of the sentence; what they cannot do is normalise a stacked bar against `tokens` and
expect the parts to add up. `presentKinds` is what makes the difference legible — no bits set
means nothing reported a breakdown at all, which is a different answer from a breakdown that was
genuinely zero.

Two entries need justifying:

- **`prunableRequests`** is deliberately the same idiom as the existing `priceableRequests` /
  `pricedRequests` pair (`usage.go:59-80`), whose godoc explains why `Requests` is the wrong
  denominator for coverage. It is what makes #952's "report zero honestly for agents with no tool
  inventory" expressible: prunable-minus-avoided is a real gap, `0 of 0 prunable` is an honest
  zero, and neither reads as the other.
- **`presentKinds` folded by OR** keeps "reported zero" ≠ "not exposed" true at aggregate level,
  not just per event. Without it a by-model table shows a blank `CACHE-WR` column and the reader
  cannot tell whether that model wrote no cache or the provider never reports it.

New groupings, added to the existing `byMethod` / `byStatus` / `byPlugin` accumulators:
`GroupModel`, `GroupEndpoint`, `GroupAgent`, `GroupSession`. Like the existing ones they are a
read-time choice, so history exists from before the operator selected that view.

Wall time (#950's third quantity) is not additive across buckets, so it does not go in `Counts`.
It goes on the session summary as first-event / last-event.

### Client identity

Nothing on a `SessionEvent` identifies the calling agent (`authlib/pipeline/session.go:75-149`);
`Identity` is the JWT subject and is nil on a laptop. No User-Agent is captured anywhere. So
#952's "report per agent" has no substrate.

The listener gains one fact:

```go
// SessionEvent
Client *EventClient // nil when the request carried no User-Agent

type EventClient struct {
    Name    string // "claude-code" | "opencode" | "codex" | "" when unrecognised
    Version string
    Raw     string // the UA verbatim, so an unrecognised client is still nameable
}
```

Claude Code sends `claude-cli/x.y.z`. This is a **display axis, not a security boundary** — a
User-Agent is client-controlled, and the spec says so where the field is defined so nobody later
builds authorisation on it. It also gives #941 / #942 / #943 (OpenCode, Codex, Bob detection) and
#949 one field instead of three inventions.

## Storage: the cost ledger

The 6h in-memory ring (`NumBuckets = 360`) dies on `abctl service restart`, so "what did today
cost" is not computable — and the v0.9.0 exit bar asks for numbers that persist.

`~/.cortex/cost/YYYY-MM-DD.jsonl`. Cortex already owns `~/.cortex/`
(`cmd_service.go:154`). One file per day makes retention a file deletion rather than a compaction
routine.

One line per **closed minute × (endpoint, model, agent, provenance)**, carrying the `Counts`
field names verbatim:

```json
{"at":"2026-09-13T09:14:00Z","endpoint":"litellm.example.com",
 "model":"claude-opus-5","agent":"claude-code/2.1.14","provenance":"gateway",
 "requests":12,"errors":0,"inputTokens":3080,"cacheReadTokens":2159000,
 "cacheWriteTokens":12000,"outputTokens":440,"presentKinds":15,
 "costMicros":41700,"pricedRequests":12,"priceableRequests":12,
 "avoidedTokens":4120,"avoidedMicros":2400,"avoidedRequests":12,
 "prunableRequests":12}
```

The last four keys are **not on a real row** — see the schema table above for why. A shipped line
is the same shape without them, plus `incompleteRequests` when any of the minute's figures is a
floor rather than an exact total, which is a field this design did not anticipate and which exists
so that caveat survives a restart instead of being the one thing a persisted total silently loses.

`provenance` is part of the row key, not a row field: a single minute can mix a gateway's own
figures with modelled ones, and one `provenance` per row would have to pick a winner. Keying on
it keeps `PricedBy` reconstructible from the ledger exactly as `/v1/usage` reports it today.

`agent` is `EventClient.Name` and `.Version` joined with `/` — one string, because it is a
grouping label rather than structured data at this layer.

**On disk an absent client is `""`, not `"unknown"`.** This design said `"unknown"` and the
code refused it: a durable file must not bake a display value into a column where it becomes
permanently indistinguishable from an agent that really called itself that. `"unknown"` is
`pipeline.UnknownClientLabel` and `costledger.labelFor` applies it at the query boundary, so
`group=agent` returns the same key whether the answer came from the ring or from disk. The
constant is exported for exactly that reason — two spellings would surface as two rows, each
holding half the unattributed spend.

Written by the **proxy**, not abctl, for the reason `authlib/usage`'s package doc already gives:
an aggregate built inside a client starts empty when that client connects, so two operators
watching the same traffic would see different histories.

Four properties:

- **The ring and the ledger never both own a minute.** The ledger holds only *closed* minutes.
  A restart mid-minute loses ≤60s of cost — stated in the docs, not hidden.

  **The ring does not supply the open minute, and no single function stitches.** Both halves of
  that sentence were wrong and the code corrects them. The open minute comes from the ledger's own
  `Writer.pending()`, for four reasons `costledger`'s package doc spells out — the ring prices
  independently, its request denominator counts non-inference traffic, it is only 6h deep so an
  idle night rotates the minute out, and reading it would make ownership a timing question rather
  than a provable boundary. And the work is split across three functions, not one:
  `Window(from, to)` stitches disk and open minute (and is what a reader calls), `Query(from, to)`
  is the disk half alone — its own doc says "Reads only what the Writer has flushed … Prefer
  `Window`" — and `Fold(rows, group)` does the grouping, which is why neither signature carries a
  `group` parameter. Splitting them is what makes "only closed minutes reach disk" directly
  testable.
- **Windows ≤6h read the ring; `today` and `7d` read the ledger.**

  **Not the same returned shape, and a client must branch.** A ledger-backed window returns exactly
  ONE bucket spanning the whole span rather than a series, and omits `unpricedBy` and `pricedBy` —
  `unpricedBy` needs the endpoint-and-model pair of requests that could *not* be priced, which a row
  carrying only its own labels cannot distinguish from a priced one of the same pair, and emitting
  one map without the other would read as "no pricing gaps here". `Snapshot.BucketSeconds` is the
  discriminator and its godoc says so in those words: "Reading this field is how a client tells the
  two apart." abctl duly branches — `applyTodayFigure` refuses the figure unless `snap.Window` is
  really `today`, because a proxy with no ledger answers `window=today` from the ring's maximum span
  and reports *that* as the window it served, so trusting the request would label six hours of spend
  as a day's.
- **No prompt content.** Hosts, model names, counts, dollars. Documented explicitly, because it
  is a new on-disk artifact.
- **On by default for `--local`, off in Kubernetes.** Writing files inside a pod is wrong and
  #898's collector is the right sink there. Retention 30 days, configurable. A headline figure
  that reads "cost unavailable" until an operator flips a flag defeats the point of the feature.

An active 8h day writes roughly 480 active minutes × a few label combinations ≈ 350 KB.

## API

`/v1/usage` changes are purely additive, so old clients keep working:

- the new `Counts` fields
- `group=model|endpoint|agent|session`
- `window=today|7d`, served from the ledger

## abctl

### The spend strip

One row, directly under the title bar, above every pane — **except the two pickers.**
`spendStripVisible` returns false on `paneNamespaces` and `panePods`: those run before a
connection exists, so there is no spend to report and nothing to put in the row. Note the
asymmetry the code documents at the reservation site: `layout()` reserves the row on HEIGHT
alone and is blind to the pane, because it runs only on `WindowSizeMsg` and a pane-aware
reservation would go stale on the next pane change. So a picker renders one row shorter than
it needs, which is invisible, rather than an events table rendering one row too long, which
pushes the footer off screen.

```
┌─ abctl ── localhost:9094 ── ● live ─────────────── [Sessions] Pipeline ─┐
  SPEND  $4.17 today    $1.12 /1h    ~$0.11/min    saved $0.24 (5.4%)
  ────────────────────────────────────────────────────────────────────────
   #  TIME       PHASE  MODEL             TOKENS           COST
  12  09:14:02   resp   claude-opus-5     38,402(−12.3k)   $0.2546(−$0.0037)
```

It costs one row unconditionally. That is the point: cost is read before the data, not found by
navigating to it.

**Degradation drops whole figures right to left, never clips a number** — #953 asks for "no
truncated numbers", and a strip that truncates mid-figure is worse than one that shows fewer:

```
80 cols  SPEND  $4.17 today    $1.12 /1h    ~$0.11/min   saved $0.24
64 cols  SPEND  $4.17 today    $1.12 /1h    ~$0.11/min
48 cols  SPEND  $4.17 today    $1.12 /1h
32 cols  SPEND  $4.17 today
```

Below 20 rows tall the strip gives its row back to the table.

**It does not fold into the title bar, and the headline is lost.** Below `spendStripMinHeight`
the strip is simply not drawn and the header stays a bare `styleTitle.Render(title)` with no
figure in it. The fold was designed and never built: carrying a figure into the title means
budgeting title width against a pane name and an endpoint that are already competing for it,
which is a second degradation ladder rather than a reuse of this one. What shipped is the honest
subset — the row is worth more to the table at that height than to the chrome — and the code's
own test comment was corrected to say the strip "yields its row". Recorded rather than deleted
because a reader who wants the headline on a 19-row terminal should find out that it was asked
for and not delivered, not that nobody thought of it.

"Today" means **local midnight to now**, in the machine's timezone. Stated because a laptop
crosses timezones and a UTC day would silently reset mid-afternoon.

**Never `$0.00` for an unknown cost.** `usage_render.go:347` already establishes this — a zero
cost and an unknown cost are different answers — and the strip honours it while naming the fix:

```
  SPEND  cost unavailable   no rates for api.example.com claude-opus-5   [$] to fix
  SPEND  $4.17 today   ⚠ 12 of 318 requests unpriced   saved $0.24
```

### The Cost pane

Key `$`, alias `C`. Every short key is taken: `c` is the events-pane column picker
(`keys.go:204`), and `? g G m w b s r l / p y e P q tab n f u` are all bound. `$` is
unambiguous for money.

Five stacked sections in one scroll:

```
  COST — all sessions — today                        [w]indow  [g]roup

  TOTAL   $4.17   318 requests   218.1M tokens   4.21s mean

  BY MODEL            REQ    INPUT   CACHE-RD  CACHE-WR    OUT     COST
  claude-opus-5       184     308k    215.9M      1.2M     44k   $3.82
  claude-haiku-4-5     91      41k      2.1M       88k     12k   $0.35
  (43 non-inference requests carry no model)

  WHERE THE MONEY GOES
    output       ████████████████████████          44%   $1.83
    cache-read   █████████████████                 31%   $1.29
    input        █████████                         17%   $0.71
    cache-write  ████                               8%   $0.34

  AVOIDED (not spend)
    tool-prune   412k tokens   $0.24   5.4% of prompt   est.

  COVERAGE  ⚠ 12 of 318 priceable requests unpriced
    api.example.com claude-opus-5    12    ← add a rate for this
    provenance: 71% gateway · 29% modelled (bundled)
```

`AVOIDED` is never in the same column as money, always carries its `est.` / `projected` marker,
and reads `$0.00` only for an agent that genuinely had an inventory to prune — never for one
that had none.

Two of #953's asks are requirements on this pane rather than side effects, so they are stated
here:

- **It updates live without disturbing the events pane.** The pane polls `/v1/usage` on the
  existing `usagePollInterval` chain and is redrawn from its own state; the events table's cursor,
  filter and scroll position are untouched. The generation-guard pattern in
  `usage_pane.go:64-79` — stale replies and stale ticks dropped by sequence number — is reused
  rather than reinvented, because a second poll chain that reschedules the first is exactly the
  bug that comment records.
- **It honours a persisted user configuration — which does not exist yet.** `events_columns.go:15`
  records that nothing in the TUI persists a selection today; it all lives in memory for the life
  of the process. So this bullet of #953 requires creating that store, not reading it. The
  smallest thing that satisfies it: `~/.cortex/abctl-ui.json`, following the existing
  `.cortex/claude-code-state.json` convention (`cmd_claudecode.go:68`), holding the Cost pane's
  window and grouping. Written on change, best-effort — a corrupt or unwritable file falls back to
  defaults and never blocks the UI.

  The events pane's column selection is the obvious second tenant of that file and is
  **deliberately not migrated here**: it would widen this PR's TUI surface for no cost-related
  gain. The file is where it should go when someone does it.

The existing Usage pane stays as it is. It gains the new groupings for free (they are read-time
choices on the same accumulators); adding a `cost` metric to its `[t]` cycle is a natural
follow-on but is not required by this work.

### Other surfaces

| surface | change |
|---|---|
| sessions pane | gains `COST`. **No wall-time column shipped** — see below |
| events pane | `COST` populated on a default install — the settled figure, no client arithmetic |
| help overlay | new `$` entry. `TestPaneKeysCoverAllPanes` fails until it is documented, which is how `paneUsage` shipped reachable but invisible |
| `abctl cost [--json]` | non-TUI one-liner over `/v1/usage`, **not over `Query`** — see below |

Two corrections to that table:

- **Wall time was tried and removed.** A `SPAN` column (`UpdatedAt - CreatedAt`) shipped and was
  taken out again: it measured *elapsed* time, so a session idle for an hour reported an hour,
  which made it nearly redundant with `UPDATED` two columns to its left — and it was always blank
  on cached-only rows and the first column the fitter dropped, so it cost width on every terminal
  and showed on few. The shipped columns are `ID / UPDATED / EVENTS / TOKENS / COST / ACTIVE`, and
  `UPDATED` is last-event age via `relTime`. The design quantity — wall time on the session summary
  as first-event / last-event, per the Schema section — is still the right shape; a column was the
  wrong surface for it.
- **`abctl cost` does not touch `Query`.** It goes over HTTP, through
  `apiclient.GetUsageWindow(ctx, window, …)`, exactly as the TUI does. That is better than the
  design said and is the reason to note it: the CLI and the pane agree by construction because they
  ask the same server the same question, where a CLI reading the ledger directly would have been a
  second answer that only works on the host holding the files.

## Savings, honestly

#952's hard requirement is "distinguish savings from cache effects so the two are not
double-counted".

**A pruned tool definition that would have been a cache read is worth ~0.1× what the same tokens
are worth as fresh input.** Pricing a saving at the input rate overstates it by up to 10×. So
every `Saving` carries its `Tier`, attributed where the tier mix is known — the response — which
is the same reason costing lives in the parser. A cache-tier saving then renders visibly an order
of magnitude smaller instead of being silently inflated.

Two debts are stated rather than papered over:

- `pricing.EstimateTokensFromBytes` calibrates `promptTokens ÷ bodyBytesAfter`
  (`tui/prune_saving.go:97`). Sound for homogeneous JSON, wrong when the removed span had a
  different token density than what remained. It gets quoted in dollars, so it carries
  `Estimated` and the caveat travels with the function. Attributing a whole saving to a single
  tier (`prune_saving.go:104-112`) is likewise a deliberate simplification that moves with it.
- Pruning changes the prompt prefix, which can *invalidate* a cache that would otherwise have
  been read — a "saving" that costs more. We cannot measure that today. `Estimated` covers it and
  the documentation says so.

## Testing

- **Default-install regression** — a pipeline of `inference-parser` plus `tool-prune` with
  `remove: []`; assert the response event carries a cost. This is the test whose absence let the
  blank `COST` column ship.
- **Counterfactual invariant** — `Totals.CostMicros` and `PricedRequests` are unchanged by any
  number of `Avoided` entries. *Written: `usage/pricing_test.go`'s
  `TestAggregator_TotalsAreInvariantToAvoidedCost` records the same request twice, once with an
  absurd `Avoided` payload, and asserts every money field is identical. An earlier revision of
  this line called it vacuous and unwritten; the container exists on `costevent.Event`, so a
  consumer folding it into spend is a real thing the test forbids.*
- **One owner** — one golden request; the row figure, the `/v1/usage` total and the ledger line
  quote the same `costMicros`.
- **`presentKinds` fold** — a model that wrote no cache renders differently from a provider that
  does not report cache.
- **Honest zero** — an agent with no tool inventory yields `prunableRequests: 0` and renders `—`,
  not `$0.00`. *Unwritten: `prunableRequests` does not exist (see Schema). The rule it tests is
  covered instead by `usage.Counts.PricedRequests` against `PriceableRequests`, which the strip and
  the Cost pane both render as a coverage gap rather than as a zero.*
- **Restart continuity** — "today" is continuous across `abctl service restart`.
- **Strip width** — golden renders at 80 / 64 / 48 / 32 columns assert no clipped number.
  `footer_fit_test.go` is the precedent.
- **Legacy key** — an event keyed `litellm-budget-track` still decodes.
- **Behaviour preservation for phase 2** — the record carries both the old and the new figure
  during the transition, so drift is a comparison rather than a regression.

## Delivery

One PR, seven signed commits, reviewable in order. Visible wins land early rather than all at the
end.

**What actually landed:** one PR, but more commits than seven, because several of these were
followed by their own fixes — the writer coming off the request path, the open minute being
supplied at all, a corrupt ledger line no longer discarding the rest of its day. Rows 1, 2 and 7
did not land in this form: 1 and 2 were superseded upstream (see the banner), and 7 — tool-prune's
resolver, `Avoided` end to end — did not land at all, which is why the `avoided*` fields are absent
and why the Cost pane has no `AVOIDED` section. Rows 3, 4, 5 and 6 map to `d3fb2ca7`, `b97dbc29` (abctl) / `003a30f8` (core),
`6c7c7fe0` (core) / `9f0e35b0` (abctl) and `6870ad3b`; the Cost pane is `c86507ca`, an eighth commit this table does not list.

| commit | content | effect |
|---|---|---|
| 1 | `costevent.Key = "cost"`, legacy fallback, presence ≠ pricedness | #972 phase 1; no behaviour change |
| 2 | costing moves into `inference-parser`, reads the gateway cost header; budget-track becomes budget-only | #972 phase 2; **rows show money on a default install** |
| 3 | `Counts` split, `presentKinds` fold, `GroupModel` / `GroupEndpoint`; `/v1/usage` additive | #950's core |
| 4 | header spend strip, sessions-pane `COST` and wall time | **cost always on screen** |
| 5 | cost ledger; headline becomes "today" and survives restart; `abctl cost --json` | exit-bar item 3 |
| 6 | `Client` from User-Agent, `GroupAgent` | unblocks #941 / #942 / #943 |
| 7 | tool-prune loses its resolver, `EstimateTokensFromBytes`, `Avoided` end to end, Cost pane on `$`, `~/.cortex/abctl-ui.json` | #972 phase 3 = **#952**; #953 |

If the review proves unwieldy, the natural fracture is after commit 2 — the plumbing that fixes
blank rows is independently valuable and independently testable.

## Risks

- **Phase 2 touches the streaming response path**, where #926 (early flush of a leading thinking
  block, a P0) also lives. These must not collide. Mitigation is the both-figures transition
  above.
- **An estimate quoted in dollars is the reputational risk.** `Estimated` and `Projected` are
  surfaced in the UI, not merely carried in the struct, so an estimate of an unapplied prune
  cannot read like measured spend.
- **A new on-disk artifact** brings retention, rotation and schema-evolution duties. Per-day
  files and additive-only fields keep both cheap.

## Non-goals

- No change to what any request is charged. Every commit is behaviour-preserving for spend
  totals; the tests that prove it are named above.
- Tool-prune's savings stay a counterfactual, never folded into the ledger's or usage's cost
  totals.
- The strip is not configurable away in this work. Cost being unconditionally visible is the
  feature.

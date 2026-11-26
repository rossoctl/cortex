# Cortex as an `accountant` host: savings in the ledger and in the central Prometheus

**Status:** draft for review.
**Date:** 2026-11-17.
**Method documents:** the `accountant` repository (`github.com/rossoctl/accountant`): `docs/spec.md`,
`docs/design.md`, `docs/cortex-wiring.md` (W-sections), `docs/metrics.md`. This document does not
restate the method. It says what Cortex builds, in which order, and why.
**Issues:** rossoctl/cortex#1417 (the Cortex questions), rossoctl/rossoctl#2649 (roll-up).
**Not in the merged tree:** the development harnesses this document names
(`scripts/dev/compare-contextguru-savings.py`, `scripts/dev/integration-contextguru-litellm.py`,
`scripts/dev/verify-contextguru-keepalive.sh`), `docs/accounting-differences.json` and the
implementation plan. They were used while the work was built and were removed before merge.
The checks that stay are the Go tests in section 6.

## 1. Intent

Cortex must compute context savings with the same rules as the context-guru proxy, and report them
to the same central Prometheus.

- **Consistent savings.** Today the two hosts compute savings with different rules. The proxy
  prices every change with `accountant`, which is cache-aware and per tier. Cortex prices only
  tool-prune, with a byte-ratio estimate in one tier (`core/cost/settle/avoided.go`), and records
  nothing for context-guru. The same session can show different figures in each host. After this
  work, both hosts call one library with the same inputs, so the figures can differ only by a
  difference that is written down.
- **Independent of context-guru.** Accounting is a Cortex feature, not a context-guru feature. It
  works for a pipeline with tool-prune only, or with no context operator at all, and it works in a
  build that does not link the context-guru plugin. context-guru is one context operator among
  others.
- **Central reporting.** The proxy pushes the `accountant/metrics` counters (`llm.savings.*`) to the
  central collector. Cortex sends no metrics. The collector already has a Cortex receiver (`:4318`,
  nginx path `/cortex/`) that sets `proxy="cortex"`. After this work, Cortex pushes the same
  counters with the same names and labels.

The user chose this order: the ledger switches first, and the metrics come after. Then the ledger
(and agentop) and Prometheus come from one `accountant.Result` and one rate table, and they cannot
disagree.

## 2. Goals, non-goals, anti-goals

**Goals**

1. G1. Cortex hosts `accountant` the way the proxy does: one `Trackers` per process, `BeginRequest`
   with the client body, `Add` for each operation, `EndRequest` with the forwarded body and cache
   facts, and `Compute` with the billed usage.
2. G2. context-guru and tool-prune report their changes as `accountant.Operation`s.
3. G3. `settle` writes the `accountant` figure into `event.Saving`, the ledger and `/v1/usage`.
4. G4. Cortex pushes `llm.savings.*` to the central collector, from the same `Result`.
5. G5. `core/accounting` imports `accountant`, the standard library and Cortex packages only. It
   does not import context-guru. A test enforces this (section 5.2).
6. G6. The figures match the proxy's on the same traffic. The shared `accountant` test cases and a
   cross-host test prove it, because the two hosts share no host code (section 6).
7. G7. Keep-alive comes in this PR, taken from PR #1412. Each ping's spend reaches the ledger, and
   its rescue is credited on the next request, through `accountant` (section 5.8).

**Non-goals (later work, listed in section 9)**

- Detached model calls. context-guru has no `OpsEmitter` yet.
- Spend operations for `ibac`, `sparc` and other plugins that call a model.
- agentop's per-change view (W9 step 11).
- `llm.inference.*` and `llm_cache_miss_*` metrics. The central rules read them, but they are not
  savings. They belong to the session-event export design
  (`docs/superpowers/specs/2026-11-13-session-event-export-design.md`).

**Anti-goals: these make the work a failure even if it runs**

- A silent drift from the proxy. Cortex has its own host code (body parsing, breakpoints, cache
  facts), so a difference from the proxy is possible. Each one must be caught by a test, or be
  listed in `docs/accounting-differences.json` with a reason. A difference that is neither is a bug.
- Accounting that needs context-guru to run.
- Added latency on the request path for the hashing. The proxy hashes off the request path, and
  Cortex must do the same.
- A failure in accounting that fails a request. Accounting fails open: a request is always
  forwarded and answered.
- An API-equivalent dollar in a money total. Only `Basis = usd` enters a money total (section 5.7a).
- Two numbers for one thing. After the switch, the ledger, `/v1/usage`, agentop and Prometheus all
  read one `Result`.

## 3. Constraints

- **Versions.**
  - Cortex `main` builds against context-guru v0.7.3. The keeper from PR #1412 needs
    `keepalive.Keeper`, `dispatch.Dispatcher` and related APIs. These are in the released tag
    **v0.7.4-beta.1**. Checked on 2026-11-17: PR #1412's plugin builds against v0.7.4-beta.1 with
    no local `replace`. One plugin test fails there,
    `TestCortexSubstitutesCacheAwareSummarizerForAPing` (`SummarySubstituted = 0, want 1`). The
    PR must fix or explain it. The PR requires v0.7.4-beta.1, or a later v0.7.4 if one is tagged
    before merge.
  - context-guru pins `accountant` v0.1.2. The latest tag is v0.2.0, and its API change is only
    additive, so context-guru compiles against v0.2.0 under Go's minimum version selection. Cortex
    requires v0.3.0, the first tag with `accountant.BillingFrom` (all five modules, including
    `scripts/readme-demo`).
  - The `accountant/metrics` module has no tag. Cortex pins a pseudo-version, as context-guru does.
- **One change to another repo, decided by the user:** `accountant.BillingFrom`, in a new
  `accountant` release (v0.3.0). Nothing else in this design changes another repo.
- **`core` is consumed outside this repo.** A new exported API in `core/pipeline` is a contract. Keep
  it small.
- **The client body is not kept today.** `pctx.Body` is the forwarded body. The framework keeps only
  the client body's length and sha256 (`core/pipeline/context.go:1136-1147`).
- **`settle` runs on the request goroutine in the response pass**, from
  `InferenceParser.settleCost` (`core/plugins/inferenceparser/cost.go:65`). It runs before the
  session Recorders (ledger writer, usage aggregator).
- **Cortex's rate table has no 1h cache-write tier** (`core/cost/pricing/rates.go:23-33`), and its
  usage has no 1h split (`pricing.Usage`, `InferenceExtension`).
- **The `metrics.Pricer` signature is `Price(model, at, tier)`.** Cortex's `pricing.Resolver` also
  needs the endpoint and the prompt total.
- **`BillingFrom` does not exist** in `accountant`. The proxy hard-codes `BillingAPI`.
- **Three listeners:** forward proxy, reverse proxy and ext_proc. The plumbing must not be specific
  to one listener.
- **No OTel `MeterProvider` exists in Cortex.**

## 4. Approaches

These are the approaches I considered. The user picked **A**.

- **A. Ledger first, then metrics (picked).** Follow W9 steps 7 to 9: plumbing, shadow, switch.
  Then feed the same `Result` to an `accountant/metrics` sink and push it over OTLP.
  The ledger and Prometheus come from one `Result` and one `Pricer`. The cost is that metrics
  arrive after the ledger switch.
- **B. Metrics first.** Add the Tracker and the push, and keep `settle` on its current figure.
  Central data arrives sooner, but Prometheus and the ledger disagree until the switch. This is the
  inconsistency that the user wants to remove.
- **C. Accounting inside the context-guru plugin only (dropped).** It does not cover tool-prune or
  spend calls, so it is not consistent.

## 5. Design

### 5.1 Overview

```
request pass                                   response pass (settle)           after
------------                                   ----------------------           -----
client body ──► acct.Begin (keep bytes)
plugins run; each calls pctx.ReportOps(ops...)
forwarded body ──► acct.End  ─────────► goroutine: parse, hash, BeginRequest,
                                                   Add..., EndRequest  ──► done ch
                                       usage known ──► wait done ──► Compute(Usage)
                                                       Result ──► event.Saving (ledger)
                                                              └─► metrics.Observe ──► push
```

The new code lives in one new package tree, `core/accounting`. `core/pipeline` gets only a small
surface.

### 5.2 Host code owned by Cortex

The proxy's host code is unexported, inside context-guru's `proxy` package. Cortex does not import
it, and it does not need a new package in context-guru. `core/accounting` has its own host code. It
follows the host obligations in `accountant/docs/cortex-wiring.md` W6, the same rules the proxy
follows. The parts are listed below.

| Part | What it does | Rule it follows |
|---|---|---|
| `wire.Parse(raw) (accountant.Body, []accountant.Breakpoint)` | Splits an Anthropic Messages or OpenAI Chat / Responses body into tools, system and messages, with one `[]byte` per position. Reads the `cache_control` breakpoints. Builds the Body with `accountant.NewBody`. | W6.3: hash without `cache_control`, wire-byte sizes, one breakpoint per position, and 1h wins. Reuse `core/parse/inference` to detect the format. |
| `facts.Provider(host, path)` and `facts.For(provider, model, forwarded, at)` | Classifies the provider from the destination host and path. Returns `accountant.EntryFacts`: TTL, lifetime, `MinCacheable`, `TTLs`, `ReadGranularity`, `StorageBilled`. | `accountant/docs/cached-prefix.md`. The values are a Cortex table in `core/accounting/facts`, and each row cites its source. |
| `skip.Reason(...)` | Says when not to call `Compute`: non-2xx, more than one round, no session, a server-held Responses history, an unplaced deferral, or a panic. A translated request (`pctx.TranslateTarget()` ok) gets no record either, but that check runs earlier, in `Host.RequestEnd` itself, not through `SkipReason` — the listener only converts the body to the target protocol after `Run` returns, so a record built from `RequestEnd` would classify and price it under the wrong protocol's rules. | W6.1. |
| `thread.Of(pctx)` | Gives the `accountant.Thread`. | W5.2. See section 5.4. |
| Setting ops from the framework | A model change through `pctx.SetRequestModel` (inference-router) becomes a `KindSetting` op built by the framework. A plugin does not need to report it. | W6.4, envelope changes. |

**Boundary test.** A test in `core/accounting` runs `go list -deps` and fails if any package under
`github.com/rossoctl/context-guru` appears, as `accountant`'s `boundary_test.go` does for its own
module. The context-guru plugin depends on `core/accounting`. `core/accounting` never depends on
the plugin.

**What keeps the two hosts consistent.** They share the library and the rules, not code. Section 6
lists the tests that hold them together. Where Cortex knowingly differs, the difference goes in
`docs/accounting-differences.json`, as accountant testing §10.4 requires of every host.

**Usage normalization is already right.** `accountant.Usage.Input` must be *uncached* input. Cortex
already subtracts cached tokens for OpenAI Chat and Responses (`core/parse/inference/inference.go:579`,
`responses.go:294`), and Anthropic reports uncached input natively. `core/accounting` maps
`InferenceExtension` to `accountant.Usage` directly. The cross-host test (section 6) includes one
OpenAI fixture so that a regression here is caught.

### 5.3 When accounting is on

Add a top-level runtime config block:

```yaml
accounting:
  enabled: auto     # auto | true | false
```

- `auto`, the default: accounting is on when the outbound pipeline has at least one context operator
  (`context-guru` or `tool-prune`). Otherwise it is off. A pipeline with no operator has nothing to
  account for, so it pays nothing: no client body is kept, and no ext_proc buffering changes.
- `true` forces it on. Use this to get a `Result` for the cache figures alone.
- `false` turns it off.
- The block is the same in a cluster and on a laptop. Only the snapshot (section 5.4) is laptop-only.
- A hot reload that changes whether accounting is on is refused, as for `mtls.*`. The reason: the
  listeners decide at boot whether they keep the client body.
- With accounting off, `settle` keeps today's `avoided.go` path until commit 6. After commit 6,
  accounting off means no savings are recorded.
- This is my call.

### 5.3a Pipeline surface (`core/pipeline`)

1. Add `pctx.ReportOps(ops ...accountant.Operation)`. It appends to a per-request slice in pipeline
   order. The framework sets `Operator` to the plugin's name when it is empty, and sets `DryRun`
   when the plugin runs under `on_error: observe` (W5.1). A reverted write (where `SetBody` returned
   false) reports no op, and the plugin is responsible for that.
   - So that `core/pipeline` has no import of `accountant`, the slice holds `any`, and
     `core/accounting` asserts the type. *Builder's call:* importing `accountant` directly is also
     acceptable, because it is stdlib-only (its `boundary_test.go` enforces that).
2. Keep the client body. When accounting is on, the listener stores the request body as received
   before the first plugin, as `pctx.ClientBody()`. It stores the slice, not a copy. `SetBody`
   replaces the slice, so the original slice stays valid. Add a test that no mutator writes into
   the original slice. If accounting is off, nothing is kept.
3. Add a hook after the request pass: `Accounting.End(pctx)`. Each listener calls it once, after the
   outbound request pass and before the dial. It must return at once, because the work runs on a
   goroutine.

### 5.4 `core/accounting`

- `Host` holds one `*accountant.Trackers`, the host code from section 5.2, the pricer (section
  5.6) and an optional `metrics.Observer`.
- `End(pctx)` captures the client body, the forwarded body, the ops, the session, the thread, the
  model, the provider and the request ID. It then starts a goroutine with a `recover`. The goroutine
  does the parse, `BeginRequest`, `Add` for each op, and `EndRequest`, then closes a done channel.
  It stores a handle under a pctx state key.
- `Compute(pctx, usage)` is called from `settle`. It waits on the done channel, with a bound of 2s,
  and calls `Compute` under a `recover`. A timeout or a panic gives no `Result`, and the request is
  recorded as unpriced (fail open).
- The W6.1 skip rules come from `skip.Reason` (section 5.2).
- **Thread.** `core/accounting` sets the thread itself: `"main"` for the primary agent, and
  `pctx.Subagent()` for a subagent, with `Parent` = `pctx.ParentSubagent()`. This works with no
  context-guru plugin. If the context-guru plugin ran, it may set a thread through
  `pctx.SetAccountingThread(id, parent)`, which overrides the default. That keeps context-guru's
  own thread split where it exists. A difference between the two thread rules goes in the
  differences file. *Builder's call:* the exact setter name.
- **Client compaction (R55).** If the context-guru plugin ran, it calls
  `pctx.SetClientCompaction(accountant.CompactionUnknown)` on the first request after a client
  compaction: the request that holds the summary (`thread.Result.AfterCompaction`). The host copies
  it into `RequestMeta.Compaction`, and the accountant lists the changes that ended in
  `Result.Compaction`. The verdict is always `unknown`, because the wire does not tell `/compact`
  from an auto compaction. With no context-guru plugin, the host reports nothing.
  `Host.Savings` copies `Result.Compaction` into `Extras.Compaction`, and the inference parser
  writes it on the cost record as `event.Event.Compaction`
  (`"compaction":{"trigger":"unknown","ended":[<ChangeID>...]}`). A compaction alone is a reason to
  publish a record. No figure and no total reads it.
- **Facts.** `facts.Provider` and `facts.For` (section 5.2). The host is the destination host after
  any redirect, because that is the server that billed.
- **The billed tier (#2136).** `facts.For` sets the TTL from the marks, before the response. A
  gateway can bill a 1h ask as 5m. So `Compute` checks the ask against the bill before it calls the
  library's `Compute`, for a provider with an exact lifetime (Anthropic):
  1. If the response wrote to the cache, its tier is 1h only when `CacheWrite1h >= CacheWrite`. The
     thread remembers that tier, in its `threadBilling` entry.
  2. If the response wrote nothing, the thread's remembered tier applies.
  3. If the thread has no remembered tier, the asked tier applies.
  4. If the ask was 1h and the tier is not 1h, call `Request.LowerTTL(5 * time.Minute)` and count
     `llm.cache.ttl_downgrades`.

  This is context-guru's rule (`accrec.BilledTTL`) and the accountant's W6.6. The tier is not in the
  snapshot: after a restart, the asked tier applies until the thread's next write.
- **Persistence (laptop only).** Write a `Snapshot()` every 60s to `~/.cortex/accountant-state.json`
  (mode 0600). `Restore` it at startup. Refuse a snapshot older than 1h, as the proxy does. The
  reason: no provider cache lives longer than 1h, so older state can only price a cache that has
  expired. In a cluster there is no snapshot, and a pod restart starts fresh (design §11 Q13).
- **Memory.** The library limits `Trackers` to 10,000 threads. The host keeps one `threadBilling`
  entry per thread (billing mode and billed tier), with the same bound.

### 5.5 Context operators

Every context operator reports through `pctx.ReportOps`. Accounting works with any set of them,
including none.

- **context-guru plugin** (`core/plugins/contextguru`). It is optional.
  1. Change `apply.BodyWithModel` to `apply.BodyOpts`. Then `Result.Trace` reaches the plugin, and
     `Opts.ThreadOf` can be set.
  2. Call `pctx.ReportOps(res.Trace.Ops...)`.
  3. Set the thread from `res.Trace.Thread` with `pctx.SetAccountingThread` (section 5.4).
     On the first request after a client compaction, call `pctx.SetClientCompaction` (section 5.4).
  4. Wrap `cgModel`. Each in-request model call adds a `KindSpend` op with `Call` = `ModelCall{Host,
     Model, Usage, Detached:false}` and an empty `ChangeID` (W4.6).
  - context-guru reports a replayed change on every turn under the same `ChangeID`, and the library
    prices it once. Cortex does nothing special for replays.
- **tool-prune** (`core/plugins/toolprune`). The plugin reports one `KindRewrite` per removed tool:
  - `At` = the tools section, at that tool's index in the client body.
  - `SizeBefore` = the tool declaration's wire bytes. `SizeAfter` = 0.
  - `ChangeID` = `tool-prune:<tool name>:<first 6 bytes of sha256(declaration), as hex>`.
  - `DryRun` = not applied.
  - The plugin reports the op on every turn, and the library prices it once, the same as for
    context-guru.
  - The current `pruneEvent` stays until commit 6 (section 7).

### 5.6 Pricing

- **One rate table.** `settle` and the metrics sink both price `Result` tokens with Cortex's
  `pricing.Resolver`. That is W7.1, and it is answer 1 of cortex#1417.
- **One pricing function.** `core/accounting/price` has one function,
  `USD(resolver, endpoint, model, promptTotal, tier, tokens) (usd float64, estimated bool)`.
  `settle` calls it for the ledger, and the metrics pricer calls it for Prometheus. The two outputs
  cannot price one token differently.
  - `estimated` = provenance is `ProvBundled` or `ProvNone`.
- **The `metrics.Pricer` signature.** It is `Price(model, at, tier)`, with no endpoint and no prompt
  total. Cortex does not change `accountant`. Instead, the Cortex pricer is bound to one request:
  1. `core/accounting` holds one mutex for the sink.
  2. Under the mutex, it sets the request's endpoint and prompt total on the pricer.
  3. It calls `Observe`, and then releases the mutex.
  - `Observe` only adds to in-memory counters (`accountant/docs/metrics.md`), so this lock costs
    microseconds.
  - The call runs after `settle`, off the response path's critical part. A per-observation pricer
    in `accountant/metrics` would remove the lock. That is optional later work, not a dependency.
- **The 1h tier.**
  - Add `TierCacheWrite1h` to `pricing.Rates`. For a provider with no listed 1h rate, use the 5m
    write rate plus the provider's 1h premium where it is known (Anthropic: 2x input). Otherwise
    price at the 5m rate and mark the figure estimated.
  - Parse `cache_creation.ephemeral_1h_input_tokens` into `InferenceExtension` as `CacheWrite1h`, a
    subset of `CacheWrite`, so that `accountant.Usage.CacheWrite1h` is filled.
  - This also corrects the current spend figure for 1h writes. That is a separate fix, and it gets
    its own commit (commit 4).

### 5.7 Billing mode: `BillingFrom`

**Decided by the user (2026-11-17, replaces the earlier `BillingAPI` decision):** the billing mode
comes from `accountant.BillingFrom(headers)`, built **in the `accountant` repository** as
accountant design §7.1 says, so that Cortex and the proxy can share one rule.

- **The function** (new in `accountant`, released as v0.3.0):
  `func BillingFrom(h http.Header) Billing`. It reads only the credential **type**, and it never
  returns or logs any part of the credential (accountant R2).
  - `x-api-key` present → `api`.
  - `Authorization: Bearer <token>`: an Anthropic API key (`sk-ant-api…`) or an OpenAI API key
    (`sk-…`, not a JWT) → `api`. An Anthropic OAuth token from a subscription login
    (`sk-ant-oat…`) or a ChatGPT login token (a JWT) → `subscription`.
  - Anything else, or no credential → `unknown`.
  - The exact prefixes and their sources are the `accountant` change's to pin, with a test table.
- **Where Cortex reads it.** The **client's** credential header, as the request arrived, before any
  plugin rewrites it. token-exchange replaces `Authorization`, so the read happens in
  `RequestStart`, before the first plugin. The result is stored on the request, and the Host keeps
  the last mode for each thread, for keep-alive pings.
- **Pings** use the mode of the thread's last request, because a ping uses the same credential.
- **Until `accountant` v0.3.0 is tagged,** Cortex's `go.mod` points at the local `accountant`
  branch through a `replace` line marked dev-only. The PR cannot merge with that line.
- **The proxy** still hard-codes `BillingAPI` until it adopts `BillingFrom`. That is listed in
  `docs/accounting-differences.json`.
- **A contract test** in Cortex feeds captured headers to `BillingFrom` through the Host, and
  expects the same mode as the `accountant` test table.

### 5.7a Recording by billing basis (accountant design §7.1, in full)

**Decided by the user: full §7.1 recording.**

- `event.Saving` gains `Basis`: `usd` (mode `api`), `subscriptionEquivalent` (mode `subscription`)
  or `unknown`. So does each accountant spend record (a ping, or a context-guru model call).
- **Money totals count only `Basis = usd`.** These are `TotalAvoidedUSD`, the ledger's
  `AvoidedMicros` and `PenaltyMicros`, `/v1/usage`, and the sessions list.
- **A subscription saving is valued in usage-limit units:** tokens × the weights. The default
  weights are the API rate ratios: input 1, cache write 5m 1.25, cache write 1h 2, cache read 0.1,
  output 5. Every subscription figure is marked `Estimated`. The weights are configurable:

  ```yaml
  accounting:
    subscription_weights:   # optional; defaults shown
      input: 1
      cache_write_5m: 1.25
      cache_write_1h: 2
      cache_read: 0.1
      output: 5
  ```

- **The ledger** gains `AvoidedUnits` and `PenaltyUnits` (usage-limit units, as integer
  milli-units), and `AvoidedUnknownMicros`, for figures whose basis is `unknown`. `/v1/usage` and
  the sessions list carry the same fields.
- **agentop** shows a usage-limit column for subscription savings, and the API-equivalent dollar
  figure beside it, labelled "API equivalent". An `unknown` figure is shown beside the totals, never
  added to them.
- **Scope (my call):** §7.1 names `event.Saving` and `SpendRecord`, so `Basis` applies to savings
  and to accountant spends. Cortex's existing per-request spend figure (`CostUSD`) is unchanged.
  Moving subscription request spend out of money totals is a separate change.
- **Metrics:** `accountant/metrics` already moves dollar counters only for `api`. The token
  counters move for every mode.

### 5.8 Keep-alive

**What comes from PR #1412.** The keeper and its wiring in the context-guru plugin:
`keepalive.go`, `dispatcher.go`, `windows.go`, their tests, `keepalive_substitute_test.go`,
`build_test.go`, and `scripts/dev/verify-contextguru-keepalive.sh`. Also the commits they need to
build. The builder decides which of #1412's commits those are. Its pricing of pings
(`82cd72b4`), `core/cost/spend`, `Deps.Costs` and `spend.LogRecorder` are **not** taken.

**How a ping is accounted.**

1. The keeper reports each ping through `KeepAliveEmitter.KeepAlivePing(KeepAliveReport)`. The
   plugin implements that emitter.
2. The plugin converts the report to an `accountant.Ping`, using the field table in wiring W4.6, and
   calls `core/accounting`'s `Ping(session, client, ping)`. `KeepAliveReport` is a context-guru type,
   so the conversion stays in the plugin, and `core/accounting` sees only `accountant` types.
3. `core/accounting` does the following:
   - It refuses a ping with a zero window, a non-2xx status or zero usage, as the proxy does.
   - It calls `accountant.PingOps(ping, billing)`, with the billing mode of the thread's last request
     (section 5.7), then `ComputeStandalone`.
   - It keeps the `PingFacts` for that thread, unless the ping was manual, and applies them at the
     next `EndRequest` for the thread. Then it clears them.
   - It writes the ping's spend as a cost record (section 5.9).
4. The next request on the thread gets a `rescue` cache effect under the ping's `ChangeID`
   (`ka:<thread>:<window>`), so the benefit is credited where it lands.

**A prefix that a sibling kept warm (accountant R54, #2138).** Subagents of one session share
`system` and `tools`. A request of one subagent refreshes that cache entry for all of them, so a
ping does not get credit for it.

- `core/accounting` keeps each `(session, thread)`'s last request: its time (`RequestMeta.At`),
  its forwarded `Body` and its breakpoints. Hashes and sizes only (R2). `sibling.go` holds it.
- At `EndRequest`, if the request carries ping facts, it sets `EntryFacts.SharedWarm` to
  `accountant.SharedWarmFrom(forwarded, siblings)`. A sibling is another thread of the session, on the
  same model, whose last request was not after this one and was inside the TTL before it. The
  provider caches each model apart. If either model is unknown, the thread counts. The rescue leaves that
  prefix out.
- The store has the same limits as `threadBilling`: 10,000 entries, trimmed to 9,000 by age. A
  session keeps at most 64 threads; past that it drops its oldest, which can still be warm under a
  1h TTL. The store is not in the snapshot.
- A sibling that is missed (its record has not ended yet, or the process restarted) overstates the
  rescue. A sibling whose request failed understates it. Both are accepted.
- Not covered: a ping on a sibling thread also refreshes the shared prefix, and the store does not
  record pings. Neither does R54 cover the `ttlRescue` of a head held at 1h.

**The standalone record path.** A ping is not a proxied request, so no `settle` runs for it.
`core/accounting` writes its record to the ledger and the usage aggregator directly, through a new
`Standalone(record)` method on the ledger writer. *Builder's call:* the method name, and whether
the usage aggregator gets its own entry point or a shared one.

This path follows the contract that #1412 wrote for `CostRecorder`:
- It is safe for concurrent use.
- It does not block on I/O.
- It never makes the session active. A ping is not agent activity, and counting it as activity
  would distort the idle tracking that keep-alive depends on.
- It may drop a record for a session that no longer exists.

The record has `Source: "keep-alive"`, so `/v1/usage` and agentop can show ping spend apart from
agent spend.

### 5.9 Event and ledger

The fields follow W7.1.

- **`event.Saving`.** One entry per operator, with these fields:
  - `Component` and `Operator`: the dotted name.
  - `USD`: Saved, priced.
  - `PenaltyUSD` (new): Caused, plus the net break cost attributed to the operator, priced.
  - `Tier`: the tier with the largest Saved.
  - `Projected`: from `DryRun`.
  - `Estimated`: set if any op or rate is estimated.
  - `OpsVersion` (new): `accountant` `Result.Version`.
  - New on `event.Event`: `UnattributedBreakUSD` and `Thread`. Leave `Changes []ChangeSaving` out
    until agentop's per-change view (non-goal).
- **Ledger.** `AvoidedMicros` stays gross saved, as today. Add `PenaltyMicros`. agentop and
  `/v1/usage` then show saved, penalty and net. The JSONL row gains one field, and older rows read
  it as 0.
  - *My call:* keep the gross column, and do not change its meaning. A net column would change what
    every existing row means.
- **Readers.** These must read the new figure:
  - `avoidedMicros` in the sessions list.
  - agentop `cost`, spend and the drawer.
  - `prune_saving.go`. It is hard-coded to `"tool-prune"`, so generalize it to any component.

### 5.10 Metrics push

1. **Sink.** Build `acmetrics.New(meter, pricer, acmetrics.WithComponents(...))` once.
   `core/accounting` calls `Observe` after `Compute`, with the same `Result`. The call is under a
   `recover`, and the sink makes no network calls.
2. **Push.** `core/accounting/push` builds an OTel SDK `MeterProvider` with a periodic reader and
   the `otlpmetrichttp` exporter, to `<url>/v1/metrics`. It does not use context-guru's
   `metricspush`. It matches what the central collector reads from the proxy:
   - The resource attributes are `service.instance.id` (a random machine ID kept in
     `~/.cortex/machine-id`, mode 0600), `service.version` (the Cortex version) and `service.name`
     `cortex`.
   - The temporality is cumulative.
   - The token goes in `Authorization: Bearer <token>`.
   - The URL must be https. http is allowed only for loopback.
   - A failed export logs a WARN at most once a minute and increments
     `llm.proxy.export_failures`. It never fails a request.
   - The collector's Cortex receiver (`:4318`, `/cortex/`) sets `proxy="cortex"`. Cortex does not
     send it.
3. **Config.** Add a top-level `metrics_push:` block to the runtime config:

   ```yaml
   metrics_push:
     enabled: true            # default true, but with no URL nothing is sent
     url: ""                  # no default; METRICS_PUSH_URL overrides
     token: ""                # METRICS_PUSH_TOKEN
     interval: 30s            # METRICS_PUSH_INTERVAL
   ```

   - With no URL, Cortex logs one line and pushes nothing.
   - The block is the same in a cluster and on a laptop.
   - A change to it requires a restart, as for `listener.*`.
   - `agentop setup` does not set a URL. The user sets it, so nothing leaves the machine without
     the user's choice.
4. **Labels.** The labels are whatever `acmetrics` emits, and the collector sets `proxy`. The
   series must pass the central label allow-list (context-guru `central_e2e_test.go:183`).
   Section 6 has a Cortex test that checks this without importing context-guru.

## 6. Verification

Each commit has a check that must pass before the next commit is written.

- **Cache cases.** Every `accountant/test/cases` case (`cases.All()`) plays through
  `core/accounting`'s Begin, Add, End and Compute path and its facts conversion. Every case
  must pass. No case is skipped or deferred. The cases hold bodies that are already parsed (`accountant.Body`), so they do
  not test `wire.Parse`.
- **Wire parse.** Golden fixtures in `core/accounting/wire/testdata`: Anthropic Messages, OpenAI
  Chat and OpenAI Responses bodies, with the expected positions, sizes and breakpoints. They cover
  the W6.3 rules: hashing without `cache_control`, wire-byte sizes, and one breakpoint per
  position with 1h winning. The proxy's `accParse` is the reference to port from. It is read, not
  imported. Its equality with the proxy is proved by the cross-host harness below.
- **Boundary.** The `go list -deps` test of section 5.2. A Cortex build without the context-guru
  plugin has accounting on, and the tool-prune saving still reaches the ledger.
- **Shadow agreement.** In commit 5, `settle` logs the old figure, the new figure and the difference,
  and counts them in a `pipeline.Metric`. Each difference on a captured laptop session matches a
  `differs` entry.
- **Cross-host.** For one captured session played through Cortex and through the proxy, the
  per-operator Saved and Caused tokens are equal, except for listed differences. The
  Cortex/proxy/control test from `62333ee9` on `feat/contextguru-keepalive` is the harness. Port it
  to `main` as part of commit 5. It runs the proxy as a separate binary, so Cortex imports nothing
  from context-guru for it. Include one OpenAI fixture.
- **Ledger.** Totals before and after the switch, on the same replay, differ only by the documented
  differences.
- **Keep-alive.** The K1 check of `compare-contextguru-savings.py`: during a freeze longer than the
  cache TTL, the Cortex arm pings, and its first request after the freeze reads from cache. The
  ledger has one `keep-alive` record per ping, and that request carries a `rescue` effect under the
  ping's `ChangeID`. `verify-contextguru-keepalive.sh` passes.
- **Metrics.** An e2e test pushes to a local OTLP HTTP receiver and asserts the series names,
  resource attributes and labels. The allow-list is copied into the test, with a comment that cites
  its source.
- **No added latency.** A benchmark of the request pass with accounting on and off. The difference
  must be within noise, because the hashing runs off the request path.

## 7. Delivery: one PR

The user wants the whole integration in one PR. The PR has one commit per step below, in this
order, so a reviewer can read it step by step and each commit builds and passes its tests.

| Commit | Change |
|---|---|
| 1 | `core/accounting` host code (section 5.2): `wire`, `facts`, `skip`, `thread`, the boundary test, and the `accountant` cases played through it. Require `accountant` v0.2.0. No callers yet. |
| 2 | Plumbing: the `accounting:` config, `pctx.ReportOps`, the client body and `Accounting.End` in all three listeners. tool-prune reports ops. `Compute` runs and is logged at debug level only. Persistence on the laptop. |
| 3 | The context-guru plugin reports ops, spends and its thread (section 5.5). The spend usage comes from `llmclient`'s `ChatUsage`, taken from PR #1412. |
| 3a | `BillingFrom` (section 5.7): the Host reads the client's credential type in `RequestStart`, sets `RequestMeta.Billing`, keeps the last mode per thread. Requires the local `accountant` branch with `BillingFrom` (dev-only `replace`). The `accountant` change itself is a separate branch in that repository. |
| 3b | Keep-alive (section 5.8): bump context-guru to v0.7.4-beta.1 or later, bring in the keeper from #1412, `core/accounting.Ping`, and the ledger's standalone record path. Fix or explain the failing substitute test. |
| 4 | The 1h tier: the rate and the usage parse. Spend figures for 1h writes change, and that is a fix. |
| 5 | `core/accounting/price` (section 5.6), the basis and the subscription weights (section 5.7a), and `settle` computes the `accountant` figure beside the old one and logs the difference. Port the cross-host harness (`compare-contextguru-savings.py` from PR #1412) and add `docs/accounting-differences.json`. |
| 6 | Switch: `settle` writes the `accountant` figure, with the new event fields, `PenaltyMicros`, and the usage-limit and unknown columns (section 5.7a). Update the agentop readers, reusing `rowSavingFor` from PR #1412 without its floor at zero. Remove `avoided.go`'s tool-prune path, `pruneEvent`'s pricing fields and `EstimateTokensFromBytes` if nothing else uses it. Remove the shadow log from commit 5. |
| 7 | Metrics: the sink, `core/accounting/push` and the `metrics_push` config. Update the docs (`docs/pricing.md`, `docs/laptop-service.md`, CLAUDE.md). |

**The shadow check is a development gate.** Before commit 6 is written, commit 5 runs on captured
laptop sessions and in the cross-host harness. Every difference it logs must match an entry in
`docs/accounting-differences.json`. The PR description reports the result. The shadow code does not
stay in the merged tree.

**From PR #1412**, only the parts named above are taken: the keeper (section 5.8), `ChatUsage`,
`PenaltyUSD`, `rowSavingFor` and the three scripts. Its Responses compaction and marker fix
(`6bbc0424`, `6db11237`) are taken only if the keeper needs them to build. After merge, #1412 is
closed as superseded. Its `core/cost/spend`, `Deps.Costs`, `saving.go`, `unique.go`, the context-guru
path in `avoided.go`, and its keep-alive pricer are replaced by this design and are not taken.

## 8. Risks

- **Drift from the proxy.** The two hosts have separate host code. A change to the proxy's parse or
  facts that the cases do not cover will not reach Cortex. Mitigation: the cases are the contract.
  If one host finds a gap, the fix is a new case in `accountant`, which both hosts then must pass.

- **Concurrency order.** `Compute` results depend on the order of calls on one thread (design §11
  Q14). The proxy does not order them, and Cortex does not either, so both hosts behave the same.
  If `accountant` later requires order, both hosts change.
- **The client body costs memory.** A body can be up to 32 MiB. It is held only until `End`'s
  goroutine finishes parsing it, not until the response.
- **ext_proc buffers bodies only when a plugin asks.** With accounting on, the inbound request body
  must be buffered. Check that this does not change streaming, because requests are never streamed.

## 9. Later work

2. `OpsEmitter` for detached calls, after context-guru adds it.
3. Spends for `ibac`, `sparc` and other plugins that call `llmclient`.
4. The proxy adopts `BillingFrom`.
7. Optional: a per-observation pricer in `accountant/metrics`, which removes the lock in section
   5.6.
5. agentop's per-change view and `Changes []ChangeSaving`.
6. `llm.inference.*` metrics.

## 10. Decisions

- The user picked approach A, ledger first and then metrics (section 4).
- `accountant` is integrated without context-guru. There is no shared host package (section 5.2).
- The whole integration is one PR, with ordered commits (section 7).
- Keep-alive is in the PR (section 5.8).
- Billing mode comes from `accountant.BillingFrom`, built in `accountant` (v0.3.0) (section 5.7).
- Full §7.1 recording by billing basis (section 5.7a).

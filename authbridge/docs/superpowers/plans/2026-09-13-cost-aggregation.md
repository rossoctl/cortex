# Cost Aggregation Implementation Plan (commit 3)

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

> **STATUS: implemented.** Landed as `d3fb2ca7`, plus four follow-ups — `5d004725`,
> `a6b356c4`, `e9baee2a` and `cf4ab175`.
>
> An earlier revision of this banner cited `cbe34bbc`, which is not an ancestor of this
> branch: it is the same change on an abandoned branch that was never merged. A banner
> pointing at an unreachable commit is worse than no banner, because a reviewer who
> looks it up concludes the doc is describing someone else's tree.
>
> **Superseded by later commits on this branch.** Nothing this commit built was undone;
> what was reversed is two of its *deferrals*, and one sentence it wrote about the
> aggregator was wrong. Each is noted again where this plan states the thing that
> changed:
>
> - `GroupSession` was deferred here and landed in `b97dbc29` (abctl) / `003a30f8` (core), which also added the
>   `bucket.bySession` accumulator this plan says was not needed.
> - `GroupAgent` was deferred here and landed in `6870ad3b`, along with `byAgent`.
> - `ParseGroup`'s error string therefore names two more values than the one this plan
>   prescribes.
>
> **Line numbers drift.** Every `file.go:NN` below was accurate when written; many have
> moved since — `fold` is at `snapshot.go:438`, not the `:159-210` cited under Global
> Constraints, and not the `:345` an earlier revision of this line gave. Read them as
> "roughly here" and find the symbol by name.
>
> ---
>
> **SECOND SWEEP, 2026-09-15.** Two more review rounds have landed since the four
> follow-ups above. Three things this plan *prescribes* are no longer what the tree says,
> all of them in Task 3's replacement godoc — which matters more than usual, because that
> task's whole deliverable is documentation, so a stale prescription here is a stale
> comment there:
>
> - **Costing is `authlib/costing`, not `inference-parser`.** `cf4ab175` moved the
>   attribution: a gateway's cost-header semantics are vendor-specific knowledge with no
>   place in a provider-shaped body parser, so the decision lives in its own package and
>   the parser *calls* it at the point the token counters are final. The paragraph this
>   task tells you to write credits the parser with owning the rule.
> - **"Cost used to be computed in four places" is two.** `costing`'s own account is
>   `litellm-budget-track` and the usage aggregator. Four was #972's count, which included
>   two client-side renderers of a figure someone else published.
> - **budget-track AMENDS the settled record**, it does not consume it. The distinction is
>   load-bearing: no settled record meant no enforcement, which is why a body-less priced
>   response escaped the budget as well as the chart.
>
> Also in that task, and in the same class as the `ParseGroup` note the banner already
> makes: **the `group` parameter line gained `session` and `agent`.** The shipped line is
> `none (default), model, endpoint, session, agent, status, plugin`. Adding a grouping
> without extending both that list and the disclosure block is the drift to watch for —
> the endpoint then answers 400 for a value it does not accept while failing to name one
> it does, and leaks an axis the block exists to make explicit.
>
> **`Counts` has gained a field since**, and the constraint that says every new one must be
> summed in `Add` held: `IncompleteRequests` (`4d0c1046`, `4e2b81d9`) counts the priced
> requests whose figure is a floor rather than an exact number, and `Add` sums it
> *alongside* `PricedRequests` rather than out of it, because it is a subset disclosure and
> not a deduction. `Snapshot` gained `IncompleteBy` (`acde75ae`), which says which *way*
> each of those figures is inexact, and `Degraded` (`1c714e68`), which a ledger-backed
> window uses to admit that its read lost rows. None of that contradicts this plan; it is
> listed so a reader does not take the field table below for the current one.
>
> One small mis-citation, in the `tokens` correction under Global Constraints: the sentence
> quoted there is real, but it is not on `Counts.Tokens`. `Tokens` carries no godoc of its
> own; "Tokens is NOT always the sum of the fields below" sits on the four split fields
> that follow it, which is where this plan's Task 1 Step 3 put them.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the usage aggregator carry the four-way token split and break totals down by model and by endpoint, so a cost table can say *what* the money went on rather than only *how much* there was.

**Architecture:** Three tasks, one commit. `usage.Counts` gains the split fields the session event has published since #811, plus an OR-folded `presentKinds` so "reported zero" stays distinguishable from "not exposed" at aggregate level. The `Group` enum gains `GroupModel` — which is a *rename* of the existing `byMethod` series, not a new map — and `GroupEndpoint`, which is genuinely new. `/v1/usage` accepts both and its documentation is corrected on two counts that this branch has already made false.

**Tech Stack:** Go 1.x, standard library `testing` (table-driven), `net/http/httptest` for the handler.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-13-cost-first-class-design.md`

## Global Constraints

- **Module root is `authbridge/`.** All Go commands run from `authbridge/`, relative to the repo root.
- **Work in an isolated checkout**, not the shared one: it may carry unrelated uncommitted edits under `authlib/pricing/`.
- **`git commit -s` is mandatory** (DCO; a `commit-msg` hook enforces it).
- **Attribution trailer is `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.** Never `Co-Authored-By` — the hook rejects it.
- **Field names come from the wire event, not invented here.** The spec's rule is one vocabulary end to end: `inputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `outputTokens`, `reasoningTokens`, `presentKinds` — exactly as `pipeline.InferenceExtension` spells them (`authlib/pipeline/extensions.go:184-195`).
- **`/v1/usage` changes must be purely additive.** Existing clients keep working: `tokens` keeps meaning what it meant, `group=method` keeps returning what it returns today.

  **"`tokens` stays as the sum" was the wrong way to say that, and `Counts.Tokens`' own godoc now says so:** "Tokens is NOT always the sum of the fields below." `parsercommon.Fill` prefers the provider's `total_tokens` when one was reported and only falls back to summing the split, so a gateway reporting a total with prompt and completion absent yields a non-zero `tokens` over an all-zero split. The additivity requirement is unaffected — no existing client's reading of `tokens` changed — but a *new* client normalising a stacked bar against it would be wrong on exactly those gateways. `presentKinds` is the field that keeps that legible: no bits set means nothing reported a breakdown at all, which is a different answer from a breakdown that was genuinely zero.
- **`reasoningTokens` is a SUBSET of `outputTokens`.** Never add the two.
- **Every new `Counts` field must be summed in `Counts.Add`.** `fold` (`snapshot.go:159-210`) and abctl's `(other)`-band collapse (`tui/usage_stacked.go:86,93`) both delegate to it, so a field omitted there is silently wrong at every resolution but the storage one. That exact bug already happened to `PricedRequests` and is why `Add` is exported.
- **The unauthenticated endpoint must not echo query input** into an error body (`sessionapi/usage.go:110-116`). `ParseGroup` deliberately names the valid set instead of quoting what it got; keep that.
- **Do not run `make lint`** — it fails on pre-existing errors elsewhere and rewrites unrelated files. Use `go vet` and `golangci-lint run --new-from-rev=main` on changed packages.
- **Do not run `gofmt -w .` at the module root.** Format only files you touched.
- **Known pre-existing failure on this machine:** `cmd/abctl`'s `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` fails for a TEST-ISOLATION BUG: the fixture is deliberately bundle-less, but the machine's own `~/.cortex/ca/bundle.crt` leaks through. It fails on `main` too. It is a REAL bug rather than an environment quirk, out of scope for this branch; do not let it mask a real failure.
- **Comment register:** long comments that explain *why*, naming the bug the code prevents. `authlib/usage/usage.go` is the model.

---

## File Structure

| file | responsibility | task |
|---|---|---|
| `authlib/usage/usage.go` | `Counts` fields + `Add`; `bucket.byEndpoint`; `foldInto` populates both | 1, 2 |
| `authlib/usage/usage_test.go` | split accumulation, reasoning-not-double-counted, `presentKinds` fold | 1 |
| `authlib/usage/snapshot.go` | `Group` consts, `ParseGroup`, `bucket.series` | 2 |
| `authlib/usage/snapshot_test.go` | grouping selection, `method` alias equivalence | 2 |
| `authlib/sessionapi/usage.go` | handler godoc corrections; no code change needed for groups | 3 |
| `authlib/sessionapi/usage_test.go` | handler accepts the new groups, rejects unknown without echoing | 3 |

---

## Task 1: Carry the token split into the aggregate

**Files:**
- Modify: `authlib/usage/usage.go:49-100` (`Counts` + `Add`), `:537-566` (`foldInto`), `:129-152` (`bucket` — no new field needed for this task)
- Test: `authlib/usage/usage_test.go`

**Interfaces:**
- Consumes: `pipeline.InferenceExtension`'s `InputTokens`, `CacheReadTokens`, `CacheWriteTokens`, `OutputTokens`, `ReasoningTokens`, `PresentKinds` (`authlib/pipeline/extensions.go:184-195`).
- Produces: `usage.Counts` fields `InputTokens`, `CacheReadTokens`, `CacheWriteTokens`, `OutputTokens`, `ReasoningTokens` (all `int64`) and `PresentKinds` (`uint8`). Task 2 relies on these flowing into every label map, which they do because `addLabel` takes a whole `Counts`.

**Why:** `usage.Counts` has carried a single scalar `Tokens` since it was written, while the session event has published the full split since #811. A cost total assembled from one number cannot say where the money went, and the split prices very differently — a cache read is ~0.1× uncached input and a cache write ~1.25×, so for a long-running agent, whose traffic is overwhelmingly cache reads, the shape of the split *is* the shape of the bill.

`PresentKinds` is the non-obvious one. Without it, a by-model table showing a blank `CACHE-WR` column cannot distinguish a model that wrote no cache from a provider that never reports cache writes. OR-folding preserves that distinction across a window: a bit set anywhere means some response in the window reported that kind.

- [ ] **Step 1: Write the failing test**

Append to `authlib/usage/usage_test.go`. Read the top of that file first for the existing helper style — there will already be a way to build an `Aggregator` with a fixed clock (`WithClock`) and to record an event; reuse it rather than writing a second one.

```go
// inferenceEvent builds a priceable response event with an explicit token split.
func inferenceEvent(model string, in, cacheRead, cacheWrite, out, reasoning int, kinds uint8) *pipeline.SessionEvent {
	return &pipeline.SessionEvent{
		At:         time.Now(),
		Phase:      pipeline.SessionResponse,
		StatusCode: 200,
		Host:       "gw.example.com",
		Inference: &pipeline.InferenceExtension{
			Model:            model,
			InputTokens:      in,
			CacheReadTokens:  cacheRead,
			CacheWriteTokens: cacheWrite,
			OutputTokens:     out,
			ReasoningTokens:  reasoning,
			TotalTokens:      in + cacheRead + cacheWrite + out,
			PresentKinds:     kinds,
		},
	}
}

func TestCounts_CarriesTheTokenSplit(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("m", 100, 2000, 50, 30, 0, 0b1111))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.InputTokens; got != 100 {
		t.Errorf("InputTokens = %d, want 100", got)
	}
	if got := snap.Totals.CacheReadTokens; got != 2000 {
		t.Errorf("CacheReadTokens = %d, want 2000", got)
	}
	if got := snap.Totals.CacheWriteTokens; got != 50 {
		t.Errorf("CacheWriteTokens = %d, want 50", got)
	}
	if got := snap.Totals.OutputTokens; got != 30 {
		t.Errorf("OutputTokens = %d, want 30", got)
	}
	// The legacy aggregate must keep working for clients written against it.
	if got := snap.Totals.Tokens; got != 2180 {
		t.Errorf("Tokens = %d, want 2180 (unchanged legacy sum)", got)
	}
}

func TestCounts_ReasoningIsNotAddedToOutput(t *testing.T) {
	// ReasoningTokens is a SUBSET of OutputTokens: the provider reports how much
	// of the generated output was reasoning. Adding them double-counts every
	// reasoning token, and at the output rate -- the most expensive tier.
	a := New()
	a.Record("s1", inferenceEvent("m", 0, 0, 0, 100, 40, 0b11001))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.OutputTokens; got != 100 {
		t.Errorf("OutputTokens = %d, want 100 -- reasoning must not be added", got)
	}
	if got := snap.Totals.ReasoningTokens; got != 40 {
		t.Errorf("ReasoningTokens = %d, want 40 reported alongside, not folded in", got)
	}
}

func TestCounts_SplitAccumulatesAcrossEvents(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("m", 10, 100, 5, 3, 0, 0b1111))
	a.Record("s1", inferenceEvent("m", 20, 200, 7, 4, 0, 0b1111))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.InputTokens; got != 30 {
		t.Errorf("InputTokens = %d, want 30", got)
	}
	if got := snap.Totals.CacheReadTokens; got != 300 {
		t.Errorf("CacheReadTokens = %d, want 300", got)
	}
}

func TestCounts_PresentKindsFoldsByOr(t *testing.T) {
	// One model reports cache counters, another does not. A reader of the window
	// total must be able to tell "no cache writes happened" from "nothing here
	// reports cache writes" -- otherwise a blank column is unreadable.
	a := New()
	a.Record("s1", inferenceEvent("reports-cache", 10, 0, 0, 5, 0, 0b1111))
	a.Record("s1", inferenceEvent("no-cache-fields", 10, 0, 0, 5, 0, 0b1001))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.PresentKinds; got != 0b1111 {
		t.Errorf("PresentKinds = %#b, want %#b (union of what any response reported)", got, 0b1111)
	}
}

func TestCounts_PresentKindsZeroWhenNothingReports(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("m", 0, 0, 0, 0, 0, 0))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupNone)

	if got := snap.Totals.PresentKinds; got != 0 {
		t.Errorf("PresentKinds = %#b, want 0", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./authlib/usage/ -run 'TestCounts_' -v 2>&1 | tail -20`
Expected: FAIL — `snap.Totals.InputTokens undefined`.

- [ ] **Step 3: Add the fields to `Counts`**

In `authlib/usage/usage.go`, add to the `Counts` struct after `Tokens`:

```go
	// The four billed token kinds, plus reasoning. Names match
	// pipeline.InferenceExtension exactly: the spec's rule is one vocabulary from
	// parser to aggregate to ledger to collector, because aggregate-side synonyms
	// are how two halves of a system come to disagree about what a field means.
	//
	// Tokens above stays as the sum for clients written against it. These are
	// additive to the wire, not a replacement.
	//
	// They matter because the kinds price very differently — a cache read is
	// roughly 0.1x uncached input and a cache write roughly 1.25x — so for a
	// long-running agent, whose traffic is overwhelmingly cache reads, the split
	// IS the shape of the bill. One scalar cannot express that.
	InputTokens      int64 `json:"inputTokens,omitempty"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	OutputTokens     int64 `json:"outputTokens,omitempty"`
	// ReasoningTokens is a SUBSET of OutputTokens, not a sibling of it: the
	// provider reports how much of what it generated was reasoning. Adding the two
	// double-counts every reasoning token at the output rate, which is the most
	// expensive tier there is.
	ReasoningTokens int64 `json:"reasoningTokens,omitempty"`

	// PresentKinds is the OR of every folded event's InferenceExtension.PresentKinds:
	// a set bit means at least one response in this bucket actually REPORTED that
	// kind. Bit layout matches parsercommon.Kind (Input=1, CacheRead=2,
	// CacheWrite=4, Output=8, Reasoning=16).
	//
	// Summing would be meaningless, hence the union. It exists because a zero in
	// one of the fields above has two readings — "this traffic wrote no cache" and
	// "nothing here reports cache writes" — and a by-model table showing a blank
	// column cannot be interpreted without knowing which. The per-event flag
	// already carries that distinction; dropping it at the aggregate would throw
	// away the only thing that makes an empty cell readable.
	PresentKinds uint8 `json:"presentKinds,omitempty"`
```

- [ ] **Step 4: Sum them in `Add`**

In `Counts.Add`, after `c.Tokens += o.Tokens`:

```go
	c.InputTokens += o.InputTokens
	c.CacheReadTokens += o.CacheReadTokens
	c.CacheWriteTokens += o.CacheWriteTokens
	c.OutputTokens += o.OutputTokens
	c.ReasoningTokens += o.ReasoningTokens
	// Union, not sum: PresentKinds is a set of which kinds were reported, so
	// adding two buckets' flags would produce a number that is not a bit set.
	c.PresentKinds |= o.PresentKinds
```

Extend `Add`'s doc comment to say that `PresentKinds` is OR-ed rather than added, so the next person adding a field does not pattern-match on `+=`.

- [ ] **Step 5: Populate them in `foldInto`**

In `authlib/usage/usage.go:543-556`, replace the token extraction and the `one := Counts{...}` literal:

```go
	var tokens int64
	var model string
	var split Counts
	if e.Inference != nil {
		tokens = int64(e.Inference.TotalTokens)
		model = e.Inference.Model
		split = Counts{
			InputTokens:      int64(e.Inference.InputTokens),
			CacheReadTokens:  int64(e.Inference.CacheReadTokens),
			CacheWriteTokens: int64(e.Inference.CacheWriteTokens),
			OutputTokens:     int64(e.Inference.OutputTokens),
			ReasoningTokens:  int64(e.Inference.ReasoningTokens),
			PresentKinds:     e.Inference.PresentKinds,
		}
	}

	one := Counts{
		Requests:          1,
		Tokens:            tokens,
		CostMicros:        ec.micros,
		PricedRequests:    ec.priced,
		PriceableRequests: ec.priceable,
		InputTokens:       split.InputTokens,
		CacheReadTokens:   split.CacheReadTokens,
		CacheWriteTokens:  split.CacheWriteTokens,
		OutputTokens:      split.OutputTokens,
		ReasoningTokens:   split.ReasoningTokens,
		PresentKinds:      split.PresentKinds,
	}
```

`one` is already passed whole to every `addLabel` call, so each label map picks the split up with no further change.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./authlib/usage/ -run 'TestCounts_' -v 2>&1 | tail -20`
Expected: PASS, all five.

- [ ] **Step 7: Prove the split survives folding**

The window's totals are summed from raw buckets, but a client asking for a coarser resolution gets folded buckets — and `fold` is a second summation site. Add:

```go
func TestCounts_SplitSurvivesFolding(t *testing.T) {
	// fold() is the second place Counts are summed. It delegates to Counts.Add,
	// so a new field is carried automatically -- but that is exactly the property
	// that silently broke for PricedRequests once, which is why Add is exported
	// and why this test exists.
	a := New()
	a.Record("s1", inferenceEvent("m", 10, 100, 5, 3, 0, 0b1111))

	snap := a.Snapshot(10*time.Minute, 5*time.Minute, "s1", GroupNone)

	var in, cr int64
	var kinds uint8
	for _, b := range snap.Buckets {
		in += b.InputTokens
		cr += b.CacheReadTokens
		kinds |= b.PresentKinds
	}
	if in != 10 || cr != 100 {
		t.Errorf("folded buckets: InputTokens = %d (want 10), CacheReadTokens = %d (want 100)", in, cr)
	}
	if kinds != 0b1111 {
		t.Errorf("folded PresentKinds = %#b, want %#b", kinds, 0b1111)
	}
}
```

Run: `go test ./authlib/usage/ -run TestCounts_SplitSurvivesFolding -v`
Expected: PASS without any change to `fold`. If it fails, `fold` is hand-summing fields instead of calling `Add` — fix `fold` to delegate, do not duplicate the field list.

- [ ] **Step 8: Run the package**

Run: `go test ./authlib/usage/ 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 9: Do not commit yet.** Task 3 commits.

---

## Task 2: Name the model grouping honestly, and add endpoint

**Files:**
- Modify: `authlib/usage/snapshot.go:13-18` (`Group` consts), `:21-37` (`ParseGroup`), `:320-340` (`bucket.series`)
- Modify: `authlib/usage/usage.go:129-152` (`bucket` gains `byEndpoint`), `:576-583` (`foldInto` populates it)
- Test: `authlib/usage/snapshot_test.go`

**Interfaces:**
- Consumes: `Counts` with the split (Task 1) — every label map carries it already.
- Produces: `usage.GroupModel Group = "model"`, `usage.GroupEndpoint Group = "endpoint"`. `GroupMethod` remains a valid `ParseGroup` input resolving to the same series as `GroupModel`. Later commits add `GroupAgent`; the `series` switch is where it lands.

**Why — read this before touching anything:** the spec says to add `GroupModel` "alongside the existing `byMethod`". **That is wrong, and following it literally would create two maps with identical contents.** `usage.go:543-548` reads `model` from `e.Inference.Model` and nothing else, and `:576-578` is `if model != "" { addLabel(&b.byMethod, truncateLabel(model), one) }`. A2A and MCP method names **never** enter `byMethod`. So `/v1/usage?group=method` is already group-by-model wearing the wrong name.

(What makes the name look right is `cmd/abctl/tui/events_pane.go:486-496`, whose `eventMethodValue` *does* return A2A and MCP methods — but that is the events-table cell, a different code path from the aggregator.)

Duplicating the map would double per-bucket memory in a ring of 360 buckets × up to 64 labels and give two names for one answer. So: rename the concept, keep the old spelling accepted.

`byEndpoint` is genuinely new. `e.Host` is on every event and is aggregated nowhere today, yet it is half of the rate key — the same model bills differently on a discounted gateway than on the vendor endpoint, and only the host tells them apart.

- [ ] **Step 1: Write the failing test**

Append to `authlib/usage/snapshot_test.go` (reuse `inferenceEvent` from Task 1 — it is in the same package):

```go
func TestSnapshot_GroupModelReturnsTheModelSeries(t *testing.T) {
	a := New()
	a.Record("s1", inferenceEvent("claude-opus-5", 10, 0, 0, 5, 0, 0b1001))
	a.Record("s1", inferenceEvent("claude-haiku-4-5", 20, 0, 0, 7, 0, 0b1001))

	snap := a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel)

	series := mergeSeries(snap.Buckets)
	if len(series) != 2 {
		t.Fatalf("series has %d keys (%v), want 2", len(series), series)
	}
	if got := series["claude-opus-5"].InputTokens; got != 10 {
		t.Errorf("claude-opus-5 InputTokens = %d, want 10", got)
	}
	if got := series["claude-haiku-4-5"].InputTokens; got != 20 {
		t.Errorf("claude-haiku-4-5 InputTokens = %d, want 20", got)
	}
}

// mergeSeries sums every bucket's Series into one map, so a test can assert on a
// window total without caring which bucket a record landed in.
func mergeSeries(buckets []Bucket) map[string]Counts {
	out := map[string]Counts{}
	for _, b := range buckets {
		for k, v := range b.Series {
			cur := out[k]
			cur.Add(v)
			out[k] = cur
		}
	}
	return out
}

func TestSnapshot_GroupMethodIsAnAliasForModel(t *testing.T) {
	// group=method is on the wire today and tui/usage_pane.go's cycleGroup passes
	// it, so it must keep working -- and it must return the SAME series as
	// group=model, because it was already the model series under a wrong name.
	a := New()
	a.Record("s1", inferenceEvent("claude-opus-5", 10, 0, 0, 5, 0, 0b1001))

	byModel := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupModel).Buckets)
	byMethod := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupMethod).Buckets)

	if len(byModel) != len(byMethod) {
		t.Fatalf("model series has %d keys, method series has %d; want identical", len(byModel), len(byMethod))
	}
	for k, v := range byModel {
		if byMethod[k] != v {
			t.Errorf("key %q: model = %+v, method = %+v; want identical", k, v, byMethod[k])
		}
	}
}

func TestParseGroup_AcceptsModelAndEndpointAndStillAcceptsMethod(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Group
	}{
		{"", GroupNone},
		{"none", GroupNone},
		{"model", GroupModel},
		{"method", GroupMethod},
		{"endpoint", GroupEndpoint},
		{"status", GroupStatus},
		{"plugin", GroupPlugin},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseGroup(tc.in)
			if err != nil {
				t.Fatalf("ParseGroup(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseGroup(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseGroup_RejectsUnknownWithoutEchoingInput(t *testing.T) {
	// The message crosses an UNAUTHENTICATED endpoint. Reflecting caller bytes
	// into a response body hands out a reflection primitive, so the error names
	// the valid set instead of quoting what it got.
	const attack = "<script>alert(1)</script>"
	_, err := ParseGroup(attack)
	if err == nil {
		t.Fatal("ParseGroup accepted an unknown group")
	}
	if strings.Contains(err.Error(), attack) || strings.Contains(err.Error(), "script") {
		t.Errorf("error echoes caller input: %q", err.Error())
	}
	for _, want := range []string{"model", "endpoint", "status", "plugin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the valid value %q", err.Error(), want)
		}
	}
}

func TestSnapshot_GroupEndpointBreaksDownByHost(t *testing.T) {
	a := New()
	e1 := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e1.Host = "gw-a.example.com"
	e2 := inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001)
	e2.Host = "gw-b.example.com"
	a.Record("s1", e1)
	a.Record("s1", e2)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupEndpoint).Buckets)

	if got := series["gw-a.example.com"].InputTokens; got != 10 {
		t.Errorf("gw-a InputTokens = %d, want 10", got)
	}
	if got := series["gw-b.example.com"].InputTokens; got != 20 {
		t.Errorf("gw-b InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupEndpointOmitsEventsWithNoHost(t *testing.T) {
	// Host is empty when the listener did not populate it. An empty-string key in
	// a breakdown table renders as a blank row that looks like a bug.
	a := New()
	e := inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001)
	e.Host = ""
	a.Record("s1", e)

	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "s1", GroupEndpoint).Buckets)

	if _, ok := series[""]; ok {
		t.Error(`series has an "" key; an unknown host must be omitted, not shown as a blank row`)
	}
}
```

Add `"strings"` to that file's imports if absent.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./authlib/usage/ -run 'TestSnapshot_Group|TestParseGroup_' -v 2>&1 | tail -20`
Expected: FAIL — `undefined: GroupModel`, `undefined: GroupEndpoint`.

- [ ] **Step 3: Rename the concept in the `Group` consts**

In `authlib/usage/snapshot.go`, replace the const block:

```go
const (
	GroupNone Group = "none"
	// GroupModel breaks totals down by the model named on the request.
	//
	// This is the series that shipped as GroupMethod. The aggregator only ever
	// populated it from Inference.Model (see foldInto) — A2A and MCP method names
	// never entered it — so "method" was a misnomer from the start. abctl's
	// events-table METHOD cell DOES show A2A/MCP methods, which is what made the
	// name look right; that is a different code path.
	GroupModel Group = "model"
	// GroupMethod is the name GroupModel shipped under. Accepted forever, resolves
	// to the same series.
	//
	// Kept because it is on the wire and tui/usage_pane.go's cycleGroup passes it:
	// dropping it would break a live client to fix a spelling. NOT a second map —
	// series() maps both to byMethod.
	GroupMethod Group = "method"
	// GroupEndpoint breaks totals down by target host.
	//
	// Half of the rate key. The same model bills differently on a discounted
	// gateway than on the vendor endpoint, and only the host distinguishes them, so
	// a cost table without this axis cannot explain why two identical-looking
	// requests cost different amounts.
	GroupEndpoint Group = "endpoint"
	GroupStatus   Group = "status"
	GroupPlugin   Group = "plugin"
)
```

- [ ] **Step 4: Accept the new values in `ParseGroup`**

Add cases for `GroupModel` and `GroupEndpoint` alongside the existing ones, and update the error string to name the new valid set. Keep it a fixed string that does not interpolate the caller's value:

```go
	return "", errors.New("unknown group (want none, model, endpoint, status, plugin; method is accepted as an alias for model)")
```

That is the string this commit wrote. **It is not the string in the tree**, because two later
commits added values and each extended the message with them — `session` in `b97dbc29` (abctl) / `003a30f8` (core) and
`agent` in `6870ad3b`. The shipped string names both. Adding a `ParseGroup` case without
extending this message is the drift to watch for: the endpoint answers 400 for a value it
does not accept and then fails to name a value it does.

- [ ] **Step 5: Map both names in `bucket.series`, and add the endpoint arm**

```go
	switch g {
	// Both spellings read the same map. GroupMethod is the name this series
	// shipped under; see its godoc.
	case GroupModel, GroupMethod:
		src = b.byMethod
	case GroupEndpoint:
		src = b.byEndpoint
	case GroupStatus:
		src = b.byStatus
	case GroupPlugin:
		src = b.byPlugin
	default:
		return nil
	}
```

- [ ] **Step 6: Add the accumulator and populate it**

In `authlib/usage/usage.go`, add to the `bucket` struct beside `byMethod`:

```go
	// byEndpoint tallies by target host. Bounded by maxLabelsPerBucket like every
	// other label map: Host comes off the request, so its cardinality is set
	// off-host rather than by anything this process controls.
	byEndpoint map[string]Counts
```

In `foldInto`, beside the existing `byMethod` population:

```go
	if e.Host != "" {
		addLabel(&b.byEndpoint, truncateLabel(e.Host), one)
	}
```

Guarded on non-empty for the reason the test pins: an `""` key renders as a blank row in a breakdown table and reads as a bug rather than as missing data.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./authlib/usage/ -run 'TestSnapshot_Group|TestParseGroup_' -v 2>&1 | tail -25`
Expected: PASS.

- [ ] **Step 8: Confirm nothing regressed**

Run: `go test ./authlib/usage/ ./authlib/sessionapi/ ./cmd/abctl/tui/ 2>&1 | tail -10`
Expected: PASS. `cmd/abctl/tui` matters here: `usage_pane.go`'s `cycleGroup` cycles through `GroupStatus`, `GroupMethod`, `GroupPlugin`, and must still compile and behave.

- [ ] **Step 9: Do not commit yet.** Task 3 commits.

---

## Task 3: Serve the new groupings and correct the endpoint's documentation

**Files:**
- Modify: `authlib/sessionapi/usage.go:11-64` (handler godoc)
- Test: `authlib/sessionapi/usage_test.go`

**Interfaces:**
- Consumes: `usage.ParseGroup` (Task 2), `usage.Counts` (Task 1).
- Produces: nothing new. The handler already delegates group parsing to `usage.ParseGroup`, so the new values are served with **no code change** — this task is tests plus documentation.

**Why:** two claims in that file's godoc are already false on this branch, and one becomes misleading as the axes grow.

1. It says cost "is populated from the per-request figure litellm-budget-track settles and publishes". As of the previous commit, `inference-parser` settles it and budget-track only enforces a budget. Cortex has already been bitten once on this branch by an operator-facing doc that outlived the code it described.
2. Its `group` parameter list reads `none (default), method, status, plugin`.
3. It carries a deliberate, valuable disclosure block about what each grouping leaks to an unauthenticated port — "written down so the decision to expose it is a decision rather than an oversight". `group=endpoint` exposes which gateways and vendor endpoints a deployment talks to, which that block does not yet cover. Adding an axis without extending the disclosure quietly widens the exposure the comment exists to make explicit.

- [ ] **Step 1: Write the failing test**

Append to `authlib/sessionapi/usage_test.go` (read the file first for how it builds a `Server` with a usage aggregator — reuse that helper):

```go
func TestHandleUsage_AcceptsModelAndEndpointGroups(t *testing.T) {
	for _, group := range []string{"model", "endpoint", "method", "status", "plugin", "none", ""} {
		t.Run("group="+group, func(t *testing.T) {
			srv := newTestServerWithUsage(t)
			rec := httptest.NewRecorder()
			srv.handleUsage(rec, httptest.NewRequest(http.MethodGet, "/v1/usage?group="+group, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			var snap usage.Snapshot
			if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
				t.Fatalf("decode: %v", err)
			}
		})
	}
}

func TestHandleUsage_RejectsUnknownGroupWithoutReflectingIt(t *testing.T) {
	srv := newTestServerWithUsage(t)
	rec := httptest.NewRecorder()
	srv.handleUsage(rec, httptest.NewRequest(http.MethodGet, "/v1/usage?group=%3Cscript%3E", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "script") {
		t.Errorf("response reflects caller input: %s", rec.Body.String())
	}
}

func TestHandleUsage_SplitFieldsAppearOnTheWire(t *testing.T) {
	// The whole point of the schema rule is that a consumer reads the same field
	// names the parser published. Assert on the JSON, not the struct.
	srv := newTestServerWithUsage(t)
	srv.usage.Record("s1", inferenceEventForAPI("claude-opus-5", 10, 2000, 50, 30))

	rec := httptest.NewRecorder()
	srv.handleUsage(rec, httptest.NewRequest(http.MethodGet, "/v1/usage?session=s1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, field := range []string{"inputTokens", "cacheReadTokens", "cacheWriteTokens", "outputTokens"} {
		if !strings.Contains(body, field) {
			t.Errorf("response body has no %q field: %s", field, body)
		}
	}
}
```

`inferenceEventForAPI` is a local helper you write in this file, mirroring Task 1's `inferenceEvent` but without needing `PresentKinds` — the `usage` package's copy is not exported, so do not try to import it.

- [ ] **Step 2: Run the test to verify it fails or passes**

Run: `go test ./authlib/sessionapi/ -run TestHandleUsage_ -v 2>&1 | tail -20`

Expected: the group-acceptance tests **PASS immediately** — the handler delegates to `ParseGroup`, which Task 2 already taught the new values. That is the correct outcome and not a reason to add code. `TestHandleUsage_SplitFieldsAppearOnTheWire` should also pass. If a test fails, the failure is real: investigate rather than adjusting the assertion.

This is the one task in this plan whose tests are characterisation rather than TDD, because the production change is documentation. Say so in your report.

- [ ] **Step 3: Correct the two false claims and extend the disclosure**

In `authlib/sessionapi/usage.go`, update the `group` parameter line:

```go
//	group       none (default), model, endpoint, status, plugin. "method" is
//	            accepted as an alias for "model" — the series shipped under that
//	            name before it was clear the aggregator only ever populated it
//	            from the inference model.
```

Replace the paragraph beginning "costMicros is populated from the per-request figure litellm-budget-track settles":

```go
// costMicros is populated from the figure inference-parser settles on the
// response pass. It prefers the gateway's own post-discount cost header — the
// authoritative figure — and falls back to pricing the parsed token counters when
// the header is absent or reports 0, which every streamed response does.
//
// The parser owns that decision because it is the only component that knows when
// the token counters are final. Cost used to be computed in four places, and
// which one answered depended on which plugins an operator had enabled: a request
// could carry a token count with no money in a live abctl view while showing
// dollars here. litellm-budget-track now consumes the settled figure to enforce a
// budget rather than computing one of its own.
```

**Do not paste that block as it stands** — three of its claims were corrected by `5d004725`
and `cf4ab175` and the banner's second sweep gives the reasoning: the settling component is
`authlib/costing`, which the parser *calls*; cost used to be decided in **two** places, not
four; and `litellm-budget-track` **amends** the settled record rather than consuming it. The
`group` line above it is likewise two values short of the shipped one. Left in place because
the *shape* of the correction — name the component, name what it replaced, say why the old
answer varied by plugin composition — is the point, and the shipped paragraph is that same
shape with the right nouns in it.

Extend the disclosure block. Add to the existing bullet list:

```go
//   - group=endpoint exposes which gateways and vendor endpoints this deployment
//     sends inference to, including internal hostnames. That is deployment
//     topology, not just model choice, and it is the most sensitive of the
//     groupings for that reason.
```

And correct the `group=method` bullet to name it `group=model` (mentioning that the alias exposes the same thing), so the disclosure list matches the parameter list.

- [ ] **Step 4: Re-run the package**

Run: `go test ./authlib/sessionapi/ 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Full suite, vet, lint**

Run: `go test ./authlib/... ./cmd/abctl/... 2>&1 | grep -v "^ok\|no test files" | head -20`
Expected: no output except possibly the known `cmd/abctl` `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` failure (a TEST-ISOLATION BUG: the fixture is deliberately bundle-less, but the machine's own `~/.cortex/ca/bundle.crt` leaks through, fails on `main` too).

Run: `go vet ./authlib/... && golangci-lint run --new-from-rev=main ./authlib/usage/... ./authlib/sessionapi/... 2>&1 | tail -15`
Expected: clean.

Run: `gofmt -l authlib/usage authlib/sessionapi`
Expected: no output.

- [ ] **Step 6: Commit — this is commit 3 of the PR**

Run these from the **repo root**, not from `authbridge/` — the paths below are repo-relative. Every other command in this plan runs from `authbridge/`; this is the one exception.

```bash
git add authbridge/authlib/usage authbridge/authlib/sessionapi \
        authbridge/docs/superpowers/plans/2026-09-13-cost-aggregation.md
git commit -s -m "Feat: Aggregate the token split, and break cost down by model and endpoint

usage.Counts carried a single scalar Tokens while the session event has
published the full split since #811. A total assembled from one number
cannot say where the money went, and the kinds price very differently -- a
cache read is roughly 0.1x uncached input, a cache write 1.25x -- so for a
long-running agent, whose traffic is overwhelmingly cache reads, the split
is the shape of the bill.

Counts now carries inputTokens, cacheReadTokens, cacheWriteTokens,
outputTokens and reasoningTokens, using the names the wire event already
uses so there is one vocabulary from parser to aggregate. reasoningTokens
is a subset of outputTokens and is never added to it. Tokens stays as the
sum for existing clients.

presentKinds is OR-folded rather than summed. Without it a blank
CACHE-WRITE column cannot be read: \"this traffic wrote no cache\" and
\"nothing here reports cache writes\" are different answers.

GroupModel is a RENAME, not a new series. foldInto only ever populated
byMethod from Inference.Model -- A2A and MCP method names never entered it
-- so group=method was already group-by-model under a wrong name. Both
spellings resolve to the same map; a second map would have doubled
per-bucket memory in a 360-bucket ring to give two names for one answer.

GroupEndpoint is genuinely new. Host is on every event, aggregated
nowhere, and is half the rate key: the same model bills differently on a
discounted gateway than on the vendor endpoint.

Also corrects two claims in the /v1/usage godoc that this branch already
made false, and extends its deliberate what-this-leaks disclosure to cover
group=endpoint, which exposes deployment topology rather than only model
choice.

Refs #950

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage for commit 3:** four-way token split in `Counts` (Task 1) ✓; `reasoningTokens` as a subset (Task 1) ✓; `presentKinds` OR-folded (Task 1) ✓; `GroupModel` (Task 2, as a rename — spec corrected) ✓; `GroupEndpoint` (Task 2) ✓; `/v1/usage` purely additive (Task 3) ✓; one vocabulary shared with the wire event (Task 1, enforced by `TestHandleUsage_SplitFieldsAppearOnTheWire`) ✓.

**Deferred by design:** `GroupAgent` needs `EventClient`, which is commit 6. `GroupSession` needs no new accumulator — the aggregator already keys per-session rings — but the *grouping* value is only meaningful once #949 lands, so it is not added here; `series()` is where it would go.

**Both deferrals were subsequently reversed, and the second sentence above is wrong.**
`GroupSession` landed in `b97dbc29` (abctl) / `003a30f8` (core) and `GroupAgent` in `6870ad3b`. The reversal of the
*timing* is argued in the spend-strip plan's Task 4: the reasoning here was about what the
numbers would *mean* on a laptop, and applying an interpretation caveat to the mechanism was
the mistake — the sessions pane needs per-session cost now, and the #949 caveat belongs in the
docs rather than in a missing feature.

The claim that it needs **no new accumulator is simply false**, and the per-session rings are
why it looks true. A per-session ring answers "what did session X cost" when you ask for that
session; it cannot answer "break the all-sessions window down BY session", which is what a
sessions list needs in one request instead of one per row. `b97dbc29` (abctl) / `003a30f8` (core) therefore added
`bucket.bySession` *and* a `sessionID` parameter on `foldInto` to feed it — the first change to
that signature since the coverage-counter fix. The lesson generalises: the aggregator holds
marginals, and "there is already a ring keyed on it" is never evidence that a breakdown by it
exists.

**Type consistency check:** `Counts` fields are `int64` to match the existing `Tokens`/`CostMicros`; `PresentKinds` is `uint8` to match `pipeline.InferenceExtension.PresentKinds`. `e.Inference.*Tokens` are `int` on the extension, so every assignment is an explicit `int64(...)` conversion — Task 1 Step 5 writes them that way.

**Known deviation from the spec, ruled during planning:** the spec's "add `GroupModel` alongside `byMethod`" is wrong; Task 2's Why section carries the evidence and the ruling.


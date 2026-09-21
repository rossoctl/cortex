# Per-Tier Cost Breakdown in abctl

Status: proposed · 2026-09-19 · targets `authbridge/authlib/{pricing,costing,costevent,usage}`
and `authbridge/cmd/abctl`

## 1. Problem

abctl can say what traffic cost in total, and it can say how many tokens of each
kind that traffic used. It cannot say what each **kind** cost.

That is the question an operator actually has. A cache read bills at roughly 0.1x
the uncached input rate and a cache write at roughly 1.25x, so a long-running
coding agent's bill is dominated by a tier that barely shows up in the token
counts — and the reverse: the tier with the most tokens is usually the cheapest
one on the invoice. Reading the token split and guessing the money split gets the
answer wrong by close to an order of magnitude, which is the same reasoning that
put `PromptUSD` on the per-event record in the first place.

A second, smaller problem surfaced from a live screen: with a single model in the
window, the spend drawer's per-model breakdown restates the strip's own figures
(`$4.5462` and `saved ~$0.2091` appear twice) and adds nothing. A tier breakdown
is the decomposition that stays informative when there is only one model.

## 2. What exists today

Verified against `fe19c8c1`, not assumed:

| Layer | Per-tier tokens | Per-tier cost |
|---|---|---|
| `pricing.CostWithReason` | yes, `Usage.tokens()` | **computed, then discarded** |
| `costing.Settled` | — | prompt/output halves only |
| `costevent.Event` | — | `PromptUSD`, `OutputUSD` |
| `usage.Counts` | yes, five fields | no — one `CostMicros` |
| `costledger.Row` | via embed | via embed |
| `/v1/usage` | yes | no |
| `abctl cost` | yes, `tokenSplit` | no |
| spend drawer | no | no |

Two facts shape the whole design:

**The tier amounts already exist for one line.** `pricing.CostWithReason` loops
the four tiers and accumulates `usd += float64(n) * eff.Base[i]`, then throws the
per-tier values away. Capturing them is a change at the source, not a new
calculation.

**`costledger.Row` embeds `usage.Counts`.** Its own doc says this is deliberate,
"so the ledger, /v1/usage and any future collector share one vocabulary: a field
added there appears here, and a divergence is a compile error instead of a review
question." Four fields on `Counts` therefore reach the ring, the durable ledger,
the `Fold` query path and the wire without further work.

## 3. Decisions

Settled with the requester; each is a decision, not a default.

**3.1 The tiers are modelled; the total may not be.** A request priced from a
gateway's own header has an authoritative total and no breakdown, because no
gateway publishes one. `Modelled.PromptUSD`'s doc already states the consequence:
"PromptUSD + anything is not a total of anything."

**3.2 Apportion the authoritative total by the modelled mix.** The mix is the rate
table's, the magnitude is the gateway's:

```
tierMicros[i] = CostMicros × modelledTier[i] / Σ modelledTier
```

The column therefore always sums to the headline figure, which is the first thing
a reader checks. The alternative — printing modelled dollars beside an
authoritative total that differs — puts two totals on screen and invites "which
one am I paying?".

Two arithmetic details the formula hides, both of which decide a test:

- **The multiply overflows int64** for a large total against a large modelled
  figure. The ratio is computed in `float64` and applied to the total, which is
  safe because the result is bounded by `CostMicros` — a quantity already known to
  fit — rather than by the product.
- **Integer division loses a micro or two**, so four rounded tiers need not sum to
  the total. The remainder is assigned to the **largest** tier, where it is
  proportionally smallest and cannot flip a rank. Without this rule §8 case 1 is
  unsatisfiable, so the rule is part of the design rather than an implementation
  detail.

**3.3 Approximation is acceptable, and `~` is the whole disclosure.** `~` already
means inexact throughout this UI (`~$0.2091` for savings, documented in the `?`
overlay), and it rides on each figure rather than on the column header, matching
the drawer's established rule that each row carries its own caveats. No coverage
fraction and no threshold prose appear on screen.

**3.4 The column refuses only when there is no mix at all.** Where `Σ
modelledTier == 0` — every priced request in the window came from a gateway header
and none has rates — there is nothing to apportion by, and each tier cell renders
`emptyCell` (`—`, "not known here") rather than a figure.

Deliberately **no percentage threshold**. An earlier draft refused below a coverage
floor, on the reasoning that apportioning a window by a mix drawn from one request
is a guess rather than an approximation. That is true, but any particular floor is
a number nobody can defend: 50% and 10% are equally arbitrary, and a constant whose
value is unjustifiable is worse than the behaviour it guards. The mix is the only
evidence available, `~` says it is inexact, and a reader who wants coverage has
`abctl cost`, which reports priced-versus-priceable already.

**3.5 `reasoning` is not a tier.** It is a subset of `output` — `tokenSplit`
already labels it `reasoning (of output)` — so including it as a fifth bar would
double-count. It stays in `abctl cost`'s token line and out of the bars.

**3.6 No residual twins for the new fields.** `CostMicros` has
`UngroupedCostMicros` and `SeriesOvershootMicros` because the authoritative total
must reconcile across grouping. A modelled mix is an apportionment key; an
unreconciled remainder in it moves a bar by a fraction of a percent and cannot
misstate the total, which is pinned to `CostMicros` by 3.2.

**3.7 Display order is by amount, descending.** `pricing.Tier` is declared
`Input, CacheWrite, CacheRead, Output`; the panel ranks the four by what they
cost, the way the drawer already ranks models, so the row worth reading is first.
Array order and display order are deliberately different.

## 4. Layout

The current screen has no content region: strip, drawer, hint and table all carry
the same visual weight, and the spend figures interleave labels with values
(`$3.8402 today`). Two states replace it.

### 4.1 Resting

```
 abctl · 127.0.0.1:47601                              Sessions · Pipeline
  TODAY       LAST 1H     SAVED       CACHE HIT   TOKENS
  $3.8402     $4.5462     ~$0.2091    93%         5.6M
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  SESSION      UPDATED   EVENTS    TOKENS       COST       SAVED
  ecb7387f…    40s ago      105      7.0M    $5.8344    ~$0.2917  ●
  default      18h ago        1         —          —           —

 ● live · 2.1 events/sec · polled 2m ago    [$] spend  [u] usage  [q] quit
```

Labels sit **above** values rather than beside them. This is the change that fixes
legibility, and it costs two rows against today's single strip line. The rule under
the band is the table's own top border, not a spent row.

### 4.2 Expanded (`$`)

```
 abctl · 127.0.0.1:47601                              Sessions · Pipeline
  TODAY       LAST 1H     SAVED       CACHE HIT   TOKENS
  $3.8402     $4.5462     ~$0.2091    93%         5.6M

  WHERE IT WENT · 1h             BY MODEL · 1h
  cache-read  ████████ ~$2.8186  claude-opus-5   $4.5462   35 req
  output      ███      ~$1.3639
  input       █        ~$0.2728
  cache-write ▏        ~$0.0909  [a] model · endpoint · agent  [w] 1h  esc
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  SESSION      UPDATED   EVENTS    TOKENS       COST       SAVED
  ecb7387f…    40s ago      105      7.0M    $5.8344    ~$0.2917  ●
  default      18h ago        1         —          —           —

 ● live · 2.1 events/sec · polled 2m ago    [$] close  [u] usage  [q] quit
```

Tier bars appear only when expanded: `$` remains the gesture that reveals cost
detail, and a third permanent row would cost the table too much.

The mock shows one model because the screen it came from had one; the right column
keeps the drawer's existing shape of up to three ranked series plus an `(other)`
band, and its last line hosts the key hints. That asymmetry — four tier rows on the
left, up to four series rows on the right — is why both columns fit the same fixed
line count.

**Bars are solid blocks, not letter runs.** `usage_stacked.go` rejected shaded
blocks for its time-series chart because adjacent segments were indistinguishable,
and replaced them with letters on coloured grounds. That reasoning does not
transfer: here each bar occupies its own line beside its own label, so there is no
neighbour to disambiguate from and a solid block is both unambiguous and legible.
Colour is decoration; the label carries the identity, so the panel survives a
monochrome terminal.

### 4.3 Degradation

The two reservations must move together — `layout()` and `paneView`'s padding —
because splitting them is what produced both the overflow and the under-fill
defects already fixed on this surface. Order of sacrifice as width shrinks:

1. Both columns fit: as drawn.
2. Too narrow for two columns: the **tier** column drops and the panel degrades to
   exactly today's per-model drawer. The addition yields to the existing contract.
3. Below the strip's own width floor: the band drops to the current single-line
   strip, whose ladder already drops whole figures right to left.

Height: the resting band reserves its rows through `spendStripReservesRow`; the
expanded panel keeps a fixed line count so that a caveat can never change the
height, which is why 3.3 puts the marker on figures instead of on a caveat line.

### 4.4 Cross-cutting fixes

Independent of the tier work, and the reason the current screen reads as ugly:

| Fix | Why |
|---|---|
| Right-align numerics, fixed decimals | Digits line up; the main cause of raggedness |
| `ecb7387f…` rather than the full UUID | Reclaims ~30 columns of the widest column |
| Scope stated once per region | `$5.8344` lifetime beside `$3.8402` today reads as a bug otherwise |
| Never print one figure twice | The old drawer restated the strip's window and saved figures |
| Drop the `└` glyph | It implied a parent row that does not exist |
| Muted labels, bright values | Hierarchy through the palette already in `styles.go` |

## 5. Data flow

```
pricing.CostWithReason ──[numTiers]float64──▶ costing.Settled ──▶ costevent.Event
                                                                        │
                                                          usage.foldInto│
                                                                        ▼
                                          usage.Counts (4 × int64) ──▶ costledger.Row (embed)
                                                     │                        │
                                                     ├──▶ /v1/usage           └──▶ durable ledger
                                                     ▼
                                    usage.ApportionTiers(Counts) ──▶ drawer · abctl cost · --json
```

The apportionment of 3.2 lives in **one** exported helper in `usage`, called by
every surface, so the drawer, the text command and the JSON cannot disagree about
a figure derived twice. Its shape, so PR 2 is not a coin flip:

```go
// ApportionTiers splits c.CostMicros across the four tiers by the modelled mix.
// ok is false when there is no mix to apportion by (3.4); callers render emptyCell.
func (c Counts) ApportionTiers() (tiers [4]int64, ok bool)
```

A method on `Counts` rather than a free function, because the inputs and the total
are all fields of the receiver and every caller already holds one.

Four `int64` fields go **adjacent to `CostMicros`** in `Counts`, so that the
modelled-versus-authoritative distinction is visible in one glance rather than
inferred from field names. They are carried by `Add`'s checked accumulate; the two
existing reflection guards — `TestCountsAdd_EveryInt64FieldSaturatesAndSaysSo` and
`TestFoldInto_CarriesEveryCountsField` — fail until this is done, which is the
intended forcing function rather than an obstacle.

## 6. Migration

The ledger is append-only with 30-day retention, so rows written before this
change carry no tier fields. A `7d` window spanning the upgrade therefore yields a
partial mix — which is exactly the case 3.2 and 3.3 already describe, apportioned
from what is known and marked `~`. No backfill, no schema version, no reader
branch.

## 7. Non-goals

- A cost metric on the Usage pane. It has none today; adding one is a separate change.
- New panes or new keys. `$`, `a`, `w`, `esc` keep their current meanings.
- Per-tier savings. `AvoidedMicros` stays a single figure; splitting an estimate by a modelled mix compounds two approximations.
- Reasoning as a fifth bar (3.5).
- Per-tier residual reconciliation (3.6).

## 8. Testing

Per this surface's established standard, every behavioural claim gets a
discriminating test plus a mutation control that is verified to have applied, to
build, and to fail on the intended assertion.

The cases that need to exist, chosen because each has a plausible wrong
implementation:

1. Tiers sum to `CostMicros` **exactly**, across a range of totals chosen so the remainder is non-zero — otherwise the assertion passes on arithmetic that happens to divide evenly and says nothing about 3.2's remainder rule.
2. A window priced entirely from gateway headers, with `Σ modelledTier == 0`, renders `emptyCell` and does not divide by zero (3.4).
3. A mix covering a small fraction of the priced spend still apportions, and wears `~` — the positive control for having removed the threshold, since a reintroduced floor would blank this case.
4. Display order is by amount, not by `pricing.Tier` declaration order (3.7). The fixture must order the two differently, or the test passes under either implementation.
5. `reasoning` never appears as a bar and never joins the sum (3.5).
6. The panel's line count is identical across every coverage state, which is what keeps the reservation honest.
7. At a width too narrow for two columns, the output equals today's drawer.
8. Saturation: a tier at `MaxInt64` sets `Saturated` and does not wrap — the failure already found once in `rankSeriesByCost`, whose raw `+=` ranked an overflowing series below a ten-micro one.

## 9. PR split

Three, stacked on `main`, each independently reviewable and each shipping a
verifiable increment:

**PR 1 — capture.** `pricing` returns the per-tier array it already computes;
`costing.Settled` and `costevent.Event` carry it. No surface change. Ships the
per-event data and is testable purely as arithmetic.

**PR 2 — aggregate.** Four fields on `usage.Counts`, wired through `Add` and
`foldInto`; ledger, `/v1/usage` and `Fold` follow from the embed. Exposes
`usage.ApportionTiers` and the `--json` field. Ships a machine-readable breakdown
with no TUI risk.

**PR 3 — render.** The KPI band, the two-column expanded panel, the degradation
ladder and §4.4's cross-cutting fixes. Carries the layout change, so it is the one
that touches the reservation logic.

## 10. Risks

**The layout change is the risky one.** This surface has already produced a resize
panic, an overflow of five rows and an under-fill of six, all in the reservation
arithmetic that PR 3 touches. Mitigation: the reservation and the padding move in
the same commit, and the fixed-line-count property (§8 case 6) is asserted across
every state rather than at one representative width.

**A modelled mix can be unrepresentative** even when it covers most of the spend — a window where
the cheap models are the modelled ones apportions the expensive model's spend by
the wrong shape. `~` is the disclosure, and 3.2 accepts this consciously: the
alternative shows two totals.

**The band costs two rows permanently.** On a short terminal that is two sessions
fewer. `spendStripReservesRow`'s existing height floor governs, so the band
disappears entirely rather than squeezing the table to nothing.

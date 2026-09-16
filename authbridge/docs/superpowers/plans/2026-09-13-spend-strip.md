# Spend Strip Implementation Plan (commit 4)

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

> **STATUS: implemented.** Landed as `b97dbc29` (abctl) / `003a30f8` (core). The SPAN column it specifies was
> subsequently removed and ACTIVE moved last (`2f4c5d57`) — see that commit for why.
>
> An earlier revision of this banner cited `c5b1f596`, which is not an ancestor of this
> branch: it is the same change on an abandoned branch that was never merged. A banner
> pointing at an unreachable commit is worse than no banner, because a reviewer who
> looks it up concludes the doc is describing someone else's tree.
>
> **Three things this plan promises did not ship in the shape it describes**, and each
> is noted again where the plan says it:
>
> - **No wall-time column, under any name.** The File Structure table calls it `AGE`;
>   Task 4 calls it `SPAN`. No column called `AGE` ever existed — `SPAN` is what was
>   built, and `2f4c5d57` removed it. The shipped columns are
>   `ID / UPDATED / EVENTS / TOKENS / COST / ACTIVE`. `UPDATED` is last-event age via
>   `relTime`, not wall time, and that is precisely why `SPAN` went: `SPAN` measured
>   `UpdatedAt - CreatedAt`, so an idle session reported the length of its idleness and
>   said almost the same thing as `UPDATED` two columns to its left.
> - **No `sessionSpan` method.** `(*model).sessionCost` exists; `sessionSpan` does not,
>   and never did on this branch (`grep -rn sessionSpan cmd/abctl/` finds nothing).
> - **The strip does not fold into the title bar below 20 rows.** It is simply not
>   drawn, and the header stays a bare `styleTitle.Render(title)` with no figure. The
>   test comment that said otherwise was corrected to "yields its row"; this plan was
>   not.
>
> Also superseded: `b97dbc29` (abctl) / `003a30f8` (core) closed a latent bug the plan does not mention (the window
> figure was rendered whenever a today figure existed, even for a window that priced
> nothing) and `6c7c7fe0` (core) / `9f0e35b0` (abctl) supplied the "today" figure Task 4 Step 5 sets up.
>
> **Line numbers drift.** Every `file.go:NN` below was accurate when written and many
> have moved. Read them as "roughly here" and find the symbol by name.
>
> ---
>
> **SECOND SWEEP, 2026-09-15.** The three corrections above still hold — verified: there
> is no `SPAN` column, `grep -rn sessionSpan cmd/abctl/` still finds nothing, and below 20
> rows the header is still a bare `styleTitle.Render(title)`. What has changed is
> everything the strip *renders*, across eight further commits. Five differences an
> implementer reading the code blocks below would otherwise have to discover:
>
> - **`spendStripVisible` split in two, and that split is the answer to the open question
>   in Task 3 Step 4.** `spendStripReservesRow()` is height-only and is what `layout()`
>   calls; `spendStripVisible()` is that **and** the pane check, and is what `paneView`
>   calls. The step asks the implementer to "confirm no pane transition leaves a stale
>   `bodyHeight`" and offers dropping the pane check from the reservation as the fallback.
>   That fallback is what shipped, in both halves rather than one. `layout()` now reserves
>   up to **five** rows, not four: the filter input takes a conditional row too.
> - **The renderer degrades along two axes, not one.** A figure is a
>   `stripFigure{full, compact}` pair, and `fitStripFigures` searches count × verbosity —
>   all N spelled out, all N compact, N-1 spelled out, and so on — then drops the `SPEND`
>   label before it drops a number. So its parameter is `[]stripFigure`, not `[]string`,
>   and the caveat's *words* go before any figure does.
> - **Every money figure wears up to three one-column markers**, composed as
>   `!~$4.1700+`: `!` the ledger read lost rows, `~` at least one figure in this total is
>   inexact rather than exact, `+` the figure covers only part of the priceable traffic.
>   `moneyFigure` is the one place they go on, and `7789f8f0` made the fourth refusal —
>   a negative total is never rendered as money — cover all six money surfaces rather than
>   two. Markers are never what a narrow terminal drops; that is why dropping the caveat's
>   words is safe.
> - **A fifth figure was added** (`a529c66f`): how long ago the strip last polled, shown
>   only when that answer is stale. A wedged figure that looks live is worse than a dated
>   one.
> - **`spendSummary` gained eight fields.** `HasSnapshot` and `Failed` separate "no poll
>   has answered yet" from "the poll failed"; `Incomplete` carries the exactness caveat;
>   and `TodayUnpriced`, `TodayPriceable`, `TodayIncomplete`, `TodayDegraded` exist because
>   `f80c4248` found the two figures sharing one set of caveats — the window's coverage gap
>   was qualifying the day's total and vice versa. Task 1's struct is a strict subset of
>   the shipped one; its *optional-fields* premise is what made all of that additive, which
>   is the thing to keep.
>
> Two smaller ones on the sessions list: the `COST` column's title is now the dynamic
> `COST/<window>` (`a529c66f`), found by prefix so the span can change without breaking
> lookups; and a cell too narrow for its figure renders `…` rather than blank
> (`722da202`), because blank already means "unpriced" and the two must not collide.
> `a5a7e07b` rebuilds the rows on a resize, not only the columns.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put spend on screen unconditionally — one row of chrome, visible from every pane — plus a `COST` column on the sessions list, so cost is read before the data rather than navigated to.

**Architecture:** Four tasks, one commit. A small always-on poll keeps a spend summary current independent of the Usage pane's own chain. A pure render function turns that summary into one line, degrading by dropping whole figures right-to-left and never clipping a number. The line is inserted between the title and the body in `paneView`, with one row taken out of the height budget. The sessions table gains cost and wall time.

**Tech Stack:** Go 1.x, bubbletea/lipgloss, standard library `testing` (table-driven).

**Spec:** `authbridge/docs/superpowers/specs/2026-09-13-cost-first-class-design.md`

## Global Constraints

- **Module root is `authbridge/`.** Go commands run from `authbridge/`, relative to the repo root. `git` runs from the repo root itself.
- **`abctl` is its own module** (`cmd/abctl/go.mod`) with a `replace` to local `authlib`, satisfied by `authbridge/go.work`. It always builds against the local `authlib`, so new `usage.Counts` fields are visible without a version bump.
- **`git commit -s` is mandatory.** Trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; never `Co-Authored-By`.
- **Never `$0.00` for an unknown cost.** `usage_render.go:347` establishes this — a zero cost and an unknown cost are different answers, and only one means the traffic was free. The strip says `cost unavailable`.
- **Degrade by dropping whole figures, never by clipping a number.** #953 asks for "no truncated numbers". A half-rendered dollar amount is worse than a missing one.
- **All width arithmetic goes through `lipgloss.Width`, never `len()` or rune counts.** `footer.go:88-92` records the bug this prevents: a budget computed in display columns but sliced by rune index overflowed on any wide character — a CJK path asked to fit 40 columns rendered 55.
- **The strip must not disturb the events pane.** No shared state with `eventsTbl`'s cursor, filter or scroll position.
- **Reuse the generation-guard pattern** from `usage_pane.go:64-79` for any new poll chain: stale replies and stale ticks dropped by sequence number. That comment records a real bug — a quick exit and re-entry left two chains alive, each rescheduling the other's successor and doubling the request rate.
- Do NOT run `make lint`. Do NOT run `gofmt -w .` at the module root — format only files you touched.
- Known pre-existing failure on this machine: `cmd/abctl`'s `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` fails for a TEST-ISOLATION BUG: the fixture is deliberately bundle-less, but the machine's own `~/.cortex/ca/bundle.crt` leaks through; it fails on `main` too.
- Comment register: long comments explaining *why*, naming the bug the code prevents.

## Scope note: what this commit does NOT show

The spec's strip has four figures: `$4.17 today · $1.12 /1h · ~$0.11/min · saved $0.24 (5.4%)`.

Two of those are not available yet and **must not be faked**:

- **"today"** needs the durable cost ledger, which is commit 5. Until then the headline figure is window-scoped and labelled as such (`$1.12 /1h`), not labelled "today".
- **"saved"** needs the `Avoided` container, which is commit 7. Until then the strip renders **no saved figure at all** — not `saved $0.00`, which would assert that pruning saved nothing when the truth is that nothing measures it yet. Same rule as `cost unavailable`.

So this commit ships a two-figure strip (window spend + burn rate) that later commits extend. Task 2's renderer takes a struct with optional fields so those commits add data, not render branches.

---

## File Structure

| file | responsibility | task |
|---|---|---|
| `cmd/abctl/tui/spend.go` | **new** — `spendState`, its poll chain, and the summary it derives | 1 |
| `cmd/abctl/tui/spend_test.go` | **new** — poll/guard behaviour | 1 |
| `cmd/abctl/tui/spend_strip.go` | **new** — the pure renderer and its degradation ladder | 2 |
| `cmd/abctl/tui/spend_strip_test.go` | **new** — width table, unpriced states | 2 |
| `cmd/abctl/tui/app.go` | insert the strip in `paneView`; model field | 3 |
| `cmd/abctl/tui/keys.go` | `layout()` height budget | 3 |
| `cmd/abctl/tui/sessions_pane.go` | `COST` and `AGE` columns — `COST` only; see the banner | 4 |
| `cmd/abctl/tui/sessions_pane_test.go` | column contents, unpriced session | 4 |

---

## Task 1: An always-on spend summary

**Files:**
- Create: `cmd/abctl/tui/spend.go`, `cmd/abctl/tui/spend_test.go`
- Modify: `cmd/abctl/tui/app.go` (model gains `spend spendState`; `Init` starts the chain)

**Interfaces:**
- Consumes: `apiclient.Client.GetUsage(ctx, window, resolution, session, group)` — the existing method `usage_pane.go:148` calls. `usage.Snapshot`, `usage.Counts`.
- Produces:
  ```go
  // spendSummary is what the strip renders. Fields are optional on purpose so
  // later commits add data without adding render branches.
  type spendSummary struct {
      WindowUSD   float64 // spend over WindowLabel
      WindowLabel string  // "1h"
      BurnPerMin  float64 // 0 means "not enough data", not "free"
      Priced      bool    // false => render "cost unavailable", never $0.00
      Unpriced    int64   // priceable requests with no figure
      Priceable   int64
      TodayUSD    float64 // commit 5
      HasToday    bool    // commit 5
      SavedUSD    float64 // commit 7
      HasSaved    bool    // commit 7
  }
  ```
  and `(*model).spendSummary() spendSummary`, plus `spendTick`/`fetchSpend` commands.

**Why a second poll chain rather than reusing `m.usage`:** the Usage pane's state is pane-scoped — `openUsage` sets it, `resumeUsagePolling` starts its chain, and it holds a `session` scope and a user-chosen window. The strip needs all-sessions data whenever abctl is running, whatever pane is showing. Driving both from one chain would mean the strip going stale or empty the moment a user scoped the Usage pane to one session, which is precisely when they are looking at cost.

Cost of the extra chain: one `GET /v1/usage` every 20s, asking for a single bucket. Both chains can be alive at once; that is two cheap requests per 20s and is stated in the code so it is a decision rather than an oversight.

- [ ] **Step 1: Write the failing test**

Create `cmd/abctl/tui/spend_test.go`:

```go
package tui

import (
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

func TestSpendSummary_DerivesWindowAndBurnRate(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests:          10,
			CostMicros:        1_120_000, // $1.12
			PricedRequests:    10,
			PriceableRequests: 10,
		},
		Priced: true,
	}

	got := m.spendSummary()

	if got.WindowUSD != 1.12 {
		t.Errorf("WindowUSD = %v, want 1.12", got.WindowUSD)
	}
	if got.WindowLabel != "1h" {
		t.Errorf("WindowLabel = %q, want %q", got.WindowLabel, "1h")
	}
	// $1.12 over 60 minutes.
	if want := 1.12 / 60; got.BurnPerMin < want-1e-9 || got.BurnPerMin > want+1e-9 {
		t.Errorf("BurnPerMin = %v, want %v", got.BurnPerMin, want)
	}
	if !got.Priced {
		t.Error("Priced = false, want true")
	}
}

func TestSpendSummary_NothingPricedIsNotZero(t *testing.T) {
	// The distinction the whole strip rests on: an unknown cost is not a zero one.
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 10, PriceableRequests: 10},
		Priced: false,
	}

	got := m.spendSummary()

	if got.Priced {
		t.Error("Priced = true, want false")
	}
	if got.WindowUSD != 0 {
		t.Errorf("WindowUSD = %v; a caller must read Priced, not a sentinel", got.WindowUSD)
	}
	if got.Unpriced != 10 {
		t.Errorf("Unpriced = %d, want 10", got.Unpriced)
	}
}

func TestSpendSummary_CountsTheCoverageGap(t *testing.T) {
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{
			Requests: 318, CostMicros: 4_170_000,
			PricedRequests: 306, PriceableRequests: 318,
		},
		Priced: true,
	}

	got := m.spendSummary()

	// Priceable minus priced, NOT requests minus priced: Requests counts every
	// proxied response including MCP and health checks, and using it as the
	// denominator made a correctly configured deployment read a permanent warning.
	if got.Unpriced != 12 {
		t.Errorf("Unpriced = %d, want 12", got.Unpriced)
	}
	if got.Priceable != 318 {
		t.Errorf("Priceable = %d, want 318", got.Priceable)
	}
}

func TestSpendSummary_NoSnapshotYet(t *testing.T) {
	m := &model{}
	got := m.spendSummary()
	if got.Priced {
		t.Error("Priced = true with no snapshot; want false so the strip says nothing yet")
	}
}

func TestSpendSummary_NoSavedFigureUntilItIsMeasured(t *testing.T) {
	// Rendering "saved $0.00" would assert that pruning saved nothing, when the
	// truth is that nothing measures it yet. Same rule as "cost unavailable".
	m := &model{}
	m.spend.snap = &usage.Snapshot{
		Window: "1h",
		Totals: usage.Counts{Requests: 1, CostMicros: 100, PricedRequests: 1, PriceableRequests: 1},
		Priced: true,
	}

	if got := m.spendSummary(); got.HasSaved {
		t.Error("HasSaved = true; no Avoided data exists until a later commit")
	}
}

func TestSpendTick_StaleGenerationIsDropped(t *testing.T) {
	// The guard usage_pane.go:74-79 documents: two live chains each rescheduling
	// the other's successor doubles the request rate for the life of the session.
	m := &model{}
	m.spend.tickGen = 2

	if m.spendTickIsCurrent(1) {
		t.Error("a tick from generation 1 was accepted while generation 2 is current")
	}
	if !m.spendTickIsCurrent(2) {
		t.Error("the current generation's tick was dropped")
	}
}

func TestSpendLoaded_StaleReplyIsDropped(t *testing.T) {
	m := &model{}
	m.spend.reqSeq = 5
	fresh := &usage.Snapshot{Window: "1h"}

	m.applySpendLoaded(spendLoadedMsg{req: 4, snap: fresh})
	if m.spend.snap != nil {
		t.Error("a reply from an older request was applied")
	}

	m.applySpendLoaded(spendLoadedMsg{req: 5, snap: fresh})
	if m.spend.snap != fresh {
		t.Error("the current request's reply was dropped")
	}
}

var _ = time.Second // keep the import if unused above
```

Remove the `time` placeholder line if the final test file uses `time` for real.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/abctl/tui/ -run 'TestSpend' -v 2>&1 | tail -20`
Expected: FAIL — `m.spend undefined`, `m.spendSummary undefined`.

- [ ] **Step 3: Create `spend.go`**

```go
package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// spendPollInterval is how often the strip refreshes. Matches the Usage pane's
// cadence: the strip is glanceable chrome, not a live meter, and a faster poll
// would spend requests to move a figure the user is not watching.
const spendPollInterval = 20 * time.Second

// spendWindow is the span the strip's figure covers. One bucket at this
// resolution, so the server folds and the client does no arithmetic over buckets.
const (
	spendWindow     = time.Hour
	spendResolution = time.Hour
)

// spendState is the always-on spend summary behind the strip.
//
// Deliberately NOT m.usage. That state is pane-scoped: openUsage sets it, and it
// carries a user-chosen window and an optional single-session scope. The strip
// needs all-sessions data whenever abctl is running, whatever pane is showing —
// driving both from one chain would blank the strip the moment a user scoped the
// Usage pane to one session, which is exactly when they are looking at cost.
//
// The cost is one extra GET /v1/usage per 20s asking for a single bucket. Both
// chains can be alive at once; that is two cheap requests per interval, recorded
// here so it reads as a decision rather than an oversight.
type spendState struct {
	snap      *usage.Snapshot
	err       error
	lastFetch time.Time

	// reqSeq is the id of the most recently ISSUED request; a reply carrying a
	// smaller id is stale and dropped.
	reqSeq uint64
	// tickGen identifies the current polling chain. See usage_pane.go's tickGen:
	// two live chains each rescheduling the other's successor doubles the request
	// rate for the life of the session.
	tickGen uint64
}

type spendLoadedMsg struct {
	snap *usage.Snapshot
	req  uint64
	err  error
}

type spendTickMsg struct{ gen uint64 }

// spendSummary is what the strip renders.
//
// Optional fields rather than a narrower struct, because two of the spec's four
// figures are not measurable yet: "today" needs the durable ledger and "saved"
// needs the Avoided container. Later commits set HasToday / HasSaved and the
// renderer picks them up without gaining a branch — and until then the strip
// shows nothing for them rather than a zero, because "saved $0.00" asserts that
// pruning saved nothing when the truth is that nothing measures it.
type spendSummary struct {
	WindowUSD   float64
	WindowLabel string
	// BurnPerMin is 0 when it cannot be derived. Zero means "not enough data",
	// never "free".
	BurnPerMin float64
	// Priced reports whether ANY request in the window produced a cost. False
	// means render "cost unavailable" — never $0.00, which reads as free traffic.
	Priced    bool
	Unpriced  int64
	Priceable int64

	TodayUSD float64 // set once the cost ledger exists
	HasToday bool
	SavedUSD float64 // set once tool-prune savings are aggregated
	HasSaved bool
}

// spendSummary derives the strip's figures from the last snapshot.
func (m *model) spendSummary() spendSummary {
	snap := m.spend.snap
	if snap == nil {
		return spendSummary{}
	}
	out := spendSummary{
		WindowLabel: snap.Window,
		Priced:      snap.Priced,
		Priceable:   snap.Totals.PriceableRequests,
	}
	// Priceable minus priced, NOT requests minus priced. Requests counts every
	// proxied response — MCP calls, health checks — while only inference can be
	// priced, so the wrong denominator left a correctly configured deployment
	// reading a permanent warning with nothing to act on.
	if gap := snap.Totals.PriceableRequests - snap.Totals.PricedRequests; gap > 0 {
		out.Unpriced = gap
	}
	if snap.Priced {
		out.WindowUSD = float64(snap.Totals.CostMicros) / 1e6
		if mins := spendWindow.Minutes(); mins > 0 {
			out.BurnPerMin = out.WindowUSD / mins
		}
	}
	return out
}

// spendTickIsCurrent reports whether a tick belongs to the live chain.
func (m *model) spendTickIsCurrent(gen uint64) bool { return gen == m.spend.tickGen }

// applySpendLoaded stores a reply unless it is stale.
func (m *model) applySpendLoaded(msg spendLoadedMsg) {
	if msg.req != m.spend.reqSeq {
		return
	}
	m.spend.snap, m.spend.err, m.spend.lastFetch = msg.snap, msg.err, time.Now()
}

// startSpendPolling begins (or restarts) the chain, invalidating any previous one.
func (m *model) startSpendPolling() tea.Cmd {
	m.spend.tickGen++
	return tea.Batch(m.fetchSpend(), spendTick(m.spend.tickGen))
}

func spendTick(gen uint64) tea.Cmd {
	return tea.Tick(spendPollInterval, func(time.Time) tea.Msg { return spendTickMsg{gen: gen} })
}

// fetchSpend requests the all-sessions single-bucket snapshot off the render loop.
func (m *model) fetchSpend() tea.Cmd {
	if m.client == nil {
		return nil
	}
	client := m.client
	m.spend.reqSeq++
	req := m.spend.reqSeq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// group=none: the strip shows one number, so asking for a breakdown would
		// pay for a label map nothing renders.
		snap, err := client.GetUsage(ctx, spendWindow, spendResolution, "", usage.GroupNone)
		return spendLoadedMsg{snap: snap, req: req, err: err}
	}
}
```

- [ ] **Step 4: Add the model field and wire the messages**

In `app.go`'s `model` struct add `spend spendState`. In `Init()`, add `m.startSpendPolling()` to the returned batch. In `Update`, add cases:

```go
	case spendLoadedMsg:
		m.applySpendLoaded(msg)
		return m, nil
	case spendTickMsg:
		if !m.spendTickIsCurrent(msg.gen) {
			return m, nil
		}
		return m, tea.Batch(m.fetchSpend(), spendTick(msg.gen))
```

Read `Init()` and the `Update` switch first and follow their existing shape — if `Init` returns a `tea.Batch` already, add to it rather than replacing it.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/abctl/tui/ -run 'TestSpend' -v 2>&1 | tail -25`
Expected: PASS.

- [ ] **Step 6: Confirm the package still passes**

Run: `go test ./cmd/abctl/tui/ 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 7: Do not commit yet.** Task 4 commits.

---

## Task 2: The strip renderer and its degradation ladder

**Files:**
- Create: `cmd/abctl/tui/spend_strip.go`, `cmd/abctl/tui/spend_strip_test.go`

**Interfaces:**
- Consumes: `spendSummary` (Task 1), `formatUSD`/`formatUSDCell` (`tui/prune_saving.go:138,168`), `lipgloss.Width`, the `styleOK`/`styleWarn`/`styleMuted`/`styleTitle` set (`tui/styles.go`).
- Produces: `renderSpendStrip(s spendSummary, width int) string` — a pure function of its arguments, no model access. Task 3 calls it.

**Why a pure function:** every other chrome renderer here takes what it needs and returns a string (`fitHintLine`, `renderCostSummary`), which is what lets them be table-tested at many widths without building a `model`. The strip's whole risk is width behaviour, so it must be testable that way.

- [ ] **Step 1: Write the failing test**

Create `cmd/abctl/tui/spend_strip_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderSpendStrip_NeverExceedsTheWidth(t *testing.T) {
	// The strip lives in the chrome. A line that overflows wraps, and a wrapped
	// chrome line costs a row of the table below it -- the same failure
	// fitHintLine exists to prevent.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for _, w := range []int{120, 100, 80, 64, 48, 32, 24, 16, 8, 1} {
		got := renderSpendStrip(s, w)
		if gw := lipgloss.Width(got); gw > w {
			t.Errorf("width %d: rendered %d columns: %q", w, gw, got)
		}
		if strings.Contains(got, "\n") {
			t.Errorf("width %d: strip contains a newline: %q", w, got)
		}
	}
}

func TestRenderSpendStrip_DropsWholeFiguresNeverClipsANumber(t *testing.T) {
	// #953: "no truncated numbers". A half-rendered dollar amount is worse than a
	// missing one -- it reads as a real, smaller figure.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for _, w := range []int{120, 100, 80, 64, 48, 32, 24, 16, 8} {
		got := renderSpendStrip(s, w)
		if got == "" {
			continue
		}
		// Every dollar amount present must be present in full.
		for _, whole := range []string{"$1.12"} {
			if strings.Contains(got, "$1.1") && !strings.Contains(got, whole) {
				t.Errorf("width %d: %q contains a clipped %q", w, got, whole)
			}
		}
		if strings.HasSuffix(strings.TrimSpace(got), "$") {
			t.Errorf("width %d: %q ends mid-figure", w, got)
		}
	}
}

func TestRenderSpendStrip_WideEnoughShowsBothFigures(t *testing.T) {
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	got := renderSpendStrip(s, 120)
	for _, want := range []string{"SPEND", "$1.12", "1h", "/min"} {
		if !strings.Contains(got, want) {
			t.Errorf("strip %q missing %q", got, want)
		}
	}
}

func TestRenderSpendStrip_NarrowKeepsTheHeadlineFigure(t *testing.T) {
	// Degradation drops from the RIGHT: the leftmost figure is the headline and is
	// the last thing to go. (fitHintLine drops from the front for the opposite
	// reason -- its essential hints are last.)
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	got := renderSpendStrip(s, 24)
	if !strings.Contains(got, "$1.12") {
		t.Errorf("narrow strip %q dropped the headline figure", got)
	}
	if strings.Contains(got, "/min") {
		t.Errorf("narrow strip %q kept the burn rate; it should drop before the headline", got)
	}
}

func TestRenderSpendStrip_UnpricedSaysSoAndNeverShowsZero(t *testing.T) {
	s := spendSummary{WindowLabel: "1h", Priced: false, Unpriced: 12, Priceable: 318}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "unavailable") {
		t.Errorf("strip %q does not say cost is unavailable", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("strip %q renders $0.00 for an unknown cost", got)
	}
}

func TestRenderSpendStrip_PartiallyPricedDisclosesTheGap(t *testing.T) {
	// A dollar total covering only the priced subset must say so; presenting a
	// subtotal as the whole spend is the failure the coverage counters exist for.
	s := spendSummary{
		WindowUSD: 4.17, WindowLabel: "1h", BurnPerMin: 0.07,
		Priced: true, Unpriced: 12, Priceable: 318,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "12") {
		t.Errorf("strip %q does not disclose the 12 unpriced requests", got)
	}
}

func TestRenderSpendStrip_FullyPricedIsNotAnnotated(t *testing.T) {
	// A correctly configured deployment must not carry a permanent warning; that
	// is what trains an operator to ignore the one signal that matters.
	s := spendSummary{
		WindowUSD: 4.17, WindowLabel: "1h", BurnPerMin: 0.07,
		Priced: true, Unpriced: 0, Priceable: 318,
	}
	got := renderSpendStrip(s, 120)
	if strings.Contains(got, "unpriced") {
		t.Errorf("fully priced strip %q still warns about coverage", got)
	}
}

func TestRenderSpendStrip_NoDataYetRendersNothingUseful(t *testing.T) {
	// Before the first poll returns. An empty strip is honest; "$0.00" is not.
	got := renderSpendStrip(spendSummary{}, 120)
	if strings.Contains(got, "$0.00") {
		t.Errorf("strip %q renders $0.00 before any data arrived", got)
	}
}

func TestRenderSpendStrip_NoSavedFigureWhenUnmeasured(t *testing.T) {
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true, HasSaved: false}
	if got := renderSpendStrip(s, 120); strings.Contains(got, "saved") {
		t.Errorf("strip %q shows a saved figure with nothing measuring it", got)
	}
}

func TestRenderSpendStrip_ShowsSavedWhenMeasured(t *testing.T) {
	// Proves the field is wired now so a later commit adds data, not a branch.
	s := spendSummary{
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		SavedUSD: 0.24, HasSaved: true,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "saved") || !strings.Contains(got, "$0.24") {
		t.Errorf("strip %q does not show the measured saving", got)
	}
}

func TestRenderSpendStrip_ShowsTodayWhenAvailable(t *testing.T) {
	s := spendSummary{
		WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true,
		TodayUSD: 4.17, HasToday: true,
	}
	got := renderSpendStrip(s, 120)
	if !strings.Contains(got, "today") || !strings.Contains(got, "$4.17") {
		t.Errorf("strip %q does not show today's total", got)
	}
	// Today becomes the headline when present, so it must survive a narrow width.
	if narrow := renderSpendStrip(s, 24); !strings.Contains(narrow, "$4.17") {
		t.Errorf("narrow strip %q dropped today's total, which is the headline", narrow)
	}
}

func TestRenderSpendStrip_WideCharacterSafety(t *testing.T) {
	// footer.go:88-92 records the bug: a budget computed in display columns but
	// sliced by rune index overflowed on any wide character. The strip's own text
	// is ASCII, but the width arithmetic must be column-based regardless, because
	// the next figure added may not be.
	s := spendSummary{WindowUSD: 1.12, WindowLabel: "1h", BurnPerMin: 0.0187, Priced: true}
	for w := 1; w <= 60; w++ {
		if gw := lipgloss.Width(renderSpendStrip(s, w)); gw > w {
			t.Fatalf("width %d: rendered %d columns", w, gw)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/abctl/tui/ -run TestRenderSpendStrip -v 2>&1 | tail -20`
Expected: FAIL — `undefined: renderSpendStrip`.

- [ ] **Step 3: Create `spend_strip.go`**

Build the line as an ordered list of whole figures, then drop from the right until it fits. The ordering rule: the headline is leftmost and survives longest — the opposite direction from `fitHintLine`, and for the opposite reason (its essential hints are last; the strip's essential figure is first).

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// stripLabel prefixes the line. Kept short: it is spent on every width.
const stripLabel = "SPEND"

// renderSpendStrip draws the always-on spend line.
//
// A pure function of its arguments so it can be table-tested at many widths,
// which is where its entire risk lives. Chrome that overflows wraps, and a
// wrapped chrome line costs a row of the table below it.
//
// Degradation drops WHOLE FIGURES from the right and never clips a number.
// #953 asks for "no truncated numbers", and a half-rendered dollar amount is
// worse than a missing one: "$1.1" reads as a real, smaller figure rather than as
// an incomplete one. This is the same discipline as fitHintLine, mirrored —
// helpView orders its hints so the essential ones come last and fitHintLine cuts
// the front; the strip's essential figure is first, so it cuts the back.
func renderSpendStrip(s spendSummary, width int) string {
	if width <= 0 {
		return ""
	}

	// Nothing measured yet: say nothing rather than assert a zero.
	if !s.Priced && !s.HasToday {
		if s.Priceable > 0 {
			return fitStripFigures(stripLabel, []string{
				"cost unavailable",
				fmt.Sprintf("%d of %d unpriced", s.Unpriced, s.Priceable),
				"[u] usage",
			}, width)
		}
		return ""
	}

	// Ordered most to least important; the tail is what a narrow terminal loses.
	var figures []string
	if s.HasToday {
		figures = append(figures, formatUSDCell(s.TodayUSD)+" today")
	}
	figures = append(figures, formatUSDCell(s.WindowUSD)+" /"+s.WindowLabel)
	if s.BurnPerMin > 0 {
		figures = append(figures, "~"+formatUSDCell(s.BurnPerMin)+"/min")
	}
	if s.HasSaved {
		figures = append(figures, "saved "+formatUSDCell(s.SavedUSD))
	}
	// The coverage gap rides at the end: it qualifies the total, so it is the
	// first thing a narrow terminal gives up — but a fully priced deployment must
	// never carry it, because a permanent warning with nothing to act on is what
	// teaches an operator to ignore the one signal that matters.
	if s.Unpriced > 0 && s.Priceable > 0 {
		figures = append(figures, fmt.Sprintf("%d of %d unpriced", s.Unpriced, s.Priceable))
	}
	return fitStripFigures(stripLabel, figures, width)
}

// stripGap separates figures.
const stripGap = "   "

// fitStripFigures joins as many leading figures as fit, dropping whole ones from
// the right. Returns "" when even the first figure cannot fit without clipping.
//
// Width arithmetic is lipgloss.Width throughout, never len() or a rune count:
// footer.go records the bug that costs — a budget computed in display columns and
// then sliced by rune index overflowed on any wide character, rendering 55
// columns for a 40-column budget.
func fitStripFigures(label string, figures []string, width int) string {
	if len(figures) == 0 {
		return ""
	}
	for n := len(figures); n >= 1; n-- {
		candidate := label + "  " + strings.Join(figures[:n], stripGap)
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	// The label does not fit alongside even one figure. Drop the label before
	// dropping the number: the figure is the information, the label is decoration.
	if lipgloss.Width(figures[0]) <= width {
		return figures[0]
	}
	return ""
}
```

Style the line in Task 3 where it meets the rest of the chrome, or here with `styleMuted`/`styleTitle` if it does not complicate the width arithmetic — **lipgloss styles must be applied after fitting, or `lipgloss.Width` must be measured on the styled string.** Check which by running the width test.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/abctl/tui/ -run TestRenderSpendStrip -v 2>&1 | tail -30`
Expected: PASS, all twelve. If `TestRenderSpendStrip_NarrowKeepsTheHeadlineFigure` fails at width 24, check that dropping the label (the `fitStripFigures` tail branch) is reached.

- [ ] **Step 5: Do not commit yet.**

---

## Task 3: Put the strip on screen and pay for its row

**Files:**
- Modify: `cmd/abctl/tui/app.go:1229-1233` (`paneView`'s shared `JoinVertical`)
- Modify: `cmd/abctl/tui/keys.go:817-823` (`layout`'s height budget)
- Test: `cmd/abctl/tui/spend_strip_test.go` (append)

**Interfaces:**
- Consumes: `renderSpendStrip` (Task 2), `m.spendSummary()` (Task 1).
- Produces: `(*model).spendStripVisible() bool` and the rendered row. Nothing later depends on these.

**Why the height budget is the risky half:** `layout()` computes `bodyH := m.height - 3` under the comment "Reserve 3 rows for title + blank + footer lines". The arithmetic is actually title(1) + footer(2) = 3; there is no blank row, so the comment is already slightly wrong and the strip's row must be *added*, not borrowed. Get this wrong and every table renders one row too tall, pushing the footer off-screen.

Two panes never reach the shared render path: `paneNamespaces` and `panePods` return early at `app.go:1131-1174` with their own `JoinVertical`. They are pre-connection pickers with no session and no cost, so the strip must not draw there — but `layout()` sizes their tables from the same `bodyH`.

**Ruling, so the implementer does not have to decide it:** reserve the row **unconditionally** when the strip is enabled by height, and accept that the two picker panes render one row shorter than they could. Making `layout()` pane-aware would require calling it on every pane transition, which is a broader change than this commit should make, and a one-row-shorter picker is invisible next to a wrong-by-one-row events table. Below the fold threshold no row is reserved at all.

- [ ] **Step 1: Write the failing test**

```go
func TestSpendStripVisible_HiddenOnPreConnectionPickers(t *testing.T) {
	// The namespace and pod pickers run before any session exists, so there is no
	// cost to show. They also return early from paneView with their own layout.
	for _, p := range []paneID{paneNamespaces, panePods} {
		m := &model{height: 40}
		m.pane = p
		if m.spendStripVisible() {
			t.Errorf("pane %v: strip visible on a pre-connection picker", p)
		}
	}
}

func TestSpendStripVisible_ShownOnTheDataPanes(t *testing.T) {
	for _, p := range []paneID{paneSessions, paneEvents, paneDetail, panePipeline, paneUsage, paneCatalog} {
		m := &model{height: 40}
		m.pane = p
		if !m.spendStripVisible() {
			t.Errorf("pane %v: strip hidden on a data pane", p)
		}
	}
}

func TestSpendStripVisible_FoldsAwayOnAShortTerminal(t *testing.T) {
	// The spec's rule: below 20 rows the strip folds into the title bar rather
	// than spending a row the table needs more.
	//
	// "Folds into the title bar" is the spec's phrase and NOT what this asserts or
	// what shipped: below the threshold the strip is not drawn and the title carries
	// no figure. The shipped comment says "yields its row" for that reason. The
	// assertion itself is right — this only ever tested that the row is given back.
	m := &model{height: 19}
	m.pane = paneEvents
	if m.spendStripVisible() {
		t.Error("strip took a row on a 19-row terminal")
	}
	m.height = 20
	if !m.spendStripVisible() {
		t.Error("strip hidden at 20 rows, the documented threshold")
	}
}

func TestLayout_ReservesExactlyOneRowForTheStrip(t *testing.T) {
	// Get this wrong and every table renders one row too tall, pushing the footer
	// off-screen.
	tall := &model{width: 100, height: 40}
	tall.pane = paneEvents
	tall.layout()
	withStrip := tall.bodyHeight

	short := &model{width: 100, height: 40}
	short.pane = paneEvents
	short.height = 19 // below the fold threshold: no row reserved
	short.layout()

	// Compare like for like: recompute the tall case at the same height with the
	// strip suppressed.
	if withStrip != 40-4 {
		t.Errorf("bodyHeight with strip = %d, want %d (height - title - strip - 2 footer rows)", withStrip, 40-4)
	}
	if short.bodyHeight != 19-3 {
		t.Errorf("bodyHeight below the fold = %d, want %d (no strip row)", short.bodyHeight, 19-3)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/abctl/tui/ -run 'TestSpendStripVisible|TestLayout_Reserves' -v 2>&1 | tail -20`
Expected: FAIL — `m.spendStripVisible undefined`.

- [ ] **Step 3: Add the visibility rule**

In `spend_strip.go`:

```go
// spendStripMinHeight is the terminal height at which the strip earns its row.
//
// Below it the row is worth more to the table than to the chrome, so the strip
// yields it entirely rather than shrink the body further. 20 rows is roughly where
// an events table stops being able to show a turn's request and response together.
//
// The spec asks for the today figure to fold into the title bar at that point, and
// this doc comment originally repeated that. It does not happen: below the threshold
// nothing is drawn and the header is a bare styleTitle.Render(title). Carrying a
// figure into the title means budgeting title width against a pane name and an
// endpoint that already compete for it — a second degradation ladder, not a reuse of
// this one — so the honest subset shipped and the headline is lost on a short
// terminal. Say "yields its row", not "folds", anywhere this is described.
const spendStripMinHeight = 20

// spendStripVisible reports whether the strip takes a row in the current layout.
//
// False on paneNamespaces and panePods: those run before a connection exists, so
// there is no spend to report, and they return early from paneView with their own
// JoinVertical anyway.
func (m *model) spendStripVisible() bool {
	if m.height < spendStripMinHeight {
		return false
	}
	switch m.pane {
	case paneNamespaces, panePods:
		return false
	}
	return true
}
```

- [ ] **Step 4: Pay for the row in `layout`**

In `keys.go`, replace the budget computation:

```go
	// Reserve 1 row for the title and 2 for the footer (status + hint), plus 1 for
	// the spend strip when it is showing.
	//
	// The strip's row is ADDED, not borrowed: the previous comment here said
	// "title + blank + footer" but the arithmetic was title(1) + footer(2) = 3 and
	// there was never a blank row.
	//
	// Reserved unconditionally by height rather than per-pane, so the two
	// pre-connection pickers render one row shorter than they strictly need.
	// Making this pane-aware would mean re-running layout on every pane
	// transition; a one-row-shorter picker is invisible next to an events table
	// that is wrong by one row and pushes the footer off-screen.
	reserved := 3
	if m.spendStripVisible() {
		reserved++
	}
	bodyH := m.height - reserved
```

Note `spendStripVisible()` reads `m.pane`, and `layout()` is called from `WindowSizeMsg`. Since the reservation is by height only for the panes that matter, this is stable across pane changes among the data panes — but confirm no pane transition leaves a stale `bodyHeight` by checking whether `Update` calls `layout()` anywhere other than on resize. If a transition between a picker and a data pane can leave it stale, make `spendStripVisible()`'s pane check height-only (drop the pane switch from the *reservation* while keeping it in the *render*), and say so in your report.

- [ ] **Step 5: Render it**

In `app.go`'s `paneView`, replace the final join:

```go
	header := styleTitle.Render(title)
	if m.filtering {
		body = m.filterInput.View() + "\n" + body
	}
	rows := []string{header}
	if m.spendStripVisible() {
		if strip := renderSpendStrip(m.spendSummary(), m.width); strip != "" {
			rows = append(rows, styleMuted.Render(strip))
		}
	}
	rows = append(rows, body, m.footerView())
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
```

- [ ] **Step 6: Run the tests**

Run: `go test ./cmd/abctl/tui/ -run 'TestSpendStrip|TestLayout' -v 2>&1 | tail -25`
Expected: PASS.

- [ ] **Step 7: Prove the strip is actually wired, by mutation**

A render call nothing asserts on is the failure mode here. Verify by deletion: comment out the `rows = append(rows, styleMuted.Render(strip))` line, run the suite, and confirm at least one test fails. If none does, add a `View()`-level test that asserts the rendered output contains the strip's label for a model with a priced summary. Restore the line and confirm `git diff` shows it byte-identical. Record the before/after in your report.

- [ ] **Step 8: Do not commit yet.** Task 4 commits.

---

## Task 4: Cost and wall time on the sessions list, and commit

**Files:**
- Modify: `authlib/usage/usage.go` (`bucket` gains `bySession`; `foldInto` takes and records the session id)
- Modify: `authlib/usage/snapshot.go` (`GroupSession` in the consts, `ParseGroup`, `bucket.series`)
- Modify: `cmd/abctl/tui/spend.go` (the strip's fetch switches to `group=session`, serving both consumers from one request)
- Modify: `cmd/abctl/tui/sessions_pane.go:16-88` (`COST` and `SPAN` columns)
- Test: `authlib/usage/snapshot_test.go`, `cmd/abctl/tui/sessions_pane_test.go`

**Interfaces:**
- Consumes: `spendState.snap` (Task 1), `usage.Snapshot.Buckets[].Series` keyed by session id.
- Produces: `usage.GroupSession Group = "session"`; `(*model).sessionCost(id string) (usd float64, priced bool)` and `(*model).sessionSpan(id string) time.Duration`.

`sessionCost` shipped. **`sessionSpan` was never written**, because the column it would have fed never needed a method: `SPAN` was `UpdatedAt - CreatedAt` read straight off the session row, and `2f4c5d57` then removed the column. Nothing computes a session span in abctl today. Recorded because a reader looking for the helper should learn it is absent by design, not go looking for a deletion.

**Why this reverses an earlier decision:** the plan for commit 3 (`2026-09-13-cost-aggregation.md`) deferred `GroupSession`, reasoning that the grouping is only meaningful once #949 can tell concurrent agent sessions apart. That reasoning was about *interpretation*, and it was wrong to apply it to the *mechanism*: the sessions pane needs per-session cost now, and a `bySession` label map on the all-sessions ring is what makes it one request instead of N. The #949 caveat still holds for what the numbers *mean* on a laptop — several agents may share the `"default"` bucket — and that limitation belongs in the docs, not in a missing feature.

**Why one request, not two:** Task 1's fetch asked for `group=none` on the grounds that a breakdown nothing renders is wasted. Something renders it now, and `Totals` is unaffected by grouping (`snapshot.go:291-294` folds last, and totals are summed from raw buckets), so switching that one fetch to `group=session` serves the strip and the sessions table from a single poll. Update Task 1's comment when you change it, so it does not contradict the code.

- [ ] **Step 1: Write the failing test**

In `authlib/usage/snapshot_test.go`:

```go
func TestSnapshot_GroupSessionBreaksDownBySession(t *testing.T) {
	a := New()
	a.Record("sess-a", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	a.Record("sess-b", inferenceEvent("m", 20, 0, 0, 7, 0, 0b1001))

	// The ALL-sessions ring must carry the breakdown: one request has to answer
	// for every session, or a sessions list costs one request per row.
	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)

	if got := series["sess-a"].InputTokens; got != 10 {
		t.Errorf("sess-a InputTokens = %d, want 10", got)
	}
	if got := series["sess-b"].InputTokens; got != 20 {
		t.Errorf("sess-b InputTokens = %d, want 20", got)
	}
}

func TestSnapshot_GroupSessionOmitsAnEmptyID(t *testing.T) {
	a := New()
	a.Record("", inferenceEvent("m", 10, 0, 0, 5, 0, 0b1001))
	series := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)
	if _, ok := series[""]; ok {
		t.Error(`series has an "" key; an unattributed event must not render as a blank row`)
	}
}
```

In `cmd/abctl/tui/sessions_pane_test.go`:

```go
func TestSessionsTable_ShowsCostPerSession(t *testing.T) {
	m := &model{width: 120, height: 40}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3, TotalTokens: 100}}
	m.spend.snap = &usage.Snapshot{
		Window: "6h", Priced: true,
		Buckets: []usage.Bucket{{
			Series: map[string]usage.Counts{
				"sess-a": {Requests: 3, CostMicros: 250_000, PricedRequests: 3, PriceableRequests: 3},
			},
		}},
	}

	m.rebuildSessionsTable()

	row := m.sessionsTbl.Rows()[0]
	joined := strings.Join(row, " ")
	if !strings.Contains(joined, "0.25") {
		t.Errorf("row %v does not show the session's $0.25 cost", row)
	}
}

func TestSessionsTable_UnpricedSessionShowsNoZero(t *testing.T) {
	// The rule the whole feature rests on: an unknown cost is not a zero one, and
	// a table cell has even less room to explain itself than the strip does.
	m := &model{width: 120, height: 40}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: "sess-a", EventCount: 3, TotalTokens: 100}}
	m.spend.snap = &usage.Snapshot{Window: "6h", Priced: false}

	m.rebuildSessionsTable()

	row := m.sessionsTbl.Rows()[0]
	for _, cell := range row {
		if strings.Contains(cell, "$0.00") || cell == "0.0000" {
			t.Errorf("row %v renders a zero cost for an unpriced session", row)
		}
	}
}
```

- [ ] **Step 2: Run both to verify they fail**

Run: `go test ./authlib/usage/ -run TestSnapshot_GroupSession -v; go test ./cmd/abctl/tui/ -run TestSessionsTable_ -v 2>&1 | tail -20`
Expected: FAIL — `undefined: GroupSession`, and no cost cell.

- [ ] **Step 3: Add `bySession` to the aggregator**

`foldInto` does not currently receive the session id — `Record(sessionID string, e *pipeline.SessionEvent)` has it and calls `foldInto` for both the all-sessions ring and the per-session ring. Add a `sessionID string` parameter to `foldInto` and pass it from both call sites, then:

```go
	// Recorded on whichever ring is being folded, including the per-session one
	// where it is redundant — a uniform call site beats a conditional, and the
	// per-session map has exactly one key so it costs nothing.
	//
	// Empty is skipped: an unattributed event would render as a blank row, which
	// reads as a bug rather than as missing attribution.
	if sessionID != "" {
		addLabel(&b.bySession, truncateLabel(sessionID), one)
	}
```

Add the `bySession map[string]Counts` field to `bucket` with a comment noting it is bounded by `maxLabelsPerBucket` like the rest, and that session ids are caller-controlled so that bound is load-bearing.

Add `GroupSession Group = "session"` to the consts, a `ParseGroup` case, the `series` arm, and extend `ParseGroup`'s error string.

**Note for the `/v1/usage` disclosure block** (`sessionapi/usage.go:29-45`): `group=session` exposes session identifiers. On a laptop those are internal; in-cluster they may carry meaning. Add a bullet, for the same reason the block exists.

- [ ] **Step 4: Switch the strip's fetch and add the accessors**

In `spend.go`, change `usage.GroupNone` to `usage.GroupSession` in `fetchSpend`, and replace the `group=none` comment with why it is now `group=session` (one poll serving the strip's totals and the sessions table's per-row cost; `Totals` is unaffected by grouping).

Add:

```go
// sessionCost returns what one session cost over the strip's window.
//
// priced=false means no figure, which the caller must render as blank rather than
// as $0.00 — a table cell has even less room to explain itself than the strip.
func (m *model) sessionCost(id string) (usd float64, priced bool) {
	if m.spend.snap == nil || !m.spend.snap.Priced || id == "" {
		return 0, false
	}
	var micros int64
	var pricedReqs int64
	for _, b := range m.spend.snap.Buckets {
		c, ok := b.Series[id]
		if !ok {
			continue
		}
		micros += c.CostMicros
		pricedReqs += c.PricedRequests
	}
	if pricedReqs == 0 {
		return 0, false
	}
	return float64(micros) / 1e6, true
}

// sessionSpan is how long a session has been alive: UpdatedAt - CreatedAt.
//
// Both come from session.SessionSummary (authlib/session/store.go:336-343), which
// carries CreatedAt and UpdatedAt — verified, not assumed. Second resolution beats
// the alternative of deriving the span from the usage snapshot's buckets, which
// would be capped at the bucket width and would silently disagree with the
// UPDATED column right beside it.
//
// Note what this measures: elapsed wall time from first to last event, NOT time
// spent active. A session idle for an hour reports an hour. That is the honest
// reading of "how long has this session been going", and it matches what UPDATED
// already implies; a duty-cycle figure would need a different name.
func sessionSpan(s session.SessionSummary) time.Duration {
	if s.CreatedAt.IsZero() || !s.UpdatedAt.After(s.CreatedAt) {
		return 0
	}
	return s.UpdatedAt.Sub(s.CreatedAt)
}
```

A free function rather than a method: it needs no model state, so it is table-testable directly. Render it with the existing `relTime`-style compactness (`sessions_pane.go:111`) rather than a raw `time.Duration` string — `1h23m45.6s` in an 8-column cell is exactly the truncation the strip work exists to avoid.

**Cached-only rows** (`sessions_pane.go:66-72`) have no `SessionSummary`, so their `SPAN` cell is `""` — the same treatment their `COST` cell gets.

- [ ] **Step 5: Add the columns**

In `sessions_pane.go`, add `{Title: "COST", Width: 10}` and `{Title: "SPAN", Width: 8}` to `newSessionsTable`, and the matching cells in `rebuildSessionsTable`. Use `formatUSDCell` for cost and render `""` when `priced` is false. For the cached-only rows (`:66-72`) both cells are `""` — those sessions are ones the server no longer lists, so the aggregator has nothing for them either.

Widths are refined by `layout()`; check whether `layout` sets explicit sessions-table column widths and extend that if so.

- [ ] **Step 6: Run the tests**

Run: `go test ./authlib/usage/ ./cmd/abctl/tui/ 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 7: Mutation-check the wiring**

Same discipline as Task 3 Step 7: delete the COST cell from `rebuildSessionsTable`, confirm `TestSessionsTable_ShowsCostPerSession` fails, restore, confirm `git diff` is byte-identical. Record it.

- [ ] **Step 8: Full suite, vet, lint**

Run: `go test ./authlib/... ./cmd/abctl/... 2>&1 | grep -v "^ok\|no test files" | head -20`
Expected: no output beyond the known `cmd/abctl` `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` failure.

Run: `go vet ./authlib/... ./cmd/abctl/... && golangci-lint run --new-from-rev=main ./authlib/usage/... ./cmd/abctl/tui/... 2>&1 | tail -15`

Run: `gofmt -l authlib/usage authlib/sessionapi cmd/abctl/tui`
Expected: no output.

- [ ] **Step 9: Commit — this is commit 4 of the PR**

From the **worktree root**:

```bash
git add authbridge/authlib/usage authbridge/authlib/sessionapi authbridge/cmd/abctl/tui \
        authbridge/docs/superpowers/plans/2026-09-13-spend-strip.md
git commit -s -m "Feat: Show spend unconditionally in abctl, and per-session cost

Cost was a place you navigated to. The Usage pane had no cost metric at all,
the sessions list had no cost column, and the only money on screen was a
per-row figure in the events table -- which was blank on a default install.

A one-row strip now sits under the title bar on every data pane, showing
window spend and burn rate. It degrades by dropping whole figures from the
right and never clips a number: a half-rendered \"\$1.1\" reads as a real,
smaller figure, which is worse than a missing one. It never renders \$0.00
for an unknown cost, and a fully priced deployment carries no coverage
warning -- a permanent warning with nothing to act on is what teaches an
operator to ignore the one signal that matters.

Two of the strip's four figures are deliberately absent rather than faked.
\"Today\" needs the durable cost ledger and \"saved\" needs tool-prune
attribution; both land in later commits. Until then the strip shows neither,
because \"saved \$0.00\" asserts that pruning saved nothing when the truth is
that nothing measures it yet. The summary struct already carries the fields,
so those commits add data rather than render branches.

The strip's row is added to the height budget, not borrowed. The comment
there claimed to reserve \"title + blank + footer\" but the arithmetic was
title plus two footer rows; there was never a blank row to take.

GroupSession lands here rather than with the other groupings: the sessions
list needs per-session cost, and a bySession label map on the all-sessions
ring answers for every row in one request instead of one per row. What the
numbers MEAN on a laptop is still limited by #949 -- concurrent agents can
share one session id -- but that is a documentation matter, not a reason to
omit the mechanism.

Refs #950, #953

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage for commit 4:** always-on strip in the chrome (Tasks 2–3) ✓; header placement above the body (Task 3) ✓; degradation by whole figures (Task 2) ✓; never `$0.00` for unknown (Tasks 2, 4) ✓; ~~fold below 20 rows (Task 3) ✓~~; sessions pane `COST` + wall time (Task 4) — `COST` ✓, wall time ✗.

**Two ticks corrected.** *Fold below 20 rows*: the row is yielded, which is the half of that requirement that carries the terminal-height argument; the today figure is NOT carried into the title bar, so the headline is lost rather than preserved. *Wall time*: `SPAN` shipped and `2f4c5d57` removed it as near-redundant with `UPDATED`, so no wall-time figure is on the sessions list at all. Both were ticked because the task was done as written; neither was checked against what the spec asked the task to achieve. That gap — tick the step, not the requirement — is what let a plan marked implemented overstate two deliverables at once.

**Deliberately not in this commit, and not faked:** the "today" headline (needs commit 5's ledger) and the "saved" figure (needs commit 7's `Avoided`). Task 1's struct carries both fields and Task 2 has passing tests for both render paths, so the later commits supply data only.

**Type consistency:** `spendSummary` floats are dollars; `usage.Counts.CostMicros` is millionths and every conversion is an explicit `/1e6`. ~~`sessionSpan` returns `time.Duration`.~~ (No `sessionSpan` shipped — see Task 4.) `renderSpendStrip(s spendSummary, width int) string` is the single signature Task 3 calls, and is what shipped.

**Deviation from the commit-3 plan, ruled here:** `GroupSession` was deferred there and lands here, with the reasoning in Task 4's Why. If commit 3 has already shipped when this is implemented, that is a `ParseGroup` case and a `series` arm added on top of it, not a conflict.


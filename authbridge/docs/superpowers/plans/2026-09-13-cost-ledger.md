# Cost Ledger Implementation Plan (commit 5)

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

> **STATUS: implemented.** Landed as `6c7c7fe0` (core) / `9f0e35b0` (abctl).
>
> An earlier revision of this banner cited `4765644f`, which is not an ancestor of this
> branch: it is the same change on an abandoned branch that was never merged. A banner
> pointing at an unreachable commit is worse than no banner, because a reviewer who
> looks it up concludes the doc is describing someone else's tree.
>
> **Five later commits changed what this plan describes.** Each is noted again at the
> place where this plan says the thing that changed:
>
> - `ae2c7f41` — the open minute. This plan says the in-memory usage ring supplies it.
>   It does not and could not; `Writer.pending()` does, and `Window()` stitches. Until
>   that commit `window=today` under-reported, silently and indefinitely once traffic
>   stopped. `costledger/row.go`'s package doc names this plan's claim as the error an
>   earlier draft made and gives four reasons the ring cannot be the source.
> - `97104114` — `Record` was taken off the request path entirely. No IO at all now,
>   not "one append per minute roll".
> - `85aa46d9` — a corrupt ledger line is skipped and reading continues, rather than
>   ending the day's read at that point.
> - `6870ad3b` — the `agent` column, which this plan deliberately ships empty, is
>   populated and part of the row key.
> - `758e0148` (core) / `168b1478` (abctl) — the ring-versus-ledger difference this plan treats as an internal
>   detail became a user-visible disclosure on `/v1/usage` and in `costledger`'s doc.
>
> **Line numbers drift.** Every `file.go:NN` below was accurate when written and many
> have moved. Read them as "roughly here" and find the symbol by name.
>
> ---
>
> **SECOND SWEEP, 2026-09-15. "Five later commits" is an undercount: eighteen have
> touched `authlib/costledger` since `6c7c7fe0` (core) / `9f0e35b0` (abctl).** The five above are the ones that
> reversed a *claim*; these changed the *code this plan prints*, so an implementer
> copying a block out of it would write something the tree has already rejected. Each is
> repeated at the block it affects.
>
> - `f49803b2` and `3481e040` — **the admission guard.** This plan's `Record` drops
>   everything with a nil `Inference`. The shipped predicate is "this event is inference
>   **or** somebody priced it", so a response the parser could not read but the gateway
>   charged for is recorded, with `Model: ""` and no token counts. Task 1 Step 4 and
>   `TestWriter_NonInferenceTrafficIsIgnored` both describe the old rule.
> - `47cbd8af` — **the write path no longer rolls back, and retention gained a floor.**
>   No `Stat`, no `Truncate`; the injected `dayFile` interface omits both so no future
>   edit can quietly reintroduce them, because a rollback computed from a size can
>   destroy rows *another* writer appended. A torn append costs one line, fenced off with
>   a single newline byte, which the reader then skips and counts. Retention's cutoff is
>   floored at the newest day file's own date: a forward clock step used to delete
>   everything including today (`TestPrune_AForwardClockStepDoesNotDeleteTheLedger`), and
>   the cutoff is `ref` minus `retainDays-1`, so `retainDays` **files** survive counting
>   today rather than `retainDays+1`.
> - `eecd52b7` — **the day zone is pinned and the append is fsynced.** One `store.loc`
>   decides every day boundary, on write, on read and on prune, because a row filed under
>   a date no query for that local day visits is written, retained and invisible. `Window`
>   drops a disk row for the held minute on **equality**, not "at or after": the earlier
>   `>=` discarded every newer disk row (measured: 250,000 micros returned instead of
>   1,250,000).
> - `dd6e4703` — **nothing is lost silently.** `Dropped()`, `SkippedLines()` and
>   `TruncatedDays()` are exported; the last two are **gauges for the most recent read**,
>   not cumulative counters, which is why `sessionapi` samples them *after* `Window` and
>   not before.
> - `7fd5885e` — **label length and per-minute cardinality are capped** (`maxLabelLen`
>   96, `maxLabelsPerMinute` 64, overflow folded into `(other)`). The labels come off a
>   request, so their length and their count are chosen off-host; capping on the write
>   side is also what makes a line longer than the reader's `maxLineBytes` unreachable.
> - `1c714e68` — **`ledgerSnapshot` now emits `Snapshot.Degraded`**, a pointer carrying
>   the two gauges above, so a day that lost lines no longer produces a response
>   byte-identical to a clean one.
>
> **Signatures this plan does not list.** `Writer` also has `Window`, `Dropped`,
> `SkippedLines`, `TruncatedDays`, and the options `WithRetentionDays` and
> `WithSettleInterval` — the last because a minute that ends and is followed by no traffic
> has to be settled by a timer rather than by the next event. `newStore` takes
> `(dir, retainDays, loc)`; `store` has no `close`; `readDay` returns
> `([]Row, dayIssues, error)` and scans line by line with `bufio.Scanner` rather than
> decoding a stream. **`store_test.go` was never created** — the file-layout, retention and
> corrupt-line tests the File Structure table assigns to it live in `writer_test.go` and
> `query_test.go`.
>
> **The config block gained a key and a floor.** `cost_ledger` is
> `enabled *bool` + `dir string` + `retention_days int`; a non-zero `retention_days`
> below **8** is **refused at load** rather than clamped, because `window=7d` is served
> from these files and a rolling 7×24h span touches EIGHT local day files unless it begins
> exactly at midnight. The floor is derived from the window
> (`minCostLedgerRetentionDays = usage.Window7dLocalDays`) rather than written as a
> literal — as a literal it said 7, shipped, and admitted the partial week it existed to
> refuse, while the paragraph beside it already said eight. `0` still means the package
> default of 30. It is **not
> hot-reloadable**: the reloader holds no reference to it and the writer is opened once at
> startup, so every key here takes effect on restart. Say that where an operator will read
> it — someone who edits `enabled: false` in a live config has every reason to believe the
> writing stopped.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make "what did today cost" answerable, and make it survive `abctl service restart`.

**Architecture:** A new `authlib/costledger` package registers as a **second session Recorder** alongside the usage aggregator, accumulates one composite row per (endpoint, model, agent, provenance) per minute, and appends closed minutes to `~/.cortex/cost/YYYY-MM-DD.jsonl`. A reader answers spans longer than the in-memory ring. The proxy wires it on for `--local` and off in Kubernetes. `abctl` gains a `cost` subcommand and the strip's headline becomes "today".

**Tech Stack:** Go 1.x, standard library only (`encoding/json`, `os`, `time`, `sync`). No new dependencies.

**Spec:** `authbridge/docs/superpowers/specs/2026-09-13-cost-first-class-design.md`

## Global Constraints

- **Module root is `authbridge/`.** Go commands run from `authbridge/`, relative to the repo root; `git` from the repo root itself.
- **`git commit -s` mandatory.** Trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; never `Co-Authored-By`.
- **Field names come from `usage.Counts`**, verbatim, so the ledger, `/v1/usage` and the future collector share one vocabulary. Do not invent ledger-side spellings.
- **The ledger holds NO prompt content.** Hosts, model names, counts, dollars, timestamps. Nothing else, ever. This is a user-facing promise in the docs.
- **The ring and the ledger must never both own a minute.** The ledger holds only *closed* minutes on disk. A restart mid-minute loses ≤60s and that is documented, not hidden.

  **The second half of this constraint as originally written — "the in-memory ring supplies the open one" — is wrong, and following it is what produced the defect `ae2c7f41` fixed.** The open minute comes from THIS package's own accumulator: `Writer.pending()` returns it and `Window()` stitches the two halves. `ledgerSnapshot` never reads the ring at all. `costledger/row.go`'s package doc names this claim as the error an earlier draft of that doc made, and gives four reasons the ring cannot be the source — it prices independently via `usage.Aggregator.costOf`, its request denominator counts MCP and health traffic this package excludes, it is only 6h deep so a minute held overnight has already rotated out of it, and it knows nothing about what has been flushed, which would make ownership a timing question rather than a provable boundary. Read that doc before touching either side of the seam.
- **On by default for `--local`, off in Kubernetes.** Writing files inside a pod is wrong; the central collector is the right sink there.
- **"Today" means local midnight to now, in the machine's timezone.** A laptop crosses timezones and a UTC day would reset mid-afternoon.
- **Never `$0.00` for an unknown cost.** Unpriced minutes contribute no cost and are visible as the priced/priceable gap.
- **A ledger failure must never break the proxy.** It is observability. Every write path degrades to a logged warning; no error propagates into request handling.
- **Large test files must be written in SEVERAL SMALL tool calls**, never one big `Write`. A single oversized write exceeds this environment's stream watchdog and the agent dies mid-call, deterministically. This already killed one dispatch on this plan.
- Do NOT run `make lint`. Do NOT `gofmt -w .` at the module root.
- Known pre-existing failure on this machine: `cmd/abctl`'s `TestRunExec_BeforeFirstStartRunsAndSaysWhatIsLost` — a TEST-ISOLATION BUG: the fixture is deliberately bundle-less, but the machine's own `~/.cortex/ca/bundle.crt` leaks through; fails on `main` too.
- Comment register: long comments explaining *why*, naming the bug the code prevents.

## Design decision made during planning — read before Task 1

The spec implies the ledger reads the usage aggregator's closed buckets. **It does not.** `session.Recorder` is a one-method interface — `Record(sessionID string, e *pipeline.SessionEvent)` (`authlib/session/store.go:43-45`) — and `Store.AddRecorder` (`:307-309`) accepts any number of them. `usage.Aggregator` is already registered that way.

So the ledger registers as a **second, independent Recorder**. Two reasons this is better than reading the aggregator:

1. **It can store the exact cross-product.** The aggregator keeps independent *marginals* — `byMethod`, `byStatus`, `byPlugin`, `byEndpoint`, `bySession` — not a joint distribution. Reading it could never reconstruct "this endpoint × this model × this provenance", and summing marginals would double-count. Its own godoc explains why the per-plugin series intentionally sums to more than `Requests`.
2. **`usage` needs no changes at all**, so the ledger cannot regress charting.

Cost: the ledger decodes each event's cost record itself, via `costevent.Record`. That is a second read of one small JSON blob per response — not a second pricing decision, which is what #972 forbids. The figure is whatever `inference-parser` settled; the ledger never prices anything.

**The `agent` column stays empty until commit 6** (it needs `EventClient` from the User-Agent). Write the field, leave it `""`, and let commit 6 populate it. Do not omit the field — a schema that gains a column later is worse than one that has an empty column now.

That commit is `6870ad3b` and it landed: the column is populated and is part of the row key, so two agents on one endpoint and model in one minute are two rows. It kept `""` as the storage for *absence* rather than adopting the aggregator's display string `"unknown"` — a durable file must not bake a display value into a field where it becomes permanently indistinguishable from an agent that really called itself that — and `labelFor` maps `""` back to `"unknown"` at the query boundary, so `group=agent` returns the same key whether it was served from the ring or from disk.

---

## File Structure

| file | responsibility | task |
|---|---|---|
| `authlib/costledger/row.go` | the wire row and its key | 1 |
| `authlib/costledger/writer.go` | Recorder impl, per-minute accumulation, flush on roll | 1 |
| `authlib/costledger/writer_test.go` | accumulation, roll, degradation on IO failure | 1 |
| `authlib/costledger/store.go` | day-file append, rotation, retention | 1 |
| `authlib/costledger/store_test.go` | file layout, retention, corrupt-line tolerance | 1 |
| `authlib/costledger/query.go` | read a span, fold to `usage.Counts`, group | 2 |
| `authlib/costledger/query_test.go` | span selection, grouping, today boundary | 2 |
| `authlib/usage/snapshot.go` | `window=today\|7d` parsing | 3 |
| `authlib/sessionapi/usage.go` | serve ledger-backed windows | 3 |
| `cmd/authbridge-proxy/main.go`, `local.go` | wire on for `--local`, off in-cluster | 3 |
| `cmd/abctl/cmd_cost.go` + test | `abctl cost [--json]` | 4 |
| `cmd/abctl/tui/spend.go` | `TodayUSD`/`HasToday` from the ledger-backed window | 4 |

---

## Task 1: Write closed minutes to disk

**Files:**
- Create: `authlib/costledger/row.go`, `writer.go`, `store.go`, and their tests

**Interfaces:**
- Consumes: `session.Recorder`, `pipeline.SessionEvent`, `costevent.Record`, `usage.Counts`.
- Produces:
  ```go
  type Row struct {
      At         time.Time `json:"at"`
      Endpoint   string    `json:"endpoint,omitempty"`
      Model      string    `json:"model,omitempty"`
      Agent      string    `json:"agent,omitempty"`     // empty until commit 6
      Provenance string    `json:"provenance,omitempty"`
      usage.Counts         // embedded: one vocabulary, no synonyms
  }
  func New(dir string, opts ...Option) (*Writer, error)
  func (w *Writer) Record(sessionID string, e *pipeline.SessionEvent) // session.Recorder
  func (w *Writer) Flush() error   // force the open minute out; for tests and shutdown
  func (w *Writer) Close() error
  ```
  Task 2 reads what this writes; Task 3 constructs it.

**Why embed `usage.Counts` rather than restate the fields:** the spec's rule is one vocabulary from parser to aggregate to ledger to collector. Embedding makes divergence a compile error instead of a review question, and it means the four-way token split and the coverage counters arrive for free. It also means `Counts.Add` is the summation for both.

- [ ] **Step 1: Write the failing test for accumulation and roll**

Create `authlib/costledger/writer_test.go`. Build it up in a few small calls, not one:

```go
package costledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
)

// at is a fixed clock instant used across these tests. Deliberately not UTC
// midnight-adjacent, so a timezone bug does not accidentally pass.
var at = time.Date(2026, 9, 13, 9, 14, 30, 0, time.Local)

// costedEvent builds a response event carrying a settled cost record, the way
// inference-parser publishes it.
func costedEvent(t *testing.T, host, model string, costUSD float64, in, out int) *pipeline.SessionEvent {
	t.Helper()
	rec, err := json.Marshal(costevent.Event{
		CostUSD: costUSD, Settled: true,
		Source: costevent.SourceUsageFallback, Provenance: "bundled",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &pipeline.SessionEvent{
		At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: host,
		Inference: &pipeline.InferenceExtension{
			Model: model, InputTokens: in, OutputTokens: out,
			TotalTokens: in + out, PresentKinds: 0b1001,
		},
		Plugins: map[string]json.RawMessage{costevent.Key: rec},
	}
}

func TestWriter_AccumulatesTheOpenMinuteWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	w := newTestWriter(t, dir, func() time.Time { return at })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// The open minute belongs to the in-memory ring, not to the ledger. Writing it
	// would mean the same minute existed in two places and a reader stitching them
	// would double-count.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files while the minute was still open; want 0", len(entries))
	}
}

func TestWriter_FlushesOnMinuteRoll(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))

	// Advance past the minute boundary and record again: the previous minute closes.
	now = at.Add(time.Minute)
	w.Record("s1", costedEvent(t, "gw", "m", 0.10, 10, 5))

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the one closed minute)", len(rows))
	}
	if rows[0].Requests != 2 {
		t.Errorf("Requests = %d, want 2 accumulated", rows[0].Requests)
	}
	if rows[0].CostMicros != 500_000 {
		t.Errorf("CostMicros = %d, want 500000", rows[0].CostMicros)
	}
	if rows[0].InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200", rows[0].InputTokens)
	}
}

func TestWriter_SeparateRowPerCompositeKey(t *testing.T) {
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw-a", "opus", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw-b", "opus", 0.25, 100, 50))
	w.Record("s1", costedEvent(t, "gw-a", "haiku", 0.05, 10, 5))

	now = at.Add(time.Minute)
	w.Record("s1", costedEvent(t, "gw-a", "opus", 0.01, 1, 1))

	rows := readAllRows(t, dir)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 distinct (endpoint, model) keys", len(rows))
	}
}

func TestWriter_ProvenanceIsPartOfTheKey(t *testing.T) {
	// A single minute can mix a gateway's own figures with modelled ones, and one
	// provenance per row would have to pick a winner. Keying on it keeps PricedBy
	// reconstructible from the ledger exactly as /v1/usage reports it.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	a := costedEvent(t, "gw", "m", 0.25, 100, 50)
	b := costedEvent(t, "gw", "m", 0.25, 100, 50)
	setProvenance(t, b, "authoritative")
	w.Record("s1", a)
	w.Record("s1", b)

	now = at.Add(time.Minute)
	w.Flush()

	rows := readAllRows(t, dir)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — one per provenance", len(rows))
	}
}

func TestWriter_UnpricedTrafficIsRecordedWithoutCost(t *testing.T) {
	// An unpriced request still happened. It contributes no dollars and shows up as
	// the priced/priceable gap — dropping it would make the gap invisible and the
	// total look complete.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	delete(e.Plugins, costevent.Key) // no settled cost
	w.Record("s1", e)

	now = at.Add(time.Minute)
	w.Flush()

	rows := readAllRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", rows[0].CostMicros)
	}
	if rows[0].PricedRequests != 0 {
		t.Errorf("PricedRequests = %d, want 0", rows[0].PricedRequests)
	}
	if rows[0].PriceableRequests != 1 {
		t.Errorf("PriceableRequests = %d, want 1 — it carried a model and tokens", rows[0].PriceableRequests)
	}
}

func TestWriter_NonInferenceTrafficIsIgnored(t *testing.T) {
	// MCP calls, health checks, tunnel opens. Recording them would put every
	// proxied response in the ledger and in the cost denominator.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", &pipeline.SessionEvent{At: at, Phase: pipeline.SessionResponse, StatusCode: 200, Host: "gw"})

	now = at.Add(time.Minute)
	w.Flush()

	if rows := readAllRows(t, dir); len(rows) != 0 {
		t.Errorf("got %d rows for non-inference traffic, want 0", len(rows))
	}
}

func TestWriter_HoldsNoPromptContent(t *testing.T) {
	// A user-facing promise in the docs. Assert on the serialized bytes, because
	// that is what lands on disk.
	dir := t.TempDir()
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	e := costedEvent(t, "gw", "m", 0.25, 100, 50)
	e.Inference.Messages = []pipeline.InferenceMessage{{Role: "user", Content: "SECRET-PROMPT-TEXT"}}
	e.Inference.Completion = "SECRET-COMPLETION-TEXT"
	w.Record("s1", e)

	now = at.Add(time.Minute)
	w.Flush()

	raw := readAllBytes(t, dir)
	for _, secret := range []string{"SECRET-PROMPT-TEXT", "SECRET-COMPLETION-TEXT", "Messages", "completion"} {
		if bytesContains(raw, secret) {
			t.Errorf("ledger bytes contain %q", secret)
		}
	}
}

func TestWriter_IOFailureDoesNotPropagate(t *testing.T) {
	// The ledger is observability. A full disk or a read-only home must never turn
	// into a failed request.
	dir := filepath.Join(t.TempDir(), "unwritable")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := at
	w := newTestWriter(t, dir, func() time.Time { return now })

	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
	now = at.Add(time.Minute)
	// Must not panic and must not block. Flush may return an error; Record never
	// surfaces one.
	_ = w.Flush()
	w.Record("s1", costedEvent(t, "gw", "m", 0.25, 100, 50))
}
```

Write the helpers `newTestWriter`, `readAllRows`, `readAllBytes`, `bytesContains`, `setProvenance` in a **separate small call** to the same file. `newTestWriter` calls `New(dir, WithClock(fn))` and `t.Cleanup(w.Close)`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./authlib/costledger/ 2>&1 | tail -10`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Create `row.go`**

```go
// Package costledger persists per-minute cost and token totals to disk so a
// question like "what did today cost" survives a restart.
//
// It exists because the usage aggregator's ring is 6 hours of in-memory buckets
// (usage.NumBuckets x usage.BucketWidth) and dies with the process. A coding
// session spans days; the release bar asks for numbers that are still there
// tomorrow.
//
// It is a SECOND session.Recorder, registered alongside the usage aggregator,
// rather than a reader of it. The aggregator keeps independent marginals —
// by-model, by-endpoint, by-plugin — not a joint distribution, so reading it
// could never reconstruct "this endpoint x this model x this provenance", and
// summing marginals would double-count. Recording independently also means this
// package cannot regress charting.
//
// It prices NOTHING. Every figure here is the one inference-parser settled and
// published on the event; this package only decodes and adds. That is the
// invariant cortex #972 exists to protect: one component turns tokens into
// dollars.
//
// On disk it holds hosts, model names, counts, dollars and timestamps. No prompt
// content, no completions, no tool arguments — ever. That is a user-facing promise
// and TestWriter_HoldsNoPromptContent asserts it against the serialized bytes.
package costledger

import (
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Row is one minute's totals for one (endpoint, model, agent, provenance) key.
//
// usage.Counts is EMBEDDED rather than restated so the ledger, /v1/usage and any
// future collector share one vocabulary: a field added there appears here, and a
// divergence is a compile error instead of a review question. It also means
// Counts.Add is the summation for both.
type Row struct {
	At time.Time `json:"at"`
	// Endpoint is the target host. Half of the rate key: the same model bills
	// differently on a discounted gateway than on the vendor endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	Model    string `json:"model,omitempty"`
	// Agent identifies the calling coding agent, as "name/version".
	//
	// EMPTY until the change that captures the client from the User-Agent. The
	// field ships now, unpopulated, because a schema that gains a column later is
	// worse for every reader than one that has an empty column from the start.
	Agent string `json:"agent,omitempty"`
	// Provenance is part of the KEY, not a summary of the row: one minute can mix a
	// gateway's own figures with modelled ones, and a single provenance per row
	// would have to pick a winner. Keying on it keeps /v1/usage's pricedBy
	// reconstructible from the ledger.
	Provenance string `json:"provenance,omitempty"`

	usage.Counts
}

// key is the in-memory accumulation key. Mirrors the JSON identity fields exactly.
type key struct {
	endpoint, model, agent, provenance string
}

func (r Row) key() key {
	return key{r.Endpoint, r.Model, r.Agent, r.Provenance}
}
```

- [ ] **Step 4: Create `writer.go`**

Implement in one modest call:

```go
package costledger

import (
	"log/slog"
	"sync"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Writer accumulates the open minute in memory and appends closed minutes.
//
// Only CLOSED minutes reach disk. The open one belongs to the in-memory ring, and
// a reader stitches the two — so a minute never exists in both places and cannot
// be double-counted. The cost of that boundary is that a restart mid-minute loses
// up to 60 seconds of cost, which is documented rather than hidden.
type Writer struct {
	store *store
	now   func() time.Time

	mu sync.Mutex
	// open is the minute currently accumulating, truncated to the minute.
	open time.Time
	rows map[key]*Row
}

// Option configures a Writer.
type Option func(*Writer)

// WithClock replaces the time source, for tests.
func WithClock(fn func() time.Time) Option { return func(w *Writer) { w.now = fn } }

// New opens a ledger under dir, creating it if needed.
func New(dir string, opts ...Option) (*Writer, error) {
	w := &Writer{now: time.Now, rows: map[key]*Row{}}
	for _, o := range opts {
		o(w)
	}
	s, err := newStore(dir)
	if err != nil {
		return nil, err
	}
	w.store = s
	return w, nil
}

// Record implements session.Recorder.
//
// Never returns an error and never blocks on IO beyond one append per minute
// roll: this runs on the synchronous session-append path, and the ledger is
// observability. A failure here must not become a failed request.
//
// SUPERSEDED by 97104114: "beyond one append per minute roll" was still one
// filesystem write on the request path, and that path holds session.Store's WRITE
// LOCK, so every other request in the proxy queues behind it. Record now does NO IO
// at all — one mutex, one map operation, and at most one non-blocking channel send.
// A background goroutine owns the filesystem and is the only thing that touches it
// after construction, which also removes the need for any lock around the day files.
//
// The send is non-blocking, so a full queue DROPS rather than waits: the only way to
// fill a 1024-deep buffer is a filesystem that has stopped keeping up, and the
// alternative to losing a minute of cost history is stalling every proxied request
// until the disk comes back. Drops are counted, warned about, and exposed on
// Dropped() — an undisclosed drop is how a total quietly becomes wrong.
func (w *Writer) Record(_ string, e *pipeline.SessionEvent) {
	if e == nil || e.Inference == nil {
		// Non-inference traffic the proxy handled — MCP, health checks, tunnels.
		// Recording it would put every proxied response in the cost denominator,
		// the mistake that made a correct deployment read "1/10 priced" forever.
		return
	}
	// Response events only: a request event has no token counts and no cost, so
	// folding it would double the request count for every turn.
	if e.Phase != pipeline.SessionResponse {
		return
	}

	// SUPERSEDED by f49803b2 and 3481e040, and this guard is the part of the plan an
	// implementer must NOT copy. Both halves of it are wrong.
	//
	// "Inference == nil" was standing in for "not priceable", and it is not the same
	// predicate. inference-parser populates Extensions.Inference only for the endpoints
	// it can read, and leaves it nil for /v1/embeddings, /v1/rerank, /v1/moderations,
	// anything else a gateway mounts, and any response whose body was empty or
	// unparseable — while still settling a cost for all of them from the gateway's own
	// response header, which needs neither a model nor a body. Returning here made every
	// one of those free: the spend reached no /v1/usage total, no ledger and no budget.
	// The shipped predicate is "this event is inference OR somebody priced it", named
	// rather than inlined so the guard reads as that rule instead of as a nest of
	// negations:
	//
	//	ev, hasCost := costevent.Record(e)
	//	settledCost := hasCost && ev.Priced()
	//	if e.Inference == nil && !settledCost { return }
	//
	// PRICED, not merely present: a record that exists but priced nothing is still
	// non-inference traffic. The consequence to carry forward is that a row can now hold
	// Model: "" and no token counts at all; the shipped test is named
	// TestRecord_APricedResponseWithNoInferenceExtensionIsStillRecorded.
	// TestWriter_NonInferenceTrafficIsIgnored below still holds, because its fixture
	// carries no cost record; the shipped test for the other half is
	// TestRecord_AnUnpricedNonInferenceResponseIsStillIgnored.
	//
	// And the phase gate admits SessionDenied as well as SessionResponse. As written it
	// does not, which made the `e.Phase == pipeline.SessionDenied` arm of the Errors
	// assignment below unreachable — a denial is a response that happened and can carry
	// a settled cost.

	minute := e.At.Truncate(time.Minute)
	r := Row{
		At:       minute,
		Endpoint: e.Endpoint(),
		Model:    e.Inference.Model,
		Counts: usage.Counts{
			Requests:         1,
			InputTokens:      int64(e.Inference.InputTokens),
			CacheReadTokens:  int64(e.Inference.CacheReadTokens),
			CacheWriteTokens: int64(e.Inference.CacheWriteTokens),
			OutputTokens:     int64(e.Inference.OutputTokens),
			ReasoningTokens:  int64(e.Inference.ReasoningTokens),
			Tokens:           int64(e.Inference.TotalTokens),
			PresentKinds:     e.Inference.PresentKinds,
		},
	}
	if e.StatusCode >= 400 || e.Phase == pipeline.SessionDenied {
		r.Errors = 1
	}
	// Priceable: carried a model and a non-zero token count. The denominator for
	// coverage — Requests is the wrong one, since only inference can be priced.
	if r.Model != "" && r.Tokens > 0 {
		r.PriceableRequests = 1
	}
	// The figure inference-parser settled. Find, not Decode: an unpriced record
	// still exists and carries provenance, and later it carries savings.
	if ev, ok := costevent.Record(e); ok {
		r.Provenance = ev.Provenance
		if ev.Priced() {
			r.CostMicros = ev.Micros()
			r.PricedRequests = 1
		}
	}

	w.add(minute, r)
}

// add folds one row into the open minute, flushing first if the minute rolled.
func (w *Writer) add(minute time.Time, r Row) {
	w.mu.Lock()
	defer w.mu.Unlock()

	switch {
	case w.open.IsZero():
		w.open = minute
	case minute.After(w.open):
		w.flushLocked()
		w.open = minute
	case minute.Before(w.open):
		// A late event for an already-closed minute. Folding it into the open minute
		// would misdate it; appending a second row for a closed minute is correct,
		// because a reader sums every row for a timestamp rather than assuming one.
		w.appendRows([]Row{r})
		return
	}

	if cur, ok := w.rows[r.key()]; ok {
		cur.Add(r.Counts)
		return
	}
	row := r
	w.rows[r.key()] = &row
}

// Flush forces the open minute to disk. For shutdown and for tests.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *Writer) flushLocked() error {
	if len(w.rows) == 0 {
		return nil
	}
	out := make([]Row, 0, len(w.rows))
	for _, r := range w.rows {
		out = append(out, *r)
	}
	w.rows = map[key]*Row{}
	return w.appendRows(out)
}

// appendRows writes and swallows the error into a log. Callers on the hot path
// must not learn about disk problems.
func (w *Writer) appendRows(rows []Row) error {
	err := w.store.append(rows)
	if err != nil {
		slog.Warn("costledger: append failed; cost history for this minute is lost",
			"error", err, "rows", len(rows))
	}
	return err
}

// Close flushes and releases the store.
func (w *Writer) Close() error {
	if err := w.Flush(); err != nil {
		return err
	}
	return w.store.close()
}
```

`e.Endpoint()` may not exist — the field is `e.Host`. Use `e.Host`; the parameter name in the spec's JSON is `endpoint` because that is what it means to a reader, and the mapping from `Host` is deliberate. Fix the code to `Endpoint: e.Host` and say so in a comment.

- [ ] **Step 5: Create `store.go`**

Day files, append-only, retention by deletion. Keep it small: `newStore(dir)`, `append([]Row) error`, `close() error`, `prune(keepDays int) error`, `path(t time.Time) string` returning `dir/2026-09-13.jsonl`.

Two of those signatures shipped differently, and both differences are the retention window
moving from the caller to the store: it is `newStore(dir string, retainDays int)`, which
defaults a non-positive value, and `prune(now time.Time) error`, which reads `s.retainDays`
and takes the *clock* instead. Passing `now` is what makes retention testable without waiting
a day, and holding `retainDays` on the store is what stops two call sites disagreeing about
how long a day file lives. Open with `os.OpenFile(..., os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)` — `0o600` because the file records spend, and `0o644` would make it world-readable on a shared machine. One `json.Encoder` per append call; do not hold a long-lived handle across day boundaries.

Default retention 30 days. An active 8h day writes roughly 480 active minutes × a few label combinations ≈ 350 KB, so 30 days is ~10 MB.

Tolerate a corrupt line on read (Task 2) rather than failing the whole file: a truncated final line is the expected outcome of a crash mid-append.

- [ ] **Step 6: Run the tests**

Run: `go test ./authlib/costledger/ -v 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 7: Do not commit yet.** Task 4 commits.

---

## Task 2: Read a span back

**Files:** Create `authlib/costledger/query.go`, `query_test.go`

**Interfaces:**
- Produces: `func (w *Writer) Query(from, to time.Time) ([]Row, error)`, and `func Fold(rows []Row, group usage.Group) (usage.Counts, map[string]usage.Counts)`.
- Task 3 calls both; Task 4's CLI calls them too.

> **SUPERSEDED — both signatures shipped wider than this.** `Fold` returns a THIRD value,
> `int64`: the cost no series entry carries. Gateway-priced responses from endpoints
> `inference-parser` cannot read have no model, so `group=model` skips them while the total
> keeps them, and a client summing the breakdown got less than the total with nothing
> saying why. It is counted in the loop that decides what to skip rather than re-derived by
> the caller, because only that loop knows which rows were dropped. Surfaces as
> `Snapshot.UngroupedCostMicros`, absent when the breakdown reconciles.
>
> `Query` also stopped being the reader to call — see the banner. `Window` stitches the
> open minute; `Query` is the disk half alone.

**Why `Fold` lives here and not in `usage`:** it folds `Row`s, which `usage` does not know about. It returns `usage.Counts` and a `usage.Group`-keyed series so the caller's shape matches `/v1/usage` exactly and the HTTP layer does not branch on where the data came from.

- [ ] **Step 1: Write the failing test**

Create `authlib/costledger/query_test.go` in **two or three small tool calls** — never one large write.

```go
package costledger

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// writeDay writes lines straight to a day file, bypassing Writer, so a query test
// controls exactly what is on disk without driving the accumulation path.
func writeDay(t *testing.T, dir string, day time.Time, lines ...string) {
	t.Helper()
	name := filepath.Join(dir, day.Format("2006-01-02")+".jsonl")
	var buf []byte
	for _, l := range lines {
		buf = append(buf, l...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(name, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// line renders one ledger row as JSON, so a test can state exactly what is on disk.
func line(min time.Time, endpoint, model string, requests, in, out, micros int64) string {
	return fmt.Sprintf(
		`{"at":%q,"endpoint":%q,"model":%q,"requests":%d,"inputTokens":%d,`+
			`"outputTokens":%d,"costMicros":%d,"pricedRequests":%d,"priceableRequests":%d}`,
		min.Format(time.RFC3339Nano), endpoint, model, requests, in, out, micros, requests, requests)
}

func TestQuery_SpanInsideOneDay(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		line(base.Add(time.Minute), "gw", "m", 1, 20, 5, 200),
		line(base.Add(2*time.Minute), "gw", "m", 1, 30, 5, 300),
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Inclusive of both endpoints at minute granularity, so the third row is out.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
}

func TestQuery_SpanCrossingLocalMidnight(t *testing.T) {
	// The case a UTC-vs-local bug shows up in, and the reason "today" is local
	// midnight: a laptop crossing a timezone must not have its day reset
	// mid-afternoon.
	dir := t.TempDir()
	midnight := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)
	before := midnight.Add(-30 * time.Minute) // 23:30 on the 13th
	after := midnight.Add(30 * time.Minute)   // 00:30 on the 14th
	writeDay(t, dir, before, line(before, "gw", "m", 1, 10, 5, 100))
	writeDay(t, dir, after, line(after, "gw", "m", 1, 20, 5, 200))
	w := newTestWriter(t, dir, func() time.Time { return after })

	got, err := w.Query(before, after)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 across two day files: %+v", len(got), got)
	}
}

func TestQuery_MissingDayFileIsNotAnError(t *testing.T) {
	// An idle day writes no file. That is the normal case on a laptop, not a
	// fault — erroring would make "this week" fail for anyone who took a day off.
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base.AddDate(0, 0, -3), base)
	if err != nil {
		t.Fatalf("Query over an empty range: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows from an empty ledger, want 0", len(got))
	}
}

func TestQuery_TruncatedFinalLineIsSkippedAndTheRestSurvives(t *testing.T) {
	// A truncated tail is the expected outcome of a crash mid-append. Losing the
	// whole day because its last line is half-written would turn a 60-second gap
	// into a 24-hour one.
	dir := t.TempDir()
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	writeDay(t, dir, base,
		line(base, "gw", "m", 1, 10, 5, 100),
		line(base.Add(time.Minute), "gw", "m", 1, 20, 5, 200),
		`{"at":"2026-09-13T09:02:00`, // truncated mid-write
	)
	w := newTestWriter(t, dir, func() time.Time { return base })

	got, err := w.Query(base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want the 2 intact ones: %+v", len(got), got)
	}
}

func TestFold_ByModelSumsToTotals(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Endpoint: "gw", Model: "opus", Counts: usage.Counts{
			Requests: 2, InputTokens: 100, CostMicros: 300, PricedRequests: 2, PriceableRequests: 2}},
		{At: base, Endpoint: "gw", Model: "haiku", Counts: usage.Counts{
			Requests: 1, InputTokens: 20, CostMicros: 50, PricedRequests: 1, PriceableRequests: 1}},
	}

	totals, series := Fold(rows, usage.GroupModel)

	if totals.CostMicros != 350 {
		t.Errorf("totals.CostMicros = %d, want 350", totals.CostMicros)
	}
	if series["opus"].CostMicros != 300 || series["haiku"].CostMicros != 50 {
		t.Errorf("series = %+v, want opus 300 and haiku 50", series)
	}
	// The property that makes a breakdown table trustworthy: its rows account for
	// the total it sits under.
	var sum int64
	for _, c := range series {
		sum += c.CostMicros
	}
	if sum != totals.CostMicros {
		t.Errorf("series sums to %d but totals is %d", sum, totals.CostMicros)
	}
}

func TestFold_ByEndpoint(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{
		{At: base, Endpoint: "gw-a", Model: "m", Counts: usage.Counts{Requests: 1, CostMicros: 10}},
		{At: base, Endpoint: "gw-b", Model: "m", Counts: usage.Counts{Requests: 1, CostMicros: 20}},
	}

	_, series := Fold(rows, usage.GroupEndpoint)

	if series["gw-a"].CostMicros != 10 || series["gw-b"].CostMicros != 20 {
		t.Errorf("series = %+v, want gw-a 10 and gw-b 20", series)
	}
}

func TestFold_MethodIsAnAliasForModel(t *testing.T) {
	// Same equivalence the live path guarantees: two spellings, one series.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "opus", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}

	_, byModel := Fold(rows, usage.GroupModel)
	_, byMethod := Fold(rows, usage.GroupMethod)

	if len(byModel) != len(byMethod) || byModel["opus"] != byMethod["opus"] {
		t.Errorf("model series %+v and method series %+v differ", byModel, byMethod)
	}
}

func TestFold_UnpricedRowsCountButCostNothing(t *testing.T) {
	// The coverage gap must survive the round trip to disk, or a partial total
	// reads as a complete one.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "m", Counts: usage.Counts{Requests: 1, PriceableRequests: 1}}}

	totals, _ := Fold(rows, usage.GroupModel)

	if totals.CostMicros != 0 {
		t.Errorf("CostMicros = %d, want 0", totals.CostMicros)
	}
	if totals.PricedRequests != 0 || totals.PriceableRequests != 1 {
		t.Errorf("coverage = %d/%d, want 0/1", totals.PricedRequests, totals.PriceableRequests)
	}
}

func TestFold_EmptyLabelIsNeverASeriesKey(t *testing.T) {
	// A blank row in a breakdown table reads as a bug rather than as missing
	// attribution — the same guard the live foldInto applies.
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	rows := []Row{{At: base, Model: "", Counts: usage.Counts{Requests: 1, CostMicros: 10}}}

	totals, series := Fold(rows, usage.GroupModel)

	if _, ok := series[""]; ok {
		t.Error(`series has an "" key`)
	}
	if totals.CostMicros != 10 {
		t.Errorf("CostMicros = %d, want the row still counted in totals", totals.CostMicros)
	}
}

func TestFold_GroupNoneReturnsNoSeries(t *testing.T) {
	base := time.Date(2026, 9, 13, 9, 0, 0, 0, time.Local)
	totals, series := Fold([]Row{{At: base, Counts: usage.Counts{Requests: 1, CostMicros: 10}}}, usage.GroupNone)
	if totals.CostMicros != 10 {
		t.Errorf("CostMicros = %d, want 10", totals.CostMicros)
	}
	if series != nil {
		t.Errorf("series = %+v, want nil for GroupNone", series)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./authlib/costledger/ -run 'TestQuery_|TestFold_' -v 2>&1 | tail -20`
Expected: FAIL — `w.Query undefined`, `undefined: Fold`.

- [ ] **Step 3: Implement `query.go`**

```go
package costledger

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Query returns every row whose minute falls in [from, to], inclusive at minute
// granularity.
//
// Reads only what the Writer has flushed — closed minutes. The caller stitches the
// open minute from the in-memory ring; see the package doc for why the two must
// never both own a minute.
func (w *Writer) Query(from, to time.Time) ([]Row, error) {
	if to.Before(from) {
		from, to = to, from
	}
	fromMin, toMin := from.Truncate(time.Minute), to.Truncate(time.Minute)

	var out []Row
	// Walk dates rather than globbing the directory: the read stays bounded by the
	// span the caller asked for instead of by how long the ledger has been running.
	for d := dayOf(fromMin); !d.After(dayOf(toMin)); d = d.AddDate(0, 0, 1) {
		rows, err := w.store.readDay(d)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if m := r.At.Truncate(time.Minute); m.Before(fromMin) || m.After(toMin) {
				continue
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// dayOf truncates to LOCAL midnight. Local, not UTC: "today" means the operator's
// day, and a laptop that crosses a timezone must not have its day reset
// mid-afternoon.
func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// Fold sums rows into one total plus an optional per-label series, in the shape
// /v1/usage already serves.
//
// Returns usage.Counts and a Group-keyed map so an HTTP handler need not branch on
// whether the data came from the ring or from disk. Summation is Counts.Add, so a
// field added there is carried here with no edit — the same property that keeps
// fold() and abctl's (other)-band collapse correct.
func Fold(rows []Row, group usage.Group) (usage.Counts, map[string]usage.Counts) {
	var totals usage.Counts
	var series map[string]usage.Counts
	for _, r := range rows {
		totals.Add(r.Counts)
		key, ok := labelFor(r, group)
		if !ok {
			continue
		}
		if series == nil {
			series = map[string]usage.Counts{}
		}
		cur := series[key]
		cur.Add(r.Counts)
		series[key] = cur
	}
	return totals, series
}

// labelFor picks the grouping key, returning ok=false when the row carries no
// value for that axis — so an empty key never becomes a blank row, the same guard
// the live foldInto applies. The row still counts toward totals either way.
func labelFor(r Row, group usage.Group) (string, bool) {
	var v string
	switch group {
	// Both spellings read Model: group=method shipped as the model series under a
	// wrong name, and the alias must behave identically here too.
	case usage.GroupModel, usage.GroupMethod:
		v = r.Model
	case usage.GroupEndpoint:
		v = r.Endpoint
	default:
		return "", false
	}
	return v, v != ""
}
```

`usage.GroupAgent` does not exist until the client-identity commit — do **not** add a case for it or invent the constant. That commit adds the arm along with the column it reads.

Add to `store.go`:

```go
// readDay decodes one day file. A missing file is not an error: an idle day writes
// none, which is the normal case on a laptop.
//
// A decode failure stops reading THAT file and returns what was read so far rather
// than failing the query. A truncated final line is the expected outcome of a crash
// mid-append, and discarding a whole day because its last line is half-written
// would turn a 60-second gap into a 24-hour one.
//
// SUPERSEDED by 85aa46d9, which is the same argument carried one step further. Stopping
// at the bad line only works if the bad line is the LAST one — which it is for a crash
// mid-append and is not for any other kind of corruption. A bad line in the morning
// discarded the rest of the day, reintroducing exactly the 24-hour gap this comment
// says it exists to avoid. The shipped readDay SKIPS the undecodable line and keeps
// going, counts how many it skipped, and logs the count; only a scanner error — IO, or
// a line past maxLineBytes — ends the read, and then it warns rather than debug-logs,
// because the figure that follows is short by however much came after that point.
// Neither path ever fails the query, and no message carries bytes from the bad line: a
// corrupt ledger line could contain anything and this text reaches a log an operator
// pastes.
func (s *store) readDay(day time.Time) ([]Row, error) {
	f, err := os.Open(s.path(day))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Row
	dec := json.NewDecoder(f)
	for {
		var r Row
		if err := dec.Decode(&r); err != nil {
			if err != io.EOF {
				slog.Debug("costledger: stopping at an undecodable line",
					"day", day.Format("2006-01-02"), "rowsRead", len(out), "error", err)
			}
			return out, nil
		}
		out = append(out, r)
	}
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./authlib/costledger/ -v 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 5: Do not commit yet.**

---

## Task 3: Wire it on locally, off in-cluster, and serve `window=today|7d`

**Files:** `authlib/usage/snapshot.go` (`ParseWindow`), `authlib/sessionapi/usage.go`, `cmd/authbridge-proxy/main.go`, `cmd/authbridge-proxy/local.go`

**Interfaces:** `usage.ParseWindow` gains the two symbolic windows; the sessionapi `Server` gains a ledger reference and routes long windows to it.

### The design decision this task turns on: how a symbolic window is represented

"Today" is not a length, it is a boundary, so it cannot be a `time.Duration`. Keep `ParseWindow`'s existing signature working for its existing callers and add a companion:

```go
// authlib/usage/snapshot.go

// Spec is a parsed window request. Either Dur is set (a fixed length the ring can
// serve) or From/To are (a boundary only the ledger can serve).
//
// Two representations in one type rather than two functions, because the handler's
// job is exactly to choose between them and a caller that forgets to ask which
// kind it has would silently serve the wrong span.
type Spec struct {
	// Label is what the response echoes back, so a client always learns which
	// window it actually got — which matters most when it is not the one asked for.
	Label string
	// Dur is non-zero for a fixed-length window.
	Dur time.Duration
	// From and To bound a symbolic window. Zero when Dur is set.
	From, To time.Time
}

// Symbolic reports whether this window needs the ledger.
func (s Spec) Symbolic() bool { return s.Dur == 0 }

// ParseWindowSpec parses any window the API accepts, including the symbolic ones.
//
// now is passed rather than read from the clock so a test can pin a day boundary,
// and so "today" is computed once per request instead of drifting between the
// bound calculation and the response label.
func ParseWindowSpec(s string, now time.Time) (Spec, error)
```

`ParseWindow(s string) (time.Duration, error)` stays as it is and returns an error for `today`/`7d` — a duration caller genuinely cannot express them, and silently substituting a length would be worse than refusing.

`today` is local midnight to `now`. `7d` is 7×24h back from `now`, **not** seven calendar days, and the label says `7d` so that reading is available to a client. State that in the godoc; a "last 7 days" that silently meant "since last Monday" would be a different number.

- [ ] **Step 1: Write the failing tests**

Two files, each in small tool calls.

`authlib/usage/snapshot_test.go` (append):

```go
func TestParseWindowSpec_FixedLengths(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	for _, in := range []string{"10m", "1h", "6h"} {
		t.Run(in, func(t *testing.T) {
			got, err := ParseWindowSpec(in, now)
			if err != nil {
				t.Fatalf("ParseWindowSpec(%q): %v", in, err)
			}
			if got.Symbolic() {
				t.Errorf("%q reported Symbolic; want a fixed length", in)
			}
			if got.Label != in {
				t.Errorf("Label = %q, want %q", got.Label, in)
			}
		})
	}
}

func TestParseWindowSpec_TodayIsLocalMidnightToNow(t *testing.T) {
	// Local, not UTC. A laptop crossing a timezone must not have its day reset
	// mid-afternoon, and a UTC day would do exactly that.
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if !got.Symbolic() {
		t.Fatal("today reported a fixed length; want symbolic")
	}
	wantFrom := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)
	if !got.From.Equal(wantFrom) {
		t.Errorf("From = %v, want local midnight %v", got.From, wantFrom)
	}
	if !got.To.Equal(now) {
		t.Errorf("To = %v, want now %v", got.To, now)
	}
	if got.Label != "today" {
		t.Errorf("Label = %q, want \"today\"", got.Label)
	}
}

func TestParseWindowSpec_TodayJustAfterMidnightIsAShortWindow(t *testing.T) {
	// The boundary case: at 00:05, "today" is five minutes, not 24 hours. A
	// fixed-length reading would report yesterday evening's spend as today's.
	now := time.Date(2026, 9, 14, 0, 5, 0, 0, time.Local)
	got, err := ParseWindowSpec("today", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 5*time.Minute {
		t.Errorf("span = %v, want 5m", d)
	}
}

func TestParseWindowSpec_SevenDaysIsRollingNotCalendar(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	got, err := ParseWindowSpec("7d", now)
	if err != nil {
		t.Fatalf("ParseWindowSpec: %v", err)
	}
	if d := got.To.Sub(got.From); d != 7*24*time.Hour {
		t.Errorf("span = %v, want exactly 7x24h (rolling, not calendar)", d)
	}
}

func TestParseWindowSpec_RejectsUnknownWithoutEchoingInput(t *testing.T) {
	const attack = "<script>alert(1)</script>"
	_, err := ParseWindowSpec(attack, time.Now())
	if err == nil {
		t.Fatal("accepted an unknown window")
	}
	if strings.Contains(err.Error(), "script") {
		t.Errorf("error echoes caller input: %q", err.Error())
	}
}

func TestParseWindow_StillRejectsSymbolicWindows(t *testing.T) {
	// A duration caller cannot express "today". Refusing beats silently
	// substituting a length, which would report a number for a span nobody asked
	// for.
	for _, in := range []string{"today", "7d"} {
		if _, err := ParseWindow(in); err == nil {
			t.Errorf("ParseWindow(%q) succeeded; want an error", in)
		}
	}
}
```

`authlib/sessionapi/usage_test.go` (append; reuse the file's real `newTestServer(t, WithUsage(agg))` + `fetchUsage` idiom, and add a `WithCostLedger` option if the server needs one):

```go
func TestHandleUsage_TodayIsServedFromTheLedger(t *testing.T) {
	dir := t.TempDir()
	// A closed minute on disk, plus nothing in the ring, so a non-zero total can
	// only have come from the ledger.
	// ... construct a costledger.Writer over dir, Record one costed event, Flush ...

	srv := newTestServer(t, WithUsage(agg), WithCostLedger(led))
	snap := fetchUsage(t, srv, "?window=today")

	if snap.Window != "today" {
		t.Errorf("window = %q, want \"today\" echoed back", snap.Window)
	}
	if snap.Totals.CostMicros == 0 {
		t.Error("CostMicros = 0; the ledger row did not reach the response")
	}
	if !snap.Priced {
		t.Error("priced = false for a ledger-sourced total")
	}
}

func TestHandleUsage_TodayWithoutALedgerDegradesAndSaysSo(t *testing.T) {
	// Kubernetes has no ledger by design. A 400 would make the abctl cost view
	// fail there rather than showing what IS available, so the handler serves the
	// ring's maximum window and reports the window it actually served.
	srv := newTestServer(t, WithUsage(agg)) // no ledger
	snap := fetchUsage(t, srv, "?window=today")

	if snap.Window == "today" {
		t.Error("window = \"today\" but no ledger exists; the response must name the window actually served")
	}
	if snap.Window != usage.MaxWindow.String() {
		t.Errorf("window = %q, want the ring maximum %q", snap.Window, usage.MaxWindow)
	}
}

func TestHandleUsage_SevenDaysIsServedFromTheLedger(t *testing.T) {
	// Same shape as today, over a span the ring cannot cover at all.
}

func TestHandleUsage_UnknownWindowStillDoesNotEchoInput(t *testing.T) {
	srv := newTestServer(t, WithUsage(agg))
	rec := httptest.NewRecorder()
	srv.handleUsage(rec, httptest.NewRequest(http.MethodGet, "/v1/usage?window=%3Cscript%3E", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "script") {
		t.Errorf("response reflects caller input: %s", rec.Body.String())
	}
}

func TestHandleUsage_LedgerBackedWindowGroupsByModel(t *testing.T) {
	// The Cost pane's by-model table must work over "today", not only over the
	// ring's windows — otherwise the breakdown silently covers a different span
	// from the total above it.
}
```

Fill the three sketched bodies out following the two complete ones; they are the same shape with a different window and a `group=model` query.

### Verified obstacle: the abctl client cannot express a symbolic window

`apiclient.Client.GetUsage(ctx, window, resolution time.Duration, sessionID string, group usage.Group)` takes **durations** and does `q.Set("window", window.String())` (`cmd/abctl/apiclient/client.go:200-202`). There is no way to send `window=today` through it, so Task 4's `abctl cost` and the strip's "today" figure are both blocked until the client can.

Add a sibling rather than changing the existing signature:

```go
// GetUsageWindow fetches a snapshot for a symbolic window the server names —
// "today", "7d" — which a time.Duration cannot express.
//
// A sibling of GetUsage rather than a widened signature: GetUsage has several
// callers and its duration parameters are the right shape for the chart windows,
// which really are fixed lengths. "Today" is not a length, it is a boundary.
func (c *Client) GetUsageWindow(ctx context.Context, window string, resolution time.Duration, sessionID string, group usage.Group) (*usage.Snapshot, error)
```

Then express `GetUsage` in terms of it (`GetUsageWindow(ctx, window.String(), …)`) so there is one request-building path and the two cannot drift on query encoding or on the `ErrNotFound` behaviour its godoc promises.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./authlib/usage/ -run TestParseWindow -v; go test ./authlib/sessionapi/ -run TestHandleUsage_ -v 2>&1 | tail -20`
Expected: FAIL — `undefined: ParseWindowSpec`, `undefined: WithCostLedger`.

- [ ] **Step 3: Implement**

**`authlib/usage/snapshot.go`** — `Spec`, `Symbolic()`, `ParseWindowSpec` as specified above. `ParseWindowSpec` delegates to `ParseWindow` for the fixed-length cases so there is one place the duration set is defined, and adds only the two symbolic arms. Keep the error message a fixed string naming the valid set.

**`authlib/sessionapi/usage.go`** — route on the spec:

```go
	spec, err := usage.ParseWindowSpec(r.URL.Query().Get("window"), time.Now())
	if err != nil {
		writeUsageError(w, err)
		return
	}
	// ... resolution, group, session parsing unchanged ...

	var snap usage.Snapshot
	switch {
	case !spec.Symbolic():
		snap = s.usage.Snapshot(spec.Dur, resolution, sessionID, group)
	case s.ledger == nil:
		// No ledger: Kubernetes by design, where files in a pod are the wrong sink
		// and the central collector is the right one. Serve what the ring HAS
		// rather than 400 — an abctl cost view must degrade to a shorter window,
		// not fail — and report the window actually served so the client never
		// mislabels a 6-hour figure as a day's.
		snap = s.usage.Snapshot(usage.MaxWindow, resolution, sessionID, group)
	default:
		snap, err = s.ledgerSnapshot(spec, sessionID, group)
		if err != nil {
			// A read failure is not a client error and must not look like one.
			slog.Warn("sessionapi: cost ledger read failed", "window", spec.Label, "error", err)
			http.Error(w, `{"error":"cost history unavailable"}`, http.StatusServiceUnavailable)
			return
		}
	}
```

and the ledger-backed assembly:

```go
// ledgerSnapshot builds a Snapshot from persisted rows.
//
// ONE bucket spanning the whole window, not a series at the requested resolution.
// The ledger exists to answer "what did today cost", and a client wanting a shaped
// chart asks for a ring window instead — synthesising per-minute buckets from disk
// for a 7-day span would read millions of rows to draw a chart nothing requests.
// BucketSeconds reports the real span so a client cannot mistake it for a fine
// series.
func (s *Server) ledgerSnapshot(spec usage.Spec, sessionID string, group usage.Group) (usage.Snapshot, error) {
	rows, err := s.ledger.Query(spec.From, spec.To)
	if err != nil {
		return usage.Snapshot{}, err
	}
	totals, series := costledger.Fold(rows, group)
	return usage.Snapshot{
		Window:        spec.Label,
		BucketSeconds: int(spec.To.Sub(spec.From).Seconds()),
		Session:       sessionID,
		Group:         group,
		Buckets:       []usage.Bucket{{At: spec.From, Counts: totals, Series: series}},
		Totals:        totals,
		Priced:        totals.PricedRequests > 0,
	}, nil
}
```

Three things about that body are not what shipped, and the first is the whole point of
`ae2c7f41`:

- **`s.ledger.Window(...)`, never `Query(...)`.** `Query` answers only for what has been
  flushed, so it systematically omits the minute currently accumulating — and omits it
  *indefinitely* once traffic stops, because the flush is driven by the next event. The case
  this endpoint exists for is the worst of it: a session whose whole conversation fit inside
  one minute has nothing on disk at all, so the response said `priced:false` over real money.
  `Window` composes disk and open minute and guarantees no minute is in both; its doc gives
  the three-step non-overlap argument.
- **No `sessionID` parameter.** `handleUsage` refuses `session=` alongside a symbolic window
  before reaching here, because the ledger's rows hold no session ids — a snapshot echoing a
  session it did not filter by would be a wrong label on a right number.
- **`UnpricedBy` and `PricedBy` are absent, deliberately**, and the shipped doc says why:
  provenance is in the row key so `PricedBy` is reconstructible later, but `UnpricedBy` needs
  the endpoint-and-model pair of requests that could NOT be priced, which a row carrying only
  its own labels cannot tell from a priced one of the same pair. Emitting one map without the
  other would read as "no pricing gaps here". The gap stays visible as
  `Totals.PricedRequests` against `Totals.PriceableRequests`.

~~`sessionID` is accepted and echoed but **not filtered on**: the ledger's rows carry no session id, because a session is a laptop-lifetime concept while the ledger is a day-lifetime one. Say so in the godoc rather than silently returning all-sessions data under a session label — and if that seems wrong, the honest alternative is to reject `session` together with a symbolic window, which is also acceptable. Pick one and state it.~~

**The alternative offered in that last sentence is what shipped, so read the bullets above and
disregard this paragraph.** `handleUsage` **rejects** `session=` alongside a symbolic window,
unconditionally — even where the ring could have answered for one session over six hours —
because an API whose behaviour depends on whether a deployment happens to have a ledger is one
a client cannot code against, and the refusal names the fix. Struck through rather than deleted
because "pick one and state it" is a real thing to hand an implementer; the defect was not the
choice, it was recording the choice in one place and leaving the alternative asserted in
another, twelve lines apart and in the wrong order.

`UnpricedBy`/`PricedBy` are not reconstructed here — for the reason in the third bullet above,
which is firmer than "leave it for whoever needs it": `PricedBy` *is* reconstructible from the
row key, `UnpricedBy` is not, and emitting one without the other reads as "no pricing gaps
here". `Snapshot.Degraded` **is** populated, which this task did not anticipate — `1c714e68`
added it so a day whose read lost lines stops producing a response byte-identical to a clean
one. Sample its two gauges **after** `Window`, never before: they report the most recent read,
so reading them first hands back the previous caller's answer as this one's.

**`cmd/authbridge-proxy/main.go`** — construct the ledger only on the `--local` path; nil otherwise. One startup log line either way, naming which and why, in the register of the existing `slog.Info("session tracking enabled", …)` line.

**`authlib/config/config.go`** — a `cost_ledger:` block following the existing pointer-with-`omitempty` convention that `tls_bridge` and `pricing` use:

```go
	CostLedger *CostLedgerConfig `yaml:"cost_ledger,omitempty" json:"cost_ledger,omitempty"`
```

with `Enabled *bool` (a pointer, so "unset" and "explicitly false" differ — the local default is true and an operator must be able to turn it off) and `RetentionDays int` defaulting to 30.

**`cmd/abctl/apiclient/client.go`** — add `GetUsageWindow` as specified in the obstacle section above, and re-express `GetUsage` in terms of it.

**`authlib/sessionapi/usage.go`'s disclosure block** — extend it: the ledger-backed windows expose a spend history far longer than the ring's six hours on an unauthenticated port, which is a materially larger disclosure of business-sensitive data than anything the endpoint served before. That block's stated purpose is making exposure a decision rather than an oversight, and this is the largest single increase in it on this branch.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./authlib/usage/ ./authlib/sessionapi/ ./cmd/abctl/... 2>&1 | tail -10`
Expected: PASS but for the known `cmd/abctl` `bundle.crt` failure.

- [ ] **Step 5: Do not commit yet.**

---

## Task 4: `abctl cost`, the "today" headline, and commit

**Files:** `cmd/abctl/cmd_cost.go` + test, `cmd/abctl/main.go` (dispatch + usage text), `cmd/abctl/tui/spend.go`

**Interfaces:** `runCost(args []string, stdout, stderr io.Writer) int`, following `runTools`/`runClaudeCode`'s exact shape (`cmd/abctl/main.go:81-94`).

- [ ] **Step 1: Write the failing tests**

Create `cmd/abctl/cmd_cost_test.go`. Read `cmd_tools_test.go` first for the file-level idiom — these subcommands are tested by calling `run*` with buffers and asserting on the exit code and output, not by spawning a process.

```go
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRunCost_PrintsAHumanSummary(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,` +
		`"tokens":218100000,"costMicros":4170000,"pricedRequests":306,` +
		`"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"$4.17", "today", "318"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunCost_DisclosesTheCoverageGap(t *testing.T) {
	// 306 of 318 priced. Presenting the dollar total without the gap presents a
	// subtotal as the whole spend — the failure the coverage counters exist for.
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,` +
		`"costMicros":4170000,"pricedRequests":306,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if !strings.Contains(out.String(), "12") {
		t.Errorf("output does not disclose the 12 unpriced requests:\n%s", out.String())
	}
}

func TestRunCost_FullyPricedCarriesNoWarning(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":318,` +
		`"costMicros":4170000,"pricedRequests":318,"priceableRequests":318},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	if strings.Contains(strings.ToLower(out.String()), "unpriced") {
		t.Errorf("fully priced output still warns:\n%s", out.String())
	}
}

func TestRunCost_NothingPricedSaysUnavailableNotZero(t *testing.T) {
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":10,` +
		`"priceableRequests":10},"priced":false}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 — nothing priced is not an error", code)
	}
	got := out.String()
	if !strings.Contains(got, "unavailable") {
		t.Errorf("output does not say cost is unavailable:\n%s", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Errorf("output renders $0.00 for an unknown cost:\n%s", got)
	}
}

func TestRunCost_JSONUsesTheCountsFieldNames(t *testing.T) {
	// The schema rule: one vocabulary from parser to aggregate to ledger to CLI.
	// An unattended workload parses this, so the names are the contract.
	srv := fakeUsageServer(t, `{"window":"today","totals":{"requests":2,` +
		`"inputTokens":100,"cacheReadTokens":2000,"cacheWriteTokens":50,` +
		`"outputTokens":30,"costMicros":250000,"pricedRequests":2,` +
		`"priceableRequests":2},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	if code := runCost([]string{"--endpoint", srv.URL, "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	for _, field := range []string{"inputTokens", "cacheReadTokens", "cacheWriteTokens", "outputTokens", "costMicros"} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("--json output missing %q:\n%s", field, out.String())
		}
	}
}

func TestRunCost_UnreachableProxyExitsNonZeroAndSaysWhatToRun(t *testing.T) {
	var out, errOut strings.Builder
	code := runCost([]string{"--endpoint", "http://127.0.0.1:1"}, &out, &errOut)

	if code == 0 {
		t.Fatal("exit = 0 for an unreachable proxy")
	}
	// A user whose proxy is down needs the next command, not a bare dial error.
	if !strings.Contains(errOut.String(), "abctl service status") {
		t.Errorf("stderr does not name the recovery command:\n%s", errOut.String())
	}
}

func TestRunCost_NoLedgerServesAShorterWindowAndSaysSo(t *testing.T) {
	// Kubernetes, or a local install with the ledger disabled. The server answers
	// with the window it actually served; the CLI must print THAT, not "today".
	srv := fakeUsageServer(t, `{"window":"6h0m0s","totals":{"requests":5,` +
		`"costMicros":100000,"pricedRequests":5,"priceableRequests":5},"priced":true}`)
	defer srv.Close()

	var out, errOut strings.Builder
	runCost([]string{"--endpoint", srv.URL}, &out, &errOut)

	got := out.String()
	if strings.Contains(got, "today") {
		t.Errorf("output claims \"today\" when the server served 6h:\n%s", got)
	}
	if !strings.Contains(got, "6h") {
		t.Errorf("output does not name the window actually served:\n%s", got)
	}
}

func TestRunCost_HelpListsTheFlags(t *testing.T) {
	var out, errOut strings.Builder
	if code := runCost([]string{"--help"}, &out, &errOut); code != 0 {
		t.Errorf("exit = %d for --help, want 0", code)
	}
	combined := out.String() + errOut.String()
	for _, want := range []string{"--json", "--window", "--endpoint"} {
		if !strings.Contains(combined, want) {
			t.Errorf("help does not mention %q:\n%s", want, combined)
		}
	}
}
```

Write `fakeUsageServer(t, body string) *httptest.Server` in the same file: an `httptest.Server` returning `body` with `Content-Type: application/json` for `/v1/usage`, and 404 otherwise. Check whether `cmd_tools_test.go` or a sibling already has an equivalent helper and reuse it rather than adding a second.

Also add to `cmd/abctl/main_test.go` (or wherever root-usage text is asserted): `cost` appears in `writeRootUsage`'s output. If no such test exists, add one — `paneUsage` shipped once as a feature reachable by a key no help text mentioned, and the root usage block is how a subcommand is discovered at all.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/abctl/ -run 'TestRunCost|TestRootUsage' -v 2>&1 | tail -20`
Expected: FAIL — `undefined: runCost`.

- [ ] **Step 3: Implement `cmd/abctl/cmd_cost.go`**

Follow `runTools`/`runClaudeCode`'s shape exactly (`main.go:81-94` dispatches them): `func runCost(args []string, stdout, stderr io.Writer) int`, its own `flag.FlagSet` writing to `stderr`, returning an exit code rather than calling `os.Exit`. Flags: `--json`, `--window` (default `today`), `--endpoint`.

The human output is a few lines, not a table — this is the concise answer to "what did today cost":

```
COST — today
  $4.17          318 requests   218.1M tokens
  input 100k · cache-read 215.9M · cache-write 1.2M · output 44k
  ⚠ 12 of 318 priceable requests unpriced
```

Rules it must obey, all already established elsewhere in this branch: print the window the **server** reported, never the one requested; `cost unavailable` rather than `$0.00` when `priced` is false; no coverage line at all when the gap is zero; exit 0 when nothing is priced (that is an answer, not a failure) and non-zero only when the request itself failed.

`--json` emits the snapshot's `totals` verbatim plus the window, so the field names are `usage.Counts`' own. Do not re-key or re-case them: an unattended workload parses this, and the whole point of the shared schema is that the CLI, `/v1/usage` and the ledger say the same words.

Register in `main.go`'s dispatch switch alongside `tools`/`claude-code`/`exec`/`service`/`observe`, and add a line to `writeRootUsage`'s text.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./cmd/abctl/ -run 'TestRunCost|TestRootUsage' -v 2>&1 | tail -25`

- [ ] **Step 5: The strip's "today" headline**

In `cmd/abctl/tui/spend.go`, add a second fetch for `window=today` via the new `GetUsageWindow`, setting `TodayUSD`/`HasToday` on the summary. The renderer already handles both fields and its tests already cover the present and absent paths, so **this is data only — if you find yourself editing `spend_strip.go`, stop and ask.**

Its own generation guard, separate counters from the window poll: two chains against one endpoint, and a reply from one must never be applied as the other's. Poll it far less often than the window figure — `today` moves slowly and a 5-minute interval is ample; say so in a comment.

Set `HasToday` **only** when the response's `window` is actually `today`. If the server degraded to a ring window because no ledger exists, `HasToday` stays false and the strip shows the window figure alone — otherwise the headline would label a 6-hour total as a day's. Add a test for exactly that, since it is the one case where the honest answer is to show less.

- [ ] **Step 6:** Full suite, vet, lint, `gofmt -l` on touched dirs.

- [ ] **Step 7: Commit — commit 5 of the PR.** Message must state: what the ledger holds and does not hold; that only closed minutes are written and a restart loses ≤60s; that it is on for `--local` and off in Kubernetes and why; that "today" is local midnight; and that `agent` ships empty until the client-capture commit.

---

## Self-Review

**Spec coverage:** durable per-minute rows at `~/.cortex/cost/YYYY-MM-DD.jsonl` (Task 1) ✓; `usage.Counts` field names (Task 1, by embedding) ✓; provenance in the key (Task 1) ✓; ~~ring owns the open minute (Task 1) ✓~~ — **this tick was false when written**, see below; one stitching reader (Task 2) ✓; `window=today|7d` (Task 3) ✓; on locally / off in-cluster (Task 3) ✓; retention 30 days (Task 1/3) ✓; no prompt content (Task 1, asserted on bytes) ✓; `abctl cost --json` (Task 4) ✓; "today" headline (Task 4) ✓.

**The one false tick, kept visible.** Nothing owned the open minute when this commit landed:
the ring did not supply it, this package did not yet hold it out, and `ledgerSnapshot` read
`Query` — so `window=today` under-reported by up to a minute, and by everything since the last
event once traffic stopped. `ae2c7f41` closed it by making the writer's own accumulator the
source and `Window()` the stitch. Left as a struck-through tick rather than edited to pass,
because the interesting failure is not the missing code: it is that a self-review restated the
plan's own wrong constraint and ticked it. A checklist item copied from a constraint can only
ever verify that the constraint was followed, never that it was right. `costledger/row.go`'s
package doc is where the four reasons the ring cannot be the source now live.

**Deliberate gaps, not omissions:** `agent` is empty until the client-capture commit; the field ships now so no reader sees a schema change. `Avoided`/savings columns arrive with the savings commit — `usage.Counts` embedding means they appear automatically once added there, which is the point of embedding.

**Deviation from the spec, ruled during planning:** the ledger is an independent second `session.Recorder`, not a reader of the usage aggregator. Reasoning is in the "Design decision" section — the aggregator stores marginals, not the joint distribution the spec's row shape requires.

**Risk to watch:** `Row` embeds `usage.Counts`, so a field added to `Counts` silently changes the on-disk schema. That is desirable for shared vocabulary but means the ledger's JSON is only as stable as `Counts`. Additive-only changes keep old files readable; a rename would not. Worth a sentence in the package doc.

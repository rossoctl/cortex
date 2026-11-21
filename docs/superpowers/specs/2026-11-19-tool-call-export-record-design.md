# Tool calls in the export: a paired, content-free `toolCall` record — design

Status: **draft**.
Date: 2026-11-19.
Issue: rossoctl/cortex#1906 (feature tracker, §2 of three)
Background: #1810 (the export's own spec and tracker), #1830/#1812/#1831 (what shipped),
#1740 (three count fields), #1924 (Claude Code telemetry), #1997 (`sessionIncarnation`),
#1922 (prompt grouping)
Parent spec: [`2026-11-13-session-event-export-design.md`](2026-11-13-session-event-export-design.md).
This one narrows its §7.2 `toolCallNames` item and supersedes the shape proposed there.

Items marked **Open** must be decided before the work they belong to begins. Nothing here
is merged.

**40 numbered requirements T0–T37, plus the lettered T10a and T11a that narrow T10 and T11**, phrased as plain declaratives: a requirement says what
the system does once built. The numbering is stable so it can be cited in review and in the
PRs that implement it, and is therefore in the order the requirements were ADDED, not document
order: T0 sits in §2.0 because it bounds the whole spec, T23–T27 and T36–T37 in §4.3a,
T28–T33 and T35 in §4.4 and §4.4a, and T34 in §7.1b.

Revised twice on 2026-11-19, once on 2026-11-20 and four times on 2026-11-21.

**First revision**, after review raised two things the first draft buried. §2.0 is new: every
field here is **already collected and most of it is already on disk**, so this is a projection
change and not an instrumentation one — and arguments and result content are a separate
decision (Tier 3), not a larger version of this one. §4.3a is new: the first draft carried a
tool duration with **no inter-call gap at all**, which cannot answer occupancy. §2.1 and §2.2
corrected: today's record has no per-tool timing of any kind, and the timeline's pairing is
derived rather than stored.

**Second revision**, after @webchang's review on #2009, which read the draft against the code
and against a converter written for it. Four measurement claims were wrong and are corrected:

- **§7.1 contradicted §3.1.** A span's two boundaries are both model traffic, so a local tool
  call *does* get a span — the first draft's "local calls are invisible" was #1810 §9's gap
  classification misapplied. §7.1 now states the real limit (the proxy sees the gap around a
  tool, never inside it), and §7.1b separates sizing the model leg from sizing a sandbox host.
- **§3.2 and §3.3 double-counted shared windows** and priced `AskUserQuestion` as tool time.
  §3 now states three method rules and every figure is re-derived under them: the ratio moves
  4.1 → **4.2 : 1**, the long band 55.4% → **61.7%** of work.
- **§3.5 is new:** `durationMs` is a client round trip including permission waits, which the
  first draft stated only for the gap.
- **§4.3a's gap was undefinable** — T24's interval always contained `modelMs` while T25
  classified a different one. Now one named interval, with `modelMs` outside it by
  construction and the fourth `gapKind` dropped as an artifact.

**Third revision**, after @webchang's follow-ups on #2009. §7.1b is rewritten: the question is
**where a tool runs**, which the proxy cannot see at all — a built-in tool may run locally or in
a remote sandbox, and an MCP tool may be served over stdio or HTTP, so `toolSource` says what
kind of tool it is and never where it ran. Cortex therefore specifies no sandbox-side record and
guarantees stable join keys instead (T34). T32 changes from "out of scope" to **marked**: a
backgrounded call gets `endReason: "background"`, so the undercount is visible per call rather
than hidden inside `result`. T3 and §3.3 now say banding is an **analyzer's** policy — the
export carries raw timings and names no bands — which also removes the naming question from
this spec.

Record shape changed in the second revision: `startedAt` replaces `at` and is always present (T29), `seq` and
`parallelGroup` are gone, `callIndex` (T30) and a closed `endReason` (T31) are added. T28 gives
event records an explicit `kind`. T33 says a call below a `since` floor is dropped. §10 splits into what the review
settled and what is still open.

**Fourth revision**, after @huang195's and @clawgenti's reviews on #2009. The must-fix was a
privacy contradiction: T10 said a name is exported verbatim "whatever its source", which
licensed shipping `mcp__acme-billing-prod__refund` in clear text and disagreed with T11's keyed
hash two lines below. T10 is now scoped to built-ins. Also: `lost` is dropped from `endReason`
(no case produces it — a call with no reason is still OPEN, and T17 does not export one); T35
names the five fields written even at zero, since `omitempty` would drop a real `callIndex: 0`
and a clamped 0 ms duration; T28 is scoped to an opted-in stream, which it has to be or T20 is
false; `inputBytes` reaches the example and the field table; T36 and T37 say that `modelMs` and
`gapMs` repeat across a batch and that a `tool` gap is not idle time, so neither can be summed
naively; §2.2's claim that a background job extends a `Bash` span is removed, because the code
does not do that and T32 says so correctly; and the stale first-draft figures in §9 and the
guide are corrected to §3's.

**Fifth revision**, after @clawgenti's second pass on #2009 found two more must-fixes, both
places where the fourth revision's new text claimed more than the code supports:

- **"No case produces `lost`" was false.** `endStrandedTools` is a FIFTH closing case this spec
  never mentioned: it runs after the pairing and gives a tool an end at a failed model-host dial
  WITHOUT marking it lost. Such a call has an end and none of the other four reasons, which T35 says
  cannot happen and T17 would drop. `endReason` gains `stranded`, and the implementation note
  says plainly that the pairing package cannot see this case today — `endStrandedTools` needs
  the agents, turns and hosts the pairing has none of — so how it learns it is **Open**.
- **The T10 fix left `toolSource: "unknown"` uncovered** by either rule. T10a says it follows
  T11 and is hashed: the branch is on "provably a built-in", never on "provably MCP", because a
  hashed built-in costs a reader one unreadable label where a cleartext MCP name costs a
  customer's identity. The field table said "verbatim" without qualification and now does not.

Also: the guide's lost-span filter had the same blind spot as the first item and now names the
third kind; the #1924 row no longer cites a claim §7.1 stopped making; and T35 says six fields
rather than five.

**Sixth revision**, finishing what the fifth started: it added `stranded` and left it
half-wired, which is the whole of this round.

- **The guide had the duration backwards.** It said a stranded span's duration is the outage.
  It is the opposite: `endStrandedTools` exists to CUT the outage off, and the dial it cuts at
  measured within 0.1 s of the tool's real end on every session checked against Claude Code's
  transcripts — one 9-minute `Bash` had drawn as 8 hours. A reader following the old text would
  have discarded the one class of span already corrected. What the guide should warn about, and
  now does, is the **result**: a stranded span may have none, or one that arrived hours later.
- **T16 said "no result, no duration"**, which `stranded` contradicts. The rule is "no END, no
  duration": `durationMs` is present exactly when `endReason.HasEnd()`.
- **The late-result case is specified**: `stranded` wins over `result`, because it is the reason
  that explains the duration; `resultSeq` stays present; and `durationMs` ends at the dial, not
  at `resultSeq`. That is the one case where the two disagree.
- **"None of the four reasons ... has `End == nil`"** was the sentence the fifth revision's own
  commit message called false, still sitting 15 lines below the paragraph disproving it.

**Seventh revision.** The sixth said the guide now warned about a stranded span's missing
result and about only a failed dial being caught. It did not — the edit landed on the duration
and not on the caveats, and the commit message described the intent rather than the result. The
guide now carries both warnings. Also: the stranding cut is matched by HOST, not by agent, so
its documented shortcut can cut a subagent's running tool at another agent's blip — named now,
because `stranded` claims to explain a duration; the reasons-without-a-result count is
recounted with `stranded` among them; `stranded` wins over `background` as well as `result`;
and T11a settles the HMAC key's lifecycle, which T11 left open — generated once, never
rotated, because a key that changes re-labels every MCP tool and splits one tool's history into
two series a consumer cannot rejoin.

**Code references name symbols, not line numbers.** Every `agents.go:NNN` pin in the first
four revisions was correct when written and wrong a day later: #2041 moves all of them into
`core/session/toolpair`. A spec that cites `toolpair.Calls`, `closeCalls`, `preview` or
`endStrandedTools` still reads correctly after the move; one that cites a line is stale the day
it merges, and a reader who checks a stale pin and finds unrelated code stops trusting the rest.

Written after measuring, not before. §3 is the measurement; every design choice in §5 and §6
traces to a number in it. The measurement corpus is one developer's 30-day laptop archive:
40,496 export records, 12,104 tool calls, `claude-code/2.1.270` throughout with 46
`codex/0.163.0` records. **The mechanism generalises; the ratios do not** — see §9.

---

## 1. Goal

1. Answer "which tools does our community run, and what do they cost in time" from the
   export alone, across every session and beyond the archive's 30-day retention.
2. Answer "how does inference time compare with tool time" as a ratio a capacity plan can
   use — **as the agent saw it**. The record cannot say where a tool ran or what it cost
   there; see §7.1b before using these figures to size any host.
3. Keep the export content-free by construction, as #1810 §3.2 requires: no tool arguments,
   no tool results, no prompts.

### 1.1 Why the export, when `/timeline` already pairs tool calls

`GET /v1/sessions/{id}/timeline` answers per-tool latency today by pairing on tool-use id
(#1810 §9.1). It is the right tool for one session. It cannot do this job:

It is also **content-light, not content-free**, which matters when comparing it with this
record: `Tool.Preview` carries up to 80 runes of argument text — a command line, a file path,
a query (`preview`) — and `Tool.ID` is the tool-use id itself
(`timeline.Tool`). #1810 §9.1 states the same caveat in its own words ("no prompts",
not "safe to share"). A `toolCall` record carries neither (T6, T8).

| Need | Why `/timeline` cannot |
|---|---|
| Fleet totals across every session | one session per HTTP request, one compute each |
| Longitudinal: how p50 moved across a release | no durable artifact; the archive ages out at 30 days |
| Join tool timing to token counts | the timeline carries `outputTokens` and no cache counts |
| Deep sessions | capped at 50,000 events per session |

Measured cost of doing it the timeline way on the reference install: 82 timeline requests at
~1 s each for a session only on disk, against ~10 s for one `jq` pass over a 29,606-record
NDJSON file (#1810 §9.2).

---

## 2. What exists today

### 2.0 Nothing here needs new capture — and that is the point

**Everything this spec exports is already collected, and most of it is already on disk.** The
work is a projection, not an instrumentation change. Stating it plainly because the rest of
§2 reads like a capability gap, and it is not one:

| Already collected | Where | On disk? |
|---|---|---|
| Tool call name and id | `InferenceToolCall{ID, Name}` on a response | yes |
| Tool call **arguments** | `InferenceToolCall.Arguments` | yes — `archive/format.go` interns `HistoryToolCalls[i].Arguments` |
| Tool **result content** | `InferenceToolResult.Content` | yes — `archive/format.go` interns `ToolResults[i].Content` |
| Result error flag | `InferenceToolResult.IsError` | yes |
| Pairing key | `InferenceToolResult.ToolUseID`, message `ToolUseIDs` | yes, interned |
| Inference duration | `SessionEvent.Duration` | yes |

So a 30-day archive already holds every tool call's arguments and every tool result's text.
The export carries none of it, by construction (#1810 R7).

**Two things follow, and they are different decisions, not two sizes of one:**

1. **Names, per-call timing and result size are a projection change.** Content-free, covered
   by this spec, and shippable without touching the privacy posture.
2. **Arguments and result content are NOT.** They are content — a `Bash` command line is the
   canonical example (#1810 §4.1) — so exporting them is Tier 3, which needs redaction
   (#1921) and its own loopback-only route and command (#1925), not a flag on this one.
   `TestRecord_CarriesNoContent` exists to fail the build if they ever arrive here by
   accident.

T0. This spec exports no tool argument and no tool result content, in any tier or mode it
defines. A consumer that needs those uses the Tier 3 path, which is specified elsewhere and
carries a different privacy posture.

### 2.1 Shipped

- `core/sessionexport.Record` — ~36 flat scalars per event, an allowlist, with
  `TestRecord_CarriesNoContent` pinning the privacy contract (#1812).
- `Walk` over `archive.Earlier` — newest-first, one event at a time, O(1) state (#1830).
- `GET /v1/export` and `agentop export` (#1830, #1831), with `--gzip` (#1996).

A record carries `toolCount` (the manifest the request offered) and `toolCallCount` (the
calls the response emitted). **Both are counts and nothing else.** Today's export therefore
has:

- no tool name;
- **no per-tool timing of any kind.** `InferenceToolCall` is three strings — `{ID, Name,
  Arguments}` — and `InferenceToolResult` is two strings and a bool. Neither carries a timestamp or a
  duration, and no field anywhere on the event model does per-call timing. The only per-tool
  timing in the system is **derived** by the timeline (§2.2), never stored;
- **no gap between calls.** No `gapMs`, think-time or idle field exists on a record, on the
  event, or on a timeline span. A consumer can derive a gap between *events* from `at` and
  `durationMs`, which is what #1810 §9 did; it cannot derive one between *tool calls*,
  because it has no tool call to anchor to.

The second and third are what make today's record unable to answer occupancy: a count says
how many calls a turn made, never how long the sandbox was busy or idle.

### 2.2 The pairing is DERIVED by `core/session/timeline`, not stored

`timeline.Tool` is `{Agent, ID, Name, Start, End, Preview, Error, Lost, Seq}`, built in
`core/session/timeline/agents.go`. Reading that code is what sized this spec, so its shape is
stated here rather than summarised:

- It walks **request/response pairs per agent** (`type call struct {req, resp *model.SessionEvent}`),
  not single events.
- A span's **start is `responseEnd(req, resp)`** = `req.At + resp.Duration`, falling back to
  `resp.At`. So a tool's start is derived from its asking request plus the response's measured
  duration — not from any one event's timestamp.
- A span's **end is the first later request of the same agent carrying that `ToolUseID`**, a
  forward lookup across events, clamped up to the start when the result lands before the
  asking response finished.
- Four further closing cases exist and each one is load-bearing: `endedRun` (a
  `SubagentHandback`, `StructuredOutput` or `end_subtask` call never gets a result), the agent
  moving on without the result (`lastTurn`), a `<task-notification>` stopping the run
  (`stopped`), and background jobs (`backgroundRe`): the `Bash` span still **ends at its
  acknowledgement** — nothing extends it — and the job becomes a separate `timeline.Job`
  (the `Job` the pairing records, closed by its `<task-notification>` or by `TaskStop`). An earlier draft of this bullet
  said the span is extended, which contradicted T32 and would send an implementer of §6 looking
  for code that does not exist.

### 2.3 The gap, and why it is structural

`Walk` is newest-first and stateless; pairing needs forward lookup and per-agent state. So
this is **not** a new field on `FromEvent`. It needs either a buffered second pass per session
or a reorganised walk — a design decision with a memory cost, in a package whose premise is
that it is cheap. §6 is that decision.

---

## 3. The measurement

Source: `/v1/export` for the record view, `/v1/sessions/{id}/timeline` for the paired view,
over four sessions of one live install. 348 spans with an end and not `lost`, across **323
distinct windows** (§3.4).

**Three method rules, each of which moves a figure below.** The first draft of this spec
stated none of them, and §3.2 and §3.3 were wrong as a result:

1. **A shared window is counted once**, not once per call in its batch. Summing per span
   double-counts a batch's window. Measured: 828.2 s naive against **812.9 s** counted once —
   a 1.9% overcount, smaller than §3.4's 14% span share suggests, because batched calls are
   short.
2. **An interactive tool is excluded from a tool-time total.** `AskUserQuestion` is a person
   answering, not work: 12.7 s on one call here, and 62.6 s p50 over 68 calls on #1810's
   larger corpus. Leaving it in prices human think time as tool time.
3. **`durationMs` is a client round trip, including waits** (§3.5). No rule removes that; it
   is stated so a reader knows what the figures contain.

### 3.1 Per-tool timing

Every span, including batch members, since a per-tool p50 wants each call:

| Tool | n | p50 | p95 | total |
|---|---|---|---|---|
| `Bash` | 272 | 420 ms | 7,860 ms | 811.1 s |
| `AskUserQuestion` | 1 | 12,714 ms | — | 12.7 s |
| `Edit` | 39 | 35 ms | 76 ms | 1.4 s |
| `Agent` | 1 | 1,093 ms | — | 1.1 s |
| `Read` | 18 | 41 ms | 194 ms | 1.0 s |
| `Write` | 17 | 17 ms | 239 ms | 0.9 s |

**`Bash` is 78% of calls and over 97% of tool time.** One tool dominates, which is why the
name alone nearly answers the workload question — and why the *duration distribution*, not
the name, is the signal worth carrying.

### 3.2 Inference against tools

Windows counted once, `AskUserQuestion` excluded:

| | calls | total |
|---|---|---|
| Model | 400 | 3,371.6 s |
| Tool | 322 | 800.2 s |

**4.2 : 1** — inference is 80.8% of measured work. For sandbox capacity this is the headline:
the agent waits on the model four times longer than it runs tools. The ratio is robust to the
method rules — it was 4.1 : 1 before them — because both corrections are small against a gap
this wide.

### 3.3 Duration bands (<1 s / 1–10 s / >10 s)

Windows counted once, `AskUserQuestion` excluded. Band edges: short `< 1000 ms`, medium
`1000–10000 ms` inclusive, long `> 10000 ms`.

| Band | share of calls | share of work |
|---|---|---|
| short | 78.9% | 7.4% |
| medium | 18.9% | 31.0% |
| **long** | **2.2%** | **61.7%** |

The heaviest 10% of calls carry **82.4%** of tool time; the heaviest single call was a 145.2 s
`Bash`. It is the reason a mean is useless here: a design that reports averages hides the tail
that sizes the sandbox.

**These cuts and names are this measurement's choice, not the record's.** T3: the export
carries raw `durationMs`, so banding is an analyzer's policy. Nothing downstream should treat
short/medium/long as a Cortex vocabulary.

They are also not the
light/medium/heavy *weight tiers* of the TraceLab session-density analysis
(`moca-experiments/experiments/e15-session-density`, cuts at 1 s and 10 s on a call's
`work_ms`; `analysis/tracelab/analysis/tiers.py`), which elsewhere also names terciles of a
task's total time. Same cuts, different unit of analysis, so the tables are not comparable
and the names are kept apart deliberately. That analysis found 7.4% of calls carrying 86.5%
of work; the concentration here is the same shape, independently measured, and nothing more
than that should be read into the agreement.

### 3.4 A batch of calls shares one window — the finding that shapes the record

**25 batches, covering 50 of 348 spans (14%), carry an identical start and end.** The timeline
gives such a batch one merged interval, not a duration each.

A batch is **structural, not inferred from the timestamps**: every call of one response starts
at that response's end (`buildAgents` sets a span's `Start` to the asking response's end for each), so they share a start by
construction. They share an *end* when their results arrive in one later request. So the
grouping key is the asking response's seq, and `durationShared` says the results also returned
together — which is why §5 carries both rather than a timestamp match.

**A shared window is request-boundary granularity, not proof the calls ran in parallel.** Two
calls whose results land in the same request are indistinguishable from each other in time,
whether they ran together, back to back, or overlapped partly. The record therefore says "this
duration is a window shared with n calls", never "these ran concurrently".

Effect on a total: 828.2 s naive against 812.9 s with each window counted once, a **1.9%**
overcount. Small, because batched calls are short — but it is a bias, not noise, and it always
inflates. §3's method rule 1 applies it.

### 3.5 What `durationMs` measures, and what it contains

A span runs from the asking response's end to the arrival of the result, so **it is a client
round trip, not an execution time.** Everything the client did in between is inside it:

- the tool actually running;
- a **permission prompt and the person's answer** — the single largest contaminant, and the
  reason `AskUserQuestion` reads 12,714 ms here (T26, §7.1a);
- any client-side queueing, retry or transport leg before and after the tool ran.

So §3.1's figures are round trips, §3.2's ratio counts approval waits as tool time for any
tool that prompts, and no rule in §3 removes that. Method rule 2 excludes the one tool whose
*whole* duration is a person; it cannot exclude a permission prompt on a `Bash`. T26 states
the limit on the record and #1924 is the fix.

### 3.6 The two views reconcile

Summed `toolCallCount` over the same sessions at the time of the first measurement was **273**
against **267** paired spans — 2.2% apart, consistent with calls in flight at the archive
boundary. A record derived archive-side will therefore not contradict `/timeline`.

A note on the two span counts in this section: the install is live, so a later re-measurement
read 348 spans where the first read 267. Every figure in §3.1–§3.4 comes from the **later**
snapshot and is internally consistent; §3.6's reconciliation is from the earlier one. The
ratio and the concentration are stable across both, which is the claim being made — not the
absolute counts.

---

## 4. Requirements

### 4.1 Needs

T1. The export answers per-tool call counts and duration distributions across every archived
session, without a second endpoint and without a hand-written join.
T2. The export answers the inference-time-to-tool-time ratio per session and in aggregate.
T3. The export carries each call's **raw** timing — `durationMs` and `startedAt` — so any
analyzer can apply its own duration bands or weight tiers. Band names and cut points are an
analyzer's policy, not a property of the record, and the record defines none.
T4. A consumer can separate built-in tools from MCP tools without parsing a name.
T5. A tool call's record names the session, the agent role and the client, so a fleet view can
split by any of them.

### 4.2 Privacy and safety

T6. A `toolCall` record carries **no tool arguments**. A `Bash` command line is a secret and
stays in #1810's Tier 3, permanently.
T7. A `toolCall` record carries **no tool result content**. Result *size* may be carried
(T14); its text may not.
T8. A `toolCall` record carries **no tool-use id**. The id is the pairing key, used inside the
proxy and discarded: the export ships the measurement the key yields, never the key. This is
what keeps a `toolCall` record unjoinable back to a conversation.
T9. `TestRecord_CarriesNoContent` is extended to cover the `toolCall` record, planting a
secret in every content field the pairing reads and failing if any reaches the encoding. The
pairing reads more content than the record carries, which is exactly why the test must cover
it: the closing cases scan **message text** for task notifications (the returns-marker scan) and
**tool result text** for background-job acknowledgements, so the planted secrets must include
message content and result content, not only tool arguments.
T10. **A BUILT-IN tool name is exported verbatim.** An MCP name is not: it follows T11. The
built-in vocabulary is closed per client but **not shared between clients** — one agent's
`Bash` and `Read` are another's `bash` and `read` — so there is no single list to normalise
against, and normalising would lose which spelling the client actually sent. Within that
source, then, the name is never rewritten: no case folding, no canonical spelling.
`toolSource` (T4) decides which rule applies, and it is decided by the client's own list.

T10a. **A name whose source cannot be determined (`toolSource: "unknown"`) follows T11, not
T10** — it is hashed. Neither of the two rules above covers it otherwise, and the safe default
for a name that might be operator-defined is to treat it as if it were: a hashed built-in costs
a consumer one unreadable label, where a cleartext MCP name costs a customer's identity. So
the branch is on "is this provably a built-in", never on "is this provably MCP".

"Verbatim" is scoped to built-ins deliberately, and the scope is the privacy rule rather than
a detail of it. An earlier draft of T10 said "whatever its source", which read as a licence to
export `mcp__acme-billing-prod__refund` in clear text and contradicted T11 two lines below.
An implementer who reads one requirement and not the next must still land on the safe
behaviour — which is why T10a exists rather than leaving the third value to inference.
T11. An **MCP tool name is operator-defined and namespaced**, so it can carry a vendor
relationship, an internal system name or a project codename — `mcp__acme-billing-prod__refund`
is the worked example. It is therefore **not** treated as a built-in name. It is exported as a
**keyed** hash with a typed prefix (`hmac-sha256:` + hex), under an install-local key. A plain
digest is not enough: the namespace makes the input guessable, so an unkeyed hash of a
known vendor name is reversible by trying candidates. The cost is that counts do not join
across installs, which is the right trade for a name that may identify a customer. Hashing
happens **before** T12's cut, so the cut never splits a digest.
T11a. **The key is generated once and kept**, because a key that changes makes T1 and T4 fail
for MCP traffic: the same tool would carry a different label in every export, and no consumer
could total its time across two of them. Concretely — created on first use by an export that
needs it, 32 bytes from `crypto/rand`, written `0600` beside the install's other state, read
back unchanged thereafter, following the read-or-create pattern `cmd/cortex`'s gateway token
already uses. **It does not rotate.** Rotation would silently re-label every MCP tool from that
point, splitting one tool's history into two series that a consumer cannot rejoin; anyone who
needs new labels deletes the key and knows the old exports no longer compare. A lost key is the
same event, and the honest answer to it is that the old labels are unrecoverable — which is the
property that made a keyed hash safe to ship in the first place.
T12. A tool name is bounded on the way out by the same `exportLabel` every wire-sourced
string in a record passes: control runes removed, then truncated to `maxLabelLen` on a rune
boundary. `maxLabelLen` is **96 bytes**, not runes (`sessionexport.maxLabelLen`), so a multi-byte name
is cut sooner than its rune count suggests.

### 4.3 Data rules

T13. A `toolCall` record is **flat scalars only**, as #1810 R2 requires of a record. No array,
no nested object. This is what keeps one line one thing and keeps `jq` usable.
T14. `resultBytes` is the size of the tool's result, and `inputBytes` the size of its
arguments, as the effort proxy T7 permits instead of content. **Both are byte counts, and the
names say so** — a size is not a character count and the two differ for any non-ASCII payload.
Absent means not measured, never zero — the export's zero-means-absent rule. A size is not
content: it is the same concession the trajectory schema's `sessionToolCall` already makes
with `command_chars` and `result_preview_chars`.
T15. **A batch of calls is marked as such.** A record whose duration comes from a window
shared with the other calls of its asking response carries `durationShared: true` and the
batch's size in `parallelN`. The batch's identity is `callSeq` (§3.4), so no separate group key
exists. A consumer can then divide, take the window once per batch, or exclude it — explicitly,
by its own choice. The marker says the window is shared, **never** that the calls ran
concurrently.
T16. A call with no result and **no end** carries no duration, rather than the zero-length
span `/timeline` draws it with. A zero would pass a consumer's `durationMs > 0` filter as a
0 ms call and drag a p50 toward zero. Which case it was is `endReason` (T31), so the record
carries no separate `lost` flag.

**`stranded` is the exception, and it is not a contradiction**: that call HAS an end, so it has
a duration. Its agent finished the tool and then could not reach the model to report back, and
the proxy cuts the span at that first failed dial — which measured within a fraction of a
second of the tool's real end on the sessions checked against Claude Code's transcripts. So the rule is "no end, no
duration", not "no result, no duration": `durationMs` is present exactly when
`endReason.HasEnd()`, which is `result`, `background` and `stranded`.

**Note for #2041:** `toolpair.EndReason.HasEnd()` there is `result || background`. Whichever of
§2.2's two options lands, `HasEnd` has to include `stranded`, or `omitempty` drops the duration
of a stranded call that measured 0 ms.
T17. A span still open when the walk reads it is not exported. An unfinished call has no
duration to report, and a record that appears with one later would make the export
non-reproducible.
T18. Durations are milliseconds, truncated, matching `Record`.
T35. **Six `toolCall` fields are always written, even at zero**, because for them zero is a
real value rather than "not recorded" — the opposite of the event record's rule:

| Always written | Why zero is real |
|---|---|
| `kind` | the discriminator (T19) |
| `sessionId`, `startedAt` | identity and placement (T29) |
| `callIndex` | the FIRST call of every batch is 0, and T34 makes it a join key |
| `durationMs` on a reason that `HasEnd` | a result clamped to its call's start is a real 0 ms call (§3.5), and T31 requires a duration there |
| `endReason` | every exported call has one |

`isError` stays `omitempty`: absent reads as false, which is what it means. The rest of the
record keeps the export's zero-means-absent rule (#1810 R3).

### 4.3a Model time, tool time and user time

The three quantities a capacity plan needs, and the one asymmetry that makes the split sound:
**model time is measured, not inferred.** `durationMs` on an inference response *is* the
model's time. Only the leftover interval has to be classified.

**One named interval, defined once.** The first draft of this section defined `gapMs` from the
previous tool call's end to this call's start, and classified a different interval in T25 —
which cannot be checked, because a call starts when its asking response ends, so that first
interval always contains the asking response's own inference. Corrected:

> **`gapMs` is the idle interval BEFORE this call's asking response: from the end of the
> previous response of the same agent, to the start of the request that produced this call's
> asking response.** It therefore contains **no** `modelMs` and no tool execution of this
> call. It is the time the agent was neither talking to the model nor running this tool.

T23. A `toolCall` record carries `modelMs`: the measured duration of the inference response
that asked for this call. Copied from that response, not derived — so a consumer gets the
inference/tool ratio per call without joining back to the event record. It sits **after**
`gapMs` and **before** `durationMs` on the timeline, and the three never overlap.
T24. A `toolCall` record carries `gapMs`, the interval defined above. Absent on an agent's
first turn, where there is no previous response — absent, never zero.
T25. A `toolCall` record carries `gapKind`, a **closed vocabulary** exported by naming its
values, never by sanitising what arrives. It classifies the `gapMs` interval by **the previous
response of the same agent** — the one whose end begins the interval:

| `gapKind` | That previous response | What the interval was |
|---|---|---|
| `user` | `toolCallCount == 0` | The model stopped and waited for a human. Nothing else can be happening. |
| `tool` | `toolCallCount > 0` | The previous turn's tool calls were running. Their own durations are on their own records. |
| `first` | there is none | The agent's first turn. `gapMs` is absent. |

A fourth value is **not** needed for "a model call landed inside the gap": by the definition
above the interval ends at the next request, so no inference can be inside it. That was an
artifact of the old definition.

T36. **`modelMs` and `gapMs` belong to the asking RESPONSE, not to the call**, so every record
of a batch repeats the same two values. Two rules follow, and the docs state both beside the
fields:

- **Take them once per `callSeq`.** Summing `modelMs` over `toolCall` records counts a batch's
  inference `parallelN` times — the same inflating bias §3.4 describes for `durationMs`, except
  here it is in the NUMERATOR of T2's ratio, and `durationShared` does not mark it.
- **An aggregate inference-to-tool ratio needs the event records.** A response that asks for no
  tool — a final answer, or a question to the user — produces no `toolCall` record at all, so
  its inference time appears on no record here. `modelMs` answers "what did this call's turn
  cost"; it cannot answer T2 on its own.

T37. **A `tool` gap is not idle time, and must not be added to tool time.** By §4.3a's
definition the interval covers what the previous turn's calls were doing, so for a `gapKind`
of `tool` it overlaps those calls' own `durationMs`: adding the two counts the same wall clock
twice. Only a `user` gap is time no tool and no model was working, which is why it is the one
that answers a duty cycle.

**A consumer with the event records can derive all three fields**, so these definitions are
the deliverable and the fields are the convenience. A definition that cannot be checked
against the data is worse than no field.

T26. **An approval wait is inside `tool` and cannot be separated from execution.** When a user
sits on a permission prompt, that time lands in the tool gap and is indistinguishable from the
command running. E15's SPEC caps it at 30 s for exactly this reason. This is the largest
single error source in any density figure built from this export, and the docs say so beside
the field. Only #1924 (Claude Code telemetry, which records permission waits explicitly)
removes it.
T27. **A `user` gap is meaningless without a stated ceiling, and the record does not impose
one.** Measured on the reference archive, the same data totals 24.8 h at a 300 s cap, 44.8 h
at 1800 s, 56.8 h at 3600 s and **539 h uncapped** — a 20x spread. So the export carries the
raw `gapMs` and the documentation requires any consumer publishing a duty cycle to publish its
ceiling alongside it (#1810 §9 states the same rule for event gaps).

Why this split is worth the three fields: on the reference archive, user wait is **72.6% of
all gap time** (44.8 h of 61.7 h, at an 1800 s ceiling — T27's own rule) and costs nothing in
sandbox occupancy. Without `gapKind`,
a density plan reads that as idle capacity it is paying for. The classifier is already
validated — run on that archive it separated `AskUserQuestion` (p50 62.6 s) from `Bash`
(p50 201 ms) by three orders of magnitude **without ever seeing a tool name** (#1810 §9).

### 4.4 Schema and compatibility

T19. A `toolCall` record is a **sibling record on the same NDJSON stream**, discriminated by a
`kind` field, not a widening of the event record. An existing consumer either filters
`select(.kind != "toolCall")` or ignores the key: an absent `kind` reads as the event record,
so no shipped consumer breaks.
T20. `toolCall` records are **opt-in**, per #1810 R49's rule for Tier 2: a plain
`GET /v1/export` is byte-identical to today's. **Settled (§10.1): per item** —
`?include=toolCalls` on the endpoint and `--with-tool-calls` on the command, not a single
Tier-2 switch, which would opt a consumer into items it never asked for.
T21. Adding the record does not bump `Record`'s `schema`. A new record *kind* on the stream is
additive; `sessionexport.SchemaVersion` bumps only on a removal or a meaning change.
T28. An event record carries an explicit `kind: "event"` **only on a stream that carries
`toolCall` records** — that is, only when T20's opt-in is on. A plain `GET /v1/export` is
byte-identical to today's, which T20 requires and which adding a key to every record would
break. On an opted-in stream a consumer can then select on `kind` in both directions rather
than on the absence of a key; an absent `kind` still reads as an event record, so a file from
either mode stays readable.

### 4.4a Placing and ordering a call

T29. A `toolCall` record carries `startedAt`, the call's start — the asking response's end. It
is **always present**, including one that never got a result, so every record can be placed
on a timeline. The end, where there is one, is `startedAt + durationMs`. The record
carries no separate `seq`: that collided with the event record's own `seq` and was undefined
for an unresolved call.
T30. A `toolCall` record carries `callIndex`, the call's position among the calls its asking
response made, from 0, in the order the model emitted them. A batch is otherwise unorderable.
T31. A `toolCall` record carries `endReason`, a closed vocabulary naming how the call ended.
`durationMs` is present only for the reasons that have an end:

| `endReason` | Duration? | Meaning |
|---|---|---|
| `result` | yes | Its result arrived. |
| `background` | yes, **to the acknowledgement only** | The result only acknowledged a job that keeps running (`backgroundRe`, in the pairing). The command's real duration is longer and is not here. |
| `ended_run` | no | By design no result: the call ends the run or hands control back (`SubagentHandback`, `StructuredOutput`, `end_subtask`). |
| `abandoned` | no | The agent moved on without the result (`lastTurn`). |
| `stopped` | no | The run was stopped while the call was open (a `<task-notification>`). |
| `stranded` | **yes** | The agent finished the tool and could not reach the model to report back: the first failed dial to a model host after the call started ends it (`endStrandedTools`). The duration is the tool's, not the outage's. `resultSeq` is absent when no result ever came, and set when one arrived late (see below). |

**`stranded` is the fifth closing case, and this spec missed it until review.** `Build` runs
`endStrandedTools` AFTER the pairing (`buildAgents` then `endStrandedTools`, both from `Build`), and it sets a tool's
`End` on a non-`Lost` span without setting `Lost` — so the timeline has a call with an end and
none of the other four reasons. A pairing that stops at those four leaves such a call with an
empty reason, which T35 says cannot happen on an exported record, and T17 would then drop a
call that did in fact finish.

**`stranded` WINS over `result` — and over `background` — when both apply.**
`endStrandedTools`'s own doc says the result "reaches the proxy only once the network is back,
hours later, or never". When it does arrive,
`buildAgents` first ends the call on it and the stranding pass then moves `End` back to the
dial — so the call satisfies both reasons, and the one that EXPLAINS the duration is the one to
export. Concretely, for a stranded call whose result arrived late:

- `endReason` is `stranded`, not `result`. The same holds for a `background` acknowledgement
  the cut lands before: the reason that explains the duration wins.
- `resultSeq` **stays present**: a request did carry the result, and a consumer joining on it
  is right to.
- `durationMs` ends at the **dial**, not at `resultSeq`. This is the one case where the two
  disagree, and the `callSeq`/`resultSeq` row would otherwise imply the latter.

So whichever option below is chosen must apply the stranding cut to a call **with** a result as
well as one without. **Open** with the rest of this case.

**The cut is matched by host, not by agent, and that bounds how precisely `stranded` explains a
duration.** `endStrandedTools` names no agent: its documented `shortcut:` is that a blip in any
other agent — a parent included — ends a tool whose own branch had no success first, so a
subagent's long-running `Bash` can be cut at the moment the main agent retried. Two agents
stranded by one outage therefore both end at its first failure. For a fleet latency figure this
is the right trade, since the alternative is a span that runs to the session's last unrelated
event; for a single call it means `stranded` says "cut at an outage this agent could not be
proven to have survived", not "cut at this agent's own failure". The upstream note says to
revisit if CONNECTs ever carry the agent id.

Implementation note: the pairing package must therefore learn this case, which it does **not**
do today — `core/session/toolpair` was extracted from `buildAgents` only, and
`endStrandedTools` needs `t.Agents`, `t.Turns` and `t.Hosts`, which the pairing has none of.
Two ways out, and this is **Open**: carry the host-failure times into the pairing, or let the
export apply the stranding pass itself over the records it is about to write. The second keeps
the pairing's inputs as they are; the first keeps the reason where the other four live.

**No `lost` value.** An earlier draft had one for "open with no reason established", and no
case produces it: a call with none of the five reasons above still has `End == nil`, which
means **still open**, and T17 does not export an open call at all. A vocabulary entry a
producer never emits is a trap for the consumer — it switches on it and ships a dead branch,
or reads it as the catch-all for "no result" and misses the four reasons that can occur
without one — `ended_run`, `stopped`, `abandoned`, and `stranded` where no late result
arrived. Add one only alongside the code that returns it.

These are the timeline's own closing cases (the closing pass), named rather than collapsed
into one `Lost` bool: "the agent moved on" and "the call ends the run by design" are different
facts, and a consumer filtering latency wants to exclude both but count them apart.
T32. **A backgrounded command is MARKED, not silently undercounted.** A `Bash` call that starts
a background job ends at the acknowledgement, and the job's own run is a separate
`timeline.Job` (a `Job` of its own), so this record cannot carry the command's real duration.
It therefore sets `endReason: "background"` with `durationMs` measuring only the
acknowledgement. A count of such calls tells a consumer **how much tool time is missing** and
from where, which an undifferentiated `result` would hide.

A job record is deliberately left for later: its start, its duration or an `endReason` when it
never ended, and its starting call's `callSeq` + `callIndex` (no ids), paired by the
`<task-notification>` match. **Note for whoever specifies it:** a backgrounded command overlaps
later calls, so summed per-command time can exceed a session's wall-clock tool time. A
consumer must not add job time to `durationMs` totals.
T33. A `toolCall` record whose asking response is below a `since` floor is **not exported**.
`Walk` stops at the first event below the floor (`Walk`'s `since` stop), so such a call has no
start to report, and T29 requires one. A result inside the window whose call is outside it
therefore yields no record, rather than a record with a missing start.

### 4.5 Documentation

T22. `docs/session-export.md` states the §3.4 parallel-window rule and T16's end-versus-result
rule beside the record, because both cost real accuracy and neither is guessable from the field
list. The doc fix that precedes this spec already states them for `/timeline`.

---

## 5. The record

```json
{"schema":1,"kind":"toolCall","sessionId":"45fa31b2-…",
 "startedAt":"2026-11-05T15:52:25.629885Z","tool":"Bash","toolSource":"builtin",
 "callSeq":2077,"callIndex":0,"resultSeq":2081,
 "durationMs":473,"endReason":"result","isError":false,
 "inputBytes":64,"resultBytes":1432,
 "agentRole":"main","client":"claude-code/2.1.270",
 "modelMs":4279,"gapMs":812,"gapKind":"tool",
 "parallelN":3,"durationShared":true}
```

Four changes from the first draft, all so a record can be placed and ordered on its own:

- **`startedAt` replaces `at`.** The first draft carried the *end*, so a call with no result — which
  has no duration — could not be placed on a timeline at all. A start exists for every call
  (the asking response's end), so it is always present; the end is `startedAt + durationMs`
  when there is one.
- **`seq` is gone.** It copied the result event's seq, which collides with the event record's
  own `seq` and is undefined for a call that never got a result. `callSeq` and `resultSeq` carry the two real
  anchors.
- **`callIndex` added.** The call's position among the calls its response asked for, from 0,
  in the order the model asked. Without it a batch cannot be put back in order.
- **`parallelGroup` is gone**, replaced by `callSeq` + `callIndex`. The group *is* the asking
  response, so `callSeq` already identifies it; a separate composite string was a second
  spelling of the same fact.

| Field | Why it is here |
|---|---|
| `kind` | T19. The discriminator. Always `"toolCall"` here; an event record carries `"event"` (T28). |
| `sessionId` | T5. The session, as the client asserted it. Not a unit of work until #1997. |
| `startedAt` | T29. When the call started — the asking response's end. **Always present**, so a call with no result can still be placed. |
| `tool` | T10/T10a/T11. A built-in name verbatim; an `mcp` or `unknown` name as a keyed hash. Bounded by T12. |
| `toolSource` | T4. `builtin`, `mcp` or `unknown` — a closed vocabulary, exported by naming the values. Only `builtin` exports its name in clear (T10a). |
| `callSeq` / `resultSeq` | The response that asked and the request that carried the result. Lets a consumer rejoin a `toolCall` to the event records either side **without a tool-use id** (T8). `resultSeq` is absent when nothing carried a result. |
| `callIndex` | T30. The call's position among its response's calls, from 0, in the order the model asked. |
| `durationMs` | T18. The paired duration. Absent exactly when `endReason.HasEnd()` is false (T16, T31) — so present for `stranded`, whose end comes from the failed dial whether or not a result ever arrived. |
| `endReason` | T31. Closed vocabulary: how the call ended, and the only thing that says whether a result arrived. No separate flag derives from it. |
| `isError` | Whether the result was an error. The tool failure rate, per tool. |
| `inputBytes` / `resultBytes` | T14. Effort proxies, never content. Bytes, as the names say. |
| `modelMs` | T23. The asking response's measured duration — model time, per call, no join needed. |
| `gapMs` | T24. The idle interval before the asking response. Contains no `modelMs`. |
| `gapKind` | T25. `user`, `tool` or `first` — what that interval was. The field that turns idle time into a duty cycle. |
| `agentRole` | T5. `main` or `subagent`, so subagent tool traffic reads apart. |
| `client` | T5. Cost-attribution and display key. **Never an authorization subject.** |
| `parallelN` / `durationShared` | T15. The §3.4 finding. The batch is `callSeq`; these say how many and whether the window is shared. |

Not here, deliberately: the tool-use id (T8), arguments (T6), result content (T7), the
`/timeline` `preview` (it is built from argument keys and is explicitly "no prompts, not safe
to share" — #1810 §9.1).

---

## 6. Where the pairing runs

Three options. The spec records all three because the choice has consequences a reviewer
should weigh, not just the one the author prefers.

### 6.1 Option A — extract the pairing into a shared package

Move the span-building out of `core/session/timeline` into something both the timeline and the
export call.

- **For:** one copy of a subtle algorithm, with §2.2's four closing cases in one place. Both
  consumers get a fix at once.
- **Against:** `timeline` is consumed by agentop, so the extraction is a refactor with a live
  dependent. It is the largest change of the three.

### 6.2 Option B — `sessionexport` depends on `timeline`

The walk calls the timeline builder per session and projects its spans.

- **For:** smallest diff, no new pairing code.
- **Against:** inherits the 50,000-event cap, which bounds the export by a timeline limit that
  has nothing to do with it; and it builds turns, hosts and agents the export discards. The cap
  lives in the **HTTP handler** (`sessionapi`'s `timelineMaxEvents`), not the builder, so an in-process
  caller could avoid it — but then the export depends on a limit staying where it is.

### 6.3 Option C — a second, bounded pass in `sessionexport`

The walk makes a pairing pass per session, keeping only tool-use id → (name, start) until the
result lands.

- **For:** keeps the export's dependencies as they are; the state is one map per session, not
  the whole timeline.
- **Against:** a second implementation of §2.2's closing cases, which will drift from the
  timeline's.

**Settled (§10.1): Option A**, scoped to tool spans and their closing inputs. §2.2's four
closing cases are exactly the kind of logic that must not exist twice, and the export is the
second consumer that wants it — a third (an OTLP emitter, #505) would make the duplication
permanent. Note for the implementer: because the closing cases read message and tool-result
text, T9's planted-secret test must plant in both, not only in tool arguments.

### 6.4 Memory, whichever option wins

T-level constraint rather than a numbered requirement, because it binds all three:

The walk is O(1) per event today. Pairing is O(open calls) per session. On the reference
archive a session held 2,911 events; the largest in §3 held 722 with 204 tool spans. A bound
is needed, with a documented behaviour when it is hit — the export's existing posture is to
degrade loudly (a truncated stream resets the connection) rather than silently, and this
should match. **Settled (§10.1):** a fixed cap on open calls per session, and a per-session
marker record when it is hit.

---

## 7. What this does not answer

7.1 **The proxy sees the gap around a tool, never inside it.** A correction to the first draft
of this spec, which claimed local tool calls are invisible. They are not: a span runs from the
asking response's end to the next request carrying the result (the span-emission loop and the closing pass in `buildAgents`; #2041's `toolpair.Calls` and `closeCalls`),
and **both boundaries are model traffic**, so a tool that touches nothing on the network still
gets a span. §3.1's `Edit` at p50 35 ms is such a call. The earlier claim confused this record
with #1810 §9's gap classification, where "no proxied traffic landed inside the gap" is how a
*local* call is inferred — a different measurement answering a different question.

What the proxy genuinely cannot see is **what happened inside the span**: whether the time went
to the tool running, to a permission prompt, to client-side queueing, or to a transport leg
(§3.5). So the record measures a client round trip per call, with full coverage of calls and
no visibility into their composition.

7.1b **The record cannot say WHERE a call ran, and therefore what it cost there.** The
question is where a tool runs, not whether a sandbox is involved — and the proxy sees neither:

- A built-in tool (`Bash`, `Read`, `Edit`) may run on the developer's machine **or** in a
  remote sandbox the agent sends its tools to.
- An MCP tool may be served by a **local** server over stdio **or** a **remote** one over HTTP.

A model proxy sees the model ask for a call and, later, the result arrive. So the record
measures **each call's time as the agent saw it**, and says nothing about where the work
happened. A local call costs the developer's machine; a sandboxed one, the sandbox host; a
remote MCP call, whoever runs that server. Each of those hosts also measures the call
differently — a sandbox's own timing excludes the transport leg this record includes, so the
two disagree and neither is wrong.

**`toolSource` says what KIND of tool it is, never where it ran.** `builtin` and `mcp` are
properties of the tool, not of its execution site, and the proxy must not infer a site from
them. If a client knows where a call ran, an optional field carrying what the client stated
would be the place for it — never a proxy-side guess.

T34. **The join keys are stable**, so a consumer holding a host's own records can join on its
side: `sessionId`, `startedAt` (UTC), `callSeq` + `callIndex`, and `agentRole`. Those five are
the contract this record offers a capacity plan; their names and meanings do not change without
a `schema` bump. Cortex specifies no sandbox-side record — a consumer that has one joins it
itself.

7.1a **An approval wait reads as execution.** T26: a permission prompt the user sits on lands
inside a `tool` gap, indistinguishable from the command running. Nobody has quantified it
here, and it inflates exactly the figure a sandbox is sized against. Measuring it against
local Claude Code transcripts — where the harness records the wait — is the cheapest way to
bound the error, and should happen before any density number from this export is published.

7.2 **Which `Bash`.** The name is `Bash`; which command ran is in the arguments, which T6 and
T0 keep out permanently. So this spec answers "how long do tools take" and never "what did they
do". A workload taxonomy finer than the tool name needs #1810's Tier 3 and a different privacy
posture.

7.3 **A session is not a unit of work** until #1997 lands `sessionIncarnation`. Per-session
aggregates of `toolCall` records average across unrelated sittings exactly as event records do.

7.4 **Prompt grouping** is #1922. A `toolCall` record says which turn asked (`callSeq`), not
which user instruction.

---

## 8. Relationship to adjacent work

| Issue | Relationship |
|---|---|
| #1906 | The tracker. This spec narrows its §2 and **supersedes** its "list on the response record" shape, for the §3.4 reason. |
| #1810 | The parent spec. This is its §7.2 `toolCallNames` item, re-scoped from names to paired calls. |
| #1740 | Adds `newToolResults` and `toolResultErrors` to the event record. Complementary: those count results per event; this counts calls with durations. |
| #1924 | Claude Code telemetry records what happens INSIDE a span, which §7.1 says the proxy cannot see: the permission waits T26 cannot separate from execution (§7.1a), and where a call actually ran (§7.1b). It is the fix for this spec's largest error source, so the two should be planned together even though this one ships first. |
| #1921 / #1925 | Redaction and the content export. Where tool arguments and result content go, per T0 — a different privacy posture, not a flag on this record. |
| #1997 | `sessionIncarnation`. Needed before per-session aggregates mean anything (§7.3). |
| rossoctl/trajectory-schema | **A consumer exists.** That schema added an observed-session kind (`trajectory-schema/session/1`, v2.1) and `tools/from-cortex.py`, which converts a `/v1/export` stream of event and `toolCall` records into it and into OTLP/JSON. Its field-by-field mapping is what surfaced T29 (a start on every record) and T30 (`callIndex`): without them a lost call cannot be placed and a batch cannot be ordered. Both are now required. The converter reports `gapKind` as unmapped and derives it from the event records instead, which is the right division — §4.3a says the definitions are the deliverable. |
| #505 | OpenTelemetry's GenAI conventions name a tool span `execute_tool {gen_ai.tool.name}` with `gen_ai.operation.name=execute_tool`, and `gen_ai.tool.call.id` is only **conditionally** required — so T8's omission of the tool-use id stays spec-compliant. An OTLP emitter would also be the third consumer of §6's pairing. |

---

## 9. Scope of the measurements

One developer's laptop archive over 30 days: 40,496 records, 12,104 tool calls, heavily
`claude-code/2.1.270` with 46 `codex/0.163.0` records. Four sessions had timeline spans.

The **mechanism** generalises, because it follows the event schema and the pairing rules, not
this traffic. The **ratios do not**: 4.2 : 1 inference-to-tool and `Bash` at over 97% of tool
time (811.1 s of 828.2 s) will move with working style, prompting and agent. Fleet numbers are the entire point of
building this, and they will come from the export rather than from §3.

A second caveat on §3.4: 14% is one archive's parallel rate under one agent's batching
behaviour. #1810 notes Claude Code's calls are sequential in the trace while Codex's overlap,
so the rate is agent-dependent and the 46 Codex records here cannot speak to it. The
requirement T15 derives from it stands regardless — a shared window must be marked whatever
its frequency — but the *rate* is not a constant to design against.

---

## 10. Open items, collected

### 10.1 Settled in review (#2009), and now normative

| § | Decision |
|---|---|
| T11 | **Keyed** hash (`hmac-sha256:` + hex, install-local key), not a plain digest: a namespaced name is guessable, so an unkeyed digest is reversible. Cross-install counts are given up deliberately. |
| T20 | **Per item** (`?include=toolCalls`). One Tier-2 switch would opt a consumer into items it never asked for. |
| §6 | **Option A**, scoped to tool spans and their closing inputs. |
| §6.4 | A fixed cap on open calls per session, and **a per-session marker record** when it is hit — matching the export's degrade-loudly posture. |
| T25 | **No fourth `gapKind`.** The redefined interval ends at the next request, so no inference can fall inside it; the question was an artifact of the first draft's definition. |
| T32 | **`endReason: "background"`** now, marking the undercount per call; a job record later. A consumer must not add job time to `durationMs` totals, since a backgrounded command overlaps later calls. |
| §7.1b | **Cortex specifies no sandbox-side record.** It guarantees stable join keys (T34) and a consumer joins its own host's records. |
| §3.3 / T3 | **Banding is the analyzer's policy.** The export carries raw timings; the spec names no bands, so the naming collision is not the export's to settle. |

### 10.2 Still open

| § | Open |
|---|---|
| §7.1a | Whether to quantify the approval-wait error against local Claude Code transcripts before publishing any density figure. Author's view: yes, and it is cheap. |
| T32 | Whether the job record gets specified in this cycle or a later one. The undercount is marked either way. |
| §7.1b | Whether an optional client-stated execution site is worth a field, for a client that knows where its tools ran. Not a proxy-side inference under any circumstance. |

Nothing above blocks implementation: §6 Option A, T11's keyed hash, T20's per-item switch and
§6.4's marker record are all settled, and T28–T34 are normative.

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

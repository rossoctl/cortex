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

**22 numbered requirements T1–T22**, phrased as plain declaratives: a requirement says what
the system does once built. The numbering is stable so it can be cited in review and in the
PRs that implement it.

Written after measuring, not before. §3 is the measurement; every design choice in §5 and §6
traces to a number in it. The measurement corpus is one developer's 30-day laptop archive:
40,496 export records, 12,104 tool calls, `claude-code/2.1.270` throughout with 46
`codex/0.163.0` records. **The mechanism generalises; the ratios do not** — see §9.

---

## 1. Goal

1. Answer "which tools does our community run, and what do they cost in time" from the
   export alone, across every session and beyond the archive's 30-day retention.
2. Answer "how does inference time compare with tool time" as a ratio a capacity plan can
   use.
3. Keep the export content-free by construction, as #1810 §3.2 requires: no tool arguments,
   no tool results, no prompts.

### 1.1 Why the export, when `/timeline` already pairs tool calls

`GET /v1/sessions/{id}/timeline` answers per-tool latency today, content-free, by pairing on
tool-use id (#1810 §9.1). It is the right tool for one session. It cannot do this job:

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

### 2.1 Shipped

- `core/sessionexport.Record` — ~36 flat scalars per event, an allowlist, with
  `TestRecord_CarriesNoContent` pinning the privacy contract (#1812).
- `Walk` over `archive.Earlier` — newest-first, one event at a time, O(1) state (#1830).
- `GET /v1/export` and `agentop export` (#1830, #1831), with `--gzip` (#1996).

A record carries `toolCount` (the manifest the request offered) and `toolCallCount` (the
calls the response emitted). It carries no tool name and no per-tool timing.

### 2.2 The pairing already exists, in `core/session/timeline`

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
  (`stopped`), and background jobs (`backgroundRe`) that extend a `Bash` span past its
  acknowledgement.

### 2.3 The gap, and why it is structural

`Walk` is newest-first and stateless; pairing needs forward lookup and per-agent state. So
this is **not** a new field on `FromEvent`. It needs either a buffered second pass per session
or a reorganised walk — a design decision with a memory cost, in a package whose premise is
that it is cheap. §6 is that decision.

---

## 3. The measurement

Method: `/v1/export` for the record view, `/v1/sessions/{id}/timeline` for the paired view,
over the same four sessions. 268 tool spans, 267 usable (one still running, zero `lost`).

### 3.1 Per-tool timing

| Tool | n | p50 | p95 | total | share of tool time |
|---|---|---|---|---|---|
| `Bash` | 210 | 473 ms | 7,561 ms | 598.9 s | 97.4% |
| `AskUserQuestion` | 1 | 12,714 ms | — | 12.7 s | 2.1% |
| `Agent` | 1 | 1,093 ms | — | 1.1 s | 0.2% |
| `Read` | 17 | 41 ms | 194 ms | 1.0 s | 0.2% |
| `Edit` | 29 | 24 ms | 66 ms | 0.8 s | 0.1% |
| `Write` | 9 | 16 ms | 224 ms | 0.4 s | 0.1% |

Errors: `Bash` 11, `Read` 3.

**`Bash` is 78% of calls and 97.4% of tool time.** One tool dominates, which is why the name
alone nearly answers the workload question — and why the *duration distribution*, not the
name, is the signal worth carrying.

### 3.2 Inference against tools

| | calls | total | p50 |
|---|---|---|---|
| Model | 307 | 2,510.7 s | 5,473 ms |
| Tool | 267 | 614.8 s | 243 ms |

**4.1 : 1** — inference is 80.3% of measured work. For sandbox capacity this is the headline:
the agent waits on the model four times longer than it runs tools.

### 3.3 Weight tiers, on E15's cuts (1 s / 10 s)

| Tier | share of calls | share of work |
|---|---|---|
| light (<1 s) | 76.0% | 8.3% |
| medium (1–10 s) | 22.1% | 36.3% |
| **heavy (>10 s)** | **1.9%** | **55.4%** |

The heaviest 10% of calls carry **77.7%** of tool time; the heaviest single call was a 145.2 s
`Bash`. This reproduces E15's concentration finding (7.4% of calls / 86.5% of work) on our own
traffic, independently. It is the number density planning turns on, and it is the reason a
mean is useless here: a design that reports averages hides the tail that sizes the sandbox.

### 3.4 Parallel calls share one window — the finding that shapes the record

**42 of 267 spans (15.7%) carry an identical start and end**: calls the model issued together.
The timeline gives a batch one merged interval, not a duration each.

So a naive per-call `durationMs` is wrong for one call in six, and wrong in the direction that
inflates: charging every member of a batch the whole window's time overcounts tool time and
drags the heavy tier upward. This is the same class of error as the 272x tool-result overcount
#1810 documents, and the record must make it explicit rather than leave a consumer to discover
it.

### 3.5 The two views reconcile

Summed `toolCallCount` over the same sessions is **273** against **267** paired spans — 2.2%
apart, consistent with calls in flight at the archive boundary. A record derived archive-side
will therefore not contradict `/timeline`.

---

## 4. Requirements

### 4.1 Needs

T1. The export answers per-tool call counts and duration distributions across every archived
session, without a second endpoint and without a hand-written join.
T2. The export answers the inference-time-to-tool-time ratio per session and in aggregate.
T3. A consumer can compute the §3.3 weight tiers from the export alone.
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
secret in every content field the pairing reads — tool arguments, result content and the
preview fields `/timeline` builds — and failing if any reaches the encoding.
T10. A built-in tool name is exported verbatim: it comes from a closed vocabulary
(`Bash`, `Read`, `Edit`, …).
T11. An **MCP tool name is operator-defined and namespaced**, so it can carry a vendor
relationship, an internal system name or a project codename — `mcp__acme-billing-prod__refund`
is the worked example. It is therefore **not** treated as a built-in name. **Open:** gate it
off by default, or export a stable hash so distinct MCP tools stay countable without being
readable. Hashing is the author's recommendation: it preserves T1 and T4 for MCP traffic,
which gating does not.
T12. A tool name is bounded on the way out by the same `exportLabel` every wire-sourced
string in a record passes: control runes removed, then truncated to `maxLabelLen` (96) on a
rune boundary.

### 4.3 Data rules

T13. A `toolCall` record is **flat scalars only**, as #1810 R2 requires of a record. No array,
no nested object. This is what keeps one line one thing and keeps `jq` usable.
T14. `resultBytes` is the size of the tool's result, as the effort proxy T7 permits instead of
content. Absent means not measured, never zero — the export's zero-means-absent rule.
T15. **A batch of parallel calls is marked as such.** A record whose duration comes from a
window shared with other calls carries `durationShared: true`, the batch's size in
`parallelN`, and a `parallelGroup` key that is stable within the session. A consumer can then
divide, take the window once per batch, or exclude it — explicitly, by its own choice.
T16. A `lost` span — one that never got a result but provably stopped — is marked `lost: true`
and **carries no duration**, rather than the zero-length duration `/timeline` draws it with. A
zero would pass a consumer's `durationMs > 0` filter as a 0 ms call and drag a p50 toward
zero.
T17. A span still open when the walk reads it is not exported. An unfinished call has no
duration to report, and a record that appears with one later would make the export
non-reproducible.
T18. Durations are milliseconds, truncated, matching `Record`.

### 4.4 Schema and compatibility

T19. A `toolCall` record is a **sibling record on the same NDJSON stream**, discriminated by a
`kind` field, not a widening of the event record. An existing consumer either filters
`select(.kind != "toolCall")` or ignores the key: an absent `kind` reads as the event record,
so no shipped consumer breaks.
T20. `toolCall` records are **opt-in**, per #1810 R49's rule for Tier 2: a plain
`GET /v1/export` is byte-identical to today's. **Open:** `?include=toolCalls` on the endpoint
and `--with-tool-calls` on the command, or a single Tier-2 switch covering every §7.2 item.
T21. Adding the record does not bump `Record`'s `schema`. A new record *kind* on the stream is
additive; `sessionexport.SchemaVersion` bumps only on a removal or a meaning change.

### 4.5 Documentation

T22. `docs/session-export.md` states the §3.4 parallel-window rule and the T16 `lost` rule
beside the record, because both cost real accuracy and neither is guessable from the field
list. The doc fix that precedes this spec already states them for `/timeline`.

---

## 5. The record

```json
{"schema":1,"kind":"toolCall","sessionId":"45fa31b2-…","seq":2081,
 "at":"2026-11-05T15:52:26.102885Z","tool":"Bash","toolSource":"builtin",
 "callSeq":2077,"resultSeq":2081,"durationMs":473,"isError":false,
 "resultBytes":1432,"agentRole":"main","client":"claude-code/2.1.270",
 "parallelGroup":"2077#0","parallelN":3,"durationShared":true}
```

| Field | Why it is here |
|---|---|
| `kind` | T19. The discriminator. Always `"toolCall"` on this record. |
| `sessionId` | T5. The session, as the client asserted it. Not a unit of work until #1997. |
| `seq` | Orders the record within its session, as on an event record. The result event's seq. |
| `at` | When the call ended, so a record sorts on one time base with event records. |
| `tool` | T10/T11. The name, bounded by T12. |
| `toolSource` | T4. `builtin`, `mcp` or `unknown` — a closed vocabulary, exported by naming the values. |
| `callSeq` / `resultSeq` | The response that asked and the request that carried the result. Lets a consumer rejoin a `toolCall` to the event records either side **without a tool-use id** (T8). |
| `durationMs` | T18. The paired duration. Absent on a `lost` record (T16). |
| `isError` | Whether the result was an error. The tool failure rate, per tool. |
| `resultBytes` | T14. Effort proxy, never content. |
| `agentRole` | T5. `main` or `subagent`, so subagent tool traffic reads apart. |
| `client` | T5. Cost-attribution and display key. **Never an authorization subject.** |
| `parallelGroup` / `parallelN` / `durationShared` | T15. The §3.4 finding, made explicit. |
| `lost` | T16. Set, with no `durationMs`, on a call that stopped without a result. |

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
  has nothing to do with it; and it builds turns, hosts and agents the export discards.

### 6.3 Option C — a second, bounded pass in `sessionexport`

The walk makes a pairing pass per session, keeping only tool-use id → (name, start) until the
result lands.

- **For:** keeps the export's dependencies as they are; the state is one map per session, not
  the whole timeline.
- **Against:** a second implementation of §2.2's closing cases, which will drift from the
  timeline's.

**Open: which option.** The author leans **A**, because §2.2's four closing cases are exactly
the kind of logic that must not exist twice, and because the export is the second consumer that
wants it — a third (an OTLP emitter, #505) would make the duplication permanent.

### 6.4 Memory, whichever option wins

T-level constraint rather than a numbered requirement, because it binds all three:

The walk is O(1) per event today. Pairing is O(open calls) per session. On the reference
archive a session held 2,911 events; the largest in §3 held 722 with 204 tool spans. A bound
is needed, with a documented behaviour when it is hit — the export's existing posture is to
degrade loudly (a truncated stream resets the connection) rather than silently, and this
should match. **Open:** the bound and what happens at it.

---

## 7. What this does not answer

7.1 **Local tool calls are invisible.** `durationMs` here is proxy-observed, so it covers
tools that generate proxied traffic. An `Edit`, `Read` or `Bash` touching nothing on the
network appears only as gap time — #1810 measures that at 5,653 calls and 5.3 h. For sandbox
sizing this may be *correct*, since those calls cost no sandbox time either, but **it
undercounts call volume and any table built from it says so.** Closing the gap needs #1924
(Claude Code telemetry), not this spec.

7.2 **Which `Bash`.** The name is `Bash`; which command ran is in the arguments, which T6
keeps out permanently. So this spec answers "how long do tools take" and never "what did they
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
| #1924 | Claude Code telemetry would supply the tool times the proxy cannot see (§7.1), including permission waits. The better long-term answer for local tools. |
| #1997 | `sessionIncarnation`. Needed before per-session aggregates mean anything (§7.3). |
| #505 | An OTLP emitter would be the third consumer of the §6 pairing, and the reason to prefer Option A. |
| rossoctl/trajectory-schema | A `toolCall` record maps closely to that schema's `$defs.sessionToolCall`, which already records a tool call as sizes rather than content (`command_chars`, `result_preview_chars`). Worth aligning field names where the semantics match. |

---

## 9. Scope of the measurements

One developer's laptop archive over 30 days: 40,496 records, 12,104 tool calls, heavily
`claude-code/2.1.270` with 46 `codex/0.163.0` records. Four sessions had timeline spans.

The **mechanism** generalises, because it follows the event schema and the pairing rules, not
this traffic. The **ratios do not**: 4.1:1 inference-to-tool and `Bash` at 97.4% of tool time
will move with working style, prompting and agent. Fleet numbers are the entire point of
building this, and they will come from the export rather than from §3.

A second caveat on §3.4: 15.7% is one archive's parallel rate under one agent's batching
behaviour. #1810 notes Claude Code's calls are sequential in the trace while Codex's overlap,
so the rate is agent-dependent and the 46 Codex records here cannot speak to it. The
requirement T15 derives from it stands regardless — a shared window must be marked whatever
its frequency — but the *rate* is not a constant to design against.

---

## 10. Open items, collected

| § | Open |
|---|---|
| T11 | MCP tool names: gate off by default, or export a stable hash. Author recommends hashing. |
| T20 | The opt-in switch: per-item (`?include=toolCalls`) or one Tier-2 switch. |
| §6 | Which of the three pairing options. Author leans A. |
| §6.4 | The per-session pairing bound, and the behaviour when it is hit. |

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

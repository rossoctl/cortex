# Session event export: a content-free per-event projection — design

Status: **draft**.
Date: 2026-11-13.
Issue: rossoctl/cortex#1810 (feature tracker)
Shipped so far: #1812 — §4 (the record) and §5.1 (NDJSON), in `core/sessionexport`
Background: #898 (central collector), #505 (OTel for the proxy), #1740 (three count
fields), #1494 (lossless capture, closed), #1466 (long-term performance view),
#1739 (per-directory cost, closed), #1815 (project label)

Items marked **Open** must be decided before the work they belong to begins. Everything in
§4 and §5.1 is normative and already merged; §5.2 onward is requirements, not code.

Requirements are numbered R1–R56 and phrased as plain declaratives: a requirement says what
the system does once built, not what an implementer ought to feel. The numbering is stable —
cite it in review and in the PRs that implement it.

Related spec: [context-guru's metrics spec](https://github.com/rossoctl/context-guru/blob/docs/prometheus-metrics-spec/docs/metrics/spec.md)
is the **aggregate** layer to this one's per-event layer, and already names Cortex as one of
its two emitters. §8.6 compares the two metric by metric and says which Context Guru ideas
Cortex should adopt.

Revised 2026-11-13 after running the analysis against a live install rather than reasoning
about it. §4 is restructured to state what is collected field by field, and what is never
collected first (§4.1). §9 is corrected: per-tool latency does **not** need this export — the
session API's `/timeline` already answers it — so §9 now separates what works today from what
only a durable record can do, and records two API traps the experiment surfaced (§9.3).
§8.6 added.

---

## 1. Goal

1. Get a month of session events out of Cortex and into a data store, without a bespoke Go
   program per question.
2. Carry no prompt, completion, tool argument or tool result off the machine, by
   construction rather than by configuration.
3. Backfill the 30 days the archive already holds, not only tap traffic from the moment an
   export is switched on.
4. Make one projection the single source for per-event analysis, so a second exporter does
   not get built for #898.
5. Keep a record small enough that a month of traffic is a file a laptop can process.
6. Pin the schema with tests, so a field added to the event later cannot start leaving the
   machine silently.

**Not goals:**

- Replacing `agentop` or the session API. Those answer live questions about one session; this
  answers offline questions about many.
- Lossless capture or replay of request bodies. That is #1494, which shipped; see §7.3.
- Instrumenting Cortex's own pipeline with spans. That is #505, a different axis; see §8.2.
- Aggregating, rolling up or pricing anything inside the proxy. A record is an input to
  analysis, not a report.
- A query language, an index, or any storage engine. NDJSON and whatever reads it.

---

## 2. What exists today

### 2.1 The projection and the writer — shipped

`core/sessionexport` (#1812) holds `Record`, `FromEvent` and an NDJSON `Writer`.

**What it covers**

- 36 scalar fields per event (§4), named one by one in `FromEvent`.
- NDJSON encoding, one record per line, with a `Count()` of records written.
- A privacy contract pinned by `TestRecord_CarriesNoContent`, which plants a secret in every
  content-bearing field of a fully-populated event and fails if any reaches the encoded
  bytes.
- A completeness contract pinned by `TestRecord_EveryFieldIsPopulated`, which reflects over
  the struct so a field added to `Record` but not to `FromEvent` fails.

**Limits**

1. No sink. `Writer` takes an `io.Writer`; nothing opens a file, rotates one, or sends
   anywhere. A caller has to exist.
2. No archive walk. Nothing reads `Page`/`Earlier` and feeds the writer, so the 30 days on
   disk are not reachable.
3. No configuration. No way to turn an export on, choose a destination, or scope it.
4. No live tap. Nothing subscribes to the event stream.
5. Tier 2 and Tier 3 (§7) are unimplemented; the record is Tier 1 only.

### 2.2 What else already reads session events

| Reader | Scope | Why it is not this |
|---|---|---|
| Session API (`core/sessionapi`) | one session, paged, live | Built for a TUI. `view=summary` drops payloads for the same reason this does, but it serves one session at a time over HTTP. |
| `/v1/sessions/{id}/timeline` | one session, as spans | **Already answers per-tool latency, content-free** — named tool spans with start, end and an argument preview (§9.1). Per-session and computed on demand, so it cannot aggregate across sessions or outlive the archive (§9.2). |
| `cortex-session-dump` (`docs/session-dump.md`) | sessions resident in the store | A snapshot tool: captures what the store still holds when it runs, and must be run before the thing you want is gone. Full-fidelity JSON, not content-free. |
| `archive.ReplaySince` | cost replay | **Excludes tunnel rows by design** — correct for cost (no tokens), wrong for an export: 10,493 of 29,606 events on the reference archive, including every non-LLM latency figure. |
| `archive.Page` / `Earlier` | any session, any depth | No filtering. This is what an export walk wants. |
| `core/cost/ledger` | priced aggregates on disk | Already an aggregate, already priced, and keyed for cost questions only. |

### 2.3 The gap

| Gap | Part that has the raw material | What this spec adds |
|---|---|---|
| A month of events in one file | the archive | a walk plus a sink (§5, §6) |
| Events off-box without prompts | `summarizeEvent`'s drop argument | the Tier 1 projection (§4) — shipped |
| Per-session unit of work | archive segment boundaries | `sessionIncarnation` (§4.7, **Open**) |
| Tool-result counts that do not overcount | nothing | #1740 (§4.7) |
| Aggregates at a central collector | the ledger, partially | #898 derives from this (§8.1) |

---

## 3. Requirements

Numbered globally. Plain declarative: a requirement describes what the system does when it
is built, not what an implementer ought to feel.

### 3.1 Needs

| Need | Who | Where |
|---|---|---|
| Where did an agent's wall-clock go | a developer tuning their own setup | offline, over a month |
| Which model/host/agent cost what | whoever pays | offline, over a month |
| Which tool calls fail, and how often | a plugin author | offline, per tool |
| How latency moved release over release | an operator | offline, over releases (#1466) |
| Fleet totals without per-request detail | a manager | a central collector (#898) |

R1. One record describes one `SessionEvent`.
R2. A record is a flat object of scalars. No nesting, no arrays, in Tier 1.
R3. An export covers events the archive holds, not only events seen after it was enabled.
R4. An export walk sees tunnel rows.
R5. A consumer can process a file one line at a time without holding it in memory.
R6. An interrupted writer leaves a file whose every complete line is still valid.

### 3.2 Privacy and safety

R7. A record carries no message content, no completion, no tool arguments, no tool results,
no MCP params or results, no A2A parts or artifacts, no plugin event payloads, no thinking
blocks, no authenticated identity and no TLS peer identity.
R8. A record carries no request path. Neither `SessionEvent.HTTPPath` nor
`Invocation.Path`, which is a copy of the same string.
R9. Every string a record carries is one of: an id the client already asserted, a value from
a closed vocabulary, or a wire-sourced label capped at 96 bytes with control runes removed.
R10. A field whose values come from a closed vocabulary is exported by naming the values, not
by sanitising whatever arrives. `agentRole` and the client name work this way.
R11. A client version reaches a record only when it matches a version grammar whose every
letter is spelled out in that grammar. Widening the grammar is done by adding a literal,
never by widening a character class.
R12. Tier 1 is the default and the only tier an export can emit without configuration.
R13. Tier 3 (content) is refused by an export path rather than offered as a flag.
R14. The privacy contract is held by a test that inspects the ENCODED BYTES of a record
projected from an event whose every content-bearing field holds a planted secret.
R15. A field added to `Record` without being populated in `FromEvent` fails a test.
R16. A field added to `SessionEvent` does not reach an export until someone adds it to
`Record` on purpose.

### 3.3 Data rules

R17. Zero means "not recorded", never a real zero, for every numeric field, and every
numeric field is `omitempty` to say so on the wire.
R18. A record carries no aggregate of fields it already carries. A consumer sums what it
needs.
R19. Token counts are exported as the disjoint split, never as a prompt or grand total.
R20. `reasoningTokens` is a subset of `outputTokens` and is documented as one at the field.
R21. A count derived from a slice that a projection may have dropped falls back to the
event's own count field for that slice.
R22. Durations are exported in milliseconds, truncated, as integers.
R23. A consumer orders a file by `at`, then `seq`. Neither alone is sufficient.
R24. A record states its schema version in every line.
R25. A nil event writes no line and is not counted.

### 3.4 Transport

R26. The default sink is a file of newline-delimited JSON.
R27. An export makes outgoing connections only. Nothing listens.
R28. A sink failure does not fail the request being recorded, and does not block the
pipeline.
R29. An export states how many records it wrote.
R30. **Open:** whether a live tap is in version one, or whether backfill-on-demand is enough
to start. See §6.3.

### 3.5 Schema stability

R31. `schema` is bumped when a field is removed, or when a field's meaning changes.
R32. Adding a field does not bump `schema`, because an absent key already reads as "not
present" (R17).
R33. A consumer reading a mixed file tells the shapes apart by `schema` alone.
R34. The field reference in §4 is the schema. A field not listed there is not in the export.

### 3.6 Documentation

R35. Every field's unit, vocabulary and absence meaning is stated where the field is
declared, not only in this document.
R36. A caveat that a consumer gets wrong is stated at the field it concerns. `client` being
spoofable is restated in the export even though the event says it, because an external
collector is where it will be mistaken for identity.
R37. A measurement quoted in this spec names the corpus it came from.

---

## 4. What a record collects

**Normative, and shipped.** `core/sessionexport.Record`, `schema: 1`. 36 fields.

Tables in this section: `Field` | `Type` | `Unit` | `Values` | `Meaning`. `Unit` is `1` for a
count, `ms` for a duration, `By` for bytes, `—` for an identifier or a label. A `Values` cell
names a closed vocabulary where one exists, a bound where the value comes off the wire, and
`any` where neither applies.

**Absence is a value.** Every numeric field is `omitempty`, and an absent key means "not
recorded" rather than zero (R17) — so a `Values` cell reading `≥0` still means a missing key
is not a zero.

### 4.1 What is NEVER collected

Stated first, because it is the point of the projection. None of these reaches a record, and
`TestRecord_CarriesNoContent` fails the build if one does (R7, R8, R14).

| Not collected | Where it lives on the event | Why not |
|---|---|---|
| Message content | `inference.messages[].content` | the prompt. 99.5% of an event and the whole privacy risk |
| Completion | `inference.completion` | the model's reply |
| Tool-call arguments | `inference.toolCalls[].arguments` | a `Bash` command line is effectively a secret |
| Tool results | `inference.toolResults[].content` | file contents, command output |
| Earlier tool calls | `inference.historyToolCalls` | re-sent whole on every request |
| Tool names | `inference.tools[]`, `toolCalls[].name` | low risk, but nothing in v1 needs them — see §7.2 |
| Thinking blocks | `inference.historyThinking`, `responseThinking` | reasoning text |
| MCP params and results | `mcp.params`, `mcp.result` | the MCP tool's name, arguments and output |
| A2A parts, artifact, error | `a2a.parts[].content`, `.artifact`, `.errorMessage` | agent-to-agent message bodies |
| Plugin event payloads | `plugins{}` | plugin-defined, so unbounded by construction |
| Authenticated identity | `identity.subject`, `.clientId`, `.agentId` | who the caller is. Tier 2 at the earliest (§7.2) |
| TLS peer identity | `tls.peerSPIFFEID` | connection-level identity |
| Request path | `httpPath`, `invocations[].path` | **a path segment can hold a bot token** (§4.7) |
| The User-Agent itself | `client.raw` | free text off a header; only the parsed name survives (§4.5) |

### 4.2 Identity

| Field | Type | Unit | Values | Meaning |
|---|---|---|---|---|
| `schema` | int | — | `1` | The record format. Always present. Bumped only on a removal or a meaning change (R31). |
| `sessionId` | string | — | any | The session the event belongs to, as the client asserted it. Always present. **Not a unit of work** — see §4.7. |
| `seq` | uint | — | ≥1 | Position in the session. Unique within one *incarnation*: it restarts at 1 when an id is reused. |
| `requestId` | string | — | any | Pairs a request record with its response record. Positional pairing misattributes whenever a client has concurrent requests, which a coding agent routinely does. |
| `at` | string | — | RFC 3339 | When the event was recorded. Always present. **Order by `at`, then `seq`** (R23). |

### 4.3 Shape of the exchange

| Field | Type | Unit | Values | Meaning |
|---|---|---|---|---|
| `direction` | string | — | `inbound`, `outbound` | Which way the request went. Closed by the type: `UnmarshalJSON` clamps an unrecognised value, so a decoded archive event cannot widen it. |
| `phase` | string | — | `request`, `response`, `denied` | Which row this is. `denied` is a terminal row for a request a plugin rejected. Closed the same way. |
| `host` | string | — | ≤96 B | Where the bytes went. |
| `requestedHost` | string | — | ≤96 B | What the client asked for, set **only** when a plugin redirected. `host` ≠ `requestedHost` is the only way a reader sees a redirect happened. |
| `httpMethod` | string | — | ≤96 B | The HTTP verb. |
| `tunnel` | bool | — | `true` when set | An opaque CONNECT or transparent-redirect row, whose bytes were never parsed. A third of the events on the reference archive. |
| `tunnelReason` | string | — | ≤96 B | Why the bytes were left opaque. `passthrough-host` is a configured passthrough working as intended; `dial-failed`, `program-refused` and `os-trust-only` each mean every plugin was blind to that traffic. |

### 4.4 Outcome and timing

| Field | Type | Unit | Values | Meaning |
|---|---|---|---|---|
| `statusCode` | int | — | ≥100 | The HTTP status. Absent on request rows, and on responses recorded before the status was known. |
| `errorKind` | string | — | ≤96 B | The error class, when the event carries one. |
| `errorCode` | string | — | ≤96 B | The error code. |
| `durationMs` | int | ms | ≥0 | The event's own measurement, truncated. **Never subtract timestamps to get this** — same-instant events are common on a fast local proxy, so pairing by `requestId` and subtracting `at` yields noise at the scale fast exchanges live at. |
| `ttfbMs` | int | ms | ≥0 | To response headers. `durationMs − ttfbMs` is how long the body took — for a streamed completion, the stream time. Absent when the exchange never reached headers, which is why a zero must not read as an instant answer. |
| `bytesUp` | int | By | ≥0 | The request body as forwarded. Absent, not zero, when no plugin wanted the body. |
| `bytesDown` | int | By | ≥0 | The response body as observed from the destination. |

**Sub-millisecond exchanges read 0** and are therefore indistinguishable from unmeasured.
Accepted deliberately: the alternative is a float, and a consumer summing floats over a month
accumulates error in a figure that is a wall-clock estimate to begin with (R22).

### 4.5 Attribution

| Field | Type | Unit | Values | Meaning |
|---|---|---|---|---|
| `client` | string | — | `<name>` or `<name>/<version>` | The coding agent. **Client-asserted and trivially spoofable** — a cost-attribution and display key, never an authorization subject (R36). |
| `agentRole` | string | — | `main`, `subagent` | Which caller under one session id made the request. Exported by naming the values (R10), because a decoded archive event carries whatever its JSON said. |

**`client` is a recognised name or nothing, and a version-shaped version or no version.** Both
halves come off the User-Agent and each needs its own guard (R9, R11):

- An **unrecognised** product token leaves the parsed name empty, and the label would otherwise
  fall back to the whole raw header.
- A **recognised** one copies the rest of the token into the version verbatim — `claude-cli` is
  a known client, so `claude-cli/private-data` parses to name `claude-code`, version
  `private-data`.

So a version reaches a record only when it matches a grammar whose every letter is spelled out
in that grammar: a numeric core of up to four parts, an optional pre-release from a closed list
of words, and the one observed build tag. A name with no version means the header's version was
not version-shaped. **Widening that grammar adds a literal, never a character class** — it was
wrong twice during review in exactly that way.

**`agentRole` empty means NOT STATED, not `main`.** Only Claude Code declares it today.

### 4.6 Inference

| Field | Type | Unit | Values | Meaning |
|---|---|---|---|---|
| `model` | string | — | ≤96 B | The model the request was sent for. Free text off the request body, capped here because no parser caps it. |
| `requestedModel` | string | — | ≤96 B | Set **only** when a plugin changed the model. |
| `servedModel` | string | — | ≤96 B | Set **only** when the response named a different model than was asked for. |
| `stream` | bool | — | `true` when set | What the request asked for. |
| `streamedResponse` | bool | — | `true` when set | What arrived. The two disagreeing is a real condition and is invisible from either alone — 295 requests on the reference archive asked to stream and did not. |
| `finishReason` | string | — | ≤96 B | The upstream's own stop reason. |

**Tokens — four disjoint kinds and one subset.** This distinction is the one to read before
summing anything (R19, R20):

| Field | Type | Unit | Values | Meaning |
|---|---|---|---|---|
| `inputTokens` | int | 1 | ≥0 | **Uncached** prompt tokens. What the ledger prices at the input rate. |
| `cacheReadTokens` | int | 1 | ≥0 | Prompt tokens served from cache. Disjoint from `inputTokens`. |
| `cacheWriteTokens` | int | 1 | ≥0 | Prompt tokens written to cache. Disjoint from both above. |
| `outputTokens` | int | 1 | ≥0 | Generated tokens. |
| `reasoningTokens` | int | 1 | ≥0 | **A SUBSET of `outputTokens`**, already counted there. `outputTokens + reasoningTokens` double-counts every reasoning token at the output rate. |
| `presentKinds` | uint8 | — | bitmask | Which kinds the provider actually reported: input 1, cacheRead 2, cacheWrite 4, output 8, reasoning 16. |

**No `promptTokens` or `completionTokens` (R18).** The event carries a legacy pair of those
names, but `inference.promptTokens` is already `input + cacheRead + cacheWrite`. Exporting it
beside `cacheReadTokens` ships a total next to its own components: a consumer pricing input on
one and cache on the other double-counts every cached token — 196,096 of them on a single
response of the reference archive.

**`presentKinds` is the one field that escapes the absence rule.** A set bit with a zero count
is "the provider reported zero"; an unset bit is "the provider does not expose this kind".
Without it both are the same absent key, and a consumer cannot tell a model that wrote no cache
from a provider that never says.

**Counts.** Each reads the slice when present and the event's own count field otherwise, because
a `view=summary` source carries the counts with the slices already dropped (R21):

| Field | Type | Unit | Values | Meaning |
|---|---|---|---|---|
| `messageCount` | int | 1 | ≥0 | How long the conversation was. |
| `toolCount` | int | 1 | ≥0 | The tool manifest the **request** offered. Separates an agentic turn from a one-shot utility completion, which matters because a coding agent interleaves both under one session id. |
| `toolCallCount` | int | 1 | ≥0 | How many calls the **response** made of it. |

**No tool-result count at all in v1.** Summing `len(toolResults)` over a session overcounts by
~272x on the reference archive, because every request re-sends the whole conversation — which
turns a 3.6% tool failure rate into an apparent 8 failures per call. The per-event counts that
fix this are #1740; until they exist, a record carries no tool-result figure rather than one a
consumer would reasonably misread.

### 4.7 Not in version 1, and why

| Field | Status | Reason |
|---|---|---|
| `sessionIncarnation` | **Open** | The one genuinely new field #1810 asked for, and the one that makes `sessionId` a unit of work. The id `default` recurred across the whole 2026-10-15→2026-11-06 range of the reference archive, in one directory; one session has 34 segments across three weeks. Derivable from archive segment boundaries, but needs the key #1739 raised. Until then a consumer wanting "one sitting" infers it from gaps in `at`. |
| `httpPath` | **Refused by default** | Its own doc: "a secret embedded in a path SEGMENT does survive, because nothing can tell it from a resource id — a bot token or a webhook path lands here verbatim", and it names an off-box export as the case to know that before. Query strings are already stripped; segments are not. **Open:** redact, opt-out, or never. R8 holds until this is settled. |
| `project` | **Open** | `SessionEvent.Project` landed in #1815, after #1810 was written: the git-repo-root label of the agent process, and the natural key for per-project cost and latency. It is `httpPath`'s question in a different field — a repo path under `$HOME` names directories a user may not want off-box. |
| `newToolResults` | Blocked on #1740 | Item 1 there. The fix for the 272x overcount above. |
| `toolResultErrors` | Blocked on #1740 | Item 2 there. |
| `toolCallNames` | Tier 2 | §7.2. Names are low risk; arguments are not. |
| `invocations` | Tier 2 | §7.2. Per-plugin outcomes, minus `path` and `details`. |
| `identity` | Tier 2 | §7.2. The authenticated principal, as distinct from `client`. |
## 5. Transport and sinks

### 5.1 NDJSON — shipped

Newline-delimited JSON: one record per line, no enclosing array.

Chosen because the consumer is a pipeline and the producer is a walk (R5, R6). A reader
processes a month one line at a time; every line-oriented tool already works on it; an
interrupted writer leaves a file whose every complete line parses, where a truncated array is
unparseable in its entirety. The session archive makes the same bet for the same reason.

The one-line property is structural, not a consequence of the field vocabulary:
`encoding/json` escapes newlines and every other control character inside strings, so a field
added later that holds prose cannot break it.

### 5.2 File sink

R38. The default sink writes to a path the operator names.
R39. **Open:** rotation. A month of the reference corpus is ~12.5 MB for 29,606 records, so
size-based rotation may be unnecessary in version one. Decide before shipping a long-running
tap.
R40. A sink reports the number of records written (R29).

### 5.3 HTTP sink

R41. An HTTP sink POSTs NDJSON to an endpoint the operator names, outgoing only (R27).
R42. A failed POST does not fail or block the request being recorded (R28).
R43. **Open:** batching, retry and back-pressure policy. A dropped batch must be *counted*
rather than silently lost.

### 5.4 Not OTLP, yet

OTLP is deliberately a follow-up. The metrics subset maps cleanly onto the GenAI semantic
conventions — `gen_ai.*` already standardises model, token counts and finish reason — and
belongs with #505. Bundling a wire format, a transport and a semantic-convention mapping into
one change makes it unreviewable.

---

## 6. Archive replay and backfill

### 6.1 Why backfill is most of the value

The archive already holds 30 days. An export that only taps live traffic delivers nothing on
the day it is switched on, and the questions in §3.1 are all retrospective.

### 6.2 The walk

R44. A backfill walks `archive.Page`/`Earlier`, not `ReplaySince`.
R45. A backfill accepts a time range and a set of session ids, both optional.
R46. A backfill is restartable and states what it covered.

`ReplaySince`/`ReplaySources` exclude tunnel rows by design — correct for cost replay, since a
tunnel carries no tokens. For an export it is wrong: 10,493 of 29,606 events on the reference
archive are tunnel rows, a third of the corpus, and they hold every non-LLM latency the proxy
has. `Page`/`Earlier` do not filter (R4).

R47. A record projected from an archive event is byte-identical to one projected from the same
event live. One projection, one schema, both paths.

### 6.3 Live tap

**Open** (R30). A tap is more code and more failure modes than a walk, and the archive is
already the durable copy on a laptop install. One option for version one: backfill only, run
on demand or on a timer, with the archive as the buffer.

---

## 7. Tiers

### 7.1 Tier 1 — the core record

§4. Always exported, content-free by construction, no configuration (R12).

### 7.2 Tier 2 — opt-in structured detail

Not implemented. Each item is a deliberate narrowing of something Tier 1 refuses:

| Item | Shape | Why it is separable from content |
|---|---|---|
| `toolCallNames` | `[]string` | The tool *names* a response asked for. Names are low risk; **arguments are not** — a `Bash` command line is effectively a secret and stays in Tier 3. Names alone yield per-tool latency (§9). |
| `invocations` | `[{plugin, action, reason}]` | Per-plugin outcomes. Deliberately **not** `path` (R8) and not `details`, which is free-form. |
| `identity` | `{subject, clientId, agentId}` | The *authenticated* principal, as distinct from `client`. Answers "who", where `client` answers "what program". |

R48. Tier 2 breaks R2 (flat scalars) and therefore needs its own schema decision: either a
`schema` bump, or arrays that a consumer may ignore. **Open.**
R49. Tier 2 is off unless configured, and configuring it is a per-destination decision.

### 7.3 Tier 3 — content

Messages, completions, tool results, MCP params. **An export path refuses these** (R13)
rather than offering a flag someone flips under time pressure. The full-fidelity case is
#1494, which shipped as lossless capture in the archive; that is replay for testing a change,
not shipping prompts to a collector.

---

## 8. Relationship to adjacent work

### 8.1 #898 — report statistics to a central collector

#898 wants *aggregates* — token count by hour — at a remote endpoint. This spec is the
per-event layer those aggregates can be derived from.

R50. #898 derives from this projection rather than reading `SessionEvent` itself, so one
field list and one privacy contract serve both.
R51. Aggregation happens outside the proxy, or in the collector. Not in `sessionexport`.

**Open:** whether #898 consumes NDJSON from a sink, or whether both share only the `Record`
type and #898 ships its own transport. Worth settling before either is built, which is the
whole reason this section exists.

### 8.2 #505 — first-class OpenTelemetry for the proxy

Different axis. #505 instruments *Cortex's own behaviour* — its pipeline, its plugins, its
spans. This exports *the traffic Cortex observed*. A record is not a span: it has no parent,
no trace id and no causal relationship to another record beyond `requestId`.

### 8.3 #1740 — three archive fields

Adds `newToolResults`, `toolResultErrors` and the interleaved-completion marker. This export
is the main consumer for all three (§4.7), and the 272x overcount is the reason they matter.

### 8.4 #1466 — long-term inference performance

Notes that the archive's raw material exists but is never aggregated beyond six hours. An
export is one way to get that aggregation outside the proxy, using `ttfbMs` and `durationMs`
across releases.

### 8.5 #1494 — lossless capture

Shipped. The Tier 3 / full-fidelity case, for replaying past traffic against a change. Not a
transport for prompts.

### 8.6 The Context Guru metrics spec — one schema, two emitters

[`context-guru/docs/metrics/spec.md`](https://github.com/rossoctl/context-guru/blob/docs/prometheus-metrics-spec/docs/metrics/spec.md)
specifies 32 Prometheus metrics pushed to a central Collector over OTLP. Read it before
building §5.3 or #898.

**IT ALREADY ASSUMES CORTEX EMITS IT.** Its §5.6 defines a collector-added `proxy` label
whose values are `cortex` and `context-guru`, and prefixes the shared set `llm_`
deliberately "so both proxies can emit it". So these are not competing designs to choose
between, and not two teams' positions to reconcile: one spec is the aggregate layer, this
one is the per-event layer underneath it, and the only wrong move is a third thing that
agrees with neither.

Both are the same community's work on the same problem from opposite ends, so this section
reads Context Guru's choices as defaults to adopt unless Cortex has a reason, rather than as
an outside design to compare against.

R55. Where a figure appears in both specs, it carries the same meaning and the same
decomposition in both. A disagreement between them is a defect in one of them, not a local
choice.

#### Summary: what each layer is for

| | Cortex (this spec) | Context Guru |
|---|---|---|
| Unit of record | **one event** — a request or response row | **one metric sample** — a counter or histogram |
| Granularity | per request, every request | per label set, summed over an interval |
| Scaling limit | file size (~430 B/record) | **active series** — ceiling ~2,000/machine |
| Retention | 30 days or 2 GiB (archive); the export file is yours | whatever Prometheus keeps |
| Transport | NDJSON to file; HTTP sink planned (§5.3) | OTLP push to a Collector over HTTPS |
| Can you ask a NEW question of OLD data? | **yes** — re-query the records | **no** — if it was not a label, it is not recoverable |
| Can you get a cheap fleet total? | no — needs a pass over records | **yes** — that is the design |
| Cost and money | **none** — pricing is the ledger's job | priced, by tier, at request time |
| Privacy mechanism | engineered and tested (R7, R14) | structural — a metric has no body |

The division is not arbitrary, and neither layer can absorb the other. A per-event record
cannot become a Prometheus series without unbounded cardinality (`sessionId`, `requestId`,
`at`); a counter cannot answer a question nobody thought to label for. One keeps the grain,
the other keeps the roll-up.

Grouped comparisons follow: the metric inventory, then where the two agree, what Cortex
should adopt, and what this means for #898.

#### Metric by metric: Context Guru's 32, against what a record carries

Context Guru's 32 metrics, in its own section order. **Cortex source** names the field or
endpoint that holds
the same figure, `—` where nothing does. **Derivable** says whether a consumer can compute
the metric from a month of records without the proxy's help.

Read the `—` rows as the work list: each is either a producer change (steps 10–12 of §10) or
a deliberate non-goal.

##### Context Guru 5.1 — Inference (the common set)

| Context Guru metric | Type | Cortex source | Derivable from records | Note |
|---|---|---|---|---|
| `llm_inference_requests_total` | counter | one record per event | **yes** — count rows where `phase=request` | the Context Guru counter is requests; a record is a row per phase, so count one phase |
| `llm_inference_errors_total` | counter | `errorKind`, `errorCode`, `statusCode` | **yes** | Context Guru's `source` (`upstream`/`proxy`/`client`) has no Cortex equivalent — `errorKind` is a different taxonomy |
| `llm_inference_request_bytes_total` | counter | `bytesUp` | **partly** | one measurement point only. Context Guru's `stage` splits `client` vs `forwarded`; a record cannot say what a plugin removed (§10 step 10) |
| `llm_inference_response_bytes_total` | counter | `bytesDown` | **partly** | same: Context Guru's `upstream` vs `delivered`. `bytesDown` is pre-rewrite |
| `llm_inference_duration_seconds` | histogram | `durationMs` | **yes** — percentiles from raw values | a record gives exact values, so any quantile; a histogram gives only its buckets |
| `llm_inference_ttfb_seconds` | histogram | `ttfbMs` | **yes** | absent rather than 0 when headers never arrived (§4.4) |
| `llm_inference_added_seconds` | histogram | **—** | **no** | the proxy's own contribution. Needs a second timestamp the event does not carry (§10 step 12) |
| `llm_inference_tokens_total` | counter | `inputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `outputTokens` | **yes** | Context Guru's `tier` splits cache-write by TTL; Cortex folds the two (§10 step 11) |
| `llm_inference_cost_usd_total` | counter | ledger `*CostMicros`, not the record | **no** | a record carries **no money** — deliberate: pricing is the ledger's job, and a rate table changes under a record that would then be wrong |
| `llm_inference_estimated_cost_usd_total` | counter | ledger `PriceableRequests` / `IncompleteRequests` | **no** | same |

##### Context Guru 5.2 — Savings, from the accountant

| Context Guru metric | Type | Cortex source | Derivable | Note |
|---|---|---|---|---|
| `llm_savings_saved_tokens_total` | counter | **—** | **no** | needs a counterfactual: the request as the client authored it, which no event holds |
| `llm_savings_added_tokens_total` | counter | **—** | **no** | same |
| `llm_savings_saved_usd_total` | counter | `avoidedMicros` (ledger, one scalar) | **no** | the Context Guru figure is decomposed by `kind`×`tier`×`component`; Cortex has the total only |
| `llm_savings_added_usd_total` | counter | **—** | **no** | Cortex counts cost avoided, never cost a plugin *added* |
| `llm_savings_cache_effect_usd_total` | counter | **—** | **no** | `break`/`rescue`/`split`/`ttl_premium`/`ttl_rescue` are unmodelled here |
| `llm_savings_baseline_usd_total` | counter | **—** | **no** | the counterfactual bill |
| `llm_savings_requests_total` | counter | ledger `PricedRequests` | **no** | partial: `priced` yes, `unpriced`/`skipped` not split |
| `llm_savings_unpriced_total` | counter | **—** | **no** | |
| `llm_savings_estimated_total` | counter | ledger `IncompleteRequests` | **no** | roughly analogous, differently defined |
| `llm_savings_check_failures_total` | counter | **—** | **no** | the seven Context Guru library invariants have no Cortex counterpart |
| `llm_savings_fallback_total` | counter | **—** | **no** | |

**This whole block stays with Context Guru** (§8.6 above). 8 of 11 have no Cortex source and none is
derivable from records, because every one needs a counterfactual — what the request *would*
have cost unmodified — which a record of what actually happened cannot contain. Adding it
would mean reimplementing the Context Guru accountant.

##### Context Guru 5.3 — Keep-alive

| Context Guru metric | Type | Cortex source | Derivable | Note |
|---|---|---|---|---|
| `llm_keepalive_pings_total` | counter | **—** | **no** | Cortex does not ping |
| `llm_keepalive_cost_usd_total` | counter | **—** | **no** | |
| `llm_keepalive_rescue_usd_total` | counter | **—** | **no** | |
| `llm_keepalive_shared_warm_tokens_total` | counter | **—** | **no** | |

Not a gap: the behaviour does not exist in Cortex. If it ever does, these are the metric
names to use (R56).

##### Context Guru 5.4 — Cache health

| Context Guru metric | Type | Cortex source | Derivable | Note |
|---|---|---|---|---|
| `llm_cache_misses_total` | counter | **—** | **partly** | a miss is inferable from `cacheReadTokens == 0` with a warm prior request, but the seven Context Guru `cause` values (`ttl_expiry`, `prefix_change`, `model_changed`, …) are not: cause needs the prefix comparison the proxy does and the record drops |
| `llm_cache_miss_cost_usd_total` | counter | **—** | **no** | money again |

##### Context Guru 5.5 — Proxy health

| Context Guru metric | Type | Cortex source | Derivable | Note |
|---|---|---|---|---|
| `llm_proxy_info` | gauge | `GET /v1/pipeline`, `/config` | **no** | Cortex exposes build and config over HTTP, not as a metric. No `/metrics` endpoint exists at all |
| `llm_proxy_component_duration_seconds` | histogram | `invocations[]` on the event (per plugin, **not** in the record) | **no** | the event carries per-plugin invocations; the record deliberately drops them (Tier 2, §7.2). Per-component timing is not recorded anywhere in Cortex |
| `llm_proxy_component_failures_total` | counter | `invocations[].action`, `.reason` | **no** | same: on the event, not the record |
| `llm_proxy_fail_open_total` | counter | **—** | **no** | not counted |
| `llm_proxy_export_failures_total` | counter | **—** | **no** | R29/R43 require a record count and a dropped-batch count; this is the metric form of it |

##### Context Guru label vocabularies, against Cortex fields

Where the same dimension exists on both sides, it must mean the same thing (R55).

| Context Guru label | Values | Cortex field | Agreement |
|---|---|---|---|
| `model` | ~10 per machine | `model` | **same**, but Cortex also has `requestedModel` and `servedModel`, which they fold into one post-change value |
| `provider` | `anthropic`, `openai`, `bedrock`, … | `host` | **differ**: Cortex records the endpoint host, not a provider taxonomy. A mapping is needed before either can group by the other |
| `status_class` | `2xx`…`5xx` | `statusCode` | **compatible** — Cortex is exact, Context Guru bucketed; exact → bucket is lossless, the reverse is not |
| `stream` | `true`, `false` | `stream` **and** `streamedResponse` | **Cortex is richer**: asked-for vs arrived. 295 requests on the reference archive asked and did not get it |
| `source` | `upstream`, `proxy`, `client` | — | **no equivalent**; `errorKind` is a different axis |
| `tier` | `input`, `cache_read`, `cache_write_5m`, `cache_write_1h`, `output` | the four split token fields | **nearly**: Cortex folds the two cache-write TTLs (§10 step 11). Cortex adds `reasoningTokens`, which has no tier |
| `stage` | `client`/`forwarded`, `upstream`/`delivered` | — | **no equivalent** (§10 step 10) |
| `component` | ~30 operator names | `invocations[].plugin` on the event | exists on the event, **not** in the record |
| `kind`, `effect`, `cause`, `direction`, `result`, `reason`, `check` | savings vocabularies | — | Context Guru only |
| `outcome` | keep-alive | — | Context Guru only |
| `proxy` | `cortex`, `context-guru` | — | **collector-added**, and the reason this section exists |
| `machine` | one per install | — | collector-added from `service.instance.id` |

##### What a record carries that no metric does

The other direction, and the reason both layers exist. None of these can be a Prometheus
series: the first group is unbounded cardinality, the second is per-request facts a counter
cannot hold.

| Cortex field | Why it has no metric form |
|---|---|
| `sessionId`, `seq`, `requestId` | identifiers — unbounded by definition |
| `at` | per-event timestamp; a metric has a scrape time instead |
| `presentKinds` | "reported zero" vs "not exposed". A counter at 0 cannot distinguish them (open question 12) |
| `requestedHost` | that a redirect happened, per request |
| `requestedModel`, `servedModel` | asked-for vs served, per request |
| `streamedResponse` | the second half of the stream question |
| `messageCount`, `toolCount`, `toolCallCount` | conversation shape — agent-shaped, not proxy-shaped |
| `agentRole` | which caller under one session id |
| `tunnel`, `tunnelReason` | **a third of all events**, and every non-LLM latency the proxy holds. No metric in the Context Guru set covers non-inference traffic |
| `project` (§4.7, Open) | per-repo cardinality as a label |
| tool spans (`/timeline`, §9.1) | per-tool latency and the command that ran |

##### The arithmetic

| | Theirs | This spec |
|---|---|---|
| Metrics / fields | 32 metrics (27 counters, 1 gauge, 4 histograms) | 36 record fields |
| Of Context Guru's, sourced in a Cortex **record** | **7 of 32** (22%) | — |
| Of Context Guru's, sourced anywhere in Cortex (record, ledger or API) | **15 of 32** (47%) | — |
| Of Context Guru's, derivable from a month of records | **5 fully, 3 partly** | — |
| Of Context Guru's with no Cortex source at all | **17 of 32** — 11 savings, 4 keep-alive, 2 cache-cause | — |
| Cortex fields with no metric counterpart | — | **11 of 36** |

The 17 unsourced are not a backlog. 15 of them (savings, keep-alive) describe behaviour
Cortex does not have, and money a record deliberately does not carry. The real work list is
the three `—` rows in 5.1 (`added_seconds`, and the two `stage` splits) plus the cache-write
TTL fold — which is exactly §10 steps 10–12.

#### Where Cortex and Context Guru agree, independently

Both specs reached these conclusions separately, which is the strongest evidence each is
right:

- **Export the disjoint token split, never a prompt or grand total.** The Context Guru `tier` label
  (R34 there) and R19 here are the same rule, arrived at for the same reason.
- **Never net a saving against a cost.** Context Guru's "Do not net them inside the change" and R18
  here both refuse a figure a consumer can compute but would misread.
- **Bucket or bound anything client-supplied.** They bucket status into `status_class` and
  cap `component` by the shipped operator list; R9 caps wire strings at 96 bytes.
- **No prompt text, ever.** Theirs comes free from the format; R7 here has to be engineered,
  which is why `TestRecord_CarriesNoContent` exists.

#### What Context Guru has that Cortex should adopt

| Context Guru idea | What this spec has | Worth adopting? |
|---|---|---|
| **`stage` label** — request bytes at `client` *and* `forwarded`; response at `upstream` *and* `delivered` | one point per side: `bytesUp` is post-rewrite, `bytesDown` is pre-rewrite | **Yes.** Two points make "what the plugin changed" a subtraction instead of an inference. Today a plugin's byte effect is invisible in a record. **Open:** needs a second measurement point in the listener, so it is not a record-only change. |
| **`cache_write_5m` vs `cache_write_1h`** | one `cacheWriteTokens` | **Yes** — providers price the two TTLs differently, so folding them loses a real cost distinction. Needs the parser to carry it first. |
| **`llm_inference_added_seconds`** — latency the proxy itself contributed | `durationMs` and `ttfbMs` are both end-to-end | **Probably.** It is the figure an operator needs to know whether Cortex is the problem, and nothing here answers it. |
| Savings decomposed by `component`, `effect`, `cause` (11 metrics) | `avoidedMicros`, one scalar | **Not here.** That is Context Guru's domain and far deeper; a record should carry the per-event input, not reimplement the model. |

#### Consequences for §8.1 (#898)

The Context Guru spec answers most of what #898 asks for, which narrows this spec's open
question. #898
should **not** ship its own exporter: either it emits that metric set from Cortex directly,
or it derives it from these records. Either way the metric names, labels and decompositions
come from the Context Guru spec, not from a second invention here.

R56. #898 adopts context-guru's metric names, label vocabularies and tier decomposition
rather than defining its own. A figure Cortex cannot produce is absent, never renamed.

**Open:** which of the two — emit directly, or derive from records? Emitting directly is
less machinery and gets fleet totals sooner; deriving keeps one measurement path and means a
metric can be backfilled from the archive, which a counter never can. This is the same
question §8.1 raises, now with the answer constrained on one side.

**Open:** Context Guru's §5.7 budgets ~1,700 series per machine against a 2,000 ceiling, and notes
that is "a real limit and not a safe margin". Any Cortex-specific label added to that set
spends from a nearly-full budget — so a Cortex dimension that is interesting per event
(`agentRole`, `project`, `tunnelReason`) is not automatically affordable as a label. Decide
per label, with Context Guru's R42 test as the arbiter.

---

## 9. What this unlocks, and what already works without it

### 9.1 What the session API answers TODAY

Corrected after measuring it. An earlier draft of this section claimed per-tool latency needed
Tier 2's `toolCallNames`; it does not. `GET /v1/sessions/{id}/timeline` already pairs each tool
call with the request carrying its result and returns named spans, content-free:

```json
{"agent":"main","name":"Bash","start":"2026-10-21T20:50:36.459496-04:00",
 "end":"2026-10-21T20:50:52.869588-04:00","preview":"pwd; ls -la","seq":8}
```

Measured on one 2,911-event session of the reference archive:

| Tool | n | p50 | p95 | total |
|---|---|---|---|---|
| `Bash` | 452 | 487 ms | 65.0 s | 6,260 s |
| `AskUserQuestion` | 7 | 37.4 s | 409.5 s | 703 s |
| `WebFetch` | 34 | 14.4 s | 26.6 s | 440 s |
| `Edit` | 87 | 48 ms | 269 ms | 5 s |
| `Read` | 5 | 24 ms | 57 ms | 0 s |

`preview` is built from a fixed list of tool-ARGUMENT keys (`command`, `file_path`, `pattern`,
`url`, `query`, `skill`, `description`, `path`, `to`), capped at 80 characters, and never reads
message content — so a Bash command line is available without a prompt. The untruncated command
is on the response event at `inference.toolCalls[].arguments.command`, which needs the full
event view.

**So the export is not what makes per-tool timing possible.** Stating that plainly here because
the opposite claim was already repeated outside this document, and it sends people to
`cortex-session-dump --full` — which carries prompts — for a question `/timeline` answers
without them.

### 9.2 What the session API does NOT answer, and the export does

The timeline is per-session, computed on demand, and capped at 50,000 events per session. What
it cannot do is the whole reason for §5 and §6:

| Question | Why the timeline cannot | What the export gives |
|---|---|---|
| Per-tool latency across 82 sessions | one session per request; one compute each | one file, one pass |
| How p50 moved release over release (#1466) | no durable aggregate; archive ages out at 30 days | records outlive the archive |
| Fleet totals (#898) | per-session HTTP, not shippable | a sink (§5.3) |
| Join tool spans to token counts | **spans and tokens are in different responses** (§9.3) | one record carries both |
| Anything after the archive's retention | 30 days or 2 GiB, whichever first | the file is yours |

Measured cost of doing it the timeline way: 82 timeline requests, ~1 s each for a session only
on disk, against ~10 s for a single `jq` pass over a 29,606-record NDJSON file.

### 9.3 Two API facts a consumer has to know, and why they argue for a record

Found by writing the queries rather than by reading the code, which is the point:

1. **`turns[].seq` is the REQUEST seq; `tools[].seq` is the RESPONSE seq.** Same field name,
   same type, different referent (`timeline.go:88` vs `:105`; set at `agents.go:176` and
   `:197`). Both are undocumented. Joining turn spans to token counts on `seq` therefore
   matches nothing and returns an empty table — no error, just no rows.
2. **The two endpoints have different ceilings.** `/timeline` reads up to 50,000 events;
   `/v1/sessions/{id}` caps `?limit` at 2,000. On a 2,911-event session the events call
   returns only the newest 2,000, so 134 of 592 tool spans have no token row unless the
   consumer pages with `?before=`.

Both are fixable in the API and should be fixed (tracked separately). Both also make the same
argument for this spec: a record that carries timing and tokens on one line has no join to get
wrong and no two ceilings to reconcile.

### 9.4 Where the wall-clock went

The motivating question, with no agent-harness access. Classifying each `turn end → next turn
start` gap by whether a tool span overlapped it, on the same session as §9.1:

| | n | p50 | total | share |
|---|---|---|---|---|
| Tool time (a tool ran in the gap) | 558 | 248 ms | 2.0 h | 18% |
| User wait (no tool in the gap) | 69 | 122.9 s | 8.8 h | 82% |

The shape reproduces the earlier whole-archive figure (18.8% / 72.6%) closely enough to trust
the method, and `AskUserQuestion` landing three orders of magnitude above the machine tools is
the validation: the classifier never sees tool names, yet separates the one human-blocking tool
from the rest.

R52. A consumer publishing a user-wait total publishes its gap ceiling with it. The figure is
very sensitive to it: 24.8 h at 300 s, 44.8 h at 1800 s, 56.8 h at 3600 s, 539 h uncapped.
R54. A consumer joining timing to tokens reads §9.3 first, or pages both sides to the same
depth.

---

## 10. Delivery plan

1. **Projection and writer.** `Record`, `FromEvent`, NDJSON `Writer`, contract tests. —
   *shipped, #1812.*
2. **Archive walk.** `Page`/`Earlier` backfill over a time range, behind a command. R44–R47.
3. **File sink.** Path, record count, the rotation decision. R38–R40.
4. **Settle `httpPath` and `project`.** §4.7. Both are R8 questions and both block a
   per-project or per-endpoint analysis.
5. **#1740 fields** land, and the record gains the two counts. §8.3.
6. **`sessionIncarnation`**, once #1739's key exists. §4.7.
7. **HTTP sink.** R41–R43, with the batching decision.
8. **Decide #898's relationship** before building its transport, now constrained on one side
   by §8.6: it adopts context-guru's metric names and labels either way. §8.1, R56.
9. **Tier 2**, with its schema decision. §7.2.

Adopted from §8.6, each needing a producer change before the record can carry it, so neither
blocks version one:

10. **A second byte measurement point** (Context Guru's `stage`), so a plugin's effect on request and
    response size is a subtraction rather than an inference. Listener change first.
11. **Split `cacheWriteTokens` by TTL** (Context Guru's `cache_write_5m` / `cache_write_1h`), since
    providers price the two differently. Parser change first.
12. **Proxy-added latency** (Context Guru's `llm_inference_added_seconds`), the figure that says
    whether Cortex itself is the delay. Nothing here answers it today.

Version one is steps 1–3: a content-free record, out of the archive, into a file. That alone
answers §3.1's first four needs.

---

## 11. Test plan

| Test | What it checks | Requirement |
|---|---|---|
| `TestRecord_CarriesNoContent` | a secret planted in every content-bearing field reaches no encoded record | R7, R8, R14 |
| `TestRecord_EveryFieldIsPopulated` | reflects over the struct; a field `FromEvent` does not set fails | R15 |
| `TestFromEvent_ClientCarriesNoHeaderContent` | drives the real `ParseUserAgent`; a secret in the product token, the version, the build tag or a trailing comment reaches no record | R9, R11 |
| `TestClientLabel_VersionShape` | the keep/drop boundary of the version grammar, by case | R11 |
| `TestFromEvent_AgentRoleIsAClosedVocabulary` | drives `json.Unmarshal`, since a decoded archive event is the threat model | R10 |
| `TestRecord_WireStringsAreBounded` | every wire-sourced string capped and control-runes stripped | R9 |
| `TestRecord_TokensAreTheSplitNeverAnAggregate` | runs the real `TokenUsage.Fill`; the cached count appears exactly once | R18, R19 |
| `TestRecord_PresentKindsSurvives` | reported-zero stays distinguishable from not-exposed | R17 |
| `TestRecord_AbsentNumbersAreOmitted` | no unmeasured figure renders as 0 | R17 |
| `TestFromEvent_CountsFallBackToTheEventsOwn` | a `view=summary` source still reports counts | R21 |
| `TestWriter_SkipsNilEvents` | a nil event writes nothing and is not counted | R25 |
| `TestWriter_IsOneLinePerRecord` | one line per record | R5, R6 |
| `TestRecord_DurationsAreMilliseconds` | the unit, since field names are the only place it is stated | R22 |
| *to write:* archive-vs-live projection | a record from an archive event is byte-identical to one from the same event live | R47 |
| *to write:* backfill sees tunnel rows | a walk does not inherit `ReplaySince`'s filter | R4, R44 |
| *to write:* sink failure is not fatal | a failing sink does not fail the recorded request | R28 |

R53. A negative control is run for every safety requirement: the fix is reverted, the test is
confirmed to fail, and the fix is restored. Three of the four defects found in #1812's review
were cases where a test passed for a reason unrelated to what it claimed to check.

---

## 12. Open questions

1. `httpPath`: redact, opt-out, or never ship? (§4.7, R8) — blocks per-endpoint analysis.
2. `project`: is a git-repo path under `$HOME` content? (§4.7) — blocks per-project cost.
3. `sessionIncarnation`: derive in the archive, or in the export walk? (§4.7, #1739)
4. Live tap in version one, or backfill only? (R30, §6.3)
5. Does #898 emit context-guru's metric set directly, or derive it from these records?
   (§8.1, §8.6, R50, R56) — emitting is less machinery and sooner; deriving keeps one
   measurement path and can backfill from the archive, which a counter never can.
6. Tier 2 and R2: `schema` bump, or ignorable arrays? (R48)
7. File rotation in version one? (R39)
8. HTTP sink batching, retry and the counting of dropped batches? (R43)
9. Is `presentKinds` as a bitmask the right wire shape for a non-Go consumer, or should it be
   an array of kind names? (§4.6)
10. Should `schema` be a string (`"1"`) rather than an int, to leave room for a date-based or
    semver scheme later? (R24, R31)
11. Which Cortex-only dimensions are affordable as metric labels, given context-guru's
    ~1,700-of-2,000 series budget? (§8.6) — `agentRole`, `project` and `tunnelReason` are all
    interesting per event and not automatically affordable per series.
12. Should `presentKinds`'s problem — "reported zero" versus "not exposed" — be raised on
    context-guru's spec? A counter at 0 cannot express it either. (§8.6, §4.6)

---

## 13. Technical notes

- **The corpus.** Every measurement here comes from one developer's laptop archive: 66
  sessions, 29,606 events, 2026-10-15→2026-11-06, heavily Claude Code (18,785 events carry
  `claude-code/2.1.270`). The **field list generalises** — it follows the event schema, not
  this traffic. The **ratios do not**: #1494 measured 59% of replies batching tool calls under
  explicit prompting against 7.3% here unprompted, so those move with working style (R37).
- **Sizing.** ~30 scalars per event; 29,606 records in 12.5 MB; a full 30-day pass in `jq`
  and Python took ~10 s single-threaded. The same corpus at full fidelity is multi-gigabyte
  (`bytesUp` alone totals 6.08 GB).
- **The 203x number.** `summarizeEvent` measured dropping payloads at 203x on real traffic,
  because 99.5% of an inference event is its `inference` field. That measurement is why this
  is a projection and not a filter over whole events.
- **The failure mode to copy, and the one to avoid.** `summarizeEvent`'s drop rule fails
  *silently* — "a search for completion text simply stops matching". An export inverts that
  risk: a content field added to the event later would start *leaving the machine*. Hence the
  allowlist (R16), which is the opposite choice from `usage.ReplayCopy`, where keeping new
  fields by default is right for an in-process consumer.
- **Widening the version grammar** (R11) is done by adding a literal. The grammar was wrong
  twice during #1812's review in the same way — a leading-digit charset, then a free character
  class after `+` — because a charset cannot tell a version from the text beside it. Only the
  vocabulary can.
- **The limit of a shape rule.** A purely numeric version core still admits
  `4111.1111.1111.1111`. A version *is* digits, so no grammar separates those. Read R11's
  promise as "nothing but digits and the listed words".

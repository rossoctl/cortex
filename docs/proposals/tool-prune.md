# Directional Body Capabilities and the `tool-prune` Plugin

**Status**: Implemented — kept as the design record, not a live proposal.
Shipped as the `tool-prune` plugin (`core/plugins/toolprune`) plus
the directional body capabilities in `core/pipeline`. Read
[`tool-prune-plugin.md`](../../docs/tool-prune-plugin.md) for current
behaviour.
**Date**: September 2026

This document specifies three changes that together let AuthBridge cut an agent's
token bill by removing tool definitions the agent never calls, and show the
operator what that saved:

1. **A directional split of the body-write capability.** `PluginCapabilities.WritesBody`
   is renamed to `WritesRequestBody` and joined by `WritesResponseBody`. Response
   streaming is then gated on the response-side flag alone, so a plugin that only
   rewrites requests no longer forfeits incremental server-sent events (SSE).
2. **`tool-prune`**, an outbound plugin that deletes named entries from the `tools`
   array of an inference request. The list is static, produced at setup time by a
   new `abctl tools scan` subcommand that analyses local Claude Code transcripts.
3. **A plugin metrics channel**, surfaced in the existing `abctl` plugin detail
   pane. Claude Code's `/cost` reports a session total, which is too coarse to
   attribute a saving to the plugin, so the plugin reports its own counters.

Part 1 is a prerequisite for part 2 but stands on its own merits: it is a
framework correctness fix that any future request-only mutator benefits from.
Part 3 is likewise generic — any plugin gains a display channel.

## Motivation

Claude Code sends the full tool manifest on every request. On the author's machine
that manifest is roughly 20,500 tokens, and it sits at the front of the cached
prompt prefix, so it is billed on every turn of every session. Measurements across
seven developers show most of those definitions are never called even once in a
30-day window.

Removing the dead entries is a pure win: fewer prompt tokens, no behaviour change
for tools the agent actually uses. Doing it in the proxy rather than in each
client's configuration means it works for every agent behind AuthBridge without
per-client setup, and it is measurable centrally.

### Why the list is static

The `tools` array is at the front of the cached prompt prefix. Any change to it
invalidates every cache breakpoint after it, at 1.25x write cost. A plugin that
learned at runtime and revised its verdict would repeatedly bust the prompt cache
and could plausibly destroy more value than it saves. A list fixed at setup time
busts the cache exactly once, then stabilises.

This is the deciding argument for setup-time analysis over runtime learning, and
it removes the need for any persistence layer inside the plugin.

## Part 1: Directional body capabilities

### The defect

`PluginCapabilities.WritesBody` is a single boolean covering both directions.
`Pipeline.WritesBody()` (`core/pipeline/pipeline.go:383-390`) is a plain OR
across the chain, with no notion of direction:

```go
func (p *Pipeline) WritesBody() bool {
	for _, plugin := range p.plugins {
		if plugin.Capabilities().Normalize().WritesBody {
			return true
		}
	}
	return false
}
```

Both proxy listeners consult that predicate to decide whether an SSE response may
be relayed incrementally (`forwardproxy/server.go:383-387`,
`reverseproxy/server.go:440-454`). A plugin that rewrites only the **request** body
therefore disables **response** streaming, for a body it never touches:

```go
if isEventStream(resp.Header.Get("Content-Type")) && resp.Body != nil {
	if s.OutboundPipeline.WritesBody() {
		slog.Warn("forward-proxy: text/event-stream response with WritesBody plugin — falling back to buffered path", ...)
```

The cost is latency and feel, not correctness: the buffered path restores the body
verbatim (`reverseproxy/server.go:466`) and the `event:` line is preserved on the
re-framing path (`reverseproxy/server.go:749-754`). But the
user-visible effect is that a long completion arrives in one lump after a silent
wait instead of appearing incrementally, which is the first thing anyone notices.

The fallback is also independent of `ErrorPolicy`: `WritesBody()` asks whether any
plugin *declares* the capability, not whether it is currently *permitted* to
mutate. So a plugin running in `on_error: observe` (measure-only) loses response
streaming before it has gained anything.

Request buffering is not part of this cost. Requests are never streamed — Claude
Code sends one complete `POST /v1/messages` with a `Content-Length` — and the
request body is already read end to end before dispatch whenever any plugin
declares `ReadsBody` (`forwardproxy/server.go:256-266`, `pipeline.go:368-376`).
`inference-parser` declares it, so every request through the demo chain is already
fully buffered. A request-only mutator adds no buffering at all.

### The change

```go
type PluginCapabilities struct {
	ReadsBody bool

	// WritesRequestBody declares the plugin may call pctx.SetBody.
	WritesRequestBody bool

	// WritesResponseBody declares the plugin may call pctx.SetResponseBody.
	// Listeners fall back from incremental SSE relay to the buffered path
	// only when some plugin declares this.
	WritesResponseBody bool

	Requires    []string
	RequiresAny []string
	Description string
}
```

`Normalize()` promotes `ReadsBody` from either write flag.
`Pipeline.WritesResponseBody()` becomes the streaming predicate;
`Pipeline.WritesRequestBody()` keeps gating request propagation.

### Why rename rather than add

Adding `WritesResponseBody` alongside an unchanged `WritesBody` would default the new
field to `false`, so an out-of-tree plugin that rewrites responses would silently
start streaming and then call `SetResponseBody` after bytes had already been sent.
That converts a latency annoyance into a correctness bug, in code the change does
not touch.

Plugins are compiled into the binary — registration is via
`plugins.RegisterPlugin`, with no dynamic loading — so renaming the field gives
every such author a **compile error** instead. That is the safest available
failure: impossible to miss, trivially fixed, and it forces the author to answer
"which body?" rather than inherit a default they never considered. It also retires
the ambiguity permanently, since after the change there is no undirected option to
pick.

The rename is confined to Go source and documentation. `PluginCapabilities` has no
struct tags and is never marshalled directly; the session API defines its own
tagged wire types (`sessionapi.CatalogEntry` at `sessionapi/server.go:55-63`, and
the pipeline view whose `readsBody` field is at `:167`) and neither exposes
`writesBody`. So **no wire key
and no configuration key changes.** Capabilities are not configurable, so
`core/config` is untouched.

### Compatibility audit

Every in-tree plugin that declares the capability today, and what it actually does:

| Plugin | Rewrites request | Rewrites response | Evidence | After the change |
|---|---|---|---|---|
| `context-guru` | yes | **no** | `contextguru/plugin.go:160`; no `SetResponseBody` call anywhere | `WritesRequestBody` — **gains** response streaming |
| `sparc` | **no** | yes | `sparc/respond.go:111,122`; calls `SetBody` nowhere | `WritesResponseBody` only — the request flag was stale, and dropping it frees the request-mutator slot so `[sparc, tool-prune]` builds |
| `cpex` | yes | yes | `cpex/plugin.go:122`; `cmf_body.go:609`, `cmf_a2a.go:216`, `cmf_inference.go:218` | both flags — unchanged |
| `tool-prune` | yes | no | new | `WritesRequestBody` — streams |

Two of the three genuinely need the buffered path and keep it. Only `context-guru`
changes behaviour, and only by regaining streaming it never needed to lose.

`validateCapabilities` (`pipeline.go:549-573`) becomes direction-aware, but the
**outcome is identical for every configuration that exists today**: all three
current plugins write requests, so they remain mutually exclusive exactly as
before. The reader-ordering rule stays triggered by either write flag, so no
configuration that validates today starts failing and none that fails starts
passing.

### One adjacent fix

`cloneCatalog` (`plugins/registry.go:202-222`) copies capability fields one at a
time, so any field added to `PluginCapabilities` is silently dropped from
`/v1/plugins`:

```go
Capabilities: pipeline.PluginCapabilities{
	ReadsBody:   caps.ReadsBody,
	WritesBody:  caps.WritesBody,
	Description: caps.Description,
	Requires:    append([]string(nil), caps.Requires...),
	RequiresAny: append([]string(nil), caps.RequiresAny...),
},
```

Replace the field-by-field construction with a struct copy plus explicit slice
reallocation, which preserves the deep-copy guarantee and picks up future fields
automatically:

```go
c := caps
c.Requires = append([]string(nil), caps.Requires...)
c.RequiresAny = append([]string(nil), caps.RequiresAny...)
```

### Documented contract fix

`SetBody`'s godoc (`pipeline/context.go:390-396`) states that a plugin without the
write capability which calls `SetBody` mutates only the in-memory context and
leaves the wire unchanged. The code does not do this: `SetBody` sets
`c.bodyMutated = true` unconditionally outside observe mode (`context.go:424`), and
the listeners gate purely on `pctx.BodyMutated()` (`forwardproxy/server.go:335`,
`reverseproxy/server.go:358`). An undeclared mutation therefore does reach the
wire.

This proposal does not add the missing enforcement — doing so silently would break
any plugin currently relying on the actual behaviour. It corrects the comment to
describe what the code does, and notes the divergence so a future change can close
it deliberately. Left as documented, it is a live trap: it makes "just don't
declare the capability" look like a legitimate way to keep streaming.

## Part 2: The `tool-prune` plugin

### Behaviour

One registration, `plugins.RegisterPlugin("tool-prune", ...)`:

```go
pipeline.PluginCapabilities{
	WritesRequestBody: true,
	RequiresAny:       []string{"inference-parser"},
	Description:       "Removes unused tool definitions from inference requests",
}
```

On each outbound request:

1. Skip unless the path matches `paths` (default `/v1/chat/completions`,
   `/v1/completions`, `/v1/messages`), matched by suffix as `context-guru` does.
2. Read the parsed manifest from `pctx.Extensions.Inference.Tools`.
3. For each configured name present in the manifest, delete its element from the
   `tools` array of the **original** request bytes with `sjson.DeleteBytes`,
   iterating indices in descending order so earlier deletions do not shift later
   ones.
4. Call `pctx.SetBody` once with the result.

Every byte outside the deleted array elements is unchanged. `gjson`/`sjson` are
already in `core/go.mod` (currently indirect), so no new dependency.

Any error or panic fails open: the original body is forwarded unmodified, so the
plugin's own failure modes cannot break a request. That is a narrower promise
than "pruning is always safe": pruning changes the request, and whether a
provider or gateway accepts a validly pruned manifest is outside what the plugin
can see. `on_error: observe` is how that is established safely.

A forced `tool_choice` is the one case where the manifest and another field must
agree, so a tool named by `tool_choice` is never removed regardless of the
configured list.

### Configuration

```yaml
pipeline:
  outbound:
    plugins:
      - inference-parser
      - mcp-parser
      - a2a-parser
      - name: tool-prune
        # on_error defaults to enforce; the empty remove list is the gate.
        config:
          remove: [NotebookEdit, ScheduleWakeup, TaskOutput]
```

`remove` is the complete verdict. There is no learning, no state, and no storage
dependency.

### Measure-only mode comes from the framework

`on_error` is a per-plugin policy already parsed by `core/config`
(`config.go:257`, values `enforce | observe | off`). Under `observe`, `SetBody` is
a no-op on bytes but still records a modify `Invocation` with `Shadow=true`
(`context.go:397-421`), so "would have removed" is countable without changing a
single request. `off` skips dispatch entirely.

This is why one registration suffices: the same plugin code serves measure and
enforce, selected by one word of configuration. `context.go` states the intent
directly — "Plugin code therefore looks identical under enforce and observe."

Off-by-default is satisfied structurally, and by the remove list rather than the
policy: an empty `remove` is a no-op whatever `on_error` says, so filling the list
is the single deliberate act that enables the plugin. `on_error: observe` remains
available as a projection mode, but is not the shipped default — two guards where
one suffices only added a step operators skipped.

### Where the list comes from: `abctl tools scan`

A new subcommand ports the discovery core of `claude-tool-audit.py` (about 40 of
its 814 lines) into Go:

- Read `~/.claude/projects/**/*.jsonl`.
- Hot-path line filter on the literal `"tool_use"` before any JSON parsing.
- Deduplicate tool calls by the unique `tool_use` block id.
- Window to the last `--days` (default 30).

`abctl` currently has no subcommand dispatch — `main.go` parses two flags and
launches the terminal UI. The change checks for a non-flag first argument before
`flag.Parse()` and dispatches, falling through to the UI otherwise.

```sh
abctl tools scan [--days 30] [--keep Name,Name] [--write <config.yaml>]
```

Without `--write` it prints the YAML block. With `--write` it patches the
`remove:` list of the `tool-prune` entry in place, idempotently.

### The offered-set problem, and how the scan stays safe

Transcripts record tools that were **called**, never tools that were **offered**.
This is structural, not a defect: a configured-but-never-invoked tool leaves no
trace. Two consequences:

- Tools never called in the window but bundled in the known Claude Code tool set
  are the removal candidates, and they are where most of the 20,500 tokens sit.
- A tool name the scan has never heard of is **kept**. Removing a tool the model
  needs is the harmful direction of failure; carrying a few extra definitions is
  not.

The bundled set is version-sensitive: developers on newer Claude Code releases
produced tool calls the current table does not recognise. Two mitigations:

1. Unknown names are always kept, so drift costs savings, never correctness.
2. At startup the plugin compares its configured `remove` list against the names
   it observes in `ext.Tools` and logs any configured name that never appears, so
   a stale list surfaces as a warning rather than a silent no-op.

A `--keep` flag and a small "implies" table cover tools whose use is indirect —
for example `Agent` implying `SendMessage`, which a transcript may not show being
called directly.

### Installation flow

`install-demo.sh` already downloads both binaries with checksum verification and
prints next steps. `authbridge-proxy` writes `cortex-ca/demo.yaml` on first run
(`cmd/authbridge-proxy/demo.go`), and that file is hot-reloaded — its own header
says so — so the list can be filled in without a restart.

- `demoConfigYAML()` gains the `tool-prune` entry with `on_error: observe` and an
  empty `remove: []`.
- `install-demo.sh` runs `abctl tools scan --write` when the config already
  exists, and otherwise prints the block in its next-steps output alongside the
  existing "Watch traffic" hint.

### What the user sees in-session

Claude Code's `/context` breakdown has `System tools`, `Tool schemas`, `MCP tools`,
`Custom agents`, `Memory files` and `Free space` line items — confirmed by string
inspection of the installed 2.1.257 binary. **It will not show this saving.** It is
a client-side pre-flight breakdown of what the CLI assembled; it necessarily
computes `Free space` itself, and the pruning happens downstream. This is the
first place a user would look, and it must be documented as unaffected.

What does move is `/cost` and any figure derived from the API response `usage`
block: the server bills the request it received, so `input_tokens` and
`cache_read_input_tokens` genuinely drop.

But `/cost` reports a **session total**, with no baseline to compare against. A
user cannot tell from one aggregate number how much of it the plugin saved, or
whether enabling the plugin was worth it. That is what part 3 is for.

The honest limit: proxy-side pruning saves money but does **not** return context
window to the user. The client still believes it sent the full manifest, so
auto-compact triggers at the same point. Recovering headroom requires client-side
configuration (`--allowedTools`, disabling unused MCP servers). AuthBridge's
advantage is the complement — it applies to every agent behind it with no
per-client change, and it measures.

## Part 3: Plugin metrics in `abctl`

### What the plugin counts

Counters are in-memory and per-process, guarded by a mutex. No persistence, no
storage backend, no new dependency.

```go
type metrics struct {
	mu sync.Mutex

	requestsSeen      uint64 // matched the path gate
	requestsPruned    uint64 // body actually rewritten (enforce)
	requestsProjected uint64 // would have been rewritten (observe)

	toolsRemoved uint64
	perTool      map[string]uint64

	bytesRemoved uint64 // sum of deleted tools array elements

	promptTokens      uint64 // from response usage, via OnFinish
	requestBytes      uint64 // body size of the same requests
	requestsWithUsage uint64
}
```

The plugin distinguishes enforce from observe without inspecting policy: under
`ErrorPolicyObserve`, `SetBody` leaves `bodyMutated` false
(`pipeline/context.go:418-421`), so checking `pctx.BodyMutated()` after the call
tells the plugin which counter to increment. This makes **observe mode a
projection**: the plugin computes exactly what it would remove and reports the
saving before a single request changes. Read the projection, then flip to
`enforce`.

### Turning bytes into tokens without guessing

The plugin knows removed bytes exactly, but billing is denominated in tokens.
Rather than bundle a tokenizer or hardcode a bytes-per-token constant, the ratio
is calibrated on the user's own traffic: `OnFinish` reads
`pctx.Extensions.Inference.PromptTokens` — populated by `inference-parser` from
the response `usage` block — alongside the request body size for that same
request. Estimated tokens saved is then `bytesRemoved x (promptTokens /
requestBytes)`, reported with its sample size and labelled an estimate.

`OnFinish` is the correct hook for response-derived data: `inference-parser` is a
`StreamingResponder`, and `RunResponse` skips `OnResponse` for such plugins.

This re-adds `OnFinish`, which the static-list decision had removed. The scope is
deliberately narrow — two counter reads, no persistence and no influence on the
removal list, which stays entirely configuration-driven.

One approximation to state plainly: under `enforce`, `PromptTokens` is already the
post-pruning count, so the ratio is measured on pruned requests. That is
acceptable for a bytes-to-tokens conversion factor, which is a property of the
tokenizer and content mix rather than of the pruning, but it is why the figure is
labelled an estimate rather than a measurement.

### A generic metrics interface

Added to `core/pipeline/plugin.go` beside the existing optional interfaces:

```go
// Metric is one operator-facing counter reported by a plugin.
type Metric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"` // count | bytes | tokens | ratio
	Note  string  `json:"note,omitempty"` // e.g. "estimate, n=1284"
}

// MetricsProvider is implemented by plugins that expose counters for
// operator display. Called on demand from the session API; must be safe
// for concurrent use and must not block.
type MetricsProvider interface {
	Metrics() []Metric
}
```

`plugins.StatsSource` and `auth.Stats` already exist but are the wrong vehicle:
`auth.Stats` is auth-specific, with typed approval and denial enums and a custom
`MarshalJSON` (`auth/auth.go:61,183`). Carrying "bytes removed" through it would
distort its meaning. A separate plugin-defined interface keeps auth statistics
auth-shaped and gives every future plugin a display channel.

### Wire and UI

Both extension points already have the exact pattern needed.

- `sessionapi`: add `Metrics []pipeline.Metric` to the pipeline plugin view and
  populate it in `describePipeline` with a three-line type assertion mirroring the
  `RawConfigProvider` case at `sessionapi/server.go:234`. Matching field on
  `apiclient.PipelinePlugin`.
- `cmd/abctl/tui/plugin_detail_pane.go`: a `Metrics:` section after the dependency
  sections and before `Config:`, following the always-newline convention that the
  comment at `:67-70` records as deliberate — it exists to stop layout jitter when
  navigating between plugins that do and do not have the section. `Note` renders
  in `styleHint`, as `Description` already does at `:27`.

Roughly 20 lines in the pane, 3 in `describePipeline`, 2 struct fields, and about
35 lines for the interface plus the plugin's counters.

The operator reads something like:

```text
Metrics:
  requests seen              1284  count
  requests pruned            1284  count
  tools removed             11556  count
  bytes removed           9389184  bytes
  bytes removed / request     7312  bytes
  tokens saved / request     ~1830  tokens   estimate, n=1284
```

In observe mode the same rows appear with `requests projected` in place of
`requests pruned`, so the distinction between a projection and a realised saving
is visible in the readout rather than inferred from configuration.

### Limits

Counters are per-process and in-memory: they reset when the proxy restarts and are
not aggregated across a fleet. That is the right trade for the laptop scenario this
targets, and it is what keeps the plugin free of a storage dependency. Fleet-wide
aggregation belongs on the existing stats server
(`runtimeutil.StartStatServer`, port 47602 in the demo config), which is a
natural later addition and does not change the plugin.

## Delivery

Four commits, sequenced so the regression argument survives review.

1. **Mechanical rename.** `WritesBody` to `WritesRequestBody` across 107
   references in 28 files (Go and documentation), with no semantic change.
   Reviewable as a single token substitution, and every existing test passing
   still carries meaning because nothing but the name moved.
2. **The split.** Add `WritesResponseBody`; declare it on `sparc` and `cpex`;
   point both listener branches at `Pipeline.WritesResponseBody()`; convert
   `cloneCatalog` to a struct copy; correct the `SetBody` godoc; add tests.
3. **The metrics channel.** `Metric` and `MetricsProvider` in `core/pipeline`;
   the `describePipeline` type assertion and wire field; the `abctl` pane section.
   Lands before the plugin so the plugin arrives already visible, and so this
   generic addition is reviewed on its own merits rather than as plugin scaffolding.
4. **`tool-prune`.** Plugin and its counters, `abctl tools scan`,
   `demoConfigYAML()` entry, `install-demo.sh` wiring, and documentation.

### Testing

For part 1, the primary regression argument is that the existing body-capability
tests pass with only the identifier renamed. On top of that:

- A truth table for `Pipeline.WritesResponseBody()` across the four plugin shapes
  (request-only, response-only, both, neither).
- Listener tests: an SSE response with a request-only writer in the chain relays
  incrementally; with a `sparc`-shaped or `cpex`-shaped chain it still buffers.
- A reflection-based `cloneCatalog` round-trip that fails if any future
  capability field is dropped.
- `validateCapabilities` table assertions covering the current plugin
  combinations, to show acceptance and rejection are unchanged.

For part 2:

- Byte-level assertions that pruning a manifest leaves every other byte of the
  request identical, including key order and whitespace.
- Descending-index deletion verified against a manifest where a naive ascending
  loop would delete the wrong elements.
- Names absent from the manifest are ignored without error.
- Malformed and truncated bodies fail open, forwarding the original bytes.
- Under `on_error: observe`, the body is unchanged and a `Shadow=true` invocation
  is recorded.
- Scanner tests over fixture transcripts: window boundaries, `tool_use` block
  deduplication, unknown names retained, `--keep` honoured.

For part 3:

- A plugin that does not implement `MetricsProvider` produces no `metrics` key on
  the wire and renders `(none)` without disturbing pane layout.
- Concurrent `Metrics()` calls against a live counter update, under the race
  detector.
- Enforce mode increments `requestsPruned`; observe mode increments
  `requestsProjected` and leaves `bytesRemoved` accumulating, so the projection is
  non-zero while the body is untouched.
- The bytes-to-tokens ratio is reported as zero-valued rather than dividing by
  zero when no response usage has been seen yet.

### Risks

| Risk | Mitigation |
|---|---|
| Removing a tool the agent needs | Unknown names always kept; a tool forced by `tool_choice` never pruned; `--keep` override; empty `remove` ships as the off switch; fail open on any error |
| Stale bundled tool set as Claude Code evolves | Drift reduces savings only; plugin warns on configured names never observed in `ext.Tools` |
| One-off prompt-cache invalidation when the list changes | Inherent and bounded: static list means it happens once, then the prefix is stable |
| Commit 1 conflicts with in-flight branches declaring `WritesBody` | One-line fix per branch; the compile error makes it self-evident |
| `context-guru` regaining response streaming exposes a latent bug in that path | Covered by the listener tests above; the path is already exercised by chains with no body writer |
| The estimated token saving is read as a measurement | Unit and sample size shown on the row; the underlying byte counts are exact and reported separately, so the estimate is never the only number |
| Counters reset on restart and mislead someone comparing across restarts | Documented; `requests seen` is displayed alongside every derived figure so the sample behind it is always visible |

## Open questions

None blocking. Two items deliberately deferred:

- Adding the missing enforcement so `SetBody` matches its documented contract.
  Needs its own compatibility review.
- Fleet-wide metric aggregation on the stats server, and a Prometheus exposition
  of `MetricsProvider`. The per-process counters in part 3 cover the laptop case
  this targets; neither addition changes the plugin.

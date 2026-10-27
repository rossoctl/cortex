# Script plugins: customizing Cortex with a file

Date: 2026-10-21
Status: accepted
Branches: `feat/script-plugins-1-capabilities` … `feat/script-plugins-5-responses`
(five stacked PRs, each under 2000 lines)

## Problem

1. Every Cortex plugin is Go, compiled into the binary behind a build tag. A user
   who wants to mask a secret, block a command or log what tools an agent calls
   has to write a Go plugin, rebuild the proxy and reinstall it.
2. Most such customizations are small, personal and short-lived — "on a pinch",
   not a product feature. A rebuild is out of proportion to them.
3. The user's real authoring tool is their coding agent. Whatever the mechanism,
   an agent must be able to write it, test it against real traffic and turn it
   on without the user doing anything but reading the result.
4. A customization that breaks a request breaks the user's agent mid-task, so
   people will not try one unless trying is safe.

Go plugins stay the main extension mechanism. This adds a second, cheaper one
beside them.

## Requirements

- A script reads **and writes** both requests and responses, as a Go plugin
  can. That is a hard requirement.
- A script keeps state between calls.
- A script can sit between Go plugins in the chain.
- Trying a script cannot break traffic: new scripts start in shadow mode.
- Laptop only. In a sidecar, a config that names a script must not run it.
- Each PR is under 2000 lines.

## Non-goals

- Rewriting response **headers**. No Go plugin can either: every listener hands
  plugins `resp.Header.Clone()` and sends the originals. That is a listener
  change of its own, and would serve Go plugins too.
- Redirecting a request (`WritesDestination`). At most one plugin per pipeline
  may redirect, and routing belongs to `inference-router`.
- A sandbox. These are the user's own files on the user's own laptop, run with
  the user's privileges — like a shell alias. Safety comes from shadow mode and
  replay, not isolation.
- Scrubbing what Cortex records. The parsers run before scripts, so the session
  store and archive keep what the client sent. The doc says so.
- Vendor formats beyond Anthropic Messages, OpenAI Chat Completions and OpenAI
  Responses in the SDK's text helpers. Every other format is reachable through
  `flow.json`.

## User experience

A plugin is one executable file in `~/.cortex/plugins/`:

```python
#!/usr/bin/env python3
import re
from cortex import on, run

AWS = re.compile(r"\bAKIA[0-9A-Z]{16}\b")

@on.request
def mask(flow):
    flow.replace_text(AWS, "[aws-key]")

@on.response(rewrite=True)
def scrub(flow):
    flow.replace_text(r"\b[\w-]+\.corp\.internal\b", "[internal-host]")

run()                 # shadow; run(enforce=True) applies edits and denies
```

| Step | The user | Cortex |
|---|---|---|
| Write | saves the file | starts it within a second, in shadow mode |
| Test | `PYTHONPATH=~/.cortex/plugins/_sdk python3 mask.py --replay <session-id>` | nothing: replay reads the session API |
| Watch | agentop timeline | rows named after the file; `print` output on the row |
| Enable | `run(enforce=True)` and save | restarts that one worker |
| Disable | deletes it, or renames it to `_mask.py` | stops it |

No YAML is edited: on a local install `config.ApplyPreset` appends a `script`
entry to the outbound chain when the chain does not already name one.

## Design

### Units

| Unit | Package | Knows about |
|---|---|---|
| Capability narrowing | `core/pipeline`, `core/plugins` | nothing script-specific |
| Worker | `core/plugins/script/internal/worker` | processes and line-delimited JSON; nothing about HTTP |
| Host plugin `script` | `core/plugins/script` | the folder, the message shapes, pctx |
| SDK | `core/plugins/script/sdk/cortex.py` (embedded) | the message shapes, vendor body formats, the session API |

### 1. Instances may narrow their capabilities

Today `Capabilities()` must be the same for every instance of a registered
name, because `plugins.Catalog()` reads it once from an unconfigured instance.
That forces one `script` name to declare `WritesResponseBody` always, which
turns off SSE streaming for the whole pipeline even when no loaded script
rewrites a response.

New rule: the unconfigured instance's capabilities are the **ceiling** the
catalog reports. A configured instance may report a **subset**.
`plugins.BuildWithDeps` reads both and logs a WARN for a configured instance
that exceeds its factory's ceiling. It warns rather than refuses: the old rule
was never enforced, and an in-tree test plugin (the parity spy) already widens
from config. `Pipeline.New` already reads capabilities from the instances it is
given, so nothing else changes.

Subset means: no boolean goes from false to true; `ReadsBody` is not kept alone
when the ceiling writes a body, since a pure reader must precede every mutator;
`Requires` and `RequiresLater` may not gain names; `RequiresAny` is the
ceiling's set or empty. `Description` is not compared.

This is not script-specific: `cpex` declares `WritesResponseBody`
unconditionally and could narrow to it only when a response hook is
configured.

### 2. Worker

One long-lived process per script file. The protocol is one JSON object per
line in each direction, over the process's stdin and stdout; stderr is
captured and logged.

- **Handshake.** The first line the process writes is
  `{"cortex": 1, "hooks": [...], "enforce": bool, "rewrite_response": bool}`.
  It must arrive within the start timeout (5s). A process that writes anything
  else, or exits, fails to start.
- **Calls.** `Call(ctx, msg)` writes `msg` with an `id`, then reads lines until
  one carries that `id`. One call at a time per worker: calls queue on a mutex,
  so a script is a plain loop and its state cannot race. Lines carrying
  `"log"` instead of an `id` are captured as the call's log output.
- **Timeout.** Each call has a deadline (default 2s, settable per slot). On
  expiry the whole process group is killed — the reply can no longer be matched
  to a request — and the worker restarts on its next call.
- **Crash.** Exit is detected on the next read. Restarts back off from 100ms to
  30s; while backing off, calls fail immediately.
- **Environment.** The user's login-shell `PATH` (resolved once per process —
  the launchd service's `PATH` is `/usr/bin:/bin:/usr/sbin:/sbin`, which finds
  Apple's Python 3.9), `HOME`, `PYTHONPATH` pointing at the SDK, and nothing
  that routes through the proxy: `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` and
  their lowercase forms are removed, so a script's own HTTP calls cannot loop
  back into the pipeline that is calling it.
- **Pool.** Workers outlive pipeline builds. A pool keyed by
  `(absolute path, content hash)` hands an existing worker to the next build
  when the file is unchanged, so editing an unrelated plugin's config does not
  restart scripts or lose their state. A worker no build references is stopped.

### 3. The `script` host plugin

Registered as `script`, built into the `local` and `full` profiles. Ceiling
capabilities: `ReadsBody`, `WritesRequestBody`, `WritesResponseBody`.

**Config** (all optional):

```yaml
- name: script
  config:
    dir: early          # ~/.cortex/plugins/early; default ~/.cortex/plugins
    timeout: 2s         # per call
    fail_closed: false  # a script error blocks the request instead of passing it
```

**Folder rules.**
- Files run in byte order of their names. A prefix like `10-` orders them.
- The plugin name is the file name with a leading `[0-9]+-` and any extension
  removed: `10-mask.py` is `mask`.
- Ignored: names starting with `.` or `_`, names ending in `~`, `.swp`, `.tmp`,
  directories, and files that are not executable.
- Two files with the same plugin name: the later one is refused and logged.
- A slot reads only the files directly in its folder; a subfolder belongs to
  the slot that names it in `dir`. No file can be in two slots.

**Lifecycle.** `Configure` scans the folder, takes each file's worker from the
pool and waits for its handshake. A file whose worker fails to start is
skipped and logged; it does not fail the build. The instance's capabilities are
the union of what the loaded scripts need: `ReadsBody` and `WritesRequestBody`
always (scripts read and rewrite request bodies), `WritesResponseBody` only if
some loaded script declared `rewrite_response`.

**Reload.** The reloader watches each slot's folder as well as the config file,
and its change check hashes both. Adding, removing, renaming or editing a script
rebuilds the pipelines exactly as a config edit does. The pool keeps unchanged
scripts running across that rebuild.

**Requests.** For each loaded script with a request hook, in order:
1. Send `{"hook": "request", "plugin", "session", "request": {...}}` where
   `request` has `method`, `host`, `path`, `headers` (with `authorization`,
   `proxy-authorization`, `cookie`, `x-api-key` and `api-key` removed), `body`
   (UTF-8, or `body_base64`), and the parsed `inference` / `mcp` / `a2a`
   extensions marshalled with their own JSON tags — the shape the session API
   already serves, so replay sees what live traffic sees.
2. Apply the reply: `deny` stops the chain; `set_headers` / `remove_headers`
   edit `pctx.Headers`; `body` goes through `pctx.SetBody`; `invocation` is
   recorded under the script's name.
3. A script in shadow mode runs and records, but its deny and edits are not
   applied; its invocation is marked `Shadow`. The next script sees the request
   as it was before the shadow script ran.

**Responses.** For each loaded script with a response hook, in reverse order —
the same order the pipeline's own response pass uses. The host is a
`StreamingResponder`, so the pipeline sends it every response through
`OnResponseFrame`, never `OnResponse`:
- Not yet delivered (`!pctx.ResponseDelivered()` at the last frame): JSON, or
  SSE held back for a response writer. The hook gets
  `{"hook": "response", ..., "response": {"status", "headers", "body"}}` and
  its `deny` and `body` apply (through `pctx.SetResponseBody`).
- Delivered: SSE relayed as it arrived. The host copies each frame (up to
  10MiB) and on `last` sends them as `frames` with `"streamed": true`. Edits
  are refused with a recorded error and a deny is recorded late, because the
  client already has every byte.
- A script that declared `rewrite_response` narrows the slot to
  `WritesResponseBody`, which sends SSE down the buffered path, so its edits
  always apply.

Finding from the end-to-end run: on that buffered SSE path the listener folds
the upstream's frames through the parsers before the last frame reaches the
host, so the parser's recorded completion is the upstream's text and not the
rewrite the client received. This is the same boundary as on requests — what
Cortex records is what was exchanged with the upstream — and is documented
rather than changed.

**Errors.** A timeout, crash, malformed reply or an edit to something the
script did not declare (a response body from a script without
`rewrite_response`) is recorded as an `observe` invocation with reason
`script_error` and the message in `details`. By default the request continues
(**fail open**: trying a script must be safe). With `fail_closed: true` the
request is denied with `plugin.script-failed`. A script in shadow mode never
fails closed.

**Gate.** `plugins.Deps` gains `LocalInstall bool`. `cmd/cortex` sets it from
`startedFromLocalInstall`; every other binary leaves it false. `Configure`
refuses to load scripts without it and the instance runs as a no-op, with one
WARN at build time, so a sidecar config naming `script` cannot run commands.

### 4. SDK

`cortex.py`, embedded in the binary and written by the host to
`~/.cortex/plugins/_sdk/cortex.py` when missing or different; the worker's
`PYTHONPATH` includes that directory. Python 3.9-compatible (the system
interpreter on macOS).

```python
from cortex import on, run

@on.request                     # flow.request, flow.json, flow.texts, flow.latest_text
@on.response                    # read only; flow.response, flow.streamed, flow.completion
@on.response(rewrite=True)      # may rewrite; forces buffering of streamed responses
run(enforce=False)
```

`flow` offers `session`, `host`, `path`, `headers`, `body`, `json`, and:
- `flow.texts` — every prompt text fragment, Anthropic or OpenAI shape.
- `flow.latest_text` — the text of the newest user message only. Gates should
  read this: the client re-sends its whole history every turn, so a rule that
  denies on history denies every later turn too.
- `flow.replace_text(pattern, repl)` — rewrites every fragment and writes the
  body back. Must be deterministic: a replacement that changes from turn to turn
  defeats the provider's prompt cache on every request.
- `flow.deny(reason, status=403)`, `flow.set_header`, `flow.remove_header`.
- On responses: `flow.completion` (assembled text), and `replace_text` on the
  response body — for a streamed response it joins each content block's text
  deltas, rewrites, and re-emits the block as one delta.

`print()` inside a hook is captured and recorded on that request's invocation.
An exception becomes a `script_error`, never a dead worker.

`PYTHONPATH=~/.cortex/plugins/_sdk python3 mask.py --replay <session-id>` reads the session from
`http://127.0.0.1:47601` (bypassing any proxy), rebuilds each recorded inference
request from its parsed messages, runs the request hooks and prints what would
be changed or denied. Replay covers text-level hooks; recorded events do not
carry raw bodies.

## Ordering

- Inside a slot: file name order.
- Between slots: the YAML. Repeat `script` with different `dir` values to put
  Go plugins between scripts.
- The host is a request writer, so `pipeline.New` places it after every body
  reader (the parsers, `ibac`). A script cannot run before a parser.
- Parsed extensions are not re-parsed after a write, so a Go plugin after a
  script that reads `pctx.Extensions` sees the client's text, not the script's.

## Costs

- A Python worker round trip on a 450KB request: 5.5ms median (measured).
  One script serves about 180 requests per second.
- Changing a script restarts that script, and rebuilds the pipelines — which
  resets Go plugins' counters, as any config reload already does.
- A loaded `rewrite=True` script buffers every streamed response in its slot's
  pipeline until it completes.
- Turning a rewrite on mid-session misses the provider's prompt cache once.

## PRs

| # | Branch | Contents |
|---|---|---|
| 1 | `feat/script-plugins-1-capabilities` | this spec; capability narrowing |
| 2 | `feat/script-plugins-2-worker` | the worker and pool |
| 3 | `feat/script-plugins-3-host` | host plugin, request side; folder; gate; preset; reloader watch; profiles |
| 4 | `feat/script-plugins-4-sdk` | SDK request side, replay, embedding, `docs/script-plugin.md` |
| 5 | `feat/script-plugins-5-responses` | response side in host and SDK |

## Testing

- Go tests never need Python: the worker tests run the test binary itself as
  the child process, switched by an environment variable.
- The host's tests use small `/bin/sh` scripts for the folder and lifecycle, and
  the self-exec worker for protocol and error cases.
- The SDK has pytest tests, run by the existing Python CI job, and a Go test
  that runs the embedded SDK under `python3` when one is on `PATH` (skipped
  otherwise).

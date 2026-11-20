# Running the evolve loop end to end on a laptop

This walks through running the [`evolve` plugin](evolve-plugin.md) against a real
altk-evolve server and a real Claude Code session, **without touching your everyday Cortex**.
A development build of Cortex runs beside the installed one on its own ports; only the
Claude Code runs you point at it go through the plugin.

By the end you will have watched the whole loop:

```
claude -p ──► dev Cortex (47700) ──► LLM gateway
                 │   ▲
   process_      │   │ get_guidelines_with_attribution
   trajectory    ▼   │ list_entities
              evolve-mcp (8201) ── cortex-candidates  (learned, not injected)
                                └─ cortex             (reviewed, injected)
```

1. a guideline you put in `cortex` is injected into a Claude Code request,
2. the session is sent to evolve when it goes idle and evolve learns candidates from it,
3. you promote a candidate and the next session receives it,
4. you withdraw a guideline and the next session no longer does.

Allow about 20 minutes. Steps 1–4 are one-time setup; steps 5–9 are the loop.

## What you need

| | |
|---|---|
| Go ≥ 1.26.10 | `go version` (`brew install go`) |
| uv | `uv --version` |
| A Cortex checkout with the plugin | this repo |
| An altk-evolve checkout | `altk-evolve` main, or the first release after 1.10.0, has everything the plugin uses: `include_details` ([#485](https://github.com/AgentToolkit/altk-evolve/pull/485)), the `environment-grounded` prompt ([#479](https://github.com/AgentToolkit/altk-evolve/pull/479)), and the fix that lets Claude models on Bedrock generate guidelines ([#477](https://github.com/AgentToolkit/altk-evolve/pull/477)). Any `altk-evolve` ≥ 1.5.2 works: without `include_details` the plugin reads the guidelines with `list_entities`, and without the prompt it learns with evolve's default and logs a warning |
| The installed Cortex | `~/.cortex/ca/ca.crt` must exist and be trusted — the dev build reuses that CA |
| LLM credentials for evolve | any litellm model; evolve calls it to generate guidelines |
| Claude Code credentials | whatever `claude -p` normally uses |

The commands below use these variables; set them once per terminal:

```bash
export CORTEX_SRC=~/src/cortex       # Cortex checkout
export EVOLVE_SRC=~/src/altk-evolve  # altk-evolve checkout
export DEV=/tmp/cortex-dev           # scratch dir for this walkthrough, evolve's data included
```

You will use **three terminals**: one for evolve, one for the dev Cortex, one to drive.

---

## 1. Build Cortex with the plugin

```bash
cd $CORTEX_SRC && make cortex
```

**What it does.** Builds `./bin/cortex` with the `full` profile, which links the `evolve`
plugin. It does not install anything; your installed Cortex in `~/.local/bin` is unchanged.

**Look for** `include_plugin_evolve` in the printed tag list:

```
→ building cortex (profile full): include_plugin_a2aparser,include_plugin_evolve,…
```

**If it is missing**, you are on a branch without the plugin, or `scripts/profile-tags/profiles.go`
has no `"evolve"` entry.

Before going further, make sure nothing is left over from an earlier run:
`lsof -nP -iTCP:8201 -iTCP:47700 -sTCP:LISTEN` should print nothing.

## 2. Write evolve's environment file

```bash
umask 077 && mkdir -p $DEV
cat > $DEV/evolve.env <<'EOF'
EVOLVE_MODEL_NAME=litellm_proxy/Azure/gpt-4o
LITELLM_PROXY_API_BASE=https://<your litellm gateway>
LITELLM_PROXY_API_KEY=<key>
EVOLVE_BACKEND=filesystem
EVOLVE_INJECTION_MODE=retrieval
EVOLVE_OTEL_ENABLED=false
EVOLVE_HOOKS_CONFIG=
EOF
echo "EVOLVE_DATA_DIR=$DEV/evolve-data" >> $DEV/evolve.env
echo "EVOLVE_SQLITE_PATH=$DEV/evolve.sqlite.db" >> $DEV/evolve.env
```

**What each line does.**

- `EVOLVE_MODEL_NAME` + the `LITELLM_*` pair — the model evolve uses for guideline generation
  and conflict resolution. Any litellm model string works; use whichever credentials you have
  (`OPENAI_API_KEY`, etc.).
- `EVOLVE_BACKEND=filesystem`, `EVOLVE_DATA_DIR`, `EVOLVE_SQLITE_PATH` — store guidelines
  as JSON files, and evolve's bookkeeping database, in `$DEV`, apart from any evolve data you
  already have, so cleaning up deletes only what this walkthrough made. The `echo` lines write
  them as absolute paths: evolve resolves a relative one against the directory it was started
  from.
- `EVOLVE_INJECTION_MODE=retrieval` — **required.** Makes `get_guidelines_with_attribution`
  rank guidelines by similarity to the task. The default mode matches the task text
  literally and on this backend returns almost nothing, so the plugin would inject nothing.
- `EVOLVE_OTEL_ENABLED=false` — stops evolve exporting its own telemetry.
- `EVOLVE_HOOKS_CONFIG=` — ignores any `evolve.hooks.yaml` lying around, so the run is
  predictable. (Drop this line if you *want* evolve's redaction hooks.)

The file holds a key, hence `umask 077`.

## 3. Start evolve's MCP server — terminal 1

```bash
set -a; source $DEV/evolve.env; set +a
cd $DEV && env -u HTTPS_PROXY -u HTTP_PROXY \
  uv run --project $EVOLVE_SRC evolve-mcp --transport sse --port 8201
```

**What it does.** Runs the stock evolve MCP server over SSE on `127.0.0.1:8201`. This is the
only thing the plugin talks to.

`env -u HTTPS_PROXY` matters: if your shell routes through your everyday Cortex, evolve's own
LLM calls would go through it too and show up as agent traffic in agentop.

**Look for** — the first start downloads a small embedding model, then:

```
INFO:     Uvicorn running on http://127.0.0.1:8201 (Press CTRL+C to quit)
```

**Check it** from terminal 3: `curl -s --noproxy '*' -m2 -o /dev/null -w '%{http_code}\n' 127.0.0.1:8201/sse`
prints `200` (the request then times out holding the stream open — that is normal).

**If** it exits with `address already in use`, something else holds 8201 — find it with
`lsof -nP -iTCP:8201 -sTCP:LISTEN`.

> **Security.** This server has no authentication: any local process can read and change the
> guidelines it serves. It binds loopback only. Don't run it on a shared machine.

## 4. Write the dev Cortex config

```bash
sed -e 's/127.0.0.1:4760\([0-9]\)/127.0.0.1:4770\1/g' \
    -e 's/generate_ca: true/generate_ca: false/' \
    ~/.cortex/config.yaml > $DEV/config.yaml
uv run -q --project $EVOLVE_SRC python - $DEV/config.yaml <<'EOF'
import sys, yaml
path = sys.argv[1]
with open(path) as f:
    cfg = yaml.safe_load(f)
cfg["pipeline"]["outbound"]["plugins"].append({"name": "evolve", "config": {
    "endpoint": "http://127.0.0.1:8201/sse", "agents": ["claude-code"],
    "idle_after_seconds": 60, "cache_ttl_seconds": 120}})
with open(path, "w") as f:
    yaml.safe_dump(cfg, f, sort_keys=False)
EOF
echo '{"env":{"HTTPS_PROXY":"http://127.0.0.1:47700","NODE_EXTRA_CA_CERTS":"'$HOME'/.cortex/ca/ca.crt"}}' \
  > $DEV/claude-settings.json
```

**What it does.**

- Copies your installed config and moves every listener from `4760x` to `4770x`, so both
  Cortexes can run at once: forward proxy `47700`, session API `47701`, stats `47702`,
  health `47704`.
- `generate_ca: false` — reuse `~/.cortex/ca` (which Claude Code already trusts) and never
  write to it.
- Adds the `evolve` plugin as the **last** outbound plugin (it rewrites the request body,
  and body readers such as the parsers must come before it). It edits the YAML rather than
  appending to the file, because the outbound plugin list needn't be the file's last section;
  the copy loses its comments. PyYAML comes with evolve, hence `--project $EVOLVE_SRC`. `idle_after_seconds: 60` and
  `cache_ttl_seconds: 120` shorten the defaults (15 and 30 minutes) so the loop runs in
  minutes.
- `claude-settings.json` is for step 6: Claude Code's `~/.claude/settings.json` usually pins
  `HTTPS_PROXY` to your everyday Cortex, and settings beat the shell environment, so a
  `--settings` override is the reliable way to route one run elsewhere.

**Check**: `grep -A2 'name: evolve' $DEV/config.yaml` shows the entry with its endpoint, and
`grep -c 4760 $DEV/config.yaml` prints `0`.

Because the config lives outside `~/.cortex`, Cortex treats it as not-a-local-install: the
**session archive is off** (sessions live in memory only), so the dev build cannot write to
your real archive. It does write to the shared cost ledger in `~/.cortex/cost`, so test runs
count toward today's cost in agentop.

## 5. Start the dev Cortex — terminal 2

```bash
env -u HTTPS_PROXY -u HTTP_PROXY $CORTEX_SRC/bin/cortex --config $DEV/config.yaml
```

**Look for**, among the startup lines:

```
msg="session archive disabled — …" reason="not a local install"
msg="HTTP server listening" name=forward-proxy addr=127.0.0.1:47700
msg="session API listening — UNAUTHENTICATED; …" addr=127.0.0.1:47701
```

**Check** from terminal 3 that the plugin is in the pipeline:

```bash
curl -s --noproxy '*' 127.0.0.1:47701/v1/pipeline | jq -c '[.outbound[].name]'
# ["inference-parser","mcp-parser","a2a-parser","tool-prune","evolve"]
```

`evolve` must be last.

**If** startup fails with:

- `evolve config: …` — the YAML is wrong; the message names the field.
- `unknown plugin "evolve"` — the binary was built without it (step 1).
- `cannot start: another Cortex already holds this address` — an earlier dev Cortex is still
  running. `lsof -nP -iTCP:47700 -sTCP:LISTEN` shows its pid; stop it with `kill <pid>`.
  (`curl 127.0.0.1:47704/healthz` answers 200 with an empty body, so check the status code —
  `-w '%{http_code}'` — rather than looking for output.)

The plugin does **not** connect to evolve at startup; the first request does. So a wrong
`endpoint` shows up later as `evolve_unavailable` (step 6), not here.

## 6. Seed one guideline and watch it get injected — terminal 3

Evolve starts empty, so put one guideline into the reviewed namespace by hand. A *canary* —
an instruction with a visible effect — proves the text reached the model:

```bash
set -a; source $DEV/evolve.env; set +a
ev() { (cd $DEV && env -u HTTPS_PROXY uv run -q --project $EVOLVE_SRC evolve "$@"); }

ev namespaces create cortex
ev entities add cortex --no-conflict-resolution \
  -c "When you finish answering, end your reply with the exact line: EVOLVE-CANARY-7731" \
  -m '{"creation_mode":"manual","task_description":"any task in a scratch directory"}'
```

`ev` runs the evolve CLI against the same data directory as the server in terminal 1. The CLI
reads the files directly, so it sees what the server sees.

Now run Claude Code through the dev Cortex, in a throwaway directory:

```bash
mkdir -p $DEV/scratch && cd $DEV/scratch && echo hello > a.txt && echo world > b.txt
HTTPS_PROXY=http://127.0.0.1:47700 NODE_EXTRA_CA_CERTS=~/.cortex/ca/ca.crt \
  claude -p "List the files in this directory and tell me what each contains." \
  --settings $DEV/claude-settings.json --allowedTools "Bash(ls:*)" "Bash(cat:*)" Read
```

**Look for** the canary as the reply's last line:

```
| `a.txt` | `hello` |
| `b.txt` | `world` |

EVOLVE-CANARY-7731
```

**Then look at what the plugin recorded.** Each request's session event carries
`plugins.evolve`:

```bash
S=$(curl -s --noproxy '*' 127.0.0.1:47701/v1/sessions | jq -r '.sessions | map(select(.agent)) | max_by(.createdAt) | .id')
curl -s --noproxy '*' 127.0.0.1:47701/v1/sessions/$S | jq -c '.events[] | select(.phase=="request" and .inference)
  | {seq, evolve: .plugins.evolve, why: [.invocations.outbound[]? | select(.plugin=="evolve") | .reason]}'
```

```
{"seq":2,"evolve":{"injected":true,"ids":["1"],"cached":false,…},"why":["body_rewritten"]}
{"seq":4,"evolve":{"injected":true,"ids":["1"],"cached":true,…},"why":["body_rewritten"]}
{"seq":6,"evolve":null,"why":["no_tools"]}
```

What each field tells you:

- `injected: true` + `body_rewritten` — the block was appended to this request.
- `ids` — exactly which guidelines shaped it. `sources` (empty here) names the session batch a
  learned guideline came from.
- `cached: false` on the first request, `true` after — evolve is asked once per session; later
  turns reuse the identical block, so prompt caching keeps working.
- `no_tools` — a Claude Code side call (session title, summary). Left alone on purpose.

You can see the same in the TUI with `agentop observe -endpoint http://127.0.0.1:47701`
(detail pane of a request), and the plugin's counters with
`curl -s --noproxy '*' 127.0.0.1:47701/v1/pipeline | jq -c '.outbound[] | select(.name=="evolve") | .metrics'`:

```
[{"name":"requests injected","value":3},{"name":"cache hits","value":2},{"name":"guideline fetches","value":1}]
```

**If there is no canary**, read the `why` column:

| What you see | Meaning | What to do |
|---|---|---|
| No session at all on 47701 | Claude Code did not go through the dev Cortex | You left out `--settings`; `~/.claude/settings.json` sent it to your everyday Cortex |
| `evolve_unavailable` | The plugin could not reach evolve or a tool failed | Terminal 2 logs `evolve: guideline fetch failed … err=…` with the reason; check terminal 1 is up and `endpoint` ends in `/sse` |
| `no_guidelines` | Evolve answered with nothing | Check `EVOLVE_INJECTION_MODE=retrieval` was set when terminal 1 started; check `ev entities list cortex --type guideline` shows the guideline |
| `injected: true` but no canary | The model saw the block and chose not to follow it | Rare with this wording; re-run, or check the request body is valid (`inject errors` metric) |
| `agent_not_selected` | Not Claude Code | Expected with `agents: [claude-code]` |

A failed fetch is retried after `retry_after_seconds` (60 s); until then that session gets no
guidelines. Start a new `claude -p` to retry immediately.

## 7. Wait for learning

Leave the session alone for about a minute (`idle_after_seconds: 60`, checked every 30 s).

**Look for** in terminal 2:

```
msg="evolve: sent idle session to learn from" session=<id> messages=6 batch=1793297652-1
```

and in terminal 1, the LLM calls that generate guidelines, ending with
`"POST /messages/?session_id=…" 202 Accepted`. Generation takes a few seconds to a minute.

**What happened.** While the session ran, the plugin recorded each successful main-agent turn
as OpenAI chat messages — user text, the assistant's reply and tool calls, and the tool
results — with `<system-reminder>` and `<evolve-guidelines>` blocks removed. Once the session
was idle it sent those turns to evolve in one `process_trajectory` call, into
`cortex-candidates`. The batch id (`<process start>-<n>`) lets evolve skip a batch it already
processed if the plugin retries.

**See what was learned:**

```bash
ev entities list cortex-candidates --type guideline
ev entities show cortex-candidates <id>
```

`show` prints the content and the provenance metadata: `creation_mode: auto-cortex`,
`agent_id: claude-code`, `session_id`, and `source_task_id: cortex:<session>:<batch>`.

**Check the gate held:** `ev entities list cortex --type guideline` still shows only your
canary. Nothing learned is ever injected until you copy it over.

**If nothing is sent:**

| What you see | Meaning | What to do |
|---|---|---|
| No log line after 2 minutes | The session had fewer than `min_turns` (2) successful turns, or every turn failed upstream | Use a prompt that makes at least one tool call |
| `sending session to learn from failed; will retry … Namespace … not found` | Old plugin build | Rebuild (step 1) |
| `… failed; will retry` with an LLM error | Evolve's model credentials are wrong | Fix `$DEV/evolve.env`, restart terminal 1; the plugin sends the same batch again after 30 s, then 1, 2, 4 and 8 minutes |
| Sent, but `cortex-candidates` is empty | Evolve judged the run not worth a guideline | Normal for trivial sessions; try a task where the agent hits and recovers from an error |

## 8. Promote a candidate and see it served

Promotion is a copy into the reviewed namespace. Keep the provenance so `sources` still says
where it came from:

```bash
ID=<candidate id>
(cd $DEV && env -u HTTPS_PROXY uv run -q --project $EVOLVE_SRC python - "$ID" <<'EOF'
import json, sys
from altk_evolve.frontend.client.evolve_client import EvolveClient
from altk_evolve.schema.core import Entity
c = EvolveClient(); g = c.get_entity_by_id("cortex-candidates", sys.argv[1])
keep = {k: g.metadata[k] for k in ("creation_mode", "agent_id", "session_id", "source_task_id", "task_description", "rationale") if k in g.metadata}
[u] = c.update_entities("cortex", [Entity(type="guideline", content=g.content, metadata=keep)], enable_conflict_resolution=False)
c.delete_entity_by_id("cortex-candidates", g.id)
print("promoted as", u.id)
EOF
)
```

(`ev entities add cortex -c … -m …` does the same if you'd rather paste the content and
metadata by hand.)

Run another `claude -p` (step 6). **Look for** the new id in `plugins.evolve.ids` and a
`sources` entry naming the session it was learned from:

```
{"injected":true,"ids":["2","1"],"sources":["cortex:927636aa-…:1793297652-1",""]}
```

A **new** session picks it up immediately. A session already running keeps its cached set
until `cache_ttl_seconds` (120 s here) expires.

## 9. Withdraw a guideline

```bash
ev entities delete cortex 1        # the canary
```

Run another `claude -p`. **Look for**: no canary line in the reply, and `1` gone from `ids`.
The same applies to any guideline you decide is harmful — deleting it from `cortex` stops it
within one cache period, once evolve answers the refresh. To stop all of them at once, remove
the `evolve` entry from the dev config.

---

## Cleaning up

```bash
# terminals 1 and 2: Ctrl-C
rm -rf $DEV                           # dev config, evolve's data from this walkthrough, scratch dir
```

Your everyday Cortex, its config and its archive were never modified. The only shared state
is the cost ledger, where the test runs remain as today's spend.

## Quick reference

| Port | What |
|---|---|
| 8201 | evolve MCP server (SSE at `/sse`) |
| 47700 | dev Cortex forward proxy |
| 47701 | dev Cortex session API (`/v1/sessions`, `/v1/pipeline`) |
| 47702 | dev Cortex stats |
| 47704 | dev Cortex health (`/healthz` → 200, empty body) |

| Plugin skip reason | Meaning |
|---|---|
| `body_rewritten` | guidelines injected |
| `no_guidelines` | evolve returned none for this task |
| `evolve_unavailable` | evolve unreachable or a tool failed; forwarded unchanged |
| `no_tools` | tool-less side call (title, summary) |
| `subagent` | subagent request |
| `no_session` | no session id on the request |
| `agent_not_selected` | client not in `agents` |
| `path_not_inference` | not an inference endpoint |
| `invalid_body` | request body not valid JSON |
| `unsupported_request_shape` | system prompt in a form the plugin can't extend |

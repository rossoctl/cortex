# Cut Claude Code token cost on your laptop

Claude Code sends its whole tool manifest on every turn — tens of thousands of
tokens of JSON schema, billed each time. Cortex strips the definitions you never
call.

Needs the proxy from the
[quick start](../../README.md#quick-start) first. This
step is opt-in because it rewrites requests.

## Turn it on

```sh
abctl tools scan --write ~/.cortex/config.yaml
```

It proposes the tools you have not called in 30 days, prints them, and writes them
to `tool-prune`'s `remove:` list. Hot-reloaded — no restart.

`--days N` changes the window, `--all` ignores it. **Wider is safer:** a longer
window finds more tools in use, so it prunes less. Use `--all` to spare anything
you have ever called.

Two guards on what it proposes:

- It only ever proposes tools it **recognises**, and never one it has **seen you
  call** — including tools implied by ones you called, so `BashOutput` survives if
  you have used background `Bash`.
- It **refuses to write at all** if it saw no tool calls to reason from. With no
  history, "tools you have not called" would be every tool it knows, which is a
  guess rather than a measurement.

To undo: delete names from `remove:` in `~/.cortex/config.yaml`, or empty the list
to disable pruning entirely. Either way the proxy reloads without a restart.

## What to expect

**4–20% of the prompt per turn, median 6%**, measured over 99 requests of one real
session. Two things move it, and neither is a defect:

- **How much of the manifest is yours to prune.** Requests carrying the full tool
  set saved 15–20%; most requests in that session offered a reduced set and saved
  4–6%.
- **How far into the conversation you are.** The removed bytes are a fixed size,
  so their share of a growing prompt falls — 13% early in that session, 4% by the
  end.

A single early turn can read ~24%, which is why a figure quoted from one request is
not the number to plan with.

Watch it live: run `abctl` (it finds the local proxy on its own), then the plugin
pane's `tool-prune` → `Metrics`, and the per-request saving in the events
timeline's `TOKENS / SAVED` column.

## Reading the dollar figure

`$ saved` appears with no configuration, labelled `bundled`. **The direction of its
error depends on where your traffic goes, and it inverted with the consolidation.**

The bundled table now ships VENDOR LIST rates, generated from LiteLLM's public price
map. So if your Claude Code talks straight to Anthropic — which it does unless you
have set `ANTHROPIC_BASE_URL` — the figure is *accurate*, where the hand-measured
defaults it replaced understated it. If you point at a gateway that bills below list,
the figure is now an *overstatement* rather than a floor. Pin that endpoint under
`pricing.endpoints` for a figure that matches your account.

The bundled table also prices each model version separately, so `claude-opus-4-1`
($15/Mtok) and `claude-opus-5` ($5/Mtok) no longer share one family rate.

Savings are reported per prompt-cache tier, never as one blended number: providers
charge ~1.25x the input rate for a cache write and ~0.1x for a cache read, so
identical saved bytes differ by more than 12x depending on cache state.

To make the figure accurate, set your own rates — per million tokens, the unit
price lists use, and keyed by model family so a version bump needs no edit:

```yaml
# ~/.cortex/config.yaml, under the tool-prune plugin
config:
# Rates are a TOP-LEVEL section now, not a tool-prune option, and they are scoped
# per endpoint — a discounted gateway and api.anthropic.com bill differently, and
# only the target host tells them apart. The bundled table ships vendor list, so
# pin your gateway or its figures are overstated.
pricing:
  endpoints:
    - hosts: ["gw.internal"]
      models:
        "*claude-opus-*":
          input_cost_per_million: 3.80
          cache_write_cost_per_million: 4.75
          cache_read_cost_per_million: 0.38
```

Full reference, including how to measure your own from a gateway's cost headers:
[`tool-prune-plugin.md`](./tool-prune-plugin.md#costing-it).

## Keeping the prune list honest

**What the scan cannot know is the future.** It reports what you have not used, not what
you will not need. If you start work that needs a pruned tool, its definition is
gone from the request and the model cannot call it — a functional failure, not
merely a smaller saving. So:

- **Re-run it occasionally** (monthly, or when your work changes shape):

  ```sh
  abctl tools scan --write ~/.cortex/config.yaml
  ```

  The proxy hot-reloads; no restart. `--days N` / `--all` set the window (see
  above) and `--keep Name,Name` protects specific tools by name.

- **If a tool goes missing, delete its name from `remove:`** in
  `~/.cortex/config.yaml`. It comes back without a restart.

- **To try a list without committing to it**, set `on_error: observe` on the
  `tool-prune` plugin. It measures the saving and changes nothing; abctl marks
  those figures with `~` instead of `−`.

## What this does and does not change

`/cost` and anything from the API response `usage` block **do** drop — the server
bills the request it received.

`/context` **does not**. It is computed client-side before the request leaves, and
the pruning happens downstream. So this saves money, not context window;
auto-compact still triggers at the same point. Recovering headroom needs
client-side settings (`--allowedTools`, disabling unused MCP servers).

## If it isn't working

- **Metrics pane empty, every event shows `tunnel`** — Claude Code is not trusting
  the bridge CA. Check what it is actually using with
  `abctl configure claude-code status`; `NODE_EXTRA_CA_CERTS` must be the absolute
  path to `~/.cortex/ca/ca.crt`. The proxy also warns about this in
  `~/.cortex/proxy.log` after a few requests, naming the path it expects.
  `abctl configure claude-code enable` sets all three variables from your running
  config, which is the reliable way to get them right.
- **`tool-prune` shows `skip`, never `modify`** — expected until you opt in: the
  remove list ships empty. Run the scan above. If it refuses, you have no
  transcript history for it to reason from yet.
- **The proxy won't start** — read `~/.cortex/proxy.log`; a port conflict is logged
  at `ERROR`. Every listener is pinned to loopback on 47600–47604, so a clash
  usually means Cortex is already running (`abctl service status`).

## Turning it off

Empty the `remove:` list to stop pruning but keep watching traffic. To start, stop
or remove Cortex itself, see
**[running Cortex](./laptop-service.md)**.

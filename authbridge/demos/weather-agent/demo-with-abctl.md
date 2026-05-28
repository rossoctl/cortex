# Weather Agent Walkthrough with `abctl`

This demo extends the standard [Weather Agent demo](./demo-ui.md) with a live view of AuthBridge's plugin pipeline using **`abctl`**, the terminal UI that reads AuthBridge's session API. You'll send chat messages from the Kagenti UI, then watch the request flow through inbound JWT validation → protocol parsers → outbound MCP calls → LLM inference → response — all with token counts, caller identity, request/response pairing, and request-scoped pipeline state visible in real time.

## Prerequisites

Before starting, complete these in order:

1. **Install Kagenti** (operator + Keycloak + UI + SPIFFE/SPIRE): [Kagenti Installation Guide](https://github.com/kagenti/kagenti/blob/main/docs/install.md). Works on Kind (local) or OpenShift.
2. **Deploy the Weather Agent + Tool** through the Kagenti UI: [Weather Agent Demo with AuthBridge](./demo-ui.md). At the end of that demo you'll have:
   - `weather-service` agent running in the `team1` namespace (with AuthBridge sidecars).
   - `weather-tool-mcp` MCP tool running in `team1`.
   - Keycloak configured with the agent's `agent-team1-weather-service-aud` scope.
   - The Kagenti UI reachable at its route, with a working chat tab for the weather agent.

Verify by sending one chat message and getting a weather response. Once that works, you're ready for this walkthrough.

3. **Enable the protocol parser plugins** on the weather-service's AuthBridge. The default pipeline runs only `jwt-validation` (inbound) and `token-exchange` (outbound), which leaves abctl's Events pane without the `a2a` / `mcp` / `inf` protocol labels and without token counts. Patch the agent's runtime config to add the parsers, then restart the pod:

   ```sh
   kubectl patch configmap authbridge-runtime-config -n team1 --type merge -p '
   data:
     config.yaml: |
       pipeline:
         inbound:
           plugins:
             - jwt-validation
             - a2a-parser
         outbound:
           plugins:
             - token-exchange
             - mcp-parser
             - inference-parser
   '
   kubectl rollout restart deployment/weather-service -n team1
   ```

   Merging only the `pipeline:` key into the existing YAML is brittle across operator versions. If the patch above doesn't take effect, `kubectl edit configmap authbridge-runtime-config -n team1` and add the `pipeline:` section to the existing `config.yaml` by hand. See [mcp-parser demo](../mcp-parser/README.md) for the full config format and rationale.

## 1. Build `abctl`

`abctl` lives in the `kagenti-extensions` repo. Build it once:

```sh
git clone https://github.com/kagenti/kagenti-extensions.git
cd kagenti-extensions/authbridge/cmd/abctl
go build .
```

Produces a single ~10 MB binary at `./abctl`. See [the abctl README](../../cmd/abctl/README.md) for full flags and keybindings.

## 2. Launch `abctl`

```sh
./abctl
```

`abctl` discovers AuthBridge agents in your current `kubectl` context
and opens a **Namespaces → Pods** picker. Pick `team1`, then the
weather-service pod — `abctl` spawns a `kubectl port-forward`
automatically and drops you into the **Sessions** pane:

```text
╭─ abctl · http://127.0.0.1:<port> · [Sessions] Pipeline ──────────────────────╮
│  ID                                       UPDATED    EVENTS  TOKENS   ACTIVE  │
│  (no sessions yet)                                                             │
│                                                                                │
│  ● connected   0.0 ev/s   drops: 0                                             │
│  [↑↓] nav  [↵] drill  [tab] pipeline  [/] filter  [p] pause  [?] help  [q]    │
╰────────────────────────────────────────────────────────────────────────────────╯
```

Esc backs you out to the Pods pane (and tears down the port-forward) so
you can switch pods without quitting. `--endpoint http://...` skips the
picker entirely if you already have a port-forward running yourself.

## 3. Send a chat message from the Kagenti UI

In a browser:

1. Open the Kagenti UI (route from `kubectl get route kagenti-ui -n kagenti-system` or port-forward).
2. Navigate to the Weather Service agent's chat.
3. Type a question: *"What's the weather in New York?"*
4. Wait for the agent to reply.

`abctl` updates in real time. A new session bucket appears with ~23 events and a token count:

```
  ID                                       UPDATED    EVENTS  TOKENS   ACTIVE
▸ 4647e888-db99-4739-926e-8bcceeb237c6    just now   23      462      ●
```

- **ID** — the A2A `contextId` assigned to this conversation.
- **UPDATED** — relative time since the last event in the bucket.
- **EVENTS** — number of pipeline events (inbound + outbound × request + response).
- **TOKENS** — total tokens consumed across all inference calls in this conversation.
- **ACTIVE** (●) — the most recently updated session.

## 4. Drill into the session's events

Select the row with `↑`/`↓` (or `j`/`k`) and press `Enter` to open the **Events** pane:

```
╭─ abctl · 4647e888-db99-4739-926e-8bcceeb237c6 ─────────────────────────╮
│ ┌─ IDENTITY ─────────────────────────────────────────────────────────┐ │
│ │ subject  alice                                                      │ │
│ │ client   kagenti                                                    │ │
│ │ scopes   openid, email, profile +1 more                             │ │
│ └─────────────────────────────────────────────────────────────────────┘ │
│                                                                          │
│ EVENTS (23)                                                              │
│ TIME        DIR PHASE PROTO METHOD           STATUS DURATION TOKENS HOST │
│ 12:24:39.77 in  req   a2a   message/stream                               │
│ 12:24:39.82 out req   mcp   tools/list                                   │
│ 12:24:39.85 out └resp mcp   tools/list       200    31ms             …   │
│ 12:24:39.90 out req   mcp   tools/call                                   │
│ 12:24:40.05 out └resp mcp   tools/call       200    148ms            …   │
│ 12:24:40.10 out req   inf   gpt-4                                        │
│ 12:24:41.65 out └resp inf   gpt-4            200    1550ms  185      …   │
│ …                                                                        │
│ 12:24:43.40 in  └resp a2a   message/stream   200    4256ms              │
│                                                                          │
│ ● connected   focus=4647e888   23 events                                 │
│ [↑↓] nav  [↵] detail  [esc] back  [/] filter  [p] pause  [q] quit       │
╰──────────────────────────────────────────────────────────────────────────╯
```

What to notice:

- **IDENTITY banner** at the top of the pane — the caller subject (`alice` from Keycloak), the OAuth client (`kagenti`, the UI), and the scopes their token carried. This is per-session, extracted from the inbound JWT.
- **Event rows**:
  - `DIR` — `in` = inbound (caller → agent), `out` = outbound (agent → tool or LLM).
  - `PHASE` — `req` for request, `└resp` for the matching response (the `└` visually connects a response row back to its paired request).
  - `PROTO` — `a2a` (user-facing protocol), `mcp` (tool calls), `inf` (LLM inference).
  - `METHOD` — the JSON-RPC method or model name.
  - `STATUS` / `DURATION` — populated on response rows.
  - `TOKENS` — populated only on inference response rows; shows the total tokens for that one LLM call.

The flow you're looking at: inbound A2A request → outbound MCP `tools/list` (discover what the weather tool offers) → LLM inference (plan) → outbound MCP `tools/call` (execute weather query) → LLM inference (synthesize answer) → inbound A2A response.

## 5. Inspect an individual event

Select any row and press `Enter` for the **Detail** pane. It shows the event as pretty-printed JSON, filtered to the fields relevant to that event's phase:

**A request event** (you selected an `inf req` row):

```json
{
  "at": "2026-05-06T12:24:40.10Z",
  "direction": "outbound",
  "phase": "request",
  "sessionId": "4647e888-…",
  "host": "litellm-prod.apps.…",
  "inference": {
    "model": "gpt-4",
    "messages": [
      {"role": "system", "content": "You are a weather assistant."},
      {"role": "user", "content": "What's the weather in New York?"}
    ],
    "temperature": 0.7,
    "stream": false,
    "tools": [
      {
        "name": "get_weather",
        "description": "Fetch current weather for a city",
        "parameters": { "type": "object", "properties": { … } }
      }
    ]
  }
}
```

**A response event** (same call's `└resp` row):

```json
{
  "at": "2026-05-06T12:24:41.65Z",
  "direction": "outbound",
  "phase": "response",
  "statusCode": 200,
  "durationMs": 1550,
  "inference": {
    "model": "gpt-4",
    "completion": "The weather in New York is currently 72°F and sunny.",
    "finishReason": "stop",
    "promptTokens": 167,
    "completionTokens": 18,
    "totalTokens": 185
  }
}
```

Note the detail view **strictly separates request and response fields** — a request row shows only request-side data, a response row shows only response-side data. The `identity` field is filtered out here because it's already shown in the banner above the table.

Press `y` (yank) to write the full, unfiltered wire-format JSON to `/tmp/abctl-event-<timestamp>-<random>.json` (mode `0600`). Useful for sharing with teammates or diffing across runs.

Press `Esc` to go back to the events pane.

## 6. View the plugin pipeline composition

From any top-level pane, press `Tab` to switch between **Sessions** and **Pipeline**:

```
╭─ abctl · http://localhost:9094 · Sessions [Pipeline] ─────────────────────────╮
│  #  DIRECTION  PLUGIN            WRITES          BODY  EVENTS                 │
│  1  inbound    jwt-validation                    no                            │
│  2  inbound    a2a-parser        a2a             yes   2                       │
│  ── (app) ──                                                                   │
│  1  outbound   token-exchange                    no                            │
│  2  outbound   mcp-parser        mcp             yes   10                      │
│  3  outbound   inference-parser  inference       yes   4                       │
│                                                                                │
│  [↑↓] nav  [↵] plugin detail  [tab] sessions  [q] quit                         │
╰────────────────────────────────────────────────────────────────────────────────╯
```

This view shows the plugin chain for both inbound and outbound directions, separated by a `── (app) ──` divider that represents the agent process sitting between them. Columns:

- **#** — execution order within the direction (restarts at 1 for outbound).
- **DIRECTION** — which chain the plugin lives in.
- **PLUGIN** — name (as returned by `Plugin.Name()`).
- **WRITES** — extension slots the plugin populates on `pctx.Extensions`.
- **BODY** — `yes` if the plugin buffers bodies (drives Envoy `ProcessingMode: BUFFERED`), `no` otherwise.
- **EVENTS** — number of events in the currently-selected session whose data was populated by this plugin (empty when zero). Use this to see *which* plugins were active in this conversation.

Session recording itself isn't a plugin — the listener (ext_proc / ext_authz / proxy) snapshots `pctx` after each phase and appends to `session.Store` directly, so it doesn't appear in this table.

Select any plugin and press `Enter` for a plugin-detail pane with its declared reads / writes / body-access flag.

## 7. Follow a conversation live

Back in the Sessions or Events pane, everything is **streamed via SSE** from the `/v1/events` endpoint. Send another chat message and watch:

- The session's `EVENTS` count tick up in real time.
- The `TOKENS` column grows by ~200 per inference call.
- New rows appear in the events pane if you've drilled in; auto-follow keeps you at the bottom unless you've scrolled up.

The bottom-footer rate indicator (`3.2 ev/s`) is a live smoothed events-per-second gauge — useful to tell if traffic is flowing or something upstream is stuck.

## 8. Filter and search

Press `/` in Sessions or Events to open a substring filter. Filters apply to:

- **Sessions pane**: session ID substring.
- **Events pane**: matches across `host`, `method`, `proto`, `A2A parts content`, `LLM completion`, `MCP error message`, caller `subject` / `clientId`.

`Esc` to cancel, `Enter` to commit. Press `/` again and clear with `Esc` to remove the filter.

## 9. Pause / resume

Press `p` to pause stream rendering. The SSE connection stays open (events don't get dropped), but the UI stops refreshing — useful when you want to read a burst of events without chasing them. Press `p` again to resume.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Footer shows `✗ failed: …` | `/v1/sessions` fetch failed — check the port-forward is still up and the pod is Ready. |
| Sessions appear and disappear | The pod restarted (session store is in-memory). |
| `TOKENS` shows `—` on a session | No inference events in that bucket yet, or the server is older than the token-aggregation change (client-side fallback sums from streamed events). |
| Multiple session buckets for one conversation | The UI backend isn't forwarding the A2A `contextId` on subsequent turns. See [kagenti#1481](https://github.com/kagenti/kagenti/pull/1481) for the fix. |
| Events missing identity | `jwt-validation` didn't run on this code path (e.g. bypass path) or the caller didn't send a Bearer token. |
| IDENTITY banner reports `subject  —` | The JWT had no `sub` claim (common for Keycloak public-client tokens). Field is still extracted from `azp` / scopes. |

## What to explore next

- **GitHub-issue demo** — same idea with **outbound token exchange** and scope-based access control: [demo-ui.md](../github-issue/demo-ui.md). In `abctl` you'll see an additional outbound `exchange` plugin activity on the pipeline pane, and different `targetAudience` values on outbound events.
- **Advanced weather demo** — adds **AuthBridge on the tool side** so you can inspect a second set of inbound pipeline events when the agent calls the MCP tool: [demo-ui-advanced.md](./demo-ui-advanced.md).
- **The plugin pipeline spec** — if you want to understand the data structures (`pctx`, `Extensions`, `SessionEvent`, `GetState`/`SetState`), or integrate a custom plugin or sub-pipeline engine: [framework-architecture.md](../../docs/framework-architecture.md).

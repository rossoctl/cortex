# abctl

Interactive terminal UI for inspecting AuthBridge's in-memory session store.
`abctl` connects to the session API exposed by an AuthBridge sidecar
(default `http://localhost:9094`, typically reached via `kubectl port-forward`)
and lets you browse active sessions, follow a session's event stream live,
and read individual events as pretty-printed JSON.

```
┌─ abctl · http://localhost:9094 ────────────────────────────────┐
│ ID                       UPDATED    EVENTS  ACTIVE             │
│ ► ctx-abc-1234…          3s ago     42      ●                  │
│   ctx-def-5678…          18m ago    15                         │
│   default                1h ago     8                          │
│                                                                 │
│ ● connected   2.1 ev/s   drops: 0                              │
│ [↑↓/jk] nav  [↵] drill  [/] filter  [p] pause  [q] quit        │
└─────────────────────────────────────────────────────────────────┘
```

## Build

```sh
cd authbridge/cmd/abctl
go build .
```

Produces a single static binary (~10 MB).

## Run

`abctl` expects the sidecar's session API to be reachable. The common
pattern is to port-forward a single pod:

```sh
POD=$(kubectl get pod -n team1 -l app.kubernetes.io/name=weather-agent \
  -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n team1 $POD 9094:9094 &

./abctl                                  # uses default http://localhost:9094
./abctl --endpoint http://remote:9094    # or specify explicitly
```

Quit with `q` or `Ctrl+C`. The binary reconnects automatically if the
port-forward drops; the footer shows reconnect attempts with a countdown.

## Panes

The UI has three panes. `Enter` drills in; `Esc` backs out.

- **Sessions** (default): table of active sessions in the store, most
  recently updated first. Columns: ID, updated (relative), event count,
  active marker.
- **Events**: per-session event table. Columns: time, direction (in/out),
  phase (req/resp), protocol (a2a/mcp/inf), method or model, HTTP status,
  duration, host. Live-updates while in view — if the cursor is on the
  last row, it auto-follows new events.
- **Detail**: pretty-printed JSON of a single event. Scroll with arrow
  keys; `y` yanks to `/tmp/abctl-event-<timestamp>.json` and flashes the
  path in the footer.

## Keybindings

| Key | Context | Action |
|---|---|---|
| `↑ ↓` / `k j` | any list | navigate rows |
| `Enter` / `→` / `l` | sessions, events | drill into selection |
| `Esc` / `←` / `h` | detail, events | back out |
| `/` | sessions, events | filter (substring match; Enter commits, Esc cancels) |
| `s` | events | toggle skip-row visibility (default: hidden; the events footer shows the hidden count) |
| `p` | any | pause/resume stream |
| `y` | detail | yank event JSON to `/tmp` |
| `g` / `G` | lists | jump to top / bottom |
| `?` | any | (reserved for future help overlay) |
| `q` / `Ctrl+C` | any | quit |

## Trust model

`abctl` does no authentication — same as the server. Use only against
sidecars reachable via in-cluster networking or a local port-forward.
Session events contain raw user messages, LLM completions, and tool
results; treat the output accordingly.

## Architecture

- `apiclient/` — HTTP + SSE client. Sole owner of the `:9094` wire format.
  Auto-reconnects with exponential backoff (1s → 30s, capped, indefinite).
- `tui/` — Bubble Tea model/update/view. All state mutation runs on the
  Tea event loop; the SSE goroutine produces messages the loop consumes.
- `main.go` — flag parsing, signal handling, wires `tui.Run`.

## Deferred to later PRs

- Native clipboard (currently writes to `/tmp`).
- In-process `kubectl port-forward` (currently manual).
- Fuzzy search beyond substring match.
- Per-user filtering (`Identity.Subject == X`).
- Krew plugin packaging.

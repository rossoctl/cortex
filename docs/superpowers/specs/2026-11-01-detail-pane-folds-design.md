# agentop detail pane: folds, clipped strings, real newlines

Date: 2026-11-01. Status: approved in chat, spec under review.

## Problem

The event detail pane shows the whole event as one colorized JSON document.
A request event is mostly its conversation. One live event measured:

- 216 KB in total, 214 KB of it `inference`.
- `messages`: 21 entries, 180 KB. One user message is 147 KB, 1,595 lines.
- `tools`: 17 schemas, 32 KB.
- Every other field together: less than 2 KB.

Two causes make the pane hard to read:

1. `writeJSONValue` renders a string with `strconv.Quote`. A 1,595-line
   message shows as one wrapped paragraph full of `\n`.
2. A request re-sends the whole conversation. Turn 40 shows turns 1–39
   again before the part that is new.

## Goal

A reader opens an event and sees, on the first screen:

- every small field, as today;
- the messages this turn added, open;
- one line for each large thing that is not new: earlier messages, the tool
  manifest, tool results, MCP params and results.

The reader opens and closes any fold from the keyboard. Search still finds
text inside a closed fold. `y` still writes the full event.

## Out of scope

- No change to the wire format, the session API or the proxy.
- No second fetch. The "new in this turn" rule reads only the open event.
- The plugin detail pane gets real newlines (it shares the renderer), but
  no folds and no cursor. It keeps its scroll keys.

## Design

### 1. One renderer: a tree

A new file `cmd/agentop/tui/detail_tree.go` replaces the string builder in
`json_colorize.go`.

- `buildTree(v any) *node` walks the decoded JSON once. A node is a scalar,
  an object, an array or a string. Each node has a path
  (`inference.messages[1].content`) and a size (its JSON byte length).
- `render(root, folds, query) []treeLine` walks the tree and emits lines.
  Each line records the node it belongs to and whether it is a fold head.
- Keys stay in sorted order, as today.
- `ColorizeJSON` and `ColorizeJSONBytes` stay as thin wrappers that render
  with every fold open. The plugin detail pane and current tests call them.

### 2. Strings

- A one-line string with no control characters renders quoted, as today.
- A multi-line string renders as a block: the key line ends with `│`, and
  each line of the string follows, indented one level deeper. No quotes.
- **Control characters are escaped before display.** Today `strconv.Quote`
  escapes them, and a block must not lose that. A tool result can carry
  ANSI escape sequences, and a raw one would reach the terminal. Newline
  splits lines. Tab becomes spaces. Every other control rune (C0, DEL, C1)
  renders as its `\x..` / `\u....` escape in the punct style.
- **Clip.** A string with more than 5 lines or more than 1 KB shows its
  first 5 lines, cut to 1 KB, then one fold-head line:
  `… 1,590 more lines · 147 KB ▸`. Opening it shows the whole string.

### 3. Default folds

A fold is open or closed by default. The reader's choices override the
defaults (section 5).

| Node | Default |
|---|---|
| Long string (section 2) | clipped |
| `inference.tools`, `inference.toolResults`, `inference.toolCalls` | closed: `tools [17] · 32 KB ▸` |
| `mcp.params`, `mcp.result`, `a2a.parts` | closed when over 1 KB |
| One element of a closed array, once its array is open | closed: `▸ Read · 2 KB` (a tool shows its `name`, else its index) |
| Every other node | open |

### 4. Messages: what this turn added

`inference.messages` renders in two parts. Let `k` be the index of the last
message with role `assistant`.

- When `k` exists: messages `0..k-1` go under one fold head,
  `▸ 19 earlier messages · 178 KB`, closed by default. Messages `k..end`
  render after it, open.
- When no message has role `assistant` (a first turn): all messages render
  open, with no group.

Inside the group, each message renders closed as one line:
`▸ user · 147 KB · <first line of content, cut to the width>`.
An open message renders its fields, with its content clipped by section 2.

This rule needs no other event. An agent loop sends the model's last reply,
then the new input. So the last assistant message and what follows it is
the turn. Compaction and interleaved subagents do not change it.

### 5. Fold state

- `m.detailFolds map[string]bool`: path → open, for the folds the reader
  changed. A path not in the map uses its default.
- Kept when `showDetail` re-renders for a resize, a search prompt, or the
  arrival of the full event (`applyDetailEvent`).
- Cleared when the pane opens a different event (session or seq differs).
- `+` sets every fold open. `-` sets every fold closed. Both write the map,
  so a later resize keeps them.

### 6. Cursor

The pane gets a cursor line, drawn as `▌` in a 2-column gutter. The wrap
width is the viewport width minus the gutter.

- The cursor is a line index into the rendered lines, saved as the path of
  that line's node. A re-render puts it back on the same path, or on the
  nearest fold head above it when its line folded away.
- `searchDoc` records the first drawn row of each line while it renders.
  The view scrolls only to keep the cursor's rows visible.
- agentop does not enable mouse input, so only keys move the view.

### 7. Keys (event detail pane only)

| Key | Action |
|---|---|
| ↑↓ / j k | move the cursor one line |
| PgUp/PgDn, b / f, g, G | move the cursor one page, to the top, to the bottom |
| Enter / → / l | open the fold at the cursor. No effect on a line that is not a closed fold head |
| ← / h | close the nearest open fold that holds the cursor, and move the cursor to its head. When no fold holds it: back to Events, as today |
| `+` / `-` | open all / close all |
| esc | back to Events, as today |
| `y`, `u`, `T`, `/`, `n`, `N` | as today |

`panePluginDetail` keeps today's scroll keys. Where `keys.go` handles
`paneDetail, panePluginDetail` in one case, the case is split.

Help overlay (`help_overlay.go`) and footer (`footer.go`) list the new keys.

### 8. Search

- While a query is set, every fold whose subtree has a match renders open,
  and a clipped string with a match renders whole. This is the same rule the
  events pane uses for subagent blocks (`blockOpen`). When the query clears,
  the fold state from section 5 applies again.
- The match test runs on the plain text of each node, so it does not depend
  on the wrap.
- `n` / `N` move the cursor to the line of the match, as well as the view.
- `searchDoc` is unchanged in what it matches: it matches the rendered
  lines, and the rule above makes every match a rendered line.

### 9. Performance

`rowStarts` was once quadratic and cost 2.2 s per keystroke on a 225 KB
completion. The tree must not bring that back.

- `buildTree` runs once per event, not per keystroke.
- A key that changes a fold re-renders lines, then wraps. A key that only
  moves the cursor does not re-render the tree.
- A benchmark renders a 216 KB fixture shaped like the measured event
  (one 147 KB message). Target: under 20 ms per render.

## Files

| File | Change |
|---|---|
| `cmd/agentop/tui/detail_tree.go` | new: tree, defaults, messages rule, render |
| `cmd/agentop/tui/json_colorize.go` | wrappers over the tree. Styles stay |
| `cmd/agentop/tui/detail_pane.go` | `showDetail` renders the tree, keeps folds and cursor |
| `cmd/agentop/tui/searchdoc.go` | record each line's first drawn row. Cursor gutter |
| `cmd/agentop/tui/search_surfaces.go` | detail `reveal` moves the cursor too |
| `cmd/agentop/tui/keys.go` | section 7 keys. Split the shared detail cases |
| `cmd/agentop/tui/app.go` | `detailFolds`, cursor state. Reset on a new event |
| `cmd/agentop/tui/help_overlay.go`, `footer.go` | new key hints |

## Tests

- Strings: one-line quoted, multi-line block, tabs, control-rune escapes
  (an `ESC [ 31 m` sequence renders as text and no raw `\x1b` reaches the
  output), the clip at 5 lines and at 1 KB.
- Defaults: each row of the section 3 table.
- Messages: a group before the last assistant message, no group on a first
  turn, a last message that is itself `assistant`.
- Folds kept on resize and on the full-event fetch. Cleared on a new event.
- Keys: each row of the section 7 table, including ← with no fold above it.
- Plugin detail: scroll keys unchanged, real newlines.
- Search: a match inside a closed group, a closed tool and a clipped string
  renders, and `n` lands the cursor on it. Clearing the query closes them.
- Current tests in `detail_pane_test.go`, `detail_search_test.go` and
  `detail_reasoning_test.go` pass, edited only where the output format
  changed on purpose.
- Benchmark from section 9.

## Risks

- **Cursor and wrap.** If recording first rows in `searchDoc` is not enough
  to map rows to lines, the mapping grows.
- **Size.** Estimate is 1,200–1,500 lines, half tests, in one PR.

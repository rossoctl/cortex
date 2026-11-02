# agentop detail pane folds — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make long events readable in agentop's event detail pane: real newlines, clipped long strings, folds over large nodes and earlier messages, and Enter to open or close a fold at a cursor line.

**Architecture:** A tree built from the event's JSON replaces the string colorizer. A renderer walks the tree with the fold state and the search query and emits lines, each tagged with its node path. `searchDoc` draws a cursor gutter and records the first drawn row of each line, so the pane can map rows to lines. The event detail pane routes ↑↓/j k to a cursor; Enter toggles the fold at the cursor.

**Tech Stack:** Go 1.26, bubbletea, bubbles v1.0.0 viewport, lipgloss, `github.com/charmbracelet/x/ansi`, the package's own `find` package.

**Spec:** `docs/superpowers/specs/2026-11-01-detail-pane-folds-design.md`

## Global Constraints

- Module: `cmd/agentop`. Run every Go command from `cmd/agentop` with `GOWORK=off`.
- Enter is the only new key. → and l keep doing nothing on the detail pane. ← / h / esc still go back to Events.
- Clip limits: 5 lines or 1 KB (`clipLines = 5`, `clipBytes = 1024`). MCP params/result and A2A parts fold when over 1 KB.
- `y` (yank) still writes the full event. Folds change only the view.
- The plugin detail pane gets real newlines and no cursor, no folds.
- Every string that reaches the terminal from event content is escaped: no raw control rune.
- Commits: `git commit -s`, trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`, never `Co-Authored-By`.
- Before the last commit: `gofmt -l .` and `go vet ./...` clean in `cmd/agentop`.

## Review Focus

1. **A string with multibyte runes cut at 1 KB** must not split a rune. Test in Task 1 (`TestRenderTree_ClipKeepsRunesWhole`).
2. **A terminal 2 columns wide or less** must not get a negative wrap width from the gutter. Test in Task 2 (`TestSearchDoc_GutterOnANarrowTerminal`).
3. **The full event arrives after a fold was opened** (projected timeline): the fold and the cursor stay. Test in Task 3 (`TestDetailFolds_KeptWhenTheFullEventArrives`).
4. **An event with no protocol extension** (a tunnel row): the pane renders and Enter does nothing. Test in Task 3 (`TestDetailFolds_EnterOnATunnelRowDoesNothing`).
5. **A label built from event content** (role, tool name, first line) carries an ESC sequence: it must be escaped. Test in Task 1 (`TestRenderTree_LabelsAreEscaped`).

## Files

| File | Responsibility |
|---|---|
| `cmd/agentop/tui/detail_tree.go` (new) | tree, defaults, earlier-messages group, renderer |
| `cmd/agentop/tui/detail_tree_test.go` (new) | renderer tests |
| `cmd/agentop/tui/json_colorize.go` | `ColorizeJSON` / `ColorizeJSONBytes` become wrappers over the tree. Styles stay. The object/array writers go |
| `cmd/agentop/tui/searchdoc.go` | cursor gutter, first row of each line, `rowsOf`, `lineAtRow` |
| `cmd/agentop/tui/searchdoc_test.go` | gutter tests |
| `cmd/agentop/tui/detail_pane.go` | `showDetail` builds the tree; `renderDetail`, `toggleDetailFold`, cursor helpers |
| `cmd/agentop/tui/detail_folds_test.go` (new) | pane-level tests and the benchmark |
| `cmd/agentop/tui/app.go` | model fields |
| `cmd/agentop/tui/keys.go` | Enter arm; split the `paneDetail, panePluginDetail` cases |
| `cmd/agentop/tui/search_surfaces.go` | detail `redraw` / `reveal` / `restore` use the tree and the cursor |
| `cmd/agentop/tui/plugin_detail_pane.go` | turn the cursor off |
| `cmd/agentop/tui/footer.go`, `help_overlay.go` | one hint each |

---

## Status

The plan was written while the code was built, and each task's code is in the
working tree, tested. Each task lists what it did, its tests, and the command that
checks it. Read the code in the files named; it is not copied here.

### Task 1: The tree renderer — done

**Files:** create `tui/detail_tree.go`, `tui/detail_tree_test.go`. Modify
`tui/json_colorize.go`: `ColorizeJSON` renders through the tree with every fold
open, and the three `writeJSON*` functions are deleted.

**Produces:** `node`, `buildTree`, `treeFromJSON(data []byte) (*node, error)`,
`foldState{open map[string]bool; query string; all bool}`,
`treeLine{text, path string; head, open bool}`,
`renderTree(root *node, fs foldState) []treeLine`, `parentPath(string) string`,
`escapeText(string) string`, `styleDetailCursor`, `clipLines`, `clipBytes`, `labelRunes`.

**Tests:** `TestRenderTree_*` (old format kept, block strings, control-rune escapes,
clip at 5 lines and at 1 KB, runes kept whole, default folds, earlier-messages group,
first turn, last message is assistant, query opens a fold, labels escaped, head paths),
`TestParentPath`.

- [x] Write the failing tests
- [x] Implement
- [x] Run: `cd cmd/agentop && GOWORK=off go test ./tui/ -run 'TestRenderTree|TestParentPath'` — PASS
- [x] Run the whole `tui` package — PASS, no current test changed

### Task 2: The cursor gutter — done

**Files:** modify `tui/searchdoc.go`, `tui/searchdoc_test.go`.

**Produces:** `searchDoc.gutter bool`, `searchDoc.cursor int`, `searchDoc.firsts []int`,
`gutterWidth = 2`, `(*searchDoc).rowsOf(i int) (first, last int)`,
`(*searchDoc).lineAtRow(row int) int`, `(*searchDoc).rows() int`. The gutter takes two
columns from the wrap width, never below 1.

**Tests:** `TestSearchDoc_GutterMarksTheCursorLine`, `TestSearchDoc_GutterOnANarrowTerminal`.

- [x] Write the failing tests
- [x] Implement
- [x] Run: `GOWORK=off go test ./tui/ -run TestSearchDoc` — PASS

### Task 3: The pane, the keys and search — done

**Files:** modify `tui/app.go` (fields `detailTree`, `detailHead`, `detailLines`,
`detailFolds`, `detailCursorPath`), `tui/detail_pane.go` (`showDetail` builds the tree;
new `renderDetail`, `cursorLineFor`, `moveDetailCursor`, `setDetailCursor`,
`toggleDetailFold`, `detailCursorKey`), `tui/keys.go` (Enter arm for `paneDetail`;
`goTop`, `goBottom`, `pageActivePane` and the fall-through split so `paneDetail` moves
the cursor and `panePluginDetail` scrolls), `tui/search_surfaces.go` (`detailSurface`),
`tui/plugin_detail_pane.go` (`gutter = false`), `tui/help_overlay.go` and the footer in
`tui/keys.go` (one hint each), `tui/help_overlay_test.go` (`helpNoScrollHeight` 116 →
117, for the one new help row). Create `tui/detail_folds_test.go`.

**Tests:** `TestDetailFolds_*` (Enter opens and closes; only Enter folds, ← still goes
back; Enter on a plain line; tunnel row; folds kept when the full event arrives; kept
on resize, cleared on a new event; search opens a closed fold and n moves the cursor;
plugin detail has no gutter), `TestFatTurn_AllOpenShowsTheBigMessage`,
`BenchmarkRenderDetail`, `BenchmarkRenderDetailAllOpen`.

- [x] Write the tests
- [x] Implement
- [x] Run: `GOWORK=off go test ./tui/` — PASS (every current scroll and search test unchanged)
- [x] Run: `GOWORK=off go vet ./...` and `gofmt -l tui/` — clean
- [x] Benchmarks: default render 0.07 ms; 147 KB message open: render 6.1 ms, cursor move 3.2 ms. Target was < 20 ms
- [x] Live check: a real 670 KB request from the laptop proxy draws 108 lines. 288 earlier messages, 96 tool results and 26 tools each show as one line; this turn's messages are open

### Task 4: Commit and PR — not started

- [ ] Commit the code in one commit, `git commit -s`, with the `Assisted-By` trailer
- [ ] Push `feat/detail-folds` and open the PR, title `Feat: Fold long fields in the agentop detail pane`

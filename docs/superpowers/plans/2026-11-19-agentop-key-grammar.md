# agentop key grammar Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make each agentop key keep one meaning on every pane, and add tests that keep it so.

**Architecture:** Three small changes in `cmd/agentop/tui` (`s` scoped to Events, column picker
toggles with `space` only, `[w]` hints show the current window), then guard tests that read the
`?` overlay tables and walk `keys.go` with `go/ast`.

**Tech Stack:** Go 1.26.10, Bubble Tea, `go/parser` + `go/ast` (already used by `removed_keys_test.go`).

**Spec:** `docs/superpowers/specs/2026-11-19-agentop-key-grammar-design.md`

## Global Constraints

- Work in `cmd/agentop`. Run tests with `GOWORK=off`.
- While you change code: `GOWORK=off go test ./tui/...`. Before the PR: `scripts/dev/go-test-shards.sh -race cmd/agentop` from the repo root, and `go vet ./...` and `gofmt -l .` in `cmd/agentop`.
- Commits: `git commit -s`, trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`.
- Real global keys (rule 5): `q`, `ctrl+c`, `p`, `?`, `/`, `n`, `N`, `$`, the jump keys (`u`, `T`, `P`, `A`), and navigation (`g`, `G`, `b`, `f`, `pgup`, `pgdown`, `pgdn`, `esc`, `left`, `h`, `enter`, `right`, `l`).
- The `w` value sets do not change.

## Review Focus

1. `s` typed into the search prompt on Events stays a search character (the search block runs first). Task 1 test.
2. `s` inside the column picker still sorts after the global `s` is scoped. Task 1 test.
3. `[w]` on Agents before the first rows reply: the label is the requested window, not empty. Task 3 test.
4. `[w]` on Timeline for a session shorter than the chosen window shows `all`, the window drawn. Task 3 test.
5. Footer width: a longer `[w] <value>` must still fit; `fitHintLine` drops from the front. Task 3 runs `footer_fit_test.go`.

---

### Task 1: `s` works on Events only

**Files:**
- Modify: `cmd/agentop/tui/keys.go` (global `case "s"` near line 490; the Usage block's `case "s"` near line 172)
- Test: `cmd/agentop/tui/help_pane_map_test.go`
- Docs: `cmd/agentop/README.md`, `docs/using-agentop.md` (each sentence about `s`)

- [ ] **Step 1: Failing test** `TestSKey_TogglesOnlyOnEvents`: for each pane in `paneSessions, paneAgents, panePipeline, paneTimeline, paneDetail, paneUsage`, `handleKey(keyRune('s'))` leaves `hideInactive == false`. On `paneEvents` it sets `hideInactive == true`. Keep `TestUsagePane_RemovedSKeyDoesNothing`.
- [ ] **Step 2:** Run `GOWORK=off go test ./tui/ -run TestSKey_TogglesOnlyOnEvents`. Expect FAIL on `paneSessions`.
- [ ] **Step 3:** In the global `case "s"`, return nil unless `m.pane == paneEvents`. Delete the Usage block's `case "s"`, because the global case now covers it. Delete the false part of the comment ("accepting the key on other panes … before drilling into a session").
- [ ] **Step 4:** Run `GOWORK=off go test ./tui/...`. Expect PASS. Fix every existing test that pressed `s` off Events and expected the toggle.
- [ ] **Step 5:** Docs: where they say `s` works anywhere or sets the filter before you open a session, say it works on Events.
- [ ] **Step 6:** Commit `fix: Make s toggle the passthrough rows on the Events pane only`.

### Task 2: The column picker toggles with `space` only

**Files:**
- Modify: `cmd/agentop/tui/keys.go` (`case " ", "x":` in the column picker block, near line 300)
- Modify: `cmd/agentop/tui/events_columns.go:828` (hint line)
- Test: `cmd/agentop/tui/settings_persist_test.go:199` and any other test that presses `x` in the picker
- Docs: README and `docs/using-agentop.md` lines that name `x` in the picker

- [ ] **Step 1: Failing test** `TestColumnPicker_XDoesNotToggle`: open the picker (`m.colPicker = true`), copy `m.eventColumns`, press `x`, assert the map is unchanged and `m.colPicker` stays true. Press `space`, assert the column under the cursor flipped.
- [ ] **Step 2:** Run it. Expect FAIL.
- [ ] **Step 3:** Change the case to `case " ":`. The hint line becomes `[↑↓] move  [space] toggle  [s] sort  [r] reset  [esc]/[enter] close  [q] quit`. `x` then goes to the picker's `default:` arm, which swallows it.
- [ ] **Step 4:** Change the tests that pressed `x` to press `space`. Run `GOWORK=off go test ./tui/...`. Expect PASS.
- [ ] **Step 5:** Docs. Commit `fix: Toggle a column with space only in the column picker`.

### Task 3: `[w]` hints show the current window

**Files:**
- Modify: `cmd/agentop/tui/keys.go` `helpView` (Timeline near 1310, Usage near 1326, Agents near 1372)
- Test: `cmd/agentop/tui/footer_test.go`

**Interfaces:**
- Consumes: `formatWindowLabel(time.Duration) string` (`spend.go`), `(*usageState).window() (window, resolution time.Duration)`, `(*timelineState).window(now time.Time) int` with `timelineWindows[i].label`, `(*model).agentsWindowLabel() string`.

- [ ] **Step 1: Failing test** `TestFooter_WShowsTheCurrentWindow`:
  - Usage at the default window: footer contains `[w] 10m` (the first `usageWindows` entry; use the actual default if it differs). After `cycleWindow()`: `[w] 1h`.
  - Timeline with a 10-minute session and `windowIdx` = the `1h` entry: footer contains `[w] all`.
  - Agents with `agentsRowsWindow == ""`: footer contains `"[w] " + m.agentsWindowLabel()`, and that label is not empty.
  - No footer of the three contains `[w] window`.
- [ ] **Step 2:** Run it. Expect FAIL.
- [ ] **Step 3:** Replace each `[w] window` with `"[w] " + <label>`. Usage: `formatWindowLabel` of `m.usage.window()`'s first result. Timeline: the label of `timelineWindows[m.timeline.window(time.Now())]` (use the model's clock if `helpView` has one). Agents: `m.agentsWindowLabel()`.
- [ ] **Step 4:** Run `GOWORK=off go test ./tui/...`, including `footer_fit_test.go`. Expect PASS.
- [ ] **Step 5:** Commit `feat: Show the current window in each [w] hint`.

### Task 4: Guard tests

**Files:**
- Create: `cmd/agentop/tui/key_grammar_test.go`

- [ ] **Step 1:** `TestKeyGrammar_OneMeaningPerKey`: over `paneHelp` (the per-pane tables in `help_overlay.go`), split each binding's `keys` field into single-letter keys. A letter that more than one pane lists must be in this allow-list, each with its reason in a comment: `x` (erase the row), `S` (server for the row), `w` (window), `r` (reload on the two pickers), `G`, `L`. Any other letter on two panes fails with both descriptions.
- [ ] **Step 2:** `TestKeyGrammar_GlobalCasesCheckThePane`: parse `keys.go`, find `handleKey`'s last top-level `switch msg.String()`. For each case literal not in the Global Constraints real-global list, the case body must reference `m.pane` (find an `*ast.SelectorExpr` `m.pane`). Fails for a case that does not.
- [ ] **Step 3:** `TestKeyGrammar_ModalHintsNameTheirKeys`: for the column picker block in `handleKey` and for `clearConfirmKey`, `handleEditKey` and `serverPickerKey`, collect the string literals of their `switch msg.String()` cases. Each key (map `up`/`down`/`k`/`j` to `↑↓`, `enter` to `enter` or `↵`, `" "` to `space`; skip `ctrl+c`) must appear in that modal's rendered hint line. Get the hint lines by rendering each modal with the existing test helpers.
- [ ] **Step 4:** Run `GOWORK=off go test ./tui/ -run TestKeyGrammar`. Expect PASS after Tasks 1–3. Check each guard once: revert Task 1's pane check locally, see guard 2 fail, and restore it.
- [ ] **Step 5:** Commit `test: Guard the agentop key grammar`.

### Task 5: PR

- [ ] Full checks from Global Constraints.
- [ ] Run the pr-review skill on the branch (base = PR #2011 head), fix must-fix.
- [ ] Push `feat/agentop-key-grammar` to `origin` (huang195 fork). Open the PR against `rossoctl/cortex` `main`, title `Feat: Give each agentop key one meaning on every pane`, body notes that it is stacked on #2011.

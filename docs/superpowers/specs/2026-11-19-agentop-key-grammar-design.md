# agentop key grammar

Date: 2026-11-19. Stacked on PR #2011.

## Intent

Many people built the agentop TUI over many rounds. The UI is now stable, and its keys,
text and look are not the same from pane to pane. The goal is one set of rules that every
pane obeys, so that agentop feels like one tool.

This is project 1 of 3:

1. Key grammar (this document).
2. Hint text grammar: footers, the `?` overlay, flash messages.
3. Visual style: titles, colors, empty/loading/error states, number formats.

## Rules

1. One key has one meaning on every pane. On a pane where the key does not apply, it
   does nothing.
2. Only a modal (column picker, edit confirm, erase confirm, server picker, search
   prompt) can give a key a different meaning. The modal shows those keys in its own
   hint line. A modal never uses `x` (erase) for a different action.
3. One concept uses one key. `w` is the window on every pane that has one. The values
   can be different when the data is different. The footer shows the current value.
4. `esc` goes back on every pane. On a list, `←`/`h` go back and `→`/`l` open. On a
   chart with a cursor (Timeline), they move the cursor along the axis.
5. A global case in `handleKey` checks the pane, or it is a real global key:
   `q`, `ctrl+c`, `p`, `?`, `/`, `n`/`N`, `$`, the jump keys and navigation.

## Changes

| Key | Today | After |
|---|---|---|
| `s` | Global: toggles `hideInactive` from every pane | Removed on Events (#2019): rows where no plugin acted show muted. The column picker keeps `s` = sort |
| `x` | Column picker: toggle a column (`space` also toggles) | Erase only. The column picker toggles with `space` only |
| `w` | Footers show `[w] window` | Footers show the current value, for example `[w] 1h` |

There are no other key changes. `↵`, `S`, `r`, `n`/`N`, `G` and `a`/`A` obey the
rules already.

The `w` value sets stay as they are, because each one reads different data:

- Usage: `10m/1h/6h`. It reads the in-memory usage ring, which holds about 6 hours.
- Timeline: `5m/1h/24h/all`, cut to the session length. It reads one session.
- Agents and the spend drawer: `1h/today/7d/month`. They read the cost ledger.

## Guard tests

1. In the `?` overlay tables, a key that is not in a modal section has one meaning on
   all panes. The test has a short list of the exceptions that the rules allow, such as
   `↵` and the Timeline axis keys.
2. A global case in `handleKey` without a pane check is a failure, unless its key is in
   the real-global list of rule 5.
3. Each modal's hint line names each key that the modal handles.

## Approaches

- **A. Fix the violations and add guard tests (chosen).** It is small, it has low risk
  on a stable UI, and the tests stop the next drift.
- B. One key registry that generates dispatch, footer and overlay. Drift becomes
  impossible, but it rewrites `keys.go` (about 1,400 lines) and every footer. That is
  the most risk on code that works.
- C. A for the keys, and a registry for text only. This choice is moved to project 2,
  because most of the drift a registry would stop is in the text.

## Not in scope

- Hint text spelling (`[esc]`/`[Esc]`, `[↵]`/`[enter]`) and verbs. That is project 2.
- Colors, titles, formats. That is project 3.

## Left to the builder

- How the guard tests find the global cases (an AST walk or a table).
- The exact exception list in guard test 1.
- Docs: `cmd/agentop/README.md` and `docs/using-agentop.md` must say that the column
  picker toggles with `space`, and sorts with `s`.

## Size

One PR: 3 small code changes, 3 guard tests and the doc lines.

# readme-demo

Generates `docs/assets/cortex-demo.svg`, the animated demo on the repo README.

```sh
go run .                    # regenerate the committed asset
go run . -out /tmp/x.svg    # write somewhere else
go run . -stills            # regenerate the still screens in docs/assets/agentop/
go test ./...               # includes the staleness checks
```

## The stills

`stills.yaml` makes the screens of [Using agentop](../../docs/using-agentop.md): one
still SVG for each beat, named for its label, in `docs/assets/agentop/`. They come from
the same capture as the demo, with the same fixture. Each beat needs `alt`, the
image's description. Keys accumulate from beat to beat, as in the demo. Its
`pipeline:` list gives the Pipeline pane stand-in plugins with real names
(`fixturepipeline.go`). There is no Usage still: the replay puts its tokens in one
bucket, so the chart is one bar. The spend breakdown needs a grid of at least 29 rows.

## What it is

A 50-second animated SVG that follows the README's quick start: the install one-liner,
`agentop configure claude-code enable` in a new terminal, three Claude Code sessions
started as usual, then `agentop observe`. In agentop, the Timeline pane shows one
session's main agent and its five subagents as they work: the bars grow, the
subagents hand back one by one, and each row has its `tok/s`. Then the events pane of
another session shows each tool call as its own row, and Enter on one opens the call
and its output.

**The agentop screens are rendered by the real agentop.** `tuicapture.go` starts a real
`session.Store`, `usage.Aggregator` and `ledger.Writer`, attaches the
production Bubble Tea model to them through the real HTTP API, feeds events, and
captures `View()`. So every column heading, bar and figure in the asset came out of
the shipping code path, and a UI change shows up on the next regeneration instead of
quietly making the demo a lie.

**Nothing is screen-recorded, and no text is real.** `demo.yaml` holds the storyboard.
Two of the sessions are invented. The live one is the shape of a real Claude Code
session, scrubbed: only its times, token counts, models, tool names and ids, and the
markers that link a subagent to its parent are kept. Every message is a hash, and the
subagent labels and commands are invented. `testdata/README.md` says how it was made.
`HOME` is redirected to a scratch directory for the whole run, so the generator cannot
read your Claude Code history even by accident.

## Editing the demo

Edit `demo.yaml`, run `go run .`, commit both. The generator fails loudly rather
than producing something subtly wrong: act runtimes must sum to `total`, a shell
act whose content cannot be typed inside its runtime is an error, and so is one
that overflows the grid.

A shell step with `ansi: true` is drawn exactly as the program writes it to a
terminal: its lines carry SGR escapes, spelled `\e` in YAML, and are decoded as the
agentop screens are. The install act is one, because setup's colours are what make it
read as setup. `bar: N` makes line N a progress bar that fills during its own delay.

To see a frame at a chosen moment, inject a global negative delay — every animation
shares one absolute timeline, so this seeks all of them at once:

```sh
python3 - <<'EOF'
import pathlib
p = pathlib.Path('../../docs/assets/cortex-demo.svg')
pathlib.Path('/tmp/t30.svg').write_text(
    p.read_text().replace('</style>', '*{animation-delay:-30s!important}\n</style>'))
EOF
# then open /tmp/t30.svg, or screenshot it with headless Chrome
```

`--virtual-time-budget` does **not** work for this: it leaves an `<img>`-embedded
SVG's animation clock at zero, so every screenshot comes back identical and looks
like proof that nothing animates.

## Why the asset is committed

GitHub cannot build it. The README references the SVG as an image, which camo
proxies and the browser renders in image mode — no scripts, but declarative CSS
animation runs, which is why none of this uses JavaScript.

## Things that will bite you

- **State groups hide with `opacity`, not `visibility`.** `visibility` is
  inheritable, but a descendant may re-declare `visible` and show through a hidden
  ancestor — and every row reveal does exactly that. Using `visibility` drew all
  nine states on top of each other.
- **Fixtures must carry real `Tools` and `Messages` arrays, not just the counts.**
  `sessionapi.summarizeEvent` recomputes `ToolCount = len(Tools)` and then nils the
  array, so counts alone arrive as zero, and the `CTX(1M)` fold skips any
  response with no manifest. Same for `AgentRole: main`.
- **Do not drop Bubble Tea commands that miss their deadline.** Their goroutines
  hold the model's timer chain; dropping them permanently stops the 2s sessions
  refresh. `pump` parks them and reads them later.
- **Give the replay's cost to the ledger before the API server starts.** The spend
  band's four spans each poll on their own cadence, so cost arriving afterwards
  produced screens where `LAST 1H` exceeded `THIS MONTH`. Only the replay's events
  arrive live; its cost goes in at the start, dated then.
- **With a replay, the clock stops at each beat's moment.** The timeline cuts its
  cells from "now", so a clock that moved during a settle moved every cell by however
  long that settle took, and two runs differed. The store also dates a session's
  update by this clock, so events recorded before the start set it to their own time:
  a tie leaves the sessions list's order to chance.
- **A beat's `frames: N` is what makes the replay read as live.** Each frame is a
  capture. Rows that two frames share are drawn once, in `<defs>`, and touching cells
  of one colour merge into one rect: without both, the asset was twice its budget.

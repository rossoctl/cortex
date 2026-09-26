# readme-demo

Generates `docs/assets/cortex-demo.svg`, the animated demo on the repo README.

```sh
go run .                    # regenerate the committed asset
go run . -out /tmp/x.svg    # write somewhere else
go test ./...               # includes the staleness check
```

## What it is

A ~50s animated SVG, in four narrative beats: the install one-liner, the Claude
Code consent prompt, three Claude Code sessions, then `abctl` answering "where did
the money go?".

**The abctl screens are rendered by the real abctl.** `tuicapture.go` starts a real
`session.Store`, `usage.Aggregator` and `ledger.Writer`, attaches the
production Bubble Tea model to them through the real HTTP API, feeds synthetic
events, and captures `View()`. So every column heading, gauge and money figure in
the asset came out of the shipping code path, and a UI change shows up on the next
regeneration instead of quietly making the demo a lie.

**Nothing is recorded.** There is no screen capture and no live proxy. `demo.yaml`
holds the storyboard; the sessions in it are invented. `HOME` is redirected to a
scratch directory for the whole run, so the generator cannot read your Claude Code
history even by accident.

## Editing the demo

Edit `demo.yaml`, run `go run .`, commit both. The generator fails loudly rather
than producing something subtly wrong: act runtimes must sum to `total`, a shell
act whose content cannot be typed inside its runtime is an error, and so is one
that overflows the grid.

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
  array, so counts alone arrive as zero, and the `CONTEXT(1M)` fold skips any
  response with no manifest. Same for `AgentRole: main`.
- **Do not drop Bubble Tea commands that miss their deadline.** Their goroutines
  hold the model's timer chain; dropping them permanently stops the 2s sessions
  refresh. `pump` parks them and reads them later.
- **Record every event before the API server starts.** The spend band's four spans
  each poll on their own cadence, so anything arriving afterwards produced screens
  where `LAST 1H` exceeded `TODAY` — both figures correct, sampled seconds apart,
  and indistinguishable from a bug.
- **The usage chart cannot be made deterministic.** It plots a ten-minute window
  ending *now* on a continuous axis, so its bars slide with the current second.
  That one state is elided from the staleness check; its contract is asserted
  structurally instead. Everything else is compared byte for byte, with nothing
  masked — masking digits once made `CONTEXT(1M)` and `CONTEXT(2M)` compare equal.

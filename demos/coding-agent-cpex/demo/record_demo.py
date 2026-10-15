#!/usr/bin/env python3
"""Record the coding-agent walkthrough, and guard its step list against drift.

    python3 demo/record_demo.py --check     # steps.md, walkthrough.sh and
                                            # scenarios/ name the same scenarios,
                                            # and every step has a caption
    python3 demo/record_demo.py --record    # check, then asciinema the
                                            # walkthrough; a GIF too with agg

The recording runs `walkthrough.sh --pace <seconds>` against the demo
`./run.sh up` started.
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

DEMO_DIR = Path(__file__).resolve().parent.parent

SCENARIO = re.compile(r"\d\d-[A-Za-z0-9-]+\.sh")
# A steps.md table row's scenario cell: | n | part | `NN-name.sh` | ... |
STEP_ROW = re.compile(r"^\|\s*\d+\s*\|([^|]*)\|\s*`(" + SCENARIO.pattern + r")`\s*\|")
# The part whose steps run only under `./run.sh up --custody`.
CUSTODY_PART = "Custody"
# The same row's last two cells: | ... | shows | caption |
CAPTION_CELL = re.compile(r"\|[^|]*\|([^|]*)\|\s*$")
# The title and closing cards: `Title: ...`, `Closing: ...` on their own line.
CARD_LINE = re.compile(r"^(Title|Closing):\s*(.+?)\s*$")
# The caption bar is the recording's full width (160 columns) less a margin,
# and carries an `n/N · ` prefix. A longer one wraps to a second line.
CAPTION_MAX = 150
# A walkthrough line that runs a scenario: run NN-name.sh
RUN_LINE = re.compile(r"^\s*run\s+(" + SCENARIO.pattern + r")\s*$")


def scenarios(demo_dir: Path) -> list[str]:
    return sorted(p.name for p in (demo_dir / "scenarios").glob("[0-9][0-9]-*.sh"))


def steps(demo_dir: Path) -> list[str]:
    text = (demo_dir / "demo" / "steps.md").read_text()
    return [m.group(2) for line in text.splitlines() if (m := STEP_ROW.match(line))]


def custody_on(demo_dir: Path = DEMO_DIR) -> bool:
    """Whether `./run.sh up --custody` is in effect (env.sh's CUSTODY_MARK)."""
    return Path(os.environ.get("CUSTODY_MARK", demo_dir / ".state" / "custody" / "enabled")).exists()


def captions(demo_dir: Path = DEMO_DIR, custody: bool | None = None) -> dict[str, str]:
    """scenario name (no .sh) -> its caption, numbered `n/N · ` in step order.
    A row without a caption maps to "". Custody steps are numbered only when
    custody is on (default: whether it is now); otherwise they keep their
    caption unnumbered, since the walkthrough does not run them."""
    if custody is None:
        custody = custody_on(demo_dir)
    text = (demo_dir / "demo" / "steps.md").read_text()
    rows = []
    for line in text.splitlines():
        if m := STEP_ROW.match(line):
            c = CAPTION_CELL.search(line[m.end() - 1 :])
            counted = custody or m.group(1).strip() != CUSTODY_PART
            rows.append((m.group(2)[:-3], c.group(1).strip() if c else "", counted))
    total = sum(counted for _, _, counted in rows)
    out, i = {}, 0
    for name, cap, counted in rows:
        i += counted
        out[name] = f"{i}/{total} · {cap}" if cap and counted else cap
    return out


def cards(demo_dir: Path = DEMO_DIR) -> dict[str, str]:
    """The `Title:` and `Closing:` lines of steps.md, keyed title/closing."""
    text = (demo_dir / "demo" / "steps.md").read_text()
    return {m.group(1).lower(): m.group(2) for line in text.splitlines() if (m := CARD_LINE.match(line))}


def check_captions(demo_dir: Path) -> list[str]:
    problems = []
    # With custody on every step is numbered, so the captions are longest.
    for name, cap in captions(demo_dir, custody=True).items():
        if not cap:
            problems.append(f"demo/steps.md has no caption for {name}.sh")
        elif len(cap) > CAPTION_MAX:
            problems.append(f"demo/steps.md: the caption for {name}.sh is {len(cap)} characters, over {CAPTION_MAX}")
    found = cards(demo_dir)
    for card in ("title", "closing"):
        if card not in found:
            problems.append(f"demo/steps.md has no {card.capitalize()}: line")
        elif len(found[card]) > CAPTION_MAX:
            problems.append(f"demo/steps.md: the {card} is {len(found[card])} characters, over {CAPTION_MAX}")
    return problems


def walkthrough_runs(demo_dir: Path) -> list[str]:
    text = (demo_dir / "walkthrough.sh").read_text()
    return [m.group(1) for line in text.splitlines() if (m := RUN_LINE.match(line))]


def _compare(label: str, got: list[str], want: list[str]) -> list[str]:
    problems = []
    for name in sorted(set(want) - set(got)):
        problems.append(f"{label} is missing {name}")
    for name in sorted(set(got) - set(want)):
        problems.append(f"{label} names {name}, which is not in scenarios/")
    dupes = sorted({n for n in got if got.count(n) > 1})
    for name in dupes:
        problems.append(f"{label} names {name} more than once")
    if not problems and got != want:
        problems.append(f"{label} runs the scenarios out of order: {', '.join(got)}")
    return problems


def check(demo_dir: Path = DEMO_DIR) -> list[str]:
    """The drift between the scenario list, steps.md and the walkthrough."""
    want = scenarios(demo_dir)
    if not want:
        return [f"no scenarios under {demo_dir / 'scenarios'}"]
    return (
        _compare("demo/steps.md", steps(demo_dir), want)
        + _compare("walkthrough.sh", walkthrough_runs(demo_dir), want)
        + check_captions(demo_dir)
    )


def record(demo_dir: Path, pace: str, out: Path) -> int:
    if not shutil.which("asciinema"):
        print("asciinema is not on PATH", file=sys.stderr)
        return 1
    cast = out.with_suffix(".cast")
    cmd = f"{demo_dir / 'walkthrough.sh'} --pace {pace}"
    rc = subprocess.call(
        ["asciinema", "rec", "--overwrite", "--cols", "100", "--rows", "40", "-c", cmd, str(cast)],
        env=os.environ.copy(),
    )
    print(f"cast: {cast} (walkthrough exit {rc})")
    if shutil.which("agg"):
        gif = out.with_suffix(".gif")
        if subprocess.call(["agg", str(cast), str(gif)]) == 0:
            print(f"gif:  {gif}")
    else:
        print("agg is not on PATH; skipping the GIF")
    return rc


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true", help="fail on drift between steps, walkthrough and scenarios")
    mode.add_argument("--record", action="store_true", help="check, then record the walkthrough")
    ap.add_argument("--pace", default="2", help="seconds between scenarios while recording (default 2)")
    ap.add_argument("--out", type=Path, default=DEMO_DIR / "demo" / "coding-agent-demo", help="output path, no extension")
    args = ap.parse_args(argv)

    problems = check()
    for p in problems:
        print(f"drift: {p}", file=sys.stderr)
    if problems:
        return 1
    if args.check:
        print(f"ok: {len(scenarios(DEMO_DIR))} scenarios, steps.md and walkthrough.sh agree")
        return 0
    return record(DEMO_DIR, args.pace, args.out)


if __name__ == "__main__":
    sys.exit(main())

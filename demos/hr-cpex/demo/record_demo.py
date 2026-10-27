#!/usr/bin/env python3
"""Record the hr-cpex walkthrough, and guard its step list against drift.

    python3 demo/record_demo.py --check     # steps.md, walkthrough.sh and
                                            # scenarios/ name the same scenarios
    python3 demo/record_demo.py --record    # check, then asciinema the
                                            # walkthrough; a GIF too with agg

The recording runs `walkthrough.sh --pace <seconds>` against whatever the
endpoint variables point at, the same as `make walkthrough`.
"""

from __future__ import annotations

import argparse
import os
import re
import shlex
import shutil
import subprocess
import sys
from pathlib import Path

DEMO_DIR = Path(__file__).resolve().parent.parent

SCENARIO = re.compile(r"\d\d-[A-Za-z0-9-]+\.sh")
# A steps.md table row's scenario cell: | n | act | `NN-name.sh` | ... |
STEP_ROW = re.compile(r"^\|\s*\d+\s*\|[^|]*\|\s*`(" + SCENARIO.pattern + r")`\s*\|")
# A walkthrough line that runs a scenario: run NN-name.sh
RUN_LINE = re.compile(r"^\s*run\s+(" + SCENARIO.pattern + r")\s*$")


def scenarios(demo_dir: Path) -> list[str]:
    return sorted(p.name for p in (demo_dir / "scenarios").glob("[0-9][0-9]-*.sh"))


def steps(demo_dir: Path) -> list[str]:
    text = (demo_dir / "demo" / "steps.md").read_text()
    return [m.group(1) for line in text.splitlines() if (m := STEP_ROW.match(line))]


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
    return _compare("demo/steps.md", steps(demo_dir), want) + _compare(
        "walkthrough.sh", walkthrough_runs(demo_dir), want
    )


def record(demo_dir: Path, pace: str, out: Path) -> int:
    if not shutil.which("asciinema"):
        print("asciinema is not on PATH", file=sys.stderr)
        return 1
    cast = out.with_suffix(".cast")
    cmd = f"{shlex.quote(str(demo_dir / 'walkthrough.sh'))} --pace {shlex.quote(pace)}"
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
    ap.add_argument("--pace", default="2", help="seconds per beat while recording (default 2)")
    ap.add_argument("--out", type=Path, default=DEMO_DIR / "demo" / "hr-cpex-demo", help="output path, no extension")
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

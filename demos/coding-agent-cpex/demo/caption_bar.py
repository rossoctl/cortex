#!/usr/bin/env python3
"""Display the current recording step's caption.

    python3 demo/caption_bar.py 03-secret-in-report-deny   # print one caption
    python3 demo/caption_bar.py --watch <file>             # follow step changes

Captions come from demo/steps.md and are numbered in walkthrough order.
The bar shows `Title:` before the first step and `Closing:` on `summary`.
record_demo.py --check requires a caption for every step. Text is centered
and bold; demo/record_tmux.sh sets the pane color.
"""

from __future__ import annotations

import argparse
import shutil
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import record_demo  # noqa: E402
from policy_slide import wrap  # noqa: E402

RESET, BOLD, YELLOW, WHITE = "\033[0m", "\033[1m", "\033[1;93m", "\033[1;97m"


def caption(step: str) -> str:
    if step in ("", "summary"):
        return record_demo.cards().get("title" if step == "" else "closing", "")
    return record_demo.captions().get(step, "")


def render(step: str, width: int, height: int) -> list[str]:
    text = caption(step)
    number, sep, rest = text.partition(" · ")
    if not sep or "/" not in number:
        number, rest = "", text
    lines = [" ".join(rest.split())]
    full = (f"{number} · " if number else "") + lines[0]
    if len(full) > width - 4:
        lines = wrap(rest, width - 4 - len(number) - 3)
    out = []
    for i, line in enumerate(lines):
        lead = f"{number} · " if number and i == 0 else (" " * (len(number) + 3) if number else "")
        pad = max((width - len(lead) - len(line)) // 2, 0)
        head = f"{YELLOW}{lead}{RESET}" if lead.strip() else lead
        out.append(" " * pad + head + f"{WHITE}{line}{RESET}")
    top = max((height - len(out)) // 2, 0)
    return ([""] * top + out + [""] * height)[:height]


def watch(state: Path) -> None:
    shown = None
    sys.stdout.write("\033[?25l")
    try:
        while True:
            try:
                step = (state.read_text().splitlines() or [""])[0].strip()
            except OSError:
                step = ""
            size = shutil.get_terminal_size()
            if (step, size) != shown:
                shown = (step, size)
                rows = render(step, size.columns, size.lines)
                sys.stdout.write("\033[H" + "\r\n".join(f"{r}\033[K" for r in rows) + "\033[J")
                sys.stdout.flush()
            time.sleep(0.1)
    except KeyboardInterrupt:
        pass
    finally:
        sys.stdout.write("\033[?25h")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("step", nargs="?", default="", help="scenario name; empty for the title, `summary` for the closing")
    ap.add_argument("--watch", type=Path, metavar="FILE", help="redraw whenever FILE names another step")
    args = ap.parse_args(argv)
    if args.watch:
        watch(args.watch)
        return 0
    size = shutil.get_terminal_size()
    print("\n".join(render(args.step, size.columns, 1)))
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Show the policy excerpt behind each walkthrough scenario.

    python3 demo/policy_slide.py 03-secret-in-report-deny   # print one slide
    python3 demo/policy_slide.py --watch <file>             # redraw whenever
                                                            # <file> names a new one

The excerpts are read from the live config at render time, between
`# slide: <name>` and `# end-slide` comments in config/cpex-policy.yaml and
config/cortex.yaml, so they cannot drift from what cortex runs. Comment
lines inside an excerpt are left out. With `bat` on PATH it highlights the
YAML; otherwise plain ANSI colours do.
"""

from __future__ import annotations

import argparse
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

DEMO_DIR = Path(__file__).resolve().parent.parent
POLICY = "config/cpex-policy.yaml"
CORTEX = "config/cortex.yaml"

# scenario -> (title, [(file, slide, caption)], note)
SLIDES: dict[str, tuple[str, list[tuple[str, str, str]], str]] = {
    "00-sandbox-boundary": (
        "00 · sandbox boundary",
        [],
        "No policy here: the OS sandbox is the boundary. A command that skips "
        "the proxy has no route out, and the developer token is outside the sandbox.",
    ),
    "01-unapproved-host-deny": (
        "01 · host allowlist",
        [(POLICY, "egress-allowlist", "tool: http_egress · pre_invocation")],
        "A tunnel is decided on its host: paste.evil.example is not in the list.",
    ),
    "02-clean-report-allow": (
        "02 · a report that passes",
        [(POLICY, "project-authz", "tool: http_egress · pre_invocation (last rule)")],
        "Every http_egress rule passes: allowed host, untainted session, a body "
        "that can be inspected, no secret, and project A is in the developer's teams.",
    ),
    "03-secret-in-report-deny": (
        "03 · secret or uninspectable body",
        [(POLICY, "body-checks", "tool: http_egress · pre_invocation")],
        "The AWS key in the excerpt is secret_in_body. Padded past the excerpt, "
        "or gzipped, the body is egress_body_uninspectable.",
    ),
    "04-tainted-session-deny": (
        "04 · reading .env taints the session",
        [
            (POLICY, "llm-taint", "llm: model calls · pre_invocation"),
            (CORTEX, "observe-codes", "cortex.yaml · cpex plugin"),
            (POLICY, "session-tainted", "tool: http_egress · pre_invocation"),
        ],
        "The deny is observed, so the turn goes on; the taint stays.",
    ),
    "05-wrong-project-deny": (
        "05 · project authorization",
        [(POLICY, "project-authz", "tool: http_egress · pre_invocation")],
        "The developer's teams claim is [A], so /projects/B/ is refused.",
    ),
    "06-fresh-session-allow": (
        "06 · taint is per session",
        [(POLICY, "session-tainted", "tool: http_egress · pre_invocation")],
        "A new session carries no secret label, so its clean report passes.",
    ),
    "09-intent-deny": (
        "09 · the model's proposed tool call",
        [(POLICY, "llm-output", "llm: model replies · post_invocation")],
        "Checked on the reply, before the agent runs anything. An untainted "
        "session's tool call goes on to the http_egress rules.",
    ),
}

INTRO = (
    "policy",
    [],
    f"Excerpts from {POLICY} appear here as each scenario runs.",
)

# Lines worth the eye: what the rule does when it fires.
KEY_LINE = re.compile(r"on_deny|taint\(|deny\(|observe_codes")

RESET, BOLD, DIM, REV = "\033[0m", "\033[1m", "\033[2m", "\033[7m"
CYAN, GREEN, YELLOW, MAGENTA = "\033[36m", "\033[32m", "\033[33m", "\033[35m"


def excerpt(rel: str, name: str, demo_dir: Path = DEMO_DIR) -> list[str]:
    """The lines between `# slide: name` and the next `# end-slide`, comments
    dropped and the common indent removed."""
    lines, inside = [], False
    for line in (demo_dir / rel).read_text().splitlines():
        s = line.strip()
        if s == f"# slide: {name}":
            inside = True
        elif s == "# end-slide" and inside:
            break
        elif inside and s and not s.startswith("#"):
            lines.append(line.rstrip())
    if not lines:
        raise KeyError(f"no slide {name!r} in {rel}")
    indent = min(len(x) - len(x.lstrip()) for x in lines)
    return [x[indent:] for x in lines]


def colour(line: str) -> str:
    """Minimal YAML colouring: keys, quoted strings, list dashes."""
    m = re.match(r"^(\s*)(- )?([A-Za-z_][\w.]*:)?(.*)$", line)
    lead, dash, key, rest = m.group(1), m.group(2) or "", m.group(3) or "", m.group(4)
    rest = re.sub(r'("[^"]*")', GREEN + r"\1" + RESET, rest)
    return f"{lead}{MAGENTA}{dash}{RESET}{CYAN}{key}{RESET}{rest}"


def clip(text: str, width: int) -> str:
    return text if len(text) <= width else text[: max(width - 1, 0)] + "…"


def wrap(text: str, width: int) -> list[str]:
    out, line = [], ""
    for word in text.split():
        if line and len(line) + 1 + len(word) > width:
            out.append(line)
            line = word
        else:
            line = f"{line} {word}" if line else word
    return out + ([line] if line else [])


def body_bat(lines: list[str], width: int) -> list[str]:
    cmd = ["bat", "-l", "yaml", "--style=plain", "--color=always", "--paging=never", f"--terminal-width={width}"]
    for i, x in enumerate(lines, 1):
        if KEY_LINE.search(x):
            cmd.append(f"--highlight-line={i}")
    p = subprocess.run(cmd, input="\n".join(lines) + "\n", capture_output=True, text=True)
    return p.stdout.splitlines() if p.returncode == 0 else body_plain(lines, width)


def body_plain(lines: list[str], width: int) -> list[str]:
    out = []
    for x in lines:
        x = clip(x, width - 2)
        if KEY_LINE.search(x):
            out.append(f"{YELLOW}{BOLD}▌{RESET}{BOLD}{colour(x)}{RESET}")
        else:
            out.append(" " + colour(x))
    return out


def render(scenario: str, width: int, demo_dir: Path = DEMO_DIR) -> str:
    title, parts, note = SLIDES.get(scenario, INTRO)
    use_bat = shutil.which("bat") is not None
    out = [f"{REV}{BOLD}{clip(' ' + title, width).ljust(width)}{RESET}"]
    for rel, name, caption in parts:
        out.append("")
        out.append(f"{DIM}{clip(caption, width)}{RESET}")
        lines = excerpt(rel, name, demo_dir)
        out.extend(body_bat(lines, width) if use_bat else body_plain(lines, width))
    out.append("")
    out.extend(f"{DIM}{x}{RESET}" for x in wrap(note, width))
    return "\n".join(out)


def watch(state: Path) -> None:
    shown, size = None, None
    sys.stdout.write("\033[?25l")  # hide the cursor
    try:
        while True:
            try:
                current = state.read_text().strip()
            except OSError:
                current = ""
            cols = shutil.get_terminal_size().columns
            if (current, cols) != (shown, size):
                shown, size = current, cols
                sys.stdout.write("\033[H\033[2J" + render(current, cols - 1) + "\n")
                sys.stdout.flush()
            time.sleep(0.2)
    except KeyboardInterrupt:
        pass
    finally:
        sys.stdout.write("\033[?25h")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("scenario", nargs="?", help="scenario name, e.g. 03-secret-in-report-deny")
    ap.add_argument("--watch", type=Path, metavar="FILE", help="redraw whenever FILE names another scenario")
    args = ap.parse_args(argv)
    if args.watch:
        watch(args.watch)
        return 0
    if not args.scenario:
        ap.error("name a scenario or pass --watch")
    print(render(args.scenario, shutil.get_terminal_size().columns - 1))
    return 0


if __name__ == "__main__":
    sys.exit(main())

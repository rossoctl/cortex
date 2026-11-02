#!/usr/bin/env python3
"""Display policy rules and decisions for each walkthrough scenario.

    python3 demo/policy_slide.py 03-secret-in-report-deny   # print one slide
    python3 demo/policy_slide.py 03-secret-in-report-deny \\
        --verdict 'deny cpex.secret_in_body report with AWS key'
    python3 demo/policy_slide.py --watch <file>             # follow file changes

Before a request, rules are dimmed and labeled with what they check.
After a decision, the slide shows DENY or ALLOW and highlights the matching
deny() rule, or all rules for an allowed request.

The watched file contains the scenario name on line 1 and
`allow|deny|blocked <code> <label>` on line 2 after a decision.
See demo/record_tmux.sh. `summary` selects the closing slide.

Excerpts are read from config/cpex-policy.yaml and config/cortex.yaml at
render time, between `# slide: <name>` and `# end-slide`. Comments inside
excerpts are omitted.
"""

from __future__ import annotations

import argparse
import re
import shutil
import sys
import time
from pathlib import Path

DEMO_DIR = Path(__file__).resolve().parent.parent
POLICY = "config/cpex-policy.yaml"
CORTEX = "config/cortex.yaml"

# scenario -> (title, [(file, slide, where it applies, what it checks)], note)
SLIDES: dict[str, tuple[str, list[tuple[str, str, str, str]], str]] = {
    "00-sandbox-boundary": (
        "the sandbox",
        [],
        "No policy rule here: the OS sandbox is the boundary. A command that "
        "skips the proxy has no route out, and the developer's token is outside "
        "the sandbox.",
    ),
    "01-unapproved-host-deny": (
        "which hosts the agent may reach",
        [(POLICY, "egress-allowlist", "every outgoing connection", "Only approved hosts can be reached.")],
        "The host judged is the one the connection goes to. A Host header naming "
        "an approved service does not change it.",
    ),
    "02-clean-report-allow": (
        "a report that passes",
        [
            (
                POLICY,
                "project-authz",
                "every outgoing request (the last rule)",
                "Reports go only to projects the developer works on.",
            )
        ],
        "Every rule before this one passes too: an approved host, a session "
        "that read no secret, a readable body with no secret in it.",
    ),
    "03-secret-in-report-deny": (
        "what the report carries",
        [
            (
                POLICY,
                "body-checks",
                "every outgoing request",
                "The body must be readable, and must not hold a secret.",
            )
        ],
        "",
    ),
    "04-tainted-session-deny": (
        "reading a secret marks the session",
        [
            (
                POLICY,
                "llm-taint",
                "every model call",
                "A secret in what the agent read marks the session; the turn goes on.",
            ),
            (POLICY, "session-tainted", "every outgoing request", "A marked session sends nothing out."),
        ],
        "",
    ),
    "05-wrong-project-deny": (
        "which projects the developer works on",
        [
            (
                POLICY,
                "project-authz",
                "every outgoing request",
                "Reports go only to projects the developer works on: A.",
            )
        ],
        "",
    ),
    "06-fresh-session-allow": (
        "the mark belongs to one session",
        [(POLICY, "session-tainted", "every outgoing request", "A marked session sends nothing out.")],
        "This session read no secret, so it is not marked.",
    ),
    "07-custody-allow": (
        "the agent never holds the credential",
        [
            (
                POLICY,
                "project-authz",
                "every outgoing request (the last rule)",
                "Reports go only to projects the developer works on.",
            )
        ],
        "With --custody, one more step runs after every rule allowed: Cortex reads "
        "the support credential from Vault as the developer and attaches it.",
    ),
    "08-custody-no-credential-on-deny": (
        "no credential for a refused report",
        [
            (POLICY, "body-checks", "every outgoing request", "The body must be readable, and must not hold a secret."),
            (
                POLICY,
                "project-authz",
                "every outgoing request",
                "Reports go only to projects the developer works on: A.",
            ),
        ],
        "Vault is asked only once every rule allowed, so a refused report never "
        "reaches it. If Vault does not answer, the report is refused.",
    ),
    "09-intent-deny": (
        "the model's suggestions",
        [
            (
                POLICY,
                "llm-output",
                "every model reply, before the agent sees it",
                "In a marked session, a suggested command must not carry the secret.",
            )
        ],
        "",
    ),
}

SUMMARY = (
    "what the demo showed",
    [],
    "",
)
FINDINGS = [
    "The agent's commands can reach the network only through Cortex. "
    "Requests to hosts that aren't approved are refused.",
    "Requests to an approved host are still checked. Cortex refuses a report that contains a secret, "
    "that it can't inspect, or that goes to a project the developer isn't on.",
    "Once the agent reads a secret, that session can't send anything else out. Other sessions are unaffected.",
    "Cortex also checks the commands the model suggests, "
    "and blocks one that would leak the secret before the agent runs it.",
]

INTRO = ("policy", [], f"The rules from {POLICY} that apply to each step appear here.")

# Decision code descriptions.
CODE_PLAIN = {
    "secret_in_body": "The body holds an AWS key.",
    "egress_body_uninspectable": "The body is padded or compressed past what policy can read.",
    "egress_host_not_allowed": "The host is not on the approved list.",
    "session_tainted_secret": "This session read a secret earlier.",
    "project_not_authorized": "The developer is not on that project.",
    "secret_in_tool_call": "The suggested command carries the secret.",
    "secret_in_tool_result": "What the agent read holds a secret.",
    "sandbox": "Outside Cortex, the sandbox lets nothing through.",
    "delegation_vault_timeout": "Vault did not answer, so there is no credential to attach.",
}
# A blocked CONNECT returns a tunnel error without the policy code.
CODE_ALIAS = {"connect_refused": "egress_host_not_allowed"}

RESET, BOLD, DIM, REV = "\033[0m", "\033[1m", "\033[2m", "\033[7m"
CYAN, GREEN, YELLOW, MAGENTA = "\033[36m", "\033[32m", "\033[33m", "\033[35m"
BADGE = {
    "deny": ("\033[1;97;41m", "\033[1;31m", "DENY"),
    "blocked": ("\033[1;97;41m", "\033[1;31m", "BLOCKED"),
    "allow": ("\033[1;30;42m", "\033[1;32m", "ALLOW"),
}


def excerpt(rel: str, name: str, demo_dir: Path = DEMO_DIR) -> list[str]:
    """Read a named slide excerpt, removing comments and common indentation."""
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


def blocks(lines: list[str]) -> list[list[str]]:
    """Split a list excerpt into rules; keep non-list excerpts whole."""
    out: list[list[str]] = []
    for x in lines:
        if x.startswith("- ") or not out:
            out.append([])
        out[-1].append(x)
    return out


def colour(line: str, strings: str = GREEN) -> str:
    """Minimal YAML colouring: keys, quoted strings, list dashes."""
    m = re.match(r"^(\s*)(- )?([A-Za-z_][\w.]*:)?(.*)$", line)
    lead, dash, key, rest = m.group(1), m.group(2) or "", m.group(3) or "", m.group(4)
    rest = re.sub(r'("[^"]*")', strings + r"\1" + RESET + BOLD, rest)
    return f"{lead}{MAGENTA}{dash}{RESET}{BOLD}{CYAN}{key}{RESET}{BOLD}{rest}"


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


def fold(line: str, width: int) -> list[str]:
    """Wrap YAML at spaces, indenting continuations by four extra columns.

    Split words that exceed the available width.
    """
    if len(line) <= width:
        return [line]
    indent = " " * (len(line) - len(line.lstrip()) + 4)
    out, cur = [], line[: len(line) - len(line.lstrip())]
    for word in line.split(" "):
        if not word:
            continue
        trial = f"{cur} {word}" if cur.strip() else cur + word
        if len(trial) <= width:
            cur = trial
            continue
        if len(indent) + len(word) > width:
            # Too long for any line: start it here and split it.
            cur = trial
        else:
            if cur.strip():
                out.append(cur)
            cur = indent + word
        while len(cur) > width:
            out.append(cur[:width])
            cur = indent + cur[width:]
    out.append(cur)
    return out


def parse_verdict(text: str | None) -> tuple[str, str, str] | None:
    """`allow|deny|blocked <code> <label...>` -> (kind, code, label)."""
    parts = (text or "").split(None, 2)
    if len(parts) < 2 or parts[0] not in BADGE:
        return None
    code = parts[1].removeprefix("cpex.")
    return parts[0], CODE_ALIAS.get(code, code), parts[2] if len(parts) > 2 else ""


def decided(block: list[str], verdict: tuple[str, str, str] | None) -> bool:
    if verdict is None:
        return False
    kind, code, _ = verdict
    if kind == "allow":
        return True
    return f"'{code}'" in "\n".join(block)


def badge(verdict: tuple[str, str, str] | None, width: int) -> list[str]:
    """Return three lines: decision badge, explanation, and affected action."""
    if verdict is None:
        return [f"{DIM}  waiting for the agent's request…{RESET}", "", ""]
    kind, code, label = verdict
    ground, fg, word = BADGE[kind]
    head = f"  {ground} {word} {RESET}"
    if kind != "allow":
        head += f"  {fg}{code}{RESET}"
    meaning = "Every rule passed." if kind == "allow" else CODE_PLAIN.get(code, "")
    return [
        head,
        f"  {BOLD}{clip(meaning, width - 2)}{RESET}",
        f"{DIM}  {clip('request: ' + label, width - 2)}{RESET}" if label else "",
    ]


def render(scenario: str, width: int, demo_dir: Path = DEMO_DIR, verdict: str | None = None) -> str:
    if scenario == "summary":
        out = [f"{REV}{BOLD}{clip(' ' + SUMMARY[0], width).ljust(width)}{RESET}", ""]
        for i, f in enumerate(FINDINGS, 1):
            lines = wrap(f, width - 5)
            out.append(f" {BOLD}{YELLOW}{i}.{RESET}  {BOLD}{lines[0]}{RESET}")
            out.extend(f"     {BOLD}{x}{RESET}" for x in lines[1:])
            out.append("")
        return "\n".join(out)
    title, parts, note = SLIDES.get(scenario, INTRO)
    v = parse_verdict(verdict)
    out = [f"{REV}{BOLD}{clip(' ' + title, width).ljust(width)}{RESET}", ""]
    if scenario in SLIDES:
        out.extend(badge(v, width))
    for rel, name, where, checks in parts:
        out.append("")
        out.append(f"{BOLD}{clip(checks, width)}{RESET}")
        out.append(f"{DIM}{clip('applies to ' + where, width)}{RESET}")
        for block in blocks(excerpt(rel, name, demo_dir)):
            hit = decided(block, v)
            bar = BADGE[v[0]][1] + "▌" + RESET if hit else " "
            # Reserve green for allowed requests.
            strings = GREEN if v and v[0] == "allow" else YELLOW
            for line in block:
                for x in fold(line, width - 2):
                    out.append(f"{bar} {colour(x, strings)}{RESET}" if hit else f"{DIM}  {x}{RESET}")
    if note:
        out.append("")
        out.extend(f"{DIM}{x}{RESET}" for x in wrap(note, width))
    return "\n".join(out)


def watch(state: Path) -> None:
    shown = None
    sys.stdout.write("\033[?25l")  # hide the cursor
    try:
        while True:
            try:
                lines = state.read_text().splitlines()
            except OSError:
                lines = []
            scenario = lines[0].strip() if lines else ""
            verdict = lines[1].strip() if len(lines) > 1 else None
            size = shutil.get_terminal_size()
            key = (scenario, verdict, size)
            if key != shown:
                shown = key
                body = render(scenario, size.columns - 1, verdict=verdict).split("\n")[: size.lines]
                # Redraw in one write to avoid flicker; clear trailing content.
                sys.stdout.write("\033[H" + "".join(f"{x}\033[K\r\n" for x in body[:-1]) + body[-1] + "\033[K\033[J")
                sys.stdout.flush()
            time.sleep(0.1)
    except KeyboardInterrupt:
        pass
    finally:
        sys.stdout.write("\033[?25h")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("scenario", nargs="?", help="scenario name, e.g. 03-secret-in-report-deny")
    ap.add_argument("--verdict", help="show the verdict state: 'allow|deny|blocked <code> <label>'")
    ap.add_argument("--watch", type=Path, metavar="FILE", help="redraw whenever FILE changes")
    args = ap.parse_args(argv)
    if args.watch:
        watch(args.watch)
        return 0
    if not args.scenario:
        ap.error("name a scenario or pass --watch")
    print(render(args.scenario, shutil.get_terminal_size().columns - 1, verdict=args.verdict))
    return 0


if __name__ == "__main__":
    sys.exit(main())

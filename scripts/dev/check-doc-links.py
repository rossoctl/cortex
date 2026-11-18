#!/usr/bin/env python3
"""Check relative Markdown links and anchors in the repository."""

import os
import re
import sys
import tempfile
from functools import lru_cache
from pathlib import Path
from urllib.parse import unquote

SKIP_DIRS = {".git", ".claude", "node_modules", "testdata"}
SKIP_PATHS = {"docs/superpowers"}

# Paths that code prints to users or names in comments. A move must keep them valid.
KEPT_PATHS = [
    (
        "docs/pricing.md",
        "",
        "cmd/agentop/cmd_config_migrate_pricing.go, cmd/agentop/cmd_service.go, cmd/cortex/local.go",
    ),
    (
        "docs/praxis-policy.md",
        "move-from-the-cpex-plugin",
        "cmd/agentop/cmd_update.go, cmd/agentop/setup_step_binaries.go, cmd/cortex/praxis.go, scripts/install.sh",
    ),
    ("docs/session-export.md", "", "cmd/agentop/cmd_export.go"),
    ("docs/laptop-local-model.md", "", "cmd/agentop/cmd_server.go"),
    (
        "docs/laptop-service.md",
        "",
        "comments in cmd/agentop/cmd_service.go and cmd/agentop/cmd_claudecode.go",
    ),
]

FENCE = re.compile(r"^\s*(`{3,}|~{3,})")
CODE_SPAN = re.compile(r"(`+).+?\1")
LINK = re.compile(r"\]\(([^)\s]+)\)")
HEADING = re.compile(r"^#{1,6}\s+(.*?)\s*#*\s*$")
HTML_ANCHOR = re.compile(r"<a\s+(?:name|id)=\"([^\"]+)\"", re.IGNORECASE)
HTML_TAG = re.compile(r"<[^>]*>")


def prose_lines(path):
    """Yield the lines of path that are outside fenced code blocks."""
    fence = None
    for line in Path(path).read_text(encoding="utf-8").splitlines():
        m = FENCE.match(line)
        if fence is None:
            if m:
                fence = m.group(1)
                continue
            yield line
        elif m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence):
            fence = None


def slug(text):
    text = HTML_TAG.sub("", text).lower()
    text = re.sub(r"[^\w\- ]", "", text)
    return text.replace(" ", "-")


@lru_cache(maxsize=None)
def slugs(path):
    """Return the GitHub anchors of a Markdown file."""
    found, seen = set(), {}
    for line in prose_lines(path):
        found.update(HTML_ANCHOR.findall(line))
        m = HEADING.match(line)
        if not m:
            continue
        base = slug(m.group(1))
        n = seen.get(base, 0)
        seen[base] = n + 1
        found.add(base if n == 0 else f"{base}-{n}")
    return frozenset(found)


def markdown_files(root):
    for dirpath, dirnames, filenames in os.walk(root):
        rel = Path(dirpath).relative_to(root)
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS and (rel / d).as_posix() not in SKIP_PATHS)
        for name in sorted(filenames):
            if name.endswith(".md"):
                yield Path(dirpath) / name


def check_links(root):
    root = Path(root)
    problems = []
    for md in markdown_files(root):
        for line in prose_lines(md):
            for target in LINK.findall(CODE_SPAN.sub("", line)):
                if target.startswith(("http:", "https:", "mailto:")):
                    continue
                reason = check_target(root, md, target)
                if reason:
                    problems.append(f"{md.relative_to(root).as_posix()}: {target} ({reason})")
    return problems


def check_target(root, md, target):
    path, _, anchor = target.partition("#")
    path = unquote(path.split("?", 1)[0])
    if not path:
        dest = md
    elif path.startswith("/"):
        dest = root / path.lstrip("/")
    else:
        dest = md.parent / path
    if not dest.exists():
        return "missing file"
    if anchor and dest.is_file() and dest.suffix == ".md":
        if unquote(anchor) not in slugs(dest.resolve()):
            return "missing anchor"
    return None


def check_kept(root):
    root = Path(root)
    problems = []
    for path, anchor, printer in KEPT_PATHS:
        dest = root / path
        if not dest.is_file() or (anchor and anchor not in slugs(dest.resolve())):
            link = f"{path}#{anchor}" if anchor else path
            problems.append(f"{link}: {link} (kept path printed by {printer})")
    return problems


def selftest():
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        (root / "a.md").write_text(
            "# Title\n\n## Dup\n\n## Dup\n\n```\n# Not a heading\n```\n\n"
            "[ok](b.md#real-heading) [dir](sub/) [dup](#dup-1)\n"
            "[gone](missing.md) [noanchor](b.md#nope) [fenced](#not-a-heading)\n"
            "`[code](ignored.md)`\n"
        )
        (root / "b.md").write_text('## Real Heading\n\n<a id="custom"></a>\n\n[c](#custom)\n')
        (root / "sub").mkdir()
        (root / "sub" / "x.md").write_text("")
        got = sorted(check_links(root))
        want = sorted(
            [
                "a.md: missing.md (missing file)",
                "a.md: b.md#nope (missing anchor)",
                "a.md: #not-a-heading (missing anchor)",
            ]
        )
        if got != want:
            print(f"selftest failed:\n  got  {got}\n  want {want}")
            return 1
    return 0


def main():
    if sys.argv[1:] == ["--selftest"]:
        return selftest()
    args = [a for a in sys.argv[1:] if a != "--"]
    if len(args) > 1 or (args and args[0].startswith("-")):
        print("usage: check-doc-links.py [--selftest | ROOT]", file=sys.stderr)
        return 2
    root = Path(args[0]) if args else Path(__file__).resolve().parents[2]
    problems = check_links(root) + check_kept(root)
    for p in problems:
        print(p)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())

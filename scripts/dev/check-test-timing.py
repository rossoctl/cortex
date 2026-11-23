#!/usr/bin/env python3
"""Flag time-dependent waits in Go tests.

A test that sleeps, polls, or bounds a call by wall-clock time passes on a quiet
machine and fails on a loaded one. This hook scans each test file it is given.
A line that needs real time carries `// timing: <reason>`, on it or in the
comment lines directly above it.

The rules are in CLAUDE.md, "Tests and time".
"""

import re
import sys

# A duration of a microsecond or more that is not negated: a negated one makes
# a deadline already past.
UNIT = r"(?<!-)\btime\.(Microsecond|Millisecond|Second|Minute)\b"

SINCE = re.compile(r"\btime\.Since\(")

RULES = [
    (
        re.compile(r"\btime\.Sleep\("),
        "sleeps for a fixed time. Block on a channel the code or fake closes when it is done.",
    ),
    (
        re.compile(r"\btime\.(After|Tick)\("),
        "a time limit beside a wait. Block on the event alone; go test's -timeout ends a real hang.",
    ),
    (
        re.compile(r"\b(Eventually\w*|Never)\("),
        "polls until a condition holds. Block on a channel that closes when it does.",
    ),
    (
        SINCE,
        "asserts on elapsed time. Assert on the outcome; to show a call does not wait on X, hold X until it returns.",
    ),
    (
        re.compile(r"\bruntime\.Gosched\("),
        "yields and hopes another goroutine ran. Block on a channel that goroutine closes.",
    ),
]

# Calls whose arguments set a real timer, and the struct field that does.
TIMERS = re.compile(r"\b(WithTimeout|WithDeadline|WithTimeoutCause|WithDeadlineCause|NewTimer|NewTicker|AfterFunc)\(")
TIMEOUT_FIELD = re.compile(r"\bTimeout:\s*[^,}]*" + UNIT)
TIMER_WHY = (
    "sets a real timeout. Use time.Hour when the test does not exercise it, "
    "and 0 or a past deadline when the test is about it expiring."
)

# A time.Since that is only reported, not compared.
LOGGED = re.compile(r"\.(Log|Logf|Error|Errorf|Fatal|Fatalf|Skip|Skipf)\(.*\btime\.Since\(")

EXEMPT = re.compile(r"\btiming:\s*\S")


def split_go(text):
    """Each line's code and comment text, with string, rune and comment contents
    blanked out of the code."""
    code, comment = [], []
    cur_code, cur_comment = [], []
    state = None  # None, "line", "block", '"', "'", "`"
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        nxt = text[i + 1] if i + 1 < n else ""
        if c == "\n":
            code.append("".join(cur_code))
            comment.append("".join(cur_comment))
            cur_code, cur_comment = [], []
            if state in ("line", '"', "'"):
                state = None
        elif state == "line" or state == "block":
            if state == "block" and c == "*" and nxt == "/":
                state = None
                i += 1
            else:
                cur_comment.append(c)
        elif state in ('"', "'"):
            if c == "\\":
                i += 1
            elif c == state:
                cur_code.append(c)
                state = None
        elif state == "`":
            if c == "`":
                cur_code.append(c)
                state = None
        elif c == "/" and nxt == "/":
            state = "line"
            i += 1
        elif c == "/" and nxt == "*":
            state = "block"
            i += 1
        else:
            if c in ('"', "'", "`"):
                state = c
            cur_code.append(c)
        i += 1
    code.append("".join(cur_code))
    comment.append("".join(cur_comment))
    return code, comment


def call_args(code, start):
    """The text inside the parentheses that open at start."""
    depth = 0
    for j in range(start, len(code)):
        if code[j] == "(":
            depth += 1
        elif code[j] == ")":
            depth -= 1
            if depth == 0:
                return code[start + 1 : j]
    return code[start + 1 :]


def problems(code):
    found = [why for pattern, why in RULES if pattern.search(code) and not (pattern is SINCE and LOGGED.search(code))]
    timer = any(re.search(UNIT, call_args(code, m.end() - 1)) for m in TIMERS.finditer(code))
    if timer or TIMEOUT_FIELD.search(code):
        found.append(TIMER_WHY)
    return found


def exempt(code, comment, n):
    """Whether line n, or the run of comment-only lines above it, says timing:."""
    if EXEMPT.search(comment[n]):
        return True
    k = n - 1
    while k >= 0 and not code[k].strip() and comment[k].strip():
        if EXEMPT.search(comment[k]):
            return True
        k -= 1
    return False


def findings(path):
    # Go ends a line only at \n, so nothing else may split one.
    with open(path, encoding="utf-8", newline="") as f:
        text = f.read()
    lines = text.split("\n")
    code, comment = split_go(text)
    for n, line in enumerate(code):
        if not exempt(code, comment, n):
            for why in problems(line):
                yield f"{path}:{n + 1}: {why}\n    {lines[n].strip()}"


def main(paths):
    found = [f for path in paths for f in findings(path)]
    for f in found:
        print(f)
    if found:
        print(
            "\nThese tests depend on timing, so a slow CI runner can fail them. Wait on\n"
            "events instead: when the code has none to wait on, add a test hook it calls,\n"
            "or inject its clock or tick channel. Rules and examples: CLAUDE.md#tests-and-time.\n"
            "\n"
            "A test that drives a real service, or measures how a cost scales, needs the\n"
            "clock: mark that line with `// timing: <reason>`, on it or just above it."
        )
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

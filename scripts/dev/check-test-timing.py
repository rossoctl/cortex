#!/usr/bin/env python3
"""Flag time-dependent waits in Go tests.

A test that sleeps, polls, or bounds a call by wall-clock time passes on a quiet
machine and fails on a loaded one. This hook scans each test file it is given.
A line that needs real time carries `// timing: <reason>`, on it or in the
comment lines directly above it.

A duration is judged where it is written: in a timer call's arguments, which may
span lines, or where a timeout, wait or deadline is set. A duration passed
through any other variable is out of the hook's sight.

The rules are in CLAUDE.md, "Tests and time".
"""

import re
import sys

# A duration of a microsecond or more.
UNIT = re.compile(r"\btime\.(Microsecond|Millisecond|Second|Minute)\b")
# A negated one, which makes a deadline already past.
NEGATED = re.compile(r"-\s*(\d+\s*\*\s*)?time\.(Microsecond|Millisecond|Second|Minute)\b")

ELAPSED = re.compile(r"\btime\.(Since|Until)\(|\btime\.Now\(\)\.Sub\(")
ELAPSED_WHY = (
    "asserts on elapsed time. Assert on the outcome; to show a call does not wait on X, hold X until it returns."
)

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
        re.compile(r"\bruntime\.Gosched\("),
        "yields and hopes another goroutine ran. Block on a channel that goroutine closes.",
    ),
]

# Calls whose arguments set a real timer, and a timeout, wait or deadline set
# as a field or by assignment.
TIMERS = re.compile(r"\b(WithTimeout|WithDeadline|WithTimeoutCause|WithDeadlineCause|NewTimer|NewTicker|AfterFunc)\(")
TIMEOUT_SET = re.compile(r"\b\w*(Timeout|Wait|Deadline)\s*(:|:?=)\s*([^,;}]*)")
TIMER_WHY = (
    "sets a real timeout. Use time.Hour when the test does not exercise it, "
    "and 0 or a past deadline when the test is about it expiring."
)

# Calls that only report a value.
LOG_CALL = re.compile(r"\.(Log|Logf|Error|Errorf|Fatal|Fatalf|Skip|Skipf)\(")

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


def call_end(code, start):
    """The index of the parenthesis that closes the one opening at start."""
    depth = 0
    for j in range(start, len(code)):
        if code[j] == "(":
            depth += 1
        elif code[j] == ")":
            depth -= 1
            if depth == 0:
                return j
    return len(code)


def real_duration(text):
    """Whether text holds a duration that is not negated."""
    return bool(UNIT.search(NEGATED.sub("", text)))


def line_problems(code):
    found = [why for pattern, why in RULES if pattern.search(code)]
    # An elapsed time inside a log call's arguments is only reported.
    logged = [(m.end() - 1, call_end(code, m.end() - 1)) for m in LOG_CALL.finditer(code)]
    if any(not any(a < m.start() < b for a, b in logged) for m in ELAPSED.finditer(code)):
        found.append(ELAPSED_WHY)
    if any(real_duration(m.group(3)) for m in TIMEOUT_SET.finditer(code)):
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
    found = {n: line_problems(line) for n, line in enumerate(code)}
    # A timer call's arguments may span lines; it is reported where it starts.
    src = "\n".join(code)
    for m in TIMERS.finditer(src):
        n = src.count("\n", 0, m.start())
        if real_duration(src[m.end() : call_end(src, m.end() - 1)]) and TIMER_WHY not in found[n]:
            found[n].append(TIMER_WHY)
    for n, whys in found.items():
        if whys and not exempt(code, comment, n):
            for why in whys:
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

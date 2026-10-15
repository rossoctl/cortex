# Coding-agent demo steps

The recorded cut, in order. `walkthrough.sh` narrates and runs these, and
`record_demo.py --record` captures that run. `record_demo.py --check`
fails when this list, the walkthrough and `scenarios/` disagree, so add a
scenario to all three.

| # | Act | Scenario | Shows |
|---|-----|----------|-------|
| 1 | Sandbox | `00-sandbox-boundary.sh` | no route around the proxy; the developer token is unreadable |
| 2 | Sandbox | `01-unapproved-host-deny.sh` | unapproved host refused at the tunnel (allowlist parity) |
| 3 | Disclosure | `02-clean-report-allow.sh` | clean report to the developer's project: 201 |
| 4 | Disclosure | `03-secret-in-report-deny.sh` | secret, padded or gzipped body to an allowed host: 403 |
| 5 | Disclosure | `04-tainted-session-deny.sh` | reading `.env` taints the session; clean reports refused |
| 6 | Disclosure | `05-wrong-project-deny.sh` | clean report to a project the developer is not on: 403 |
| 7 | Disclosure | `06-fresh-session-allow.sh` | taint is per session: a new session's report goes through |
| 8 | Intent | `09-intent-deny.sh` | model reply proposing exfiltration refused, JSON and SSE |

## Recording

```bash
./run.sh up
python3 demo/record_demo.py --record   # asciinema -> demo/coding-agent-demo.cast (+ .gif with agg)
python3 demo/record_demo.py --check
```

The recording runs `walkthrough.sh --pace 2`, so it needs no input. The
`.cast` and `.gif` are not committed.

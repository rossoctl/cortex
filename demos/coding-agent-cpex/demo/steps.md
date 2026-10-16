# Coding-agent demo steps

The scenarios, in the order `walkthrough.sh` runs them, with the caption the
tmux recording shows while each one runs. `record_demo.py --check` fails when
this list, the walkthrough and `scenarios/` disagree, or a step has no
caption, so add a scenario to all three.

| # | Part | Scenario | Shows | Caption |
|---|-----|----------|-------|---------|
| 1 | Sandbox | `00-sandbox-boundary.sh` | no route around the proxy; the developer token is unreadable | The agent's sandbox allows network access only through Cortex. |
| 2 | Sandbox | `01-unapproved-host-deny.sh` | unapproved host refused at the tunnel (as a sandbox allowlist would) | The agent tries to upload to a host that isn't approved. Cortex refuses the connection. |
| 3 | Disclosure | `02-clean-report-allow.sh` | clean report to the developer's project: 201 | The agent files a clean bug report with the developer's project. It goes through. |
| 4 | Disclosure | `03-secret-in-report-deny.sh` | secret, padded or gzipped body to an allowed host: 403 | The agent sends a report with an AWS key in it to an approved host. Cortex refuses it. |
| 5 | Disclosure | `04-tainted-session-deny.sh` | reading `.env` taints the session; clean reports refused | The agent reads a .env file. After that, Cortex refuses everything the session sends. |
| 6 | Disclosure | `05-wrong-project-deny.sh` | clean report to a project the developer is not on: 403 | The agent files a clean report with a project the developer isn't on. Cortex refuses it. |
| 7 | Disclosure | `06-fresh-session-allow.sh` | taint is per session: a new session's report goes through | A new session that hasn't read the secret files the same report. It goes through. |
| 8 | Model reply | `09-intent-deny.sh` | model reply proposing exfiltration refused, JSON and SSE | The model suggests posting the key online. Cortex blocks the reply. |

The recording opens and closes on these:

Title: A coding agent's network traffic, checked by Cortex and CPEX policy.
Closing: All of the agent's traffic goes through Cortex, and Cortex decides what is allowed to leave.

## Recording

```bash
./run.sh up
python3 demo/record_demo.py --record   # asciinema -> demo/coding-agent-demo.cast (+ .gif with agg)
python3 demo/record_demo.py --check
demo/record_tmux.sh --only 03          # the tmux recording, one scenario
```

The recording runs `walkthrough.sh --pace 2`, so it needs no input. The
`.cast` and `.gif` are not committed.

# Coding-agent demo steps

Scenarios and recording captions in walkthrough order. When adding a
scenario, update this table and `walkthrough.sh` along with `scenarios/`.
`record_demo.py --check` checks their order and requires a caption for each step.

| # | Part | Scenario | Shows | Caption |
|---|-----|----------|-------|---------|
| 1 | Sandbox | `00-sandbox-boundary.sh` | proxy bypass and developer-token read blocked | Sandboxed commands can reach the network only through Cortex. |
| 2 | Sandbox | `01-unapproved-host-deny.sh` | unapproved host blocked, including a spoofed `Host` header | The agent sends a report to an unapproved host. Cortex blocks the connection. |
| 3 | Disclosure | `02-clean-report-allow.sh` | report without secrets to project A accepted (201) | A report without secrets reaches project A. |
| 4 | Disclosure | `03-secret-in-report-deny.sh` | secret, oversized, and gzip-encoded bodies blocked (403) | The report contains an AWS key. Cortex blocks it. |
| 5 | Disclosure | `04-tainted-session-deny.sh` | reading `.env` blocks later reports from that session | The agent reads .env. Cortex blocks later reports from that session. |
| 6 | Disclosure | `05-wrong-project-deny.sh` | report to an unauthorized project blocked (403) | The developer has access only to project A. Cortex blocks a report to project B. |
| 7 | Disclosure | `06-fresh-session-allow.sh` | new session without secret taint accepted | A new session submits the same report successfully. |
| 8 | Custody | `07-custody-allow.sh` | Vault credential attached after policy approval (`--custody`) | Cortex retrieves the support credential from Vault after the report passes policy. |
| 9 | Custody | `08-custody-no-credential-on-deny.sh` | no Vault lookup on denial; report blocked if Vault is unavailable (`--custody`) | Blocked reports never trigger a Vault lookup. If Vault is unavailable, Cortex also blocks valid reports. |
| 10 | Model reply | `09-intent-deny.sh` | proposed secret leak blocked in JSON and SSE responses | Cortex blocks a model response proposing a command that leaks the key. |

Custody steps require `./run.sh up --custody`. Otherwise, they are skipped
and captions number only the eight active steps.

Recording title and closing captions:

Title: Coding agent security with Cortex and CPEX.
Closing: Cortex checks sandboxed requests and model traffic against policy before forwarding them.

## Recording

```bash
./run.sh up
python3 demo/record_demo.py --record   # asciinema -> demo/coding-agent-demo.cast (+ .gif with agg)
python3 demo/record_demo.py --check
demo/record_tmux.sh --only 03          # the tmux recording, one scenario
```

The recorder runs `walkthrough.sh --pace 2` without prompting. Generated
`.cast` and `.gif` files are not committed.

# hr-cpex demo steps

The recorded cut, in order. `walkthrough.sh` narrates and runs these, and
`record_demo.py --record` captures that run. `record_demo.py --check`
fails when this list, the walkthrough and `scenarios/` disagree, so add a
scenario to all three.

| # | Act | Scenario | Shows |
|---|-----|----------|-------|
| 1 | Workday | `01-bob-allow.sh` | role gate passes; RFC 8693 exchange at Keycloak |
| 2 | Workday | `02-alice-deny.sh` | APL role gate denies before any exchange |
| 3 | Workday | `03-eve-redact.sh` | SSN redacted in the request and the result |
| 4 | GitHub | `04-alice-internal-allow.sh` | APL, Cedar permit, exchange, granted-scope check |
| 5 | GitHub | `05-alice-external-cedar-deny.sh` | Cedar default deny |
| 6 | GitHub | `06-bob-apl-deny.sh` | cheap APL gate before Cedar |
| 7 | Email | `07-bob-pii-deny.sh` | CEL PII check; deny on the session timeline |
| 8 | Email | `08-bob-taint-deny.sh` | session taint blocks a clean email |
| 9 | Email | `09-cross-principal-taint-isolation.sh` | taint is bound to the subject |
| 10 | Approval | `10-adjust-under-threshold.sh` | small change applies; bad amount, NaN body and unrouted tool refused |
| 11 | Approval | `11-adjust-needs-approval.sh` | CIBA manager approval, single use, scoped |
| 12 | Assertions | `12-assertions.sh` | engine identity headers; spoof overwritten |
| 13 | Workload | `13-workload-delegation.sh` | SPIFFE client assertion, then exchange (needs `make spire`) |

## Recording

```bash
make port-forward                 # in another terminal
make record                       # asciinema -> demo/hr-cpex-demo.cast (+ .gif with agg)
python3 demo/record_demo.py --check
```

The recording runs `walkthrough.sh --pace 2`, so it needs no input. The
`.cast` and `.gif` are not committed.

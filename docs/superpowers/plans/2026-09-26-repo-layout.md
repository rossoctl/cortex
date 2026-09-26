# Repo Layout and Naming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take the top level from 10 directories to 7, rename `authlib` to `core` (it is 1.7% auth), and rename five internal packages to match what they do — without changing any published contract.

**Architecture:** Six commits, one per concern, so a 1,000-plus-reference rename can be reviewed piecewise. Each substitution matches the bare name without requiring a trailing slash, because the same class of pattern cost #1134 nine broken copy-paste blocks.

**Tech Stack:** Go 1.26.5 (12 modules, `go.work`), POSIX sh, GitHub Actions, Docker.

**Spec:** `docs/superpowers/specs/2026-09-26-repo-layout-design.md`

## Global Constraints

- **No published contract changes.** Image names, binary names, `x-authbridge-*` headers, `AUTHBRIDGE_*` env vars, `authbridge-*` ConfigMaps, `/etc/authbridge/*`, `authproxy-routes`, the `abph_` prefix. See spec §2.
- **`install.sh` is not edited at all**, including line ~485's `--ref` probe of `${ref}/authbridge/install.sh`.
- **Substitutions must match the bare name**, not `name/`. Nineteen sites depend on it — spec §6.
- **`replace` directives change on both sides**: `…/cortex/authlib => ../../authlib` becomes `…/cortex/core => ../../core`. Five of them.
- All Go commands with `GOWORK=off`. `go mod tidy -diff` must pass in **all 12** modules.
- Link checks resolve targets on disk with `#anchor` stripped. Never infer from a diff.
- `git commit -s`; end each message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Never `Co-Authored-By`.

---

### Task 1: Group the deployables under `deploy/`

**Files:** move `proxy-init/`, `sparc-service/`, `lineage-attach/`; modify `.github/workflows/build.yaml`, `.github/dependabot.yml`, `.github/workflows/security-scans.yaml`, `Makefile`, `scripts/local-build-and-test.sh`, docs referencing those paths.

- [ ] **Step 1: Move the three directories**

```bash
mkdir -p deploy && for d in proxy-init sparc-service lineage-attach; do git mv "$d" "deploy/$d"; done
```

- [ ] **Step 2: Fix the two build contexts**

`build.yaml` has `context: ./proxy-init` and `context: ./sparc-service`. They become `./deploy/proxy-init` and `./deploy/sparc-service`. **Image `name:` values do not change** — the operator selects by name.

- [ ] **Step 3: Sweep every other reference, by shape not by guess**

```bash
git grep -n 'proxy-init\|sparc-service\|lineage-attach' -- ':!docs/superpowers' ':!deploy'
```

Fix each. Watch for bare spellings (`cd proxy-init`, `-C proxy-init`, `directory: /proxy-init`, `make -C proxy-init`) as well as `proxy-init/` with a slash. `deploy/proxy-init/Makefile` may compute paths relative to itself — read it.

- [ ] **Step 4: Verify**

```bash
git grep -n 'context: ./proxy-init\|context: ./sparc-service'   # expect nothing
grep -cE '^\s+- name: (proxy-init|authbridge|authbridge-envoy|authbridge-lite|authbridge-cpex|sparc-service)$' .github/workflows/build.yaml  # expect 6
docker build -f deploy/proxy-init/Dockerfile.init -t proxy-init:check deploy/proxy-init
docker build -f deploy/sparc-service/Dockerfile  -t sparc:check      deploy/sparc-service
```

- [ ] **Step 5: Commit** — `refactor: Group the cluster-side deployables under deploy/`

---

### Task 2: Consolidate `scripts/`

**Files:** move `scripts/local-build-and-test.sh`, `scripts/verify-spire-keycloak.sh`, `scripts/verify-moved-ca-diagnostics.sh` into `scripts/dev/`; modify `CLAUDE.md`, `CONTRIBUTING.md`, `.claude/skills/demo/SKILL.md`, and each script's own header.

- [ ] **Step 1: Move them**

```bash
mkdir -p scripts/dev && for s in local-build-and-test verify-spire-keycloak verify-moved-ca-diagnostics; do git mv "scripts/$s.sh" "scripts/dev/$s.sh"; done
```

- [ ] **Step 2: Fix path arithmetic inside them.** `local-build-and-test.sh` computes `SCRIPT_DIR` and derives `REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"` — that becomes `../..`. This exact hazard was live in the last PR; read all three scripts for `dirname`, `cd`, and relative paths before assuming.

- [ ] **Step 3: Sweep references**

```bash
git grep -n 'local-build-and-test\|verify-spire-keycloak\|verify-moved-ca' -- ':!docs/superpowers'
```

- [ ] **Step 4: Verify**

```bash
bash -n scripts/dev/*.sh && shellcheck --severity=error scripts/dev/*.sh
sh -c 'SD=$(cd scripts/dev && pwd); RR=$(cd "$SD/../.." && pwd); test -f "$RR/go.work" && echo "REPO_ROOT ok"'
```

- [ ] **Step 5: Commit** — `refactor: Consolidate the loose dev scripts under scripts/dev/`

---

### Task 3: `authlib` → `core`

The largest task. 850 import occurrences across 393 files, one module path, five `replace` directives, nineteen bare-name sites.

**Files:** `git mv authlib core`; every `*.go` importing it; all 12 `go.mod`; `go.work`; `.github/workflows/{ci,build,release-binaries,dependabot-tidy}.yaml`; `.github/dependabot.yml`; `Makefile`; Dockerfiles; docs.

- [ ] **Step 1: Write the migration script** as `scripts/layout-migrate.sh`, committed so review is "re-run it and diff":

```sh
#!/bin/sh
# One-shot: authlib -> core. Deleted in Task 6.
set -eu
cd "$(git rev-parse --show-toplevel)"

git mv authlib core

# Module path.
(cd core && go mod edit -module github.com/rossoctl/cortex/core)

# Import paths AND the left side of replace directives.
find . -name '*.go' -o -name 'go.mod' | grep -v '/\.git/' | while read -r f; do
  sed -i '' 's|github.com/rossoctl/cortex/authlib|github.com/rossoctl/cortex/core|g' "$f"
done

# Right side of replace directives: ../../authlib -> ../../core
find . -name go.mod -not -path './.git/*' -print0 \
  | xargs -0 sed -i '' 's|=> \(\.\./\)*authlib|=> \1core|g'

# Bare-name path references. NOTE: no trailing slash required — that omission
# is what let sixteen spellings survive #1134. docs/superpowers/ is excluded
# as a dated archive; install.sh is excluded entirely.
git ls-files -z -- '*.yaml' '*.yml' '*.md' '*.sh' 'Makefile' '*.toml' 'Dockerfile*' 'go.work' \
  | grep -zv '^docs/superpowers/' | grep -zv '^install' \
  | xargs -0 sed -i '' \
      -e 's|\bauthlib/|core/|g' \
      -e 's|\bcd authlib\b|cd core|g' \
      -e 's|-C authlib\b|-C core|g' \
      -e 's|working-directory: authlib\b|working-directory: core|g' \
      -e 's|\./authlib\b|./core|g' \
      -e 's|directory: /authlib\b|directory: /core|g'

for m in $(find . -name go.mod -not -path './.git/*'); do
  (cd "$(dirname "$m")" && GOWORK=off go mod tidy)
done
```

- [ ] **Step 2: Run it, then verify by shape — not by pattern**

```bash
sh scripts/layout-migrate.sh
# Decompose the way #1134's remediation did, rather than trusting the patterns:
git grep -o 'authlib' -- ':!docs/superpowers' | wc -l          # raw mentions remaining
git grep -n 'authlib' -- ':!docs/superpowers' ':!install.sh'   # read every one
```

Expect zero outside `docs/superpowers/`. Any survivor is a shape the script missed — fix it and say which shape in the report.

- [ ] **Step 3: Gates**

```bash
export GOWORK=off
for m in $(find . -name go.mod -not -path './.git/*' -exec dirname {} \;); do
  (cd "$m" && go mod tidy -diff && go build ./... && go vet ./...) || echo "FAILED: $m"
done
(cd core && go test -race ./...)
for p in local full lite envoy; do (cd cmd/authbridge-proxy && go build -tags "$(go -C ../../scripts/profile-tags run . $p)" ./...) || echo "PROFILE FAILED: $p"; done
sh install_test.sh && pytest tests/ -v -x --ignore=tests/e2e
(cd scripts/readme-demo && go test -count=1 ./...)
```

- [ ] **Step 4: Commit** — `refactor: Rename authlib to core`

---

### Task 4: `storage/redis` → `core/storage/redis`

The only directory that changes depth, so it is the only one whose own relative paths can break.

- [ ] **Step 1: Move and repoint**

```bash
git mv storage/redis core/storage/redis && rmdir storage
(cd core/storage/redis && go mod edit -module github.com/rossoctl/cortex/core/storage/redis)
```

- [ ] **Step 2: Fix its `replace` (if any) and every reference to its old path**, including `go.work`, `.github/dependabot.yml` (`directory: /storage/redis`), and CI jobs. Check the module for relative paths — it gained one level.

- [ ] **Step 3: Verify** — `go mod tidy -diff`, `go build`, `go test` in `core/storage/redis`; `go mod tidy -diff` in `core` and every `cmd/*`; confirm `storage/` no longer exists at root.

- [ ] **Step 4: Commit** — `refactor: Move the redis driver beside the interface it implements`

---

### Task 5: Rename five packages inside `core/`

| From | To |
|---|---|
| `core/shared` | `core/memstore` |
| `core/runtimeutil` | `core/bootstrap` |
| `core/contracts` | `core/capabilities` |
| `core/tls` | `core/tlsconfig` |
| `core/{pricing,costing,costledger,costevent,usage}` | `core/cost/{pricing,settle,ledger,event,usage}` |

- [ ] **Step 1: Move each, rename its `package` clause, and sweep its import path.** 195 import sites for the cost group alone; `pricing` has 56, `usage` 60, `costevent` 50.

- [ ] **Step 2: Update the identifier prefixes.** `costing.X` becomes `settle.X`, `costledger.X` becomes `ledger.X`, `costevent.X` becomes `event.X`. The compiler finds these; run `go build ./...` until clean rather than trusting a sed.

- [ ] **Step 3: Check for name shadowing.** `event` and `settle` are common words — if a local variable named `event` exists in a file that now imports `core/cost/event`, it shadows the package. `go vet` will not always catch it; the build will.

- [ ] **Step 4: Gates** — the full set from Task 3 Step 3.

- [ ] **Step 5: Commit** — `refactor: Name core's packages for what they do`

---

### Task 6: Prose, and retire the migration script

- [ ] **Step 1: Rewrite the descriptions that are now wrong.** The docs call it *"the shared auth library"* and enumerate *"validation, exchange, cache, bypass, spiffe, routing, auth, config, all listener implementations, all plugins"* — the 1.7% first, the 32% (cost, session, observability) absent. 168 markdown mentions outside the archive. Describe what `core/` contains.

- [ ] **Step 2: `git rm scripts/layout-migrate.sh`.**

- [ ] **Step 3: Mark the spec implemented** and note it moved with the change it describes, if it did.

- [ ] **Step 4: Final gates** — everything from Task 3 Step 3, plus:

```bash
# Published contracts unchanged — the "do not break anything" gate.
grep -cE '^\s+- name: (proxy-init|authbridge|authbridge-envoy|authbridge-lite|authbridge-cpex|sparc-service)$' .github/workflows/build.yaml   # 6
ls cmd/                                        # abctl authbridge-{cpex,envoy,praxis,proxy}
git grep -c 'x-authbridge-direction'           # unchanged count
git grep -oE 'AUTHBRIDGE_[A-Z_]+' | sort -u | wc -l   # 9
git grep -c '/etc/authbridge'                  # unchanged
git diff --stat c4f8ffb5 -- install.sh         # empty
# Links resolve, anchors stripped.
for f in $(git ls-files '*.md'); do d=$(dirname "$f"); grep -oE '\]\([^)#h][^)]*\)' "$f" | sed 's/](//;s/)$//;s/#.*$//' | while read -r l; do [ -e "$d/$l" ] || echo "BROKEN $f -> $l"; done; done
```

- [ ] **Step 5: Commit** — `docs: Describe core for what it contains`

---

## Self-Review

**Spec coverage.** §3 layout → Tasks 1, 2, 3, 4. §4 renames → Task 5, with the two deliberate non-renames (`placeholder`, `praxis`) untouched by any task. §5's twelve gates → Task 3 Step 3 and Task 6 Step 4. §6's nineteen bare-name sites → Task 3 Step 1's slash-free substitutions plus Step 2's shape verification. §8 commit structure → the six tasks.

**Placeholder scan.** No TBD. Every step carries its command or its table.

**Type consistency.** The six target names (`core`, `deploy`, `memstore`, `bootstrap`, `capabilities`, `tlsconfig`, `cost/*`) are used identically in the spec's §3/§4 and in Tasks 3–5. All seven were checked for collisions against existing directories and package clauses before being chosen.

**One risk the plan cannot remove:** Task 5's `event` and `settle` are short, common words. Step 3 names the shadowing hazard, but only the build proves it.

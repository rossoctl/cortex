# PPE 5/5: Remove the Laptop CPEX Build — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Status: BLOCKED. Do not execute a task in this plan yet.** Two conditions in the issue are not met. See "Blocked" below.

**Goal:** Remove the laptop CPEX build. Keep every artifact that Kubernetes uses.

**Architecture:** Delete the release tarballs, the installer flag and the agentop logic that keeps a cpex build. Change docs and remove the Makefile target. Change no Go plugin, no image and no CI cpex job.

**Tech Stack:** Go 1.26.10 (7 modules), bash, GitHub Actions YAML, Rust workspace untouched.

**Spec:** rossoctl/cortex#1801 (issue body). Parent: rossoctl/cortex#1785.

## Blocked

The issue says: *"Do not start this issue until Hai writes on it that the deprecation window has ended. … Until then, `/sdd-fast` must not run on this issue."*

Two facts at upstream `main` `c72995b433ef86a74ffd3d301e7abcf7b0b310be` (2026-11-13):

1. **The prerequisite is not merged.** #1800 is open, and its PR **#1866** is open. #1797, #1798 and #1799 are closed. `docs/praxis-policy.md` does not exist at the tip. Task 2 of this plan must name that page in the new `--cpex` error, so the task needs #1866 in its base.
2. **The deprecation window has not started.** The window starts with the first release that ships #1800. #1800 is not merged, so no release ships it yet. This is a decision that belongs to a person (see "For a human after review").

Do not send this issue to a work agent until both conditions hold. When they hold, rebase the branch onto `main` and run the tasks below. The line estimates and the PR split stay correct.

## Facts at the tip

All line numbers are from `c72995b433ef86a74ffd3d301e7abcf7b0b310be`.

- `.github/workflows/release-binaries.yaml`
  - header comments name `build-cpex`: lines 10, 11, 20, 21, 22;
  - job `build-cpex`: lines 205-302;
  - `publish.needs` includes `build-cpex`: line 380;
  - `publish` condition reads `needs.build-cpex.result == 'success'`: line 383;
  - the tarball download loop collects `cortex-cpex_${TAG}_…`: line 431;
  - release-notes text for `cortex-cpex_*`: lines 500-501.
- `scripts/install.sh`
  - help text `--cpex`: line 181;
  - the `CPEX` variable and its comment: lines 219-222;
  - flag parse `--cpex) CPEX=1`: line 229;
  - the unknown-option list names `--cpex`: line 244;
  - `cpex_platform_check`: lines 415-421;
  - `require_cpex_build`: lines 424-434;
  - the `--cpex` branch of the release check: lines 517-527;
  - the cpex download path: lines 1181-1248. Line 1248 holds the "Replacing the installed cpex build of cortex with the default one (pass --cpex to keep it)." message.
- `scripts/release_smoke_test_linux.sh`: the `--cpex` scenes are lines 365-404. Line 398 asserts the "Replacing the installed cpex build…" message.
- `cmd/agentop`
  - `cmd_update.go:276` appends `"--cpex"` when `installedIsCpex(...)`. The doc comment at lines 34-35 names the cpex build.
  - `setup_step_binaries.go:178` defines `installedIsCpex(path string) bool`. It reads `" (cpex) "` in `--version`. **Keep this function.** It is how the update detects an old install.
  - `cmd_doctor_checks.go:88-91` appends `" -s -- --cpex"` to the fix one-liner.
  - `update_stage.go:32` defines `releaseTarballs(tag, goos, goarch string, cpex, praxis bool) []string`. `update_stage.go:52` maps `cortex-cpex_` to `proxyBinary` in `tarballBinary`.
- `Makefile:87-109` holds `cortex-cpex` and `CORTEX_CPEX_DIR`. The `.PHONY` list is line 4.
- `cmd/cortex-cpex/Dockerfile:91` runs its own `go build -tags "cpex…"`. It does not call `make cortex-cpex`. `.github/workflows/build.yaml:80` builds the image from that Dockerfile. **So requirement 5 says remove the Makefile target.**
- `scripts/install_test.sh:2021` holds `_switch='Replacing the installed cpex build of cortex with the default one (pass --cpex to keep it).'`

## Global Constraints

- Work in the worktree `.claude/worktrees/ppe-1801`. Do not touch the top-level checkout.
- Do not remove these: `cmd/cortex-cpex/Dockerfile`, `cmd/cortex-cpex/entrypoint.sh`, `cmd/cortex-cpex/CPEX_FFI_*`, `.github/workflows/build.yaml:71-161`, `scripts/fetch-cpex-ffi.sh`, `scripts/profile-tags` profiles `cpex` and `fullcpex`, `core/plugins/cpex/`, and the `go-ci-cpex` job in `.github/workflows/ci.yaml:120-197`.
- Do not remove `installedIsCpex` from `cmd/agentop/setup_step_binaries.go`. Task 3 needs it.
- Commit with `git commit -s`. End each commit message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. Do not add `Co-Authored-By`.
- PR title prefix is `docs:` or `feat:`. Use the prefix that the majority of the diff needs.
- Run `go vet` with `GOWORK=off` in each module that you change. `gofmt` always needs a path.
- PR limit: 2000 counted lines.

## Review Focus

1. **A user who runs `install.sh --cpex` after this change.** They expect a clear error that gives the replacement path. The error must print the `--praxis` flag and `docs/praxis-policy.md`, and must exit non-zero.
2. **An install that runs a cpex build today, then runs `agentop update`.** They expect the normal `cortex` binary and one line that says why. They must not get a failed download of a tarball that no release publishes.
3. **A release after this change.** `publish` must no longer wait for a cpex tarball. It must still wait for the `build` and `build-gateway` results.
4. **Kubernetes.** The `authbridge-cpex` image build and `go-ci-cpex` must not change.
5. **`agentop doctor` on a cpex install.** Its fix line must not tell the user to pass a flag that no longer exists.

## File Structure

| File | Change |
|---|---|
| `.github/workflows/release-binaries.yaml` | Remove the `build-cpex` job and every reference to it |
| `scripts/release_smoke_test_linux.sh` | Remove the `--cpex` scenes |
| `scripts/install.sh` | `--cpex` fails with the new message; remove the cpex helpers and download path |
| `scripts/install_test.sh` | Add a case for the new `--cpex` error |
| `cmd/agentop/cmd_update.go` | Stop passing `--cpex`; print one line |
| `cmd/agentop/update_stage.go` | Drop the `cpex` parameter from `releaseTarballs` |
| `cmd/agentop/cmd_doctor_checks.go` | Drop the `--cpex` fix |
| `cmd/agentop/*_test.go` | Update the tests that name cpex |
| `Makefile` | Remove the `cortex-cpex` target |
| `docs/cpex-plugin.md`, `docs/laptop-service.md`, `cmd/README.md`, `README.md` | State that the laptop build is gone |

---

### Task 1: Release workflow and Linux smoke test

**Files:**
- Modify: `.github/workflows/release-binaries.yaml:10-22,205-302,380-383,431,500-501`
- Modify: `scripts/release_smoke_test_linux.sh:365-404`

**Interfaces:**
- Consumes: nothing.
- Produces: no `cortex-cpex` tarball in a release. `publish` needs only `version`, `build` and `build-gateway`.

- [ ] **Step 1: Check that no other job consumes `build-cpex`**

Run: `grep -n "cpex" .github/workflows/release-binaries.yaml`
Expected: only the lines listed in "Facts at the tip". `build-cpex` publishes the artifact `dist-cpex-<os>-<arch>`. No other job downloads it.

- [ ] **Step 2: Delete the `build-cpex` job**

Remove lines 205-302, from `  build-cpex:` to the end of the `upload-artifact` step.

- [ ] **Step 3: Remove `build-cpex` from `publish` and from the header comments**

The new form:

```yaml
  publish:
    needs: [version, build, build-gateway]
    if: ${{ !cancelled() && needs.build.result == 'success' }}
```

Delete the comment lines 10, 11 and 20-22 that describe the cpex build. Keep the comment that says `build-gateway` is optional and that `publish` alone can write.

- [ ] **Step 4: Remove the cpex tarball from the publish download loop**

Delete the loop body that builds `dist/cortex-cpex_${TAG}_${target%/*}_${target#*/}.tar.gz` at line 431. Keep the loop for `cortex`, `agentop` and `cortex-gateway`.

- [ ] **Step 5: Remove the `cortex-cpex_*` release-notes lines**

Delete lines 500-501, the two `add '…'` calls that describe `cortex-cpex_*` and `install.sh --cpex`.

- [ ] **Step 6: Delete the `--cpex` scenes from the Linux smoke test**

Remove the block from `log "Switch to the cpex build: …"` through the "Switch back to the default build" assertions, lines 365-404. Keep the install re-run check directly above it and the `log "Uninstall"` step directly below it.

- [ ] **Step 7: Verify**

Run: `grep -n -i cpex .github/workflows/release-binaries.yaml scripts/release_smoke_test_linux.sh`
Expected: no output.

Run: `command -v actionlint >/dev/null && actionlint .github/workflows/release-binaries.yaml || echo "actionlint not installed"`
Expected: no error. If `actionlint` is not installed, run `bash -n scripts/release_smoke_test_linux.sh` and state in the PR that `actionlint` was not available.

- [ ] **Step 8: Commit**

```bash
git add .github/workflows/release-binaries.yaml scripts/release_smoke_test_linux.sh
git commit -s -m "feat: Stop publishing the laptop cpex tarballs"
```

### Task 2: install.sh

**Files:**
- Modify: `scripts/install.sh:181,219-222,229,244,415-434,517-527,1181-1248`
- Test: `scripts/install_test.sh`

**Interfaces:**
- Consumes: nothing.
- Produces: `--cpex` exits 1 with `die "--cpex is removed: the laptop cpex build is gone. Use --praxis with praxis.policy (see docs/praxis-policy.md); Kubernetes cpex use is not affected."`

- [ ] **Step 1: Write the failing test**

Add a case near the other argument-parsing cases in `scripts/install_test.sh`. It runs the script with `--cpex` and no network access and asserts a non-zero status and the message. Use the existing helper style in that file:

```bash
# --cpex is gone: it names the replacement and exits non-zero.
_cpex_out="$TMP_DIR/cpex.out"
if bash "${REPO_ROOT}/scripts/install.sh" --cpex >"${_cpex_out}" 2>&1; then
	check_fails "--cpex fails" 0
else
	check_fails "--cpex fails" 1
fi
assert_contains "${_cpex_out}" "docs/praxis-policy.md" "--cpex names the policy page"
assert_contains "${_cpex_out}" "--praxis" "--cpex names --praxis"
```

Match the helper names that `scripts/install_test.sh` already defines (`check_fails`, `assert_contains`, `TMP_DIR`).

- [ ] **Step 2: Run the test to see it fail**

Run: `bash scripts/install_test.sh 2>&1 | grep -n cpex`
Expected: FAIL. `--cpex is not available for …` or a successful install.

- [ ] **Step 3: Make `--cpex` fail in the flag parse**

Replace line 229 with:

```bash
		--cpex) die "--cpex is removed: the laptop cpex build is gone. Use --praxis with praxis.policy (see docs/praxis-policy.md); Kubernetes cpex use is not affected." ;;
```

Delete the `CPEX` variable and its comment (lines 219-222). Keep `--cpex` in the unknown-option list at line 244, or drop it — either is correct once the flag has its own case. Keep it.

- [ ] **Step 4: Delete the cpex helpers and the download path**

Remove `cpex_platform_check` (415-421), `require_cpex_build` (424-434), the `--cpex` branch at 517-527, and the whole cpex download path at 1181-1248. Remove the `--cpex` line from the help text at 181.

At line 1248, the "Replacing the installed cpex build of cortex with the default one (pass --cpex to keep it)." message must stay. It is now how a cpex install moves to the normal build. Remove only the parenthetical ` (pass --cpex to keep it)`.

- [ ] **Step 5: Run the test to see it pass**

Run: `bash scripts/install_test.sh 2>&1 | tail -20`
Expected: PASS. No case names `--cpex` as a working flag.

Run: `bash -n scripts/install.sh`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add scripts/install.sh scripts/install_test.sh
git commit -s -m "feat: Reject install.sh --cpex with a pointer to praxis mode"
```

### Task 3: agentop

**Files:**
- Modify: `cmd/agentop/cmd_update.go:34-35,181,255-280`
- Modify: `cmd/agentop/update_stage.go:31-45,52`
- Modify: `cmd/agentop/cmd_doctor_checks.go:85-92`
- Modify: `cmd/agentop/cmd_update_test.go`, `cmd/agentop/update_stage_test.go`, `cmd/agentop/cmd_doctor_checks_test.go`
- Keep: `cmd/agentop/setup_step_binaries.go:176-180`

**Interfaces:**
- Consumes: `installedIsCpex(path string) bool` from `setup_step_binaries.go`. It stays.
- Produces: `releaseTarballs(tag, goos, goarch string, praxis bool) []string`. The `cpex` parameter is gone. `updateFlags` no longer appends `--cpex`. A new function `cpexUpdateNotice() string` returns the one line.

- [ ] **Step 1: Write the failing tests**

In `cmd/agentop/update_stage_test.go`, change the table to the new signature. The `cpex` column goes. `releaseTarballs("v1.2.3", "darwin", "arm64", false)` returns `["agentop_v1.2.3_darwin_arm64.tar.gz", "cortex_v1.2.3_darwin_arm64.tar.gz"]`.

In `cmd/agentop/cmd_update_test.go`, keep the existing scene that installs `cortex (cpex) v9.9.9` (line 154). Change its assertion: the tarballs are the plain `cortex_` ones, and the update output holds the notice line.

```go
if !slices.Equal(*checked, names) || strings.Contains(strings.Join(names, " "), "cortex-cpex") {
	t.Fatalf("cpex install did not move to the default build: %v", names)
}
if !strings.Contains(out, "the laptop cpex build is removed") {
	t.Errorf("update did not say why it left the cpex build: %q", out)
}
```

In `cmd/agentop/cmd_doctor_checks_test.go`, change the `"cpex build"` case (line 60) so that the fix is `installerOneLiner` with no `--cpex` suffix.

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd cmd/agentop && go test -short -run 'TestReleaseTarballs|TestUpdate|TestDoctor' ./...`
Expected: FAIL, from the changed signatures and the missing notice.

- [ ] **Step 3: Change `releaseTarballs`**

```go
// releaseTarballs is the agentop tarball, then the cortex one, then cortex-gateway when praxis.
func releaseTarballs(tag, goos, goarch string, praxis bool) []string {
	suffix := "_" + tag + "_" + goos + "_" + goarch + ".tar.gz"
	names := []string{"agentop" + suffix, proxyBinary + suffix}
	if praxis {
		names = append(names, "cortex-gateway"+suffix)
	}
	return names
}
```

In `tarballBinary`, the `default:` branch now names only `cortex_`. Remove the `cortex-cpex_` mention from its comment.

- [ ] **Step 4: Change the two call sites**

`cmd_update.go:181`: drop the `slices.Contains(flags, "--cpex")` argument. `updateFlags`: delete the `installedIsCpex(...) → append "--cpex"` block at lines 275-277.

Add the notice. In `cmd_update.go`:

```go
// cpexUpdateNotice is the one line an update prints when it replaces a cpex build.
const cpexUpdateNotice = "the laptop cpex build is removed: installed the default cortex; use praxis mode with praxis.policy for laptop policy (docs/praxis-policy.md)"
```

In the update path, before the install runs, check `installedIsCpex(filepath.Join(env.binDir, proxyBinary))`. Cache the result in a local variable, because the install overwrites the binary. When the result is true, print `cpexUpdateNotice` to stdout, once.

- [ ] **Step 5: Change doctor**

In `cmd_doctor_checks.go`, delete the `fix += " -s -- --cpex"` branch and its comment. `bad.fix` is always `[]string{installerOneLiner}`.

- [ ] **Step 6: Run the tests to see them pass**

Run: `scripts/dev/go-test-shards.sh -short cmd/agentop`
Expected: PASS.

Then, from `cmd/agentop`: `go test -short $(go list ./... | grep -v 'agentop$')`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/agentop
git commit -s -m "feat: Move a cpex install to the default build on agentop update"
```

### Task 4: Makefile and docs

**Depends on:** Task 1, Task 2, Task 3.

**Files:**
- Modify: `Makefile:4,87-109`
- Modify: `docs/cpex-plugin.md`, `docs/laptop-service.md`, `cmd/README.md`, `README.md`

**Interfaces:**
- Consumes: the final `--cpex` error text (Task 2) and the update notice text (Task 3). The docs copy these strings.
- Produces: no `cortex-cpex` Makefile target.

- [ ] **Step 1: Remove the Makefile target**

Delete `CORTEX_CPEX_DIR`, `CPEX_FFI_DIR` and the `cortex-cpex` rule (87-109). Remove `cortex-cpex` from `.PHONY` at line 4. Check that no other target calls it: `grep -n "cortex-cpex" Makefile`.

- [ ] **Step 2: Update the four docs**

In each file, state these facts and no others:
- the laptop cpex build and `install.sh --cpex` are removed;
- the `--cpex` error names `--praxis` and `docs/praxis-policy.md`;
- an install that runs a cpex build moves to the default `cortex` on `agentop update`, with one notice line;
- `make cortex-cpex` is removed;
- the `authbridge-cpex` image, the `cpex` and `fullcpex` build profiles, `core/plugins/cpex/` and `go-ci-cpex` stay.

In `docs/cpex-plugin.md`, change the #1800 deprecation banner to say "removed on a laptop".

In `docs/laptop-service.md`, change the "Deprecated." paragraph in the cpex section to say removed, and remove the `--cpex` instructions from that section.

In `cmd/README.md` and `README.md`, change the words "deprecated" for the laptop cpex build to "removed". Keep the Kubernetes sentence unchanged.

- [ ] **Step 3: Verify the docs agree with the code**

Run: `grep -rn -i "cpex" docs/cpex-plugin.md docs/laptop-service.md cmd/README.md README.md Makefile`
Expected: no line tells a user to run `--cpex` or `make cortex-cpex`.

Run: `grep -rn "praxis-policy.md" docs/ cmd/README.md README.md scripts/install.sh cmd/agentop`
Expected: every reference resolves. `docs/praxis-policy.md` exists in the base (it comes from #1800).

- [ ] **Step 4: Commit**

```bash
git add Makefile docs cmd/README.md README.md
git commit -s -m "docs: State that the laptop cpex build is removed"
```

---

## PR List

One PR. Every change is a removal, and the total is about 635 counted lines.

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Remove the laptop cpex build` | "" | T1, T2, T3, T4 | 635 |

The base of P1 must be `main` after #1866 merges. If #1866 is not merged, stack P1 on the #1866 branch (`feat/ppe-docs-1800-pP1`). Do not start P1 before a human writes the go-ahead (see below).

## Gate

One command, from the worktree root:

```sh
bash -n scripts/install.sh && bash -n scripts/release_smoke_test_linux.sh && bash scripts/install_test.sh && (cd cmd/agentop && GOWORK=off go vet ./... && go test -short $(go list ./... | grep -v 'agentop$')) && scripts/dev/go-test-shards.sh -race cmd/agentop && (cd core && GOWORK=off go vet ./...) && (cd cmd/cortex && GOWORK=off go vet ./...) && (cd cmd/cortex-envoy && GOWORK=off go vet ./...) && (cd scripts/profile-tags && GOWORK=off go vet ./...) && gofmt -l cmd/agentop
```

Expected: no output from `gofmt`, and exit 0.

The cpex module (`cmd/cortex-cpex` has no Go code) and `core/storage/redis` are not changed by this plan. `go mod tidy -diff` is still required by the issue: run it in `core/`, `cmd/cortex`, `cmd/cortex-envoy`, `cmd/agentop`, `scripts/profile-tags` and `scripts/readme-demo` with `GOWORK=off`.

## For a human after review

These steps are not tasks. A person must do them, or they change state outside the repository.

**1. Start and end the deprecation window (Hai).** This blocks every task above.

Steps:
- Merge #1866 (the #1800 PR). Wait for a release that ships it: a `v*` tag whose release notes have the cpex deprecation line.
- After that release, write one comment on #1801: "the deprecation window has ended". #1800 requires at least one release in which users saw the deprecation.
- Then run `/sdd-fast https://github.com/rossoctl/cortex/issues/1801` again.

Needs: nothing in this plan. It needs #1866 merged and one release.

**2. Add a release-notes line after the removal PR merges.** The repo publishes release notes from PR titles. After the merge, check the next release's notes: they must say the laptop CPEX build is removed and name `--praxis`. If the title does not carry it, edit the release body by hand.

Needs: Task 1 through Task 4.

**3. Tell the CPEX contributor.** #1800 already asks for a message to Frederico Araujo (`araujof`) about the deprecation. After this removal PR merges, send one message that the laptop build is gone. Do not send it before the merge.

Needs: the merge of PR P1.

## Self-Review

- **Spec coverage:** Requirement 1 → Task 1. Requirement 2 → Task 2. Requirement 3 → Task 3. Requirement 4 → Task 1, Step 6. Requirement 5 → Task 4, Step 1. Requirement 6 → Task 4, Step 2. Requirement 7 → the Gate section.
- **Step scan:** every step names a file, a line or a command. The two exact strings (the `--cpex` error and the update notice) appear once, in the task that owns them.
- **Type consistency:** `releaseTarballs` has four parameters in Task 3 and in the Gate. `installedIsCpex` keeps one signature and one caller in Task 3.
- **Review focus:** each of the five lines has a test. Line 1 → Task 2, Step 1. Line 2 → Task 3, Step 1. Line 3 → Task 1, Step 3 and Step 7. Line 4 → the file list in Global Constraints, which no task changes. Line 5 → Task 3, Step 1.
- **Proportion:** about 635 counted lines of change. The plan is not longer than the code it removes.

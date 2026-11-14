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

1. **The prerequisite is not merged.** #1800 is open, and its PR **#1866** is open. #1797, #1798 and #1799 are closed. `docs/praxis-policy.md` does not exist at the tip. T2 of this plan must name that page in the new `--cpex` error, so the base of P1 must contain #1866.
2. **The deprecation window has not started.** The window starts with the first release that ships #1800. #1800 is not merged, so no release ships it yet. This is a decision that belongs to a person (see "For a human after review").

Do not send this issue to a work agent until both conditions hold. When they hold, rebase the branch onto `main` and run the tasks below. The line estimates and the PR split stay correct.

## Facts at the tip

All line numbers are from `c72995b433ef86a74ffd3d301e7abcf7b0b310be`.

- `.github/workflows/release-binaries.yaml`
  - the cpex paragraph in the header: lines 10-11;
  - the job count and the `build-cpex` sentence in the header: lines 19-23;
  - job `build-cpex`: lines 205-302;
  - `publish.needs` includes `build-cpex`: line 380;
  - `publish` condition reads `needs.build-cpex.result == 'success'`: line 383;
  - the tarball download loop collects `cortex-cpex_${TAG}_…`: lines 429-434, inside the "Check every tarball is present" step;
  - release-notes text for `cortex-cpex_*` and `install.sh --cpex`: lines 500-501;
  - line 515 names `-cpex` as an OLD binary name, in the rename note. **Keep line 515.**
- `scripts/install.sh`
  - help text `--cpex`: lines 181-182;
  - the `CPEX` variable and its comment: lines 219-222;
  - flag parse `--cpex) CPEX=1`: line 229;
  - the unknown-option hint names `--cpex`: line 244;
  - `cpex_platform_check`: lines 415-421;
  - `require_cpex_build`: lines 424-434;
  - the `--cpex` branch of the release check: lines 517-527;
  - the `--cpex` guard before staging: lines 1181-1187;
  - the `require_cpex_build` call: line 1211;
  - the `proxy_tgz` override: line 1237;
  - the "leaving the cpex build" notice: lines 1241-1250. Line 1245 has the `[ -z "${CPEX}" ] &&` test; line 1248 has the message. **Keep lines 1241-1250 and change them** — they are how an old cpex install moves to the default build.
- `scripts/release_smoke_test_linux.sh`
  - the header clause "a switch to the cpex build and back": line 8;
  - the `(--cpex)` parenthetical in `install_cortex`'s comment: line 59;
  - the `(cpex)` sentence in `assert_running_version_is`'s comment: line 163;
  - the `plugins_list_cpex` helper: lines 173-188;
  - the `--cpex` scenes: lines 365-404. Line 398 asserts the "Replacing the installed cpex build…" message.
- `cmd/agentop`
  - `cmd_update.go:275-277` appends `"--cpex"` when `installedIsCpex(...)`. The usage text at lines 34-36 and the `updateFlags` comment at lines 252-257 name the cpex build.
  - `cmd_update.go:181` passes `slices.Contains(flags, "--cpex")` to `releaseTarballs`.
  - `setup_step_binaries.go:178` defines `installedIsCpex(path string) bool`. It reads `" (cpex) "` in `--version`. **Keep this function.** It is how the update detects an old install.
  - `cmd_doctor_checks.go:86-92` appends `" -s -- --cpex"` to the fix one-liner.
  - `update_stage.go:31-45` defines `releaseTarballs(tag, goos, goarch string, cpex, praxis bool) []string` and its `cpex` branch; `update_stage.go:52` maps `cortex-cpex_` to `proxyBinary` in `tarballBinary`.
  - `setupHandOffFlags` (`cmd_update.go:154`) never held `--cpex`. It does not change.
  - `cmd_update_test.go:106-109`: `fakeStagedRelease(t, tag string, cpex, praxis bool)` passes `cpex` to `releaseTarballs`. Call sites: lines 85, 156, 194, 216, 241.
  - `cmd_update_test.go:154-186`: `TestUpdateHandsOffToTheStagedSetup` installs a cpex cortex stub, asserts stdout is exactly `"staged setup\n"`, and expects 9 setup args. The setup args stay 9: `--cpex` is filtered out by `setupHandOffFlags` before setup, so only stdout changes.
  - `cmd_update_test.go:164` and `:217` call `releaseTarballs` with the old signature.
  - `update_stage_test.go:119-137`: `TestReleaseTarballs` has a `cpex` column and a "cpex" case.
  - `cmd_doctor_checks_test.go:59-60`: the `"cpex build"` case expects `installerOneLiner + " -s -- --cpex"`.
- `Makefile:87-109` holds `cortex-cpex`, `CORTEX_CPEX_DIR` and `CPEX_FFI_DIR`. The `.PHONY` list is line 4.
- `cmd/cortex-cpex/Dockerfile:91` runs its own `go build -tags "cpex…"`. It does not call `make cortex-cpex`. `.github/workflows/build.yaml:80` builds the image from that Dockerfile. No workflow calls `make cortex-cpex`. **So requirement 5 says remove the Makefile target.**
- `scripts/install_test.sh`
  - `make_release` has a cpex mode: the `src-cpex` directory and copies at lines 1249-1250, `_mr_x` at line 1255, the `cpex | nocpex` arm at lines 1263-1268, and the cpex checksums line at line 1284. Only the cpex section uses that mode.
  - the cpex test section is lines 1956-2046, from the `--- --cpex:` comment to the last `piped: the released copy gets --cpex` check. The `--praxis` section starts at line 2048.
  - line 2021 holds `_switch='Replacing the installed cpex build of cortex with the default one (pass --cpex to keep it).'`.
  - `install_test.sh` has no `assert_contains` helper and no `TMP_DIR`. Its helpers are `check`, `check_fails`, `run_install`, `t_in`, `t_argv`, `t_nlog`, `t_stage`, and the sandbox is `T_RUN` inside `TMP`.
- The docs and comments that name the laptop build:
  - `README.md:118` links `[cpex policy build](./docs/laptop-service.md#the-cpex-build)`;
  - `cmd/README.md:126-142`, and the `[cpex builds](#cpex-builds)` link at line 11;
  - `docs/cpex-plugin.md:6-7` (the banner) and the body of the "On a laptop" section at lines 76-94 (the heading is line 74), which links `#on-a-laptop` and `laptop-service.md#the-cpex-build`;
  - `docs/laptop-service.md:223-250`, the "The cpex build" section;
  - `docs/plugin-catalog.md:67-69` says the plugin needs "the `authbridge-cpex` image or `make cortex-cpex`";
  - `.github/CLAUDE.md:61-67` says a `cortex-cpex_*` release tarball exists and that `install.sh --cpex` installs it;
  - `scripts/profile-tags/profiles.go:48` says `fullcpex` is "make cortex-cpex".

## Global Constraints

- Work in the worktree `.claude/worktrees/ppe-1801`. Do not touch the top-level checkout.
- Do not remove these: `cmd/cortex-cpex/Dockerfile`, `cmd/cortex-cpex/entrypoint.sh`, `cmd/cortex-cpex/CPEX_FFI_*`, `.github/workflows/build.yaml:71-161`, `scripts/fetch-cpex-ffi.sh`, `scripts/profile-tags` profiles `cpex` and `fullcpex`, `core/plugins/cpex/`, and the `go-ci-cpex` job in `.github/workflows/ci.yaml:120-197`.
- Do not remove `installedIsCpex` from `cmd/agentop/setup_step_binaries.go`. T3 needs it.
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
| `scripts/release_smoke_test_linux.sh` | Remove the `--cpex` scenes and the `plugins_list_cpex` helper |
| `scripts/install.sh` | `--cpex` fails with the new message; remove the cpex helpers and download path |
| `scripts/install_test.sh` | Replace the cpex section with a case for the new `--cpex` error |
| `cmd/agentop/cmd_update.go` | Stop passing `--cpex`; print one line |
| `cmd/agentop/update_stage.go` | Drop the `cpex` parameter from `releaseTarballs` |
| `cmd/agentop/cmd_doctor_checks.go` | Drop the `--cpex` fix |
| `cmd/agentop/*_test.go` | Update the tests that name cpex |
| `Makefile` | Remove the `cortex-cpex` target |
| `docs/cpex-plugin.md`, `docs/laptop-service.md`, `cmd/README.md`, `README.md`, `docs/plugin-catalog.md`, `.github/CLAUDE.md` | State that the laptop build is gone |
| `scripts/profile-tags/profiles.go` | Drop the dead `make cortex-cpex` from the `fullcpex` comment |

---

### T1 — Release workflow and Linux smoke test

**Depends on:** nothing.

**Files:**
- Modify: `.github/workflows/release-binaries.yaml:10-11,19-23,205-302,380-383,429-434,500-501`
- Modify: `scripts/release_smoke_test_linux.sh:8,59,163,173-188,365-404`

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
    # Run whatever build-gateway did, but only when build succeeded.
    # !cancelled() is needed: without a status function, a failed need skips this job.
    if: ${{ !cancelled() && needs.build.result == 'success' }}
```

Delete the cpex paragraph at lines 10-11. In the paragraph at lines 19-23, write "Four jobs" in place of "Five jobs", remove `build-cpex` from the job list, and replace the "every `build` and `build-cpex` tarball arrived, so a failed cpex build blocks the release rather than shipping without the variant (and the main-latest prune below never deletes the variant's assets)" sentence with "every `build` tarball arrived". Keep the sentence that says `build-gateway` is optional and that `publish` alone can write.

- [ ] **Step 4: Remove the cpex tarball from the publish download loop**

Delete the `for target in darwin/arm64 linux/amd64 linux/arm64` loop that builds `dist/cortex-cpex_${TAG}_${target%/*}_${target#*/}.tar.gz`, lines 429-434. Keep the `for name in agentop cortex` loop and the `cortex-gateway` warning loop.

- [ ] **Step 5: Remove the `cortex-cpex_*` release-notes lines**

Delete lines 500-501, the two `add '…'` calls that describe `cortex-cpex_*` and `install.sh --cpex`. **Keep line 515**, which names `-cpex` as an old binary name in the rename note.

- [ ] **Step 6: Delete the `--cpex` parts of the Linux smoke test**

Remove the block from the `# The cpex build is TAG's own variant` comment through the "Switch back to the default build" assertions, lines 365-404. Keep the install re-run check directly above it and the `log "Uninstall"` step directly below it.

Then remove the parts that only that block used:
- line 8: delete `, a switch to the cpex build and back` from the header sentence (it spans lines 7-8; keep the sentence grammatical);
- line 59: delete ` (--cpex)` from `install_cortex`'s comment;
- line 163: delete the sentence `# The cpex build prints "cortex (cpex) <version>"; pass "(cpex) <version>" for it.`;
- lines 173-188: delete the `plugins_list_cpex` helper and its comment. Nothing else calls it.

- [ ] **Step 7: Verify**

Run: `grep -n -i cpex .github/workflows/release-binaries.yaml scripts/release_smoke_test_linux.sh`
Expected: exactly one line, line 515 of the workflow, the rename note that lists `-cpex` as an old binary name. No other line.

Run: `shellcheck --severity=error scripts/release_smoke_test_linux.sh`
Expected: exit 0. This is the form the repository's CI uses (`.github/workflows/security-scans.yaml:77`). CI runs no YAML linter, and `actionlint` is not installed here: do not add it, and do not install it.

- [ ] **Step 8: Commit**

```bash
git add .github/workflows/release-binaries.yaml scripts/release_smoke_test_linux.sh
git commit -s -m "feat: Stop publishing the laptop cpex tarballs"
```

### T2 — install.sh and its tests

**Depends on:** nothing.

**Files:**
- Modify: `scripts/install.sh:181-182,219-222,229,244,415-421,424-434,517-527,1181-1187,1211,1237,1241-1250`
- Test: `scripts/install_test.sh:1249-1250,1255,1263-1268,1284,1956-2046,2107-2110`

**Interfaces:**
- Consumes: nothing. The base must contain `docs/praxis-policy.md` (from #1800/#1866); the error names it by path only, so the code does not read it.
- Produces: `--cpex` exits 1 and prints, on stderr, `error: --cpex is removed: the laptop cpex build is gone. Use --praxis with praxis.policy (see docs/praxis-policy.md); Kubernetes cpex use is not affected.`

- [ ] **Step 1: Write the failing test**

Replace the whole cpex section of `scripts/install_test.sh`, lines 1956-2046, with this block. Use the file's real helpers: `run_install` sets `T_RUN` and `T_ST`, and `t_in` answers `yes`/`no`.

```sh
# --- --cpex is gone: it names the replacement, and fetches nothing ---
make_release v9.9.9
run_install ok release --cpex
check "--cpex: dies" "1" "${T_ST}"
check "  naming --praxis and the policy page" "yes yes" \
	"$(t_in '--praxis' err) $(t_in 'docs/praxis-policy.md' err)"
check "  fetching nothing, staging nothing, running no agentop" "0 0 0" \
	"$(grep -c . "${T_RUN}/urls" || true) $(ls -A "${T_RUN}/tmp" | wc -l | tr -d ' ') $(t_nlog argv)"
```

Keep these three cases, which move into this block, with the notice text changed:
- cpex installed, re-run without `--cpex`: says so, installs the default. `make_release v9.9.9` (no second argument), `T_INSTALLED=cpex`, `run_install ok release --ref=v9.9.9`, and the `_switch` string becomes `'Replacing the installed cpex build of cortex with the default one.'`.
- default installed, re-run without `--cpex`: no notice.
- The `MR_OS=linux MR_ARCH=amd64 make_release v9.9.9 cpex` line becomes `make_release v9.9.9`.

Delete the cases that name `--cpex` as a working flag: the darwin/amd64 refusal, the linux hands-off and checksums cases, the "no cpex build" and `nocpex` cases, and the piped `--cpex` case.

At the end of the file, delete the two checks that expect `--cpex` in the help or in the unknown-option hint (lines 2107-2110). Keep the `--praxis` checks beside them.

Then remove the cpex mode from `make_release`: delete the `src-cpex` directory and its copy (lines 1249-1250), `_mr_x` (line 1255), the `cpex | nocpex` case arm (lines 1263-1268), and the `[ "${2:-two}" != cpex ]` checksums line (line 1284). Keep `${TMP}/fake-cortex-cpex`; `T_INSTALLED=cpex` in `run_install` still uses it.

- [ ] **Step 2: Run the test to see it fail**

Run: `bash scripts/install_test.sh 2>&1 | grep -n cpex`
Expected: FAIL on the new `--cpex: dies` case, because the script still installs the cpex build.

- [ ] **Step 3: Make `--cpex` fail in the flag parse**

Replace line 229 with:

```bash
		--cpex) die "--cpex is removed: the laptop cpex build is gone. Use --praxis with praxis.policy (see docs/praxis-policy.md); Kubernetes cpex use is not affected." ;;
```

Delete the `CPEX` variable and its comment (lines 219-222). Remove `--cpex` from the unknown-option hint at line 244: a removed flag must not be listed as a usable one.

- [ ] **Step 4: Delete the cpex helpers, the guards and the download path**

Remove:
- the `--cpex` help entry (lines 181-182);
- `cpex_platform_check` (415-421) and `require_cpex_build` (424-434);
- the `--cpex, checked here` block (517-527);
- the `# --- --cpex: only where the cpex build is published ---` guard (1181-1187);
- `[ -z "${CPEX}" ] || require_cpex_build "${version}"` (line 1211);
- `[ -z "${CPEX}" ] || proxy_tgz="cortex-cpex_…"` (line 1237).

Then change the "leaving the cpex build" notice, which stays:
- line 1245: `if [ -z "${CPEX}" ] && [ -x "${BIN_DIR}/cortex" ]; then` becomes `if [ -x "${BIN_DIR}/cortex" ]; then`. Without this, `set -u` fails on the deleted `CPEX`.
- line 1248: remove the parenthetical. The message becomes `Replacing the installed cpex build of cortex with the default one.`

- [ ] **Step 5: Run the test to see it pass**

Run: `bash scripts/install_test.sh 2>&1 | tail -20`
Expected: PASS, and the run ends with `0 failed`.

Run: `bash -n scripts/install.sh && shellcheck --severity=error scripts/install.sh scripts/install_test.sh`
Expected: no output, exit 0.

- [ ] **Step 6: Commit**

```bash
git add scripts/install.sh scripts/install_test.sh
git commit -s -m "feat: Reject install.sh --cpex with a pointer to praxis mode"
```

### T3 — agentop

**Depends on:** nothing.

**Files:**
- Modify: `cmd/agentop/cmd_update.go:34-36,181,252-257,275-277`
- Modify: `cmd/agentop/update_stage.go:31-45,52`
- Modify: `cmd/agentop/cmd_doctor_checks.go:86-92`
- Modify: `cmd/agentop/cmd_update_test.go:85,106-109,154-186,194,216-217,241`
- Modify: `cmd/agentop/update_stage_test.go:119-137`
- Modify: `cmd/agentop/cmd_doctor_checks_test.go:59-60`
- Keep: `cmd/agentop/setup_step_binaries.go:166-180`

**Interfaces:**
- Consumes: `installedIsCpex(path string) bool` from `setup_step_binaries.go`. It stays.
- Produces: `releaseTarballs(tag, goos, goarch string, praxis bool) []string`. The `cpex` parameter is gone. `updateFlags` no longer appends `--cpex`. A new constant `cpexUpdateNotice` holds the one line: `"the laptop cpex build is removed: installed the default cortex; use praxis mode with praxis.policy for laptop policy (docs/praxis-policy.md)"`.

- [ ] **Step 1: Write the failing tests**

`update_stage_test.go`: `TestReleaseTarballs` (lines 119-137) loses the `cpex` column and the `"cpex"` case. The call becomes `releaseTarballs("v1.2.3", "darwin", "arm64", c.praxis)`. The two remaining cases expect `["agentop_v1.2.3_darwin_arm64.tar.gz", "cortex_v1.2.3_darwin_arm64.tar.gz"]` and, with praxis, the extra `cortex-gateway` name.

`cmd_update_test.go`: change `fakeStagedRelease` to `func fakeStagedRelease(t *testing.T, tag string, praxis bool)` and its `releaseTarballs` call to `releaseTarballs(tag, runtime.GOOS, runtime.GOARCH, praxis)`. Update its five call sites: `fakeStagedRelease(t, c.release, false)` at line 85, `(t, "v9.9.9-rc.1", false)` at line 156, `(t, "v9.9.9", true)` at line 194, `(t, "v9.9.10", false)` at lines 216 and 241.

`TestUpdateHandsOffToTheStagedSetup` (lines 154-186) keeps the `cortex (cpex) v9.9.9` stub at line 154. Its assertions become:

```go
	if code != 7 || out.String() != cpexUpdateNotice+"\nstaged setup\n" {
		t.Fatalf("exit %d stdout %q, want 7 and the notice then the staged setup's line:\n%s", code, out.String(), errb.String())
	}
	names := releaseTarballs("v9.9.9-rc.1", runtime.GOOS, runtime.GOARCH, false)
	if !strings.HasPrefix(names[1], "cortex_") || !slices.Equal(*checked, names) {
		t.Errorf("checked %q, want %q", *checked, names)
	}
```

The setup-argument checks in that test do not change: `--cpex` was never in `setupHandOffFlags`, so `got` still has 9 lines and `want` is the same.

`cmd_doctor_checks_test.go`: the `"cpex build"` case (line 60) expects `installerOneLiner`, with no ` -s -- --cpex` suffix.

- [ ] **Step 2: Run the tests to see them fail**

Run: `cd cmd/agentop && go test -run 'TestReleaseTarballs|TestUpdate|TestDoctor' .`
Expected: FAIL, from the changed signatures and the missing notice. No `-short` here: `newDoctorScene` calls `freePorts`, which skips every scene under `-short`, so the update and doctor tests would be skipped instead of failing.

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

- [ ] **Step 4: Drop the `--cpex` flag and print the notice**

`cmd_update.go:181`: drop the `slices.Contains(flags, "--cpex")` argument.

`updateFlags`: delete the `if installedIsCpex(...) { flags = append(flags, "--cpex") }` block at lines 275-277. Change its doc comment (lines 252-257) and the usage text (lines 34-36) so neither says the update keeps the cpex build. Say instead that the update moves a cpex install to the default build and says so.

Add the constant above `installRelease`:

```go
// cpexUpdateNotice is the one line an update prints when it replaces a cpex build.
const cpexUpdateNotice = "the laptop cpex build is removed: installed the default cortex; use praxis mode with praxis.policy for laptop policy (docs/praxis-policy.md)"
```

In `installRelease`, after the `ref` checks and before `var run func() error` — so both the release path and the `--ref=main` installer path print it — add:

```go
	if installedIsCpex(filepath.Join(env.binDir, proxyBinary)) {
		_, _ = fmt.Fprintln(stdout, cpexUpdateNotice)
	}
```

Read it before the install runs, because the install overwrites the binary.

- [ ] **Step 5: Change doctor**

In `cmd_doctor_checks.go`, delete the `fix += " -s -- --cpex"` branch and its comment (lines 86-91). `bad.fix` is always `[]string{installerOneLiner}`.

- [ ] **Step 6: Run the tests to see them pass**

Run: `cd cmd/agentop && go test -run 'TestReleaseTarballs|TestUpdate|TestDoctor' .`
Expected: PASS. This runs the changed scenes. `-short` skips them (see Step 2), so do not use it here.

Then: `scripts/dev/go-test-shards.sh -short cmd/agentop` and, from `cmd/agentop`, `go test -short $(go list ./... | grep -v 'agentop$')`.
Expected: PASS both. The full `-race` suite in the gate is what runs the scenes that `-short` skips.

- [ ] **Step 7: Commit**

```bash
git add cmd/agentop
git commit -s -m "feat: Move a cpex install to the default build on agentop update"
```

### T4 — Makefile, docs and stale comments

**Depends on:** nothing. The two strings it copies are written in full in T2 and T3, so it does not wait for them.

**Files:**
- Modify: `Makefile:4,87-109`
- Modify: `docs/cpex-plugin.md`
- Modify: `docs/laptop-service.md:223-250`
- Modify: `docs/plugin-catalog.md:67-69`
- Modify: `cmd/README.md:11,126-142`
- Modify: `README.md:118`
- Modify: `.github/CLAUDE.md:61-67`
- Modify: `scripts/profile-tags/profiles.go:48`

**Interfaces:**
- Consumes: nothing at build time. It copies these two strings, written here:
  - the `--cpex` error: `--cpex is removed: the laptop cpex build is gone. Use --praxis with praxis.policy (see docs/praxis-policy.md); Kubernetes cpex use is not affected.`
  - the update notice: `the laptop cpex build is removed: installed the default cortex; use praxis mode with praxis.policy for laptop policy (docs/praxis-policy.md)`
- Produces: no `cortex-cpex` Makefile target.

- [ ] **Step 1: Remove the Makefile target**

Delete `CORTEX_CPEX_DIR`, `CPEX_FFI_DIR` and the `cortex-cpex` rule (lines 87-109). Remove `cortex-cpex` from `.PHONY` at line 4. Then run `grep -n "cortex-cpex" Makefile` and expect no output.

In `scripts/profile-tags/profiles.go:48`, change the `fullcpex` comment from "make cortex-cpex, a cgo build of cortex" to "the `authbridge-cpex` image, a cgo build of cortex".

- [ ] **Step 2: Update the docs and the release note in `.github/CLAUDE.md`**

State these facts, and no others:
- the laptop cpex build and `install.sh --cpex` are removed;
- `--cpex` now exits with the error string above, which names `--praxis` and `docs/praxis-policy.md`;
- an install that runs a cpex build moves to the default `cortex` on `agentop update`, with the notice line above;
- `make cortex-cpex` is removed;
- the `authbridge-cpex` image, the `cpex` and `fullcpex` build profiles, `core/plugins/cpex/` and `go-ci-cpex` stay, and Kubernetes use is unchanged.

File by file:
- `docs/laptop-service.md:223-250`: replace the body of the section with a short paragraph that states the facts above. **Keep the heading `## The cpex build`** so the links to it still resolve. Do not keep the `--cpex` command, the `(cpex)` version check or the "keep passing --cpex" paragraph.
- `docs/cpex-plugin.md`: change the banner at lines 6-7 so it says the laptop build is removed and points to `--praxis`. Replace the body of the "On a laptop" section (lines 76-94) with a paragraph that says the laptop build is gone and that the `authbridge-cpex` image is unchanged. **Keep both headings**, so the `#on-a-laptop` self-link at line 7 and the `laptop-service.md#the-cpex-build` link at lines 92-93 still resolve; change the sentence at line 92 to say the laptop build is removed.
- `cmd/README.md`: delete the "Laptop" bullet at lines 137-142. Keep the `authbridge-cpex` image bullet and the Kubernetes text. Keep the `[cpex builds](#cpex-builds)` link at line 11; the section stays.
- `README.md:118`: delete `[cpex policy build](./docs/laptop-service.md#the-cpex-build) ·` from the "Learn more" line. The laptop build no longer exists to point at.
- `docs/plugin-catalog.md:67-69`: change "the `authbridge-cpex` image or `make cortex-cpex`" to "the `authbridge-cpex` image".
- `.github/CLAUDE.md:61-67`: delete the paragraph that says a `cortex-cpex_*` release tarball exists and that `install.sh --cpex` installs it. State that the laptop build is removed and that `build-cpex` no longer exists. Keep the `authbridge-cpex` image description.

- [ ] **Step 3: Verify the docs agree with the code**

Run: `grep -rn "make cortex-cpex\|install.sh --cpex\|sh -s -- --cpex" README.md cmd/README.md docs/*.md .github/CLAUDE.md Makefile scripts/profile-tags/profiles.go`
Expected: no line. The removed flag may still appear in a sentence that says it is removed.

Run: `grep -rn -i "cortex-cpex_" README.md cmd/README.md docs/*.md .github/CLAUDE.md`
Expected: no line that says a release still publishes it.

Run: `grep -rn "praxis-policy.md" docs/ cmd/README.md README.md scripts/install.sh cmd/agentop`
Expected: every reference resolves. `docs/praxis-policy.md` must exist in the base (it comes from #1800); if it is absent, stop — the base is wrong, not the docs.

- [ ] **Step 4: Commit**

```bash
git add Makefile docs/cpex-plugin.md docs/laptop-service.md docs/plugin-catalog.md cmd/README.md README.md .github/CLAUDE.md scripts/profile-tags/profiles.go
git commit -s -m "docs: State that the laptop cpex build is removed"
```

---

## PR List

One PR. Most changes are removals, and the total is about 450 counted lines, far under the 2000 limit. Tasks in one PR run at the same time, so T1, T2, T3 and T4 all start together.

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Remove the laptop cpex build` | "" | T1, T2, T3, T4 | 450 |

The base of P1 must be `main` after #1866 merges. If #1866 is not merged, stack P1 on the #1866 branch (`feat/ppe-docs-1800-pP1`). Do not start P1 before a human writes the go-ahead (see below).

## Gate

One command, from the worktree root:

```sh
bash -n scripts/install.sh && bash -n scripts/release_smoke_test_linux.sh && shellcheck --severity=error scripts/install.sh scripts/install_test.sh scripts/release_smoke_test_linux.sh && bash scripts/install_test.sh && (cd cmd/agentop && GOWORK=off go vet ./... && GOWORK=off go mod tidy -diff && go test -short $(go list ./... | grep -v 'agentop$')) && scripts/dev/go-test-shards.sh -race cmd/agentop && (cd core && GOWORK=off go vet ./...) && (cd core/storage/redis && GOWORK=off go vet ./...) && (cd cmd/cortex && GOWORK=off go vet ./...) && (cd cmd/cortex-envoy && GOWORK=off go vet ./...) && (cd scripts/profile-tags && GOWORK=off go vet ./...) && (cd scripts/readme-demo && GOWORK=off go vet ./...) && gofmt -l cmd/agentop
```

Expected: no output from `gofmt` or `go mod tidy -diff`, and exit 0.

Checked on the unchanged base `c72995b43`: `bash -n` on both scripts, `shellcheck --severity=error` on all three scripts, `gofmt -l cmd/agentop`, `go vet ./...` in all 7 modules, and `go mod tidy -diff` in `cmd/agentop` all pass. The test, shard and tidy steps for the other modules were not run (they are slow, and the plan changes no import in them).

`scripts/profile-tags/profiles.go` is one of the 7 modules' files, so `scripts/profile-tags` is a real vet target. `cmd/cortex-cpex` holds no Go code. `go mod tidy -diff` is required by requirement 7; run it also in `core/`, `cmd/cortex`, `cmd/cortex-envoy`, `scripts/profile-tags` and `scripts/readme-demo` with `GOWORK=off` before you push.

## For a human after review

These steps are not tasks. A person must do them, or they change state outside the repository.

**1. Start and end the deprecation window (Hai).** This blocks every task above.

Steps:
- Merge #1866 (the #1800 PR). Wait for a release that ships it: a `v*` tag whose release notes have the cpex deprecation line.
- After that release, write one comment on #1801: "the deprecation window has ended". #1800 requires at least one release in which users saw the deprecation.
- Then run `/sdd-fast https://github.com/rossoctl/cortex/issues/1801` again.

Needs: nothing in this plan. It needs #1866 merged and one release.

**2. Add a release-notes line after the removal PR merges.** The repo publishes release notes from PR titles. After the merge, check the next release's notes: they must say the laptop CPEX build is removed and name `--praxis`. If the title does not carry it, edit the release body by hand.

Needs: T1 through T4.

**3. Tell the CPEX contributor.** #1800 already asks for a message to Frederico Araujo (`araujof`) about the deprecation. After this removal PR merges, send one message that the laptop build is gone. Do not send it before the merge.

Needs: the merge of PR P1.

## Self-Review

- **Spec coverage:** Requirement 1 → T1. Requirement 2 → T2. Requirement 3 → T3. Requirement 4 → T1, Step 6. Requirement 5 → T4, Step 1. Requirement 6 → T4, Step 2. Requirement 7 → the Gate section.
- **What is left out:** the deprecation window and the release note are human steps, in "For a human after review". The Go plugin, the `authbridge-cpex` image, the `cpex`/`fullcpex` profiles, the FFI pins and `go-ci-cpex` stay, as the issue says.
- **Step scan:** every step names a file, a line or a command. The two exact strings (the `--cpex` error and the update notice) appear in full in T2, T3 and T4, so no task waits on another for a value.
- **Dependencies:** T1, T2, T3 and T4 each depend on no task. They touch different files. The full strings a doc needs are in T4 itself. All four are in P1, so they run at the same time.
- **Type consistency:** `releaseTarballs` has four parameters in T3. `installedIsCpex` keeps one signature. `cpexUpdateNotice` is a const, not a function.
- **Review focus:** each of the five focus lines has a test or a check. Line 1 → T2, Step 1. Line 2 → T3, Step 1. Line 3 → T1, Steps 3 and 7. Line 4 → the file list in Global Constraints, which no task changes. Line 5 → T3, Step 1.
- **Proportion:** about 450 counted lines of change. The plan is not longer than the code and docs it changes.

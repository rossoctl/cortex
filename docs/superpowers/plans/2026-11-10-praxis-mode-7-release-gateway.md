# Praxis mode 7/8: Release cortex-gateway as a standalone binary — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Issue:** #1723 (part of #1714). **Depends on:** #1719 and #1724, both closed and merged.

**Goal:** Ship `cortex-gateway` in each release as a standalone tarball for darwin/arm64, linux/amd64 and linux/arm64, with no system library to install. A gateway failure must not block the Go release.

**Architecture:** One POSIX `sh` build script builds the release binary on the host it runs on (native macOS, or `rust:alpine` for a static musl Linux binary) and then runs one linkage check script. The release workflow gets a `build-gateway` matrix job that calls the build script, and `publish` treats the gateway tarballs as optional. A new weekly workflow builds against the newest Praxis release with the same script and opens an issue when it fails.

**Tech Stack:** POSIX sh, GitHub Actions, Cargo (Rust 1.96.0, pinned in `praxis/rust-toolchain.toml`), `rust:1.96.0-alpine`, `readelf`, `otool`.

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md`, Section 4 "Release and install" and "A standalone binary", Section 6 step 7.

## What already exists (do not redo)

- `praxis/server/Cargo.toml` already turns on `openssl = { version = "0.10", features = ["vendored"] }`. No Cargo change is necessary.
- `praxis/server/src/main.rs` already prints `cortex-gateway <ver>` for `--version`. `<ver>` is the build-time env `CORTEX_VERSION`, or `dev`. `cmd/cortex/supervise.go` refuses a gateway whose version differs from `cortex`. So the release build MUST set `CORTEX_VERSION=${VERSION}`.
- `scripts/install.sh --praxis` already downloads `cortex-gateway_<tag>_<os>_<arch>.tar.gz`, expects one file `cortex-gateway` in it, and refuses a release whose `checksums.txt` does not list that file (`require_praxis_build`). The tarball name and its content are therefore fixed.
- `.github/dependabot.yml` already has a `cargo` entry for `/praxis`. It proposes `openssl-src` bumps. No change.
- `Cargo.lock` has no `aws-lc-sys` (TLS uses `ring` and vendored OpenSSL). The native code to link statically is `openssl-src`, `zstd-sys`, `tikv-jemalloc-sys` and `ring`.

## Global Constraints

- Targets: darwin/arm64, linux/amd64, linux/arm64. No darwin/amd64 build.
- Tarball name: `cortex-gateway_${TAG}_${os}_${arch}.tar.gz`. It holds one file: `cortex-gateway`.
- `cortex-gateway --version` must print exactly `cortex-gateway ${VERSION}`.
- Linux binaries: static musl, built in `rust:1.96.0-alpine` (pinned by index digest), with `-C target-feature=+crt-static`. The ELF must have no `NEEDED` entry.
- macOS binaries: only `/usr/lib/` and `/System/Library/` libraries in `otool -L`. `MACOSX_DEPLOYMENT_TARGET=12.0`.
- A gateway build failure does not block the Go release. `build` or `build-cpex` failure still blocks it, as today.
- The weekly Praxis job does not block anything, and it reports a failure.
- Pin every action and every container image by digest, as the rest of `.github/workflows/` does.
- Shell scripts pass `shellcheck --severity=error`. Workflows pass the relaxed `yamllint` config in `security-scans.yaml`.
- Prose follows the root `CLAUDE.md` naming rule (say "Cortex" for the product) and is in ASD-STE100 Simplified Technical English.

## Decisions taken in this plan (and why)

1. **One PR.** The estimate is about 600 counted lines, far below 2000. The tasks are small and share the build script.
2. **Build logic lives in `scripts/build-gateway.sh`, not inline YAML.** The release job, the weekly job and the local gate all run the same file. `build-cpex` keeps its inline `BUILD` because it is not reused.
3. **The script `cd`s into `praxis/`.** rustup reads `rust-toolchain.toml` only from the current directory and its parents, not from `--manifest-path` (see the note in `praxis.yaml`).
4. **The Linux build passes `--target <host triple>`.** With `--target`, `RUSTFLAGS` does not reach build scripts and proc-macros. A proc-macro cannot be a `crt-static` musl artifact, so without `--target` the build fails. It also puts the output in `target/<triple>/release`, so a macOS build and a container build in one checkout do not overwrite each other.
5. **`JEMALLOC_SYS_WITH_LG_PAGE=16` on linux/arm64.** jemalloc fixes the page size at build time. The arm64 runner has 4 KiB pages, but some arm64 Linux hosts use 64 KiB pages, and a 4 KiB jemalloc crashes there. 64 KiB (`2^16`) also works on 4 KiB hosts.
6. **Strip with `CARGO_PROFILE_RELEASE_STRIP=symbols` in the script.** The Go release strips with `-s -w`. The env keeps `praxis/Cargo.toml` unchanged for developers.
7. **`MACOSX_DEPLOYMENT_TARGET=12.0`.** Go 1.26 binaries need macOS 12, so the gateway claims the same floor.
8. **`build-gateway` uses `fail-fast: false`.** Each platform is optional on its own. `install.sh --praxis` checks per `os/arch` in `checksums.txt`, so two of three tarballs is a valid release.
9. **`publish` runs when `build` and `build-cpex` succeed, whatever `build-gateway` did:** `if: ${{ !cancelled() && needs.build.result == 'success' && needs.build-cpex.result == 'success' }}`. A missing gateway tarball gives a `::warning::`, not an error.
10. **On `main-latest`, the prune step deletes an old gateway asset when this build has none.** That is correct: `checksums.txt` would not list the old asset, so nothing could verify it. `install.sh --praxis --ref=main` then refuses with its existing message.
11. **The release notes name the gateway only when at least one gateway tarball is present.**
12. **No build cache in the release job.** A release builds clean, as `build-cpex` does. The weekly job also builds clean, so that it sees what a release sees.
13. **The clean-host check on Linux runs the binary in three places:** the glibc runner, plain `alpine` (musl), and `gcr.io/distroless/static-debian12` (no libc and no OpenSSL). With no `NEEDED` entry, that covers the issue's "glibc Linux and musl Linux with no OpenSSL package". Plain `alpine` is not the "no OpenSSL" host, because its base image ships `libssl3`.
14. **The weekly job reads the newest Praxis version from crates.io** (`https://crates.io/api/v1/crates/praxis-proxy`, field `.crate.max_stable_version`, with a `User-Agent` header, which crates.io requires). It rewrites the `=X.Y.Z` pins of `praxis-proxy` and `praxis-proxy-filter` in both Cargo manifests, runs `cargo update -p praxis-proxy -p praxis-proxy-filter`, `cargo test --locked`, then the static Linux build. It runs also when the newest version is the pinned one: it then checks the current pin against a fresh toolchain image.
15. **The weekly report is one GitHub issue.** On failure, a `report` job (`issues: write`) comments on the open issue titled `Weekly build against the latest Praxis failed`, or opens it. A human closes it. A red scheduled run alone reaches only the person who last edited the cron line.
16. **No change to `praxis.yaml`.** A static build on every `praxis/**` PR costs about 10 minutes. The release job and the weekly job catch a static-link break. Not in scope of #1723.
17. **No change to `release-smoke-linux.yaml`.** Its job count feeds the install statistics (see its header), so a new leg is not a local decision.

## Review Focus

1. A release where `build` passes, `build-cpex` passes and `build-gateway` fails on every leg: `publish` must run, publish no gateway asset, and warn. Check the `if:` of `publish` and the presence check (Task 2).
2. A release where `build-cpex` fails and `build-gateway` passes: `publish` must NOT run. The new `if:` must not open that path (Task 2).
3. `readelf` that fails or prints nothing (not an ELF file, or a missing tool): the check must fail, never read "no NEEDED lines" as static. Pinned by `test_readelf_error_fails` (Task 1).
4. A Homebrew OpenSSL path in `otool -L` (`/opt/homebrew/opt/openssl@3/lib/libssl.3.dylib`): the check must fail and name the library. Pinned by `test_darwin_homebrew_ssl_fails` (Task 1).
5. A binary that prints `cortex-gateway dev` in a release (`CORTEX_VERSION` not passed into the container): the check must fail, because the supervisor would then refuse it. Pinned by `test_version_mismatch_fails` (Task 1).

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Release cortex-gateway as a standalone binary` | (none) | T1, T2, T3, T4 | 600 |

## Gate

Run from the worktree root. It needs `pre-commit`, `shellcheck`, `yamllint`, a running `podman` machine, and Rust through rustup. It takes about 20 minutes on a cold cache.

```sh
pre-commit run --all-files \
&& shellcheck --severity=error scripts/build-gateway.sh scripts/check-gateway-linkage.sh scripts/check-gateway-linkage_test.sh \
&& sh scripts/check-gateway-linkage_test.sh \
&& yamllint -d '{extends: relaxed, rules: {line-length: {max: 150, level: warning}, truthy: {check-keys: false}, document-start: disable, comments: {min-spaces-from-content: 1}}}' .github/workflows \
&& VERSION=gate-test sh scripts/build-gateway.sh \
&& podman run --rm -v "$PWD:/src" -w /src -e VERSION=gate-test docker.io/library/rust:1.96.0-alpine sh scripts/build-gateway.sh \
&& podman run --rm -v "$PWD/dist:/d:ro" gcr.io/distroless/static-debian12 /d/cortex-gateway --version
```

---

### Task T1: Build script and linkage check

**Depends on:** none.

**Files:**
- Create: `scripts/build-gateway.sh`
- Create: `scripts/check-gateway-linkage.sh`
- Create: `scripts/check-gateway-linkage_test.sh`
- Modify: `.github/workflows/ci.yaml` (add a job after `release-notes-script`, same shape)

**Interfaces:**
- Produces: `sh scripts/build-gateway.sh`. Run from the repo root. Env `VERSION` is required (die with a message when empty). Writes `dist/cortex-gateway` and exits non-zero when the build or the check fails. Optional env `MACOSX_DEPLOYMENT_TARGET` (default `12.0`).
- Produces: `sh scripts/check-gateway-linkage.sh <binary> <version>`. Exit 0 when the binary is standalone and prints `cortex-gateway <version>`. Exit 1 otherwise, with the reason on stderr. Uses `uname -s` to pick `otool -L` (Darwin) or `readelf -d` (Linux). Any other OS: exit 1.

- [ ] **Step 1: Write the failing test `scripts/check-gateway-linkage_test.sh`.** Follow the shape of `scripts/release_notes_test.sh` (POSIX sh, one function per case, a failure counter, exit 1 when any case fails). Each case makes a temp dir with stub `uname`, `otool` and `readelf` scripts first on `PATH`, and a stub binary that prints a given `--version` line. Cases and expected results:
  - `test_linux_static_passes`: `uname` prints `Linux`, `readelf -d` prints `There is no dynamic section in this file.` and exits 0, binary prints `cortex-gateway v1.2.3`. Expect exit 0.
  - `test_linux_needed_fails`: `readelf -d` prints ` 0x0000000000000001 (NEEDED)             Shared library: [libssl.so.3]`. Expect exit 1, and stderr contains `libssl.so.3`.
  - `test_readelf_error_fails`: `readelf` prints nothing and exits 1. Expect exit 1.
  - `test_darwin_system_libs_pass`: `uname` prints `Darwin`, `otool -L` prints the binary path line, then `\t/usr/lib/libSystem.B.dylib (...)` and `\t/System/Library/Frameworks/Security.framework/Versions/A/Security (...)`. Expect exit 0.
  - `test_darwin_homebrew_ssl_fails`: `otool -L` also lists `\t/opt/homebrew/opt/openssl@3/lib/libssl.3.dylib (...)`. Expect exit 1, and stderr contains `libssl.3.dylib`.
  - `test_version_mismatch_fails`: static Linux stubs, binary prints `cortex-gateway dev`, version argument `v1.2.3`. Expect exit 1, and stderr contains `cortex-gateway dev`.
- [ ] **Step 2: Run** `sh scripts/check-gateway-linkage_test.sh`. Expected: FAIL (the script under test does not exist).
- [ ] **Step 3: Write `scripts/check-gateway-linkage.sh`.** Linux: run `readelf -d`, fail when it exits non-zero, fail and print each line that contains `(NEEDED)`. Darwin: run `otool -L`, skip its first line (the binary's own path), fail and print each dependency path that does not start with `/usr/lib/` or `/System/Library/`. Then run `<binary> --version` and compare it with `cortex-gateway <version>` exactly.
- [ ] **Step 4: Run** `sh scripts/check-gateway-linkage_test.sh` and `shellcheck --severity=error scripts/check-gateway-linkage.sh scripts/check-gateway-linkage_test.sh`. Expected: all cases pass, no shellcheck error.
- [ ] **Step 5: Write `scripts/build-gateway.sh`** (POSIX sh, `set -eu`). Order:
  1. Die when `VERSION` is empty.
  2. When `/etc/alpine-release` exists: `apk add --no-cache musl-dev perl make linux-headers`. (perl and make build `openssl-src`. make builds jemalloc.)
  3. `cd praxis && rustup toolchain install` (decision 3).
  4. `triple=$(rustc -vV | sed -n 's/^host: //p')`.
  5. Linux: `export RUSTFLAGS="-C target-feature=+crt-static"`. On `aarch64-*` also `export JEMALLOC_SYS_WITH_LG_PAGE=16` (decision 5). Darwin: `export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-12.0}"`.
  6. `CORTEX_VERSION="${VERSION}" CARGO_PROFILE_RELEASE_STRIP=symbols cargo build --release --locked -p cortex-gateway --target "${triple}"`.
  7. Copy `target/${triple}/release/cortex-gateway` to `../dist/cortex-gateway` (`mkdir -p ../dist`).
  8. From the repo root: `sh scripts/check-gateway-linkage.sh dist/cortex-gateway "${VERSION}"`.
  Each non-obvious line gets a one-line comment with its reason from the decisions above.
- [ ] **Step 6: Prove the static musl build first (the issue's first scope item).** Run `podman run --rm -v "$PWD:/src" -w /src -e VERSION=gate-test docker.io/library/rust:1.96.0-alpine sh scripts/build-gateway.sh`. Expected: exit 0 and the check passes. Then `podman run --rm -v "$PWD/dist:/d:ro" gcr.io/distroless/static-debian12 /d/cortex-gateway --version` prints `cortex-gateway gate-test`. If a native crate does not link statically, fix it in this script (an env var or an `apk` package) and record the fix in a comment. Do not change Praxis.
- [ ] **Step 7: Run the macOS build:** `VERSION=gate-test sh scripts/build-gateway.sh`. Expected: exit 0. `otool -L dist/cortex-gateway` lists only `/usr/lib/` and `/System/Library/` paths.
- [ ] **Step 8: Add the CI job** to `.github/workflows/ci.yaml`, named `gateway-linkage-script` ("check-gateway-linkage.sh tests"), `ubuntu-latest`, `timeout-minutes: 5`, checkout pinned as the neighbours are, then `run: sh scripts/check-gateway-linkage_test.sh`. One comment line: the check runs only at release time, so a regression would otherwise show only on a release.
- [ ] **Step 9: Commit** (`git commit -s`): `feat: Add the cortex-gateway release build and linkage check`.

### Task T2: The `build-gateway` release job

**Depends on:** T1.

**Files:**
- Modify: `.github/workflows/release-binaries.yaml` (header comment, new job after `build-cpex`, `publish` job)

**Interfaces:**
- Consumes: `scripts/build-gateway.sh` (T1).
- Produces: workflow artifacts `dist-gateway-<os>-<arch>`, each with `cortex-gateway_${TAG}_${os}_${arch}.tar.gz`.

- [ ] **Step 1: Resolve the image digests.** For `docker.io/library/rust:1.96.0-alpine`, `docker.io/library/alpine:3.22` and `gcr.io/distroless/static-debian12`, get the multi-arch index digest: `podman manifest inspect <image>` must list both `amd64` and `arm64`, and the digest to pin is the one of that index (`skopeo inspect --raw docker://<image> | shasum -a 256`, or `crane digest <image>`). Pin each as `<image>@sha256:<digest>`.
- [ ] **Step 2: Add the `build-gateway` job.** Model it on `build-cpex`: `needs: version`, `contents: read`, matrix `{os: darwin, arch: arm64, runner: macos-15}`, `{os: linux, arch: amd64, runner: ubuntu-latest}`, `{os: linux, arch: arm64, runner: ubuntu-24.04-arm}`, `fail-fast: false` (decision 8), `timeout-minutes: 60`. Env `TAG`, `VERSION` from `needs.version.outputs`. Steps:
  1. Checkout (pinned, `persist-credentials: false`).
  2. darwin: `sh scripts/build-gateway.sh`. linux: `docker run --rm -v "${GITHUB_WORKSPACE}:/src" -w /src -e VERSION "${RUST_ALPINE}" sh scripts/build-gateway.sh` (`RUST_ALPINE` is a job env with the pinned image, as `ALPINE_GO` is).
  3. linux only, the clean-host runs (decision 13): `dist/cortex-gateway --version` on the runner, then the same in the pinned `alpine` image and in the pinned distroless image, each with `-v "${GITHUB_WORKSPACE}/dist:/d:ro"`.
  4. `tar -C dist -czf "dist/cortex-gateway_${TAG}_${os}_${arch}.tar.gz" cortex-gateway`, then `rm -f dist/cortex-gateway`.
  5. Upload with the pinned `actions/upload-artifact`, `name: dist-gateway-${{ matrix.os }}-${{ matrix.arch }}`, `retention-days: 1`, `if-no-files-found: error`.
- [ ] **Step 3: Change `publish`.** `needs: [version, build, build-cpex, build-gateway]` and the `if:` from decision 9. In "Check every tarball is present": after the required checks, loop over the three gateway targets and emit `::warning::No cortex-gateway tarball for <os>/<arch>. This release ships without it.` for each missing one. Do not add them to `missing`. `checksums.txt` covers whatever is present, unchanged.
- [ ] **Step 4: Release notes.** In the publish step, when `ls dist/cortex-gateway_*.tar.gz` finds a file, add: ``'`cortex-gateway_*` is the praxis-mode gateway, for macOS arm64 and linux amd64/arm64'`` and ``'(static). Install it with `install.sh --praxis`.'`` (each line under 150 characters).
- [ ] **Step 5: Header comment.** Replace "Four jobs" with "Five jobs". Say that `build-gateway` is optional: a failed gateway leg does not block the release, and on `main-latest` the prune then removes the old gateway asset (decision 10).
- [ ] **Step 6: Verify.** Run the gate's `yamllint` line. Read the final `publish` `if:` and check Review Focus items 1 and 2 against it.
- [ ] **Step 7: Commit:** `feat: Build cortex-gateway in the release workflow`.

### Task T3: Weekly build against the latest Praxis

**Depends on:** T1.

**Files:**
- Create: `.github/workflows/praxis-latest.yaml`

**Interfaces:**
- Consumes: `scripts/build-gateway.sh` (T1).

- [ ] **Step 1: Write the workflow.** `on: schedule: [{cron: "17 6 * * 1"}]` and `workflow_dispatch`. Top-level `permissions: contents: read`. Job `build` on `ubuntu-latest`, `timeout-minutes: 90`:
  1. Checkout (pinned, `persist-credentials: false`).
  2. Resolve the version (decision 14): `curl -fsSL -A "rossoctl-cortex-ci (github.com/rossoctl/cortex)" https://crates.io/api/v1/crates/praxis-proxy | jq -r .crate.max_stable_version`. Fail when the result does not match `^[0-9]+\.[0-9]+\.[0-9]+$`. Print the pinned and the newest version.
  3. Rewrite the pins: `sed -i -E 's/^(praxis-proxy(-filter)? = \{ version = )"=[0-9.]+"/\1"='"$v"'"/' praxis/server/Cargo.toml praxis/filters/Cargo.toml`, then `git diff --stat` for the log.
  4. In `praxis/`: `rustup toolchain install`, `cargo update -p praxis-proxy -p praxis-proxy-filter`, `cargo test --locked`.
  5. `docker run --rm -v "${GITHUB_WORKSPACE}:/src" -w /src -e VERSION=praxis-latest <pinned rust:alpine> sh scripts/build-gateway.sh`. Use the same pinned image as T2 (copy the digest).
- [ ] **Step 2: Add the `report` job** (decision 15): `needs: build`, `if: failure()`, `permissions: issues: write`, env `GH_TOKEN: ${{ github.token }}`. Find the open issue with `gh issue list --repo "${GITHUB_REPOSITORY}" --state open --search 'in:title "Weekly build against the latest Praxis failed"' --json number --jq '.[0].number'`. When found, `gh issue comment` with the run URL (`${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}`). Otherwise `gh issue create` with that title and a body that has the run URL and one line: "The pinned Praxis release still builds. This job does not block anything."
- [ ] **Step 3: Header comment** (5 lines at most): what it builds, that it blocks nothing, where it reports.
- [ ] **Step 4: Verify.** Run the gate's `yamllint` line. Run the sed line of step 1.3 on a scratch copy of both manifests with `v=9.9.9` and check that `grep -n '"=9.9.9"'` finds exactly 3 lines (two in `server`, one in `filters`).
- [ ] **Step 5: Commit:** `ci: Build cortex-gateway weekly against the latest Praxis`.

### Task T4: Documentation

**Depends on:** T2, T3.

**Files:**
- Modify: `.github/CLAUDE.md` (workflow table, "Release binaries" section)
- Modify: `praxis/README.md` (new section "Release build (#1723)" after "Run the tests")

- [ ] **Step 1: `.github/CLAUDE.md`.** Add a table row for `praxis-latest.yaml` (weekly and manual, builds against the newest Praxis, opens or comments on one issue, blocks nothing). In "Release binaries", add one paragraph: `cortex-gateway_<ver>_<os>_<arch>.tar.gz` from `build-gateway` (darwin/arm64 natively, linux static musl in `rust:alpine`), built by `scripts/build-gateway.sh` and checked by `scripts/check-gateway-linkage.sh`. It is optional: a failed leg does not block the release. Change "jobs are `version`, `build`, `build-cpex` ... and `publish`" to include `build-gateway`, and say that `publish` requires every tarball except the gateway ones.
- [ ] **Step 2: `praxis/README.md`.** The new section says, in a short list: how to run the release build locally (the two `build-gateway.sh` commands from the gate), why OpenSSL is vendored (upstream links the system OpenSSL, so a plain build is not portable), the duty "Cortex owns OpenSSL security updates for `cortex-gateway`: an OpenSSL CVE needs an `openssl-src` bump (Dependabot proposes it) and a release", and "no FIPS claim for this binary".
- [ ] **Step 3: Run** `pre-commit run --all-files`. Expected: pass.
- [ ] **Step 4: Commit:** `docs: Describe the cortex-gateway release build`.

## Done when (from the issue), and who proves it

- The three tarballs run on a clean host with no OpenSSL package: T1 step 6 and 7 locally, T2 step 2 in the release (`--version` in distroless and alpine, and the linkage check).
- A Go release succeeds when the gateway job fails: T2 step 3 (`publish`'s `if:` and the warning instead of the error). Proof after merge: a `workflow_dispatch` dry run. The PR body says that this proof is still to do.

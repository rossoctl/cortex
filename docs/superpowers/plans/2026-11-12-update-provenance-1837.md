# Build provenance check for `agentop update` (#1837) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For a `v*` release tag, `agentop update` downloads the release tarballs itself, checks each SHA-256 against `checksums.txt` and each build-provenance attestation against a fixed policy, and only then runs `<stage>/agentop setup --from <stage>`. Other refs (`main`) keep the website `install.sh` path.

**Architecture:** `installRelease` (the seam shared with #1836) branches on the ref. For a `v*` tag (or `""`, resolved to the newest tag) it calls `stageRelease` (download, checksum, provenance callback, safe extract into a 0700 temp dir), then execs the staged agentop with the `hand_off` flags. The provenance check uses sigstore-go against the Sigstore public-good trusted root (TUF), with the bundle from the GitHub attestations API (no login).

**Tech Stack:** Go (module `github.com/rossoctl/cortex/cmd/agentop`, `go 1.26.10`), `github.com/sigstore/sigstore-go v1.3.0`, `archive/tar`, `compress/gzip`, `net/http/httptest` in tests.

**Spec:** GitHub issue rossoctl/cortex#1837 (body = the design; its "Decisions" section is final). Read it with `gh issue view 1837 -R rossoctl/cortex`.

## Global Constraints

- Base: `refs/base/provenance-1837` = `16fa0be3964dbdd9303b8eeafb03a6f0a30673f4`. Worktree `.claude/worktrees/provenance-1837`, branch `feat/update-provenance-1837`.
- All code is in `cmd/agentop` (package `main`). No new config option. No feature flag: this is a security fix to an existing command (the repository's feature-flag rule is in the kagenti repo, not in this one).
- Dependency: `github.com/sigstore/sigstore-go v1.3.0`. **Decision:** not v1.4.0, because v1.4.0 requires `go >= 1.27.0` and would force a `go` directive bump of the agentop module and the CI toolchain. v1.3.0 requires `go 1.25.8`. A spike on 2026-11-12 verified the fixture bundle with v1.3.0 (accepted for v0.9.5, refused for v0.9.6 with "expected SAN value").
- Policy values (exact):
  - issuer: `https://token.actions.githubusercontent.com`
  - SAN: `https://github.com/rossoctl/cortex/.github/workflows/release-binaries.yaml@refs/tags/<tag>`
  - verifier options: `verify.WithTransparencyLog(1)`, `verify.WithObserverTimestamps(1)` (the fixture bundle has 1 tlog entry and no TSA timestamps)
- Attestations API: `GET https://api.github.com/repos/rossoctl/cortex/attestations/sha256:<hex>`, headers `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`, and `Authorization: Bearer <token>` when `GH_TOKEN` is set, else when `GITHUB_TOKEN` is set. Response: `{"attestations":[{"bundle":{...},"bundle_url":"..."}]}`.
- Release assets: `https://github.com/rossoctl/cortex/releases/download/<tag>/<name>`.
- Tarball names: `agentop_<tag>_<os>_<arch>.tar.gz`; `cortex_<tag>_<os>_<arch>.tar.gz`, or `cortex-cpex_<tag>_<os>_<arch>.tar.gz` for cpex; plus `cortex-gateway_<tag>_<os>_<arch>.tar.gz` for praxis. Each tarball holds one binary: `agentop`, `cortex`, `cortex`, `cortex-gateway`.
- Fail closed: on any error nothing in `binDir` or `~/.cortex` changes, the stage is deleted, `installRelease` returns an error that names the reason. Do not write `~/.cortex/update.json`.
- Do not set `CORTEX_INSTALLER_DREW`; remove it from the staged agentop's environment.
- Commits: `git commit -s`, trailer `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`; no `Co-Authored-By`.
- Fixtures (committed with this plan, do not count toward PR size): `cmd/agentop/testdata/provenance/agentop_v0.9.5_darwin_arm64.attestations.json` (the API response for `agentop_v0.9.5_darwin_arm64.tar.gz`; one attestation whose statement lists all v0.9.5 tarballs) and `cmd/agentop/testdata/provenance/trusted_root.json` (Sigstore public-good trusted root). The digest of `agentop_v0.9.5_darwin_arm64.tar.gz` is `6c6205e6ec0869a4c8291d7d9a0f0cca7c054d494619721085f9416595787461`.

## Review Focus

1. The API returns an attestation whose `bundle` is null and only `bundle_url` is set: expect a clear refusal ("attestation for <name> has no inline bundle"), not a panic. Test in T2.
2. The API returns several attestations and only the second one passes: expect accept. Test in T2 (the fixture bundle twice, the first one with a changed signature).
3. `checksums.txt` lists the same tarball twice, or as `../name` or `/abs/name`: expect a refusal. Test in T3.
4. Ctrl-C during the download: expect the stage to be deleted and the process to exit non-zero with nothing installed. The download uses a context from `signal.NotifyContext`; T4 checks that a cancelled context makes `stageRelease` return an error and leave no stage (T3 test with a cancelled ctx).
5. `ref == ""` and the newest release cannot be read: expect exit 1 with "could not read the newest release", not a fall back to the unchecked installer. Test in T4.

## Decisions (made here, with reasons)

- **D1. `ref == ""` resolves to `latestRelease()` inside `installRelease`.** The newest stable release is a `v*` tag, so it must take the checked path. When it cannot be read, fail closed: a fall back to `install.sh` would let anyone who blocks one redirect skip the check.
- **D2. cpex and praxis come from `flags`** (`slices.Contains(flags, "--cpex")`, `"--praxis"`). `updateFlags` already computes them from `installedIsCpex` and `configPraxisEnabled`, so the seam signature stays as #1836 defines it.
- **D3. Setup flags = `flags` filtered to `--claude-code`, `--yes`, `--no-service`, `--install-only`, `--no-modify-path`, `--praxis`, in the order of `flags`.** `--cpex` and `--ref=…` are install.sh flags; setup does not take them.
- **D4. The stage is `os.MkdirTemp("", "cortex-stage.")`** (mode 0700, under `os.TempDir()`), so `isStagingDir` accepts it and setup deletes it on its way out. `installRelease` also does `defer os.RemoveAll(stage)`: harmless after setup deleted it, and it cleans up when setup exits on a usage error.
- **D5. Order inside `stageRelease`:** download `checksums.txt`; download and checksum every tarball; call `check` for every tarball; then extract every tarball. Nothing is extracted before every check passes.
- **D6. Tar entries:** accept `tar.TypeReg` and `tar.TypeDir` whose name passes `filepath.IsLocal(filepath.Clean(name))`; refuse everything else (absolute, `..`, symlink, hardlink, devices) with an error that names the entry. After extract, each expected binary must be a regular file in the stage; chmod 0755.
- **D7. Inline bundle only.** shortcut: a response with only `bundle_url` is refused (that blob is snappy-compressed and needs a new dependency); upgrade when GitHub stops inlining bundles.
- **D8. The trusted root is fetched per `checkProvenance` call** with `root.FetchTrustedRoot()` (sigstore-go's TUF default, cache under `~/.sigstore`). At most 3 calls per update. Through a package var so tests load the fixture.
- **D9. The bundle is fetched before the trusted root**, so an unreachable API fails with the API's name and needs no TUF.
- **D10. Download and API timeouts:** `http.Client{Timeout: 5 * time.Minute}` for release assets, `30 * time.Second` for the API. A non-200 answer is an error that names the URL's file and the status. API 404 = "no attestation for <name>".
- **D11. Seam T1 is the same move as #1836's T1** (same names: `installRelease`, `installReleaseFn`), so the second PR to merge has a trivial conflict.
- **D12. One PR.** About 1,250 counted lines, under 2,000.

## File Structure

- `cmd/agentop/cmd_update.go` — `runUpdate`, `installRelease` (T1, T4), `provenanceCheckFn`, hand-off (T4), `updateUsage` text (T4).
- `cmd/agentop/update_provenance.go` (new, T2) — attestation fetch and policy.
- `cmd/agentop/update_stage.go` (new, T3) — tarball names, checksums, download, extract.
- Tests: `cmd_update_test.go` (T1 unchanged, T4), `update_provenance_test.go` (T2), `update_stage_test.go` (T3).
- Docs: `docs/laptop-service.md`, `docs/proxy-contract.md` (T5).

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | feat: Check build provenance before agentop update installs a release | "" | T1, T2, T3, T4, T5 | 1250 |

## Tasks

| Task | Title | PR | Depends on |
|---|---|---|---|
| T1 | Move the installer run into `installRelease` | P1 | — |
| T2 | Attestation fetch and provenance policy | P1 | — |
| T3 | Download, checksum and stage a release | P1 | — |
| T4 | Checked install path for `v*` tags and the hand-off | P1 | T1, T2, T3 |
| T5 | Docs for the checked update path | P1 | — |

---

### Task T1: Move the installer run into `installRelease`

Depends on: none. Est. 40 lines.

**Files:**
- Modify: `cmd/agentop/cmd_update.go` (the exec block of `runUpdate`, lines 82-97 at base)

**Interfaces:**
- Produces:
  ```go
  // installRelease runs the installer for ref ("" = newest release) with flags.
  func installRelease(env *setupEnv, ref string, flags []string, stdout, stderr io.Writer) error
  // installReleaseFn is what callers use, so tests can replace the installer.
  var installReleaseFn = installRelease
  ```
  On an installer failure the error wraps the `*exec.ExitError` (return the `cmd.Run()` error as it is). `env` and `ref` are not used in the body yet.

- [ ] **Step 1:** Move the `exec.Command("sh", …updateScript, installerURL…)` line, `cmd.Stdout, cmd.Stderr`, the signal comment, `signal.Notify`/`defer signal.Stop` and `cmd.Run()` into `installRelease`. `runUpdate` calls `installReleaseFn(env, *ref, updateFlags(env, *ref, yes), stdout, stderr)`. On error: `errors.As(err, &exit)` → `return exit.ExitCode()`; else print `agentop: %v` and return 1.
- [ ] **Step 2:** Run `cd cmd/agentop && go test -count=1 -run 'TestUpdate' ./...`. Expected: PASS with no test change.
- [ ] **Step 3:** Commit `refactor: Move the installer run of agentop update into installRelease`.

### Task T2: Attestation fetch and provenance policy

Depends on: none. Est. 380 lines (code 130, tests 180, go.mod 70; go.sum not counted).

**Files:**
- Create: `cmd/agentop/update_provenance.go`, `cmd/agentop/update_provenance_test.go`
- Modify: `cmd/agentop/go.mod`, `cmd/agentop/go.sum` (`go get github.com/sigstore/sigstore-go@v1.3.0 && go mod tidy`)
- Fixtures: `cmd/agentop/testdata/provenance/*` (already committed)

**Interfaces:**
- Produces:
  ```go
  var attestationsAPI = "https://api.github.com/repos/rossoctl/cortex/attestations/"
  var provenanceTrustedMaterial = func() (root.TrustedMaterial, error) { return root.FetchTrustedRoot() }
  type provenanceWant struct{ issuer, san string }
  func releaseProvenance(tag string) provenanceWant // the Global Constraints values
  // fetchAttestations returns the inline bundles for the sha256 digest; name is only for messages.
  func fetchAttestations(ctx context.Context, name string, digest []byte) ([]json.RawMessage, error)
  // verifyBundles is nil when at least one bundle passes the policy for digest.
  func verifyBundles(name string, bundles []json.RawMessage, digest []byte, want provenanceWant, tm root.TrustedMaterial) error
  // checkProvenance = fetchAttestations, then provenanceTrustedMaterial, then verifyBundles with releaseProvenance(tag).
  func checkProvenance(ctx context.Context, name string, digest []byte, tag string) error
  ```
- Error texts (tests match these substrings): `could not reach the GitHub attestation API` (transport error; include the URL), `GitHub attestation API answered <status> for <name>`, `no attestation for <name>` (404 or empty list), `has no inline bundle`, `Sigstore trusted root` (trusted material error), `provenance of <name>` (prefix of a verify failure, wrapping sigstore-go's error).

Verification uses `bundle.Bundle.UnmarshalJSON`, `verify.NewVerifier(tm, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))`, `verify.NewShortCertificateIdentity(want.issuer, "", want.san, "")`, `verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id))`.

- [ ] **Step 1: Write the failing tests** in `update_provenance_test.go`. Helper `provenanceFixture(t) (bundles []json.RawMessage, tm root.TrustedMaterial, digest []byte)` loads the two fixtures (`root.NewTrustedRootFromJSON`). Tests:
  - `TestProvenanceAccepted`: `verifyBundles(n, bundles, digest, releaseProvenance("v0.9.5"), tm)` is nil.
  - `TestProvenanceWrongDigest`: digest with its last byte XOR 1 → error.
  - `TestProvenanceWrongSANRepository`: `want.san` with `rossoctl/cortex` replaced by `rossoctl/other` → error containing `SAN`.
  - `TestProvenanceWrongSANTag`: `releaseProvenance("v0.9.6")` → error containing `refs/tags/v0.9.6`.
  - `TestProvenanceWrongIssuer`: `want.issuer = "https://accounts.google.com"` → error.
  - `TestProvenanceChangedSignature`: decode the bundle into `map[string]any`, base64-decode `dsseEnvelope.signatures[0].sig`, XOR byte 10 with 1, re-encode → error.
  - `TestProvenanceSecondBundlePasses`: `[changed, good]` → nil.
  - `TestFetchAttestations`: an `httptest` server serving the fixture at path `/sha256:<hex>`; set `attestationsAPI = srv.URL + "/"`. Cases: `GH_TOKEN=a` → header `Bearer a`; only `GITHUB_TOKEN=b` → `Bearer b`; neither → no `Authorization` header; 404 → `no attestation for`; `{"attestations":[{"bundle":null,"bundle_url":"x"}]}` → `has no inline bundle`; a closed server → `could not reach the GitHub attestation API`.
  - `TestCheckProvenanceEndToEnd`: server serves the fixture, `provenanceTrustedMaterial` returns the fixture root → `checkProvenance(ctx, "agentop_v0.9.5_darwin_arm64.tar.gz", digest, "v0.9.5")` is nil, and with tag `v0.9.6` is an error.
  Restore every replaced var with `t.Cleanup`.
- [ ] **Step 2:** `cd cmd/agentop && go test -count=1 -run 'Provenance|FetchAttestations' ./...`. Expected: FAIL (undefined names).
- [ ] **Step 3:** `go get github.com/sigstore/sigstore-go@v1.3.0`, implement `update_provenance.go`, `go mod tidy`. Check that the `go` directive in `go.mod` stays `1.26.10`.
- [ ] **Step 4:** Same test command. Expected: PASS. Then `GOWORK=off go mod tidy -diff` prints nothing.
- [ ] **Step 5:** Commit `feat: Check release attestations against the release workflow policy`.

### Task T3: Download, checksum and stage a release

Depends on: none. Est. 420 lines (code 180, tests 240).

**Files:**
- Create: `cmd/agentop/update_stage.go`, `cmd/agentop/update_stage_test.go`

**Interfaces:**
- Produces:
  ```go
  var releaseDownloadBase = "https://github.com/rossoctl/cortex/releases/download/"
  // releaseTarballs is the agentop tarball, then the cortex (or cortex-cpex) one, then cortex-gateway when praxis.
  func releaseTarballs(tag, goos, goarch string, cpex, praxis bool) []string
  // checksumFor is the lower-case hex SHA-256 for name in a checksums.txt body.
  func checksumFor(sums []byte, name string) (string, error)
  // extractTarball extracts a .tar.gz into dir under rule D6.
  func extractTarball(path, dir string) error
  // stageRelease downloads names for tag into a new stage (D4, D5). size is the
  // total tarball bytes. On error it removes the stage and returns stage "".
  func stageRelease(ctx context.Context, tag string, names []string, check func(name string, digest []byte) error) (stage string, size int64, err error)
  ```
- `checksumFor` accepts a line that matches `^[0-9a-fA-F]{64}[ \t]+\*?(\./)?<regexp.QuoteMeta(name)>$` (as `scripts/install.sh` does, with the hash length fixed at 64). No match → `checksums.txt has no usable entry for <name>`. More than one match → `checksums.txt lists <name> more than once`.
- Other errors (substrings): `download failed: <name>` (with status or transport error), `checksum mismatch for <name>`, `unsafe entry <entry> in <tarball>`, `archive did not contain expected binary: <bin>`.
- Expected binary per tarball prefix: `agentop_` → `agentop`, `cortex_` and `cortex-cpex_` → `cortex`, `cortex-gateway_` → `cortex-gateway`.

- [ ] **Step 1: Write the failing tests** in `update_stage_test.go`. Helper `tarGz(t, entries ...tarEntry) []byte` (`tarEntry{name string; typ byte; body string; link string}`) and `releaseServer(t, tag string, files map[string][]byte)` that serves `/<tag>/<name>` and sets `releaseDownloadBase = srv.URL + "/"`. Set `TMPDIR` to `t.TempDir()` in each stage test. Tests:
  - `TestReleaseTarballs`: tag `v1.2.3`, `darwin`/`arm64`. Default → `[agentop_v1.2.3_darwin_arm64.tar.gz cortex_v1.2.3_darwin_arm64.tar.gz]`; cpex → second is `cortex-cpex_v1.2.3_darwin_arm64.tar.gz`; praxis → a third `cortex-gateway_v1.2.3_darwin_arm64.tar.gz`.
  - `TestChecksumFor`: `HASH  ./n`, `HASH  n`, `HASH *n` accepted (upper-case hash returned lower-case); `HASH  ../n`, `HASH  /etc/n`, `HASH  xn`, a 63-char hash refused; no line → `no usable entry`; two lines → `more than once`.
  - `TestStageRelease`: two tarballs (`agentop` and `cortex` scripts), right checksums. Expect: `check` called once per name, in order, with `sha256.Sum256` of each tarball; stage under `TMPDIR` with mode 0700; `stage/agentop` and `stage/cortex` mode 0755; `size` = sum of tarball lengths.
  - `TestStageReleaseChecksumMismatch`: wrong hash for the cortex tarball → error `checksum mismatch for cortex_…`; `check` never called; `TMPDIR` is empty.
  - `TestStageReleaseCheckFails`: `check` returns `errors.New("bad provenance")` for the second name → error wraps it; nothing extracted (no stage); `TMPDIR` empty.
  - `TestStageReleaseMissingFile`: server 404s a tarball → `download failed:`; `TMPDIR` empty.
  - `TestStageReleaseCancelled`: ctx already cancelled → error; `TMPDIR` empty.
  - `TestExtractTarballRefusesUnsafeEntries`: one subtest each for `/abs`, `../up`, `a/../../up`, a symlink, a hardlink → error containing `unsafe entry`, and no file outside `dir`.
  - `TestStageReleaseMissingBinary`: agentop tarball holds `other` → `archive did not contain expected binary: agentop`.
- [ ] **Step 2:** `cd cmd/agentop && go test -count=1 -run 'ReleaseTarballs|ChecksumFor|StageRelease|ExtractTarball' ./...`. Expected: FAIL (undefined names).
- [ ] **Step 3:** Implement `update_stage.go`. Hash while downloading (`io.MultiWriter`); use `http.NewRequestWithContext`; set the D10 timeout.
- [ ] **Step 4:** Same test command. Expected: PASS.
- [ ] **Step 5:** Commit `feat: Download, check and stage a release in agentop update`.

### Task T4: Checked install path for `v*` tags and the hand-off

Depends on: T1, T2, T3. Est. 380 lines (code 110, tests 270).

**Files:**
- Modify: `cmd/agentop/cmd_update.go` (`installRelease` body, new `provenanceCheckFn`, `updateUsage`)
- Modify: `cmd/agentop/cmd_update_test.go`

**Interfaces:**
- Consumes: `installRelease`, `installReleaseFn` (T1); `checkProvenance`, `attestationsAPI`, `provenanceTrustedMaterial` (T2); `releaseTarballs`, `stageRelease`, `releaseDownloadBase` (T3); existing `latestRelease`, `latestReleaseURL`, `updateScript`, `installerURL`.
- Produces: `var provenanceCheckFn = checkProvenance` (tests replace it).

`installRelease` after this task:
1. `ref == ""` → `ref = latestRelease()`; still `""` → `errors.New("could not read the newest release")` (D1).
2. `!strings.HasPrefix(ref, "v")` → the T1 body (website installer), unchanged.
3. Else: `ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`; `start := time.Now()`; `stageRelease(ctx, ref, releaseTarballs(ref, runtime.GOOS, runtime.GOARCH, cpex, praxis), func(n string, d []byte) error { return provenanceCheckFn(ctx, n, d, ref) })` (D2); `stop()`; on error return it; `defer os.RemoveAll(stage)` (D4).
4. Args: `setup --from <stage>`, then the D3 flags, then `--handoff-bytes=<max(size,1)>` and `--handoff-seconds=<int(time.Since(start).Seconds())>`.
5. `exec.Command(filepath.Join(stage, "agentop"), args...)` with `Stdin = os.Stdin`, `Stdout`, `Stderr`, `Env` = `os.Environ()` without any `CORTEX_INSTALLER_DREW=` entry. Keep the `signal.Notify`/`defer signal.Stop` block and its comment around `cmd.Run()`. Return the `cmd.Run()` error as it is.

`updateUsage`: replace the paragraph "Runs the same installer as the one-liner. The installer downloads and verifies the release, then gives it to agentop setup." with: "For a release (the newest, or --ref=vX), agentop downloads the release itself, checks each archive's SHA-256 against checksums.txt and its build provenance against GitHub's attestation for this repository's release workflow and that tag, and only then gives it to the new agentop setup. Any failed check stops the update and changes nothing. --ref=main runs the same installer as the one-liner." Keep the rest. Change "Exit status: … else the installer's, which is setup's once it hands off." to "Exit status: 0 already current, 1 when a download or check fails, else setup's (or the installer's for --ref=main)."

- [ ] **Step 1: Write the failing tests** in `cmd_update_test.go`. Helper `fakeRelease(t, tag string, cpex, praxis bool) (setupArgs string, checked *[]string)`: builds the `releaseTarballs(tag, runtime.GOOS, runtime.GOARCH, cpex, praxis)` tarballs (agentop = `#!/bin/sh\nprintf '%s\n' "$@" "drew=${CORTEX_INSTALLER_DREW-unset}" > "$FAKE_SETUP_ARGS"\necho staged setup\nexit 7\n`; cortex and cortex-gateway = any script), serves them with `checksums.txt` (T3's `releaseServer` pattern), sets `TMPDIR` to `t.TempDir()`, sets `FAKE_SETUP_ARGS` to a file path, and sets `provenanceCheckFn` to a recorder that appends the name and returns nil. Tests:
  - `TestUpdateHandsOffToTheStagedSetup`: scene `installed(t, "--claude-code", "--no-service")`, cortex is the cpex build (`echo cortex \(cpex\) v9.9.9`), `t.Setenv("CORTEX_INSTALLER_DREW", "1")`, `runUpdate([]string{"--ref=v9.9.9-rc.1", "-y"})`. Expect exit 7; stdout `staged setup\n`; recorded names = the agentop and `cortex-cpex_` tarballs; args lines = `setup`, `--from`, `<a dir under TMPDIR>`, `--claude-code`, `--no-service`, `--yes`, `--handoff-bytes=<sum of tarball sizes>`, `--handoff-seconds=<digits>`, `drew=unset`; `TMPDIR` is empty after the run.
  - `TestInstallReleasePraxis`: `installRelease(env, "v9.9.9", []string{"--praxis"}, …)` → names include `cortex-gateway_v9.9.9_…`; args include `--praxis`.
  - `TestUpdateChecksumMismatchChangesNothing`: `fakeRelease`, then serve a wrong hash for the agentop tarball. Record the SHA-256 of every file in `binDir` before; `runUpdate([]string{"--ref=v9.9.10", "-y"})` → exit 1, stderr contains `checksum mismatch for agentop_v9.9.10`; same hashes after; no recorded provenance call; `TMPDIR` empty.
  - `TestUpdateUnreachableAttestationAPIFailsClosed`: `fakeRelease`, then `provenanceCheckFn = checkProvenance`, `attestationsAPI` = URL of a closed `httptest` server, `provenanceTrustedMaterial` fails the test if called. `installRelease(env, "v9.9.10", nil, …)` → error containing `attestation API`; `FAKE_SETUP_ARGS` file not written; `binDir` hashes unchanged.
  - `TestInstallReleaseMainUsesTheWebsiteInstaller`: `fakeInstaller(t, false)`; `installRelease(env, "main", []string{"--ref=main"}, …)` → error with `*exec.ExitError` code 7, stdout `installer: --ref=main\n`.
  - `TestUpdateFailsClosedWithoutTheNewestRelease`: `latestIs(t, "")`, `runUpdate(nil)` → exit 1, stderr contains `could not read the newest release`.
  - Rework `TestUpdateSkipsTheInstallerWhenCurrent`: keep cases `newest`, `pinned`, `main`. Delete `newest unknown` (now the test above). Cases `older` (latest `v9.9.10`), `pinned to another` (`--ref=v9.9.8`) and `cortex behind` now call `fakeRelease` for their tag and want exit 7 and stdout `staged setup\n`.
  - Rework `TestUpdateRunsTheInstallerAndKeepsTheInstall` into the hand-off test above (delete the old one).
  - `TestUpdateFailsWhenTheDownloadFails`: add `--ref=main` to its args so it still checks curl's error through the website installer; add a subtest with `--ref=v9.9.10` and a release server that 404s everything → exit 1, stderr contains `download failed: checksums.txt`.
- [ ] **Step 2:** `cd cmd/agentop && go test -count=1 -run 'Update|InstallRelease' ./...`. Expected: FAIL.
- [ ] **Step 3:** Implement the `installRelease` body and `updateUsage` text above.
- [ ] **Step 4:** Same test command, then `go test -race -count=1 ./...`. Expected: PASS.
- [ ] **Step 5:** Commit `feat: Check build provenance before agentop update installs a release (#1837)`.

### Task T5: Docs for the checked update path

Depends on: none. Est. 30 lines.

**Files:**
- Modify: `docs/laptop-service.md` (section "Re-running the installer is safe", first sentence about `agentop update`)
- Modify: `docs/proxy-contract.md` (section "`install.sh` flags", the paragraph that starts "`scripts/install.sh` parses these")

Facts to state (and nothing more):
- For a release tag (the newest release, or `--ref=vX`), `agentop update` does not run `install.sh`. The installed agentop downloads `checksums.txt` and the tarballs from `https://github.com/rossoctl/cortex/releases/download/<tag>/`, checks each SHA-256, checks each tarball's build-provenance attestation, extracts into a temp stage, and runs `<stage>/agentop setup --from <stage>`.
- The attestation must come from `https://github.com/rossoctl/cortex/.github/workflows/release-binaries.yaml@refs/tags/<tag>`, issuer `https://token.actions.githubusercontent.com`, with a transparency-log entry checked against the Sigstore public-good trusted root. The bundle comes from `https://api.github.com/repos/rossoctl/cortex/attestations/sha256:<digest>` with no login; `GH_TOKEN` or `GITHUB_TOKEN` is sent when set, for the rate limit.
- A missing, invalid or unreachable attestation stops the update and changes nothing.
- The flags sent to setup are the `hand_off` set: `--claude-code`, `--yes`, `--no-service`, `--install-only`, `--no-modify-path`, `--praxis`, plus `--handoff-bytes` and `--handoff-seconds`. `--cpex` selects the `cortex-cpex_` tarball; `--praxis` adds the `cortex-gateway_` tarball.
- `--ref=main` (any ref that does not start with `v`) still runs the website `install.sh` with the `updateFlags` flags. The documented one-liner and a first install still use `install.sh`, with no provenance check.
- Users can check a tarball by hand with `gh attestation verify <tarball> --repo rossoctl/cortex`.

- [ ] **Step 1:** Edit both sections.
- [ ] **Step 2:** `pre-commit run --files docs/laptop-service.md docs/proxy-contract.md` passes (or reports no hooks for these files).
- [ ] **Step 3:** Commit `docs: Describe the provenance check in agentop update`.

## Gate

From the worktree root:

```sh
cd cmd/agentop && test -z "$(gofmt -l .)" && go vet ./... && go build ./... && go test -race -count=1 ./... && GOWORK=off go mod tidy -diff && golangci-lint run --new-from-rev=16fa0be3964dbdd9303b8eeafb03a6f0a30673f4 ./...
```

## For a human after review

These need a real network, real releases or a person's decision. They need T1-T4 merged or built locally.

1. Build agentop from the branch (`make agentop`). On a Mac and on Linux, with a real network and no `gh` login, run `./bin/agentop update --ref=<newest tag> --yes`. Check that the update installs, and that `agentop doctor` is clean.
2. Check that every tarball kind of the newest release has an attestation: for each `agentop_`, `cortex_`, `cortex-cpex_`, `cortex-gateway_` tarball, `curl -s https://api.github.com/repos/rossoctl/cortex/attestations/sha256:<sha256 from checksums.txt> | jq '.attestations | length'` is at least 1.
3. Release order: do not tag a release that contains #1836 (auto-update) without #1837. Merge order does not matter.
4. Whichever of #1836 and #1837 merges second: resolve the conflict in `installRelease` so that the body is this plan's T4 body.

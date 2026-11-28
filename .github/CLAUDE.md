# .github — agent notes: CI, images and releases

Loaded by Claude Code when you read a file under `.github/`. The repo-wide rules,
including the PR title format and the Go checks that CI gates on, are in the root
[`CLAUDE.md`](../CLAUDE.md).

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-*, push to main | Pre-commit; `go vet`, build and tests for all 7 modules, and for each `cmd/cortex` and `cmd/cortex-envoy` profile that `Build + test each shipped profile` builds (`local`, `full`, `lite`, `hostegress`, `envoy`), failing any but `hostegress` that links `core/listener/vmcontrol`; a macOS job for `peerproc`, `tlsbridge`, `cmd/cortex` and `cmd/agentop`; a Windows job that builds core and `cmd/cortex` and tests `bootstrap`, the session archive and the cost ledger, without `-race` (`go-ci-windows`); the cgo `cpex` builds (`go-ci-cpex`); `go mod tidy -diff` for all 7 modules; proxy-init iptables, Python, install-script, release-notes-script and release-promote-script tests. `go fmt` rewrites rather than fails, so it does not gate |
| `build.yaml` | Tag push (`v*`), push to main, manual; PRs to main build amd64 only and skip `**.md` | Multi-arch Docker builds for the six images below. Every Go image passes `GO_BUILD_TAGS` naming a profile — plugins are all opt-in, so an image built without tags registers none |
| `praxis.yaml` | PR to main/release-*, push to main, on `praxis/**`, `docs/praxis-policy.md` or the Go parser paths it ports | `cargo fmt --check`, clippy and tests for `praxis/`. A failure blocks only when `praxis/`, the policy page, the workflow, or the Go side of praxis mode (the hop, the egress listener, `cmd/cortex`'s praxis wiring) changed |
| `security-scans.yaml` | PR to main | Dependency review, shellcheck, YAML lint, Hadolint, Bandit, Trivy |
| `codeql.yaml` | PR to main (skips `**.md`), every push to main | CodeQL for Go and Python. The push run is the baseline that the PR check compares with, so an alert already on main is not new for a PR |
| `scorecard.yaml` | Weekly / push to main | OpenSSF Scorecard security health metrics |
| `praxis-latest.yaml` | Weekly (Monday), manual | Builds and tests `cortex-gateway` against the newest Praxis on crates.io. On failure it opens one issue, or comments on the open one. It blocks nothing |
| `release-binaries.yaml` | Tag push (`v*`), manual; push to main publishes the main-latest channel when `MAIN_CHANNEL_ENABLED` is set | Release tarballs — see [Release binaries](#release-binaries) |
| `pr-verifier.yml` | PR | PR title check |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files — **disabled** (`if: false`) |
| `site-dispatch.yaml` | Push to main on `scripts/install.sh`, manual | Sends `docs-updated` to `rossoctl/.github`, so the site's `/cortex/install.sh` copy updates in minutes, not at its daily schedule. Needs the `SITE_DISPATCH_TOKEN` secret; without it, a warning only |

Also: `release-smoke-linux.yaml` and `release-smoke-macos.yaml` (install a just-published release as a user does, on each platform; both counted by `scripts/collect-install-stats.sh`), `release-promote.yaml` (see [Release binaries](#release-binaries)), `dependabot-tidy.yml`, `install-stats.yaml`, `project.yml`, `self-assign.yml`, `stale.yaml`.

`ci.yaml`'s Python job pip-installs `python-keycloak==5.3.1`, two majors behind the
`>=7.1.1,<8` that `requirements.txt` declares. CI does not test the declared version.

## Container Images

All images are pushed to `ghcr.io/rossoctl/cortex/` from `build.yaml`:

| Image | Source | Description |
|-------|--------|-------------|
| **`authbridge`** | **`cmd/cortex/Dockerfile`** | **proxy-sidecar image (default mode): cortex, full plugin set incl. parsers. No Envoy.** |
| `authbridge-envoy` | `cmd/cortex-envoy/Dockerfile` | envoy-sidecar combined image: Envoy + cortex-envoy (ext_proc, `envoy` profile) |
| `authbridge-lite` | `cmd/cortex/Dockerfile` (+ `GO_BUILD_TAGS` from the `lite` profile) | proxy-sidecar image with a sidecar-minimum plugin set (see `scripts/profile-tags`). A build variant of `authbridge`, not a separate binary; not yet referenced by the operator's default config |
| `authbridge-cpex` | `cmd/cortex-cpex/Dockerfile` | proxy-sidecar build of `cmd/cortex` with the CPEX plugin. Two tag sources: the literal `cpex` tag, always required because it selects the FFI-backed cpex manager and gates `core/plugins/all/plugins_cpex.go`, plus the plugin tags its Dockerfile appends from `GO_BUILD_TAGS` (resolved from the `cpex` profile in `build.yaml`) — `cpex` alone registers no plugins. Links `libcpex_ffi.a` from a pinned CPEX release (CGO_ENABLED=1), fetched and verified by `scripts/fetch-cpex-ffi.sh`. The pins are in `cmd/cortex-cpex/`: `CPEX_FFI_VERSION` is the release tag, `CPEX_FFI_ABI` the FFI ABI integer, `CPEX_FFI_SHA256` the per-target tarball digests |
| `proxy-init` | `deploy/proxy-init/Dockerfile.init` | Alpine + iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes) |
| `sparc-service` | `deploy/sparc-service/Dockerfile` | Python SPARC reflection service (FastAPI wrapper around the ALTK pre-tool reflection component), called by the `sparc` plugin |

CI no longer builds the legacy `authbridge-unified`, `authbridge-light`,
`client-registration` and `spiffe-helper` images. Older release tags still publish them.

### Adding a New Component Image to CI

1. Add an entry to the `build.yaml` matrix (`image_config` array), with `name`, `context` and `dockerfile` fields.
2. For a Go image, add a case to the "Resolve build args" step that sets `GO_BUILD_TAGS` from a profile. Without it, the Dockerfile fails.
3. The image is pushed to `ghcr.io/rossoctl/cortex/<name>`.

## Release binaries

`v*` tag pushes trigger `release-binaries.yaml`, which cross-compiles `cortex` and
`agentop` for linux/darwin × amd64/arm64 and attaches tarballs to the GitHub Release.
Its default `cortex` tarball, `cortex_<ver>_<os>_<arch>.tar.gz`, is built with the
default plugin set (the `authbridge` image's). The `lite` profile ships only as the
`authbridge-lite` image, and opt-in plugins such as `sessionbudget` and
`context-guru` are built from source. To publish a pure-Go variant, append a
`<suffix>:<build-tags>` entry to the `proxy_variants` array in the workflow — never
enumerate combos.

The laptop cpex build is removed, and so is the `build-cpex` job. `publish` still
writes `cortex-cpex_*` tarballs for darwin/arm64 and linux/amd64/arm64, as copies of
the `cortex_*` ones: an agentop from before the removal asks for that name, and the copy
lets it move a cpex install to the default build. `install.sh --cpex` exits with an error that
names `--praxis` and `docs/praxis-policy.md`. An install that runs a cpex build moves to
the default `cortex` on `agentop update`, which prints one line that says so. When
its config names the `cpex` plugin, the update stops before it downloads, because the
default `cortex` does not start on that config. The `authbridge-cpex` image is unchanged.

The praxis-mode gateway ships as `cortex-gateway_<ver>_<os>_<arch>.tar.gz`, from the
`build-gateway` matrix job: darwin/arm64 natively, and linux/amd64 and linux/arm64 as
static musl binaries in `rust:alpine`. `scripts/build-gateway.sh` builds it and
`scripts/check-gateway-linkage.sh` checks that it is standalone. It is optional: a
failed `build-gateway` leg does not block the release.

The workflow's jobs are `version`, `build`, `build-gateway` (all
`contents: read`) and `publish`. Only `publish` writes. It refuses to publish unless
every expected tarball arrived, except the gateway tarballs: a missing one gives a
warning. It writes one `checksums.txt` over all of them, and attests every tarball's
build provenance.

A new stable release (`vX.Y.Z`) is created as a **prerelease**, so nothing installs it
yet. When both release smoke workflows have passed for the release run that published
it, `release-promote.yaml` (`scripts/release_promote.sh`) clears the flag. It also sets
Latest, unless a higher stable version exists. The promote workflow finds that release
run from the end of each smoke run's title, `, release run <id>`: a run started by
`workflow_run` records it nowhere else, so the smoke workflows' `run-name:` lines are
part of the contract. A tag push runs the `release-binaries.yaml` in the tagged commit, so
only a tag on a commit that has this step is staged at all: one on an older commit, such
as a backport from `release-0.6` or `release-0.7`, publishes an ordinary release that
takes Latest. Only a release the publish step **creates** is staged. An existing one
keeps its flags, because the auto-updater follows Latest both ways, and flagging a
live release would take installs back a version. CONTRIBUTING's "A release is tested
before it is Latest" is the maintainer-facing version.

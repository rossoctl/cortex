# .github — agent notes: CI, images and releases

Loaded by Claude Code when you read a file under `.github/`. The repo-wide rules,
including the PR title format and the Go checks that CI gates on, are in the root
[`CLAUDE.md`](../CLAUDE.md).

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-*, push to main | Pre-commit; `go vet`, build and tests for all 7 modules; a macOS job for `peerproc`, `tlsbridge`, `cmd/cortex` and `cmd/agentop`; the cgo `cpex` builds (`go-ci-cpex`); `go mod tidy -diff` for all 7 modules; proxy-init iptables, Python, install-script and release-notes-script tests. `go fmt` rewrites rather than fails, so it does not gate |
| `build.yaml` | Tag push (`v*`), push to main, manual; PRs to main build amd64 only and skip `**.md` | Multi-arch Docker builds for the six images below. Every Go image passes `GO_BUILD_TAGS` naming a profile — plugins are all opt-in, so an image built without tags registers none |
| `praxis.yaml` | PR to main/release-*, push to main, on `praxis/**` or the Go parser paths it ports | `cargo fmt --check`, clippy and tests for `praxis/`. A failure blocks only when `praxis/` or the workflow changed |
| `security-scans.yaml` | PR to main | Dependency review, shellcheck, YAML lint, Hadolint, Bandit, Trivy, CodeQL |
| `scorecard.yaml` | Weekly / push to main | OpenSSF Scorecard security health metrics |
| `praxis-latest.yaml` | Weekly (Monday), manual | Builds and tests `cortex-gateway` against the newest Praxis on crates.io. On failure it opens one issue, or comments on the open one. It blocks nothing |
| `release-binaries.yaml` | Tag push (`v*`), manual; push to main publishes the main-latest channel when `MAIN_CHANNEL_ENABLED` is set | Release tarballs — see [Release binaries](#release-binaries) |
| `pr-verifier.yml` | PR | PR title check |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files — **disabled** (`if: false`) |
| `site-dispatch.yaml` | Push to main on `scripts/install.sh`, manual | Sends `docs-updated` to `rossoctl/.github`, so the site's `/cortex/install.sh` copy updates in minutes, not at its daily schedule. Needs the `SITE_DISPATCH_TOKEN` secret; without it, a warning only |

Also: `release-smoke-linux.yaml`, `dependabot-tidy.yml`, `install-stats.yaml`, `project.yml`, `self-assign.yml`, `stale.yaml`.

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

The one other variant is `cortex-cpex_<ver>_<os>_<arch>.tar.gz`: `cortex` built with
cgo, the `cpex` tag and the `fullcpex` profile (`full` + `cpex`). It cannot
cross-compile, so a separate `build-cpex` matrix job builds it natively for
darwin/arm64 (`macos-15`, macOS 14.5 floor) and linux/amd64 and linux/arm64 (static
musl, in `golang:alpine` via `docker run`). There is no darwin/amd64 FFI. The
executable inside is still `cortex`, and `cortex --version` prints
`cortex (cpex) <ver>`. `install.sh --cpex` installs it.

The praxis-mode gateway ships as `cortex-gateway_<ver>_<os>_<arch>.tar.gz`, from the
`build-gateway` matrix job: darwin/arm64 natively, and linux/amd64 and linux/arm64 as
static musl binaries in `rust:alpine`. `scripts/build-gateway.sh` builds it and
`scripts/check-gateway-linkage.sh` checks that it is standalone. It is optional: a
failed `build-gateway` leg does not block the release.

The workflow's jobs are `version`, `build`, `build-cpex`, `build-gateway` (all
`contents: read`) and `publish`. Only `publish` writes. It refuses to publish unless
every expected tarball arrived, except the gateway tarballs: a missing one gives a
warning. It writes one `checksums.txt` over all of them, and attests every tarball's
build provenance.

# Contributing to Cortex

Greetings! We are grateful for your interest in joining the Rossoctl community and making a positive impact. Whether you're raising issues, enhancing documentation, fixing bugs, or developing new features, your contributions are essential to our success.

To get started, kindly read through this document and familiarize yourself with our code of conduct.

We can't wait to collaborate with you!

## Contributing Code

Please follow the [Contribution guide](https://github.com/rossoctl/rossoctl/blob/main/CONTRIBUTING.md#contributing-to-this-project) as found in the Rossoctl Repository for instructions on how to contribute to our repositories.

## Claiming an Issue

Comment `/claim` on an issue to have it automatically assigned to you. Issues labeled `blocked` or `in-progress` cannot be claimed this way. If you need to release an issue, comment `/unassign` or ask a maintainer.

## Prerequisites

- **Go 1.26.10+** (matches `go.work`)
- **Python 3.12+** (for `keycloak_sync.py` and the demo setup scripts)
- **Docker or Podman** (for building container images)
- **pre-commit** (for local hooks)

## Development Setup

```bash
# Clone the repository
git clone https://github.com/rossoctl/cortex.git
cd cortex

# Install pre-commit hooks
pre-commit install

# Build the proxy-init image (one-target Makefile in deploy/proxy-init/).
# For every image at once, use scripts/dev/local-build-and-test.sh —
# see "Testing against a local cluster" below.
cd deploy/proxy-init && make docker-build-init
```

Most day-to-day work needs no cluster: `make agentop` / `make cortex`
build to `./bin/`, and `make dev-install` builds both and runs
`./bin/agentop setup --from ./bin --yes --no-modify-path --restart` — the same
setup a release install hands off to. It installs them to `~/.local/bin` without
editing your shell profile; setup says so when that directory is not on your PATH.
Note that `--restart` restarts the shared local Cortex service every time, which
cuts every attached session.

## Testing against a local cluster

For changes that need a real Kubernetes environment (SPIRE identity, the
operator's sidecar injection, token exchange against Keycloak), the loop is a
Kind cluster plus the Rossoctl installer. You need both the
[`rossoctl`](https://github.com/rossoctl/rossoctl) and `cortex` repos cloned.

Only the build and verify steps below live in this repo. Creating the cluster and
installing the platform belong to `rossoctl` and are documented there. Link to
its guide rather than reproducing its command lines here — a copied install
command is what went stale last time.

**1. Install the platform.** Follow rossoctl's
[install guide](https://github.com/rossoctl/rossoctl/blob/main/docs/operate/install-kubernetes.md).
Its `scripts/kind/setup-rossoctl.sh` creates the Kind cluster itself (default
name `rossoctl`), or reuses an existing one with `--skip-cluster`. For JWT-SVID
auth rather than client secrets, that repo's
`deployments/envs/dev_values_federated-jwt.yaml` sets
`authBridge.clientAuthType: federated-jwt`.

**2. Build and load your local images.** `scripts/dev/local-build-and-test.sh` is
the supported path — it builds from both repos and loads everything into Kind. It
requires the cluster to exist already, which is why it comes second:

```bash
cd cortex
export KIND_EXPERIMENTAL_PROVIDER=podman   # Podman only
CLUSTER_NAME=rossoctl ROSSOCTL_DIR=../rossoctl ./scripts/dev/local-build-and-test.sh
```

Pass `CLUSTER_NAME` explicitly: this script defaults to `rossoctl-dev` while
rossoctl's installer defaults to `rossoctl`, and a mismatch loads your images
into a cluster nothing is running in.

It builds `spiffe-idp-setup` (from the rossoctl repo — easy to miss) plus
`authbridge`, `authbridge-envoy`, `authbridge-lite` and `proxy-init` from this
one, all tagged `:local`. Confirm with
`docker exec rossoctl-control-plane crictl images | grep local` (`podman exec`
under Podman). On Podman the script also loads via tar archives, because
`kind load docker-image` does not work with Podman's image store.

> **This step is inert on its own.** Step 1 installs released image tags, and
> loading images into the node store does not change what a running Deployment
> uses. Making the platform *run* your `:local` builds needs an image-override
> values file, and the overlay that used to do that was removed from `rossoctl` —
> so check that repo's current guide for the supported way before relying on this.

**3. Verify the platform came up.**

```bash
cd cortex
./scripts/dev/verify-spire-keycloak.sh
```

It checks the SPIRE server, the OIDC discovery provider, the JWKS `use` field,
Keycloak, the Keycloak admin secret, and the SPIFFE IdP setup job. Run it before
debugging anything workload-level — most "token exchange is broken" reports turn
out to be one of these six.

**4. Deploy a workload.** Use a demo rather than hand-written manifests; they
are kept current, and the manual path is not. Start from
[`demos/README.md`](demos/README.md).

## Installing an unreleased build

A fix merged to `main` is installable immediately, without waiting for a release:

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.sh \
  | sh -s -- --claude-code --ref=main
```

`--ref=X` means "install X" — both the installer script and the binaries — for any X
that has published binaries: `main` and any `vX.Y.Z` release, release candidates
included. Every push to `main` rebuilds a rolling pre-release, so `--ref=main` tracks
the tip.

`--ref=` also accepts a branch or a commit, but no binaries are published for those, so
you get that ref's *script* with the newest *release's* binaries. The installer warns
when that happens rather than leaving you to infer it.

You should not need `--ref` to get a stable release. The plain one-liner installs the
release GitHub marks **Latest**, which is never a prerelease. It reads that from where
`github.com/rossoctl/cortex/releases/latest` redirects, which is not bound by the API's
60-requests-per-hour-per-IP limit, and from the releases API when the redirect does not
answer.

Each binary reports its own build: `agentop --version` → `main-a1b2c3d`. Quote that,
not "main", in a bug report — the channel moves under you.

Three things to know:

- **A `--ref=main` run restarts the service whenever `main` has moved.** The installer
  always downloads, and setup compares what it staged with what is installed. A newer
  build replaces the binaries and restarts the service. That cuts any running Claude
  Code session, because `HTTPS_PROXY` is fixed in each session's environment at
  startup. The build you already have changes nothing: setup ends on
  `cortex main-a1b2c3d is installed and healthy.`
- **`checksums.txt` rolls with the assets.** The installer fetches assets and
  checksums in the same run, so verification is sound. Downloading them hours apart
  will mismatch.
- **The plain one-liner takes you back.** Running it without `--ref` installs the
  newest stable release over the channel build and restarts the service, so there is
  no stuck state to clean up.

The rolling release is tagged `main-latest`, not `main`. A GitHub release needs a git
tag, and a tag named `main` would collide with the branch — `git rev-parse main` would
then resolve to the tag rather than the branch, and every git command in the repo would
warn that the name is ambiguous. `--ref=main` is the spelling to use, but `--ref=main-latest`
does the same thing, since that is the title the Releases page shows. CI moves the tag to
the published commit on every merge, so the release's "Source code" archives match the
binaries beside them.

If you have `AUTHBRIDGE_VERSION`, `AUTHBRIDGE_REF`, or `AUTHBRIDGE_INSTALL_ONLY` exported
in a shell profile, unset them. They are no longer read, and the installer now refuses to
run rather than quietly ignoring them — so a stale export fails the **default** one-liner
even though you passed no flags. The error names the flag that replaced it and echoes your
value back, so the fix is copy-pasteable.

### Enabling the channel (one-time, maintainers)

Merge, **cut a release**, then set the repo variable `MAIN_CHANNEL_ENABLED=true`
(Settings → Secrets and variables → Actions → Variables). The next push to `main`
publishes the rolling release.

Two things to check deliberately before flipping it, because both become live at that
moment and are inert until then:

- **`release-binaries.yaml`'s publish job holds `contents: write` on every `main`
  push**, not just on tag pushes. The builds run in separate `contents: read` jobs and
  hand it finished tarballs, and every action is SHA-pinned, but that is still a wider
  blast radius than before.
- **The job moves the `main-latest` tag and prunes its assets** on each publish,
  deleting any asset that build did not produce, so a retired binary does not linger
  beside the current ones with no checksum to verify it. Those are the two destructive
  operations in the workflow. Both are guarded to the rolling tag, and `install_test.sh`
  asserts that each sits inside that guard and that no `${TAG}` comparison in the
  workflow names anything other than `CHANNEL_TAG`, so a `v*` release cannot become
  movable or prunable by a rename going unnoticed.

The middle step is load-bearing, though not for the reason it first appears. The default
one-liner is safe as soon as `main` carries the `newest_release()` v-tag filter: it
resolves a `v` tag, re-execs that released copy, and the copy's `resolve_version` then
returns the `v*` tag it was handed without calling `newest_release` at all. What needs a
*filtered release* is anyone running a **released** copy as the parent, fetched from a
tag's URL rather than from `main`. v0.7.0 printed exactly such a one-liner in its own
"has no 'service' command" message. The one this installer prints when an agentop has no
`setup` command fetches from `main` with `--ref`, so it is not one of them. An
unfiltered parent resolves the rolling release as its version and installs unreleased
binaries. Cutting a release first stops that window growing; it cannot fix copies already
published.

## Prebuilt release binaries

Prefer not to compile from source? Every `v*` release attaches prebuilt `agentop` (the
session-inspector TUI) and `cortex` binaries for linux and macOS (amd64 +
arm64) on the [Releases page](https://github.com/rossoctl/cortex/releases).

```sh
VER=v0.9.0                 # a released tag
OS=darwin ARCH=arm64       # one of: linux/darwin × amd64/arm64
base="https://github.com/rossoctl/cortex/releases/download/${VER}"

curl -fsSLO "${base}/agentop_${VER}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "${base}/checksums.txt"
sha256sum -c checksums.txt --ignore-missing    # macOS: shasum -a 256 -c ... --ignore-missing
tar xzf "agentop_${VER}_${OS}_${ARCH}.tar.gz"
sudo mv agentop /usr/local/bin/                # onto PATH
agentop --version
```

`cortex` ships the same way (`cortex_${VER}_${OS}_${ARCH}.tar.gz`).

- **Linux** binaries are fully static (`CGO_ENABLED=0`) — no libc dependency, run anywhere.
- **macOS** binaries are portable but unsigned; after extracting, clear the Gatekeeper
  quarantine once — `xattr -dr com.apple.quarantine ./agentop` (or `codesign --sign -
  ./agentop`) — **before** `sudo mv` moves the binary out of the directory, while
  `./agentop` still names it.

`cortex` defaults to the `full` plugin profile when built with `make cortex`;
pass `PROFILE=lite` or `local` for smaller sets. See
[architecture.md's build-tag section](docs/architecture.md#build-tag-plugin-selection)
for the underlying `go build` invocations.

`PROFILE` has to satisfy the config you are running. `~/.cortex/config.yaml` names the
plugins it wants, and a profile that omits one is a fatal error at startup rather than a
reduced feature set — `PROFILE=lite` against a config using `inference-parser` will not
start at all. What you find afterwards differs by platform: macOS restarts it
indefinitely (the plist supervises the proxy, which backs off between attempts), while
Linux gives up after five failures inside five minutes and leaves the unit `failed`, so
there you get a stopped service rather than a looping one. Either way the proxy logs the
missing plugin and the set it does have, and `agentop service status` shows the last log
lines; the default `full` avoids the question.

## Release candidates

A tag with `-rc`, `-alpha` or `-beta` in it publishes a **prerelease**: binaries and
images like any `v*` tag, but the plain one-liner never installs it. That one-liner
installs the release carrying GitHub's **Latest** badge, and GitHub never gives the badge
to a prerelease.

Cut one by pushing the tag and letting CI publish the release:

```sh
git tag -a v0.9.0-rc.1 -m v0.9.0-rc.1 <commit>
git push https://github.com/rossoctl/cortex.git v0.9.0-rc.1
```

Testers install it by name — the release page prints the same command — and the plain
one-liner takes them back to the newest stable release afterwards:

```sh
curl -fsSL https://www.rossoctl.dev/cortex/install.sh \
  | sh -s -- --ref=v0.9.0-rc.1
```

To promote a candidate, tag the same commit `v0.9.0`.

Things to know:

- **Spell the suffix `-rc.N`, `-alpha.N` or `-beta.N`.** Those are what
  `release-binaries.yaml` flags as a prerelease. Any other spelling (`v0.9.0-pre1`)
  publishes an ordinary release, which takes the Latest badge and goes to everyone.
- **Do not create the release from the Releases page first.** If you do, the tag build
  flags it by its tag when it uploads the binaries, but until then an unticked "Set as
  a pre-release" box leaves a candidate with no assets as Latest, and the plain
  one-liner fails for everyone in that window.
- **Installers from v0.8.1 and earlier still pick candidates.** Their copy of the
  script takes the newest release of any kind, so running one directly —
  `curl …/v0.8.1/scripts/install.sh | sh`, with no `--ref` — installs the candidate.
  The documented one-liner runs `main`'s copy and is not affected.
- **Container images are not gated.** A candidate's tag pushes its images under that
  tag and also moves `latest`, as every `v*` tag and every merge to `main` already does.

## Install stats

[rossoctl.github.io/cortex/install-stats](https://rossoctl.github.io/cortex/install-stats/)
estimates how many people install each release, from GitHub's release-asset download
counts. Cortex collects nothing from a user's machine, so downloads are the only
signal there is — and the page is explicit about what that does and does not support.

Two things worth knowing before reading it:

- **`release-smoke-linux.yaml` is in those numbers.** It installs the way a user
  does, `curl | sh` from GitHub Releases, which is the point of it — so it cannot
  be told apart from a real Linux user by behaviour. The page subtracts it by
  *count* instead: **one** install per job lands in the figures the page reads,
  with one job per architecture. Each job installs three times — a fresh install
  of the newest stable tag, an upgrade to the tag under test, and a no-op re-run —
  but on a push to `main` the latter two land on `main-latest`, which the collector
  excludes because `--clobber` resets its download counts on every push. Change
  that structure, and the estimate drifts until `scripts/collect-install-stats.sh`
  is updated to match.
- **The figures include us.** At the current scale, maintainers running the
  installer are a large share of the total. Do not read a daily number as new users.

`.github/workflows/install-stats.yaml` snapshots the counts daily and commits them
to `gh-pages`; a lifetime total is all GitHub reports, so a daily figure only
exists as the difference between two snapshots, and the series cannot be
backfilled.

## Issues

Prioritization for pull requests is given to those that address and resolve existing GitHub issues. Utilize the available issue labels to identify meaningful and relevant issues to work on.

If you believe that there is a need for a fix and no existing issue covers it, feel free to create a new one.

As a new contributor, we encourage you to start with issues labeled as **good first issues**.

## Committing

All commits must be signed off (`git commit -s`) per the [Developer Certificate of Origin](https://developercertificate.org/).

### PR Title Convention

PR titles must follow the format:

```
<Prefix>: <Subject starting with uppercase>
```

The CI title check matches prefixes **case-insensitively** (the shared
`pr-verifier` workflow sets `prefix_case_sensitive: false`), and
`scripts/release_notes.sh` lowercases titles before matching, so `feat:` and
`Feat:` both pass and both file the same way. The capitalized spelling is the
repo's convention — the allowed set is written capitalized: `Build`, `Chore`,
`CI`, `Docs`, `Feat`, `Fix`, `Perf`, `Refactor`, `Revert`, `Style`, `Test`,
`Feature`, `Bug fix`, `Proposal`, `Breaking change`, `Other`, `Other/Misc`.
Use it; nothing enforces the capitals.

The prefix also decides where the PR appears in the release notes, which
`scripts/release_notes.sh` builds from the PRs merged since the previous
release: `Feat`/`Feature` under New features, `Fix`/`Bug fix`/`Perf` under Bug
fixes, `Breaking change` (or a `!` before the colon) under Breaking changes,
and everything else under a collapsed Other changes.

## Pull Requests

When submitting a pull request, clear communication is appreciated:

- Detailed description of the problem you are trying to solve, along with links to related GitHub issues
- Explanation of your solution, including links to any design documentation and discussions
- Information on how you tested and validated your solution
- Updates to relevant documentation and examples, if applicable

Smaller pull requests are typically easier to review and merge. If your pull request is big, collaborate with the maintainers to find the best way to divide it.

## Code Style

### Go Code
- Before pushing, run `go vet ./...` and `gofmt -l .` from each module you touched.
  Always give `gofmt` a path — a bare `gofmt -l` reads stdin, so it scans nothing
  and exits 0. `gofmt -l .` also lists any pre-existing offenders, so check the
  names it prints are yours.
- `go vet` is gated: all five `Go CI (…)` jobs run it, and a finding fails the job.
  `gofmt` is not — CI's lint step runs `go fmt`, which rewrites files and exits 0,
  so unformatted code still goes green. There are no Go hooks in pre-commit either.
- Run per-module with `GOWORK=off` — how the root Makefile builds, and how every
  CI job but core runs.
- If your change deletes a package or its last import of a dependency, also run
  `go mod tidy -diff` in every module — CI gates on it, and `build`/`vet`/`test`
  all pass while it fails.
- Apache 2.0 license header in all Go files

### Python Code (keycloak_sync.py, sparc-service, demo scripts)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies declared in `requirements.txt` — exact pins for the
  langchain/pydantic stack, bounded ranges elsewhere

## Licensing

Cortex is [Apache 2.0 licensed](LICENSE) and we accept contributions via
GitHub pull requests.

## Certificate of Origin

By contributing to this project you agree to the Developer Certificate of
Origin (DCO). This document was created by the Linux Kernel community and is a
simple statement that you, as a contributor, have the legal right to make the
contribution. See the [DCO](https://developercertificate.org/) for details.

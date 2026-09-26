# Contributing to Rossoctl Extensions

Greetings! We are grateful for your interest in joining the Rossoctl community and making a positive impact. Whether you're raising issues, enhancing documentation, fixing bugs, or developing new features, your contributions are essential to our success.

To get started, kindly read through this document and familiarize yourself with our code of conduct.

We can't wait to collaborate with you!

## Contributing Code

Please follow the [Contribution guide](https://github.com/rossoctl/rossoctl/blob/main/CONTRIBUTING.md#contributing-to-this-project) as found in the Rossoctl Repository for instructions on how to contribute to our repositories.

## Claiming an Issue

Comment `/claim` on an issue to have it automatically assigned to you. Issues labeled `blocked` or `in-progress` cannot be claimed this way. If you need to release an issue, comment `/unassign` or ask a maintainer.

## Prerequisites

- **Go 1.26.5+** (matches `go.work`)
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

Most day-to-day work needs no cluster: `make abctl` / `make authbridge-proxy`
build to `./bin/`, and `make dev-install` puts them on your PATH. Note that
`dev-install` restarts the shared local Cortex service, which cuts every
attached session.

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
curl -fsSL https://raw.githubusercontent.com/rossoctl/cortex/main/install.sh \
  | sh -s -- --claude-code --ref=main
```

`--ref=X` means "install X" — both the installer script and the binaries — for any X
that has published binaries: `main` and any `vX.Y.Z` release. Every push to `main`
rebuilds a rolling pre-release, so `--ref=main` tracks the tip.

`--ref=` also accepts a branch or a commit, but no binaries are published for those, so
you get that ref's *script* with the newest *release's* binaries. The installer warns
when that happens rather than leaving you to infer it.

You should not need `--ref` to get a release. The plain one-liner resolves the newest
release itself, from the releases API and — when that is unavailable — from
`releases.atom`, which is not bound by the API's 60-requests-per-hour-per-IP limit.

Each binary reports its own build: `abctl --version` → `main-a1b2c3d`. Quote that,
not "main", in a bug report — the channel moves under you.

Three things to know:

- **Every `--ref=main` run replaces and restarts the service.** The installed build
  never matches the requested `main`, so the installer always re-downloads and
  `service install` always reinstalls. That cuts any running Claude Code session,
  because `HTTPS_PROXY` is fixed in each session's environment at startup.
- **`checksums.txt` rolls with the assets.** The installer fetches assets and
  checksums in the same run, so verification is sound. Downloading them hours apart
  will mismatch.
- **The plain one-liner takes you back.** Running it without `--ref` reinstalls the
  newest release and reinstalls the service, so there is no stuck state to clean up.

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

- **`release-binaries.yaml` holds `contents: write` on every `main` push**, not just on
  tag pushes. It builds only first-party code with SHA-pinned actions, but that is a
  wider blast radius than before. If you want it narrower, split build (`contents: read`)
  from publish (`contents: write`) and pass `dist/` between them as an artifact.
- **The job moves the `main-latest` tag** on each publish. That is the one destructive
  operation in the workflow. It is guarded to the rolling tag, and `install_test.sh`
  asserts no `${TAG}` comparison in the workflow names anything other than
  `CHANNEL_TAG`, so a `v*` release cannot become movable by a rename going unnoticed.

The middle step is load-bearing, though not for the reason it first appears. The default
one-liner is safe as soon as `main` carries the `newest_release()` v-tag filter: it
resolves a `v` tag, re-execs that released copy, and the copy then matches
`case "${SCRIPT_REF}" in v*)` and never calls `newest_release` at all. What needs a
*filtered release* is anyone running a **released** copy as the parent — including the
pinned one-liner this installer prints in its own "abctl is too old" message. An
unfiltered parent resolves the rolling release as its version and installs unreleased
binaries. Cutting a release first stops that window growing; it cannot fix copies already
published.

## Issues

Prioritization for pull requests is given to those that address and resolve existing GitHub issues. Utilize the available issue labels to identify meaningful and relevant issues to work on.

If you believe that there is a need for a fix and no existing issue covers it, feel free to create a new one.

As a new contributor, we encourage you to start with issues labeled as **good first issues**.

## Committing

All commits must be signed off (`git commit -s`) per the [Developer Certificate of Origin](https://developercertificate.org/).

### PR Title Convention

PRs must follow **conventional commits** format:

```
<type>: <Subject starting with uppercase>
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`

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
- `go vet` is gated: all four `Go CI (…)` jobs run it, and a finding fails the job.
  `gofmt` is not — CI's lint step runs `go fmt`, which rewrites files and exits 0,
  so unformatted code still goes green. There are no Go hooks in pre-commit either.
- Run per-module with `GOWORK=off` — how the root Makefile builds, and how every
  CI job but authlib runs.
- If your change deletes a package or its last import of a dependency, also run
  `go mod tidy -diff` in every module — CI gates on it, and `build`/`vet`/`test`
  all pass while it fails.
- Apache 2.0 license header in all Go files

### Python Code (keycloak_sync.py, sparc-service, demo scripts)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies declared in `requirements.txt` — exact pins for the
  langchain/pydantic stack, bounded ranges elsewhere

## Licensing

Rossoctl Extensions is [Apache 2.0 licensed](LICENSE) and we accept contributions via
GitHub pull requests.

## Certificate of Origin

By contributing to this project you agree to the Developer Certificate of
Origin (DCO). This document was created by the Linux Kernel community and is a
simple statement that you, as a contributor, have the legal right to make the
contribution. See the [DCO](https://developercertificate.org/) for details.

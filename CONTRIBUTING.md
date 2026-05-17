# Contributing to Kagenti Extensions

Greetings! We are grateful for your interest in joining the Kagenti community and making a positive impact. Whether you're raising issues, enhancing documentation, fixing bugs, or developing new features, your contributions are essential to our success.

To get started, kindly read through this document and familiarize yourself with our code of conduct.

We can't wait to collaborate with you!

## Contributing Code

Please follow the [Contribution guide](https://github.com/kagenti/kagenti/blob/main/CONTRIBUTING.md#contributing-to-this-project) as found in the Kagenti Repository for instructions on how to contribute to our repositories.

## Claiming an Issue

Comment `/claim` on an issue to have it automatically assigned to you. Issues labeled `blocked` or `in-progress` cannot be claimed this way. If you need to release an issue, comment `/unassign` or ask a maintainer.

## Prerequisites

- **Go 1.24+** (for authbridge and authlib)
- **Python 3.12+** (for client-registration and Keycloak sync)
- **Docker** (for building container images)
- **pre-commit** (for local hooks)

## Development Setup

```bash
# Clone the repository
git clone https://github.com/kagenti/kagenti-extensions.git
cd kagenti-extensions

# Install pre-commit hooks
pre-commit install

# Build the proxy-init image (one-target Makefile in proxy-init/).
# For the four combined-sidecar images plus this one, use the
# repo-root local-build-and-test.sh.
cd authbridge/proxy-init && make docker-build-init
```

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

### Go Code (AuthProxy)
- Use `go fmt` (enforced by pre-commit and CI)
- Use `go vet` (enforced by pre-commit and CI)
- Apache 2.0 license header in all Go files

### Python Code (client-registration)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies version-pinned in `requirements.txt`

## Licensing

Kagenti Extensions is [Apache 2.0 licensed](LICENSE) and we accept contributions via
GitHub pull requests.

## Certificate of Origin

By contributing to this project you agree to the Developer Certificate of
Origin (DCO). This document was created by the Linux Kernel community and is a
simple statement that you, as a contributor, have the legal right to make the
contribution. See the [DCO](https://developercertificate.org/) for details.

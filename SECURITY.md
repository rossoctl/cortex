# Security Policy

## Reporting a Vulnerability

We take security vulnerabilities seriously. If you discover a security issue,
please report it responsibly.

### How to Report

1. **Do NOT create public GitHub issues** for security vulnerabilities
2. **Email**: Report vulnerabilities privately via GitHub Security Advisories
   - Go to the [Security tab](../../security/advisories/new) and create a new advisory
3. **Include**: A clear description of the vulnerability, steps to reproduce,
   and potential impact

### What to Expect

- We will acknowledge receipt within 48 hours
- We aim to provide an initial assessment within 7 days
- We will keep you informed of our progress
- We will credit you in the security advisory (if desired)

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| main    | :white_check_mark: |

## Security Measures

This project implements several security controls:

- **CI Security Scans**: Dependency Review, Trivy (filesystem + IaC), Bandit (Python), Hadolint (Dockerfiles), Shellcheck, YAML Lint
- **OpenSSF Scorecard**: Automated security health metrics
- **Dependency Updates**: Dependabot for GitHub Actions dependencies
- **Pre-commit Hooks**: Trailing whitespace, go fmt/vet, Helm lint
- **PR Verification**: Conventional commit enforcement via pr-verifier

## Security-Related Configuration

For deployment security configuration, see:
- [AuthBridge README](authbridge/README.md) - Authentication bridge architecture
- [operator](https://github.com/rossoctl/operator) - Admission webhook configuration (migrated from this repo)

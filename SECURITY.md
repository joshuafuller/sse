# Security Policy

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| 3.x     | :white_check_mark: |
| < 3.0   | :x:                |

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Report security issues privately via email: **joshuafuller@gmail.com**

Include:
- A description of the vulnerability
- Steps to reproduce
- Potential impact

You will receive acknowledgement within 48 hours and a status update within 7 days.

## Automated Security Checks

This repository runs the following security checks on every push and pull request:

| Tool | Purpose |
|------|---------|
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | Go vulnerability database — detects known CVEs in dependencies |
| [Trivy](https://github.com/aquasecurity/trivy) | CVE scanning + CycloneDX SBOM generation |
| [OpenSSF Scorecard](https://scorecard.dev) | Supply-chain security posture scoring |
| [gosec](https://github.com/securego/gosec) | Go static analysis for security issues (via golangci-lint) |
| [Semgrep](https://semgrep.dev) | Semantic code analysis for security patterns |

## SBOM

A CycloneDX Software Bill of Materials (SBOM) is generated on every push to `master` and
attached as a build artifact (`sbom-<sha>`) in the
[Security Scan workflow](https://github.com/joshuafuller/sse/actions/workflows/trivy.yml).

# Security Policy

## Reporting a vulnerability

Please report security issues **privately** — do not open a public issue or pull request. Use GitHub's private vulnerability reporting on this repository (**Security → Report a vulnerability**). Include steps to reproduce and the affected version or commit; you'll receive an acknowledgement and a fix timeline. Please allow a reasonable disclosure window before public discussion.

## Security & privacy model

`aimesh` is built on a governed substrate ([`meshcore`](meshcore/)): verified model identity with no silent fallback, filesystem containment (models operate on isolated read-only copies; only the host writes), and a single governed config-write path. For what data leaves your machine, how to run fully local/offline, the workspace-integrity guarantees, and the trusted-root model for the ACP and MCP surfaces, see [docs/security.md](docs/security.md).

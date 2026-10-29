# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in `protolsp`, please report it
privately to **security@trendvidia.com**. PGP key available on request.
**Do not open a public GitHub issue or pull request** for
security-sensitive reports.

Please include:

- A description of the issue and the impact you observed.
- Steps to reproduce, ideally a minimal workspace (`.pxf`, `.proto`,
  `.protolsp.pxf`) or an LSP message trace.
- The commit or release tag the report applies to.
- Whether you intend to publish a writeup, and on what timeline.

## What to Expect

- Acknowledgement within **3 business days**.
- An initial assessment (severity, scope, affected versions) within **7
  business days**.
- Status updates at least every 14 days until the report is closed.
- Credit in the release notes and any published advisory, unless you ask
  to remain anonymous.

## Scope

In scope:

- Crashes, hangs, or unbounded memory triggered by the contents of an
  untrusted workspace: `.pxf` documents, `.proto` sources, chain layers,
  or workspace config.
- Reads or writes outside the workspace the client opened.
- `pxf.Secret` values reaching hover, diagnostics, logs, or RPC
  responses unmasked.
- `protolsp-ws` accepting connections its Origin check should refuse.

Out of scope:

- Running `protolsp-ws` as a shared, multi-tenant service. It has no
  authentication or per-connection sandbox yet; see the hosted-platform
  items in [`docs/roadmap.md`](docs/roadmap.md).
- Vulnerabilities in third-party dependencies — please report those
  upstream. We will of course bump dependency versions in response.
- Performance regressions and slow but bounded analysis of large
  workspaces (these are tracked as normal issues).

## Supported Versions

Until `v1.0.0`, only the latest minor release receives security fixes.
After `v1.0.0`, the policy will be revisited and documented here.

## Coordinated Disclosure

We follow a 90-day coordinated disclosure window by default. If a fix
ships sooner, the advisory will be published immediately. Reports
involving an actively exploited vulnerability will be triaged
out-of-band — flag this in your initial email.

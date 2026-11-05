# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x (main) | Yes |

## Reporting a vulnerability

**Do not open a public GitHub issue for a security vulnerability.**

Report it privately via a GitHub Security Advisory on this repository
(Security → Advisories → New draft security advisory), or email the
maintainer via the address on their GitHub profile. Include:

- A description of the vulnerability and its impact.
- Steps to reproduce (a minimal config or request is ideal).
- Any suggested fix, if you have one.

We will acknowledge the report promptly, investigate, and coordinate a fix
and disclosure timeline with you.

## Security posture

RelayDeck is designed with these properties; please verify them rather than
assume them:

- **At-least-once delivery, never exactly-once.** A crash between the
  destination receiving a request and the attempt being recorded can cause a
  duplicate send. Endpoints must be idempotent.
- **SSRF protection** is a core feature (see README): private/loopback/
  link-local/metadata destinations are blocked, resolved IPs are re-checked,
  and redirects to private IPs are blocked. `allow_private_destinations`
  is a local-development opt-in only.
- **Secrets** (API key, HMAC secrets) are compared in constant time and are
  never logged or returned by the API. Stored headers are redacted.
- **No credential storage:** RelayDeck stores no user passwords. Destination
  custom headers are operator-configured; treat your config file as secret
  material and never commit it with real secrets (see `.env.example`).

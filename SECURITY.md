# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x (main) | Yes |

Only the latest release on `main` receives security fixes. If you run an
older release, upgrade first and check whether the problem still exists.

## Reporting a vulnerability

**Do not open a public GitHub issue for a security vulnerability.**

Report it privately through one of these channels:

- **GitHub Security Advisory** on this repository: Security tab, then
  Advisories, then "New draft security advisory". This is the preferred
  channel.
- **Contact form at https://mrpythonist.com.** Use the contact form on the
  maintainer's site and mark the message as a RelayDeck security report.

Include:

- A description of the vulnerability and its impact.
- Steps to reproduce (a minimal config or request is ideal).
- Any suggested fix, if you have one.

We will acknowledge the report promptly, investigate, and coordinate a fix
and disclosure timeline with you. Please do not disclose the issue publicly
until a fix is released.

## Security posture

RelayDeck is designed with these properties; please verify them rather than
assume them:

- **At-least-once delivery, never exactly-once.** A crash between the
  destination receiving a request and the attempt being recorded can cause a
  duplicate send. Endpoints must be idempotent.
- **SSRF protection** is a core feature (see the README security section):
  private, loopback, link-local, and metadata destinations are blocked,
  resolved IPs are re-checked, and redirects to private IPs are blocked.
  `allow_private_destinations` is a local-development opt-in only.
- **Secrets** (API key, HMAC secrets) are compared in constant time and are
  never logged or returned by the API. Stored headers are redacted.
- **No credential storage:** RelayDeck stores no user passwords. Destination
  custom headers are operator-configured; treat your config file as secret
  material and never commit it with real secrets (see `.env.example`).
- **Outbound requests** go only to operator-configured destination URLs, and
  every redirect target is re-validated against the public-IP rule.

## Out of scope

- Reports that RelayDeck delivers a webhook more than once. That is the
  documented at-least-once behaviour, not a vulnerability.
- Denial of service by a configured destination, or by a source that is
  allowed to send unlimited webhooks. Rate limiting per destination is on
  the roadmap (v0.4.0).
- Issues in third-party providers (Stripe, GitHub, Shopify) or in the
  destination applications that receive deliveries.

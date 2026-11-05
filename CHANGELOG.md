# Changelog

All notable changes to RelayDeck are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased] — v0.1.0

### Added
- Webhook ingestion (`POST /api/v1/webhooks/{source}`) with durable SQLite (WAL)
  persistence; events acknowledged only after the transaction commits.
- Idempotency via `Idempotency-Key` header (per source).
- Per-source HMAC-SHA256 signature verification (constant-time compare).
- Destinations CRUD API with source filtering and payload-substring filtering.
- Background delivery workers with lease-based claiming, stale-lease recovery,
  exponential backoff with jitter, per-destination retry settings.
- Manual retry (`POST /api/v1/deliveries/{id}/retry`) and event replay
  (`POST /api/v1/events/{id}/replay`).
- Embedded dashboard (Overview, Events, Deliveries, Destinations, Sources,
  Failed, Settings) with dark/light themes; all data from the live API.
- CLI: `serve`, `version`, `config validate`, `events list|inspect|replay`,
  `destinations list`, `health`.
- Health (`/health`), readiness (`/ready`) and Prometheus metrics (`/metrics`).
- SSRF protection: private/loopback/link-local/metadata blocking, resolved-IP
  re-checks (DNS-rebinding defence), redirect-to-private blocking, explicit
  trusted-internal opt-in.
- API-key authentication for the management API; sensitive-header redaction
  and secret masking in API responses.
- Structured JSON logging, graceful shutdown, Docker + Docker Compose,
  GitHub Actions CI and release workflows.

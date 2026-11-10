# Changelog

All notable changes to RelayDeck are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- Multi-tenancy: users, workspaces, memberships, workspace API keys
  (migration 002). Every tenant row carries `workspace_id`; legacy
  single-tenant data keeps workspace "" and still works.
- Auth: email + password signup/login/logout with bcrypt, session
  cookies, workspace auth for the management API and dashboard.
- Per-workspace ingestion `POST /ingest/{workspace_public_id}/{source}`.
  Legacy ingestion kept for self-hosted mode, HTTP 410 in hosted mode.
- Plans in `internal/plans/plans.go`: Free $0 / 1,000 events / 3
  destinations / 3 days, Starter $19 / 50,000 / 30 days, Growth $49 /
  500,000 / 90 days. Monthly usage counting, usage endpoint, HTTP 429
  when over the limit.
- Retention pruning: hourly background job with per-plan retention,
  `relaydeck prune` CLI, and `POST /api/v1/workspaces/{id}/prune`.
- Stripe billing code only: Checkout session creation and webhook
  handler for `checkout.session.completed` and
  `customer.subscription.deleted`. No live keys, price IDs from env.
- Pricing page at `/pricing`, login/signup page, `docs/HOSTED.md`.
- Tenancy isolation tests, auth tests, limit tests, pruning tests.

### Changed

- Sources, destinations and events now use workspace-scoped keys:
  `(workspace_id, name)`, `(workspace_id, name)`,
  `(workspace_id, source, idempotency_key)`. Pre-Phase-1 databases are
  migrated in place on startup.


### Added

- Project documentation set: rewritten README, API and CLI references
  (`docs/api.md`, `docs/cli.md`), development guide (`docs/development.md`),
  support guide (`SUPPORT.md`), and YAML issue templates.
- CI and release workflows (`.github/workflows/ci.yml`,
  `.github/workflows/release.yml`) prepared for upload through the GitHub web
  UI. They are not yet live in the repository.

### Fixed

- Corrected the v0.1.0 notes: the CI and release workflows were listed as
  shipped, but they were never added to the repository. The Docker build is
  also still unverified. Retention pruning was not wired in v0.1.0; it is
  wired in v0.2.0 (see above).

## [0.1.0] - 2026-11-05

The first release. A working webhook gateway: durable ingestion, at-least-once
delivery with retries, HMAC verification, SSRF protection, an embedded
dashboard, and a CLI. No hosted service, no mandatory external dependencies.

### Added

- Webhook ingestion (`POST /api/v1/webhooks/{source}`) with durable SQLite
  (WAL) persistence. Events are acknowledged only after the transaction
  commits.
- Idempotency via the `Idempotency-Key` header (per source).
- Per-source HMAC-SHA256 signature verification (constant-time compare).
- Destinations CRUD API with source filtering and payload-substring filtering.
- Background delivery workers with lease-based claiming, stale-lease recovery,
  exponential backoff with jitter, and per-destination retry settings.
- Manual retry (`POST /api/v1/deliveries/{id}/retry`) and event replay
  (`POST /api/v1/events/{id}/replay`).
- Embedded dashboard (Overview, Events, Deliveries, Destinations, Sources,
  Failed, Settings) with dark/light themes. All data comes from the live API.
- CLI: `serve`, `version`, `config validate`, `events list | inspect | replay`,
  `destinations list`, `health`.
- Health (`/health`), readiness (`/ready`), and Prometheus metrics (`/metrics`).
- SSRF protection: private, loopback, link-local, and metadata blocking,
  resolved-IP re-checks (DNS-rebinding defence), redirect-to-private blocking,
  and an explicit trusted-internal opt-in.
- API-key authentication for the management API, sensitive-header redaction,
  and secret masking in API responses.
- Structured JSON logging, graceful shutdown, a Dockerfile, Docker Compose
  setup, and a systemd unit.

### Known limitations in 0.1.0

- The Docker build has not been run and verified.
- Retention pruning is not wired: `retention_days` is validated and stored,
  but no scheduled job deletes old events yet.
- GitHub Actions workflows (CI and release) were not part of this release.
  They are prepared for the next release.
- Delivery is at-least-once by design. RelayDeck never claims exactly-once
  delivery or zero loss.

# RelayDeck Roadmap

## v0.1.0 - MVP (released 2026-11-05)
- Durable ingestion, idempotency, HMAC verification
- SQLite (WAL) storage, lease-based workers, exponential backoff + jitter
- Destinations + routing, dashboard, CLI, metrics, SSRF protection
- Dockerfile, Docker Compose, systemd unit, docs
- Known gaps: Docker build unverified, retention pruning not wired,
  no CI or release workflows yet (prepared for the next release)

## v0.2.0 - Hosted SaaS, Phase 1 (done in code, 2026-11-08)
- Multi-tenancy: users, workspaces, memberships, workspace API keys
- Auth: email + password (bcrypt), session cookies, workspace auth for
  the management API and dashboard
- Per-workspace ingestion (`POST /ingest/{workspace}/{source}`), legacy
  ingestion kept for self-hosted mode only
- Plans and limits in code: Free $0 / 1,000 events / 3 destinations /
  3 days, Starter $19 / 50,000 / 30 days, Growth $49 / 500,000 / 90 days
- Usage counting per workspace per month, usage endpoint, HTTP 429
  when over the monthly limit, destination cap on the Free plan
- Retention pruning job wired (per-plan retention) plus `relaydeck prune`
- Stripe billing code: Checkout session creation and webhook handler
  (checkout.session.completed, customer.subscription.deleted), no live
  keys, price IDs from env vars only
- Pricing page, hosted docs (`docs/HOSTED.md`), migration 002
- Not verified live in Phase 1: Stripe checkout, VPS deploy, TLS,
  Docker build, load beyond a smoke test

## v0.2.1 - Delivery hardening (done, 2026-11-11)
- Outbound HMAC signing of deliveries (per-destination signing secret,
  `X-RelayDeck-Signature: sha256=<hex>` over the raw body)
- JSON-path event filtering (`filter_jsonpath`, for example
  `type=payment_intent.succeeded`), in addition to substring matching;
  both filters must pass when both are set
- Retention pruning job (`retention_days` enforced automatically): already
  wired in Phase 1 (hourly background job with per-plan retention plus
  `relaydeck prune`), verified, not rebuilt here
- Delivery payload transformation (header injection templates rendered
  from the event payload, `{{path}}` placeholders)
- `relaydeck destinations` create/update/delete via the CLI (the API
  endpoints already existed; the CLI now calls them)

## v0.3.0 - Operability
- Optional PostgreSQL storage backend (SQLite remains the default; never mandatory)
- Webhook endpoint testing from the dashboard ("send test event")
- Alerting webhooks for dead deliveries
- Structured audit log of destination changes

## v0.4.0 - Scale-out (only if demand justifies it)
- Multi-instance worker coordination via the same lease mechanism
- Optional Redis-backed queue (opt-in, never required)
- Rate limiting per destination

## Explicitly out of scope
- Exactly-once delivery claims (see README: at-least-once by design)
- A complex rules engine in the MVP

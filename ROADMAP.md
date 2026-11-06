# RelayDeck Roadmap

## v0.1.0 - MVP (released 2026-11-05)
- Durable ingestion, idempotency, HMAC verification
- SQLite (WAL) storage, lease-based workers, exponential backoff + jitter
- Destinations + routing, dashboard, CLI, metrics, SSRF protection
- Dockerfile, Docker Compose, systemd unit, docs
- Known gaps: Docker build unverified, retention pruning not wired,
  no CI or release workflows yet (prepared for the next release)

## v0.2.0 - Delivery hardening
- Outbound HMAC signing of deliveries (per-destination signing secret)
- JSON-path event filtering (in addition to substring matching)
- Retention pruning job (`retention_days` enforced automatically)
- Delivery payload transformation (header injection templates)
- `relaydeck destinations` create/update/delete via CLI

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
- A hosted cloud service
- A complex rules engine in the MVP

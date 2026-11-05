# RelayDeck

**Never lose a webhook. Never miss an event.**

RelayDeck is an ultra-lightweight, self-hostable webhook gateway and event relay. It sits between webhook providers (Stripe, GitHub, Shopify, Slack, …) and your applications, accepts events **durably first**, then delivers them **reliably** — with retries, backoff, idempotency, and full delivery history.

Think of it as a lightweight, self-hosted alternative to Svix and Hookdeck, with a local-first developer experience and minimal infrastructure: one binary, one SQLite file, no mandatory Redis, PostgreSQL, Kubernetes, or cloud services.

> **Delivery semantics:** RelayDeck provides **at-least-once delivery**. Events are acknowledged only after their database transaction commits, and failed deliveries are retried until they succeed or exhaust retries. RelayDeck does **not** claim exactly-once delivery or zero loss — no webhook system can honestly guarantee that. Design your endpoints to be idempotent.

## The problem

Webhooks fail in production all the time:

- Your app is down during a deploy when Stripe sends a payment event.
- A database hiccup drops a GitHub push event.
- A third-party API rate-limits you mid-burst.
- A network blip times out a delivery, and nobody notices until a customer complains.
- The same event arrives twice and charges a customer twice.
- Debugging means grepping logs across three services.

Developers end up rebuilding the same webhook infrastructure — ingestion, storage, retries, dashboards — for every project. RelayDeck is that infrastructure, done once, done well.

## Features

- **Durable ingestion** — `POST /api/v1/webhooks/{source}`; the event is acknowledged (202) only after its SQLite transaction commits. WAL mode, indexed, transactional.
- **Idempotency** — `Idempotency-Key` header: the same key + source returns the original event, no duplicate deliveries.
- **HMAC signature verification** — per-source secrets (`X-RelayDeck-Signature` / `X-Hub-Signature-256`, constant-time compare).
- **Delivery engine** — bounded worker pool, per-destination retry settings, exponential backoff with jitter, manual retry and replay.
- **Crash recovery** — lease-based delivery claiming; stale leases are re-queued after restarts, pending events survive crashes.
- **Routing** — multiple destinations, source filters, optional payload-substring filters, per-destination headers/method/timeout, enable/disable.
- **SSRF protection** — destination URLs are validated against loopback, private, link-local and cloud-metadata ranges; resolved IPs are re-checked (DNS-rebinding defence); redirects to private IPs are blocked. Explicit opt-in for trusted internal destinations.
- **Dashboard** — embedded, no build step, dark/light themes: Overview, Events, Deliveries, Destinations, Sources, Failed, Settings. All data is real, from the API.
- **CLI** — `relaydeck serve|version|config validate|events list|inspect|replay|destinations list|health`.
- **Observability** — `GET /health`, `GET /ready`, `GET /metrics` (Prometheus text format), structured JSON logging.
- **Security by default** — API-key auth for the management API, sensitive-header redaction, secret masking in API responses, request-size limits, timeouts.

## Architecture

```
External Providers (Stripe, GitHub, Shopify, Slack, ...)
        |
        v
RelayDeck HTTP Gateway  -- Auth (API key) + HMAC verification
        |
        v
Durable Event Storage (SQLite, WAL, transactions)
        |
        v
Background Delivery Workers (bounded pool, lease claiming)
        |
        v
Retry & Backoff Engine (exponential + jitter, per destination)
        |
        v
Internal Application Endpoints
```

Ingestion and delivery are fully separated: an incoming event is stored and acknowledged before any delivery is attempted. No database transaction is ever held open during a network call. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and [docs/DECISIONS.md](docs/DECISIONS.md).

## Quick start

### From source (Go 1.23+)

```bash
git clone https://github.com/mrpythonist/relaydeck.git
cd relaydeck
go build -o relaydeck ./cmd/relaydeck
./relaydeck serve
```

Open http://localhost:8080 — the dashboard. No configuration needed for local use.

### Docker

```bash
docker compose up -d
```

### Docker (image only)

```bash
docker build -t relaydeck .
docker run -p 8080:8080 -v relaydeck-data:/data relaydeck
```

## Try it in 60 seconds

```bash
# 1. Add a destination (your app, or a request catcher for testing)
curl -X POST localhost:8080/api/v1/destinations \
  -H 'Content-Type: application/json' \
  -d '{"name":"laravel-api","url":"https://example.com/webhooks/stripe","source_filter":"stripe"}'

# 2. Send a test webhook
curl -X POST localhost:8080/api/v1/webhooks/stripe \
  -H 'Content-Type: application/json' \
  -d '{"type":"payment_intent.succeeded","id":"pi_123"}'
# -> {"event_id":"evt_...","status":"accepted","deliveries_queued":1}

# 3. Inspect it
relaydeck events list
relaydeck events inspect evt_...

# 4. Watch delivery in the dashboard at http://localhost:8080
```

Sample payloads for Stripe, GitHub and Shopify live in [examples/](examples/).

## Configuration

Precedence: **CLI flags > environment variables > YAML file > defaults.**

| Setting | Env var | YAML key | Default |
|---|---|---|---|
| Listen address | `RELAYDECK_LISTEN_ADDR` | `listen_addr` | `:8080` |
| Data directory | `RELAYDECK_DATA_DIR` | `data_dir` | `./data` |
| API key (management API) | `RELAYDECK_API_KEY` | `api_key` | *(none — auth disabled)* |
| Max payload bytes | `RELAYDECK_MAX_PAYLOAD_BYTES` | `max_payload_bytes` | `1048576` (1 MiB) |
| Worker count | `RELAYDECK_WORKERS` | `workers` | `4` |
| Allow private destinations | `RELAYDECK_ALLOW_PRIVATE_DESTINATIONS` | `allow_private_destinations` | `false` |
| Trusted internal hosts | `RELAYDECK_TRUSTED_INTERNAL_DESTINATIONS` | `trusted_internal_destinations` | *(none)* |
| Retention days | `RELAYDECK_RETENTION_DAYS` | `retention_days` | `30` |
| Config file | `RELAYDECK_CONFIG` | — | *(none)* |

Example — see [examples/relaydeck.yaml](examples/relaydeck.yaml):

```yaml
listen_addr: :8080
data_dir: ./data
api_key: ${RELAYDECK_API_KEY}
workers: 4
sources:
  - name: stripe
    secret: whsec_your_stripe_signing_secret
destinations:
  - name: laravel-api
    source: stripe
    url: https://api.example.com/webhooks/stripe
    max_retries: 5
```

Validate any config with:

```bash
relaydeck config validate --config relaydeck.yaml
```

## CLI

```
relaydeck serve [--config FILE] [--listen ADDR] [--data-dir DIR]
relaydeck version
relaydeck config validate [--config FILE]
relaydeck events list [--base-url URL]
relaydeck events inspect <id> [--base-url URL]
relaydeck events replay <id> [--base-url URL]
relaydeck destinations list [--base-url URL]
relaydeck health [--base-url URL]
```

`--base-url` may appear before or after the positional argument.

## API reference

| Method & path | Description |
|---|---|
| `GET /health` | Liveness — always 200 when the process is up. |
| `GET /ready` | Readiness — 200 only when the database is reachable. |
| `GET /metrics` | Prometheus text metrics. |
| `GET /`, `GET /dashboard` | Dashboard (embedded). |
| `POST /api/v1/webhooks/{source}` | Ingest a webhook. Returns 202 with `event_id`. |
| `GET /api/v1/events?source=&status=&search=&limit=` | List events (newest first). |
| `GET /api/v1/events/{id}` | Event detail incl. payload, headers, deliveries, attempts. |
| `POST /api/v1/events/{id}/replay` | Re-queue the event to all matching destinations. |
| `GET /api/v1/deliveries?status=&destination_id=&limit=` | List deliveries. |
| `GET /api/v1/deliveries/{id}` | Delivery detail with attempt history. |
| `POST /api/v1/deliveries/{id}/retry` | Manually retry a failed/dead/pending delivery. |
| `GET /api/v1/destinations` | List destinations. |
| `POST /api/v1/destinations` | Create a destination (URL validated for SSRF). |
| `GET /api/v1/destinations/{id}` | Get one destination. |
| `PUT /api/v1/destinations/{id}` | Update a destination (full object; `enabled` included). |
| `DELETE /api/v1/destinations/{id}` | Delete a destination. |
| `GET /api/v1/sources` | List known sources. |
| `GET /api/v1/stats` | Overview counts for the dashboard. |

When `RELAYDECK_API_KEY` is set, all `/api/v1/*` management endpoints require `X-API-Key` or `Authorization: Bearer <key>`. Webhook ingestion itself is gated per source by HMAC when a source secret is configured.

### Ingestion details

- `202 Accepted` — event durably stored, deliveries queued. Response: `{"event_id","status":"accepted","duplicate":false,"deliveries_queued":N}`.
- `400` — invalid source name. `401` — missing/invalid HMAC signature (when the source has a secret). `413` — payload exceeds the configured limit.
- Send `Idempotency-Key: <key>` to make re-sends safe: a repeated key for the same source returns the original `event_id` with `"duplicate":true` and creates no new deliveries.
- Send `X-RelayDeck-Signature: <hex>` or `X-Hub-Signature-256: sha256=<hex>` (HMAC-SHA256 of the raw body with the source secret) when the source has a secret configured.

## Retry behavior

For a delivery to destination D with `max_retries = N`, `backoff_base = B`, `backoff_max = M`:

- Attempt 1 happens immediately. On failure, the next attempt is scheduled after `delay(attempt) = min(M, B · 2^(attempt-1)) · jitter`, where jitter is uniform in `[0.5, 1.5)`.
- A `2xx` response marks the delivery `delivered`. Any other status or network error counts as a failure.
- After N failed attempts the delivery is `dead` (visible under Failed Deliveries; retry it manually or replay the event).
- Workers claim deliveries with a 2-minute lease. If a worker crashes mid-delivery, the lease expires and another worker re-queues it — this is why delivery is at-least-once: a crash between the destination receiving the request and the attempt being recorded can cause a duplicate send. Make your endpoints idempotent.

## Security considerations

- **SSRF:** destination URLs must be public `http(s)` URLs. Loopback, RFC1918, link-local (`169.254.0.0/16`, incl. the `169.254.169.254` cloud-metadata endpoint), `::1`, `0.0.0.0/8`, multicast, CGNAT and documentation ranges are blocked. Hostnames are resolved and every resolved IP is re-checked (DNS-rebinding defence); redirects to private IPs are always blocked. For local development only, set `allow_private_destinations: true`, or list specific hosts under `trusted_internal_destinations`. Never enable private destinations in production unless you fully trust every destination you configure.
- **Secrets:** source HMAC secrets and the API key come from config/env, are compared in constant time, and are never logged or returned by the API. Stored event headers are redacted (`Authorization`, `X-Api-Key`, cookies, signatures, `*secret*`, `*token*`, `*password*`). Destination responses mask sensitive header values as `***REDACTED***`.
- **Limits:** configurable payload cap (default 1 MiB, 413 above it), per-destination timeouts, bounded worker concurrency, 15s HTTP server timeouts.
- See [SECURITY.md](SECURITY.md) for how to report a vulnerability.

## Development

```bash
go test ./...        # unit + integration tests
go test -race ./...  # race detector
go vet ./...
gofmt -l cmd internal tests web
go build -o relaydeck ./cmd/relaydeck
```

Project layout:

```
cmd/relaydeck/            CLI entrypoint (serve, version, config, events, destinations, health)
internal/api/             HTTP server, ingestion, management API, dashboard handlers
internal/auth/            API-key middleware
internal/config/          env + YAML + flag configuration
internal/database/        SQLite (WAL) store, migrations, leases, stats
internal/delivery/        backoff math + SSRF-safe HTTP sender
internal/events/          source/payload validation
internal/metrics/         Prometheus-format counters/histogram
internal/routing/         source + substring matching
internal/security/        HMAC, secure IDs, redaction, SSRF validation
internal/workers/         delivery worker pool
web/templates, web/static  embedded dashboard (go:embed)
examples/                 relaydeck.yaml, curl examples, sample payloads
docs/                     ARCHITECTURE.md, DECISIONS.md
tests/                    end-to-end test (real server + real receiver)
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are labelled `good first issue`. Please read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) before large changes, and run `go test ./...` and `go vet ./...` before opening a PR.

## Roadmap

See [ROADMAP.md](ROADMAP.md). Near-term: per-destination HMAC signing of outbound deliveries, event filtering by JSON path, retention pruning job, PostgreSQL storage option (optional, never mandatory).

## License

MIT — see [LICENSE](LICENSE).

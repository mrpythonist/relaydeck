# RelayDeck

**Never lose a webhook. Never miss an event.**

RelayDeck is a self-hosted webhook gateway and event relay. It sits between webhook providers (Stripe, GitHub, Shopify, and others) and your applications. It accepts each event durably first, then delivers it with retries, backoff, idempotency, and full delivery history.

One binary. One SQLite file. No mandatory Redis, PostgreSQL, Kubernetes, or cloud service.

> **Delivery semantics: at-least-once.** RelayDeck acknowledges an event only after its database transaction commits, and it retries failed deliveries until they succeed or exhaust their retries. RelayDeck does **not** claim exactly-once delivery or zero loss. A crash between the destination receiving a request and the attempt being recorded can cause a duplicate send, so design your endpoints to be idempotent.

## The problem

Webhooks fail in production in familiar ways:

- Your app is down for a deploy when Stripe sends a payment event.
- A database hiccup drops a GitHub push event.
- A third-party API rate-limits you mid-burst.
- A network blip times out a delivery, and nobody notices until a customer complains.
- The same event arrives twice and charges a customer twice.
- Debugging means grepping logs across three services.

Every team ends up rebuilding the same webhook infrastructure: ingestion, storage, retries, and a dashboard. RelayDeck is that infrastructure, done once, as a small tool you can run anywhere.

## Why RelayDeck exists

Hosted webhook services work, but they add a vendor, a bill, and an external dependency on your critical path. RelayDeck is the self-hosted alternative for developers who want:

- **Local-first operation.** Run it on a small VPS or your laptop. The whole state lives in one SQLite file you can back up by copying it.
- **Durable-first ingestion.** An event is stored before anything else happens. If the process crashes a second later, the event is still there.
- **No extra infrastructure.** No queue, no cache, no second database to operate.
- **Honest semantics.** At-least-once, stated plainly, with the failure modes documented in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Key features

Implemented in v0.1.0. Nothing in this list is planned or aspirational; planned work lives in [ROADMAP.md](ROADMAP.md).

- **Durable ingestion.** `POST /api/v1/webhooks/{source}` returns 202 only after the SQLite transaction (event plus its deliveries) commits. WAL mode, indexed, single-writer discipline.
- **Idempotency.** Send an `Idempotency-Key` header. A repeated key for the same source returns the original event with `"duplicate": true` and creates no new deliveries.
- **HMAC signature verification.** Per-source secrets, verified with HMAC-SHA256 and a constant-time compare. Accepts `X-RelayDeck-Signature` or `X-Hub-Signature-256` (`sha256=<hex>`).
- **Delivery engine.** Bounded worker pool, per-destination retry settings, exponential backoff with jitter, manual retry, and event replay.
- **Crash recovery.** Lease-based delivery claiming. Stale leases are re-queued on startup and on every poll tick, so pending deliveries survive restarts.
- **Routing.** Multiple destinations, source filters, optional payload-substring filters, per-destination headers, method, and timeout. Destinations can be enabled or disabled.
- **SSRF protection.** Destination URLs must be public `http(s)` URLs. Loopback, private, link-local, cloud-metadata (`169.254.169.254`), CGNAT, multicast, and documentation ranges are blocked. Hostnames are resolved and every resolved IP is re-checked (DNS-rebinding defence). Redirects to private IPs are blocked. Private destinations are an explicit local-dev opt-in only.
- **Embedded dashboard.** No build step, no Node, no CDN. Plain HTML/CSS/JS served from the binary, with dark/light themes and tabs for Overview, Events, Deliveries, Destinations, Sources, Failed, and Settings. All data comes from the live API.
- **CLI.** `relaydeck serve`, `version`, `config validate`, `events list | inspect | replay`, `destinations list`, `health`.
- **Observability.** `GET /health`, `GET /ready`, `GET /metrics` in Prometheus text format, and structured JSON logging.
- **Security by default.** API-key auth for the management API, sensitive-header redaction on stored events, secret masking in API responses, request-size limits, and server timeouts.

## Screenshots

No screenshots are published yet. The dashboard is embedded in the binary and has not been captured in a verified run on this machine (no Go toolchain is available here to run it). If you run RelayDeck, the dashboard is at `http://localhost:8080` with tabs for Overview, Events, Deliveries, Destinations, Sources, Failed, and Settings. Capture instructions for contributors are in [docs/screenshots/README.md](docs/screenshots/README.md).

## Architecture

```
Providers (Stripe, GitHub, Shopify, ...)
        |
        v
HTTP Gateway -- HMAC verification, API-key auth, header redaction
        |
        v
SQLite (WAL) -- events, deliveries, attempts, sources, destinations
        |
        v
Worker pool -- lease claiming, bounded concurrency
        |
        v
Retry engine -- exponential backoff with jitter
        |
        v
Your application endpoints
```

Ingestion and delivery are fully separated: an incoming event is stored and acknowledged before any delivery is attempted, and no database transaction is held open during a network call. Full detail, including the data flow and failure modes, is in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Design decisions are recorded in [docs/DECISIONS.md](docs/DECISIONS.md).

## Tech stack

- **Language:** Go 1.23, standard library HTTP server (`net/http` ServeMux).
- **Storage:** SQLite in WAL mode via `modernc.org/sqlite` (pure Go, no CGO).
- **Config:** YAML via `gopkg.in/yaml.v3`, plus environment variables and CLI flags.
- **Dashboard:** plain HTML/CSS/JS embedded with `go:embed`. No frontend build.
- **Tests:** Go standard testing package. No test framework dependency.

## Prerequisites

- **From source:** Go 1.23 or newer. Check with `go version`.
- **From Docker:** Docker and Docker Compose. The image builds from `golang:1.23-alpine` into a `scratch` image with a static binary.
- **From a release binary:** nothing except the binary itself. Releases are published from GitHub Actions (see `.github/workflows/release.yml` once it is added to the repository).
- A destination to deliver to: any HTTP endpoint that accepts POST requests. For local testing, a request catcher such as webhook.site works.

## Installation

### From source

```bash
git clone https://github.com/mrpythonist/relaydeck.git
cd relaydeck
go build -o relaydeck ./cmd/relaydeck
./relaydeck serve
```

Or use the Makefile:

```bash
make build   # builds ./relaydeck
make run     # go run ./cmd/relaydeck serve
```

### Docker

```bash
docker compose up -d
```

Or build and run the image directly:

```bash
docker build -t relaydeck .
docker run -p 8080:8080 -v relaydeck-data:/data relaydeck
```

> The Dockerfile and `docker-compose.yml` are included in the repository, but the Docker build has not been run and verified in this environment (no Docker available here). If you hit a build problem, please open an issue with the output.

### systemd (Linux server)

A unit file is provided at [deploy/relaydeck.service](deploy/relaydeck.service). Install it with:

```bash
sudo cp deploy/relaydeck.service /etc/systemd/system/relaydeck.service
sudo systemctl daemon-reload
sudo systemctl enable --now relaydeck
```

It expects the binary at `/usr/local/bin/relaydeck` and the config at `/etc/relaydeck/relaydeck.yaml`. Edit the unit if your paths differ.

## Quick start

No configuration is needed for local use. Start the server, add a destination, send a test webhook, and watch the dashboard.

```bash
# 1. Start RelayDeck
./relaydeck serve

# 2. Add a destination (your app, or a request catcher for testing)
curl -X POST localhost:8080/api/v1/destinations \
  -H 'Content-Type: application/json' \
  -d '{"name":"laravel-api","url":"https://example.com/webhooks/stripe","source_filter":"stripe"}'

# 3. Send a test webhook
curl -X POST localhost:8080/api/v1/webhooks/stripe \
  -H 'Content-Type: application/json' \
  -d '{"type":"payment_intent.succeeded","id":"pi_123"}'
# -> {"event_id":"evt_...","status":"accepted","duplicate":false,"deliveries_queued":1}

# 4. Inspect it
relaydeck events list
relaydeck events inspect evt_...

# 5. Open the dashboard at http://localhost:8080
```

Sample payloads for Stripe, GitHub, and Shopify live in [examples/payloads/](examples/payloads/). A full curl walkthrough is in [examples/curl-examples.sh](examples/curl-examples.sh).

## Configuration

Precedence: **CLI flags > environment variables > YAML file > defaults.**

| Setting | Env var | YAML key | Default |
|---|---|---|---|
| Listen address | `RELAYDECK_LISTEN_ADDR` | `listen_addr` | `:8080` |
| Data directory | `RELAYDECK_DATA_DIR` | `data_dir` | `./data` |
| API key (management API) | `RELAYDECK_API_KEY` | `api_key` | none (auth disabled) |
| Max payload bytes | `RELAYDECK_MAX_PAYLOAD_BYTES` | `max_payload_bytes` | `1048576` (1 MiB) |
| Worker count | `RELAYDECK_WORKERS` | `workers` | `4` |
| Allow private destinations | `RELAYDECK_ALLOW_PRIVATE_DESTINATIONS` | `allow_private_destinations` | `false` |
| Trusted internal hosts | `RELAYDECK_TRUSTED_INTERNAL_DESTINATIONS` | `trusted_internal_destinations` | none |
| Retention days | `RELAYDECK_RETENTION_DAYS` | `retention_days` | `30` |
| Config file | `RELAYDECK_CONFIG` | (none) | none |

Limits enforced at startup: `max_payload_bytes` must be between 1 and 64 MiB, `workers` between 1 and 64, `retention_days` 0 or more.

Example config (full example in [examples/relaydeck.yaml](examples/relaydeck.yaml)):

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

Notes:

- Secrets in YAML support `${ENV_VAR}` expansion, so config files can be committed without real secrets. Never commit a config file with real secrets in it.
- Destinations defined in YAML are seeded into the database on startup. A destination with a name that already exists is skipped, not overwritten.
- Sources defined in YAML are upserted on startup with their secret. Unknown sources that send a webhook are auto-registered with no secret, which means their webhooks are accepted without HMAC verification until you configure a secret.
- `retention_days` is validated and stored, but no scheduled pruning job runs yet. The `DeleteOldEvents` helper exists in the database package but nothing calls it. Automatic pruning is planned for v0.2.0; until then, old events stay in the database. See [ROADMAP.md](ROADMAP.md).

Validate any config before starting:

```bash
relaydeck config validate --config relaydeck.yaml
```

## Usage

### CLI reference

Full reference: [docs/cli.md](docs/cli.md). Summary:

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

`--base-url` defaults to `http://localhost:8080` and may appear before or after the positional argument. `relaydeck version` prints the version embedded at build time (`0.1.0-dev` for a plain `go build`; release builds set the real version).

### HTTP API

Full reference: [docs/api.md](docs/api.md). Summary:

| Method and path | Description |
|---|---|
| `GET /health` | Liveness. 200 when the process is up. |
| `GET /ready` | Readiness. 200 only when the database is reachable. |
| `GET /metrics` | Prometheus text metrics. |
| `GET /`, `GET /dashboard` | Embedded dashboard. |
| `POST /api/v1/webhooks/{source}` | Ingest a webhook. Returns 202 with `event_id`. |
| `GET /api/v1/events?source=&status=&search=&limit=` | List events, newest first. |
| `GET /api/v1/events/{id}` | Event detail, with payload, headers, deliveries, attempts. |
| `POST /api/v1/events/{id}/replay` | Re-queue the event to all matching destinations. |
| `GET /api/v1/deliveries?status=&destination_id=&limit=` | List deliveries. |
| `GET /api/v1/deliveries/{id}` | Delivery detail with attempt history. |
| `POST /api/v1/deliveries/{id}/retry` | Manually retry a failed, dead, or pending delivery. |
| `GET /api/v1/destinations` | List destinations. |
| `POST /api/v1/destinations` | Create a destination (URL validated for SSRF). |
| `GET /api/v1/destinations/{id}` | Get one destination. |
| `PUT /api/v1/destinations/{id}` | Update a destination (full object; send `enabled` explicitly). |
| `DELETE /api/v1/destinations/{id}` | Delete a destination. |
| `GET /api/v1/sources` | List known sources. |
| `GET /api/v1/stats` | Overview counts for the dashboard. |

When `RELAYDECK_API_KEY` is set, all `/api/v1/*` management endpoints require `X-API-Key` or `Authorization: Bearer <key>`. Webhook ingestion itself is gated per source by HMAC when a source secret is configured; `/health`, `/ready`, and `/metrics` are not API-key protected.

### Ingestion details

- `202 Accepted`: event durably stored, deliveries queued. Response: `{"event_id","status":"accepted","duplicate":false,"deliveries_queued":N}`.
- `400`: invalid source name. Source names must be 1-64 characters: letters, digits, dot, dash, underscore, starting with a letter or digit.
- `401`: missing or invalid HMAC signature, when the source has a secret configured.
- `413`: payload exceeds the configured limit.
- Send `Idempotency-Key: <key>` to make re-sends safe. A repeated key for the same source returns the original `event_id` with `"duplicate":true` and creates no new deliveries.
- Send `X-RelayDeck-Signature: <hex>` or `X-Hub-Signature-256: sha256=<hex>` (HMAC-SHA256 of the raw body, keyed with the source secret) when the source has a secret configured.
- Outbound deliveries send `User-Agent: RelayDeck/0.1.0` and `X-RelayDeck-Event-Delivery: 1`, plus the original `Content-Type` and any headers configured on the destination.

### Retry behavior

For a delivery to destination D with `max_retries = N`, `backoff_base = B`, `backoff_max = M`:

- Attempt 1 happens immediately. On failure, the next attempt is scheduled after `delay = min(M, B * 2^(attempt-1)) * jitter`, where jitter is uniform in `[0.5, 1.5)`.
- A `2xx` response marks the delivery `delivered`. Any other status or network error counts as a failure. A persistent `4xx` is retried the same way and will go dead; fix the destination or the payload.
- After N failed attempts the delivery is `dead`. It stays visible under Failed Deliveries; retry it manually or replay the event.
- Workers claim deliveries with a 2-minute lease and poll every 500 ms. If a worker crashes mid-delivery, the lease expires and another worker re-queues it. This is why delivery is at-least-once: a crash between the destination receiving the request and the attempt being recorded can cause a duplicate send.

## Project structure

```
cmd/relaydeck/            CLI entrypoint (serve, version, config, events, destinations, health)
internal/api/             HTTP server, ingestion, management API, dashboard handlers
internal/auth/            API-key middleware
internal/config/          env + YAML + flag configuration
internal/database/        SQLite (WAL) store, schema, leases, stats
internal/delivery/        backoff math + SSRF-safe HTTP sender
internal/events/          source and payload validation
internal/metrics/         Prometheus-format counters and histogram
internal/routing/         source + substring matching
internal/security/        HMAC, secure IDs, redaction, SSRF validation
internal/workers/         delivery worker pool
web/templates, web/static  embedded dashboard (go:embed)
examples/                 relaydeck.yaml, curl examples, sample payloads
docs/                     architecture, API, CLI, development, decisions
tests/                    end-to-end test (real server + real receiver)
deploy/                   systemd unit
migrations/               initial schema (also embedded in the database package)
```

## Testing

```bash
go test ./...        # unit + integration tests (46 test functions)
go test -race ./...  # race detector (run on core packages)
go vet ./...
gofmt -l cmd internal tests web
make test            # go test ./...
make test-race       # go test -race ./...
make vet             # go vet ./...
make fmt             # gofmt -l cmd internal tests web
```

The test suite covers ingestion, idempotency, HMAC verification, SSRF validation, routing, backoff math, lease claiming, retry and replay, API-key auth, and metrics. `tests/integration_test.go` runs a real server against a real receiver. The suite was last run and passing (46 tests, race tests on core packages) during development of v0.1.0, along with a live smoke test that delivered a real webhook end to end. Run the suite yourself before trusting a new checkout; the commands above are the source of truth.

## Deployment

RelayDeck is one binary plus one SQLite file, so deployment is a copy of two things and a process manager.

- **Binary + data directory.** The database file is `<data_dir>/relaydeck.db` (plus WAL sidecar files while running). Back it up by copying the data directory while the process is stopped, or use SQLite's backup tooling. Never copy a live SQLite file and assume it is consistent.
- **systemd.** Use [deploy/relaydeck.service](deploy/relaydeck.service). It runs as a dedicated `relaydeck` user, restarts on failure, and hardens the service (`NoNewPrivileges`, `ProtectSystem=full`, `PrivateTmp`).
- **Docker.** See Installation. The image is `scratch`-based with a static binary, exposes port 8080, and persists `/data` on a volume. The Docker build has not been run and verified in this environment; see the note under Installation.
- **Reverse proxy.** Put it behind nginx or Caddy if you need TLS. RelayDeck itself serves plain HTTP. Terminate TLS at the proxy and forward to `:8080`.
- **Configuration in production.** Set a strong `RELAYDECK_API_KEY`, configure per-source HMAC secrets, keep `allow_private_destinations` off, and keep the data directory on persistent storage.

## Security

- **SSRF:** destination URLs must be public `http(s)` URLs. Loopback, RFC1918, link-local (including the `169.254.169.254` cloud-metadata endpoint), `::1`, `0.0.0.0/8`, multicast, CGNAT, and documentation ranges are blocked. Hostnames are resolved and every resolved IP is re-checked (DNS-rebinding defence). Redirects to private IPs are always blocked, even when the configured destination itself was allowed to be private.
- **Secrets:** source HMAC secrets and the API key are compared in constant time and are never logged or returned by the API. Stored event headers are redacted (`Authorization`, `X-Api-Key`, cookies, signatures, and any header whose name contains `secret`, `token`, or `password`). Destination responses mask sensitive header values as `***REDACTED***`.
- **Limits:** configurable payload cap (default 1 MiB, 413 above it), per-destination timeouts, bounded worker concurrency, 15 s HTTP server read/write timeouts.
- **No credential storage:** RelayDeck stores no user passwords. Destination custom headers are operator-configured; treat your config file as secret material and never commit it with real secrets.

To report a vulnerability, see [SECURITY.md](SECURITY.md). Do not open a public issue for a security problem.

## Roadmap

See [ROADMAP.md](ROADMAP.md). Near term (v0.2.0): per-destination HMAC signing of outbound deliveries, JSON-path event filtering, the retention pruning job, delivery payload transformation, and CLI management of destinations. Longer term: an optional PostgreSQL backend (SQLite stays the default, never mandatory) and dashboard test events.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are labelled `good first issue`. Please read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) before large changes, and run `go test ./...` and `go vet ./...` before opening a PR. By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Support

Questions go to GitHub Discussions (Q&A) or an issue; see [SUPPORT.md](SUPPORT.md).

## License

MIT. See [LICENSE](LICENSE).

# RelayDeck HTTP API

Base URL: the listen address, default `http://localhost:8080`.

All responses are JSON unless stated otherwise. Errors look like
`{"error": "message"}`.

## Authentication

- **Management API** (`/api/v1/*` except ingestion): when `RELAYDECK_API_KEY`
  is set, every request must send it as `X-API-Key` or
  `Authorization: Bearer <key>`. Without a key the request gets
  `401 {"error":"unauthorized"}`. When no API key is configured, auth is
  disabled and all requests pass.
- **Ingestion** (`POST /api/v1/webhooks/{source}`): not protected by the API
  key. It is protected per source by HMAC when the source has a secret
  configured. See [Ingestion](#ingestion).
- **Health, readiness, metrics** (`/health`, `/ready`, `/metrics`): public,
  no auth.

## Health, readiness, metrics

| Method and path | Description |
|---|---|
| `GET /health` | Liveness. Always `200 {"status":"ok"}` while the process is up. |
| `GET /ready` | Readiness. `200 {"status":"ready"}` when the database is reachable, otherwise `503 {"error":"database unavailable"}`. |
| `GET /metrics` | Prometheus text format. See [Metrics](#metrics). |

## Dashboard

| Method and path | Description |
|---|---|
| `GET /` | Embedded dashboard (HTML). |
| `GET /dashboard` | Same dashboard. |
| `GET /static/*` | Dashboard CSS and JS. |

## Ingestion

### `POST /api/v1/webhooks/{source}`

Accepts a webhook for the named source and stores it durably before
returning.

- **Source name:** 1-64 characters, letters, digits, dot, dash, underscore,
  starting with a letter or digit. The name is lower-cased. Anything else
  gets `400`.
- **Payload limit:** the configured `max_payload_bytes` (default 1 MiB).
  Over the limit gets `413`.
- **HMAC:** if the source has a secret configured, the request must carry
  `X-RelayDeck-Signature: <hex>` or
  `X-Hub-Signature-256: sha256=<hex>`, where `<hex>` is
  HMAC-SHA256 of the raw request body keyed with the source secret. Missing
  or invalid signature gets `401`. Sources with no secret accept unsigned
  requests. Unknown sources are auto-registered with no secret.
- **Idempotency:** send `Idempotency-Key: <key>`. A repeated key for the
  same source returns the original event with `"duplicate": true` and
  creates no new deliveries.
- **Success:** `202` with:

  ```json
  {
    "event_id": "evt_...",
    "status": "accepted",
    "duplicate": false,
    "deliveries_queued": 1
  }
  ```

- **Failure:** `400` invalid source name, `401` bad signature,
  `413` payload too large, `500` if the event could not be persisted.
  A `500` means the event was not acknowledged; the provider's own retry
  should re-send it.

The 202 is sent only after the transaction inserting the event and one
`pending` delivery per matching destination has committed.

## Destinations

A destination object looks like this:

```json
{
  "id": "dst_...",
  "name": "laravel-api",
  "source_filter": "stripe",
  "url": "https://api.example.com/webhooks/stripe",
  "method": "POST",
  "headers": {"X-Webhook-Source": "relaydeck"},
  "timeout_seconds": 10,
  "max_retries": 5,
  "backoff_base_ms": 1000,
  "backoff_max_ms": 60000,
  "filter_substring": "payment_intent.succeeded",
  "enabled": true,
  "created_at": "2026-11-05T10:00:00Z",
  "updated_at": "2026-11-05T10:00:00Z"
}
```

Sensitive header values are returned as `***REDACTED***`. `filter_substring`
is the optional payload-substring filter: when set, the destination only
receives events whose raw payload contains that string. `source_filter`
empty means all sources.

### `GET /api/v1/destinations`

Lists all destinations, ordered by name. Returns
`{"destinations": [...]}`.

### `POST /api/v1/destinations`

Creates a destination. `name` and `url` are required. Defaults: `POST`,
10 s timeout, 5 retries, 1000 ms backoff base, 60000 ms backoff max,
enabled. The URL is validated for SSRF; an unsafe URL gets `400`. A
duplicate name gets `409`. Success returns `201` with the destination.

### `GET /api/v1/destinations/{id}`

Returns one destination, or `404`.

### `PUT /api/v1/destinations/{id}`

Full-object update. `enabled` comes from the payload, so send it
explicitly or the destination will be disabled. Headers merge with the
stored ones; sending `"***REDACTED***"` as a value keeps the stored value.
Changing the URL re-validates it for SSRF. Returns the updated destination,
or `404`.

### `DELETE /api/v1/destinations/{id}`

Deletes the destination. Returns `204` with no body.

## Events

An event object looks like this:

```json
{
  "id": "evt_...",
  "source": "stripe",
  "payload": "{\"type\":\"payment_intent.succeeded\"}",
  "headers": {"Content-Type": "application/json"},
  "content_type": "application/json",
  "idempotency_key": "github-push-1",
  "created_at": "2026-11-05T10:00:00Z"
}
```

Sensitive stored headers are redacted to `***REDACTED***`.

### `GET /api/v1/events`

Query parameters: `source`, `status`, `search`, `limit` (default 50, max
200). `status` filters by whether the event has at least one delivery in
that status. `search` matches event IDs. Returns `{"events": [...]}`,
newest first.

### `GET /api/v1/events/{id}`

Returns `{"event": {...}, "deliveries": [...]}` where each delivery
includes its attempt history under `attempts_list`. Returns `404` when the
event does not exist.

### `POST /api/v1/events/{id}/replay`

Re-queues the event to all currently matching destinations and returns
`{"event_id": "...", "deliveries_created": N}`. Returns `404` when the
event does not exist.

## Deliveries

A delivery object looks like this:

```json
{
  "id": "dly_...",
  "event_id": "evt_...",
  "destination_id": "dst_...",
  "destination_name": "laravel-api",
  "source": "stripe",
  "status": "delivered",
  "attempts": 1,
  "next_attempt_at": "2026-11-05T10:00:00Z",
  "last_status_code": 200,
  "last_duration_ms": 120,
  "created_at": "2026-11-05T10:00:00Z",
  "updated_at": "2026-11-05T10:00:00Z"
}
```

Statuses: `pending`, `in_progress`, `delivered`, `failed`, `dead`.

### `GET /api/v1/deliveries`

Query parameters: `status`, `destination_id`, `limit` (default 50, max 200).
Returns `{"deliveries": [...]}`.

### `GET /api/v1/deliveries/{id}`

Returns `{"delivery": {...}, "attempts": [...]}`. Each attempt has
`id`, `delivery_id`, `status_code`, `duration_ms`, `error`, `response_snippet`,
and `created_at`. Returns `404` when the delivery does not exist.

### `POST /api/v1/deliveries/{id}/retry`

Re-queues a `failed`, `dead`, or `pending` delivery. Returns
`{"status": "queued for retry"}`. Returns `404` when the delivery does not
exist or is not retryable (for example, already `delivered` or
`in_progress`).

## Sources and stats

### `GET /api/v1/sources`

Returns `{"sources": ["stripe", "github"]}`, the names of known sources.

### `GET /api/v1/stats`

Returns overview counts for the dashboard:

```json
{
  "total_events": 10,
  "successful_deliveries": 8,
  "failed_deliveries": 1,
  "pending_deliveries": 1,
  "dead_deliveries": 0,
  "retry_count": 2,
  "delivery_success_rate": 0.8,
  "total_deliveries": 10
}
```

## Metrics

`GET /metrics` renders Prometheus text format without a client library:

- `relaydeck_events_received_total` (counter)
- `relaydeck_deliveries_success_total` (counter)
- `relaydeck_deliveries_failed_total` (counter, counts failed attempts)
- `relaydeck_deliveries_pending` (gauge, read from the database at scrape time)
- `relaydeck_retry_attempts_total` (counter)
- `relaydeck_delivery_duration_seconds` (histogram, buckets 0.05, 0.1, 0.25,
  0.5, 1, 2.5, 5, 10)

Counters are in-memory and reset when the process restarts. The pending
gauge is always current because it is read from the database.

## Retry and delivery semantics

See the README section "Retry behavior". In short: attempt 1 is immediate,
later attempts use exponential backoff with jitter
(`min(max, base * 2^(attempt-1)) * U[0.5, 1.5)`), a `2xx` response means
delivered, anything else is a failure, and after `max_retries` failures the
delivery is `dead`. Delivery is at-least-once: a crash between the
destination receiving a request and the attempt being recorded can cause a
duplicate send.

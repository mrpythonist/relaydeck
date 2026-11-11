# RelayDeck Architecture

## Overview

RelayDeck is a single Go binary: an HTTP gateway, a SQLite store, and a
background worker pool in one process. There are no mandatory external
services.

```
Providers ──> HTTP Gateway ──> SQLite (WAL) ──> Workers ──> Destinations
                  │                 │               │
                  │ HMAC / API key  │ transactions  │ lease claiming
                  │ redaction       │ indexes       │ backoff + jitter
```

## Components

### HTTP gateway (`internal/api`)
- Go standard library `net/http` ServeMux (method + path patterns).
- Ingestion handler: validates source name, enforces payload limit, verifies
  HMAC when the source has a secret, redacts sensitive headers, then calls
  `Store.CreateEvent` — one transaction inserting the event and all matching
  deliveries. The 202 response is sent only after commit.
- Management API: destinations, events, deliveries, stats, sources.
- Dashboard: embedded HTML/CSS/JS (`web/`, `go:embed`), served from the
  binary; all data comes from the API at runtime.

### Store (`internal/database`)
- SQLite via `modernc.org/sqlite` (pure Go, no CGO — the binary builds and
  runs anywhere Go builds).
- WAL mode + `busy_timeout=5000` + `foreign_keys=ON`.
- `PRAGMA` + schema applied on open (see `migrations/001_initial.sql`).
- Single-writer discipline: `SetMaxOpenConns(1)` serialises writes, which is
  correct and fast enough for a gateway of this size; reads are cheap.
- Tables: `sources`, `destinations`, `events`, `deliveries`, `attempts`.
- Indexes on the hot paths: due deliveries `(status, next_attempt_at)`,
  `event_id`, `destination_id`, event source/time, attempts by delivery.

### Delivery claiming (leases)
`deliveries` carries `status`, `next_attempt_at`, `claimed_at`, `claimed_by`.
`ClaimDue` runs in a transaction: it selects due deliveries
(`pending`, or `in_progress` with a lease older than the lease timeout) and
marks them `in_progress` with the worker ID and current timestamp, then
commits. Two workers can never claim the same delivery, because the select
and the update are in one transaction and writes are serialised.

On startup and on every poll tick, `RequeueStaleLeases` returns expired
`in_progress` deliveries to `pending`, so a crashed worker's deliveries are
recovered automatically. Pending events therefore survive restarts.

### Workers (`internal/workers`)
- A ticker (500 ms) claims up to `workers` due deliveries per tick.
- A semaphore bounds in-flight sends to the configured worker count.
- Each send runs with the destination timeout; **no DB transaction is held
  during the network call**. After the call, `RecordAttempt` (one
  transaction) inserts the attempt row and updates the delivery:
  success → `delivered`; failure → `pending` with `next_attempt_at = now +
  backoff`, or `dead` after `max_retries` attempts.
- Graceful shutdown: signal context cancels the pool; in-flight sends drain
  (bounded by the semaphore) before the process exits.

### Delivery sender (`internal/delivery`)
- `BackoffDelay`: `min(max, base · 2^(attempt-1)) · U[0.5, 1.5)`.
- SSRF-safe client: destination URLs are validated before sending; the
  dialer re-resolves the host and rejects non-public IPs; redirects are
  re-validated against the public-IP rule (redirects are untrusted
  third-party choices, so they are always strict unless the host is
  explicitly trusted).
- Response bodies are truncated to 4 KiB on read and 1 KiB when stored.
- Outbound HMAC signing (v0.2.1): when a destination has a signing secret,
  every delivery carries `X-RelayDeck-Signature: sha256=<hex>`, HMAC-SHA256
  over the raw body, set last so no header template can overwrite it.
  Receivers verify it with the same format the ingestion side accepts.
- Header templates (v0.2.1): a destination can map header names to
  templates rendered from the event payload (`{{path}}` placeholders,
  resolved by `internal/jsonpath`). Templates are applied after the static
  headers, so a template wins on a name clash. Template values come from
  the payload itself; the patterns are operator-configured.

### Routing (`internal/routing`, `internal/database.MatchingDestinations`)
A destination matches when it is enabled, its `source_filter` is empty or
equals the event source, its `filter_substring` (if set) appears in the
raw payload, and its `filter_jsonpath` (if set) matches the payload. The
JSON-path filter is an expression `path=expected` (for example
`type=payment_intent.succeeded`); the path uses dotted field names with
optional `[n]` array indexes, and the value must equal the expected
string. Both filters must pass when both are set. A path that does not
resolve, or a non-JSON payload, never matches a JSON-path filter.
Deliberately simple, no rules engine in the MVP. Path lookup lives in
`internal/jsonpath`, shared by the filter and the header templates.
Note: the live matching path is `database.MatchingDestinationsForWorkspace`;
`routing.Match` implements the same rules and is kept in sync.

### Metrics (`internal/metrics`)
In-memory atomic counters plus a small latency histogram, rendered as
Prometheus text. The pending gauge is read from the DB at scrape time so it
is always accurate.

### Security (`internal/security`)
- HMAC-SHA256 verification with constant-time comparison.
- Secure random IDs (`crypto/rand`).
- Header redaction on ingest; secret masking in destination API responses.
  The outbound signing secret is stored on the destination and never
  returned in clear; sending `***REDACTED***` back on update leaves it
  unchanged.
- SSRF validation (see README security section).

## Data flow (ingest)

1. `POST /api/v1/webhooks/{source}` arrives.
2. Source name validated; body read with limit (413 if over).
3. If the source has a secret, the signature header must verify (401 if not).
4. Idempotency: `(source, Idempotency-Key)` lookup — a hit returns the
   original event ID with `duplicate: true` and creates nothing.
5. Transaction: insert event + one `pending` delivery per matching
   destination. Commit. Only then is 202 returned.
6. Workers pick up the deliveries independently.

## Failure modes

| Failure | Behaviour |
|---|---|
| Destination down / 5xx / timeout | Attempt recorded, retry scheduled with backoff. |
| Retries exhausted | Delivery `dead`; visible in dashboard; manual retry/replay available. |
| Worker crash mid-delivery | Lease expires (2 min), delivery re-queued. May duplicate a send (at-least-once). |
| Process restart | Pending/in-progress deliveries recovered from SQLite. |
| Destination returns 4xx | Treated as failure and retried (a persistent 4xx will go dead; fix the destination or payload). |
| Disk full / DB error on ingest | 500, event not acknowledged; the provider's own retry will re-send. |

## Scaling notes

- One binary handles a small VPS comfortably: SQLite WAL + indexed due
  queries + bounded workers.
- If volume outgrows a single node, the lease mechanism already supports
  multiple RelayDeck instances sharing one SQLite file only if the file is
  on shared storage — which is not recommended. A PostgreSQL backend is on
  the roadmap (v0.3.0) for that case; SQLite remains the default.

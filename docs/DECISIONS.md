# Architecture Decision Records

## ADR-001: Go, standard library HTTP
Go gives a single static binary, low memory, and fast startup — the right
shape for a self-hosted gateway on a small VPS. The standard library mux
(Go 1.22+ patterns) is enough; no web framework dependency.

## ADR-002: SQLite with WAL as the only store
- **Chosen:** SQLite (WAL mode) via `modernc.org/sqlite` (pure Go, no CGO).
- **Why:** zero external services, one file to back up, transactions give us
  durable acceptance, and the workload (append events, claim due deliveries)
  fits SQLite well with the right indexes.
- **Rejected:** PostgreSQL/Redis as requirements — they raise the bar for
  self-hosting. PostgreSQL may come later as an *optional* backend.
- **Trade-off:** single-writer serialisation (`SetMaxOpenConns(1)`). Fine at
  this scale; documented in ARCHITECTURE.md.

## ADR-003: modernc.org/sqlite over mattn/go-sqlite3
`mattn/go-sqlite3` needs CGO, which complicates cross-compilation and
`scratch` Docker images. modernc is pure Go. Pinned to v1.36.0 for Go 1.23
compatibility (later versions require a newer Go toolchain).

## ADR-004: At-least-once delivery, never exactly-once
A crash between the destination receiving a request and the attempt being
recorded can duplicate a send. No design can remove that window without
two-phase commit with the destination, which webhooks do not offer. We state
at-least-once everywhere and never claim zero loss or exactly-once.

## ADR-005: Lease-based delivery claiming
Instead of a separate queue, deliveries are claimed in the DB with
`claimed_at`/`claimed_by` and a 2-minute lease. One transaction selects and
marks due deliveries, so concurrent workers cannot double-claim; stale
leases are re-queued on startup and every poll tick, giving crash recovery
with no extra infrastructure.

## ADR-006: SSRF protection is strict by default
RelayDeck makes outbound requests to operator-configured URLs, so SSRF is a
first-class threat. Defaults block loopback, private, link-local, metadata,
CGNAT and documentation ranges; hostnames are resolved and every IP
re-checked; redirects are always validated against the public-IP rule.
Private destinations require an explicit opt-in (`allow_private_destinations`
or `trusted_internal_destinations`) that is documented as local-dev only.

## ADR-007: Embedded dashboard, no build step
The dashboard is plain HTML/CSS/JS embedded with `go:embed`. No Node, no
CDN, no build pipeline — the binary stays self-contained and the dashboard
works offline. All data comes from the live API; no fake or hardcoded data.

## ADR-008: Config precedence — flags > env > YAML > defaults
Matches operator expectations for twelve-factor apps and local development.
YAML supports `${ENV_VAR}` expansion for secrets so config files can be
committed without real secrets.

## ADR-009: Prometheus metrics without a client library
The metrics surface is small (counters + one histogram). Rendering the text
format by hand avoids a dependency and keeps the binary light.

## ADR-010: No rules engine in the MVP
Routing is source filter + optional payload-substring filter. A JSON-path
filter is planned (v0.2.0) only after the core proves itself; premature
rule engines are the classic way gateway projects stall.

Update (v0.2.1, 2026-11-11): the JSON-path filter landed, kept to the
same small shape — one `path=expected` expression per destination, dotted
paths with optional `[n]` indexes, both filters must pass when both are
set. Still no rules engine; the filter answers one question (does this
event belong here) and nothing else.

## ADR-011: Outbound HMAC signing reuses the ingestion format
Deliveries to a destination with a signing secret carry
`X-RelayDeck-Signature: sha256=<hex>`, HMAC-SHA256 over the raw body.
This is the same format the ingestion side verifies (`X-Hub-Signature-256`
style), so one code path (`security.HMACSHA256Hex` /
`security.VerifySignature`) serves both directions and receivers can
verify with any standard HMAC-SHA256 check. The signature is set last in
the sender, after static headers and header templates, so no template can
overwrite it. The secret is stored on the destination (like source
secrets) and never returned by the API in clear.

## ADR-012: Header templates are plain `{{path}}` substitution
Header injection uses the same dotted-path lookup as the JSON-path filter
(`internal/jsonpath`), not Go's `text/template`: templates are small
(`X-Event-Type: {{.type}}`), the lookup is one function, and a missing
path renders as an empty string instead of an error. Template values come
from the event payload, so a template can only surface data the event
already carried. Templates are applied after static headers and win on a
name clash; the HMAC signature is applied after both.

# RelayDeck Hosted SaaS - Phase 1

Hosted RelayDeck is the same Go binary as the self-hosted gateway, run in
multi-tenant mode. One service, one SQLite file, workspaces isolated by
`workspace_id` on every row. Phase 1 is inbound receiving first: it takes
webhooks from Stripe, GitHub, Shopify and other providers and delivers
them to your endpoints with retries. It is not an outbound customer
webhook portal like Svix. Delivery is at-least-once. No exactly-once or
zero-loss guarantee is claimed.

## What Phase 1 includes

- Signup, login and logout with email and password (bcrypt). Sessions are
  random tokens stored only as a SHA-256 hash, in an HttpOnly cookie,
  expiring after 7 days.
- Workspaces with an owner and members. Each workspace has a random
  public id used in its ingest URL.
- Workspace API keys, shown once at creation, stored only as a hash,
  revocable from the API.
- Per-workspace ingestion: `POST /ingest/{workspace_public_id}/{source}`.
  HMAC verification per source is unchanged from self-hosted mode.
- Plans and limits in code, in `internal/plans/plans.go`:
  Free $0: 1,000 events a month, 3 destinations, 3 days retention.
  Starter $19: 50,000 events a month, 30 days retention.
  Growth $49: 500,000 events a month, 90 days retention.
  Over the monthly limit, ingestion returns HTTP 429 with the plan, the
  limit and the used count. Retries do not count against the limit.
- Usage counting per workspace per calendar month, and
  `GET /api/v1/workspaces/{id}/usage`.
- Retention pruning: a background job runs every hour and deletes events
  older than each workspace's plan retention, with their deliveries and
  attempts. Manual trigger: `relaydeck prune [--workspace ID]` or
  `POST /api/v1/workspaces/{id}/prune`.
- Stripe billing code: Checkout session creation and a webhook handler
  for `checkout.session.completed` and `customer.subscription.deleted`.
  No live Stripe call is made by the tests, and no price ID is hard-coded.
- Landing and pricing page at `GET /pricing` (hosted mode only) and a
  login/signup page at `/` when no session exists.

## What Phase 1 does not include

- Postgres. Hosted Phase 1 stays on SQLite (WAL), one shared file, with
  `workspace_id` on every tenant row. Postgres is Phase 2, when paying
  volume justifies it.
- A live deployed service, a live Stripe account, or a domain. See below.
- GitHub OAuth, SSO, team invites beyond owner/member, audit log, or a
  customer-facing outbound webhook portal.
- Docker image publishing or CI runs here. The Docker build was never run
  on this machine, and the CI workflows are not pushable via the API.
  Those steps are manual, see the checklist at the end.

## Tenancy model

- `users` own accounts. `workspaces` are tenants. `workspace_memberships`
  links them (owner/member).
- `sources`, `destinations`, `events` and `deliveries` all carry
  `workspace_id`. The empty workspace ("") is legacy single-tenant mode.
- Composite keys stop cross-tenant collisions: sources are keyed by
  `(workspace_id, name)`, destinations are unique on
  `(workspace_id, name)`, events are unique on
  `(workspace_id, source, idempotency_key)`.
- Every management query filters by `workspace_id`. Workers claim
  deliveries globally (they serve all workspaces) but only by delivery
  id; the API never returns a row from another workspace.
- Migration `migrations/002_hosted.sql` documents the new tables. The
  application also migrates pre-Phase-1 databases in place on startup:
  it adds the columns and rebuilds the three tables with workspace-scoped
  keys, keeping every old row with workspace_id "".

## Running hosted mode

```bash
RELAYDECK_HOSTED=true \
RELAYDECK_LISTEN_ADDR=:8080 \
RELAYDECK_DATA_DIR=/var/lib/relaydeck \
relaydeck serve
```

Self-hosted mode is the default (`RELAYDECK_HOSTED` unset or false).
Legacy ingestion `POST /api/v1/webhooks/{source}` works there and
returns HTTP 410 in hosted mode.

## Environment variables

| Variable | Required in hosted mode | Notes |
|---|---|---|
| `RELAYDECK_HOSTED` | yes, `true` | Enables multi-tenancy, auth and billing routes |
| `RELAYDECK_LISTEN_ADDR` | no, default `:8080` | Bind address |
| `RELAYDECK_DATA_DIR` | no, default `./data` | SQLite file lives here as `relaydeck.db` |
| `RELAYDECK_API_KEY` | no | Legacy global key; not used for workspace auth in hosted mode |
| `RELAYDECK_WORKERS` | no, default `4` | Delivery worker count |
| `RELAYDECK_RETENTION_DAYS` | no, default `30` | Fallback retention for the legacy workspace |
| `STRIPE_SECRET_KEY` | for paid checkout | Stripe secret key, `sk_test_...` then `sk_live_...` |
| `STRIPE_WEBHOOK_SECRET` | for webhook verification | From the Stripe webhook endpoint, `whsec_...` |
| `STRIPE_PRICE_STARTER` | for Starter checkout | Stripe price id, `price_...` |
| `STRIPE_PRICE_GROWTH` | for Growth checkout | Stripe price id, `price_...` |
| `STRIPE_SUCCESS_URL` | no | Defaults to `https://<host>/dashboard?billing=success` |
| `STRIPE_CANCEL_URL` | no | Defaults to `https://<host>/pricing?billing=cancelled` |

Copy `.env.example` to `.env` for local development. Never commit a
real `.env` or any secret.

## Stripe setup (manual, in the Stripe dashboard)

1. Create a Stripe account and switch to test mode first.
2. Products and prices: create a product "RelayDeck Starter" with a
   $19/month recurring price, and "RelayDeck Growth" with a $49/month
   recurring price. Copy each price id (`price_...`) into
   `STRIPE_PRICE_STARTER` and `STRIPE_PRICE_GROWTH`.
3. API keys: copy the test secret key (`sk_test_...`) into
   `STRIPE_SECRET_KEY`. Use the live key only after testing.
4. Webhooks: add an endpoint for
   `https://<your-domain>/api/v1/billing/stripe-webhook` and subscribe to
   `checkout.session.completed` and `customer.subscription.deleted`.
   Copy the signing secret (`whsec_...`) into `STRIPE_WEBHOOK_SECRET`.
5. Test: sign up on the hosted service, start a Starter checkout, pay
   with Stripe test card 4242 4242 4242 4242, and check the workspace plan
   becomes `starter`. Then cancel the subscription in Stripe and check
   the plan returns to `free`. None of this has been run live yet.
6. Checkout creates a session with metadata `workspace_id` and `plan`.
   The webhook handler reads that metadata to set the plan, so do not
   remove it.

## Deploying on a $5-10 VPS

1. Rent a small VPS (for example Hetzner or DigitalOcean, 1 vCPU, 1 GB RAM).
   Shared hosting cannot run a Go binary; this needs a VPS.
2. Copy the binary to `/usr/local/bin/relaydeck` and the systemd unit
   from `deploy/relaydeck.service` to `/etc/systemd/system/`.
3. Create the `relaydeck` user and `/var/lib/relaydeck` data directory.
4. Put the environment variables in an env file, for example
   `/etc/relaydeck/env`, and point the unit at it with
   `EnvironmentFile=/etc/relaydeck/env`. The unit in this repo is a
   template; set `RELAYDECK_HOSTED=true` there.
5. Put nginx or Caddy in front for TLS, proxying to `127.0.0.1:8080`.
   Do not expose the Go server directly without TLS in production.
6. Back up `/var/lib/relaydeck/relaydeck.db` daily. SQLite is one file;
   copy it while the service is stopped, or use the SQLite backup API.
   Test restoring a backup before you need it.
7. `systemctl daemon-reload && systemctl enable --now relaydeck`, then
   check `/health` and `/ready`.

## API summary (hosted)

- `POST /api/v1/auth/signup` - `{email, password, name}`. Creates a
  user, a workspace and a session cookie. Returns the workspace,
  including its `public_id` for the ingest URL.
- `POST /api/v1/auth/login`, `POST /api/v1/auth/logout`,
  `GET /api/v1/auth/me`.
- `GET /api/v1/workspaces`, `POST /api/v1/workspaces`,
  `GET /api/v1/workspaces/{id}`.
- `POST /api/v1/workspaces/{id}/api-keys` - returns the plain key once.
  `GET .../api-keys`, `POST .../api-keys/{keyID}/revoke`.
- `GET /api/v1/workspaces/{id}/usage`,
  `POST /api/v1/workspaces/{id}/prune`.
- `POST /ingest/{workspace_public_id}/{source}` - webhook ingestion.
- `POST /api/v1/billing/checkout` - `{workspace_id, plan}`. Returns
  `checkout_url`. Requires Stripe to be configured.
- `POST /api/v1/billing/stripe-webhook` - Stripe webhook, signature
  verified with `STRIPE_WEBHOOK_SECRET`.
- Management API (`/api/v1/destinations`, `/api/v1/events`,
  `/api/v1/deliveries`, `/api/v1/stats`, `/api/v1/sources`) - send a
  workspace API key in `X-API-Key`, or a session cookie plus
  `X-Workspace-Id` (or `?workspace_id=`).

## Verification status

- `go vet`, `go test ./...`, `go test -race ./...` and a live hosted
  smoke test were run on this machine during Phase 1. See the Phase 1
  report for the exact output.
- Not verified live: Stripe checkout with real or test keys, a VPS
  deploy, TLS termination, Docker build, and load beyond a single
  workspace smoke test. Do not present those as done.

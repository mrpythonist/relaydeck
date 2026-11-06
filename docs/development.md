# RelayDeck development guide

## Setup

Prerequisites: Go 1.23 or newer, Git. Optional: Docker and Docker Compose
for verifying the container build.

```bash
git clone https://github.com/mrpythonist/relaydeck.git
cd relaydeck
go build -o relaydeck ./cmd/relaydeck
./relaydeck serve
```

The dashboard is at `http://localhost:8080`. No configuration is needed for
local use. To seed sources and destinations on startup, copy
`examples/relaydeck.yaml` to `relaydeck.yaml` and edit it, then run
`./relaydeck serve --config relaydeck.yaml`.

## Make targets

The Makefile wraps the common commands:

| Target | Command |
|---|---|
| `make build` | `go build -o relaydeck ./cmd/relaydeck` |
| `make test` | `go test ./...` |
| `make test-race` | `go test -race ./...` |
| `make vet` | `go vet ./...` |
| `make fmt` | `gofmt -l cmd internal tests web` |
| `make run` | `go run ./cmd/relaydeck serve` |
| `make clean` | `rm -f relaydeck` |

Run `make test`, `make test-race`, and `make vet` before opening a PR.
`gofmt -l` must print nothing.

## Tests

The suite has 46 test functions across unit tests and one end-to-end test.

- **Unit tests** live next to the code they cover (`internal/*/*_test.go`).
  They cover ingestion, idempotency, HMAC verification, SSRF validation,
  routing, backoff math, lease claiming, retry and replay, API-key auth,
  and metrics.
- **Integration test** (`tests/integration_test.go`) starts a real server
  and a real receiver and checks the full path from ingestion to delivery.

Run the full suite:

```bash
go test ./...
go test -race ./...   # race detector
```

### Live smoke test

To verify a change end to end against a real receiver, run the server and
the curl walkthrough:

```bash
go run ./cmd/relaydeck serve &
sh examples/curl-examples.sh
```

The script checks health, readiness, metrics, creates a destination, sends
Stripe, GitHub (with an `Idempotency-Key`), and Shopify payloads from
`examples/payloads/`, then lists events and stats. Point the destination at
a request catcher first if you want to see the deliveries arrive.

## Project layout

```
cmd/relaydeck/            CLI entrypoint
internal/api/             HTTP server, ingestion, management API, dashboard
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
tests/                    end-to-end test
deploy/                   systemd unit
migrations/               initial schema (also embedded in the database package)
```

Read [ARCHITECTURE.md](ARCHITECTURE.md) before changing ingestion,
delivery, or storage, and [DECISIONS.md](DECISIONS.md) before changing a
design decision recorded there.

## Database

- SQLite in WAL mode via `modernc.org/sqlite` (pure Go, no CGO).
- The schema is applied on open from the `schema` constant in
  `internal/database/database.go`, which mirrors `migrations/001_initial.sql`.
  Change both when you change the schema.
- Tables: `sources`, `destinations`, `events`, `deliveries`, `attempts`.
- Writes are serialised with `SetMaxOpenConns(1)`. Do not raise this without
  understanding the lease-claiming transaction in `ClaimDue`.
- The database file is `<data_dir>/relaydeck.db`. Never commit `*.db`,
  `*.db-wal`, or `*.db-shm` files; they are gitignored.
- `DeleteOldEvents` exists in the database package but nothing calls it yet.
  Retention pruning is a planned v0.2.0 feature, not a live one. Do not
  document it as working.

## Adding a feature

1. Open an issue first for anything larger than a small fix.
2. Add or update tests in the same package as the change.
3. Update the README or `docs/` when behaviour, config, the API, or the CLI
   changes.
4. Add a CHANGELOG entry under `[Unreleased]`.
5. Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and `gofmt`.
6. Follow the PR process in [CONTRIBUTING.md](../CONTRIBUTING.md).

## Release process

Releases are cut from `main`:

1. Update `CHANGELOG.md`: move the `[Unreleased]` entries under a new
   version section with the release date.
2. Tag the release: `git tag v0.1.1 && git push origin v0.1.1`.
3. The release workflow (`.github/workflows/release.yml`) builds the binary
   for Linux and macOS on amd64 and arm64 with the version embedded via
   `-ldflags "-X main.Version=<version>"` and publishes a GitHub release.

Note: the workflows are prepared locally and need to be uploaded through
the GitHub web UI before they run. Until then, releases are built manually
with `go build -trimpath -ldflags "-s -w -X main.Version=<version>" -o
relaydeck ./cmd/relaydeck`.

## Docker

The Dockerfile is a multi-stage build: `golang:1.23-alpine` compiles a
static binary (`CGO_ENABLED=0`), and the final image is `scratch` with the
binary and CA certificates. It exposes port 8080 and persists `/data` on a
volume. The Docker build has not been run and verified in this environment;
if you verify it, update the README note and the CHANGELOG.

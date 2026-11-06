# RelayDeck CLI reference

The binary is named `relaydeck`. All commands below are implemented in
`cmd/relaydeck/main.go`. The CLI talks to a running server over HTTP for the
`events`, `destinations`, and `health` commands, so the server must be
running for those to work.

## Commands

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

`relaydeck help`, `relaydeck --help`, and `relaydeck -h` print usage.
`relaydeck --version` and `relaydeck -version` behave like `version`.
An unknown command prints usage and exits with code 2. A runtime error
exits with code 1.

## `serve`

Starts the HTTP gateway and the delivery worker pool.

```bash
relaydeck serve
relaydeck serve --config relaydeck.yaml --listen :8080 --data-dir ./data
```

- `--config FILE`: YAML config file. Also read from the `RELAYDECK_CONFIG`
  environment variable when the flag is not given.
- `--listen ADDR`: listen address, for example `:8080`. Overrides config
  and environment.
- `--data-dir DIR`: directory for the SQLite database file
  (`relaydeck.db`). Overrides config and environment.

On startup the server creates the data directory, opens (creating) the
database, seeds sources and destinations from the config file, and starts
the workers. Destinations whose name already exists are skipped, not
overwritten. Shutdown on SIGINT or SIGTERM is graceful: in-flight sends
drain, then the HTTP server stops (15 s shutdown timeout). Logs go to
stdout as structured JSON.

## `version`

```bash
relaydeck version
# relaydeck 0.1.0
```

Prints the version embedded at build time. A plain `go build` produces
`0.1.0-dev`; release builds set the real version with
`-ldflags "-X main.Version=<version>"`.

## `config validate`

```bash
relaydeck config validate --config relaydeck.yaml
# config valid: listen=:8080 data_dir=./data workers=4 max_payload_bytes=1048576 sources=3 destinations=3
```

Loads the config (defaults, then YAML, then environment) and validates it.
Exits with an error when the file cannot be read or a value is out of range
(`max_payload_bytes` 1 to 64 MiB, `workers` 1 to 64, `retention_days` 0 or
more, non-empty listen address and data directory, named sources and
destinations).

## `events list`

```bash
relaydeck events list
relaydeck events list --base-url http://localhost:8080
```

Prints the newest 50 events as indented JSON, from
`GET /api/v1/events?limit=50`.

## `events inspect <id>`

```bash
relaydeck events inspect evt_abc123
```

Prints the event with its payload, headers, deliveries, and attempt
history as indented JSON, from `GET /api/v1/events/<id>`.

## `events replay <id>`

```bash
relaydeck events replay evt_abc123
```

Re-queues the event to all currently matching destinations
(`POST /api/v1/events/<id>/replay`) and prints the result, including
`deliveries_created`.

## `destinations list`

```bash
relaydeck destinations list
```

Prints all destinations as indented JSON, from
`GET /api/v1/destinations`. There is no CLI create, update, or delete for
destinations yet; use the API or the dashboard. CLI destination management
is planned for v0.2.0.

## `health`

```bash
relaydeck health
# health: HTTP 200 {"status":"ok"}
```

Calls `GET /health` and prints the status. Exits with an error when the
server is not healthy (non-200 response).

## Common flags

- `--base-url URL`: base URL of the running server, default
  `http://localhost:8080`. It may appear before or after the positional
  argument, and may be written as `--base-url URL` or `--base-url=URL`.
- When `RELAYDECK_API_KEY` is set in the environment, the CLI sends it as
  `X-API-Key` on its API calls.

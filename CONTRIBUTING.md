# Contributing to RelayDeck

Thanks for helping make RelayDeck better. This guide covers setup, workflow,
and the standards a pull request is judged by.

## Getting started

### Prerequisites

- Go 1.23 or newer. Check with `go version`.
- Git.
- Optional: Docker and Docker Compose, if you want to verify the container
  build.

### Setup

1. Fork the repository and clone your fork.
2. Build the binary:

   ```bash
   go build -o relaydeck ./cmd/relaydeck
   ```

3. Run the tests:

   ```bash
   go test ./...
   ```

4. Start the server for local use:

   ```bash
   go run ./cmd/relaydeck serve
   ```

   The dashboard is at `http://localhost:8080`. No configuration is needed
   for local use. Copy `examples/relaydeck.yaml` to `relaydeck.yaml` and edit
   it if you want sources and destinations seeded on startup.

## Branch and commit conventions

- Work on a branch off `main`. Name it after the change:
  `fix/retry-after-dead`, `feat/json-path-filter`, `docs/api-reference`.
- Keep commits small and focused. One logical change per commit.
- Write commit messages in the imperative mood, with a short summary line
  and a body when the change needs explanation:

  ```
  Fix stale lease recovery on restart

  RequeueStaleLeases was only called on the poll tick, so a restart
  left in-progress deliveries claimed until the next tick. Call it
  on startup as well.
  ```

- Do not commit secrets, real config files, or database files. `.env`,
  `*.db`, and `relaydeck.local.yaml` are gitignored. Check `git status`
  before committing.
- Do not commit generated files or build output. The `relaydeck` binary is
  gitignored.

## Tests

Every change to behaviour needs a test, or an explanation in the PR of why
one is not possible. The areas with the strictest coverage are ingestion,
persistence, retries, SSRF validation, and auth. New behaviour in those
areas must come with tests.

Run before opening a PR:

```bash
go test ./...        # unit + integration tests
go test -race ./...  # race detector
go vet ./...
gofmt -l cmd internal tests web   # must print nothing
```

`tests/integration_test.go` runs a real server against a real receiver. If
you change an API response, a CLI command, or a config option, update the
matching test and the docs in the same PR.

## Code standards

- **Working software over placeholders.** Do not submit TODO stubs for core
  behaviour. If something is not implemented, say so in the PR.
- **Keep dependencies minimal.** The binary should stay small and build with
  pure Go where possible. Justify any new dependency in the PR description.
- **Idiomatic Go.** Run `gofmt` and `go vet` before pushing.
- **No fake claims.** Do not claim exactly-once delivery, zero loss, or
  performance numbers you have not measured. Delivery is at-least-once by
  design; see `docs/DECISIONS.md` (ADR-004).
- **Security first.** Outbound HTTP is SSRF-sensitive. Any change to
  delivery, URL validation, or redirect handling needs a security test.
- **Docs with features.** Update the README or `docs/` when behaviour
  changes, and add a CHANGELOG entry under `[Unreleased]`.

## Pull request process

1. Open an issue first for anything larger than a small fix, so the design
   can be agreed before you write code. Small fixes and docs improvements
   can go straight to a PR.
2. Keep PRs focused: one feature or fix per PR.
3. Fill in the PR template: what changed, why, and how you tested it
   (include test output).
4. Update `CHANGELOG.md` under `[Unreleased]`.
5. A maintainer reviews for correctness, tests, docs, and honest claims.
   Expect review comments on delivery semantics and security; that is the
   normal bar for this project, not a rejection.
6. Once approved, the PR is merged into `main`. Releases are cut from `main`
   by tagging; see `docs/development.md`.

## Good first issues

Look for the `good first issue` label. Dashboard polish, extra sample
payloads, CLI ergonomics, and documentation improvements are good starters.

## Reporting bugs

Use the bug report template. Include the RelayDeck version
(`relaydeck version`), how you run it (binary, Docker, or source), your OS
and architecture, steps to reproduce, and log lines with secrets redacted.
Security problems must never be reported in a public issue; see
[SECURITY.md](SECURITY.md).

## Code of Conduct

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

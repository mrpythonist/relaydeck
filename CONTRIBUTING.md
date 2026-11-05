# Contributing to RelayDeck

Thanks for helping make RelayDeck better.

## Getting started

1. Fork the repository and clone your fork.
2. Go 1.23+ is required: `go version`.
3. Run the tests: `go test ./...`, `go test -race ./...`, `go vet ./...`.
4. Start the server: `go run ./cmd/relaydeck serve`.

## Guidelines

- **Working software over placeholders.** Do not submit TODO stubs for core
  behaviour. If something is not implemented, say so in the PR.
- **Tests for every critical path.** Ingestion, persistence, retries, SSRF and
  auth all have tests — new behaviour in those areas needs tests too.
- **Keep dependencies minimal.** The binary should stay small and build with
  pure Go where possible. Justify any new dependency in the PR description.
- **No fake claims.** Do not claim exactly-once delivery, zero loss, or
  performance numbers you have not measured.
- **Security first.** Outbound HTTP is SSRF-sensitive: any change to delivery,
  URL validation or redirect handling needs a security test.
- **Idiomatic Go.** Run `gofmt` and `go vet` before pushing.
- **Docs with features.** Update README/docs when behaviour changes.

## Pull requests

- Keep PRs focused; one feature or fix per PR.
- Describe what changed, why, and how you tested it (include test output).
- Update CHANGELOG.md under `[Unreleased]`.

## Good first issues

Look for the `good first issue` label: dashboard polish, extra sample
payloads, CLI ergonomics, and documentation improvements are good starters.

## Code of Conduct

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

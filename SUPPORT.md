# Support

## Where to ask

- **Setup and usage questions:** GitHub Discussions, Q&A category. Search
  existing discussions first; the answer may already be there.
- **Bugs:** open an issue with the bug report template. Include the
  RelayDeck version (`relaydeck version`), how you run it (binary, Docker,
  or source), your OS and architecture, steps to reproduce, and log lines
  with secrets redacted.
- **Feature requests:** open an issue with the feature request template, or
  start a discussion in the Ideas category first for larger proposals.
- **Security problems:** do not open a public issue. See [SECURITY.md](SECURITY.md).

## Before asking

1. Read the README, especially the Configuration and Ingestion details
   sections. Most setup questions are answered there.
2. Run `relaydeck config validate --config relaydeck.yaml` to check your
   config.
3. Check `relaydeck health` and `GET /ready` to confirm the server and
   database are up.
4. Check the dashboard at `http://localhost:8080`, especially the Failed
   tab, which shows the last error for each dead delivery.

## Common problems

- **Destination never receives events:** check that the destination is
  enabled, that its `source_filter` matches the event source, and that its
  payload-substring filter (if set) appears in the payload. Then check the
  delivery attempts in the dashboard for the last error.
- **401 on ingest:** the source has a secret configured, so the request
  needs a valid `X-RelayDeck-Signature` or `X-Hub-Signature-256` header
  (HMAC-SHA256 of the raw body, keyed with the source secret).
- **413 on ingest:** the payload exceeds `max_payload_bytes` (default 1 MiB).
  Raise the limit in config, within the 64 MiB maximum.
- **Destination URL rejected:** SSRF protection blocks loopback, private,
  and link-local addresses. For local development only, set
  `allow_private_destinations: true` or list the host under
  `trusted_internal_destinations`. Never enable this in production unless
  you fully trust every destination you configure.
- **401 on the management API:** when `RELAYDECK_API_KEY` is set, send it as
  `X-API-Key` or `Authorization: Bearer <key>`.
- **Events pile up as pending:** check the server logs for worker errors
  and confirm the destination is reachable from the machine running
  RelayDeck.

## What support covers

This is a community project maintained in spare time. Maintainers answer
questions and fix confirmed bugs, but there is no guaranteed response time
and no paid support or SLA. Well-written bug reports with reproduction
steps get answered fastest.

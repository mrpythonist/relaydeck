# Screenshots

No screenshots are published yet. The dashboard is embedded in the binary,
and no verified run has been captured for this repository so far.

## How to capture one

1. Build and run RelayDeck from source:

   ```bash
   go build -o relaydeck ./cmd/relaydeck
   ./relaydeck serve
   ```

2. Seed some data so the dashboard is not empty. Run
   `sh examples/curl-examples.sh` (it creates a destination and sends
   Stripe, GitHub, and Shopify payloads), or send a few webhooks of your
   own.
3. Open `http://localhost:8080` in a browser. Capture the Overview tab and
   one of the Events or Failed tabs at a normal desktop width.
4. Save the files here as PNG, for example `dashboard-overview.png` and
   `dashboard-events.png`, and reference them from the README Screenshots
   section.

Rules: only genuine captures from a real run of this project. No mockups,
no screenshots of other projects, no edited or staged images that show
data RelayDeck did not produce. Redact any real secrets, API keys, or
personal data before publishing a capture.

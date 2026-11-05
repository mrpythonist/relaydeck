#!/bin/sh
# RelayDeck curl examples. Run the server first: relaydeck serve
set -e
BASE="${BASE:-http://localhost:8080}"

echo "--- health / ready / metrics ---"
curl -s "$BASE/health"; echo
curl -s "$BASE/ready"; echo
curl -s "$BASE/metrics" | head -20

echo "--- create a destination ---"
curl -s -X POST "$BASE/api/v1/destinations" -H 'Content-Type: application/json' \
  -d '{"name":"laravel-api","url":"https://api.example.com/webhooks/stripe","source_filter":"stripe"}'; echo

echo "--- Stripe webhook ---"
curl -s -X POST "$BASE/api/v1/webhooks/stripe" -H 'Content-Type: application/json' \
  --data @examples/payloads/stripe-payment-succeeded.json; echo

echo "--- GitHub webhook (idempotent) ---"
curl -s -X POST "$BASE/api/v1/webhooks/github" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: github-push-1' \
  --data @examples/payloads/github-push.json; echo

echo "--- Shopify webhook ---"
curl -s -X POST "$BASE/api/v1/webhooks/shopify" -H 'Content-Type: application/json' \
  --data @examples/payloads/shopify-order-created.json; echo

echo "--- list events / stats ---"
curl -s "$BASE/api/v1/events?limit=5"; echo
curl -s "$BASE/api/v1/stats"; echo

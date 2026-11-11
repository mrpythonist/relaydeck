package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrpythonist/relaydeck/internal/config"
	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/metrics"
	"github.com/mrpythonist/relaydeck/internal/security"
)

func testServer(t *testing.T, apiKey string) (*Server, *database.Store) {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.APIKey = apiKey
	cfg.AllowPrivate = true
	cfg.DataDir = t.TempDir()
	srv := New(store, cfg, metrics.New())
	return srv, store
}

func doReq(t *testing.T, srv *Server, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHealthReadyMetrics(t *testing.T) {
	srv, _ := testServer(t, "")
	if rec := doReq(t, srv, "GET", "/health", nil, nil); rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
	if rec := doReq(t, srv, "GET", "/ready", nil, nil); rec.Code != 200 {
		t.Fatalf("ready: %d", rec.Code)
	}
	rec := doReq(t, srv, "GET", "/metrics", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "relaydeck_events_received_total") {
		t.Fatalf("metrics: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, srv, "GET", "/", nil, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "RelayDeck") {
		t.Fatalf("dashboard: %d", rec.Code)
	}
}

func TestIngestAcceptsAndPersists(t *testing.T) {
	srv, store := testServer(t, "")
	rec := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", []byte(`{"type":"payment_intent.succeeded"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	id, _ := out["event_id"].(string)
	if id == "" {
		t.Fatal("no event_id returned")
	}
	ev, _ := store.GetEvent(id)
	if ev == nil || string(ev.Payload) != `{"type":"payment_intent.succeeded"}` {
		t.Fatalf("event not persisted: %+v", ev)
	}
	st, _ := store.Stats()
	if st.TotalEvents != 1 {
		t.Fatalf("stats total events = %d", st.TotalEvents)
	}
}

func TestIngestPayloadLimit(t *testing.T) {
	srv, _ := testServer(t, "")
	srv.Config.MaxPayloadBytes = 10
	rec := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", []byte(`{"type":"way-too-long-payload"}`), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestIngestHMAC(t *testing.T) {
	srv, store := testServer(t, "")
	_ = store.UpsertSource("stripe", "whsec_test")
	body := []byte(`{"type":"x"}`)
	// No signature -> 401.
	if rec := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned should be 401, got %d", rec.Code)
	}
	// Wrong signature -> 401.
	if rec := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", body, map[string]string{"X-Hub-Signature-256": "sha256=" + security.HMACSHA256Hex("wrong", body)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong sig should be 401, got %d", rec.Code)
	}
	// Correct signature -> 202.
	sig := "sha256=" + security.HMACSHA256Hex("whsec_test", body)
	if rec := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", body, map[string]string{"X-Hub-Signature-256": sig}); rec.Code != http.StatusAccepted {
		t.Fatalf("valid sig should be 202, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestIngestIdempotencyViaAPI(t *testing.T) {
	srv, store := testServer(t, "")
	h := map[string]string{"Idempotency-Key": "idem-1"}
	r1 := doReq(t, srv, "POST", "/api/v1/webhooks/github", []byte(`{}`), h)
	r2 := doReq(t, srv, "POST", "/api/v1/webhooks/github", []byte(`{}`), h)
	if r1.Code != 202 || r2.Code != 202 {
		t.Fatalf("codes %d %d", r1.Code, r2.Code)
	}
	var o1, o2 map[string]any
	_ = json.Unmarshal(r1.Body.Bytes(), &o1)
	_ = json.Unmarshal(r2.Body.Bytes(), &o2)
	if o1["event_id"] != o2["event_id"] || o2["duplicate"] != true {
		t.Fatalf("idempotency mismatch: %v vs %v", o1, o2)
	}
	st, _ := store.Stats()
	if st.TotalEvents != 1 {
		t.Fatalf("expected 1 event, got %d", st.TotalEvents)
	}
}

func TestManagementAPIAuth(t *testing.T) {
	srv, _ := testServer(t, "secret-key")
	if rec := doReq(t, srv, "GET", "/api/v1/destinations", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key should be 401, got %d", rec.Code)
	}
	if rec := doReq(t, srv, "GET", "/api/v1/destinations", nil, map[string]string{"X-API-Key": "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key should be 401, got %d", rec.Code)
	}
	if rec := doReq(t, srv, "GET", "/api/v1/destinations", nil, map[string]string{"X-API-Key": "secret-key"}); rec.Code != 200 {
		t.Fatalf("valid key should be 200, got %d", rec.Code)
	}
	// Ingestion is HMAC-gated per source, not API-key gated.
	if rec := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", []byte(`{}`), nil); rec.Code != 202 {
		t.Fatalf("ingest without API key should be 202, got %d", rec.Code)
	}
}

func TestDestinationValidationRejectsPrivate(t *testing.T) {
	srv, _ := testServer(t, "")
	srv.Config.AllowPrivate = false
	body := []byte(`{"name":"bad","url":"http://127.0.0.1:9999/hook"}`)
	if rec := doReq(t, srv, "POST", "/api/v1/destinations", body, map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("private destination should be 400, got %d %s", rec.Code, rec.Body.String())
	}
	body = []byte(`{"name":"ok","url":"https://example.com/hook"}`)
	if rec := doReq(t, srv, "POST", "/api/v1/destinations", body, map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusCreated {
		t.Fatalf("public destination should be 201, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestDestinationV021Fields checks the v0.2.1 destination fields through
// the API: the signing secret is stored but never returned in clear, the
// JSON-path filter and header templates round-trip, and the clear query
// params wipe a field.
func TestDestinationV021Fields(t *testing.T) {
	srv, store := testServer(t, "")
	body := []byte(`{"name":"api","url":"https://example.com/hook","filter_jsonpath":"type=payment_intent.succeeded","signing_secret":"dst-secret","header_templates":{"X-Event-Type":"{{.type}}"}}`)
	rec := doReq(t, srv, "POST", "/api/v1/destinations", body, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created["signing_secret"] == "dst-secret" {
		t.Fatal("API returned the signing secret in clear")
	}
	if created["signing_secret"] != "***REDACTED***" {
		t.Fatalf("signing secret should be masked, got %v", created["signing_secret"])
	}
	if created["filter_jsonpath"] != "type=payment_intent.succeeded" {
		t.Fatalf("JSON-path filter not returned: %v", created["filter_jsonpath"])
	}
	tpl, _ := created["header_templates"].(map[string]any)
	if tpl["X-Event-Type"] != "{{.type}}" {
		t.Fatalf("header templates not returned: %v", created["header_templates"])
	}
	// The secret really is stored: read the row directly.
	d, _ := store.GetDestinationByName("api")
	if d == nil || d.SigningSecret != "dst-secret" {
		t.Fatalf("secret not stored: %+v", d)
	}
	id := created["id"].(string)
	// Update the JSON-path filter; the masked secret must stay unchanged.
	up := doReq(t, srv, "PUT", "/api/v1/destinations/"+id, []byte(`{"filter_jsonpath":"type=charge.failed"}`), map[string]string{"Content-Type": "application/json"})
	if up.Code != http.StatusOK {
		t.Fatalf("update: %d %s", up.Code, up.Body.String())
	}
	d, _ = store.GetDestinationByName("api")
	if d.JSONPathFilter != "type=charge.failed" || d.SigningSecret != "dst-secret" {
		t.Fatalf("update wrong: %+v", d)
	}
	// Clear the JSON-path filter and the signing secret.
	clr := doReq(t, srv, "PUT", "/api/v1/destinations/"+id+"?clear_jsonpath=1&clear_signing_secret=1", []byte(`{}`), map[string]string{"Content-Type": "application/json"})
	if clr.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", clr.Code, clr.Body.String())
	}
	d, _ = store.GetDestinationByName("api")
	if d.JSONPathFilter != "" || d.SigningSecret != "" {
		t.Fatalf("clear did not wipe fields: %+v", d)
	}
}

func TestReplayAndRetryEndpoints(t *testing.T) {
	srv, store := testServer(t, "")
	// Destination with a dead endpoint is not needed: replay works with zero dests too,
	// but exercise the full path with a real destination.
	rec := doReq(t, srv, "POST", "/api/v1/destinations", []byte(`{"name":"api","url":"https://example.com/hook"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 201 {
		t.Fatalf("create dest: %d %s", rec.Code, rec.Body.String())
	}
	ing := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", []byte(`{"type":"x"}`), nil)
	var out map[string]any
	_ = json.Unmarshal(ing.Body.Bytes(), &out)
	id := out["event_id"].(string)
	rep := doReq(t, srv, "POST", "/api/v1/events/"+id+"/replay", nil, nil)
	if rep.Code != 200 {
		t.Fatalf("replay: %d %s", rep.Code, rep.Body.String())
	}
	dels, _ := store.ListDeliveries(10, "", "")
	if len(dels) != 2 { // 1 original + 1 replay
		t.Fatalf("expected 2 deliveries after replay, got %d", len(dels))
	}
	// Retry endpoint on a pending delivery is accepted (idempotent requeue).
	rt := doReq(t, srv, "POST", "/api/v1/deliveries/"+dels[0].ID+"/retry", nil, nil)
	if rt.Code != 200 {
		t.Fatalf("retry: %d %s", rt.Code, rt.Body.String())
	}
	// Unknown event -> 404.
	if rec := doReq(t, srv, "GET", "/api/v1/events/evt_doesnotexist", nil, nil); rec.Code != 404 {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestSecretHeaderRedactionInStoredEvent(t *testing.T) {
	srv, store := testServer(t, "")
	rec := doReq(t, srv, "POST", "/api/v1/webhooks/stripe", []byte(`{}`), map[string]string{"Authorization": "Bearer super-secret"})
	if rec.Code != 202 {
		t.Fatalf("ingest: %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	ev, _ := store.GetEvent(out["event_id"].(string))
	if ev.Headers["Authorization"] != "***REDACTED***" {
		t.Fatalf("authorization header stored in clear: %v", ev.Headers)
	}
}

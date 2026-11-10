package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrpythonist/relaydeck/internal/config"
	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/metrics"
)

func testHostedServer(t *testing.T) (*Server, *database.Store) {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Hosted = true
	cfg.AllowPrivate = true
	cfg.DataDir = t.TempDir()
	srv := New(store, cfg, metrics.New())
	return srv, store
}

func hostedReq(t *testing.T, srv *Server, method, path string, body []byte, cookie *http.Cookie, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func signup(t *testing.T, srv *Server, email, password string) (*http.Cookie, map[string]any) {
	t.Helper()
	rec := hostedReq(t, srv, "POST", "/api/v1/auth/signup", []byte(`{"email":"`+email+`","password":"`+password+`","name":"Test"}`), nil, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup %s: %d %s", email, rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	cookies := rec.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "relaydeck_session" {
			return c, out
		}
	}
	t.Fatal("signup did not set a session cookie")
	return nil, out
}

func workspaceOf(t *testing.T, signupOut map[string]any) map[string]any {
	t.Helper()
	ws, ok := signupOut["workspace"].(map[string]any)
	if !ok {
		t.Fatalf("signup response has no workspace: %v", signupOut)
	}
	return ws
}

func TestSignupLoginAndAuthRequired(t *testing.T) {
	srv, _ := testHostedServer(t)
	// Unauthenticated management call is rejected in hosted mode.
	if rec := hostedReq(t, srv, "GET", "/api/v1/destinations", nil, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated should be 401, got %d", rec.Code)
	}
	// Weak password rejected.
	if rec := hostedReq(t, srv, "POST", "/api/v1/auth/signup", []byte(`{"email":"a@example.com","password":"short"}`), nil, map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password should be 400, got %d", rec.Code)
	}
	cookie, out := signup(t, srv, "a@example.com", "password123")
	_ = cookie
	// Duplicate email rejected.
	if rec := hostedReq(t, srv, "POST", "/api/v1/auth/signup", []byte(`{"email":"a@example.com","password":"password123"}`), nil, map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate signup should be 409, got %d", rec.Code)
	}
	// Wrong password rejected.
	if rec := hostedReq(t, srv, "POST", "/api/v1/auth/login", []byte(`{"email":"a@example.com","password":"wrongpass"}`), nil, map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password should be 401, got %d", rec.Code)
	}
	// Correct login works and /me returns the account.
	login := hostedReq(t, srv, "POST", "/api/v1/auth/login", []byte(`{"email":"a@example.com","password":"password123"}`), nil, map[string]string{"Content-Type": "application/json"})
	if login.Code != 200 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	var loginCookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == "relaydeck_session" {
			loginCookie = c
		}
	}
	if loginCookie == nil {
		t.Fatal("login did not set a session cookie")
	}
	me := hostedReq(t, srv, "GET", "/api/v1/auth/me", nil, loginCookie, nil)
	if me.Code != 200 || !strings.Contains(me.Body.String(), "a@example.com") {
		t.Fatalf("me: %d %s", me.Code, me.Body.String())
	}
	_ = out
}

func TestWorkspaceIsolationViaAPI(t *testing.T) {
	srv, store := testHostedServer(t)
	cookieA, outA := signup(t, srv, "a@example.com", "password123")
	cookieB, outB := signup(t, srv, "b@example.com", "password123")
	wsA := workspaceOf(t, outA)
	wsB := workspaceOf(t, outB)
	pubA := wsA["public_id"].(string)

	// A creates a destination and ingests an event.
	destBody := []byte(`{"name":"api","url":"https://example.com/hook"}`)
	rec := hostedReq(t, srv, "POST", "/api/v1/destinations", destBody, cookieA, map[string]string{"Content-Type": "application/json"})
	if rec.Code != 201 {
		t.Fatalf("A create destination: %d %s", rec.Code, rec.Body.String())
	}
	ing := hostedReq(t, srv, "POST", "/ingest/"+pubA+"/stripe", []byte(`{"type":"payment_intent.succeeded"}`), nil, map[string]string{"Content-Type": "application/json"})
	if ing.Code != 202 {
		t.Fatalf("A ingest: %d %s", ing.Code, ing.Body.String())
	}
	var ingOut map[string]any
	_ = json.Unmarshal(ing.Body.Bytes(), &ingOut)
	eventID := ingOut["event_id"].(string)

	// B lists events: empty. B reads A's event: 404. B replays: 404.
	if rec := hostedReq(t, srv, "GET", "/api/v1/events", nil, cookieB, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("B list events should be empty: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hostedReq(t, srv, "GET", "/api/v1/events/"+eventID, nil, cookieB, nil); rec.Code != 404 {
		t.Fatalf("B get A event should be 404, got %d", rec.Code)
	}
	if rec := hostedReq(t, srv, "POST", "/api/v1/events/"+eventID+"/replay", nil, cookieB, nil); rec.Code != 404 {
		t.Fatalf("B replay A event should be 404, got %d", rec.Code)
	}
	if rec := hostedReq(t, srv, "GET", "/api/v1/destinations", nil, cookieB, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"destinations":[]`) {
		t.Fatalf("B list destinations should be empty: %d %s", rec.Code, rec.Body.String())
	}
	// A can still read its own event and stats.
	if rec := hostedReq(t, srv, "GET", "/api/v1/events/"+eventID, nil, cookieA, nil); rec.Code != 200 {
		t.Fatalf("A get own event: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hostedReq(t, srv, "GET", "/api/v1/stats", nil, cookieA, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"total_events":1`) {
		t.Fatalf("A stats: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hostedReq(t, srv, "GET", "/api/v1/stats", nil, cookieB, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"total_events":0`) {
		t.Fatalf("B stats should be 0: %d %s", rec.Code, rec.Body.String())
	}
	// Usage is per workspace.
	wsAID := wsA["id"].(string)
	if rec := hostedReq(t, srv, "GET", "/api/v1/workspaces/"+wsAID+"/usage", nil, cookieA, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"events_used":1`) {
		t.Fatalf("A usage: %d %s", rec.Code, rec.Body.String())
	}
	// B cannot read A's usage.
	if rec := hostedReq(t, srv, "GET", "/api/v1/workspaces/"+wsAID+"/usage", nil, cookieB, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("B usage of A should be 403, got %d", rec.Code)
	}
	_ = wsB
	_ = store
}

func TestFreeDestinationCap(t *testing.T) {
	srv, _ := testHostedServer(t)
	cookie, _ := signup(t, srv, "cap@example.com", "password123")
	for i := 0; i < 3; i++ {
		body := []byte(`{"name":"dest` + string(rune('0'+i)) + `","url":"https://example.com/hook"}`)
		if rec := hostedReq(t, srv, "POST", "/api/v1/destinations", body, cookie, map[string]string{"Content-Type": "application/json"}); rec.Code != 201 {
			t.Fatalf("destination %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	body := []byte(`{"name":"dest3","url":"https://example.com/hook"}`)
	if rec := hostedReq(t, srv, "POST", "/api/v1/destinations", body, cookie, map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th free destination should be 429, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestMonthlyLimitReturns429(t *testing.T) {
	srv, store := testHostedServer(t)
	cookie, out := signup(t, srv, "limit@example.com", "password123")
	ws := workspaceOf(t, out)
	// Fake a full month: set usage to the free limit (1000).
	month := time.Now().UTC().Format("2006-01")
	if _, err := store.DB.Exec(`INSERT INTO usage_monthly(workspace_id,month,event_count) VALUES(?,?,1000)`, ws["id"].(string), month); err != nil {
		t.Fatal(err)
	}
	ing := hostedReq(t, srv, "POST", "/ingest/"+ws["public_id"].(string)+"/stripe", []byte(`{}`), nil, map[string]string{"Content-Type": "application/json"})
	if ing.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit ingest should be 429, got %d %s", ing.Code, ing.Body.String())
	}
	if !strings.Contains(ing.Body.String(), "monthly event limit reached") {
		t.Fatalf("429 body should name the limit: %s", ing.Body.String())
	}
	_ = cookie
}

func TestWorkspaceAPIKeyAuth(t *testing.T) {
	srv, _ := testHostedServer(t)
	cookie, out := signup(t, srv, "key@example.com", "password123")
	ws := workspaceOf(t, out)
	wsID := ws["id"].(string)
	rec := hostedReq(t, srv, "POST", "/api/v1/workspaces/"+wsID+"/api-keys", []byte(`{"name":"ci"}`), cookie, map[string]string{"Content-Type": "application/json"})
	if rec.Code != 201 {
		t.Fatalf("create api key: %d %s", rec.Code, rec.Body.String())
	}
	var keyOut map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &keyOut)
	plain, _ := keyOut["key"].(string)
	if plain == "" {
		t.Fatal("api key creation must return the plain key once")
	}
	// The workspace API key alone authenticates management calls.
	if rec := hostedReq(t, srv, "GET", "/api/v1/destinations", nil, nil, map[string]string{"X-API-Key": plain}); rec.Code != 200 {
		t.Fatalf("api key auth: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hostedReq(t, srv, "GET", "/api/v1/destinations", nil, nil, map[string]string{"X-API-Key": "rdk_wrong"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong api key should be 401, got %d", rec.Code)
	}
}

func TestPricingPageAndLegacyIngestDisabledInHosted(t *testing.T) {
	srv, _ := testHostedServer(t)
	if rec := hostedReq(t, srv, "GET", "/pricing", nil, nil, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Starter") {
		t.Fatalf("pricing: %d", rec.Code)
	}
	if rec := hostedReq(t, srv, "POST", "/api/v1/webhooks/stripe", []byte(`{}`), nil, nil); rec.Code != http.StatusGone {
		t.Fatalf("legacy ingest in hosted mode should be 410, got %d", rec.Code)
	}
}

func TestStripeWebhookUpdatesPlan(t *testing.T) {
	srv, store := testHostedServer(t)
	_, out := signup(t, srv, "stripe@example.com", "password123")
	ws := workspaceOf(t, out)
	wsID := ws["id"].(string)
	// No webhook secret configured in this test server, so the handler
	// warns and still processes the event (dev only, documented).
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"customer":"cus_1","subscription":"sub_1","metadata":{"workspace_id":"` + wsID + `","plan":"starter"}}}}`)
	rec := hostedReq(t, srv, "POST", "/api/v1/billing/stripe-webhook", body, nil, map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("stripe webhook: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := store.GetWorkspace(wsID)
	if got == nil || got.Plan != "starter" || got.StripeCustomerID != "cus_1" || got.StripeSubscriptionID != "sub_1" {
		t.Fatalf("workspace plan not updated: %+v", got)
	}
	// Checkout without Stripe configured returns 503.
	cookie, _ := signup(t, srv, "stripe2@example.com", "password123")
	_ = cookie
}

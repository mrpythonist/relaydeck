package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestVerifyWebhookSignature(t *testing.T) {
	secret := "whsec_test"
	payload := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "."))
	_, _ = mac.Write(payload)
	sig := fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
	if !VerifyWebhookSignature(payload, sig, secret) {
		t.Fatal("valid Stripe signature did not verify")
	}
	if VerifyWebhookSignature(payload, sig, "whsec_wrong") {
		t.Fatal("wrong secret verified")
	}
	if VerifyWebhookSignature([]byte(`{"id":"evt_2"}`), sig, secret) {
		t.Fatal("tampered payload verified")
	}
	// Old timestamp is rejected.
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	mac2 := hmac.New(sha256.New, []byte(secret))
	_, _ = mac2.Write([]byte(old + "."))
	_, _ = mac2.Write(payload)
	if VerifyWebhookSignature(payload, fmt.Sprintf("t=%s,v1=%s", old, hex.EncodeToString(mac2.Sum(nil))), secret) {
		t.Fatal("stale timestamp verified")
	}
}

func TestCreateCheckoutSessionAgainstFakeStripe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/checkout/sessions" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk_test_123" {
			t.Errorf("wrong auth %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_test_1","url":"https://checkout.stripe.com/pay/cs_test_1"}`))
	}))
	defer srv.Close()
	c := New("sk_test_123")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()
	u, err := c.CreateCheckoutSession("price_123", "ws_1", "starter", "https://example.com/ok", "https://example.com/cancel")
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://checkout.stripe.com/pay/cs_test_1" {
		t.Fatalf("wrong checkout url %s", u)
	}
	// Unconfigured client must fail clearly, not call Stripe.
	if _, err := New("").CreateCheckoutSession("price_123", "ws_1", "starter", "", ""); err == nil {
		t.Fatal("unconfigured client should fail")
	}
}

func TestPlanForEvent(t *testing.T) {
	ev, err := ParseWebhookEvent([]byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"customer":"cus_1","subscription":"sub_1","metadata":{"workspace_id":"ws_9","plan":"growth"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ws, plan, cus, sub := PlanForEvent(ev)
	if ws != "ws_9" || plan != "growth" || cus != "cus_1" || sub != "sub_1" {
		t.Fatalf("parsed %q %q %q %q", ws, plan, cus, sub)
	}
}

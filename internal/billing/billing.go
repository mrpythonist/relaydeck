// Package billing is Stripe billing code only. It makes no live charge
// and stores no card data. All Stripe IDs and secrets come from
// environment variables; there are no hard-coded price IDs in this code.
// Aqib must create the products, prices and webhook endpoint in the
// Stripe dashboard first, see docs/HOSTED.md. Without STRIPE_SECRET_KEY
// set, checkout creation returns an error and the API answers 503.
package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to the Stripe API over plain net/http. No Stripe SDK is
// used, so the hosted service adds no heavy dependency for billing.
type Client struct {
	SecretKey string
	// BaseURL is https://api.stripe.com by default; tests override it.
	BaseURL    string
	HTTPClient *http.Client
}

// New returns a Client. An empty secret key means billing is not
// configured; callers must check Configured before creating a session.
func New(secretKey string) *Client {
	return &Client{SecretKey: secretKey, BaseURL: "https://api.stripe.com", HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

// Configured reports whether a Stripe secret key is set.
func (c *Client) Configured() bool { return strings.TrimSpace(c.SecretKey) != "" }

// CreateCheckoutSession creates a Stripe Checkout session for a
// subscription to priceID. metadata must include workspace_id and plan.
// Returns the hosted checkout URL the user must be redirected to.
func (c *Client) CreateCheckoutSession(priceID, workspaceID, plan, successURL, cancelURL string) (string, error) {
	if !c.Configured() {
		return "", fmt.Errorf("stripe not configured: set STRIPE_SECRET_KEY")
	}
	if priceID == "" {
		return "", fmt.Errorf("no stripe price id for plan %q: set STRIPE_PRICE_STARTER / STRIPE_PRICE_GROWTH", plan)
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("line_items[0][price]", priceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	form.Set("metadata[workspace_id]", workspaceID)
	form.Set("metadata[plan]", plan)
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+"/v1/checkout/sessions", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.SecretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe checkout request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stripe checkout failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		URL string `json:"url"`
		ID  string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("stripe checkout response: %w", err)
	}
	if out.URL == "" {
		return "", fmt.Errorf("stripe checkout returned no url")
	}
	return out.URL, nil
}

// WebhookEvent is the subset of a Stripe event this service acts on.
type WebhookEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object map[string]any `json:"object"`
	} `json:"data"`
}

// VerifyWebhookSignature verifies a Stripe webhook signature header
// (Stripe-Signature: t=<timestamp>,v1=<hmac>) against the raw payload.
// It uses HMAC-SHA256 with the webhook secret over "<timestamp>.<payload>".
// Timestamps older than 5 minutes are rejected, as Stripe recommends.
func VerifyWebhookSignature(payload []byte, sigHeader, secret string) bool {
	if secret == "" || sigHeader == "" {
		return false
	}
	var timestamp string
	var signatures []string
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if kv[0] == "t" {
			timestamp = kv[1]
		}
		if kv[0] == "v1" {
			signatures = append(signatures, kv[1])
		}
	}
	if timestamp == "" || len(signatures) == 0 {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if abs(time.Now().Unix()-ts) > 5*60 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range signatures {
		a, errA := hex.DecodeString(sig)
		b, errB := hex.DecodeString(expected)
		if errA == nil && errB == nil && len(a) == len(b) && hmac.Equal(a, b) {
			return true
		}
	}
	return false
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ParseWebhookEvent decodes a Stripe event body.
func ParseWebhookEvent(payload []byte) (*WebhookEvent, error) {
	var ev WebhookEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// PlanForEvent extracts the plan and workspace from a checkout or
// subscription event object. It reads metadata.plan and
// metadata.workspace_id, which CreateCheckoutSession sets.
func PlanForEvent(ev *WebhookEvent) (workspaceID, plan, customerID, subscriptionID string) {
	obj := ev.Data.Object
	if obj == nil {
		return "", "", "", ""
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		workspaceID, _ = md["workspace_id"].(string)
		plan, _ = md["plan"].(string)
	}
	customerID, _ = obj["customer"].(string)
	subscriptionID, _ = obj["subscription"].(string)
	if subscriptionID == "" {
		subscriptionID, _ = obj["id"].(string)
	}
	return workspaceID, plan, customerID, subscriptionID
}

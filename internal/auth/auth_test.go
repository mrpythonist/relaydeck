package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireAPIKey(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := RequireAPIKey("k123", next)
	// No key -> 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	// Bearer key -> 200.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer k123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Disabled auth (empty key) -> 200 without credentials.
	h2 := RequireAPIKey("", next)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("expected 200 with auth disabled, got %d", rec.Code)
	}
	// Timing-attack surface: wrong key of same length also 401.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "k124")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

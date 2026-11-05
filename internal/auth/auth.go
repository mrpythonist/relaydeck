// Package auth provides API-key authentication helpers.
package auth

import (
	"net/http"
	"strings"

	"github.com/mrpythonist/relaydeck/internal/security"
)

// APIKeyFromRequest extracts an API key from X-API-Key or Bearer token.
func APIKeyFromRequest(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// RequireAPIKey returns middleware that enforces the configured key.
// If configuredKey is empty, all requests pass (auth disabled).
func RequireAPIKey(configuredKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if configuredKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !security.ConstantTimeEqual(APIKeyFromRequest(r), configuredKey) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

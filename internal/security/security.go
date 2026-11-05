// Package security holds HMAC verification, secure ID generation,
// sensitive-header redaction and SSRF protection for outbound delivery.
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// NewEventID returns a secure random event ID like evt_<32 hex chars>.
func NewEventID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random event id: %w", err)
	}
	return "evt_" + hex.EncodeToString(b), nil
}

// NewID returns a secure random ID with the given prefix.
func NewID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// ConstantTimeEqual compares two strings in constant time.
func ConstantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// HMACSHA256Hex returns hex(hmac-sha256(secret, body)).
func HMACSHA256Hex(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifySignature checks a signature header against the body.
// Accepts "sha256=<hex>" (X-Hub-Signature-256 style) or bare hex.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	sig := strings.TrimSpace(header)
	sig = strings.TrimPrefix(sig, "sha256=")
	expected := HMACSHA256Hex(secret, body)
	// Compare decoded bytes when possible so case differences in hex don't matter.
	a, errA := hex.DecodeString(sig)
	b, errB := hex.DecodeString(expected)
	if errA == nil && errB == nil && len(a) == len(b) {
		return subtle.ConstantTimeCompare(a, b) == 1
	}
	return subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) == 1
}

var sensitiveHeaders = map[string]bool{
	"authorization":         true,
	"x-api-key":             true,
	"cookie":                true,
	"set-cookie":            true,
	"x-hub-signature-256":   true,
	"x-relaydeck-signature": true,
	"x-auth-token":          true,
}

// RedactHeaders returns a copy of headers with sensitive values masked.
func RedactHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = "***REDACTED***"
		} else {
			out[k] = v
		}
	}
	return out
}

// IsSensitiveHeader reports whether a header name must never be logged/stored in clear.
func IsSensitiveHeader(name string) bool {
	l := strings.ToLower(name)
	if sensitiveHeaders[l] {
		return true
	}
	return strings.Contains(l, "secret") || strings.Contains(l, "token") || strings.Contains(l, "password")
}

// --- SSRF protection ---

// Resolver resolves a hostname to IPs; injectable for tests (DNS rebinding tests).
type Resolver func(host string) ([]net.IP, error)

func defaultResolver(host string) ([]net.IP, error) { return net.LookupIP(host) }

// IsPublicIP reports whether ip is a routable public address.
// Blocks loopback, private (RFC1918), link-local, multicast, unspecified,
// CGNAT, documentation and cloud-metadata ranges.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	// net.IsPrivate covers RFC1918 + CGNAT in newer Go; be explicit for
	// 100.64.0.0/10 and documentation ranges.
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 CGNAT
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return false
		}
		// 192.0.0.0/24 (incl. 192.0.2.0/24 TEST-NET-1 handled below), 198.51.100/24, 203.0.113/24
		if (ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2) ||
			(ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100) ||
			(ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113) {
			return false
		}
		// 169.254.169.254 cloud metadata is link-local, already blocked.
	}
	return true
}

// ValidateDestinationURL validates a destination URL for SSRF safety.
// allowPrivate permits private/loopback targets (local dev only, explicit opt-in).
// trustedHosts bypasses the public-IP check for explicitly listed hosts.
// resolver may be nil (uses net.LookupIP). It resolves the hostname and
// re-checks every resolved IP, mitigating DNS rebinding.
func ValidateDestinationURL(raw string, allowPrivate bool, trustedHosts []string, resolver Resolver) error {
	if resolver == nil {
		resolver = defaultResolver
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("destination url scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("destination url must have a host")
	}
	if u.User != nil {
		return fmt.Errorf("destination url must not contain credentials")
	}
	for _, t := range trustedHosts {
		if strings.EqualFold(strings.TrimSpace(t), host) {
			return nil
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if !allowPrivate && !IsPublicIP(ip) {
			return fmt.Errorf("destination host %q resolves to a non-public IP %s (blocked: SSRF protection)", host, ip)
		}
		return nil
	}
	if allowPrivate {
		return nil
	}
	ips, err := resolver(host)
	if err != nil {
		return fmt.Errorf("cannot resolve destination host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("destination host %q resolves to no addresses", host)
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return fmt.Errorf("destination host %q resolves to non-public IP %s (blocked: SSRF protection)", host, ip)
		}
	}
	return nil
}

// Package delivery implements the retry/backoff math and the SSRF-safe
// HTTP sender used by workers to deliver events to destinations.
package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/security"
)

// BackoffDelay returns delay for the given attempt number (1-based):
// min(max, base * 2^(attempt-1)) with full jitter in [0.5x, 1.5x).
// rng may be nil (uses a fresh source).
func BackoffDelay(attempt int, base, max time.Duration, rng *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base <= 0 {
		base = time.Second
	}
	if max <= 0 {
		max = time.Minute
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			d = max
			break
		}
	}
	if d > max {
		d = max
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	jitter := 0.5 + rng.Float64() // [0.5, 1.5)
	return time.Duration(float64(d) * jitter)
}

// Result is one delivery attempt outcome.
type Result struct {
	StatusCode int
	DurationMs int64
	Body       string // truncated response snippet
	Err        error
}

// Sender sends event payloads to destinations through an SSRF-safe client.
type Sender struct {
	AllowPrivate bool
	TrustedHosts []string
	Resolver     security.Resolver
}

// NewSender builds a Sender.
func NewSender(allowPrivate bool, trustedHosts []string) *Sender {
	return &Sender{AllowPrivate: allowPrivate, TrustedHosts: trustedHosts}
}

func (s *Sender) validate(raw string) error {
	return security.ValidateDestinationURL(raw, s.AllowPrivate, s.TrustedHosts, s.Resolver)
}

// safeTransport returns an http.Transport whose DialContext re-validates the
// resolved IP of every connection (DNS-rebinding defence) and whose
// CheckRedirect re-validates redirect targets.
func (s *Sender) safeTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if !s.AllowPrivate && !s.isTrusted(host) {
				ips, err := net.LookupIP(host)
				if err != nil {
					return nil, fmt.Errorf("resolve %s: %w", host, err)
				}
				for _, ip := range ips {
					if !security.IsPublicIP(ip) {
						return nil, fmt.Errorf("blocked: %s resolves to non-public IP %s", host, ip)
					}
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        20,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

func (s *Sender) isTrusted(host string) bool {
	for _, t := range s.TrustedHosts {
		if strings.EqualFold(strings.TrimSpace(t), host) {
			return true
		}
	}
	return false
}

// Send delivers payload to dest. Validates the URL first (SSRF), then sends
// with the destination timeout. No DB transaction is held during the call.
func (s *Sender) Send(ctx context.Context, dest *database.Destination, payload []byte, contentType string) Result {
	start := time.Now()
	if err := s.validate(dest.URL); err != nil {
		return Result{DurationMs: time.Since(start).Milliseconds(), Err: err}
	}
	timeout := time.Duration(dest.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	method := dest.Method
	if method == "" {
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, dest.URL, bytes.NewReader(payload))
	if err != nil {
		return Result{DurationMs: time.Since(start).Milliseconds(), Err: err}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "RelayDeck/0.1.0")
	req.Header.Set("X-RelayDeck-Event-Delivery", "1")
	// Destination headers are operator-configured (not user-supplied), so
	// they are applied as-is, including Authorization when the operator
	// explicitly configured it. Secrets are never logged or returned by
	// the API; see SECURITY.md.
	for k, v := range dest.Headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Transport: s.safeTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			// Redirect targets are untrusted third-party choices: they must
			// always resolve to public IPs (unless explicitly trusted),
			// even when the configured destination itself was allowed to
			// be private for local development.
			if err := security.ValidateDestinationURL(req.URL.String(), false, s.TrustedHosts, s.Resolver); err != nil {
				return fmt.Errorf("redirect blocked: %w", err)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return Result{DurationMs: elapsed, Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return Result{StatusCode: resp.StatusCode, DurationMs: elapsed, Body: string(body)}
}

// Success reports whether a result counts as delivered (2xx).
func Success(r Result) bool { return r.Err == nil && r.StatusCode >= 200 && r.StatusCode < 300 }

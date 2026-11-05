// Package events holds shared event validation helpers.
package events

import (
	"fmt"
	"regexp"
	"strings"
)

// Source name rules: lowercase-ish slug, 1-64 chars.
var sourceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateSourceName checks a source path segment.
func ValidateSourceName(name string) error {
	if !sourceRe.MatchString(name) {
		return fmt.Errorf("invalid source name %q: use 1-64 chars, letters/digits/dot/dash/underscore", name)
	}
	return nil
}

// ValidatePayloadSize enforces the configured limit.
func ValidatePayloadSize(n, limit int64) error {
	if n > limit {
		return fmt.Errorf("payload %d bytes exceeds limit %d bytes", n, limit)
	}
	if n < 0 {
		return fmt.Errorf("invalid payload size")
	}
	return nil
}

// NormaliseSource lower-cases and trims a source name.
func NormaliseSource(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// Package routing matches events to destinations (source filter +
// optional payload substring filter + optional JSON-path filter).
// Kept intentionally simple: no rules engine in the MVP.
package routing

import (
	"strings"

	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/jsonpath"
)

// Match reports whether dest should receive an event from source with payload.
// Both filters must pass when both are set.
func Match(dest *database.Destination, source string, payload []byte) bool {
	if !dest.Enabled {
		return false
	}
	if dest.SourceFilter != "" && dest.SourceFilter != source {
		return false
	}
	if dest.Filter != "" && !strings.Contains(string(payload), dest.Filter) {
		return false
	}
	if dest.JSONPathFilter != "" && !jsonpath.Match(payload, dest.JSONPathFilter) {
		return false
	}
	return true
}

// Filter returns the matching subset of dests.
func Filter(dests []*database.Destination, source string, payload []byte) []*database.Destination {
	var out []*database.Destination
	for _, d := range dests {
		if Match(d, source, payload) {
			out = append(out, d)
		}
	}
	return out
}

package routing

import (
	"testing"

	"github.com/mrpythonist/relaydeck/internal/database"
)

func TestMatch(t *testing.T) {
	d := &database.Destination{Enabled: true, SourceFilter: "stripe"}
	if !Match(d, "stripe", []byte(`{}`)) || Match(d, "github", []byte(`{}`)) {
		t.Fatal("source filter wrong")
	}
	d.SourceFilter = ""
	d.Filter = "paid"
	if !Match(d, "stripe", []byte(`{"type":"paid"}`)) || Match(d, "stripe", []byte(`{"type":"x"}`)) {
		t.Fatal("substring filter wrong")
	}
	d.Enabled = false
	if Match(d, "stripe", []byte(`{"type":"paid"}`)) {
		t.Fatal("disabled must not match")
	}
}

func TestMatchJSONPathFilter(t *testing.T) {
	payload := []byte(`{"type":"payment_intent.succeeded","data":{"object":{"status":"succeeded"}}}`)
	d := &database.Destination{Enabled: true, JSONPathFilter: "type=payment_intent.succeeded"}
	if !Match(d, "stripe", payload) {
		t.Fatal("matching JSON-path filter should pass")
	}
	if Match(d, "stripe", []byte(`{"type":"charge.failed"}`)) {
		t.Fatal("non-matching JSON-path filter should block")
	}
	if Match(d, "stripe", []byte(`not json`)) {
		t.Fatal("non-JSON payload should not match a JSON-path filter")
	}
	// Both filters must pass when both are set.
	d.Filter = "succeeded"
	if !Match(d, "stripe", payload) {
		t.Fatal("both filters passing should match")
	}
	d.Filter = "failed"
	if Match(d, "stripe", payload) {
		t.Fatal("substring filter failing should block even when JSON-path passes")
	}
	d.Filter = "succeeded"
	d.JSONPathFilter = "type=charge.failed"
	if Match(d, "stripe", payload) {
		t.Fatal("JSON-path filter failing should block even when substring passes")
	}
}

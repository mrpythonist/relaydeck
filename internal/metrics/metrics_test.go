package metrics

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	m := New()
	m.EventsReceived.Add(3)
	m.DeliveriesOK.Add(2)
	m.DeliveriesFailed.Add(1)
	m.RetryAttempts.Add(1)
	m.ObserveDuration(120)
	m.ObserveDuration(1500)
	out := m.Render(4)
	for _, want := range []string{
		`relaydeck_events_received_total 3`,
		`relaydeck_deliveries_success_total 2`,
		`relaydeck_deliveries_failed_total 1`,
		`relaydeck_deliveries_pending 4`,
		`relaydeck_retry_attempts_total 1`,
		`relaydeck_delivery_duration_seconds_count 2`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, out)
		}
	}
}

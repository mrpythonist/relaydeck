// Package metrics holds in-memory operational counters and renders
// Prometheus-compatible text exposition format without external deps.
package metrics

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics are process-wide counters. Pending is read from the DB at scrape time.
type Metrics struct {
	EventsReceived   atomic.Int64
	DeliveriesOK     atomic.Int64
	DeliveriesFailed atomic.Int64
	RetryAttempts    atomic.Int64
	DurationSumMs    atomic.Int64
	DurationCount    atomic.Int64
	mu               sync.Mutex
	buckets          map[float64]int64 // latency histogram buckets (seconds)
}

var bucketBounds = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

func New() *Metrics { return &Metrics{buckets: make(map[float64]int64)} }

func (m *Metrics) ObserveDuration(ms int64) {
	m.DurationSumMs.Add(ms)
	m.DurationCount.Add(int64(1))
	sec := float64(ms) / 1000.0
	m.mu.Lock()
	for _, b := range bucketBounds {
		if sec <= b {
			m.buckets[b]++
		}
	}
	m.mu.Unlock()
}

// Render returns Prometheus text format. pending is supplied by the caller (DB count).
func (m *Metrics) Render(pending int64) string {
	var sb strings.Builder
	w := func(name, help, typ string, val string) {
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", name, help, name, typ, name, val)
	}
	w("relaydeck_events_received_total", "Total webhook events accepted.", "counter", fmt.Sprint(m.EventsReceived.Load()))
	w("relaydeck_deliveries_success_total", "Total successful deliveries.", "counter", fmt.Sprint(m.DeliveriesOK.Load()))
	w("relaydeck_deliveries_failed_total", "Total failed delivery attempts.", "counter", fmt.Sprint(m.DeliveriesFailed.Load()))
	w("relaydeck_deliveries_pending", "Deliveries currently pending or scheduled.", "gauge", fmt.Sprint(pending))
	w("relaydeck_retry_attempts_total", "Total retry attempts.", "counter", fmt.Sprint(m.RetryAttempts.Load()))
	sb.WriteString("# HELP relaydeck_delivery_duration_seconds Delivery latency in seconds.\n# TYPE relaydeck_delivery_duration_seconds histogram\n")
	m.mu.Lock()
	for _, b := range bucketBounds {
		fmt.Fprintf(&sb, "relaydeck_delivery_duration_seconds_bucket{le=\"%g\"} %d\n", b, m.buckets[b])
	}
	m.mu.Unlock()
	fmt.Fprintf(&sb, "relaydeck_delivery_duration_seconds_bucket{le=\"+Inf\"} %d\n", m.DurationCount.Load())
	fmt.Fprintf(&sb, "relaydeck_delivery_duration_seconds_sum %g\n", float64(m.DurationSumMs.Load())/1000.0)
	fmt.Fprintf(&sb, "relaydeck_delivery_duration_seconds_count %d\n", m.DurationCount.Load())
	return sb.String()
}

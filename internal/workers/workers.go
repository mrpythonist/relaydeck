// Package workers runs the background delivery loop: claim due deliveries
// with a lease, send them, record attempts, schedule retries with
// exponential backoff + jitter, and recover stale leases after restarts.
package workers

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/delivery"
	"github.com/mrpythonist/relaydeck/internal/metrics"
)

// Pool is a bounded-concurrency delivery worker pool.
type Pool struct {
	Store   *database.Store
	Sender  *delivery.Sender
	Metrics *metrics.Metrics
	Workers int
	ID      string
	// LeaseTimeout: a claimed delivery older than this is re-queued.
	LeaseTimeout time.Duration
	PollInterval time.Duration
	rng          *rand.Rand
	rngMu        sync.Mutex
}

// New creates a Pool.
func New(store *database.Store, sender *delivery.Sender, m *metrics.Metrics, workers int, id string) *Pool {
	if workers <= 0 {
		workers = 4
	}
	return &Pool{
		Store: store, Sender: sender, Metrics: m, Workers: workers, ID: id,
		LeaseTimeout: 2 * time.Minute, PollInterval: 500 * time.Millisecond,
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (p *Pool) backoff(attempt int, dest *database.Destination) time.Duration {
	p.rngMu.Lock()
	defer p.rngMu.Unlock()
	return delivery.BackoffDelay(attempt, time.Duration(dest.BackoffBaseMs)*time.Millisecond, time.Duration(dest.BackoffMaxMs)*time.Millisecond, p.rng)
}

// Run loops until ctx is done. On start it re-queues stale leases
// (crash/restart recovery).
func (p *Pool) Run(ctx context.Context) {
	_, _ = p.Store.RequeueStaleLeases(p.LeaseTimeout)
	ticker := time.NewTicker(p.PollInterval)
	defer ticker.Stop()
	sem := make(chan struct{}, p.Workers)
	for {
		select {
		case <-ctx.Done():
			// Drain: wait for in-flight sends to finish (bounded by sem).
			for i := 0; i < p.Workers; i++ {
				sem <- struct{}{}
			}
			return
		case <-ticker.C:
			// Opportunistically recover stale leases each tick.
			_, _ = p.Store.RequeueStaleLeases(p.LeaseTimeout)
			claimed, err := p.Store.ClaimDue(p.ID, p.Workers, p.LeaseTimeout)
			if err != nil {
				continue
			}
			for _, d := range claimed {
				d := d
				sem <- struct{}{}
				go func() {
					defer func() { <-sem }()
					p.deliverOne(ctx, d)
				}()
			}
		}
	}
}

func (p *Pool) deliverOne(ctx context.Context, d *database.Delivery) {
	dest, err := p.Store.GetDestination(d.DestinationID)
	if err != nil || dest == nil {
		_ = p.Store.RecordAttempt(d.ID, nil, 0, fmt.Sprintf("destination %s not found", d.DestinationID), "", false, 1, 0)
		return
	}
	ev, err := p.Store.GetEvent(d.EventID)
	if err != nil || ev == nil {
		_ = p.Store.RecordAttempt(d.ID, nil, 0, fmt.Sprintf("event %s not found", d.EventID), "", false, 1, 0)
		return
	}
	res := p.Sender.Send(ctx, dest, ev.Payload, ev.ContentType)
	p.Metrics.ObserveDuration(res.DurationMs)
	if delivery.Success(res) {
		p.Metrics.DeliveriesOK.Add(1)
		code := res.StatusCode
		_ = p.Store.RecordAttempt(d.ID, &code, res.DurationMs, "", truncate(res.Body, 1024), true, dest.MaxRetries, 0)
		return
	}
	p.Metrics.DeliveriesFailed.Add(1)
	if d.Attempts+1 < dest.MaxRetries {
		p.Metrics.RetryAttempts.Add(1)
	}
	var code *int
	if res.Err == nil {
		code = &res.StatusCode
	}
	errText := ""
	if res.Err != nil {
		errText = res.Err.Error()
	} else {
		errText = fmt.Sprintf("destination returned HTTP %d", res.StatusCode)
	}
	maxRetries := dest.MaxRetries
	if maxRetries < 1 {
		maxRetries = 1
	}
	delay := p.backoff(d.Attempts+1, dest)
	_ = p.Store.RecordAttempt(d.ID, code, res.DurationMs, errText, truncate(res.Body, 1024), false, maxRetries, delay)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

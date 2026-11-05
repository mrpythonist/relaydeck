package workers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/delivery"
	"github.com/mrpythonist/relaydeck/internal/metrics"
)

func testStore(t *testing.T) *database.Store {
	t.Helper()
	s, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkerDeliversSuccessfully(t *testing.T) {
	var received int32
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		receivedBody = string(buf[:n])
		w.WriteHeader(200)
	}))
	defer srv.Close()
	store := testStore(t)
	_ = store.CreateDestination(&database.Destination{ID: "dst_aaaaaaaaaaaaaaaaaaaaaaaa", Name: "api", URL: srv.URL, Method: "POST", TimeoutSeconds: 5, MaxRetries: 3, BackoffBaseMs: 100, BackoffMaxMs: 1000, Enabled: true})
	dests, _ := store.MatchingDestinations("stripe", []byte(`{}`))
	ev := &database.Event{ID: "evt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Source: "stripe", Payload: []byte(`{"type":"paid"}`), Headers: map[string]string{}}
	if _, err := store.CreateEvent(ev, dests); err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	pool := New(store, delivery.NewSender(true, nil), m, 2, "test-worker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)
	waitFor(t, func() bool {
		dels, _ := store.ListDeliveries(10, "delivered", "")
		return len(dels) == 1
	}, "delivery to succeed")
	if atomic.LoadInt32(&received) != 1 || receivedBody != `{"type":"paid"}` {
		t.Fatalf("receiver got count=%d body=%q", received, receivedBody)
	}
	if m.DeliveriesOK.Load() != 1 {
		t.Fatalf("metrics success = %d", m.DeliveriesOK.Load())
	}
}

func TestWorkerRetriesThenDead(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	store := testStore(t)
	_ = store.CreateDestination(&database.Destination{ID: "dst_aaaaaaaaaaaaaaaaaaaaaaaa", Name: "api", URL: srv.URL, Method: "POST", TimeoutSeconds: 5, MaxRetries: 2, BackoffBaseMs: 50, BackoffMaxMs: 100, Enabled: true})
	dests, _ := store.MatchingDestinations("stripe", []byte(`{}`))
	ev := &database.Event{ID: "evt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
	_, _ = store.CreateEvent(ev, dests)
	m := metrics.New()
	pool := New(store, delivery.NewSender(true, nil), m, 2, "test-worker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)
	waitFor(t, func() bool {
		dels, _ := store.ListDeliveries(10, "dead", "")
		return len(dels) == 1
	}, "delivery to go dead after retries")
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts (maxRetries=2), got %d", attempts)
	}
	dels, _ := store.ListDeliveries(10, "dead", "")
	if dels[0].Attempts != 2 {
		t.Fatalf("expected attempts=2, got %d", dels[0].Attempts)
	}
	atts, _ := store.ListAttempts(dels[0].ID)
	if len(atts) != 2 {
		t.Fatalf("expected 2 recorded attempts, got %d", len(atts))
	}
}

func TestWorkerGracefulShutdown(t *testing.T) {
	store := testStore(t)
	m := metrics.New()
	pool := New(store, delivery.NewSender(true, nil), m, 2, "test-worker")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker pool did not stop after context cancel")
	}
}

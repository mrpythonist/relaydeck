// Package tests holds end-to-end tests that boot the real binary's HTTP
// server (via the api package) against a real SQLite file and a real
// httptest receiver, exercising ingest -> worker -> delivery end to end.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrpythonist/relaydeck/internal/api"
	"github.com/mrpythonist/relaydeck/internal/config"
	"github.com/mrpythonist/relaydeck/internal/database"
	"github.com/mrpythonist/relaydeck/internal/delivery"
	"github.com/mrpythonist/relaydeck/internal/metrics"
	"github.com/mrpythonist/relaydeck/internal/workers"
)

func TestEndToEndIngestDeliver(t *testing.T) {
	var got int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&got, 1)
		w.WriteHeader(200)
	}))
	defer receiver.Close()

	store, err := database.Open(filepath.Join(t.TempDir(), "relaydeck.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.CreateDestination(&database.Destination{
		ID: "dst_aaaaaaaaaaaaaaaaaaaaaaaa", Name: "receiver", URL: receiver.URL + "/hook",
		Method: "POST", TimeoutSeconds: 5, MaxRetries: 3, BackoffBaseMs: 100, BackoffMaxMs: 1000, Enabled: true,
	})
	cfg := config.Defaults()
	cfg.AllowPrivate = true
	m := metrics.New()
	srv := api.New(store, cfg, m)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()

	pool := workers.New(store, delivery.NewSender(true, nil), m, 2, "e2e")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Run(ctx)

	resp, err := http.Post(httpSrv.URL+"/api/v1/webhooks/github", "application/json",
		bytes.NewReader([]byte(`{"action":"push","ref":"refs/heads/main"}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest status %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	deadline := time.Now().Add(10 * time.Second)
	var dels []*database.Delivery
	for time.Now().Before(deadline) {
		dels, _ = store.ListDeliveries(10, "delivered", "")
		if len(dels) == 1 && atomic.LoadInt32(&got) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&got) != 1 {
		t.Fatal("receiver never got the event")
	}
	if len(dels) != 1 {
		t.Fatalf("expected 1 delivered delivery, got %d", len(dels))
	}
	st, _ := store.Stats()
	if st.TotalEvents != 1 || st.SuccessRate != 100 {
		t.Fatalf("stats wrong: %+v", st)
	}
}

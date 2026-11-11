package database

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testDest(id, name string) *Destination {
	return &Destination{ID: id, Name: name, URL: "https://example.com/hook", Method: "POST", TimeoutSeconds: 10, MaxRetries: 3, BackoffBaseMs: 100, BackoffMaxMs: 1000, Enabled: true}
}

func TestCreateEventAndDeliveries(t *testing.T) {
	s := openTestStore(t)
	if err := s.CreateDestination(testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "laravel-api")); err != nil {
		t.Fatal(err)
	}
	dests, _ := s.MatchingDestinations("stripe", []byte(`{}`))
	ev := &Event{ID: "evt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Source: "stripe", Payload: []byte(`{"type":"x"}`), Headers: map[string]string{"Content-Type": "application/json"}, ContentType: "application/json"}
	existed, err := s.CreateEvent(ev, dests)
	if err != nil || existed {
		t.Fatalf("create: existed=%v err=%v", existed, err)
	}
	got, err := s.GetEvent(ev.ID)
	if err != nil || got == nil || string(got.Payload) != `{"type":"x"}` {
		t.Fatalf("get event: %+v %v", got, err)
	}
	dels, err := s.ListDeliveries(10, "", "")
	if err != nil || len(dels) != 1 || dels[0].Status != "pending" {
		t.Fatalf("deliveries: %v %v", dels, err)
	}
}

func TestIdempotency(t *testing.T) {
	s := openTestStore(t)
	_ = s.CreateDestination(testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api"))
	dests, _ := s.MatchingDestinations("github", []byte(`{}`))
	ev1 := &Event{ID: "evt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Source: "github", Payload: []byte(`{}`), Headers: map[string]string{}, IdempotencyKey: "key-1"}
	existed, err := s.CreateEvent(ev1, dests)
	if err != nil || existed {
		t.Fatalf("first: %v %v", existed, err)
	}
	ev2 := &Event{ID: "evt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Source: "github", Payload: []byte(`{}`), Headers: map[string]string{}, IdempotencyKey: "key-1"}
	existed, err = s.CreateEvent(ev2, dests)
	if err != nil || !existed {
		t.Fatalf("second should be duplicate: existed=%v err=%v", existed, err)
	}
	if ev2.ID != ev1.ID {
		t.Fatalf("duplicate should return original id, got %s want %s", ev2.ID, ev1.ID)
	}
	dels, _ := s.ListDeliveries(10, "", "")
	if len(dels) != 1 {
		t.Fatalf("idempotent replay must not duplicate deliveries, got %d", len(dels))
	}
	// Same key, different source: independent.
	ev3 := &Event{ID: "evt_cccccccccccccccccccccccccccccccc", Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}, IdempotencyKey: "key-1"}
	existed, err = s.CreateEvent(ev3, dests)
	if err != nil || existed {
		t.Fatalf("different source should not collide: %v %v", existed, err)
	}
	// No key: multiple events from same source must all succeed.
	for i := 0; i < 3; i++ {
		ev := &Event{ID: "evt_dddddddddddddddddddddddddddddd0" + string(rune('0'+i)), Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
		if _, err := s.CreateEvent(ev, nil); err != nil {
			t.Fatalf("no-key event %d: %v", i, err)
		}
	}
}

func TestClaimLeaseConcurrency(t *testing.T) {
	s := openTestStore(t)
	_ = s.CreateDestination(testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api"))
	dests, _ := s.MatchingDestinations("stripe", []byte(`{}`))
	for i := 0; i < 5; i++ {
		ev := &Event{ID: "evt_eeeeeeeeeeeeeeeeeeeeeeeeeeeeee0" + string(rune('0'+i)), Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
		if _, err := s.CreateEvent(ev, dests); err != nil {
			t.Fatal(err)
		}
	}
	// Two workers claiming concurrently must never get the same delivery.
	var wg sync.WaitGroup
	seen := map[string]int{}
	var mu sync.Mutex
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			claimed, err := s.ClaimDue(id, 5, time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			for _, d := range claimed {
				seen[d.ID]++
			}
			mu.Unlock()
		}(string(rune('a' + w)))
	}
	wg.Wait()
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct claimed deliveries, got %d: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("delivery %s claimed %d times", id, n)
		}
	}
}

func TestStaleLeaseRecovery(t *testing.T) {
	s := openTestStore(t)
	_ = s.CreateDestination(testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api"))
	dests, _ := s.MatchingDestinations("stripe", []byte(`{}`))
	ev := &Event{ID: "evt_ffffffffffffffffffffffffffffffff", Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
	_, _ = s.CreateEvent(ev, dests)
	claimed, err := s.ClaimDue("w1", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	// Still leased: second claim gets nothing.
	if c2, _ := s.ClaimDue("w2", 1, time.Minute); len(c2) != 0 {
		t.Fatalf("leased delivery claimed twice: %v", c2)
	}
	// After lease expiry it becomes claimable again.
	if c3, _ := s.ClaimDue("w2", 1, time.Nanosecond); len(c3) != 1 {
		t.Fatalf("stale lease not recovered: %v", c3)
	}
	n, err := s.RequeueStaleLeases(time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	_ = n
}

func TestRecordAttemptSuccessAndDead(t *testing.T) {
	s := openTestStore(t)
	_ = s.CreateDestination(testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api"))
	dests, _ := s.MatchingDestinations("stripe", []byte(`{}`))
	ev := &Event{ID: "evt_11111111111111111111111111111111", Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
	_, _ = s.CreateEvent(ev, dests)
	dels, _ := s.ListDeliveries(10, "", "")
	code := 200
	if err := s.RecordAttempt(dels[0].ID, &code, 42, "", "ok", true, 3, 0); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDelivery(dels[0].ID)
	if d.Status != "delivered" || d.Attempts != 1 {
		t.Fatalf("expected delivered/1, got %s/%d", d.Status, d.Attempts)
	}
	// Failure path: exhaust retries -> dead.
	ev2 := &Event{ID: "evt_22222222222222222222222222222222", Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
	_, _ = s.CreateEvent(ev2, dests)
	dels, _ = s.ListDeliveries(10, "pending", "")
	if len(dels) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(dels))
	}
	code500 := 500
	for i := 0; i < 3; i++ {
		if err := s.RecordAttempt(dels[0].ID, &code500, 10, "boom", "", false, 3, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	d, _ = s.GetDelivery(dels[0].ID)
	if d.Status != "dead" || d.Attempts != 3 {
		t.Fatalf("expected dead/3, got %s/%d", d.Status, d.Attempts)
	}
	atts, err := s.ListAttempts(d.ID)
	if err != nil || len(atts) != 3 {
		t.Fatalf("expected 3 attempts recorded, got %d %v", len(atts), err)
	}
	// Retry a dead delivery.
	ok, err := s.RetryDelivery(d.ID)
	if err != nil || !ok {
		t.Fatalf("retry dead: %v %v", ok, err)
	}
	d, _ = s.GetDelivery(d.ID)
	if d.Status != "pending" {
		t.Fatalf("expected pending after retry, got %s", d.Status)
	}
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.CreateDestination(testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api"))
	dests, _ := s1.MatchingDestinations("stripe", []byte(`{}`))
	ev := &Event{ID: "evt_33333333333333333333333333333333", Source: "stripe", Payload: []byte(`{"x":1}`), Headers: map[string]string{}}
	_, _ = s1.CreateEvent(ev, dests)
	_ = s1.Close() // simulate restart
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetEvent(ev.ID)
	if err != nil || got == nil || string(got.Payload) != `{"x":1}` {
		t.Fatalf("event lost across restart: %+v %v", got, err)
	}
	dels, _ := s2.ListDeliveries(10, "pending", "")
	if len(dels) != 1 {
		t.Fatalf("pending delivery lost across restart, got %d", len(dels))
	}
	st, err := s2.Stats()
	if err != nil || st.TotalEvents != 1 || st.Pending != 1 {
		t.Fatalf("stats after restart: %+v %v", st, err)
	}
}

func TestDestinationCRUD(t *testing.T) {
	s := openTestStore(t)
	d := testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api")
	if err := s.CreateDestination(d); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDestination(testDest("dst_bbbbbbbbbbbbbbbbbbbbbbbb", "api")); err == nil {
		t.Fatal("duplicate name should fail")
	}
	got, _ := s.GetDestinationByName("api")
	if got == nil || got.URL != "https://example.com/hook" {
		t.Fatalf("get by name: %+v", got)
	}
	got.Enabled = false
	if err := s.UpdateDestination(got); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MatchingDestinations("stripe", []byte(`{}`)); len(m) != 0 {
		t.Fatal("disabled destination must not match")
	}
	if err := s.DeleteDestination(got.ID); err != nil {
		t.Fatal(err)
	}
	if g2, _ := s.GetDestination(got.ID); g2 != nil {
		t.Fatal("deleted destination still present")
	}
}

// TestDestinationV021Fields round-trips the v0.2.1 destination fields
// (JSON-path filter, signing secret, header templates) through create,
// read and update, and checks the JSON-path filter in matching.
func TestDestinationV021Fields(t *testing.T) {
	s := openTestStore(t)
	d := testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api")
	d.JSONPathFilter = "type=payment_intent.succeeded"
	d.SigningSecret = "dst-secret"
	d.HeaderTemplates = map[string]string{"X-Event-Type": "{{.type}}"}
	if err := s.CreateDestination(d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDestinationByName("api")
	if err != nil || got == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got.JSONPathFilter != "type=payment_intent.succeeded" || got.SigningSecret != "dst-secret" {
		t.Fatalf("fields not stored: %+v", got)
	}
	if got.HeaderTemplates["X-Event-Type"] != "{{.type}}" {
		t.Fatalf("templates not stored: %+v", got.HeaderTemplates)
	}
	// Matching honours the JSON-path filter.
	matchPayload := []byte(`{"type":"payment_intent.succeeded"}`)
	nomatchPayload := []byte(`{"type":"charge.failed"}`)
	if m, _ := s.MatchingDestinations("stripe", matchPayload); len(m) != 1 {
		t.Fatalf("matching payload should match 1 destination, got %d", len(m))
	}
	if m, _ := s.MatchingDestinations("stripe", nomatchPayload); len(m) != 0 {
		t.Fatalf("non-matching payload should match 0 destinations, got %d", len(m))
	}
	// Update the fields and clear them again.
	got.JSONPathFilter = "type=charge.failed"
	got.SigningSecret = "new-secret"
	if err := s.UpdateDestination(got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetDestinationByName("api")
	if got2.JSONPathFilter != "type=charge.failed" || got2.SigningSecret != "new-secret" {
		t.Fatalf("fields not updated: %+v", got2)
	}
	got2.JSONPathFilter = ""
	got2.SigningSecret = ""
	got2.HeaderTemplates = map[string]string{}
	if err := s.UpdateDestination(got2); err != nil {
		t.Fatal(err)
	}
	got3, _ := s.GetDestinationByName("api")
	if got3.JSONPathFilter != "" || got3.SigningSecret != "" || len(got3.HeaderTemplates) != 0 {
		t.Fatalf("fields not cleared: %+v", got3)
	}
}

// TestV021MigrationColumns checks the v0.2.1 columns exist on a fresh
// database and that a pre-v0.2.1 database gains them on open.
func TestV021MigrationColumns(t *testing.T) {
	s := openTestStore(t)
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('destinations') WHERE name IN ('filter_jsonpath','signing_secret','header_templates_json')`).Scan(&n)
	if err != nil || n != 3 {
		t.Fatalf("fresh DB should have 3 new columns, found %d (err %v)", n, err)
	}
	// Simulate a pre-v0.2.1 database: drop the columns via a table rebuild
	// without them, then re-open and check migrateV021 restores them.
	dbPath := filepath.Join(t.TempDir(), "old.db")
	old, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.DB.Exec(`ALTER TABLE destinations RENAME TO destinations_old;
		CREATE TABLE destinations (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL, source_filter TEXT NOT NULL DEFAULT '', url TEXT NOT NULL, method TEXT NOT NULL DEFAULT 'POST', headers_json TEXT NOT NULL DEFAULT '{}', timeout_seconds INTEGER NOT NULL DEFAULT 10, max_retries INTEGER NOT NULL DEFAULT 5, backoff_base_ms INTEGER NOT NULL DEFAULT 1000, backoff_max_ms INTEGER NOT NULL DEFAULT 60000, filter_substring TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(workspace_id, name));
		INSERT INTO destinations(id,workspace_id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at) SELECT id,workspace_id,name,source_filter,url,method,headers_json,timeout_seconds,max_retries,backoff_base_ms,backoff_max_ms,filter_substring,enabled,created_at,updated_at FROM destinations_old;
		DROP TABLE destinations_old`)
	if err != nil {
		t.Fatalf("rebuild old table: %v", err)
	}
	_ = old.Close()
	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	err = reopened.DB.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('destinations') WHERE name IN ('filter_jsonpath','signing_secret','header_templates_json')`).Scan(&n)
	if err != nil || n != 3 {
		t.Fatalf("reopened DB should have 3 new columns, found %d (err %v)", n, err)
	}
}

// TestDeleteDestinationWithDeliveries checks that deleting a destination
// also removes its delivery history instead of failing the foreign key.
func TestDeleteDestinationWithDeliveries(t *testing.T) {
	s := openTestStore(t)
	d := testDest("dst_aaaaaaaaaaaaaaaaaaaaaaaa", "api")
	if err := s.CreateDestination(d); err != nil {
		t.Fatal(err)
	}
	dests, _ := s.MatchingDestinations("stripe", []byte(`{}`))
	ev := &Event{ID: "evt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Source: "stripe", Payload: []byte(`{"type":"x"}`), Headers: map[string]string{}}
	if _, err := s.CreateEvent(ev, dests); err != nil {
		t.Fatal(err)
	}
	if dels, _ := s.ListDeliveries(10, "", ""); len(dels) != 1 {
		t.Fatalf("expected 1 delivery before delete, got %d", len(dels))
	}
	ok, err := s.DeleteDestinationForWorkspace(d.ID, "")
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if g, _ := s.GetDestination(d.ID); g != nil {
		t.Fatal("destination still present after delete")
	}
	if dels, _ := s.ListDeliveries(10, "", ""); len(dels) != 0 {
		t.Fatalf("deliveries should be gone with the destination, got %d", len(dels))
	}
	// The event itself is kept: it can belong to other destinations.
	if got, _ := s.GetEvent(ev.ID); got == nil {
		t.Fatal("event should survive its destination being deleted")
	}
	// Deleting a missing destination reports false, no error.
	if ok, err := s.DeleteDestinationForWorkspace("dst_missing", ""); err != nil || ok {
		t.Fatalf("delete missing: ok=%v err=%v", ok, err)
	}
}

func TestSourceSecret(t *testing.T) {
	s := openTestStore(t)
	_ = s.UpsertSource("stripe", "whsec_123")
	sec, found, err := s.GetSourceSecret("stripe")
	if err != nil || !found || sec != "whsec_123" {
		t.Fatalf("secret: %q %v %v", sec, found, err)
	}
	if _, found, _ := s.GetSourceSecret("unknown"); found {
		t.Fatal("unknown source should not be found")
	}
}

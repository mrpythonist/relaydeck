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

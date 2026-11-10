package database

import (
	"testing"
	"time"
)

func TestWorkspaceIsolationAtStoreLevel(t *testing.T) {
	s := openTestStore(t)
	// Two workspaces, same destination name and same idempotency key.
	wsA := &Workspace{ID: "ws_aaaaaaaaaaaaaaaaaaaaaaaa", PublicID: "pub_aaaaaaaaaaaaaaaaaaaaaaaa", Name: "A", OwnerUserID: "usr_1", Plan: "free"}
	wsB := &Workspace{ID: "ws_bbbbbbbbbbbbbbbbbbbbbbbb", PublicID: "pub_bbbbbbbbbbbbbbbbbbbbbbbb", Name: "B", OwnerUserID: "usr_2", Plan: "free"}
	if err := s.CreateWorkspace(wsA); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkspace(wsB); err != nil {
		t.Fatal(err)
	}
	da := &Destination{ID: "dst_aaaaaaaaaaaaaaaaaaaaaaaa", WorkspaceID: wsA.ID, Name: "api", URL: "https://example.com/a", Method: "POST", TimeoutSeconds: 10, MaxRetries: 3, Enabled: true}
	db := &Destination{ID: "dst_bbbbbbbbbbbbbbbbbbbbbbbb", WorkspaceID: wsB.ID, Name: "api", URL: "https://example.com/b", Method: "POST", TimeoutSeconds: 10, MaxRetries: 3, Enabled: true}
	if err := s.CreateDestination(da); err != nil {
		t.Fatalf("A destination: %v", err)
	}
	if err := s.CreateDestination(db); err != nil {
		t.Fatalf("B destination with same name should succeed in its own workspace: %v", err)
	}
	// Same idempotency key + source in both workspaces: both must succeed.
	for _, tc := range []struct {
		ws, id string
	}{
		{wsA.ID, "evt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{wsB.ID, "evt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	} {
		dests, _ := s.MatchingDestinationsForWorkspace(tc.ws, "stripe", []byte(`{}`))
		ev := &Event{ID: tc.id, WorkspaceID: tc.ws, Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}, IdempotencyKey: "key-1"}
		if existed, err := s.CreateEvent(ev, dests); err != nil || existed {
			t.Fatalf("workspace %s create: existed=%v err=%v", tc.ws, existed, err)
		}
	}
	// Lists are scoped.
	evsA, _ := s.ListEventsForWorkspace(wsA.ID, 10, "", "", "")
	if len(evsA) != 1 || evsA[0].WorkspaceID != wsA.ID {
		t.Fatalf("A events: %+v", evsA)
	}
	evsB, _ := s.ListEventsForWorkspace(wsB.ID, 10, "", "", "")
	if len(evsB) != 1 {
		t.Fatalf("B events: %d", len(evsB))
	}
	// Cross-workspace reads return nil.
	if got, _ := s.GetEventForWorkspace("evt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", wsB.ID); got != nil {
		t.Fatal("B must not read A's event")
	}
	if got, _ := s.GetDestinationForWorkspace("dst_aaaaaaaaaaaaaaaaaaaaaaaa", wsB.ID); got != nil {
		t.Fatal("B must not read A's destination")
	}
	// Sources are scoped too.
	_ = s.UpsertSourceForWorkspace(wsA.ID, "stripe", "whsec_a")
	_ = s.UpsertSourceForWorkspace(wsB.ID, "stripe", "whsec_b")
	sec, _, _ := s.GetSourceSecretForWorkspace(wsA.ID, "stripe")
	if sec != "whsec_a" {
		t.Fatalf("A source secret leaked: %q", sec)
	}
	// Usage counts per workspace.
	if n, _ := s.MonthlyEventCount(wsA.ID); n != 1 {
		t.Fatalf("A usage should be 1, got %d", n)
	}
	if n, _ := s.MonthlyEventCount(wsB.ID); n != 1 {
		t.Fatalf("B usage should be 1, got %d", n)
	}
}

func TestUsersSessionsAndAPIKeys(t *testing.T) {
	s := openTestStore(t)
	u := &User{ID: "usr_aaaaaaaaaaaaaaaaaaaaaaaa", Email: "aqib@example.com", PasswordHash: "hash", Name: "Aqib"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(&User{ID: "usr_bbbbbbbbbbbbbbbbbbbbbbbb", Email: "aqib@example.com", PasswordHash: "x"}); err == nil {
		t.Fatal("duplicate email should fail")
	}
	got, _ := s.GetUserByEmail("AQIB@example.com")
	if got == nil || got.ID != u.ID {
		t.Fatalf("email lookup should be case-insensitive: %+v", got)
	}
	ws := &Workspace{ID: "ws_aaaaaaaaaaaaaaaaaaaaaaaa", PublicID: "pub_aaaaaaaaaaaaaaaaaaaaaaaa", Name: "Acme", OwnerUserID: u.ID, Plan: "free"}
	if err := s.CreateWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMembership(ws.ID, u.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsMember(ws.ID, u.ID); !ok {
		t.Fatal("owner should be a member")
	}
	if ok, _ := s.IsMember(ws.ID, "usr_nobody"); ok {
		t.Fatal("non-member reported as member")
	}
	// Session round-trip, and expiry.
	tokenHash := HashToken("session-token-123")
	if err := s.CreateSession(tokenHash, u.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	su, _ := s.GetSessionUser(tokenHash)
	if su == nil || su.ID != u.ID {
		t.Fatalf("session user: %+v", su)
	}
	if err := s.CreateSession(HashToken("expired-token"), u.ID, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if su, _ := s.GetSessionUser(HashToken("expired-token")); su != nil {
		t.Fatal("expired session should not resolve")
	}
	// Workspace API key: lookup by plain key, revoked keys fail.
	k, err := s.CreateWorkspaceAPIKey("key_aaaaaaaaaaaaaaaaaaaaaaaa", ws.ID, "default", "rdk_testkey1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if k.KeyPrefix == "" {
		t.Fatal("key prefix empty")
	}
	gotWS, _ := s.GetWorkspaceByAPIKey("rdk_testkey1234567890")
	if gotWS == nil || gotWS.ID != ws.ID {
		t.Fatalf("api key lookup: %+v", gotWS)
	}
	if gotWS, _ := s.GetWorkspaceByAPIKey("rdk_wrongkey"); gotWS != nil {
		t.Fatal("wrong api key resolved")
	}
	if ok, _ := s.RevokeWorkspaceAPIKey(ws.ID, k.ID); !ok {
		t.Fatal("revoke should succeed")
	}
	if gotWS, _ := s.GetWorkspaceByAPIKey("rdk_testkey1234567890"); gotWS != nil {
		t.Fatal("revoked api key still resolves")
	}
}

func TestRetentionPruningPerWorkspace(t *testing.T) {
	s := openTestStore(t)
	wsA := &Workspace{ID: "ws_aaaaaaaaaaaaaaaaaaaaaaaa", PublicID: "pub_aaaaaaaaaaaaaaaaaaaaaaaa", Name: "A", Plan: "free"}
	wsB := &Workspace{ID: "ws_bbbbbbbbbbbbbbbbbbbbbbbb", PublicID: "pub_bbbbbbbbbbbbbbbbbbbbbbbb", Name: "B", Plan: "growth"}
	_ = s.CreateWorkspace(wsA)
	_ = s.CreateWorkspace(wsB)
	// One old event and one recent event in A.
	oldEv := &Event{ID: "evt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorkspaceID: wsA.ID, Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
	newEv := &Event{ID: "evt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", WorkspaceID: wsA.ID, Source: "stripe", Payload: []byte(`{}`), Headers: map[string]string{}}
	if _, err := s.CreateEvent(oldEv, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEvent(newEv, nil); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().UTC().AddDate(0, 0, -10).Format(time.RFC3339Nano)
	if _, err := s.DB.Exec(`UPDATE events SET created_at=? WHERE id=?`, oldTime, oldEv.ID); err != nil {
		t.Fatal(err)
	}
	// Prune A with 3-day retention (free plan): old event goes, recent stays.
	n, err := s.DeleteOldEventsForWorkspace(wsA.ID, 3)
	if err != nil || n != 1 {
		t.Fatalf("prune A: n=%d err=%v", n, err)
	}
	if got, _ := s.GetEvent(oldEv.ID); got != nil {
		t.Fatal("old event was not pruned")
	}
	if got, _ := s.GetEvent(newEv.ID); got == nil {
		t.Fatal("recent event was pruned")
	}
	// Pruning B must not touch A.
	if n, _ := s.DeleteOldEventsForWorkspace(wsB.ID, 3); n != 0 {
		t.Fatalf("prune B deleted %d rows from A", n)
	}
	// Disabled retention prunes nothing.
	if n, _ := s.DeleteOldEventsForWorkspace(wsA.ID, 0); n != 0 {
		t.Fatalf("retention 0 should prune nothing, got %d", n)
	}
}

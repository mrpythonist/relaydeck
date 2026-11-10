package plans

import "testing"

func TestGet(t *testing.T) {
	if got := Get("free"); got.MonthlyEvents != 1000 || got.MaxDestinations != 3 || got.RetentionDays != 3 {
		t.Fatalf("free plan wrong: %+v", got)
	}
	if got := Get("starter"); got.PriceDollars != 19 || got.MonthlyEvents != 50000 || got.RetentionDays != 30 {
		t.Fatalf("starter plan wrong: %+v", got)
	}
	if got := Get("growth"); got.PriceDollars != 49 || got.MonthlyEvents != 500000 || got.RetentionDays != 90 {
		t.Fatalf("growth plan wrong: %+v", got)
	}
	if got := Get("unknown"); got.Name != "free" {
		t.Fatalf("unknown plan should fall back to free, got %+v", got)
	}
	if got := Get(""); got.Name != "free" {
		t.Fatalf("empty plan should fall back to free, got %+v", got)
	}
	if !Valid("starter") || Valid("enterprise") {
		t.Fatal("Valid mismatch")
	}
	if IsPaid("free") || !IsPaid("growth") {
		t.Fatal("IsPaid mismatch")
	}
}

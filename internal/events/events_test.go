package events

import "testing"

func TestValidateSourceName(t *testing.T) {
	if err := ValidateSourceName("stripe"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSourceName("bad/source"); err == nil {
		t.Fatal("slash should be invalid")
	}
	if err := ValidateSourceName(""); err == nil {
		t.Fatal("empty should be invalid")
	}
	if err := ValidatePayloadSize(11, 10); err == nil {
		t.Fatal("oversize should fail")
	}
}

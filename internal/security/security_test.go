package security

import (
	"net"
	"testing"
)

func TestHMACVerify(t *testing.T) {
	body := []byte(`{"type":"x"}`)
	sig := HMACSHA256Hex("secret", body)
	if !VerifySignature("secret", body, sig) {
		t.Fatal("bare hex signature should verify")
	}
	if !VerifySignature("secret", body, "sha256="+sig) {
		t.Fatal("sha256= prefixed signature should verify")
	}
	if VerifySignature("secret", body, HMACSHA256Hex("wrong", body)) {
		t.Fatal("wrong secret must not verify")
	}
	if VerifySignature("secret", []byte("tampered"), sig) {
		t.Fatal("tampered body must not verify")
	}
	if VerifySignature("secret", body, "") {
		t.Fatal("empty signature must not verify")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc", "abc") || ConstantTimeEqual("abc", "abd") || ConstantTimeEqual("abc", "ab") {
		t.Fatal("constant-time compare wrong")
	}
}

func TestNewEventIDUnique(t *testing.T) {
	a, _ := NewEventID()
	b, _ := NewEventID()
	if a == b || len(a) < 10 || a[:4] != "evt_" {
		t.Fatalf("bad event ids: %q %q", a, b)
	}
}

func TestRedactHeaders(t *testing.T) {
	h := RedactHeaders(map[string]string{"Authorization": "Bearer x", "Content-Type": "application/json", "X-Api-Key": "k"})
	if h["Authorization"] != "***REDACTED***" || h["X-Api-Key"] != "***REDACTED***" || h["Content-Type"] != "application/json" {
		t.Fatalf("redaction wrong: %v", h)
	}
	if !IsSensitiveHeader("X-Secret-Token") || IsSensitiveHeader("Content-Type") {
		t.Fatal("sensitive detection wrong")
	}
}

func TestSSRFBlocksPrivate(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:8080/hook",
		"http://localhost:8080/hook",
		"http://10.0.0.5/hook",
		"http://192.168.1.1/hook",
		"http://172.16.0.9/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/hook",
		"http://0.0.0.0/hook",
		"ftp://example.com/hook",
		"http://example.com@127.0.0.1/hook",
	}
	for _, u := range cases {
		if err := ValidateDestinationURL(u, false, nil, nil); err == nil {
			t.Errorf("expected %q to be blocked", u)
		}
	}
}

func TestSSRFAllowsPublic(t *testing.T) {
	if err := ValidateDestinationURL("https://example.com/hook", false, nil, nil); err != nil {
		t.Fatalf("public URL should pass: %v", err)
	}
}

func TestSSRFAllowPrivateOptIn(t *testing.T) {
	if err := ValidateDestinationURL("http://127.0.0.1:8080/hook", true, nil, nil); err != nil {
		t.Fatalf("allowPrivate should permit loopback: %v", err)
	}
}

func TestSSRFTrustedHost(t *testing.T) {
	if err := ValidateDestinationURL("http://internal.corp/hook", false, []string{"internal.corp"}, nil); err != nil {
		t.Fatalf("trusted host should pass: %v", err)
	}
}

// DNS rebinding: hostname resolves to 127.0.0.1 at validation time.
func TestSSRFDNSRebinding(t *testing.T) {
	rebinding := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	if err := ValidateDestinationURL("http://evil.example.com/hook", false, nil, rebinding); err == nil {
		t.Fatal("hostname resolving to loopback must be blocked")
	}
	public := func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	if err := ValidateDestinationURL("http://evil.example.com/hook", false, nil, public); err != nil {
		t.Fatalf("hostname resolving to public IP should pass: %v", err)
	}
}

func TestIsPublicIP(t *testing.T) {
	if IsPublicIP(net.ParseIP("127.0.0.1")) || IsPublicIP(net.ParseIP("10.1.2.3")) || IsPublicIP(net.ParseIP("169.254.169.254")) || IsPublicIP(net.ParseIP("100.64.0.1")) {
		t.Fatal("non-public IP reported public")
	}
	if !IsPublicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public IP reported non-public")
	}
}

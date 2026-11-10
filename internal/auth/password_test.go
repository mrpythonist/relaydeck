package auth

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("password stored in clear")
	}
	if !CheckPassword("correct horse battery staple", hash) {
		t.Fatal("correct password did not verify")
	}
	if CheckPassword("wrong password", hash) {
		t.Fatal("wrong password verified")
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("empty password should fail")
	}
	if !ValidateEmail("aqib@example.com") || ValidateEmail("not-an-email") || ValidateEmail("a@b") {
		t.Fatal("ValidateEmail mismatch")
	}
}

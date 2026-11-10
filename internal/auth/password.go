// Package auth password helpers: bcrypt hashing for hosted accounts.
// Passwords are never stored in clear. The hash includes its own salt;
// only the hash string is stored in the users table.
package auth

import (
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword hashes a password with bcrypt (cost 11). It returns an
// error for an empty password or one longer than bcrypt's 72-byte limit.
func HashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", errors.New("password must not be empty")
	}
	if len(password) > 72 {
		return "", errors.New("password must be 72 bytes or less")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 11)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword reports whether password matches the stored bcrypt hash.
func CheckPassword(password, hash string) bool {
	if password == "" || hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ValidateEmail is a small sanity check for signup/login input.
func ValidateEmail(email string) bool {
	email = strings.TrimSpace(email)
	if len(email) < 3 || len(email) > 254 {
		return false
	}
	at := strings.Index(email, "@")
	return at > 0 && at < len(email)-1 && strings.Contains(email[at:], ".")
}

package auth

import (
	"crypto/sha256"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// ValidatePassword accepts any non-empty password that can be transported
// safely through a line-oriented terminal form. Password bytes are otherwise
// preserved exactly, including spaces.
func ValidatePassword(password string) error {
	if password == "" {
		return errors.New("password is required")
	}
	if strings.ContainsAny(password, "\r\n\x00") {
		return errors.New("password cannot contain a newline or NUL byte")
	}
	return nil
}

// HashPassword pre-hashes with SHA-256 before bcrypt. Besides avoiding
// bcrypt's 72-byte input ceiling, this gives setup and runtime one canonical
// representation for arbitrary UTF-8 passwords.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(password))
	hash, err := bcrypt.GenerateFromPassword(digest[:], bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func ComparePassword(hash, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(password))
	return bcrypt.CompareHashAndPassword([]byte(hash), digest[:])
}

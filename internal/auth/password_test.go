package auth

import (
	"strings"
	"testing"
)

func TestHashAndComparePassword(t *testing.T) {
	t.Parallel()
	for _, password := range []string{
		"a",
		" leading and trailing ",
		"пароль со словами",
		strings.Repeat("long-password-", 20),
	} {
		password := password
		t.Run(password[:1], func(t *testing.T) {
			t.Parallel()
			hash, err := HashPassword(password)
			if err != nil {
				t.Fatalf("HashPassword() error = %v", err)
			}
			if err := ComparePassword(hash, password); err != nil {
				t.Fatalf("ComparePassword() error = %v", err)
			}
			if err := ComparePassword(hash, password+"x"); err == nil {
				t.Fatal("ComparePassword() accepted a different password")
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()
	for _, password := range []string{"", "line\nbreak", "zero\x00byte"} {
		if err := ValidatePassword(password); err == nil {
			t.Errorf("ValidatePassword(%q) unexpectedly succeeded", password)
		}
	}
}

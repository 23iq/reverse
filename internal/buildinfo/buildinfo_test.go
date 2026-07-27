package buildinfo

import "testing"

func TestVersionIsNotEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
	if Version != "v1alpha" {
		t.Fatalf("Version = %q, want v1alpha", Version)
	}
}

package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeServerURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Example.COM":          "https://example.com",
		"https://example.com/": "https://example.com",
		"localhost:8443":       "https://localhost:8443",
		"[::1]:8443":           "https://[::1]:8443",
		"[::1]":                "https://[::1]",
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeServerURL(input)
			if err != nil {
				t.Fatalf("NormalizeServerURL() error = %v", err)
			}
			if got != want {
				t.Fatalf("NormalizeServerURL() = %q, want %q", got, want)
			}
		})
	}
}

func TestNormalizeServerURLRejectsUnsafeValues(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"",
		"http://example.com",
		"https://user:pass@example.com",
		"https://example.com/path",
		"https://example.com?q=1",
	} {
		if _, err := NormalizeServerURL(input); err == nil {
			t.Errorf("NormalizeServerURL(%q) unexpectedly succeeded", input)
		}
	}
}

func TestClientRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	want := Client{
		ServerURL: "https://example.com",
		Password:  "correct horse battery staple",
		LocalHost: "127.0.0.1",
	}
	if err := SaveClient(path, want); err != nil {
		t.Fatalf("SaveClient() error = %v", err)
	}
	got, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient() error = %v", err)
	}
	if got != want {
		t.Fatalf("LoadClient() = %#v, want %#v", got, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("config permissions = %o, want 600", perm)
		}
	}
}

func TestLoadClientNotConfigured(t *testing.T) {
	t.Parallel()
	_, err := LoadClient(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("LoadClient() error = %v, want ErrNotConfigured", err)
	}
}

package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicReplaceAndRollback(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	restore, err := atomicReplace(path, []byte("new"), 0o600)
	if err != nil {
		t.Fatalf("atomicReplace() error = %v", err)
	}
	assertFile(t, path, "new", 0o600)
	if err := restore(); err != nil {
		t.Fatalf("restore() error = %v", err)
	}
	assertFile(t, path, "old", 0o640)
}

func TestAtomicReplaceRollbackRemovesNewFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "new", "Caddyfile")
	restore, err := atomicReplace(path, []byte("new"), 0o644)
	if err != nil {
		t.Fatalf("atomicReplace() error = %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restored new file still exists or returned unexpected error: %v", err)
	}
}

func assertFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Fatalf("content = %q, want %q", data, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Fatalf("mode = %o, want %o", got, mode)
	}
}

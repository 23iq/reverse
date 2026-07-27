package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	t.Setenv("REVERSE_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	paths, err := RuntimePaths()
	if err != nil {
		t.Fatalf("RuntimePaths() error = %v", err)
	}
	return paths
}

func testState(t *testing.T) State {
	t.Helper()
	instanceID, err := NewInstanceID()
	if err != nil {
		t.Fatalf("NewInstanceID() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	return State{
		InstanceID:  instanceID,
		PID:         os.Getpid(),
		StartedAt:   now,
		UpdatedAt:   now,
		PublicURL:   "https://tunnel.example.com",
		LocalTarget: "localhost:3000",
		Status:      "online",
	}
}

func TestStateRoundTripUsesPrivatePermissions(t *testing.T) {
	paths := testPaths(t)
	want := testState(t)
	want.Requests = 7
	want.BytesIn = 123
	want.BytesOut = 456
	if err := WriteState(paths, want); err != nil {
		t.Fatalf("WriteState() error = %v", err)
	}

	got, err := ReadState(paths)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if got.InstanceID != want.InstanceID || got.Requests != 7 || got.BytesIn != 123 || got.BytesOut != 456 {
		t.Fatalf("ReadState() = %#v, want %#v", got, want)
	}
	dirInfo, err := os.Stat(paths.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("runtime directory permissions = %o, want 700", got)
	}
	stateInfo, err := os.Stat(paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("state permissions = %o, want 600", got)
	}
	matches, err := filepath.Glob(filepath.Join(paths.Dir, ".daemon-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary state files were not cleaned: %v", matches)
	}
}

func TestReadStateToleratesAtomicReplacements(t *testing.T) {
	paths := testPaths(t)
	state := testState(t)
	if err := WriteState(paths, state); err != nil {
		t.Fatal(err)
	}

	writerDone := make(chan error, 1)
	go func() {
		for request := uint64(1); request <= 500; request++ {
			next := state
			next.Requests = request
			next.UpdatedAt = time.Now().UTC()
			if err := WriteState(paths, next); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	for {
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatalf("replace daemon state: %v", err)
			}
			if _, err := ReadState(paths); err != nil {
				t.Fatalf("read final daemon state: %v", err)
			}
			return
		default:
			if _, err := ReadState(paths); err != nil {
				t.Fatalf("read during atomic state replacement: %v", err)
			}
		}
	}
}

func TestRuntimePathsNeverChmodsExistingBroadDirectory(t *testing.T) {
	parent := t.TempDir()
	existing := filepath.Join(parent, "shared")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVERSE_RUNTIME_DIR", existing)

	if _, err := RuntimePaths(); err == nil {
		t.Fatal("RuntimePaths() unexpectedly accepted a shared directory")
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing directory permissions changed to %o, want 755", got)
	}
}

func TestStoreFlushesLatestSnapshot(t *testing.T) {
	paths := testPaths(t)
	store, err := NewStore(paths, testState(t))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	store.Update(func(state *State) {
		state.Status = "reconnecting"
		state.Attempt = 3
		state.BytesIn = 999
	})
	if err := store.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	got, err := ReadState(paths)
	if err != nil {
		t.Fatalf("ReadState() error = %v", err)
	}
	if got.Status != "reconnecting" || got.Attempt != 3 || got.BytesIn != 999 {
		t.Fatalf("stored snapshot = %#v", got)
	}
}

func TestLockIsExclusive(t *testing.T) {
	if !Supported() {
		t.Skip("daemon mode is unavailable on this platform")
	}
	paths := testPaths(t)
	first, err := AcquireLock(paths)
	if err != nil {
		t.Fatalf("AcquireLock(first) error = %v", err)
	}
	defer first.Close()

	second, err := AcquireLock(paths)
	if !errors.Is(err, ErrAlreadyRunning) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("AcquireLock(second) error = %v, want ErrAlreadyRunning", err)
	}
}

func TestControlStatusStopAndInstanceValidation(t *testing.T) {
	if !Supported() {
		t.Skip("daemon mode is unavailable on this platform")
	}
	paths := testPaths(t)
	state := testState(t)
	if err := WriteState(paths, state); err != nil {
		t.Fatal(err)
	}
	var stopCalls atomic.Int32
	server, err := ListenControl(paths, state.InstanceID, func() State {
		return state
	}, func() {
		stopCalls.Add(1)
	})
	if err != nil {
		t.Fatalf("ListenControl() error = %v", err)
	}
	defer server.Close()

	got, err := Query(paths)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if got.PID != state.PID || got.InstanceID != state.InstanceID {
		t.Fatalf("Query() = %#v", got)
	}

	reusedPID := state
	reusedPID.InstanceID = "different-process-instance"
	if err := WriteState(paths, reusedPID); err != nil {
		t.Fatal(err)
	}
	if _, err := Stop(paths); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop() with stale identity error = %v, want ErrNotRunning", err)
	}
	if got := stopCalls.Load(); got != 0 {
		t.Fatalf("stale identity stopped live process (%d callbacks)", got)
	}

	if err := WriteState(paths, state); err != nil {
		t.Fatal(err)
	}
	if _, err := Stop(paths); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for stopCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("stop callbacks = %d, want 1", got)
	}
}

func TestReadStateWithoutFileIsNotRunning(t *testing.T) {
	paths := testPaths(t)
	if _, err := ReadState(paths); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("ReadState() error = %v, want ErrNotRunning", err)
	}
}

func TestReadStateRejectsSymlink(t *testing.T) {
	paths := testPaths(t)
	target := filepath.Join(paths.Dir, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.State); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := ReadState(paths); err == nil {
		t.Fatal("ReadState() unexpectedly followed a symlink")
	}
}

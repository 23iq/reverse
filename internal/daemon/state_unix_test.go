//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadStateRejectsFIFOWithoutOpeningIt(t *testing.T) {
	paths := testPaths(t)
	if err := unix.Mkfifo(paths.State, 0o600); err != nil {
		t.Skipf("mkfifo is unavailable: %v", err)
	}
	if _, err := ReadState(paths); err == nil {
		t.Fatal("ReadState() unexpectedly accepted a FIFO")
	}
}

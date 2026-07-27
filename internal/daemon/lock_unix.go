//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func Supported() bool {
	return true
}

func lockFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrAlreadyRunning
	}
	if err != nil {
		return fmt.Errorf("lock daemon state: %w", err)
	}
	return nil
}

func unlockFile(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("unlock daemon state: %w", err)
	}
	return nil
}

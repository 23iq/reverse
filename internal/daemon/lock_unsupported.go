//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package daemon

import "os"

func Supported() bool {
	return false
}

func lockFile(_ *os.File) error {
	return ErrUnsupported
}

func unlockFile(_ *os.File) error {
	return nil
}

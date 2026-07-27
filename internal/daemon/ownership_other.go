//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package daemon

import "os"

func ownedByCurrentUser(_ os.FileInfo) bool {
	return true
}

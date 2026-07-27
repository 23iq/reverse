//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"os"
	"syscall"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Geteuid())
}

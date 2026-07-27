//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"net"
	"time"
)

func listenControl(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

func dialControl(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

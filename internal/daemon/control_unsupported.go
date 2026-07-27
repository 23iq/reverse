//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package daemon

import (
	"net"
	"time"
)

func listenControl(_ string) (net.Listener, error) {
	return nil, ErrUnsupported
}

func dialControl(_ string, _ time.Duration) (net.Conn, error) {
	return nil, ErrUnsupported
}

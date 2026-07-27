//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package main

import (
	"os/exec"

	"github.com/23iq/reverse/internal/daemon"
)

func configureDetachedProcess(_ *exec.Cmd) error {
	return daemon.ErrUnsupported
}

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"os/exec"
	"syscall"
)

func configureDetachedProcess(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}

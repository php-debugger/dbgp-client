//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detach runs cmd in a session of its own, so it outlives the terminal
// that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// terminate asks the server to stop, which it does cleanly.
func terminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

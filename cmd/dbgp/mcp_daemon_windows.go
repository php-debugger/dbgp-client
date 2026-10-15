package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS: the process has no console.
const detachedProcess = 0x00000008

// detach runs cmd without a console and outside the console's process
// group, so it outlives the terminal that started it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
		HideWindow:    true,
	}
}

// terminate stops the server. Windows cannot signal another process, so it
// is killed.
func terminate(p *os.Process) error {
	return p.Kill()
}

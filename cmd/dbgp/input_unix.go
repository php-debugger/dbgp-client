//go:build !windows

package main

import "syscall"

// privateFiles makes the files this process creates from now on readable
// by the user only, whatever mode their creator asks for.
func privateFiles() {
	syscall.Umask(0o077)
}

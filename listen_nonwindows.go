//go:build !windows

package dbgp

import "syscall"

// listenControl is only needed on Windows.
var listenControl func(network, address string, c syscall.RawConn) error

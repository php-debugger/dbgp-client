//go:build windows

package dbgp

import "syscall"

// soExclusiveAddrUse is Windows' SO_EXCLUSIVEADDRUSE, which syscall lacks.
const soExclusiveAddrUse = ^syscall.SO_REUSEADDR

// listenControl makes the addresses dbgp listens on its own. Windows
// otherwise lets dbgp listen on every address while another program
// listens on one of them, which then gets the connections to it, and lets
// another program listen on one of dbgp's addresses later.
func listenControl(_, _ string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		err = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
	}); cerr != nil {
		return cerr
	}
	return err
}

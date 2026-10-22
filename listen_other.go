//go:build !darwin

package dbgp

import "net"

// shadowedAddr is only needed on macOS: Linux refuses to listen on every
// address when another program listens on one of them, and so does
// Windows with the exclusive addresses dbgp asks for.
func shadowedAddr(net.Listener) string { return "" }

//go:build !darwin

package dbgp

import "net"

// shadowedAddr is only needed on macOS: Linux refuses to listen on every
// address when another program listens on one of them.
func shadowedAddr(net.Listener) string { return "" }

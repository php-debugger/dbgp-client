//go:build darwin

package dbgp

import (
	"errors"
	"net"
	"strconv"
	"syscall"
)

// shadowedAddr finds an address another program listens on that l, which
// listens on every address, would share. Linux refuses to listen on every
// address then, but macOS does not, and connections to that address, such
// as PHP's to 127.0.0.1:9003, reach the other program instead.
// Binding to an address on l's port fails only when another socket is bound
// to that exact address: l itself does not get in the way.
func shadowedAddr(l net.Listener) string {
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsUnspecified() {
		return ""
	}
	// Listening on IPv6's :: takes IPv4 addresses too; Go listens there
	// even for 0.0.0.0, unless only IPv4 is available.
	ipv4Only := addr.IP.To4() != nil
	for _, ip := range localIPs() {
		if ipv4Only && ip.To4() == nil {
			continue
		}
		if bindInUse(ip, addr.Port) {
			return net.JoinHostPort(ip.String(), strconv.Itoa(addr.Port))
		}
	}
	return ""
}

// bindInUse reports whether binding a TCP socket to ip and port fails
// because another socket is bound there. The socket is only bound, never
// listening, so no connection reaches it.
func bindInUse(ip net.IP, port int) bool {
	family := syscall.AF_INET6
	var sa syscall.Sockaddr
	if ip4 := ip.To4(); ip4 != nil {
		family = syscall.AF_INET
		sa4 := &syscall.SockaddrInet4{Port: port}
		copy(sa4.Addr[:], ip4)
		sa = sa4
	} else {
		sa6 := &syscall.SockaddrInet6{Port: port}
		copy(sa6.Addr[:], ip.To16())
		sa = sa6
	}
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	// As Go sets on its listeners, so that only an exact match conflicts.
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return false
	}
	return errors.Is(syscall.Bind(fd, sa), syscall.EADDRINUSE)
}

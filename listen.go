package dbgp

import (
	"context"
	"net"
)

// listenTCP listens on addr for PHP.
func listenTCP(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: listenControl}
	return lc.Listen(context.Background(), "tcp", addr)
}

// localIPs returns this machine's addresses, except link-local ones,
// always including the loopback addresses.
func localIPs() []net.IP {
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLinkLocalUnicast() || ipNet.IP.IsLoopback() {
			continue
		}
		ips = append(ips, ipNet.IP)
	}
	return ips
}

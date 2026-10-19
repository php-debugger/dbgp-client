package dbgp

import (
	"net"
	"strings"
	"testing"
)

// macOS lets a server listen on every address while another program
// listens on one of them, which would then get the connections to it:
// StartListening refuses, as Linux does.
func TestListenRefusesShadowedAddress(t *testing.T) {
	hosts := []string{"127.0.0.1", "::1"}
	for _, ip := range localIPs() {
		if !ip.IsLoopback() && ip.To4() != nil {
			hosts = append(hosts, ip.String()) // a LAN address, as PHP in a container may use
			break
		}
	}
	for _, host := range hosts {
		other, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(other.Addr().String())
		srv, err := NewServer(Config{Addr: ":" + port})
		if err != nil {
			t.Fatal(err)
		}
		err = srv.StartListening()
		want := "another program is listening on " + net.JoinHostPort(host, port)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("with %s taken: StartListening = %v, want %q", host, err, want)
		}
		if srv.Listening() {
			t.Errorf("with %s taken: listening", host)
		}

		// Once the other program is gone, the port is free again.
		_ = other.Close()
		if err := srv.StartListening(); err != nil {
			t.Errorf("after %s was freed: %v", host, err)
		}
		_ = srv.Close()
	}
}

// Go listens on IPv6 as well for 0.0.0.0, so a program on ::1 shares it too.
func TestListenOnIPv4AnyRefusesShadowedIPv6(t *testing.T) {
	other, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	defer other.Close()
	_, port, _ := net.SplitHostPort(other.Addr().String())
	srv, err := NewServer(Config{Addr: "0.0.0.0:" + port})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	want := "another program is listening on [::1]:" + port
	if err := srv.StartListening(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("StartListening = %v, want %q", err, want)
	}
}

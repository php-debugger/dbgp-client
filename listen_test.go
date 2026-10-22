package dbgp

import (
	"net"
	"runtime"
	"strings"
	"testing"
)

// shadowedError returns what StartListening's error says when another
// program listens on addr: dbgp's own message on macOS, which would let
// dbgp listen, and "" elsewhere, where the system refuses by itself.
func shadowedError(addr string) string {
	if runtime.GOOS == "darwin" {
		return "another program is listening on " + addr
	}
	return ""
}

// StartListening refuses to listen on every address while another program
// listens on one of them, which would then get the connections to it.
// Linux refuses by itself; macOS does not, so dbgp checks.
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
		want := shadowedError(net.JoinHostPort(host, port))
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
	want := shadowedError("[::1]:" + port)
	if err := srv.StartListening(); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("StartListening = %v, want %q", err, want)
	}
}

// dbgp can listen on its port again at once after it stops listening, even
// while connections it closed itself still linger on the port (TIME_WAIT),
// as after debugging a few requests.
func TestListenAgainAfterClosingConnections(t *testing.T) {
	for _, host := range []string{"", "127.0.0.1"} {
		l, err := listenTCP(net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(l.Addr().String())
		client, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close() // dbgp's side closes first, so it lingers
		_, _ = client.Read(make([]byte, 1))
		_ = client.Close()
		_ = l.Close()

		l, err = listenTCP(net.JoinHostPort(host, port))
		if err != nil {
			t.Fatalf("listening on %q again: %v", host, err)
		}
		_ = l.Close()
	}
}

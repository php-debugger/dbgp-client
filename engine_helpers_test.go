package dbgp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Helpers for tests that run against a real PHP with a DBGp debug engine:
// PHP Debugger or Xdebug. Both accept the xdebug.* ini settings used here.
// They speak DBGp directly (spec-conformant framing) rather than through
// Client, so fixture capture does not depend on the code under test.

const debuggeeScript = "testdata/php/basic.php"

// Line numbers in testdata/php/basic.php.
const (
	lineAddBody   = 24 // $sum = $a + $b;
	lineAddReturn = 25 // return $sum;
	lineCallAdd   = 36 // $x = add(2, 3);
	lineLoopBody  = 39 // $y = $i * 2;
)

// knownBug skips a test that documents a known defect, unless DBGP_KNOWN_BUGS=1.
// Remove the call once the defect is fixed.
func knownBug(t *testing.T, id, summary string) {
	t.Helper()
	if os.Getenv("DBGP_KNOWN_BUGS") == "" {
		t.Skipf("known bug %s: %s (set DBGP_KNOWN_BUGS=1 to run)", id, summary)
	}
}

// requireDebugEngine skips the test unless php has a DBGp debug engine loaded:
// PHP Debugger (php_debugger) or Xdebug. With DBGP_REQUIRE_ENGINE=1 (as in CI)
// a missing engine fails the test instead.
func requireDebugEngine(t *testing.T) {
	t.Helper()
	skip := t.Skip
	if os.Getenv("DBGP_REQUIRE_ENGINE") != "" {
		skip = t.Fatal
	}
	if testing.Short() {
		skip("skipping debug engine test in -short mode")
	}
	php, err := exec.LookPath("php")
	if err != nil {
		skip("php not found in PATH")
	}
	loaded := `exit(extension_loaded("php_debugger") || extension_loaded("xdebug") ? 0 : 1);`
	if err := exec.Command(php, "-r", loaded).Run(); err != nil {
		skip("php has neither PHP Debugger nor Xdebug loaded")
	}
}

// debuggeePath returns the absolute path of the debuggee script.
func debuggeePath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(debuggeeScript)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// startDebuggee runs the debuggee script with Xdebug connecting to port.
// The process is killed and reaped at test cleanup.
func startDebuggee(t *testing.T, port int) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command("php",
		"-dxdebug.mode=debug",
		"-dxdebug.start_with_request=yes",
		"-dxdebug.client_host=127.0.0.1",
		fmt.Sprintf("-dxdebug.client_port=%d", port),
		"-dxdebug.log_level=0",
		debuggeePath(t))
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start php: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &out
}

// rawSession is a minimal, spec-conformant DBGp IDE side.
type rawSession struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
	nextID int
	init   []byte
}

// newRawSession listens, starts the debuggee and reads its init packet.
func newRawSession(t *testing.T) *rawSession {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	startDebuggee(t, ln.Addr().(*net.TCPAddr).Port)

	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept Xdebug connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	s := &rawSession{t: t, conn: conn, reader: bufio.NewReader(conn)}
	s.init = s.read()
	return s
}

// read reads one engine packet: length NUL xml NUL.
func (s *rawSession) read() []byte {
	s.t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	data, err := readEnginePacket(s.reader)
	if err != nil {
		s.t.Fatalf("read packet: %v", err)
	}
	return data
}

// readEnginePacket reads a single engine-to-IDE packet.
func readEnginePacket(r *bufio.Reader) ([]byte, error) {
	lenStr, err := r.ReadString(0)
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSuffix(lenStr, "\x00"))
	if err != nil {
		return nil, fmt.Errorf("bad length %q: %w", lenStr, err)
	}
	data := make([]byte, n+1)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	if data[n] != 0 {
		return nil, fmt.Errorf("missing NUL terminator")
	}
	return data[:n], nil
}

// sendRaw writes bytes to the engine unmodified.
func (s *rawSession) sendRaw(raw string) {
	s.t.Helper()
	if _, err := s.conn.Write([]byte(raw)); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

// command sends "name args -i N [-- base64(data)]" and returns the matching
// response, plus any stream/notify packets that arrived before it.
func (s *rawSession) command(name, args string, data ...string) (resp []byte, other [][]byte) {
	s.t.Helper()
	s.nextID++
	cmd := name
	if args != "" {
		cmd += " " + args
	}
	cmd += fmt.Sprintf(" -i %d", s.nextID)
	if len(data) > 0 {
		cmd += " -- " + base64.StdEncoding.EncodeToString([]byte(data[0]))
	}
	s.sendRaw(cmd + "\x00")

	want := fmt.Sprintf(`transaction_id="%d"`, s.nextID)
	for {
		p := s.read()
		if bytes.Contains(p, []byte("<response")) && bytes.Contains(p, []byte(want)) {
			return p, other
		}
		other = append(other, p)
	}
}

var transactionIDAttr = regexp.MustCompile(`transaction_id="[^"]*"`)

// attr extracts the first value of an XML attribute from a packet.
func attr(packet []byte, name string) string {
	m := regexp.MustCompile(`\s` + regexp.QuoteMeta(name) + `="([^"]*)"`).FindSubmatch(packet)
	if m == nil {
		return ""
	}
	return string(m[1])
}

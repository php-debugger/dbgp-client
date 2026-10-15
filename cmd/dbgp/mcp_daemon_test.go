package main

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// daemonEnv points dbgp's state directory, and this test's, at a temporary
// directory.
func daemonEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "appdata"))
}

// runDbgp runs the binary and returns its exit code and output.
func runDbgp(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil && cmd.ProcessState == nil {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

var runningAt = regexp.MustCompile(`running at (\S+) \(pid (\d+)\)`)

// startTestDaemon starts the server in the background on a free port and
// returns its URL. It is stopped at cleanup.
func startTestDaemon(t *testing.T, bin string, args ...string) string {
	t.Helper()
	code, out := runDbgp(t, bin, append([]string{"mcp", "start", "-http", "127.0.0.1:0"}, args...)...)
	m := runningAt.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("dbgp mcp: exit %d\n%s", code, out)
	}
	t.Cleanup(func() { runDbgp(t, bin, "mcp", "stop") })
	return m[1]
}

func TestMCPDaemon(t *testing.T) {
	bin := buildBinary(t)
	daemonEnv(t)

	url := startTestDaemon(t, bin)
	if !strings.Contains(runDbgpOut(t, bin, "mcp", "status"), url) {
		t.Error("dbgp mcp status does not show the URL")
	}
	// Starting it again leaves the running one alone.
	if code, out := runDbgp(t, bin, "mcp", "start"); code != 0 || !strings.Contains(out, "already running at "+url) {
		t.Errorf("second start: exit %d\n%s", code, out)
	}

	// Agents connect over HTTP, and can come and go.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for range 2 {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var status statusOutput
		callTool(t, cs, "status", nil, &status)
		if status.Listening {
			t.Error("listening before the listen tool")
		}
		_ = cs.Close()
	}

	if code, out := runDbgp(t, bin, "mcp", "stop"); code != 0 || !strings.Contains(out, "stopped") {
		t.Errorf("stop: exit %d\n%s", code, out)
	}
	if answering(url) {
		t.Error("still answering after stop")
	}
	if code, out := runDbgp(t, bin, "mcp", "status"); code != 1 || !strings.Contains(out, "not running") {
		t.Errorf("status after stop: exit %d\n%s", code, out)
	}
	if code, out := runDbgp(t, bin, "mcp", "stop"); code != 0 || !strings.Contains(out, "not running") {
		t.Errorf("stop when not running: exit %d\n%s", code, out)
	}
	if st, err := readState(); st != nil || err != nil {
		t.Errorf("state after stop = %+v, %v", st, err)
	}
}

// runDbgpOut runs the binary, which must succeed, and returns its output.
func runDbgpOut(t *testing.T, bin string, args ...string) string {
	t.Helper()
	code, out := runDbgp(t, bin, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d\n%s", args, code, out)
	}
	return out
}

func TestMCPDaemonDoesNotStart(t *testing.T) {
	bin := buildBinary(t)
	daemonEnv(t)
	// The port is taken by something else.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	code, out := runDbgp(t, bin, "mcp", "start", "-http", ln.Addr().String())
	if code != 1 || !strings.Contains(out, "did not start") || !strings.Contains(out, ln.Addr().String()) {
		t.Errorf("start on a taken port: exit %d\n%s", code, out)
	}
}

func TestMCPDaemonStaleState(t *testing.T) {
	daemonEnv(t)
	// A server that died without removing its state file.
	if err := writeState(daemonState{PID: 999999, URL: "http://127.0.0.1:1/mcp"}); err != nil {
		t.Fatal(err)
	}
	if st, err := running(); st != nil || err != nil {
		t.Errorf("running() = %+v, %v; want none", st, err)
	}
	if st, _ := readState(); st != nil {
		t.Errorf("stale state kept: %+v", st)
	}
}

func TestMCPArguments(t *testing.T) {
	for _, args := range [][]string{
		{"restart"},
		{"stop", "-http", "127.0.0.1:0"},
		{"start", "extra"},
	} {
		if code := runMCP(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// An agent that starts dbgp mcp while the background server runs uses it:
// with its options, and its state, shared with other agents.
func TestMCPBridge(t *testing.T) {
	bin := buildBinary(t)
	daemonEnv(t)
	url := startTestDaemon(t, bin, "-no-eval")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, bin, "mcp", "-listen")
	cmd.Stderr = &stderr
	bridged, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()

	list, err := bridged.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if tool.Name == "eval" {
			t.Error("eval offered: the background server was started with -no-eval")
		}
	}
	var added addBreakpointOutput
	callTool(t, bridged, "add_breakpoint", map[string]any{"file": "/app/index.php", "line": 3}, &added)
	var status statusOutput
	callTool(t, direct, "status", nil, &status)
	if len(status.Breakpoints) != 1 || status.Listening {
		t.Errorf("status seen by another agent = %+v; want the breakpoint, and -listen ignored", status)
	}

	if err := bridged.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if !strings.Contains(stderr.String(), "using the background server at "+url) || !strings.Contains(stderr.String(), "-listen ignored") {
		t.Errorf("stderr: %q", stderr.String())
	}
	// The background server carries on after the agent leaves.
	callTool(t, direct, "status", nil, &status)
}

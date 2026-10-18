package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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
		cs := connectDirect(ctx, t, url)
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
	direct := connectDirect(ctx, t, url)
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

// connectDirect connects to the background server over HTTP, with the token
// from the state file, as dbgp mcp does.
func connectDirect(ctx context.Context, t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	st, err := readState()
	if err != nil || st == nil {
		t.Fatalf("state = %+v, %v", st, err)
	}
	client, err := tokenClient(url, st.Token)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// Only requests with the token from the state file reach the server: other
// users on the machine cannot read the file, so they cannot use it.
func TestMCPDaemonRequiresToken(t *testing.T) {
	bin := buildBinary(t)
	daemonEnv(t)
	url := startTestDaemon(t, bin)
	st, err := readState()
	if err != nil || st == nil || st.Token == "" {
		t.Fatalf("state = %+v, %v; want a token", st, err)
	}

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	for name, auth := range map[string]string{
		"no token":     "",
		"wrong token":  "Bearer " + strings.Repeat("A", len(st.Token)),
		"bare token":   st.Token,
		"other scheme": "Basic " + st.Token,
	} {
		req, err := http.NewRequest("POST", url, strings.NewReader(initialize))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, MaxRetries: -1}, nil)
	if err == nil {
		t.Error("an agent without the token connected")
	}
	cs := connectDirect(ctx, t, url)
	_ = cs.Close()

	if strings.Contains(readLog(), st.Token) {
		t.Error("the log shows the token")
	}
	if runtime.GOOS != "windows" {
		path, _ := statePath()
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("state file: %v, %v; want mode 0600", info.Mode().Perm(), err)
		}
	}
}

// A state file left readable by others is replaced by one only the user
// can read, since it holds the token.
func TestWriteStateIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no permission bits; LocalAppData is private to the user")
	}
	daemonEnv(t)
	path, err := statePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := daemonState{PID: 1, URL: "http://127.0.0.1:1/mcp", Token: "secret"}
	if err := writeState(want); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file: %v, %v; want mode 0600", info.Mode().Perm(), err)
	}
	if got, err := readState(); err != nil || got == nil || *got != want {
		t.Errorf("readState = %+v, %v; want %+v", got, err, want)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("state directory has %d entries, want only the state file", len(entries))
	}
}

// The token goes to the background server only, not to other hosts, e.g.
// after a redirect.
func TestTokenClientSendsTokenOnlyToItsServer(t *testing.T) {
	var got []string
	record := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = append(got, req.Header.Get("Authorization"))
	})
	server := httptest.NewServer(record)
	defer server.Close()
	other := httptest.NewServer(record)
	defer other.Close()

	client, err := tokenClient(server.URL+"/mcp", "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{server.URL + "/mcp", other.URL + "/mcp"} {
		resp, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if want := []string{"Bearer secret", ""}; !slices.Equal(got, want) {
		t.Errorf("Authorization headers = %q, want %q", got, want)
	}
}

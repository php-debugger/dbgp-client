package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
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
	"sync"
	"syscall"
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
	code, out := runDbgp(t, bin, append([]string{"mcp", "start", "-mcp-port", "0"}, args...)...)
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
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("URL %s: want the server on 127.0.0.1", url)
	}
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

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	code, out := runDbgp(t, bin, "mcp", "start", "-mcp-port", port)
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
	daemonEnv(t)
	for _, args := range [][]string{
		{"restart"},
		{"stop", "-mcp-port", "0"},
		{"start", "extra"},
		{"start", "-mcp-port", "-1"},
		{"start", "-mcp-port", "65536"},
		{"start", "-mcp-port", "http"},
		{"start", "-php-port", "65536"},
		// Agents connect on 127.0.0.1 only, and PHP on every address.
		{"start", "-http", "0.0.0.0:9004"},
		{"start", "-addr", "127.0.0.1:9003"},
		{serveCommand, "-mcp-port", "-1"},
	} {
		if code := runMCP(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if st, err := readState(); st != nil || err != nil {
		t.Errorf("state = %+v, %v; want no server started", st, err)
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
	callTool(t, bridged, "listen", nil, &listenOutput{})

	if err := bridged.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	// Its dbgp mcp told the server the agent left, which counts as unlisten.
	callTool(t, direct, "status", nil, &status)
	if status.Listening {
		t.Error("still listening after the only agent that called listen left")
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
	client, err := tokenClient(url, st.Token, rand.Text())
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

// The token and agent id go to the background server only, not to other
// hosts, e.g. after a redirect.
func TestTokenClientSendsTokenOnlyToItsServer(t *testing.T) {
	var got []string
	record := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got = append(got, req.Header.Get("Authorization")+"|"+req.Header.Get(agentHeader))
	})
	server := httptest.NewServer(record)
	defer server.Close()
	other := httptest.NewServer(record)
	defer other.Close()

	client, err := tokenClient(server.URL+"/mcp", "secret", "agent-1")
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
	if want := []string{"Bearer secret|agent-1", "|"}; !slices.Equal(got, want) {
		t.Errorf("Authorization|Dbgp-Agent headers = %q, want %q", got, want)
	}
}

func TestIdentityProof(t *testing.T) {
	base := identityProof("token", "nonce-0123456789", 42)
	if identityProof("token", "nonce-0123456789", 42) != base {
		t.Error("the same challenge gave another proof")
	}
	for name, other := range map[string]string{
		"token": identityProof("other", "nonce-0123456789", 42),
		"nonce": identityProof("token", "nonce-9876543210", 42),
		"pid":   identityProof("token", "nonce-0123456789", 43),
	} {
		if other == base {
			t.Errorf("another %s gave the same proof", name)
		}
	}
}

// impostor is a server that took the port of a background server that
// died. It records every Authorization header it is sent.
type impostor struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newImpostor(t *testing.T, handler http.HandlerFunc) *impostor {
	t.Helper()
	imp := &impostor{}
	imp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		imp.mu.Lock()
		imp.auths = append(imp.auths, req.Header.Get("Authorization"))
		imp.mu.Unlock()
		handler(w, req)
	}))
	t.Cleanup(imp.Close)
	return imp
}

// sawToken reports whether the impostor was sent token.
func (imp *impostor) sawToken(token string) bool {
	imp.mu.Lock()
	defer imp.mu.Unlock()
	for _, a := range imp.auths {
		if strings.Contains(a, token) {
			return true
		}
	}
	return false
}

// A server that answers at the recorded URL is used only if it proves it is
// the one recorded; otherwise its state file is stale.
func TestMCPDaemonImpostor(t *testing.T) {
	const token = "secret-token"
	pid := os.Getpid()
	for name, handler := range map[string]http.HandlerFunc{
		"no identify": http.NotFound,
		"wrong proof": func(w http.ResponseWriter, req *http.Request) {
			_ = json.NewEncoder(w).Encode(identity{PID: pid, Proof: identityProof("guess", req.URL.Query().Get("nonce"), pid)})
		},
		"other pid": func(w http.ResponseWriter, req *http.Request) {
			// Right for its own pid, which is not the recorded one.
			_ = json.NewEncoder(w).Encode(identity{PID: pid + 1, Proof: identityProof(token, req.URL.Query().Get("nonce"), pid+1)})
		},
		"redirect": func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "http://127.0.0.1:1/identify", http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			daemonEnv(t)
			imp := newImpostor(t, handler)
			if err := writeState(daemonState{PID: pid, URL: imp.URL + "/mcp", Token: token}); err != nil {
				t.Fatal(err)
			}
			if st, err := running(); st != nil || !errors.Is(err, errNotOurs) {
				t.Errorf("running() = %+v, %v; want errNotOurs", st, err)
			}
			if st, _ := readState(); st != nil {
				t.Errorf("stale state kept: %+v", st)
			}
			if imp.sawToken(token) {
				t.Error("the impostor was sent the token")
			}
		})
	}
}

// The commands do not act on an impostor: stop does not signal the recorded
// process, and agents are served by dbgp mcp itself.
func TestMCPDaemonImpostorCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sleep and signal 0")
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep command")
	}
	bin := buildBinary(t)
	const token = "secret-token"
	// The recorded process: stop must leave it alone.
	victim := exec.Command(sleepPath, "60")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = victim.Process.Kill(); _ = victim.Wait() })
	imp := newImpostor(t, http.NotFound)
	record := func() {
		t.Helper()
		if err := writeState(daemonState{PID: victim.Process.Pid, URL: imp.URL + "/mcp", Token: token}); err != nil {
			t.Fatal(err)
		}
	}
	daemonEnv(t)

	record()
	if code, out := runDbgp(t, bin, "mcp", "stop"); code != 1 || !strings.Contains(out, "something other than the dbgp MCP server") {
		t.Errorf("stop: exit %d\n%s", code, out)
	}
	if err := victim.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("stop signalled the recorded process: %v", err)
	}
	record()
	if code, out := runDbgp(t, bin, "mcp", "status"); code != 1 || !strings.Contains(out, "something other than the dbgp MCP server") {
		t.Errorf("status: exit %d\n%s", code, out)
	}

	record()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, bin, "mcp")
	cmd.Stderr = &stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var status statusOutput
	callTool(t, cs, "status", nil, &status)
	_ = cs.Close()
	if !strings.Contains(stderr.String(), "something other than the dbgp MCP server") {
		t.Errorf("stderr: %q", stderr.String())
	}
	if imp.sawToken(token) {
		t.Error("the impostor was sent the token")
	}
}

// The real server proves who it is without giving away its token.
func TestMCPDaemonIdentify(t *testing.T) {
	bin := buildBinary(t)
	daemonEnv(t)
	url := startTestDaemon(t, bin)
	st, err := readState()
	if err != nil || st == nil {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if !verify(st) {
		t.Fatal("the real server failed verify")
	}
	identifyURL := strings.TrimSuffix(url, "/mcp") + identifyPath
	resp, err := http.Get(identifyURL + "?nonce=short")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("short nonce: status %d, want 400", resp.StatusCode)
	}
	const nonce = "0123456789abcdef0123"
	resp, err = http.Get(identifyURL + "?nonce=" + nonce)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var id identity
	if err := json.Unmarshal(body, &id); err != nil || id.PID != st.PID || id.Proof != identityProof(st.Token, nonce, st.PID) {
		t.Errorf("identify = %s (%v)", body, err)
	}
	if strings.Contains(string(body), st.Token) {
		t.Error("identify gave away the token")
	}
}

// Each dbgp mcp passing an agent on to the background server identifies
// it as an agent of its own.
func TestMCPBridgesAreSeparateAgents(t *testing.T) {
	requireEngine(t)
	bin := buildBinary(t)
	daemonEnv(t)
	startTestDaemon(t, bin, "-php-port", "0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var agents []*mcp.ClientSession
	for range 2 {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
			Connect(ctx, &mcp.CommandTransport{Command: exec.CommandContext(ctx, bin, "mcp")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		agents = append(agents, cs)
	}

	var listening listenOutput
	callTool(t, agents[0], "listen", nil, &listening)
	startPHP(t, listening.Port, "-r", `echo "hi\n";`)
	var waited waitForSessionOutput
	callTool(t, agents[0], "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected {
		t.Fatal("PHP did not connect")
	}
	if msg := callToolError(t, agents[1], "continue", nil); !strings.Contains(msg, "no session") {
		t.Errorf("the second agent's continue acted on the first agent's session: %q", msg)
	}
	var st stateOutput
	callTool(t, agents[0], "stop", nil, &st)
}

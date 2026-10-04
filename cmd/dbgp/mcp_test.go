package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// newTestMCP connects an MCP client to a dbgp MCP server on a free port that
// is not listening yet.
func newTestMCP(t *testing.T) (*mcp.ClientSession, *dbgp.Server) {
	t.Helper()
	srv, err := dbgp.NewServer(dbgp.Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := newMCPServer(srv).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, srv
}

// callTool calls a tool, fails the test on a tool error, and decodes the
// structured result into out.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, resultText(res))
	}
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("%s: decode %s: %v", name, data, err)
	}
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestMCPTools(t *testing.T) {
	cs, _ := newTestMCP(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
	}
	for _, name := range []string{"status", "listen", "unlisten"} {
		if got[name] == nil {
			t.Errorf("tool %s missing", name)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d tools, want 3", len(got))
	}
	if a := got["status"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Error("status is not marked read-only")
	}
	if got["status"].OutputSchema == nil {
		t.Error("status has no output schema")
	}
	for _, want := range []string{"listen", "php -v", "with PHP Debugger", "do not set xdebug.mode"} {
		if !strings.Contains(cs.InitializeResult().Instructions, want) {
			t.Errorf("instructions lack %q: %s", want, cs.InitializeResult().Instructions)
		}
	}
}

func TestMCPStatusBeforeListening(t *testing.T) {
	cs, _ := newTestMCP(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "status"})
	if err != nil {
		t.Fatal(err)
	}
	// Empty lists are [], not null, so agents need not special-case them.
	text := resultText(res)
	for _, want := range []string{`"listening":false`, `"address":"127.0.0.1:0"`, `"sessions":[]`, `"breakpoints":[]`, `"pathMappings":[]`} {
		if !strings.Contains(text, want) {
			t.Errorf("status text lacks %s: %s", want, text)
		}
	}
}

func TestMCPListenAndUnlisten(t *testing.T) {
	cs, srv := newTestMCP(t)
	refused := func(addr string) bool {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	}

	var listened listenOutput
	callTool(t, cs, "listen", nil, &listened)
	if !listened.Listening || listened.Address != srv.Addr() || refused(listened.Address) {
		t.Fatalf("listen = %+v; server listening %v", listened, srv.Listening())
	}
	callTool(t, cs, "listen", nil, &listened)
	if !listened.Listening {
		t.Error("second listen reported not listening")
	}

	var status statusOutput
	callTool(t, cs, "status", nil, &status)
	if !status.Listening || status.Address != listened.Address {
		t.Errorf("status after listen = %+v", status)
	}

	var unlistened unlistenOutput
	callTool(t, cs, "unlisten", nil, &unlistened)
	if unlistened.Listening || srv.Listening() || !refused(listened.Address) {
		t.Errorf("after unlisten: %+v, server listening %v", unlistened, srv.Listening())
	}
	callTool(t, cs, "status", nil, &status)
	if status.Listening {
		t.Error("status still listening after unlisten")
	}
}

func TestMCPStatusShowsBreakpointsAndMappings(t *testing.T) {
	cs, srv := newTestMCP(t)
	if _, err := srv.AddBreakpoint(dbgp.Breakpoint{Type: dbgp.BreakpointConditional, File: "/app/a.php", Line: 3, Condition: "$x > 1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.AddBreakpoint(dbgp.Breakpoint{Type: dbgp.BreakpointException, Exception: "RuntimeException"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.AddPathMapping(dbgp.PathMapping{Local: "/home/me/app", Remote: "/var/www/html"}); err != nil {
		t.Fatal(err)
	}
	var status statusOutput
	callTool(t, cs, "status", nil, &status)
	want := []breakpointInfo{
		{ID: 1, Type: "conditional", File: "/app/a.php", Line: 3, Condition: "$x > 1"},
		{ID: 2, Type: "exception", Exception: "RuntimeException"},
	}
	if fmt.Sprint(status.Breakpoints) != fmt.Sprint(want) {
		t.Errorf("breakpoints = %+v, want %+v", status.Breakpoints, want)
	}
	if len(status.PathMappings) != 1 || status.PathMappings[0] != (pathMapping{Local: "/home/me/app", Remote: "/var/www/html"}) {
		t.Errorf("pathMappings = %+v", status.PathMappings)
	}
}

// A real PHP session shows up in status once dbgp listens.
func TestMCPStatusShowsSession(t *testing.T) {
	requireEngine(t)
	cs, srv := newTestMCP(t)
	callTool(t, cs, "listen", nil, &listenOutput{})
	script, err := filepath.Abs("../../testdata/php/basic.php")
	if err != nil {
		t.Fatal(err)
	}
	// Follow the listen hint: PHP Debugger needs only the address.
	args := []string{fmt.Sprintf("-dxdebug.client_port=%d", srv.Port()), "-dxdebug.client_host=127.0.0.1", "-ddisplay_errors=0", script}
	if v, _ := exec.Command("php", "-dxdebug.mode=off", "-v").Output(); !strings.Contains(string(v), "with PHP Debugger") {
		args = append([]string{"-dxdebug.mode=debug", "-dxdebug.start_with_request=yes"}, args...)
	}
	php := exec.Command("php", args...)
	if err := php.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = php.Process.Kill(); _ = php.Wait() })

	var status statusOutput
	deadline := time.Now().Add(10 * time.Second)
	for len(status.Sessions) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		callTool(t, cs, "status", nil, &status)
	}
	if len(status.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want one", status.Sessions)
	}
	if s := status.Sessions[0]; s.ID != 1 || s.Script != script || s.Status != dbgp.StatusStarting {
		t.Errorf("session = %+v", s)
	}
}

// The built binary in MCP mode, driven over stdin and stdout.
func TestMCPBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "dbgp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	if out, _ := exec.Command(bin, "mcp", "-h").CombinedOutput(); !strings.Contains(string(out), "Usage: dbgp mcp [options]") {
		t.Errorf("mcp -h output:\n%s", out)
	}
	cmd := exec.Command(bin, "mcp", "-map", "nonsense")
	if out, _ := cmd.CombinedOutput(); cmd.ProcessState.ExitCode() != 2 {
		t.Errorf("mcp -map nonsense: exit %d\n%s", cmd.ProcessState.ExitCode(), out)
	}

	for _, tc := range []struct {
		args      []string
		listening bool
	}{
		{[]string{"mcp", "-addr", "127.0.0.1:0"}, false},
		{[]string{"mcp", "-addr", "127.0.0.1:0", "-listen"}, true},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		transport := &mcp.CommandTransport{Command: exec.CommandContext(ctx, bin, tc.args...)}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
		if err != nil {
			cancel()
			t.Fatalf("%v: connect: %v", tc.args, err)
		}
		var status statusOutput
		callTool(t, cs, "status", nil, &status)
		if status.Listening != tc.listening {
			t.Errorf("%v: listening = %v, want %v", tc.args, status.Listening, tc.listening)
		}
		if err := cs.Close(); err != nil {
			t.Errorf("%v: close: %v", tc.args, err)
		}
		cancel()
	}
}

// Agents see only what the schemas say: every result field is described,
// and lists are declared as always lists.
func TestMCPOutputSchemas(t *testing.T) {
	cs, _ := newTestMCP(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		data, _ := json.Marshal(tool.OutputSchema)
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		var check func(path string, s map[string]any)
		check = func(path string, s map[string]any) {
			props, _ := s["properties"].(map[string]any)
			for name, p := range props {
				prop := p.(map[string]any)
				if d, _ := prop["description"].(string); d == "" {
					t.Errorf("%s: %s%s has no description", tool.Name, path, name)
				}
				if prop["type"] != "array" && prop["items"] != nil {
					t.Errorf("%s: %s%s has type %v, want array", tool.Name, path, name, prop["type"])
				}
				if items, ok := prop["items"].(map[string]any); ok {
					check(path+name+"[].", items)
				}
			}
		}
		check("", schema)
	}
}

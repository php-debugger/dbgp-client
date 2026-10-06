package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"reflect"
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
	return newTestMCPOn(t, "127.0.0.1:0")
}

// newTestMCPOn is newTestMCP with the server on addr.
func newTestMCPOn(t *testing.T, addr string) (*mcp.ClientSession, *dbgp.Server) {
	t.Helper()
	srv, err := dbgp.NewServer(dbgp.Config{Addr: addr})
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
	// Clear out first: decoding into a used value keeps fields and list
	// elements the new result leaves out.
	v := reflect.ValueOf(out).Elem()
	v.Set(reflect.Zero(v.Type()))
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
	names := []string{"status", "listen", "unlisten", "add_breakpoint", "remove_breakpoint", "breakpoints",
		"add_path_mapping", "path_mappings", "sessions", "wait_for_session", "continue", "wait_for_stop", "stop", "detach",
		"stack", "variables", "variable", "variable_value", "source", "output", "warnings"}
	for _, name := range names {
		if got[name] == nil {
			t.Errorf("tool %s missing", name)
		}
	}
	if len(got) != len(names) {
		t.Errorf("got %d tools, want %d", len(got), len(names))
	}
	for _, name := range []string{"status", "breakpoints", "path_mappings", "sessions", "wait_for_session", "wait_for_stop",
		"stack", "variables", "variable", "variable_value", "source", "output", "warnings"} {
		if a := got[name].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", name)
		}
	}
	if a := got["status"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Error("status is not marked read-only")
	}
	if got["status"].OutputSchema == nil {
		t.Error("status has no output schema")
	}
	for _, want := range []string{"listen", "php -v", "with PHP Debugger", "do not set xdebug.mode", "remove_breakpoint"} {
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
		for kind, sch := range map[string]any{"input": tool.InputSchema, "output": tool.OutputSchema} {
			data, _ := json.Marshal(sch)
			var schema map[string]any
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatalf("%s %s: %v", tool.Name, kind, err)
			}
			var check func(path string, s map[string]any)
			check = func(path string, s map[string]any) {
				props, _ := s["properties"].(map[string]any)
				for name, p := range props {
					prop := p.(map[string]any)
					if d, _ := prop["description"].(string); d == "" {
						t.Errorf("%s %s: %s%s has no description", tool.Name, kind, path, name)
					}
					if prop["type"] != "array" && prop["items"] != nil {
						t.Errorf("%s %s: %s%s has type %v, want array", tool.Name, kind, path, name, prop["type"])
					}
					if items, ok := prop["items"].(map[string]any); ok {
						check(path+name+"[].", items)
					}
					check(path+name+".", prop) // nested objects
				}
			}
			check("", schema)
		}
	}
}

// callToolError calls a tool that must fail and returns its error text.
func callToolError(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error() // rejected before reaching the tool, e.g. by its schema
	}
	if !res.IsError {
		t.Fatalf("%s(%v) succeeded: %s", name, args, resultText(res))
	}
	return resultText(res)
}

func TestMCPAddBreakpoint(t *testing.T) {
	cs, _ := newTestMCP(t)
	abs, _ := filepath.Abs("app/a.php")
	tests := []struct {
		args map[string]any
		want breakpointInfo
	}{
		{map[string]any{"file": "/app/a.php", "line": 12}, breakpointInfo{ID: 1, Type: "line", File: "/app/a.php", Line: 12}},
		{map[string]any{"type": "line", "file": "app/a.php", "line": 3}, breakpointInfo{ID: 2, Type: "line", File: abs, Line: 3}},
		{map[string]any{"file": "/app/a.php", "line": 5, "condition": "$i > 2"}, breakpointInfo{ID: 3, Type: "conditional", File: "/app/a.php", Line: 5, Condition: "$i > 2"}},
		{map[string]any{"type": "call", "function": "add"}, breakpointInfo{ID: 4, Type: "call", Function: "add"}},
		{map[string]any{"type": "return", "function": "add"}, breakpointInfo{ID: 5, Type: "return", Function: "add"}},
		{map[string]any{"type": "exception", "exception": "RuntimeException"}, breakpointInfo{ID: 6, Type: "exception", Exception: "RuntimeException"}},
	}
	var want []breakpointInfo
	for _, tt := range tests {
		var out addBreakpointOutput
		callTool(t, cs, "add_breakpoint", tt.args, &out)
		if out.Breakpoint != tt.want || out.Warning != "" {
			t.Errorf("add_breakpoint(%v) = %+v, want %+v", tt.args, out, tt.want)
		}
		want = append(want, tt.want)
	}

	for _, tt := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"file": "/app/a.php"}, "needs a file and line"},
		{map[string]any{"line": 4}, "needs a file and line"},
		{map[string]any{"type": "call"}, "needs a function"},
		{map[string]any{"type": "exception"}, "needs an exception class"},
		{map[string]any{"type": "conditional", "file": "/app/a.php", "line": 3}, "type"},
		{map[string]any{"type": "watch", "file": "/app/a.php", "line": 3}, "type"},
	} {
		if msg := callToolError(t, cs, "add_breakpoint", tt.args); !strings.Contains(msg, tt.want) {
			t.Errorf("add_breakpoint(%v) error = %q, want it to mention %q", tt.args, msg, tt.want)
		}
	}

	var list breakpointListOutput
	callTool(t, cs, "breakpoints", nil, &list)
	if fmt.Sprint(list.Breakpoints) != fmt.Sprint(want) {
		t.Errorf("breakpoints = %+v, want %+v", list.Breakpoints, want)
	}
}

func TestMCPRemoveBreakpoint(t *testing.T) {
	cs, _ := newTestMCP(t)
	for _, line := range []int{1, 2} {
		callTool(t, cs, "add_breakpoint", map[string]any{"file": "/app/a.php", "line": line}, &addBreakpointOutput{})
	}
	var out breakpointsOutput
	callTool(t, cs, "remove_breakpoint", map[string]any{"id": 1}, &out)
	if len(out.Breakpoints) != 1 || out.Breakpoints[0].ID != 2 || out.Warning != "" {
		t.Errorf("remove_breakpoint = %+v, want only breakpoint 2", out)
	}
	if msg := callToolError(t, cs, "remove_breakpoint", map[string]any{"id": 1}); !strings.Contains(msg, "no breakpoint 1") {
		t.Errorf("removing it again: %q", msg)
	}
	callTool(t, cs, "remove_breakpoint", map[string]any{"id": 2}, &out)
	if out.Breakpoints == nil || len(out.Breakpoints) != 0 {
		t.Errorf("after removing all: %+v, want an empty list", out.Breakpoints)
	}
}

func TestMCPPathMappings(t *testing.T) {
	cs, _ := newTestMCP(t)
	abs, _ := filepath.Abs("testdata/php")
	var listed pathMappingListOutput
	callTool(t, cs, "path_mappings", nil, &listed)
	if listed.PathMappings == nil || len(listed.PathMappings) != 0 {
		t.Errorf("initial path_mappings = %+v, want an empty list", listed.PathMappings)
	}
	var out pathMappingsOutput
	callTool(t, cs, "add_path_mapping", map[string]any{"local": "/home/me/app", "remote": "/var/www/html"}, &out)
	callTool(t, cs, "add_path_mapping", map[string]any{"local": "testdata/php", "remote": "/srv/php"}, &out)
	callTool(t, cs, "add_path_mapping", map[string]any{"local": "/home/me/app/", "remote": "/app"}, &out)
	want := []pathMapping{{Local: abs, Remote: "/srv/php"}, {Local: "/home/me/app", Remote: "/app"}}
	if fmt.Sprint(out.PathMappings) != fmt.Sprint(want) || out.Warning != "" {
		t.Errorf("add_path_mapping = %+v, want %+v", out, want)
	}
	callTool(t, cs, "path_mappings", nil, &listed)
	if fmt.Sprint(listed.PathMappings) != fmt.Sprint(want) {
		t.Errorf("path_mappings = %+v, want %+v", listed.PathMappings, want)
	}
	for _, args := range []map[string]any{{"local": "/a"}, {"remote": "/b"}, {"local": "", "remote": "/b"}} {
		callToolError(t, cs, "add_path_mapping", args)
	}
}

// Breakpoints added and removed over MCP reach a connected PHP session.
func TestMCPBreakpointsReachSession(t *testing.T) {
	requireEngine(t)
	cs, srv := newTestMCP(t)
	script, err := filepath.Abs("../../testdata/php/basic.php")
	if err != nil {
		t.Fatal(err)
	}
	callTool(t, cs, "add_breakpoint", map[string]any{"file": script, "line": 24}, &addBreakpointOutput{})
	callTool(t, cs, "listen", nil, &listenOutput{})
	php := exec.Command("php", "-dxdebug.mode=debug", "-dxdebug.start_with_request=yes",
		fmt.Sprintf("-dxdebug.client_port=%d", srv.Port()), "-dxdebug.client_host=127.0.0.1", "-ddisplay_errors=0", script)
	if err := php.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = php.Process.Kill(); _ = php.Wait() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := srv.WaitForSession(ctx)
	if err != nil {
		t.Fatal(err)
	}

	engineLines := func() []int {
		t.Helper()
		bps, err := sess.ListBreakpoints()
		if err != nil {
			t.Fatal(err)
		}
		var lines []int
		for _, bp := range bps {
			if bp.Filename != script {
				t.Errorf("engine breakpoint in %q, want %q", bp.Filename, script)
			}
			lines = append(lines, bp.Lineno)
		}
		return lines
	}
	if got := engineLines(); fmt.Sprint(got) != "[24]" {
		t.Errorf("engine breakpoints on connect = %v, want [24]", got)
	}
	var added addBreakpointOutput
	callTool(t, cs, "add_breakpoint", map[string]any{"file": script, "line": 39, "condition": "$i == 3"}, &added)
	if got := engineLines(); fmt.Sprint(got) != "[24 39]" {
		t.Errorf("engine breakpoints after add = %v, want [24 39]", got)
	}
	callTool(t, cs, "remove_breakpoint", map[string]any{"id": 1}, &breakpointsOutput{})
	if got := engineLines(); fmt.Sprint(got) != "[39]" {
		t.Errorf("engine breakpoints after remove = %v, want [39]", got)
	}
}

// listen reports addresses it could not use, e.g. another program holding
// [::1] on the port, which PHP connecting to localhost may reach instead.
func TestMCPListenWarnings(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	other, err := net.Listen("tcp", "[::1]:"+port)
	if err != nil {
		t.Skipf("cannot take [::1]:%s: %v", port, err)
	}
	defer other.Close()

	cs, _ := newTestMCPOn(t, "localhost:"+port)
	var out listenOutput
	callTool(t, cs, "listen", nil, &out)
	if !out.Listening || out.Address != "localhost:"+port || len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "[::1]:"+port) {
		t.Errorf("listen = %+v, want localhost:%s with a warning about [::1]", out, port)
	}
}

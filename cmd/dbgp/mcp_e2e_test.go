package main

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A whole debugging run through the dbgp binary over stdio, as an agent
// does it: from listening to a finished script and nothing left behind.
func TestMCPEndToEnd(t *testing.T) {
	requireEngine(t)
	bin := buildBinary(t)
	script, err := filepath.Abs("../../testdata/php/basic.php")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	transport := &mcp.CommandTransport{Command: exec.CommandContext(ctx, bin, "mcp", "-addr", "127.0.0.1:0")}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	var bp addBreakpointOutput
	callTool(t, cs, "add_breakpoint", map[string]any{"file": script, "line": 24}, &bp)
	var listening listenOutput
	callTool(t, cs, "listen", nil, &listening)
	_, port, err := net.SplitHostPort(listening.Address)
	if err != nil {
		t.Fatal(err)
	}

	// The script runs in the background while it is debugged.
	php := exec.Command("php", "-dxdebug.mode=debug", "-dxdebug.start_with_request=yes",
		"-dxdebug.client_host=127.0.0.1", "-dxdebug.client_port="+port, "-ddisplay_errors=0", script)
	if err := php.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- php.Wait() }()
	defer func() { _ = php.Process.Kill() }()

	var waited waitForSessionOutput
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected || waited.Session.Script != script {
		t.Fatalf("wait_for_session = %+v", waited)
	}
	var st stateOutput
	callTool(t, cs, "continue", nil, &st)
	if st.Status != "break" || st.File != script || st.Line != 24 {
		t.Fatalf("continue = %+v, want break at line 24", st)
	}

	var stack stackOutput
	callTool(t, cs, "stack", nil, &stack)
	if len(stack.Frames) != 2 || stack.Frames[0].Function != "add" || stack.Frames[1].Line != 36 {
		t.Errorf("stack = %+v", stack.Frames)
	}
	var vars variablesOutput
	callTool(t, cs, "variables", nil, &vars)
	if a := byName(vars.Variables)["$a"]; a.Value != "2" {
		t.Errorf("$a = %+v", a)
	}
	var numbers variableOutput
	callTool(t, cs, "variable", map[string]any{"name": "$numbers", "depth": 1}, &numbers)
	if numbers.Variable.Size == nil || *numbers.Variable.Size != 40 {
		t.Errorf("$numbers = %+v", numbers.Variable)
	}
	var result evalOutput
	callTool(t, cs, "eval", map[string]any{"code": "$a + $b"}, &result)
	if result.Result == nil || result.Result.Value != "5" {
		t.Errorf("eval $a + $b = %+v", result.Result)
	}
	callTool(t, cs, "set_variable", map[string]any{"name": "$b", "value": "30"}, &setVariableOutput{})
	var source sourceOutput
	callTool(t, cs, "source", nil, &source)
	if len(source.Lines) == 0 {
		t.Error("source returned no lines")
	}

	callTool(t, cs, "continue", nil, &st)
	if st.Status != "stopping" {
		t.Fatalf("continue to the end = %+v, want stopping", st)
	}
	var printed outputOutput
	callTool(t, cs, "output", nil, &printed)
	if printed.Output != "x=32\ndone\n" {
		t.Errorf("output = %q, want x=32 from the changed $b", printed.Output)
	}
	var raised warningsOutput
	callTool(t, cs, "warnings", nil, &raised)
	if len(raised.Warnings) != 1 || raised.Warnings[0].Line != 42 {
		t.Errorf("warnings = %+v", raised.Warnings)
	}

	// Cleaning up leaves nothing behind: PHP exits, and dbgp is idle.
	callTool(t, cs, "stop", nil, &st)
	if st.Status != "ended" {
		t.Errorf("stop = %+v", st)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("php: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("php still running after stop")
	}
	callTool(t, cs, "remove_breakpoint", map[string]any{"id": bp.Breakpoint.ID}, &breakpointsOutput{})
	callTool(t, cs, "unlisten", nil, &unlistenOutput{})
	var status statusOutput
	callTool(t, cs, "status", nil, &status)
	if status.Listening || len(status.Breakpoints) != 0 || len(status.Sessions) != 1 || status.Sessions[0].Status != "ended" {
		t.Errorf("status after cleaning up = %+v", status)
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		_ = conn.Close()
		t.Errorf("port %s still open after unlisten", port)
	}
}

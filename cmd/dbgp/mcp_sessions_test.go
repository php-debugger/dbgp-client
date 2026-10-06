package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startPHP runs php with the debugger pointed at port, killed at cleanup.
func startPHP(t *testing.T, port int, args ...string) {
	t.Helper()
	php := exec.Command("php", append([]string{"-dxdebug.mode=debug", "-dxdebug.start_with_request=yes",
		fmt.Sprintf("-dxdebug.client_port=%d", port), "-dxdebug.client_host=127.0.0.1", "-ddisplay_errors=0"}, args...)...)
	if err := php.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = php.Process.Kill(); _ = php.Wait() })
}

func TestMCPSessionToolErrors(t *testing.T) {
	cs, _ := newTestMCP(t)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"continue", nil, "no session: call listen"},
		{"wait_for_stop", nil, "no session: call listen"},
		{"stop", nil, "no session: call listen"},
		{"detach", nil, "no session: call listen"},
		{"continue", map[string]any{"session": 9}, "no session 9"},
		{"wait_for_session", nil, "not listening for PHP connections: call listen first"},
		{"wait_for_session", map[string]any{"wait": -1}, "must not be negative"},
		{"wait_for_session", map[string]any{"wait": 601}, "at most 600"},
		{"continue", map[string]any{"command": "jump"}, "command"},
	} {
		if msg := callToolError(t, cs, tc.tool, tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("%s(%v) error = %q, want it to mention %q", tc.tool, tc.args, msg, tc.want)
		}
	}
}

func TestMCPWaitForSessionTimesOut(t *testing.T) {
	cs, _ := newTestMCP(t)
	callTool(t, cs, "listen", nil, &listenOutput{})
	start := time.Now()
	var out waitForSessionOutput
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 0.2}, &out)
	if out.Connected || out.Session != nil || time.Since(start) > 5*time.Second {
		t.Errorf("wait_for_session = %+v after %v, want not connected after 0.2s", out, time.Since(start))
	}
}

// An agent's debugging run, entirely through MCP.
func TestMCPDebuggingRun(t *testing.T) {
	requireEngine(t)
	cs, srv := newTestMCP(t)
	script, err := filepath.Abs("../../testdata/php/basic.php")
	if err != nil {
		t.Fatal(err)
	}
	callTool(t, cs, "add_breakpoint", map[string]any{"file": script, "line": 24}, &addBreakpointOutput{})
	callTool(t, cs, "listen", nil, &listenOutput{})
	startPHP(t, srv.Port(), script)

	var waited waitForSessionOutput
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected || waited.Session.ID != 1 || waited.Session.Script != script || waited.Session.Status != "starting" {
		t.Fatalf("wait_for_session = %+v, session %+v", waited, waited.Session)
	}

	step := func(args map[string]any, want stateOutput) {
		t.Helper()
		var st stateOutput
		callTool(t, cs, "continue", args, &st)
		if st != want {
			t.Errorf("continue(%v) = %+v, want %+v", args, st, want)
		}
	}
	step(nil, stateOutput{Session: 1, Status: "break", File: script, Line: 24})
	step(map[string]any{"command": "step_over"}, stateOutput{Session: 1, Status: "break", File: script, Line: 25})
	step(map[string]any{"session": 1, "command": "step_out", "wait": 5}, stateOutput{Session: 1, Status: "break", File: script, Line: 37})

	callTool(t, cs, "add_breakpoint", map[string]any{"type": "exception", "exception": "RuntimeException"}, &addBreakpointOutput{})
	step(nil, stateOutput{Session: 1, Status: "break", File: script, Line: 44, Exception: "RuntimeException", ExceptionMessage: "boom"})
	step(nil, stateOutput{Session: 1, Status: "stopping"})

	var sessions sessionsOutput
	callTool(t, cs, "sessions", nil, &sessions)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].Status != "stopping" {
		t.Errorf("sessions = %+v", sessions.Sessions)
	}

	var stopped stateOutput
	callTool(t, cs, "stop", nil, &stopped)
	if stopped.Status != "ended" {
		t.Errorf("stop = %+v, want ended", stopped)
	}
	// Removing breakpoints right after stop: the ended session is skipped.
	for _, id := range []int{1, 2} {
		var removed breakpointsOutput
		callTool(t, cs, "remove_breakpoint", map[string]any{"id": id}, &removed)
		if removed.Warning != "" {
			t.Errorf("remove_breakpoint %d after stop warned: %s", id, removed.Warning)
		}
	}
	if msg := callToolError(t, cs, "continue", map[string]any{"session": 1}); !strings.Contains(msg, "session 1 has ended") {
		t.Errorf("continue after stop: %q", msg)
	}
	if msg := callToolError(t, cs, "continue", nil); !strings.Contains(msg, "no session") {
		t.Errorf("continue with no live session: %q", msg)
	}
}

// A script that runs past the wait: continue returns running, and
// wait_for_stop waits on.
func TestMCPRunningSession(t *testing.T) {
	requireEngine(t)
	cs, srv := newTestMCP(t)
	callTool(t, cs, "listen", nil, &listenOutput{})
	startPHP(t, srv.Port(), "-r", `usleep(1500000); echo "done\n";`)

	var waited waitForSessionOutput
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected {
		t.Fatal("PHP did not connect")
	}
	var st stateOutput
	callTool(t, cs, "continue", map[string]any{"wait": 0.2}, &st)
	if st.Status != "running" {
		t.Fatalf("continue = %+v, want running", st)
	}
	if msg := callToolError(t, cs, "continue", nil); !strings.Contains(msg, "session 1 is still running: call wait_for_stop first") {
		t.Errorf("continue while running: %q", msg)
	}
	if msg := callToolError(t, cs, "stop", nil); !strings.Contains(msg, "still running") {
		t.Errorf("stop while running: %q", msg)
	}
	callTool(t, cs, "wait_for_stop", map[string]any{"wait": 10}, &st)
	if st.Status != "stopping" {
		t.Fatalf("wait_for_stop = %+v, want stopping", st)
	}
	callTool(t, cs, "detach", nil, &st)
	if st.Status != "stopping" && st.Status != "ended" {
		t.Errorf("detach = %+v", st)
	}
}

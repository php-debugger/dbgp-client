package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
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
	// The ended session stays selected, rather than whatever connects next.
	if msg := callToolError(t, cs, "continue", nil); !strings.Contains(msg, "session 1 has ended: wait_for_session returns the next one") {
		t.Errorf("continue after the selected session ended: %q", msg)
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
	if msg := callToolError(t, cs, "stop", nil); !strings.Contains(msg, "dbgp cannot stop it: call wait_for_stop") {
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

// A script stuck in a loop never stops, so it cannot be stopped, but
// detach lets it go: the session ends, and the script runs on without the
// debugger.
func TestMCPDetachRunawayScript(t *testing.T) {
	requireEngine(t)
	cs, srv := newTestMCP(t)
	callTool(t, cs, "listen", nil, &listenOutput{})
	counter := filepath.Join(t.TempDir(), "counter")
	startPHP(t, srv.Port(), "-r", fmt.Sprintf(`for ($i = 0; ; $i++) { file_put_contents(%q, $i); usleep(10000); }`, counter))
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
	if msg := callToolError(t, cs, "stop", nil); !strings.Contains(msg, "detach to let it run on") {
		t.Errorf("stop while running: %q", msg)
	}

	callTool(t, cs, "detach", nil, &st)
	if st.Session != 1 || st.Status != "ended" {
		t.Errorf("detach while running = %+v, want session 1 ended", st)
	}
	count := func() string { data, _ := os.ReadFile(counter); return string(data) }
	before := count()
	time.Sleep(300 * time.Millisecond)
	if after := count(); after == before {
		t.Errorf("the script stopped counting at %q after detach, want it to run on", before)
	}
}

// waitSessions waits up to 10s for srv to have n sessions.
func waitSessions(t *testing.T, srv *dbgp.Server, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(srv.Sessions()) < n; {
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions, want %d", len(srv.Sessions()), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A session no agent takes is detached once the claim timeout passes, so
// its script runs on; one an agent names by id is taken, and kept.
func TestMCPDetachesUnclaimedSessions(t *testing.T) {
	requireEngine(t)
	const timeout = time.Second
	cs, srv := newTestMCPWith(t, dbgp.Config{Addr: "127.0.0.1:0", ClaimTimeout: timeout}, true)
	callTool(t, cs, "listen", nil, &listenOutput{})
	done := filepath.Join(t.TempDir(), "done")
	startPHP(t, srv.Port(), "-r", fmt.Sprintf(`file_put_contents(%q, "done");`, done))
	waitSessions(t, srv, 1)
	startPHP(t, srv.Port(), "-r", `echo "hi\n";`)
	waitSessions(t, srv, 2)
	callTool(t, cs, "output", map[string]any{"session": 2}, &outputOutput{})

	select {
	case <-srv.Session(1).Done():
	case <-time.After(timeout + 5*time.Second):
		t.Fatal("the unclaimed session was not detached")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(done); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the unclaimed session's script did not run on after detach")
		}
	}

	time.Sleep(timeout)
	if srv.Session(2).State().Closed {
		t.Fatal("the session named by id was detached")
	}
	var waited waitForSessionOutput
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 0.2}, &waited)
	if waited.Connected {
		t.Errorf("wait_for_session returned session %d, want none left", waited.Session.ID)
	}
	var st stateOutput
	callTool(t, cs, "continue", nil, &st)
	if st.Session != 2 || st.Status != "stopping" && st.Status != "ended" {
		t.Errorf("continue = %+v, want session 2 run to its end", st)
	}
}

// The session tools act on the session the agent picked, not the newest:
// anything that can reach dbgp could connect and become that. Sessions show
// where they connected from.
func TestMCPSelectedSession(t *testing.T) {
	requireEngine(t)
	cs, srv := newTestMCPOn(t, phpAddr(0), true)
	callTool(t, cs, "listen", nil, &listenOutput{})
	const script = `echo "hi\n";`
	startPHP(t, srv.Port(), "-r", script)
	waitSessions(t, srv, 1)

	// Connected, but not picked yet.
	if msg := callToolError(t, cs, "continue", nil); !strings.Contains(msg, "no session: call listen") {
		t.Errorf("continue before wait_for_session: %q", msg)
	}
	var waited waitForSessionOutput
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected || waited.Session.ID != 1 || waited.Session.From != "127.0.0.1" {
		t.Fatalf("wait_for_session = %+v, session %+v", waited, waited.Session)
	}

	// Another script connects, from this machine's network address if it
	// has one, as PHP elsewhere would.
	host := lanAddr()
	if host == "" {
		host = "127.0.0.1"
	}
	startPHP(t, srv.Port(), "-dxdebug.client_host="+host, "-r", script)
	waitSessions(t, srv, 2)
	var status statusOutput
	callTool(t, cs, "status", nil, &status)
	if len(status.Sessions) != 2 || status.Sessions[1].From != host {
		t.Errorf("sessions = %+v, want the second from %s", status.Sessions, host)
	}
	if got := fromElsewhere(srv.Session(1)); got != "" {
		t.Errorf("fromElsewhere(session 1) = %q, want nothing for this machine", got)
	}
	if want := " from " + host; host != "127.0.0.1" && fromElsewhere(srv.Session(2)) != want {
		t.Errorf("fromElsewhere(session 2) = %q, want %q", fromElsewhere(srv.Session(2)), want)
	}

	// The newer session does not take over; naming it selects it.
	var st stateOutput
	callTool(t, cs, "continue", nil, &st)
	if st.Session != 1 {
		t.Errorf("continue acted on session %d, want 1", st.Session)
	}
	callTool(t, cs, "continue", map[string]any{"session": 2}, &st)
	if st.Session != 2 {
		t.Errorf("continue with session 2 acted on session %d", st.Session)
	}
	callTool(t, cs, "stop", nil, &st)
	if st.Session != 2 || st.Status != "ended" {
		t.Errorf("stop = %+v, want session 2 ended", st)
	}
}

// newSharedMCP serves the tools over HTTP as the background server does,
// with token, on a PHP port that is not listening yet.
func newSharedMCP(t *testing.T, token string) (string, *dbgp.Server) {
	t.Helper()
	srv, err := dbgp.NewServer(dbgp.Config{Addr: phpAddr(0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	server, tl := newMCPServer(srv, true, true)
	hs := httptest.NewServer(httpHandler(server, tl, daemonState{Token: token}))
	t.Cleanup(hs.Close)
	return hs.URL + "/mcp", srv
}

// connectAgent connects to the server at url as dbgp mcp does, as agent.
func connectAgent(t *testing.T, url, token, agent string) *mcp.ClientSession {
	t.Helper()
	client, err := tokenClient(url, token, agent)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: agent, Version: "0"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// Agents on the background server are told they share it; an agent on
// stdin and stdout has dbgp to itself.
func TestMCPSharedInstructions(t *testing.T) {
	url, _ := newSharedMCP(t, "token")
	shared := connectAgent(t, url, "token", "agent").InitializeResult().Instructions
	if !strings.HasSuffix(shared, sharedInstructions) {
		t.Errorf("background server instructions lack the shared ones:\n%s", shared)
	}
	own, _ := newTestMCP(t)
	if strings.Contains(own.InitializeResult().Instructions, "other agents") {
		t.Error("dbgp mcp on stdin and stdout says other agents may share it")
	}
}

// Agents sharing the background server each have their own selected
// session and place in a session's output, so one does not move the
// other's.
func TestMCPAgentsSharingServer(t *testing.T) {
	requireEngine(t)
	url, srv := newSharedMCP(t, "token")
	a := connectAgent(t, url, "token", "agent-a")
	b := connectAgent(t, url, "token", "agent-b")

	callTool(t, a, "listen", nil, &listenOutput{})
	startPHP(t, srv.Port(), "-r", `echo "one\n";`)
	var waited waitForSessionOutput
	callTool(t, a, "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected || waited.Session.ID != 1 {
		t.Fatalf("a: wait_for_session = %+v", waited)
	}
	if msg := callToolError(t, b, "continue", nil); !strings.Contains(msg, "no session") {
		t.Errorf("b: continue acted on a's session: %q", msg)
	}
	startPHP(t, srv.Port(), "-r", `echo "two\n";`)
	callTool(t, b, "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected || waited.Session.ID != 2 {
		t.Fatalf("b: wait_for_session = %+v", waited)
	}
	var st stateOutput
	for name, agent := range map[string]*mcp.ClientSession{"a": a, "b": b} {
		callTool(t, agent, "continue", nil, &st)
		if want := map[string]int{"a": 1, "b": 2}[name]; st.Session != want || st.Status != "stopping" {
			t.Errorf("%s: continue = %+v, want session %d stopping", name, st, want)
		}
	}

	// a reads session 1's output; b still gets all of it.
	var printed outputOutput
	for _, want := range []string{"one\n", ""} {
		callTool(t, a, "output", nil, &printed)
		if printed.Session != 1 || printed.Output != want {
			t.Errorf("a: output = %+v, want session 1 %q", printed, want)
		}
	}
	callTool(t, b, "output", map[string]any{"session": 1}, &printed)
	if printed.Output != "one\n" {
		t.Errorf("b: output of session 1 = %q, want all of it", printed.Output)
	}
	// Naming a session selects it for that agent only.
	callTool(t, a, "output", map[string]any{"session": 2}, &printed)
	callTool(t, b, "output", nil, &printed)
	if printed.Session != 1 || printed.Output != "" {
		t.Errorf("b: output = %+v, want session 1, which b named last, with nothing new", printed)
	}
}

// On the background server, dbgp listens while any agent that called
// listen has not called unlisten, or left: one agent does not stop PHP
// connections that another is waiting for.
// leaveAs tells the server at url that agent has gone, as its dbgp mcp
// does when it ends.
func leaveAs(t *testing.T, url, agent string) {
	t.Helper()
	client, err := tokenClient(url, "token", agent)
	if err != nil {
		t.Fatal(err)
	}
	leaveServer(client, url)
}

// An agent that leaves without ending its sessions does not hold their PHP
// processes: they are detached, unless another agent took them too.
func TestMCPLeaveDetachesSessions(t *testing.T) {
	requireEngine(t)
	url, srv := newSharedMCP(t, "token")
	a := connectAgent(t, url, "token", "agent-a")
	b := connectAgent(t, url, "token", "agent-b")
	callTool(t, a, "listen", nil, &listenOutput{})
	dir := t.TempDir()
	start := func(cs *mcp.ClientSession, name string) int {
		t.Helper()
		startPHP(t, srv.Port(), "-r", fmt.Sprintf(`file_put_contents(%q, "done");`, filepath.Join(dir, name)))
		var waited waitForSessionOutput
		callTool(t, cs, "wait_for_session", map[string]any{"wait": 10}, &waited)
		if !waited.Connected {
			t.Fatalf("%s did not connect", name)
		}
		return waited.Session.ID
	}
	detached := func(id int) bool {
		t.Helper()
		select {
		case <-srv.Session(id).Done():
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}
	finished := func(name string) bool {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return true
			}
		}
		return false
	}

	own := start(a, "own")
	shared := start(b, "shared")
	callTool(t, a, "output", map[string]any{"session": shared}, &outputOutput{})

	leaveAs(t, url, "agent-a")
	if !detached(own) || !finished("own") {
		t.Error("a's session was not detached when a left")
	}
	select {
	case <-srv.Session(shared).Done():
		t.Error("the session b took too was detached when a left")
	case <-time.After(300 * time.Millisecond):
	}
	leaveAs(t, url, "agent-b")
	if !detached(shared) || !finished("shared") {
		t.Error("the shared session was not detached when b left too")
	}
}

func TestMCPListeningPerAgent(t *testing.T) {
	url, srv := newSharedMCP(t, "token")
	a := connectAgent(t, url, "token", "agent-a")
	b := connectAgent(t, url, "token", "agent-b")
	unlisten := func(name string, cs *mcp.ClientSession, listening bool, others int) {
		t.Helper()
		var out unlistenOutput
		callTool(t, cs, "unlisten", nil, &out)
		if out.Listening != listening || out.OtherAgents != others || srv.Listening() != listening {
			t.Errorf("%s: unlisten = %+v, server listening %v; want listening %v for %d other agents",
				name, out, srv.Listening(), listening, others)
		}
	}

	callTool(t, a, "listen", nil, &listenOutput{})
	callTool(t, b, "listen", nil, &listenOutput{})
	unlisten("a", a, true, 1)
	unlisten("a again", a, true, 1)
	unlisten("b", b, false, 0)

	// An agent that did not call listen does not stop it either.
	callTool(t, a, "listen", nil, &listenOutput{})
	unlisten("b, not listening", b, true, 1)

	// An agent leaving, as its dbgp mcp tells the server when it ends,
	// counts as unlisten.
	leaveAs(t, url, "agent-b")
	if !srv.Listening() {
		t.Error("b leaving stopped a's listening")
	}
	leaveAs(t, url, "agent-a")
	if srv.Listening() {
		t.Error("still listening after a, the only agent listening, left")
	}

	// Only agents with the token can leave.
	callTool(t, a, "listen", nil, &listenOutput{})
	resp, err := http.Post(strings.TrimSuffix(url, "/mcp")+leavePath, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !srv.Listening() {
		t.Errorf("leave without the token: status %d, listening %v; want 401 and still listening", resp.StatusCode, srv.Listening())
	}
}

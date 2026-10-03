package dbgp

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestServerAddrAndPort(t *testing.T) {
	srv := newTestServer(t, Config{})
	if srv.Port() == 0 {
		t.Error("Port() = 0")
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") || !strings.HasSuffix(srv.Addr(), ":"+strconv.Itoa(srv.Port())) {
		t.Errorf("Addr() = %q, Port() = %d", srv.Addr(), srv.Port())
	}
}

func TestListenFailsOnBusyAddress(t *testing.T) {
	srv := newTestServer(t, Config{})
	if other, err := Listen(Config{Addr: srv.Addr()}); err == nil {
		_ = other.Close()
		t.Error("Listen succeeded on an address in use")
	}
}

func TestWaitForSessionHonoursContext(t *testing.T) {
	srv := newTestServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := srv.WaitForSession(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitForSession err = %v, want deadline exceeded", err)
	}
}

func TestWaitForSessionAfterClose(t *testing.T) {
	srv := newTestServer(t, Config{})
	errc := make(chan error, 1)
	go func() { _, err := srv.WaitForSession(context.Background()); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrServerClosed) {
			t.Errorf("WaitForSession err = %v, want ErrServerClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitForSession still waiting after Close")
	}
}

// A connection with a bad init packet is dropped; the server keeps serving.
func TestServerDropsBadInit(t *testing.T) {
	for name, raw := range map[string]string{
		"not xml":         "5\x00hello\x00",
		"bad length":      "abc\x00<init/>\x00",
		"missing nul":     "7\x00<init/>X",
		"wrong root":      "11\x00<response/>\x00",
		"closed too soon": "100\x00<init",
	} {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t, Config{})
			conn := dialServer(t, srv)
			_, _ = conn.Write([]byte(raw))
			if name == "closed too soon" {
				_ = conn.Close()
			} else {
				_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				if _, err := io.ReadAll(conn); err != nil {
					t.Errorf("server did not close the connection: %v", err)
				}
			}

			dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
			sess := waitSession(t, srv)
			if sess.ID() != 1 || len(srv.Sessions()) != 1 {
				t.Errorf("session %d, %d sessions; want only the good one", sess.ID(), len(srv.Sessions()))
			}
		})
	}
}

func TestServerInitTimeout(t *testing.T) {
	srv := newTestServer(t, Config{InitTimeout: 100 * time.Millisecond})
	conn := dialServer(t, srv)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Errorf("server did not close a silent connection: %v", err)
	}
}

func TestServerSessionSetup(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"defaults", Config{}, []string{
			"feature_set -n notify_ok -v 1",
			"feature_set -n resolved_breakpoints -v 1",
			"stdout -c 1",
		}},
		{"features and redirect", Config{Stdout: StdoutRedirect, Features: map[string]string{"max_depth": "2", "max_children": "50"}}, []string{
			"feature_set -n notify_ok -v 1",
			"feature_set -n resolved_breakpoints -v 1",
			"feature_set -n max_children -v 50",
			"feature_set -n max_depth -v 2",
			"stdout -c 2",
		}},
		{"output disabled", Config{Stdout: StdoutDisabled}, []string{
			"feature_set -n notify_ok -v 1",
			"feature_set -n resolved_breakpoints -v 1",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, tt.cfg)
			e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
			sess := waitSession(t, srv)
			var got []string
			for _, cmd := range e.Received() {
				got = append(got, strings.TrimSuffix(cmd.Raw, " -i "+cmd.Args["i"]))
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("setup commands =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if err := sess.SetupError(); err != nil {
				t.Errorf("SetupError: %v", err)
			}
			if st := sess.State(); st.Status != StatusStarting {
				t.Errorf("State = %+v, want starting", st)
			}
		})
	}
}

func TestServerReportsRejectedFeatures(t *testing.T) {
	srv := newTestServer(t, Config{Features: map[string]string{"no_such_feature": "1"}})
	dialFakeEngine(t, srv, fixture(t, "init"), map[string]fakeHandler{
		"feature_set": func(e *fakeEngine, cmd fakeCommand) []string {
			if cmd.Args["n"] == "no_such_feature" {
				return []string{errorPacket(cmd, 3, "invalid or missing options")}
			}
			return []string{e.withTransaction(e.fixture("feature_set"), cmd)}
		},
		"stdout": reply("stdout"),
	})
	sess := waitSession(t, srv)
	if err := sess.SetupError(); err == nil || !strings.Contains(err.Error(), "no_such_feature") {
		t.Errorf("SetupError = %v, want no_such_feature", err)
	}
}

// initWithIDEKey returns the recorded init packet with an idekey attribute.
func initWithIDEKey(t *testing.T, key string) string {
	return strings.Replace(fixture(t, "init"), `<init `, `<init idekey="`+key+`" `, 1)
}

func TestServerIDEKeyFilter(t *testing.T) {
	srv := newTestServer(t, Config{IDEKey: "agent"})

	other := dialFakeEngine(t, srv, initWithIDEKey(t, "someone-else"), standardHandlers())
	if !other.WaitClosed(3 * time.Second) {
		t.Fatal("server kept a session for another IDE key")
	}
	if cmds := other.Received(); len(cmds) != 1 || cmds[0].Raw != "detach -i 1" {
		t.Errorf("other IDE key got %+v, want a single detach", cmds)
	}

	dialFakeEngine(t, srv, initWithIDEKey(t, "agent"), standardHandlers())
	if sess := waitSession(t, srv); sess.Init().IDEKey != "agent" {
		t.Errorf("idekey = %q", sess.Init().IDEKey)
	}
	if got := len(srv.Sessions()); got != 1 {
		t.Errorf("Sessions() has %d sessions, want 1", got)
	}
}

func TestServerTracksSessions(t *testing.T) {
	srv := newTestServer(t, Config{})
	first := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	s1 := waitSession(t, srv)
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	s2 := waitSession(t, srv)

	if s1.ID() != 1 || s2.ID() != 2 {
		t.Errorf("ids = %d, %d; want 1, 2", s1.ID(), s2.ID())
	}
	if got := srv.Sessions(); len(got) != 2 || got[0] != s1 || got[1] != s2 {
		t.Errorf("Sessions() = %v", got)
	}
	if srv.Session(2) != s2 || srv.Session(3) != nil {
		t.Error("Session(id) lookup failed")
	}

	// An ended session stays listed, marked closed; the other is unaffected.
	first.Close()
	<-s1.Done()
	if !s1.State().Closed || len(srv.Sessions()) != 2 {
		t.Errorf("after first ended: %+v, %d sessions", s1.State(), len(srv.Sessions()))
	}
	if st, err := s2.Status(); err != nil || st != StatusBreak {
		t.Errorf("second session Status = %q, %v", st, err)
	}
}

func TestServerCloseEndsSessions(t *testing.T) {
	srv := newTestServer(t, Config{})
	e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	sess := waitSession(t, srv)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if !e.WaitClosed(3 * time.Second) {
		t.Error("engine connection still open after server Close")
	}
	<-sess.Done()
	if err := srv.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// breakpointCommands returns the breakpoint_set/remove commands e received.
func breakpointCommands(e *fakeEngine) []string {
	var cmds []string
	for _, cmd := range e.Received() {
		if strings.HasPrefix(cmd.Name, "breakpoint_") {
			cmds = append(cmds, strings.TrimSuffix(cmd.Raw, " -i "+cmd.Args["i"]))
		}
	}
	return cmds
}

func TestServerBreakpointsAppliedOnConnect(t *testing.T) {
	srv := newTestServer(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{File: "/app/basic.php", Line: 24}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.AddBreakpoint(Breakpoint{Type: BreakpointConditional, File: "/app/basic.php", Line: 39, Condition: "$i == 3"}); err != nil {
		t.Fatal(err)
	}
	e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	waitSession(t, srv)

	want := []string{
		"breakpoint_set -t line -f file:///app/basic.php -n 24",
		"breakpoint_set -t conditional -f file:///app/basic.php -n 39 -- JGkgPT0gMw==",
	}
	// The data follows -i; compare without the transaction id.
	var got []string
	for _, cmd := range e.Received() {
		if cmd.Name == "breakpoint_set" {
			got = append(got, strings.Replace(cmd.Raw, " -i "+cmd.Args["i"], "", 1))
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("breakpoint commands =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestServerBreakpointsAppliedToStoppedSessions(t *testing.T) {
	srv := newTestServer(t, Config{})
	e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	waitSession(t, srv)

	bp, err := srv.AddBreakpoint(Breakpoint{Type: BreakpointCall, Function: "add"})
	if err != nil || bp.ID != 1 {
		t.Fatalf("AddBreakpoint = %+v, %v", bp, err)
	}
	if got := breakpointCommands(e); len(got) != 1 || got[0] != "breakpoint_set -t call -m add" {
		t.Fatalf("after add: %q", got)
	}
	if err := srv.RemoveBreakpoint(bp.ID); err != nil {
		t.Fatal(err)
	}
	// The engine id comes from the recorded breakpoint_set response.
	if got := breakpointCommands(e); len(got) != 2 || got[1] != "breakpoint_remove -d 42420001" {
		t.Fatalf("after remove: %q", got)
	}
	if len(srv.Breakpoints()) != 0 {
		t.Errorf("Breakpoints() = %+v, want none", srv.Breakpoints())
	}
	if err := srv.RemoveBreakpoint(bp.ID); err == nil {
		t.Error("removing a removed breakpoint succeeded")
	}
}

// A running session cannot take commands; it gets new breakpoints when it
// is next continued.
func TestServerBreakpointsAppliedToRunningSessionOnContinue(t *testing.T) {
	release := make(chan struct{})
	handlers := standardHandlers()
	handlers["run"] = func(e *fakeEngine, cmd fakeCommand) []string {
		<-release
		return []string{e.withTransaction(e.fixture("run_break"), cmd)}
	}
	srv := newTestServer(t, Config{})
	e := dialFakeEngine(t, srv, fixture(t, "init"), handlers)
	sess := waitSession(t, srv)
	e.ClearReceived()

	if st, _ := sess.Continue(context.Background(), ContinueRun, 0); st.Status != StatusRunning {
		t.Fatalf("State = %+v, want running", st)
	}
	if _, err := srv.AddBreakpoint(Breakpoint{File: "/app/basic.php", Line: 39}); err != nil {
		t.Fatalf("AddBreakpoint while running: %v", err)
	}
	if got := breakpointCommands(e); len(got) != 0 {
		t.Fatalf("breakpoint sent to a running session: %q", got)
	}

	close(release)
	if _, err := sess.Wait(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	e.Handle("run", reply("run_stopping"))
	if _, err := sess.Continue(context.Background(), ContinueRun, time.Second); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, cmd := range e.Received() {
		names = append(names, cmd.Name)
	}
	if want := "run breakpoint_set run"; strings.Join(names, " ") != want {
		t.Errorf("commands = %q, want %q", strings.Join(names, " "), want)
	}
}

func TestServerBreakpointErrors(t *testing.T) {
	srv := newTestServer(t, Config{})
	for _, bp := range []Breakpoint{
		{},
		{File: "/app/basic.php"},
		{Type: BreakpointConditional, File: "/app/basic.php", Line: 3},
		{Type: BreakpointCall},
		{Type: BreakpointReturn},
		{Type: BreakpointException},
		{Type: BreakpointWatch, File: "/app/basic.php", Line: 3},
	} {
		if _, err := srv.AddBreakpoint(bp); err == nil {
			t.Errorf("AddBreakpoint(%+v) succeeded", bp)
		}
	}
	if len(srv.Breakpoints()) != 0 {
		t.Errorf("invalid breakpoints were kept: %+v", srv.Breakpoints())
	}

	// A breakpoint the engine rejects is reported, and retried on Continue.
	dialFakeEngine(t, srv, fixture(t, "init"), map[string]fakeHandler{
		"breakpoint_set": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{errorPacket(cmd, 200, "breakpoint could not be set")}
		},
		"run": reply("run_break"),
	})
	sess := waitSession(t, srv)
	if _, err := srv.AddBreakpoint(Breakpoint{File: "/app/basic.php", Line: 3}); err == nil || !strings.Contains(err.Error(), "session 1") {
		t.Errorf("AddBreakpoint err = %v, want session 1 error", err)
	}
	if _, err := sess.Continue(context.Background(), ContinueRun, time.Second); err == nil || !strings.Contains(err.Error(), "200") {
		t.Errorf("Continue err = %v, want breakpoint error 200", err)
	}
}

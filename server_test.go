package dbgp

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
		"huge length":     "9223372036854775807\x00",
		"over the limit":  strconv.Itoa(maxInitPacketBytes+1) + "\x00",
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
	probeCommand := "eval -- " + base64.StdEncoding.EncodeToString([]byte(engineMappingProbe))
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"defaults", Config{}, []string{
			"feature_set -n notify_ok -v 1",
			"feature_set -n resolved_breakpoints -v 1",
			probeCommand,
			"stdout -c 1",
		}},
		{"features and redirect", Config{Stdout: StdoutRedirect, Features: map[string]string{"max_depth": "2", "max_children": "50"}}, []string{
			"feature_set -n notify_ok -v 1",
			"feature_set -n resolved_breakpoints -v 1",
			probeCommand,
			"feature_set -n max_children -v 50",
			"feature_set -n max_depth -v 2",
			"stdout -c 2",
		}},
		{"output disabled", Config{Stdout: StdoutDisabled}, []string{
			"feature_set -n notify_ok -v 1",
			"feature_set -n resolved_breakpoints -v 1",
			probeCommand,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, tt.cfg)
			e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
			sess := waitSession(t, srv)
			var got []string
			for _, cmd := range e.Received() {
				got = append(got, strings.Replace(cmd.Raw, " -i "+cmd.Args["i"], "", 1))
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

func TestServerForgetsOldEndedSessions(t *testing.T) {
	srv := newTestServer(t, Config{})
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	open := waitSession(t, srv)
	for range maxEndedSessions + 1 {
		e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
		sess := waitSession(t, srv)
		e.Close()
		<-sess.Done()
	}
	// Ended sessions are pruned when the next one connects.
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	last := waitSession(t, srv)

	sessions := srv.Sessions()
	if len(sessions) != maxEndedSessions+2 {
		t.Fatalf("%d sessions kept, want %d", len(sessions), maxEndedSessions+2)
	}
	// The oldest ended session, id 2, is forgotten; the open one is kept.
	if sessions[0] != open || sessions[1].ID() != 3 || sessions[len(sessions)-1] != last {
		t.Errorf("kept sessions %d, %d … %d", sessions[0].ID(), sessions[1].ID(), sessions[len(sessions)-1].ID())
	}
	if srv.Session(2) != nil {
		t.Error("session 2 still found")
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

	// A breakpoint the engine rejects is reported once, here by
	// AddBreakpoint, and not sent again: Continue runs without it.
	e := dialFakeEngine(t, srv, fixture(t, "init"), map[string]fakeHandler{
		"breakpoint_set": rejectBreakpoints,
		"run":            reply("run_break"),
	})
	sess := waitSession(t, srv)
	if _, err := srv.AddBreakpoint(Breakpoint{File: "/app/basic.php", Line: 3}); err == nil || !strings.Contains(err.Error(), "session 1") {
		t.Errorf("AddBreakpoint err = %v, want session 1 error", err)
	}
	e.ClearReceived()
	if st, err := sess.Continue(context.Background(), ContinueRun, time.Second); err != nil || st.Status != StatusBreak {
		t.Errorf("Continue after the rejection was reported = %+v, %v; want it to run", st, err)
	}
	if got := breakpointCommands(e); len(got) != 0 {
		t.Errorf("rejected breakpoint sent again: %q", got)
	}
}

// rejectBreakpoints answers breakpoint_set as an engine that cannot set it.
func rejectBreakpoints(e *fakeEngine, cmd fakeCommand) []string {
	return []string{errorPacket(cmd, 200, "breakpoint could not be set")}
}

// commandNames returns the names of the commands e received.
func commandNames(e *fakeEngine) string {
	var names []string
	for _, cmd := range e.Received() {
		names = append(names, cmd.Name)
	}
	return strings.Join(names, " ")
}

// A breakpoint rejected while the session was set up is reported by the
// first Continue, which does not resume the script; the next one runs
// without it.
func TestRejectedBreakpointReportedByFirstContinue(t *testing.T) {
	srv := newTestServer(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{File: "/app/basic.php", Line: 3}); err != nil {
		t.Fatal(err)
	}
	e := dialFakeEngine(t, srv, fixture(t, "init"), map[string]fakeHandler{
		"breakpoint_set": rejectBreakpoints,
		"run":            reply("run_break"),
	})
	sess := waitSession(t, srv)
	e.ClearReceived()

	_, err := sess.Continue(context.Background(), ContinueRun, time.Second)
	if err == nil || !strings.Contains(err.Error(), "200") || !strings.Contains(err.Error(), "not resumed") {
		t.Errorf("first Continue err = %v, want the rejection, not resumed", err)
	}
	if got := commandNames(e); got != "" {
		t.Errorf("first Continue sent %q, want nothing", got)
	}
	st, err := sess.Continue(context.Background(), ContinueRun, time.Second)
	if err != nil || st.Status != StatusBreak {
		t.Errorf("second Continue = %+v, %v; want it to run", st, err)
	}
	if got := commandNames(e); got != "run" {
		t.Errorf("second Continue sent %q, want only run", got)
	}
}

// A path mapping change retries rejected breakpoints, as the new path may
// be accepted; removing a rejected breakpoint forgets it.
func TestRejectedBreakpointRetriedAfterPathMapping(t *testing.T) {
	srv := newTestServer(t, Config{})
	accept := false
	var mu sync.Mutex
	e := dialFakeEngine(t, srv, fixture(t, "init"), map[string]fakeHandler{
		"breakpoint_set": func(e *fakeEngine, cmd fakeCommand) []string {
			mu.Lock()
			defer mu.Unlock()
			if !accept {
				return rejectBreakpoints(e, cmd)
			}
			return reply("breakpoint_set_line")(e, cmd)
		},
		"breakpoint_remove": reply("breakpoint_remove"),
	})
	sess := waitSession(t, srv)
	rejected, err := srv.AddBreakpoint(Breakpoint{File: "/home/me/app/basic.php", Line: 3})
	if err == nil {
		t.Fatal("AddBreakpoint: the engine's rejection was not reported")
	}

	mu.Lock()
	accept = true
	mu.Unlock()
	e.ClearReceived()
	if err := srv.AddPathMapping(PathMapping{Local: "/home/me/app", Remote: "/app"}); err != nil {
		t.Fatalf("AddPathMapping: %v", err)
	}
	if got := breakpointCommands(e); len(got) != 1 || got[0] != "breakpoint_set -t line -f file:///app/basic.php -n 3" {
		t.Errorf("after the mapping: %q, want the breakpoint set with its new path", got)
	}

	// A breakpoint that stays rejected is forgotten once removed.
	mu.Lock()
	accept = false
	mu.Unlock()
	second, err := srv.AddBreakpoint(Breakpoint{File: "/home/me/app/other.php", Line: 5})
	if err == nil {
		t.Fatal("AddBreakpoint: the second rejection was not reported")
	}
	if err := srv.RemoveBreakpoint(second.ID); err != nil {
		t.Fatal(err)
	}
	sess.syncMu.Lock()
	n, wasSet := len(sess.rejected), sess.bpIDs[rejected.ID] != 0
	sess.syncMu.Unlock()
	if n != 0 || !wasSet {
		t.Errorf("rejected = %d entries, first breakpoint set %v; want none, and set", n, wasSet)
	}
}

// dialRefused reports whether a connection to addr is refused.
func dialRefused(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = conn.Close()
	}
	return err != nil
}

func TestNewServerDoesNotListen(t *testing.T) {
	srv, err := NewServer(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if srv.Listening() || srv.Addr() != "127.0.0.1:0" {
		t.Fatalf("new server: listening %v, addr %s", srv.Listening(), srv.Addr())
	}
	if err := srv.StartListening(); err != nil {
		t.Fatal(err)
	}
	if !srv.Listening() || srv.Port() == 0 {
		t.Fatalf("after start: listening %v, port %d", srv.Listening(), srv.Port())
	}
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	waitSession(t, srv)
}

func TestStopListening(t *testing.T) {
	srv := newTestServer(t, Config{})
	addr := srv.Addr()
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	first := waitSession(t, srv)

	if err := srv.StopListening(); err != nil {
		t.Fatal(err)
	}
	if srv.Listening() || !dialRefused(addr) {
		t.Fatal("still accepting connections after StopListening")
	}
	// The connected session is not affected.
	if st, err := first.Status(); err != nil || st != StatusBreak {
		t.Errorf("connected session: Status = %q, %v", st, err)
	}

	// Restarting reuses the port the server had.
	if err := srv.StartListening(); err != nil {
		t.Fatal(err)
	}
	if srv.Addr() != addr {
		t.Errorf("restarted on %s, want %s", srv.Addr(), addr)
	}
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	if second := waitSession(t, srv); second.ID() != 2 {
		t.Errorf("session after restart has id %d, want 2", second.ID())
	}
}

func TestStartAndStopListeningTwice(t *testing.T) {
	srv := newTestServer(t, Config{})
	if err := srv.StartListening(); err != nil || !srv.Listening() {
		t.Errorf("second StartListening: %v, listening %v", err, srv.Listening())
	}
	for i := 0; i < 2; i++ {
		if err := srv.StopListening(); err != nil || srv.Listening() {
			t.Errorf("StopListening #%d: %v, listening %v", i+1, err, srv.Listening())
		}
	}
}

func TestRestartWhenPortTaken(t *testing.T) {
	srv := newTestServer(t, Config{})
	addr := srv.Addr()
	if err := srv.StopListening(); err != nil {
		t.Fatal(err)
	}
	other, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.StartListening(); err == nil || srv.Listening() {
		t.Errorf("StartListening on a taken port: %v, listening %v", err, srv.Listening())
	}
	_ = other.Close()
	if err := srv.StartListening(); err != nil {
		t.Errorf("StartListening once the port is free: %v", err)
	}
}

func TestStartListeningAfterClose(t *testing.T) {
	srv := newTestServer(t, Config{})
	addr := srv.Addr()
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := srv.StartListening(); !errors.Is(err, ErrServerClosed) {
		t.Errorf("StartListening after Close: %v, want ErrServerClosed", err)
	}
	if !dialRefused(addr) {
		t.Error("port still open after Close")
	}
}

// requireIPv6 skips the test unless the IPv6 loopback address can be used.
func requireIPv6(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	_ = l.Close()
}

// freePort returns a port that is free on the IPv4 loopback address.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// A host name listens on every address it resolves to: for localhost,
// both 127.0.0.1 and ::1, whichever PHP resolves it to.
func TestListenOnHostName(t *testing.T) {
	requireIPv6(t)
	srv := newTestServer(t, Config{Addr: "localhost:0"})
	port := strconv.Itoa(srv.Port())
	if srv.Addr() != "localhost:"+port {
		t.Errorf("Addr() = %q, want localhost:%s", srv.Addr(), port)
	}
	addrs := srv.Addrs()
	sort.Strings(addrs)
	if want := []string{"127.0.0.1:" + port, "[::1]:" + port}; strings.Join(addrs, " ") != strings.Join(want, " ") {
		t.Errorf("Addrs() = %v, want %v", addrs, want)
	}
	if w := srv.ListenWarnings(); len(w) != 0 {
		t.Errorf("ListenWarnings() = %v", w)
	}
	for _, addr := range []string{"127.0.0.1:" + port, "[::1]:" + port} {
		if dialRefused(addr) {
			t.Errorf("%s refused", addr)
		}
	}

	if err := srv.StopListening(); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1:" + port, "[::1]:" + port} {
		if !dialRefused(addr) {
			t.Errorf("%s still accepting after StopListening", addr)
		}
	}
	if err := srv.StartListening(); err != nil || len(srv.Addrs()) != 2 || srv.Addr() != "localhost:"+port {
		t.Errorf("restart: %v, Addrs %v, Addr %q", err, srv.Addrs(), srv.Addr())
	}
}

// If another program holds one of the addresses, the server listens on the
// others and reports it: PHP connecting there would reach that program.
func TestListenWarnsWhenAnAddressIsTaken(t *testing.T) {
	requireIPv6(t)
	port := freePort(t)
	other, err := net.Listen("tcp", "[::1]:"+port)
	if err != nil {
		t.Skipf("cannot take [::1]:%s: %v", port, err)
	}
	defer other.Close()

	srv := newTestServer(t, Config{Addr: "localhost:" + port})
	if addrs := srv.Addrs(); len(addrs) != 1 || addrs[0] != "127.0.0.1:"+port {
		t.Errorf("Addrs() = %v, want only 127.0.0.1:%s", addrs, port)
	}
	if w := srv.ListenWarnings(); len(w) != 1 || !strings.Contains(w[0], "[::1]:"+port) {
		t.Errorf("ListenWarnings() = %v, want one about [::1]:%s", w, port)
	}
	if dialRefused("127.0.0.1:" + port) {
		t.Error("127.0.0.1 refused")
	}
}

func TestListenFailsWhenEveryAddressIsTaken(t *testing.T) {
	requireIPv6(t)
	port := freePort(t)
	for _, addr := range []string{"127.0.0.1:" + port, "[::1]:" + port} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Skipf("cannot take %s: %v", addr, err)
		}
		defer l.Close()
	}
	srv, err := NewServer(Config{Addr: "localhost:" + port})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.StartListening(); err == nil || srv.Listening() {
		t.Errorf("StartListening with every address taken: %v, listening %v", err, srv.Listening())
	}
}

// Once a script has finished, breakpoint changes are not sent to its
// session: the engine refuses them while stopping or stopped, which happens
// just before the connection closes.
func TestBreakpointChangesSkipFinishedSessions(t *testing.T) {
	srv := newTestServer(t, Config{})
	handlers := standardHandlers()
	handlers["run"] = reply("run_stopping")
	e := dialFakeEngine(t, srv, fixture(t, "init"), handlers)
	sess := waitSession(t, srv)
	bp, err := srv.AddBreakpoint(Breakpoint{File: "/app/basic.php", Line: 24})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := sess.Continue(context.Background(), ContinueRun, time.Second); err != nil || st.Status != StatusStopping {
		t.Fatalf("Continue = %+v, %v; want stopping", st, err)
	}

	change := func(when string) {
		t.Helper()
		e.ClearReceived()
		added, err := srv.AddBreakpoint(Breakpoint{File: "/app/basic.php", Line: 30})
		if err != nil {
			t.Errorf("%s: AddBreakpoint: %v", when, err)
		}
		if err := srv.RemoveBreakpoint(added.ID); err != nil {
			t.Errorf("%s: RemoveBreakpoint: %v", when, err)
		}
		if err := srv.AddPathMapping(PathMapping{Local: "/home/me/app", Remote: "/app"}); err != nil {
			t.Errorf("%s: AddPathMapping: %v", when, err)
		}
		if got := breakpointCommands(e); len(got) != 0 {
			t.Errorf("%s: sent %q", when, got)
		}
	}
	change("stopping")
	// The engine reports stopped while the connection is still open.
	e.Handle("status", reply("stop"))
	if st, err := sess.Status(); err != nil || st != StatusStopped {
		t.Fatalf("Status = %q, %v; want stopped", st, err)
	}
	if st := sess.State(); st.Closed || st.Status != StatusStopped {
		t.Fatalf("State = %+v, want stopped and still connected", st)
	}
	change("stopped")
	if err := srv.RemoveBreakpoint(bp.ID); err != nil {
		t.Errorf("removing the first breakpoint: %v", err)
	}
}

// setLimits lowers srv's connection limits for a test.
func setLimits(srv *Server, handshakes, live int) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.maxHandshakes, srv.maxLiveSessions = handshakes, live
}

// Connections that have not sent their init packet are limited: beyond
// the limit, new ones are closed at once.
func TestServerLimitsHandshakes(t *testing.T) {
	srv := newTestServer(t, Config{InitTimeout: time.Minute})
	setLimits(srv, 2, maxLiveSessions)
	silent := []net.Conn{dialServer(t, srv), dialServer(t, srv)}
	deadline := time.Now().Add(3 * time.Second)
	for {
		srv.mu.Lock()
		n := len(srv.handshakes)
		srv.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d handshakes, want 2", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	extra := dialServer(t, srv)
	_ = extra.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("connection beyond the limit: read %v, want it closed", err)
	}

	// Once they go, PHP can connect again.
	for _, c := range silent {
		_ = c.Close()
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		srv.mu.Lock()
		n := len(srv.handshakes)
		srv.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d handshakes left after closing them", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	waitSession(t, srv)
}

// Live sessions are limited: beyond the limit, new scripts are detached
// so they run on without the debugger.
func TestServerLimitsLiveSessions(t *testing.T) {
	srv := newTestServer(t, Config{})
	setLimits(srv, maxHandshakes, 1)
	first := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	waitSession(t, srv)

	extra := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	if !extra.WaitClosed(3 * time.Second) {
		t.Fatal("server kept a session beyond the limit")
	}
	if cmds := extra.Received(); len(cmds) != 1 || cmds[0].Raw != "detach -i 1" {
		t.Errorf("session beyond the limit got %+v, want a single detach", cmds)
	}

	// An ended session no longer counts.
	first.Close()
	select {
	case <-srv.Session(1).Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session 1 did not end")
	}
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	if sess := waitSession(t, srv); sess.ID() != 2 {
		t.Errorf("session %d, want 2", sess.ID())
	}
}

// waitForSessionID waits up to 5s for srv to have session id, without
// taking it.
func waitForSessionID(t *testing.T, srv *Server, id int) *Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess := srv.Session(id); sess != nil {
			return sess
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no session %d", id)
	return nil
}

// Sessions nobody takes within ClaimTimeout are detached, so their scripts
// run on; taken ones, by WaitForSession or Claim, are kept.
func TestServerDetachesUnclaimedSessions(t *testing.T) {
	const timeout = 200 * time.Millisecond
	srv := newTestServer(t, Config{ClaimTimeout: timeout})

	unclaimed := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	sess := waitForSessionID(t, srv, 1)
	if !unclaimed.WaitClosed(3 * time.Second) {
		t.Fatal("unclaimed session was not detached")
	}
	if cmd := unclaimed.Received(); cmd[len(cmd)-1].Name != "detach" {
		t.Errorf("unclaimed session got %v, want detach last", cmd)
	}
	<-sess.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got, err := srv.WaitForSession(ctx); err == nil {
		t.Errorf("WaitForSession returned detached session %d", got.ID())
	}

	waited := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	waitSession(t, srv)
	claimed := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	if err := srv.Claim(waitForSessionID(t, srv, 3)); err != nil {
		t.Errorf("Claim of a new session: %v", err)
	}
	if err := errors.Join(srv.Claim(srv.Session(3)), srv.Claim(srv.Session(2))); err != nil {
		t.Errorf("Claim of a taken session: %v", err)
	}
	time.Sleep(3 * timeout)
	for _, e := range []*fakeEngine{waited, claimed} {
		for _, cmd := range e.Received() {
			if cmd.Name == "detach" {
				t.Errorf("taken session was detached")
			}
		}
	}
	for _, id := range []int{2, 3} {
		if srv.Session(id).State().Closed {
			t.Errorf("session %d ended", id)
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got, err := srv.WaitForSession(ctx); err == nil {
		t.Errorf("WaitForSession returned claimed session %d", got.ID())
	}
}

func TestSessionRemoteAddr(t *testing.T) {
	sess, _ := startFakeSession(t, standardHandlers())
	if host, _, _ := net.SplitHostPort(sess.RemoteAddr().String()); host != "127.0.0.1" {
		t.Errorf("RemoteAddr() = %s, want 127.0.0.1", sess.RemoteAddr())
	}
}

// A connection that sends its init packet and then stops answering is
// dropped once setup takes too long, which frees its handshake slot.
func TestServerDropsSilentSetup(t *testing.T) {
	srv := newTestServer(t, Config{})
	setLimits(srv, 1, maxLiveSessions)
	srv.mu.Lock()
	srv.setupTimeout = 300 * time.Millisecond
	srv.mu.Unlock()

	silent := dialFakeEngine(t, srv, fixture(t, "init"), map[string]fakeHandler{
		"feature_set": func(*fakeEngine, fakeCommand) []string { return nil },
	})
	if !silent.WaitClosed(3 * time.Second) {
		t.Fatal("a connection that stopped answering during setup was kept")
	}
	dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	sess := waitSession(t, srv)
	if got := srv.Sessions(); len(got) != 1 || got[0] != sess {
		t.Errorf("sessions = %v, want only the one that answered", got)
	}
}

// Handshakes setting up at the same time cannot go past the live session
// limit together.
func TestServerLimitsConcurrentHandshakes(t *testing.T) {
	srv := newTestServer(t, Config{})
	setLimits(srv, maxHandshakes, 1)
	slow := func() map[string]fakeHandler {
		handlers := standardHandlers()
		handlers["feature_set"] = func(e *fakeEngine, cmd fakeCommand) []string {
			time.Sleep(100 * time.Millisecond)
			return reply("feature_set")(e, cmd)
		}
		return handlers
	}
	var engines []*fakeEngine
	for range 3 {
		engines = append(engines, dialFakeEngine(t, srv, fixture(t, "init"), slow()))
	}
	detached := func() int {
		n := 0
		for _, e := range engines {
			select {
			case <-e.done:
				n++
			default:
			}
		}
		return n
	}
	waitFor(t, func() bool { return detached() == 2 })
	waitFor(t, func() bool { return len(srv.Sessions()) == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := len(srv.Sessions()); n != 1 || detached() != 2 {
		t.Errorf("%d sessions and %d detached, want 1 and 2", n, detached())
	}
}

// While a session nobody took is being detached, nobody can claim it, so it
// is not detached under them; once it has ended, claiming it is harmless.
func TestClaimWhileDetachingUnclaimed(t *testing.T) {
	answer := make(chan struct{})
	var once sync.Once
	answerDetach := func() { once.Do(func() { close(answer) }) }
	defer answerDetach()
	handlers := standardHandlers()
	handlers["detach"] = func(e *fakeEngine, cmd fakeCommand) []string {
		<-answer
		return reply("detach")(e, cmd)
	}
	srv := newTestServer(t, Config{ClaimTimeout: 100 * time.Millisecond})
	e := dialFakeEngine(t, srv, fixture(t, "init"), handlers)
	sess := waitForSessionID(t, srv, 1)
	waitFor(t, func() bool {
		return slices.ContainsFunc(e.Received(), func(c fakeCommand) bool { return c.Name == "detach" })
	})

	if err := srv.Claim(sess); !errors.Is(err, ErrUnclaimed) {
		t.Errorf("Claim while detaching = %v, want ErrUnclaimed", err)
	}
	answerDetach()
	select {
	case <-sess.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the unclaimed session did not end")
	}
	waitFor(t, func() bool { return srv.Claim(sess) == nil })
}

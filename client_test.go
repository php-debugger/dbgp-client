package dbgp

import (
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWaitForConnectionReadsInit(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, standardHandlers())

	init := c.Init()
	if init == nil {
		t.Fatal("Init() = nil after connection")
	}
	if init.Language != "PHP" || init.Protocol != "1.0" || init.AppID != fixtureAppID || init.FileURI != fixtureScriptURI {
		t.Errorf("init = %+v", init)
	}
}

func TestWaitForConnectionReadsEngineVersion(t *testing.T) {
	knownBug(t, "init-engine-version", "engine version is an attribute, parsed as a child element")
	c := newTestClient(t)
	startFakeEngine(t, c, standardHandlers())
	if got := c.Init().EngineVersion; got != "3.5.3" {
		t.Errorf("EngineVersion = %q, want %q", got, "3.5.3")
	}
}

func TestWaitForConnectionTimeout(t *testing.T) {
	c := newTestClient(t)
	start := time.Now()
	err := c.WaitForConnection(100 * time.Millisecond)
	if err == nil {
		t.Fatal("WaitForConnection succeeded with no engine")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
}

func TestWaitForConnectionRejectsBadInit(t *testing.T) {
	for name, raw := range map[string]string{
		"not xml":         "5\x00hello\x00",
		"bad length":      "abc\x00<init/>\x00",
		"missing nul":     "7\x00<init/>X",
		"wrong root":      "11\x00<response/>\x00",
		"closed too soon": "100\x00<init",
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t)
			waitErr := make(chan error, 1)
			go func() { waitErr <- c.WaitForConnection(5 * time.Second) }()
			conn := dialClient(t, c)
			_, _ = conn.Write([]byte(raw))
			if name == "closed too soon" {
				_ = conn.Close()
			}
			if err := <-waitErr; err == nil {
				t.Fatal("WaitForConnection accepted a bad init packet")
			}
			if _, err := c.Status(); err == nil || !strings.Contains(err.Error(), "not connected") {
				t.Errorf("Status() after failed init: err = %v, want not connected", err)
			}
		})
	}
}

func TestCommandsBeforeConnectionFail(t *testing.T) {
	c := newTestClient(t)
	if _, err := c.Status(); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("Status() err = %v, want not connected", err)
	}
}

func TestPortAndAddr(t *testing.T) {
	c := newTestClient(t)
	if c.Port() == 0 {
		t.Error("Port() = 0")
	}
	if !strings.HasPrefix(c.Addr(), "127.0.0.1:") || !strings.HasSuffix(c.Addr(), ":"+strconv.Itoa(c.Port())) {
		t.Errorf("Addr() = %q, Port() = %d", c.Addr(), c.Port())
	}
}

// Every byte sequence the client sends must be a valid DBGp command: the
// spec frames IDE commands as "command args NUL" with no length prefix.
func TestCommandsAreSpecFramed(t *testing.T) {
	knownBug(t, "length-prefix", "commands are sent as len NUL cmd NUL; Xdebug rejects the length as a command")
	c := newTestClient(t)
	e := startFakeEngine(t, c, standardHandlers())
	if _, err := c.Status(); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range e.Received() {
		if cmd.Err != "" {
			t.Errorf("engine rejected %q: %s", cmd.Raw, cmd.Err)
		}
	}
}

func TestTransactionIDsAreUniqueAndIncreasing(t *testing.T) {
	c := newTestClient(t)
	e := startFakeEngine(t, c, standardHandlers())
	for i := 0; i < 3; i++ {
		if _, err := c.Status(); err != nil {
			t.Fatal(err)
		}
	}
	var ids []int
	for _, cmd := range e.Received() {
		if cmd.Name != "status" {
			continue
		}
		id, err := strconv.Atoi(cmd.Args["i"])
		if err != nil {
			t.Fatalf("command %q has bad -i: %v", cmd.Raw, err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 3 || ids[0] >= ids[1] || ids[1] >= ids[2] {
		t.Errorf("transaction ids = %v, want 3 increasing", ids)
	}
}

func TestExplicitTransactionIDIsKept(t *testing.T) {
	c := newTestClient(t)
	e := startFakeEngine(t, c, standardHandlers())
	if _, err := c.sendCommand("status -i 77"); err != nil {
		t.Fatal(err)
	}
	if got := e.LastCommand("status").Args["i"]; got != "77" {
		t.Errorf("-i = %q, want 77", got)
	}
}

func TestSendCommandRejectsBadTransactionID(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, standardHandlers())
	for _, cmd := range []string{"status -i", "status -i abc"} {
		if _, err := c.sendCommand(cmd); err == nil {
			t.Errorf("sendCommand(%q) succeeded", cmd)
		}
	}
}

// wireCase describes the command an API call must put on the wire.
type wireCase struct {
	name     string
	call     func(c *Client) error
	command  string
	args     map[string]string // expected options, excluding -i
	data     string
	knownBug string
}

func TestCommandWireFormat(t *testing.T) {
	cases := []wireCase{
		{name: "SetBreakpoint", command: "breakpoint_set",
			call: func(c *Client) error { _, err := c.SetBreakpoint("/app/basic.php", 24); return err },
			args: map[string]string{"t": "line", "f": "file:///app/basic.php", "n": "24"}},
		{name: "SetBreakpoint path with spaces", command: "breakpoint_set",
			call: func(c *Client) error { _, err := c.SetBreakpoint("/my app/a b.php", 3); return err },
			args: map[string]string{"t": "line", "f": "file:///my%20app/a%20b.php", "n": "3"}},
		{name: "SetConditionalBreakpoint", command: "breakpoint_set",
			call: func(c *Client) error {
				_, err := c.SetConditionalBreakpoint("/app/basic.php", 39, "$i == 3")
				return err
			},
			args:     map[string]string{"t": "conditional", "f": "file:///app/basic.php", "n": "39"},
			data:     "$i == 3",
			knownBug: "txn-after-data: -i is appended after --, so it becomes part of the data"},
		{name: "RemoveBreakpoint", command: "breakpoint_remove",
			call: func(c *Client) error { return c.RemoveBreakpoint(42420002) },
			args: map[string]string{"d": "42420002"}},
		{name: "ListBreakpoints", command: "breakpoint_list",
			call: func(c *Client) error { _, err := c.ListBreakpoints(); return err },
			args: map[string]string{}},
		{name: "Run", command: "run", call: (*Client).Run, args: map[string]string{}},
		{name: "StepInto", command: "step_into", call: (*Client).StepInto, args: map[string]string{}},
		{name: "StepOver", command: "step_over", call: (*Client).StepOver, args: map[string]string{}},
		{name: "StepOut", command: "step_out", call: (*Client).StepOut, args: map[string]string{}},
		{name: "Stop", command: "stop", call: (*Client).Stop, args: map[string]string{}},
		{name: "Detach", command: "detach", call: (*Client).Detach, args: map[string]string{}},
		{name: "GetStack", command: "stack_get",
			call: func(c *Client) error { _, err := c.GetStack(); return err },
			args: map[string]string{}},
		{name: "GetContext", command: "context_get",
			call: func(c *Client) error { _, err := c.GetContext(1, 2); return err },
			args: map[string]string{"d": "1", "c": "2"}},
		{name: "Eval", command: "eval",
			call:     func(c *Client) error { _, err := c.Eval(`$a . " " . $b`); return err },
			args:     map[string]string{},
			data:     `$a . " " . $b`,
			knownBug: "txn-after-data: -i is appended after --, so it becomes part of the data"},
		{name: "GetSource whole file", command: "source",
			call: func(c *Client) error { _, err := c.GetSource("/app/basic.php", 0, 0); return err },
			args: map[string]string{"f": "file:///app/basic.php"}},
		{name: "GetSource range", command: "source",
			call: func(c *Client) error { _, err := c.GetSource("/app/basic.php", 22, 26); return err },
			args: map[string]string{"f": "file:///app/basic.php", "b": "22", "e": "26"}},
		{name: "Status", command: "status",
			call: func(c *Client) error { _, err := c.Status(); return err },
			args: map[string]string{}},
		{name: "FeatureGet", command: "feature_get",
			call: func(c *Client) error { _, err := c.FeatureGet("max_depth"); return err },
			args: map[string]string{"n": "max_depth"}},
		{name: "FeatureSet", command: "feature_set",
			call: func(c *Client) error { return c.FeatureSet("max_children", "100") },
			args: map[string]string{"n": "max_children", "v": "100"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.knownBug != "" {
				knownBug(t, strings.SplitN(tc.knownBug, ":", 2)[0], tc.knownBug)
			}
			c := newTestClient(t)
			e := startFakeEngine(t, c, standardHandlers())
			if err := tc.call(c); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			got := e.LastCommand(tc.command)
			if got.Err != "" {
				t.Fatalf("engine rejected %q: %s", got.Raw, got.Err)
			}
			args := map[string]string{}
			for k, v := range got.Args {
				if k != "i" {
					args[k] = v
				}
			}
			if !reflect.DeepEqual(args, tc.args) {
				t.Errorf("args = %v, want %v (raw %q)", args, tc.args, got.Raw)
			}
			if got.Data != tc.data {
				t.Errorf("data = %q, want %q", got.Data, tc.data)
			}
		})
	}
}

func TestFeatureArgumentsRejectWhitespace(t *testing.T) {
	c := newTestClient(t)
	e := startFakeEngine(t, c, standardHandlers())
	if _, err := c.FeatureGet("max depth"); err == nil {
		t.Error("FeatureGet accepted a name with whitespace")
	}
	if err := c.FeatureSet("max depth", "1"); err == nil {
		t.Error("FeatureSet accepted a name with whitespace")
	}
	if err := c.FeatureSet("max_depth", "1 2"); err == nil {
		t.Error("FeatureSet accepted a value with whitespace")
	}
	if n := len(e.Received()); n != 0 {
		t.Errorf("engine received %d commands, want 0", n)
	}
}

func TestResponseDecoding(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, standardHandlers())

	t.Run("SetBreakpoint id", func(t *testing.T) {
		id, err := c.SetBreakpoint("/app/basic.php", 24)
		if err != nil || id != 42420001 {
			t.Errorf("SetBreakpoint = %d, %v; want 42420001", id, err)
		}
	})
	t.Run("ListBreakpoints", func(t *testing.T) {
		bps, err := c.ListBreakpoints()
		if err != nil {
			t.Fatal(err)
		}
		var types []string
		for _, bp := range bps {
			types = append(types, bp.Type)
		}
		if want := []string{"line", "conditional", "call", "exception"}; !reflect.DeepEqual(types, want) {
			t.Fatalf("types = %v, want %v", types, want)
		}
		if bps[0].ID != 42420001 || bps[0].Filename != fixtureScriptURI || bps[0].Lineno != 24 || bps[0].State != "enabled" {
			t.Errorf("line breakpoint = %+v", bps[0])
		}
		if bps[2].State != "disabled" {
			t.Errorf("call breakpoint state = %q, want disabled", bps[2].State)
		}
	})
	t.Run("GetStack", func(t *testing.T) {
		stack, err := c.GetStack()
		if err != nil {
			t.Fatal(err)
		}
		want := []StackFrame{
			{Level: 0, Type: "file", Filename: fixtureScriptURI, Lineno: 24, Where: "add"},
			{Level: 1, Type: "file", Filename: fixtureScriptURI, Lineno: 36, Where: "{main}"},
		}
		if !reflect.DeepEqual(stack, want) {
			t.Errorf("stack = %+v, want %+v", stack, want)
		}
	})
	t.Run("GetContext", func(t *testing.T) {
		vars, err := c.GetContext(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := []Variable{
			{Name: "$a", Type: "int", Value: "2"},
			{Name: "$b", Type: "int", Value: "3"},
			{Name: "$sum", Type: "uninitialized", Value: "null"},
		}
		if !reflect.DeepEqual(vars, want) {
			t.Errorf("vars = %+v, want %+v", vars, want)
		}
	})
	t.Run("GetSource", func(t *testing.T) {
		src, err := c.GetSource("/app/basic.php", 22, 26)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(src, "function add($a, $b)\n{\n") || !strings.Contains(src, "return $sum;") {
			t.Errorf("source = %q", src)
		}
	})
	t.Run("Status", func(t *testing.T) {
		if st, err := c.Status(); err != nil || st != StatusBreak {
			t.Errorf("Status = %q, %v; want break", st, err)
		}
	})
	t.Run("FeatureGet", func(t *testing.T) {
		if v, err := c.FeatureGet("max_depth"); err != nil || v != "1" {
			t.Errorf("FeatureGet = %q, %v; want 1", v, err)
		}
	})
}

func TestListBreakpointsReadsExpression(t *testing.T) {
	knownBug(t, "breakpoint-expression", "expression is a base64 child element, parsed as an attribute")
	c := newTestClient(t)
	startFakeEngine(t, c, standardHandlers())
	bps, err := c.ListBreakpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(bps) < 2 || bps[1].Expression != "$i == 3" {
		t.Errorf("conditional breakpoint = %+v, want expression $i == 3", bps)
	}
}

func TestFeatureGetUnsupportedIsAnError(t *testing.T) {
	knownBug(t, "feature-supported", "the supported attribute is ignored")
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{"feature_get": reply("feature_get_unsupported")})
	if v, err := c.FeatureGet("no_such_feature"); err == nil {
		t.Errorf("FeatureGet(no_such_feature) = %q, nil; want an error", v)
	}
}

func TestEvalReturnsValue(t *testing.T) {
	knownBug(t, "eval-raw-xml", "Eval returns the raw property XML instead of the value")
	c := newTestClient(t)
	startFakeEngine(t, c, standardHandlers())
	if v, err := c.Eval("$a + $b"); err != nil || v != "12" {
		t.Errorf("Eval = %q, %v; want 12", v, err)
	}
}

func TestEngineErrorIsReturned(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{"breakpoint_remove": reply("error_breakpoint_not_found")})
	err := c.RemoveBreakpoint(999999)
	if err == nil || !strings.Contains(err.Error(), "205") || !strings.Contains(err.Error(), "no such breakpoint") {
		t.Errorf("RemoveBreakpoint err = %v, want error 205: no such breakpoint", err)
	}
}

func TestEvalErrorIsReturned(t *testing.T) {
	knownBug(t, "txn-after-data", "-i is appended after --, so eval never gets a matching response")
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{"eval": reply("eval_error")})
	_, err := c.Eval("$a +")
	if err == nil || !strings.Contains(err.Error(), "206") {
		t.Errorf("Eval err = %v, want error 206", err)
	}
}

// Xdebug answers commands it cannot parse with an error that carries no
// transaction_id. The caller must get that error, not a 30s timeout.
func TestErrorWithoutTransactionIDReachesCaller(t *testing.T) {
	knownBug(t, "untagged-errors", "errors without transaction_id are dropped and the caller times out")
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{e.fixture("error_invalid_options")}
		},
	})
	errc := make(chan error, 1)
	go func() { _, err := c.Status(); errc <- err }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "invalid or missing options") {
			t.Errorf("Status err = %v, want invalid or missing options", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Status still waiting 3s after the engine replied with an error")
	}
}

func TestStreamAndNotifyPacketsDoNotDisruptCommands(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{
				e.fixture("stream_stdout"),
				e.fixture("notify_breakpoint_resolved"),
				e.withTransaction(e.fixture("status_break"), cmd),
			}
		},
	})
	if st, err := c.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

func TestUnmatchedResponsesAreIgnored(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			stray := fakeCommand{Args: map[string]string{"i": "9999"}}
			return []string{
				e.withTransaction(e.fixture("stop"), stray),
				e.withTransaction(e.fixture("status_break"), cmd),
			}
		},
	})
	if st, err := c.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

func TestDisconnectFailsPendingCommand(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{
		"run": func(e *fakeEngine, cmd fakeCommand) []string {
			e.Close()
			return nil
		},
	})
	errc := make(chan error, 1)
	go func() { errc <- c.Run() }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "connection closed") {
			t.Errorf("Run err = %v, want connection closed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run still waiting 3s after the engine disconnected")
	}
}

func TestCommandAfterDisconnectFailsFast(t *testing.T) {
	knownBug(t, "dead-connection", "after the read loop exits, commands wait for the 30s timeout")
	c := newTestClient(t)
	e := startFakeEngine(t, c, standardHandlers())
	e.Close()
	time.Sleep(100 * time.Millisecond) // let the read loop notice

	errc := make(chan error, 1)
	go func() { _, err := c.Status(); errc <- err }()
	select {
	case err := <-errc:
		if err == nil {
			t.Error("Status succeeded on a closed connection")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Status still waiting 3s after the connection closed")
	}
}

func TestMalformedPacketClosesSession(t *testing.T) {
	for name, raw := range map[string]string{
		"bad length":  "xyz\x00<response/>\x00",
		"missing nul": "10\x00<response>X",
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t)
			startFakeEngine(t, c, map[string]fakeHandler{
				"status": func(e *fakeEngine, cmd fakeCommand) []string {
					e.SendRaw(raw)
					return nil
				},
			})
			errc := make(chan error, 1)
			go func() { _, err := c.Status(); errc <- err }()
			select {
			case err := <-errc:
				if err == nil {
					t.Error("Status succeeded after a malformed packet")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Status still waiting 3s after a malformed packet")
			}
		})
	}
}

func TestUnparseableResponseIsSkipped(t *testing.T) {
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{"<response", e.withTransaction(e.fixture("status_break"), cmd)}
		},
	})
	if st, err := c.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

func TestOnBreakpointCallback(t *testing.T) {
	c := newTestClient(t)
	type hit struct {
		file  string
		line  int
		stack []StackFrame
		vars  []Variable
	}
	hits := make(chan hit, 1)
	c.OnBreakpoint(func(file string, line int, stack []StackFrame, vars []Variable) {
		hits <- hit{file, line, stack, vars}
	})
	startFakeEngine(t, c, standardHandlers())

	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case h := <-hits:
		if h.file != "/app/basic.php" || h.line != 24 {
			t.Errorf("hit at %s:%d, want /app/basic.php:24", h.file, h.line)
		}
		if len(h.stack) != 2 || h.stack[0].Where != "add" {
			t.Errorf("stack = %+v", h.stack)
		}
		if len(h.vars) != 3 || h.vars[0].Name != "$a" {
			t.Errorf("vars = %+v", h.vars)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnBreakpoint not called")
	}
}

func TestNoCallbackWhenNotStoppedAtBreak(t *testing.T) {
	c := newTestClient(t)
	called := make(chan struct{}, 1)
	c.OnBreakpoint(func(string, int, []StackFrame, []Variable) { called <- struct{}{} })
	startFakeEngine(t, c, map[string]fakeHandler{"run": reply("run_stopping")})
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Error("OnBreakpoint called for a stopping response")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestConcurrentCommands(t *testing.T) {
	c := newTestClient(t)
	e := startFakeEngine(t, c, standardHandlers())
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Status(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for _, cmd := range e.Received() {
		if cmd.Name != "status" {
			continue
		}
		if seen[cmd.Args["i"]] {
			t.Errorf("transaction id %s used twice", cmd.Args["i"])
		}
		seen[cmd.Args["i"]] = true
	}
}

func TestCloseDisconnects(t *testing.T) {
	knownBug(t, "close-race", "Close clears c.reader while readLoop reads it without the lock (go test -race)")
	c := newTestClient(t)
	e := startFakeEngine(t, c, standardHandlers())
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !e.WaitClosed(3 * time.Second) {
		t.Error("engine connection still open after Close")
	}
	if _, err := c.Status(); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("Status after Close: err = %v, want not connected", err)
	}
}

// A run that takes longer than the fixed command timeout must still return
// the break response. Takes over 30s while the bug is present.
func TestRunWaitsForSlowBreak(t *testing.T) {
	knownBug(t, "continuation-timeout", "run/step commands time out after 30s")
	if testing.Short() {
		t.Skip("slow test")
	}
	c := newTestClient(t)
	startFakeEngine(t, c, map[string]fakeHandler{
		"run": func(e *fakeEngine, cmd fakeCommand) []string {
			time.Sleep(31 * time.Second)
			return []string{e.withTransaction(e.fixture("run_break"), cmd)}
		},
	})
	if err := c.Run(); err != nil {
		t.Errorf("Run: %v", err)
	}
}

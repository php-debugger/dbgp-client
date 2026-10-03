package dbgp

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionInit(t *testing.T) {
	s, _ := startFakeSession(t, standardHandlers())

	init := s.Init()
	if init == nil {
		t.Fatal("Init() = nil after connection")
	}
	if init.Language != "PHP" || init.Protocol != "1.0" || init.AppID != fixtureAppID || init.FileURI != fixtureScriptURI {
		t.Errorf("init = %+v", init)
	}
}

func TestSessionEngineVersion(t *testing.T) {
	s, _ := startFakeSession(t, standardHandlers())
	if got := s.Init().EngineVersion; got != "0.3.3" {
		t.Errorf("EngineVersion = %q, want %q", got, "0.3.3")
	}
}

// Every byte sequence the client sends must be a valid DBGp command: the
// spec frames IDE commands as "command args NUL" with no length prefix.
func TestCommandsAreSpecFramed(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	if _, err := s.Status(); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range e.Received() {
		if cmd.Err != "" {
			t.Errorf("engine rejected %q: %s", cmd.Raw, cmd.Err)
		}
	}
}

func TestTransactionIDsAreUniqueAndIncreasing(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	for i := 0; i < 3; i++ {
		if _, err := s.Status(); err != nil {
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

func TestFormatCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		data []byte
		want string
	}{
		{"status", nil, nil, "status -i 7"},
		{"breakpoint_set", []string{"-t", "line", "-n", "3"}, nil, "breakpoint_set -t line -n 3 -i 7"},
		{"eval", nil, []byte("$a + 1"), "eval -i 7 -- JGEgKyAx"},
		{"eval", nil, []byte{}, "eval -i 7 -- "},
		{"breakpoint_set", []string{"-t", "conditional"}, []byte("$i == 3"), "breakpoint_set -t conditional -i 7 -- JGkgPT0gMw=="},
		{"feature_set", []string{"-n", "x", "-v", "a b"}, nil, `feature_set -n x -v "a b" -i 7`},
		{"feature_set", []string{"-n", "x", "-v", ""}, nil, `feature_set -n x -v "" -i 7`},
		{"feature_set", []string{"-n", "x", "-v", `say "hi" \o/`}, nil, `feature_set -n x -v "say \"hi\" \\o/" -i 7`},
		{"feature_set", []string{"-n", "x", "-v", `C:\dir`}, nil, `feature_set -n x -v "C:\\dir" -i 7`},
		{"feature_set", []string{"-n", "x", "-v", "tab\there"}, nil, "feature_set -n x -v \"tab\there\" -i 7"},
	}
	for _, tt := range tests {
		got, err := formatCommand(tt.name, 7, tt.args, tt.data)
		if err != nil || got != tt.want {
			t.Errorf("formatCommand(%q, %q, %q) = %q, %v; want %q", tt.name, tt.args, tt.data, got, err, tt.want)
			continue
		}
		// The fake engine parses commands like the real one: round-trip.
		parsed := parseFakeCommand(got)
		if parsed.Err != "" || parsed.Name != tt.name || parsed.Data != string(tt.data) || parsed.Args["i"] != "7" {
			t.Errorf("engine parses %q as %+v", got, parsed)
		}
		for i := 0; i < len(tt.args); i += 2 {
			if v := parsed.Args[tt.args[i][1:]]; v != tt.args[i+1] {
				t.Errorf("engine parses %s in %q as %q, want %q", tt.args[i], got, v, tt.args[i+1])
			}
		}
	}
}

func TestFormatCommandRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"", nil},
		{"two words", nil},
		{"status", []string{"-n"}},
		{"status", []string{"-i", "5"}},
		{"status", []string{"--", "x"}},
		{"status", []string{"-N", "x"}},
		{"status", []string{"n", "x"}},
		{"status", []string{"-nn", "x"}},
		{"status", []string{"-n", "a\x00b"}},
	}
	for _, tt := range tests {
		if got, err := formatCommand(tt.name, 1, tt.args, nil); err == nil {
			t.Errorf("formatCommand(%q, %q) = %q, want error", tt.name, tt.args, got)
		}
	}
}

// wireCase describes the command an API call must put on the wire.
type wireCase struct {
	name    string
	call    func(s *Session) error
	command string
	args    map[string]string // expected options, excluding -i
	data    string
}

func TestCommandWireFormat(t *testing.T) {
	cases := []wireCase{
		{name: "SetBreakpoint", command: "breakpoint_set",
			call: func(s *Session) error { _, err := s.SetBreakpoint("/app/basic.php", 24); return err },
			args: map[string]string{"t": "line", "f": "file:///app/basic.php", "n": "24"}},
		{name: "SetBreakpoint path with spaces", command: "breakpoint_set",
			call: func(s *Session) error { _, err := s.SetBreakpoint("/my app/a b.php", 3); return err },
			args: map[string]string{"t": "line", "f": "file:///my%20app/a%20b.php", "n": "3"}},
		{name: "SetConditionalBreakpoint", command: "breakpoint_set",
			call: func(s *Session) error {
				_, err := s.SetConditionalBreakpoint("/app/basic.php", 39, "$i == 3")
				return err
			},
			args: map[string]string{"t": "conditional", "f": "file:///app/basic.php", "n": "39"},
			data: "$i == 3"},
		{name: "RemoveBreakpoint", command: "breakpoint_remove",
			call: func(s *Session) error { return s.RemoveBreakpoint(42420002) },
			args: map[string]string{"d": "42420002"}},
		{name: "ListBreakpoints", command: "breakpoint_list",
			call: func(s *Session) error { _, err := s.ListBreakpoints(); return err },
			args: map[string]string{}},
		{name: "Continue run", command: "run", call: continueWith(ContinueRun), args: map[string]string{}},
		{name: "Continue step_into", command: "step_into", call: continueWith(ContinueStepInto), args: map[string]string{}},
		{name: "Continue step_over", command: "step_over", call: continueWith(ContinueStepOver), args: map[string]string{}},
		{name: "Continue step_out", command: "step_out", call: continueWith(ContinueStepOut), args: map[string]string{}},
		{name: "Stop", command: "stop", call: (*Session).Stop, args: map[string]string{}},
		{name: "Detach", command: "detach", call: (*Session).Detach, args: map[string]string{}},
		{name: "GetProperty", command: "property_get",
			call: func(s *Session) error { _, err := s.GetProperty("$obj", PropertyOptions{}); return err },
			args: map[string]string{"n": "$obj", "d": "0", "c": "0"}},
		{name: "GetProperty paged", command: "property_get",
			call: func(s *Session) error {
				_, err := s.GetProperty(`$user["first name"]`, PropertyOptions{Depth: 1, Context: 1, Page: 2, MaxData: 100})
				return err
			},
			args: map[string]string{"n": `$user["first name"]`, "d": "1", "c": "1", "p": "2", "m": "100"}},
		{name: "GetPropertyValue", command: "property_value",
			call: func(s *Session) error { _, err := s.GetPropertyValue("$long", PropertyOptions{Depth: 1}); return err },
			args: map[string]string{"n": "$long", "d": "1", "c": "0", "m": "0"}},
		{name: "SetProperty", command: "property_set",
			call: func(s *Session) error { return s.SetProperty("$b", "7 * 2", PropertyOptions{}) },
			args: map[string]string{"n": "$b", "d": "0", "c": "0"},
			data: "7 * 2"},
		{name: "GetContextProperties", command: "context_get",
			call: func(s *Session) error { _, err := s.GetContextProperties(1, 1); return err },
			args: map[string]string{"d": "1", "c": "1"}},
		{name: "ContextNames", command: "context_names",
			call: func(s *Session) error { _, err := s.ContextNames(1); return err },
			args: map[string]string{"d": "1"}},
		{name: "StackDepth", command: "stack_depth",
			call: func(s *Session) error { _, err := s.StackDepth(); return err },
			args: map[string]string{}},
		{name: "GetStackFrame", command: "stack_get",
			call: func(s *Session) error { _, err := s.GetStackFrame(1); return err },
			args: map[string]string{"d": "1"}},
		{name: "EvalProperty paged", command: "eval",
			call: func(s *Session) error { _, err := s.EvalProperty("$numbers", 3); return err },
			args: map[string]string{"p": "3"},
			data: "$numbers"},
		{name: "TypeMap", command: "typemap_get",
			call: func(s *Session) error { _, err := s.TypeMap(); return err },
			args: map[string]string{}},
		{name: "SetBreakpointSpec call", command: "breakpoint_set",
			call: func(s *Session) error {
				_, err := s.SetBreakpointSpec(Breakpoint{Type: BreakpointCall, Function: "App\\add"})
				return err
			},
			args: map[string]string{"t": "call", "m": "App\\add"}},
		{name: "SetBreakpointSpec return", command: "breakpoint_set",
			call: func(s *Session) error {
				_, err := s.SetBreakpointSpec(Breakpoint{Type: BreakpointReturn, Function: "add"})
				return err
			},
			args: map[string]string{"t": "return", "m": "add"}},
		{name: "SetBreakpointSpec exception", command: "breakpoint_set",
			call: func(s *Session) error {
				_, err := s.SetBreakpointSpec(Breakpoint{Type: BreakpointException, Exception: "RuntimeException"})
				return err
			},
			args: map[string]string{"t": "exception", "x": "RuntimeException"}},
		{name: "GetStack", command: "stack_get",
			call: func(s *Session) error { _, err := s.GetStack(); return err },
			args: map[string]string{}},
		{name: "GetContext", command: "context_get",
			call: func(s *Session) error { _, err := s.GetContext(1, 2); return err },
			args: map[string]string{"d": "1", "c": "2"}},
		{name: "Eval", command: "eval",
			call: func(s *Session) error { _, err := s.Eval(`$a . " " . $b`); return err },
			args: map[string]string{},
			data: `$a . " " . $b`},
		{name: "GetSource whole file", command: "source",
			call: func(s *Session) error { _, err := s.GetSource("/app/basic.php", 0, 0); return err },
			args: map[string]string{"f": "file:///app/basic.php"}},
		{name: "GetSource range", command: "source",
			call: func(s *Session) error { _, err := s.GetSource("/app/basic.php", 22, 26); return err },
			args: map[string]string{"f": "file:///app/basic.php", "b": "22", "e": "26"}},
		{name: "Status", command: "status",
			call: func(s *Session) error { _, err := s.Status(); return err },
			args: map[string]string{}},
		{name: "FeatureGet", command: "feature_get",
			call: func(s *Session) error { _, err := s.FeatureGet("max_depth"); return err },
			args: map[string]string{"n": "max_depth"}},
		{name: "FeatureSet", command: "feature_set",
			call: func(s *Session) error { return s.FeatureSet("max_children", "100") },
			args: map[string]string{"n": "max_children", "v": "100"}},
		{name: "FeatureSet quoted value", command: "feature_set",
			call: func(s *Session) error { return s.FeatureSet("idekey", `my "ide" key\`) },
			args: map[string]string{"n": "idekey", "v": `my "ide" key\`}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e := startFakeSession(t, standardHandlers())
			if err := tc.call(s); err != nil {
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
	s, e := startFakeSession(t, standardHandlers())
	if _, err := s.FeatureGet("max depth"); err == nil {
		t.Error("FeatureGet accepted a name with whitespace")
	}
	if err := s.FeatureSet("max depth", "1"); err == nil {
		t.Error("FeatureSet accepted a name with whitespace")
	}
	if n := len(e.Received()); n != 0 {
		t.Errorf("engine received %d commands, want 0", n)
	}
}

func TestResponseDecoding(t *testing.T) {
	s, _ := startFakeSession(t, standardHandlers())

	t.Run("SetBreakpoint id", func(t *testing.T) {
		id, err := s.SetBreakpoint("/app/basic.php", 24)
		if err != nil || id != 42420001 {
			t.Errorf("SetBreakpoint = %d, %v; want 42420001", id, err)
		}
	})
	t.Run("ListBreakpoints", func(t *testing.T) {
		bps, err := s.ListBreakpoints()
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
		stack, err := s.GetStack()
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
		vars, err := s.GetContext(0, 0)
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
		src, err := s.GetSource("/app/basic.php", 22, 26)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(src, "function add($a, $b)\n{\n") || !strings.Contains(src, "return $sum;") {
			t.Errorf("source = %q", src)
		}
	})
	t.Run("Status", func(t *testing.T) {
		if st, err := s.Status(); err != nil || st != StatusBreak {
			t.Errorf("Status = %q, %v; want break", st, err)
		}
	})
	t.Run("FeatureGet", func(t *testing.T) {
		if v, err := s.FeatureGet("max_depth"); err != nil || v != "1" {
			t.Errorf("FeatureGet = %q, %v; want 1", v, err)
		}
	})
}

func TestListBreakpointsReadsExpression(t *testing.T) {
	s, _ := startFakeSession(t, standardHandlers())
	bps, err := s.ListBreakpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(bps) < 2 || bps[1].Expression != "$i == 3" {
		t.Errorf("conditional breakpoint = %+v, want expression $i == 3", bps)
	}
}

func TestFeatureGetUnsupportedIsAnError(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{"feature_get": reply("feature_get_unsupported")})
	if v, err := s.FeatureGet("no_such_feature"); err == nil {
		t.Errorf("FeatureGet(no_such_feature) = %q, nil; want an error", v)
	}
}

func TestEvalReturnsValue(t *testing.T) {
	s, _ := startFakeSession(t, standardHandlers())
	if v, err := s.Eval("$a + $b"); err != nil || v != "12" {
		t.Errorf("Eval = %q, %v; want 12", v, err)
	}
}

func TestErrorResponseIsReturned(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{"breakpoint_remove": reply("error_breakpoint_not_found")})
	err := s.RemoveBreakpoint(999999)
	if err == nil || !strings.Contains(err.Error(), "205") || !strings.Contains(err.Error(), "no such breakpoint") {
		t.Errorf("RemoveBreakpoint err = %v, want error 205: no such breakpoint", err)
	}
}

func TestEvalErrorIsReturned(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{"eval": reply("eval_error")})
	_, err := s.Eval("$a +")
	if err == nil || !strings.Contains(err.Error(), "206") {
		t.Errorf("Eval err = %v, want error 206", err)
	}
}

// The engine (Xdebug and PHP Debugger alike) answers commands it cannot parse
// with an error that carries no transaction_id. The caller must get that
// error, not a 30s timeout.
func TestErrorWithoutTransactionIDReachesCaller(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{e.fixture("error_invalid_options")}
		},
	})
	errc := make(chan error, 1)
	go func() { _, err := s.Status(); errc <- err }()
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
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{
				e.fixture("stream_stdout"),
				e.fixture("notify_breakpoint_resolved"),
				e.withTransaction(e.fixture("status_break"), cmd),
			}
		},
	})
	if st, err := s.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

func TestUnmatchedResponsesAreIgnored(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			stray := fakeCommand{Args: map[string]string{"i": "9999"}}
			return []string{
				e.withTransaction(e.fixture("stop"), stray),
				e.withTransaction(e.fixture("status_break"), cmd),
			}
		},
	})
	if st, err := s.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

func TestDisconnectFailsPendingCommand(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			e.Close()
			return nil
		},
	})
	errc := make(chan error, 1)
	go func() { _, err := s.Status(); errc <- err }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "connection closed") {
			t.Errorf("Status err = %v, want connection closed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Status still waiting 3s after the engine disconnected")
	}
}

func TestCommandAfterDisconnectFailsFast(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	e.Close()
	time.Sleep(100 * time.Millisecond) // let the read loop notice

	errc := make(chan error, 1)
	go func() { _, err := s.Status(); errc <- err }()
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
			s, _ := startFakeSession(t, map[string]fakeHandler{
				"status": func(e *fakeEngine, cmd fakeCommand) []string {
					e.SendRaw(raw)
					return nil
				},
			})
			errc := make(chan error, 1)
			go func() { _, err := s.Status(); errc <- err }()
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
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{"<response", e.withTransaction(e.fixture("status_break"), cmd)}
		},
	})
	if st, err := s.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

func TestConcurrentCommands(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Status(); err != nil {
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
	s, e := startFakeSession(t, standardHandlers())
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !e.WaitClosed(3 * time.Second) {
		t.Error("engine connection still open after Close")
	}
	<-s.Done()
	if _, err := s.Status(); err == nil || !strings.Contains(err.Error(), "connection closed") {
		t.Errorf("Status after Close: err = %v, want connection closed", err)
	}
	if st := s.State(); !st.Closed || st.Status != StatusStopped {
		t.Errorf("State after Close = %+v, want closed and stopped", st)
	}
}

// After an unimplemented command the engine sends a response with neither
// command nor transaction_id once the script ends. It answers no command.
func TestUntaggedNonErrorResponseIsIgnored(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{
				e.fixture("error_unimplemented_after_1"),
				e.withTransaction(e.fixture("status_break"), cmd),
			}
		},
	})
	if st, err := s.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

// An untagged error that arrives while no command is waiting must not be
// handed to the next command.
func TestStrayUntaggedErrorIsDropped(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	e.Send(e.fixture("error_invalid_options"))
	time.Sleep(50 * time.Millisecond)
	if st, err := s.Status(); err != nil || st != StatusBreak {
		t.Errorf("Status = %q, %v; want break", st, err)
	}
}

// Only one command may be in flight: the client must not send another
// until the engine has answered the first.
func TestCommandsAreSentOneAtATime(t *testing.T) {
	release := make(chan struct{})
	first := true
	s, e := startFakeSession(t, map[string]fakeHandler{
		"status": func(e *fakeEngine, cmd fakeCommand) []string {
			if first {
				first = false
				<-release
			}
			return []string{e.withTransaction(e.fixture("status_break"), cmd)}
		},
	})

	errs := make(chan error, 2)
	go func() { _, err := s.Status(); errs <- err }()
	waitFor(t, func() bool { return len(e.Received()) == 1 })
	go func() { _, err := s.Status(); errs <- err }()

	time.Sleep(200 * time.Millisecond)
	if got := len(e.Received()); got != 1 {
		t.Errorf("engine received %d commands while the first was unanswered, want 1", got)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("Status: %v", err)
		}
	}
}

// continueWith returns a wire-format call that continues with command.
func continueWith(command string) func(s *Session) error {
	return func(s *Session) error {
		_, err := s.Continue(context.Background(), command, time.Second)
		return err
	}
}

// waitFor polls cond until it holds, failing the test after 3s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met after 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestContinueStopsAtBreak(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	st, err := s.Continue(context.Background(), ContinueRun, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := State{Status: StatusBreak, Reason: "ok", File: "/app/basic.php", Line: lineAddBody}
	if st != want {
		t.Errorf("State = %+v, want %+v", st, want)
	}
	if got := s.State(); got != want {
		t.Errorf("State() = %+v, want %+v", got, want)
	}
	if e.LastCommand("run").Err != "" {
		t.Error("engine rejected run")
	}
	if _, err := s.GetStack(); err != nil {
		t.Errorf("GetStack at break: %v", err)
	}
}

func TestContinueStepsReportLocation(t *testing.T) {
	s, _ := startFakeSession(t, standardHandlers())
	for command, line := range map[string]int{ContinueStepOver: 25, ContinueStepOut: 37, ContinueStepInto: 38} {
		st, err := s.Continue(context.Background(), command, time.Second)
		if err != nil || st.Status != StatusBreak || st.Line != line {
			t.Errorf("Continue(%s) = %+v, %v; want break at line %d", command, st, err, line)
		}
	}
}

func TestContinueToEndOfScript(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{"run": reply("run_stopping")})
	st, err := s.Continue(context.Background(), ContinueRun, time.Second)
	if err != nil || st.Status != StatusStopping || st.File != "" {
		t.Errorf("Continue = %+v, %v; want stopping", st, err)
	}
}

func TestContinueOnException(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{"run": reply("run_break_exception")})
	st, err := s.Continue(context.Background(), ContinueRun, time.Second)
	if err != nil || st.Exception != "RuntimeException" || st.Message != "boom" || st.Line != lineThrow {
		t.Errorf("Continue = %+v, %v; want RuntimeException boom at line %d", st, err, lineThrow)
	}
}

// A continuation is not bounded by a fixed timeout: the caller chooses how
// long to wait, and can keep waiting.
func TestContinueReturnsWhileRunning(t *testing.T) {
	release := make(chan struct{})
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"run": func(e *fakeEngine, cmd fakeCommand) []string {
			<-release
			return []string{e.withTransaction(e.fixture("run_break"), cmd)}
		},
		"status": reply("status_break"),
	})

	st, err := s.Continue(context.Background(), ContinueRun, 50*time.Millisecond)
	if err != nil || st.Status != StatusRunning {
		t.Fatalf("Continue = %+v, %v; want running", st, err)
	}
	if _, err := s.Status(); !errors.Is(err, ErrRunning) {
		t.Errorf("Status while running: err = %v, want ErrRunning", err)
	}
	if _, err := s.Continue(context.Background(), ContinueRun, 0); !errors.Is(err, ErrRunning) {
		t.Errorf("Continue while running: err = %v, want ErrRunning", err)
	}
	if st, err := s.Wait(context.Background(), 50*time.Millisecond); err != nil || st.Status != StatusRunning {
		t.Errorf("Wait = %+v, %v; want still running", st, err)
	}

	close(release)
	st, err = s.Wait(context.Background(), time.Second)
	if err != nil || st.Status != StatusBreak || st.Line != lineAddBody {
		t.Errorf("Wait = %+v, %v; want break at line %d", st, err, lineAddBody)
	}
	if _, err := s.Status(); err != nil {
		t.Errorf("Status after break: %v", err)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"run": func(e *fakeEngine, cmd fakeCommand) []string { return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st, err := s.Continue(ctx, ContinueRun, time.Minute)
	if !errors.Is(err, context.Canceled) || st.Status != StatusRunning {
		t.Errorf("Continue = %+v, %v; want running, context.Canceled", st, err)
	}
}

func TestContinueRejectedByEngine(t *testing.T) {
	for name, handler := range map[string]fakeHandler{
		"tagged": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{errorPacket(cmd, 5, "command is not available")}
		},
		"untagged": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{e.fixture("error_invalid_options")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := startFakeSession(t, map[string]fakeHandler{"run": handler})
			st, err := s.Continue(context.Background(), ContinueRun, time.Second)
			if err == nil || st.Status != StatusStarting || st.Error == "" {
				t.Errorf("Continue = %+v, %v; want error and status back to starting", st, err)
			}
		})
	}
}

func TestContinueRejectsOtherCommands(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	if _, err := s.Continue(context.Background(), "eval", time.Second); err == nil {
		t.Error("Continue(eval) succeeded")
	}
	if n := len(e.Received()); n != 0 {
		t.Errorf("engine received %d commands, want 0", n)
	}
}

func TestDisconnectEndsContinuation(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"run": func(e *fakeEngine, cmd fakeCommand) []string {
			e.Close()
			return nil
		},
	})
	st, err := s.Continue(context.Background(), ContinueRun, 3*time.Second)
	if err != nil || !st.Closed || st.Status != StatusStopped {
		t.Errorf("Continue = %+v, %v; want closed and stopped", st, err)
	}
	if _, err := s.Continue(context.Background(), ContinueRun, 0); err == nil {
		t.Error("Continue succeeded on a closed session")
	}
}

func TestStopUpdatesState(t *testing.T) {
	s, _ := startFakeSession(t, standardHandlers())
	if _, err := s.Continue(context.Background(), ContinueRun, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Status != StatusStopped || st.File != "" {
		t.Errorf("State after stop = %+v, want stopped with no location", st)
	}
}

func TestOutputIsCaptured(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	stream := func(text string) string {
		return `<stream xmlns="urn:debugger_protocol_v1" type="stdout" encoding="base64">` +
			base64.StdEncoding.EncodeToString([]byte(text)) + `</stream>`
	}
	e.Send(stream("x=5\n"))
	e.Send(e.fixture("stream_stdout")) // "done\n"
	waitFor(t, func() bool { _, next, _ := s.Output(0); return next == 9 })

	if text, next, truncated := s.Output(0); text != "x=5\ndone\n" || next != 9 || truncated {
		t.Errorf("Output(0) = %q, %d, %v", text, next, truncated)
	}
	if text, next, _ := s.Output(4); text != "done\n" || next != 9 {
		t.Errorf("Output(4) = %q, %d", text, next)
	}
	if text, next, _ := s.Output(100); text != "" || next != 9 {
		t.Errorf("Output(100) = %q, %d", text, next)
	}
}

func TestOutputIsBounded(t *testing.T) {
	s := &Session{}
	s.appendOutput(strings.Repeat("a", maxOutputBytes))
	s.appendOutput("bcd")
	text, next, truncated := s.Output(0)
	if len(text) != maxOutputBytes || !strings.HasSuffix(text, "abcd") || next != maxOutputBytes+3 || !truncated {
		t.Errorf("Output(0) = %d bytes ending %q, next %d, truncated %v", len(text), text[len(text)-4:], next, truncated)
	}
}

func TestNotificationsAreCollected(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())
	e.Send(e.fixture("notify_error"))
	e.Send(e.fixture("notify_breakpoint_resolved"))
	waitFor(t, func() bool { _, next := s.Notifications(0); return next == 2 })

	notes, next := s.Notifications(0)
	if next != 2 || notes[0].Name != "error" || notes[1].Name != "breakpoint_resolved" {
		t.Fatalf("Notifications = %+v, %d", notes, next)
	}
	want := Message{Filename: "/app/basic.php", Lineno: lineWarning, Type: "Warning", Text: "Undefined variable $undefinedVariable"}
	if *notes[0].Message != want {
		t.Errorf("error notification = %+v, want %+v", *notes[0].Message, want)
	}
	if bp := notes[1].Breakpoint; bp == nil || bp.ID != 42420002 || bp.Expression != "$i == 3" {
		t.Errorf("resolved breakpoint = %+v", bp)
	}
	if later, next := s.Notifications(1); len(later) != 1 || next != 2 {
		t.Errorf("Notifications(1) = %+v, %d", later, next)
	}
}

func TestNotificationsAreBounded(t *testing.T) {
	s := &Session{}
	for i := 0; i < maxNotifications+5; i++ {
		s.addNotification(Notification{Name: strconv.Itoa(i)})
	}
	notes, next := s.Notifications(0)
	if len(notes) != maxNotifications || notes[0].Name != "5" || next != maxNotifications+5 {
		t.Errorf("Notifications(0) = %d notes from %q, next %d", len(notes), notes[0].Name, next)
	}
}

func TestInspectionDecoding(t *testing.T) {
	s, e := startFakeSession(t, standardHandlers())

	t.Run("GetProperty", func(t *testing.T) {
		p, err := s.GetProperty("$obj", PropertyOptions{Depth: 1})
		if err != nil {
			t.Fatal(err)
		}
		if p.FullName != "$obj" || p.ClassName != "Service" || p.NumChildren != 3 || p.PageSize != 10 || p.Pages() != 1 {
			t.Errorf("property = %+v", p)
		}
		var names []string
		for _, c := range p.ChildProperties {
			names = append(names, c.FullName+"/"+c.Facet)
		}
		if want := "$obj->id/public $obj->cache/protected $obj->name/private"; strings.Join(names, " ") != want {
			t.Errorf("children = %q, want %q", strings.Join(names, " "), want)
		}
		if v, err := p.ChildProperties[2].DecodedValue(); err != nil || v != "svc" {
			t.Errorf("$obj->name = %q, %v", v, err)
		}
	})
	t.Run("GetProperty page", func(t *testing.T) {
		e.Handle("property_get", reply("property_get_page"))
		defer e.Handle("property_get", reply("property_get_object"))
		p, err := s.GetProperty("$numbers", PropertyOptions{Depth: 1, Page: 1})
		if err != nil {
			t.Fatal(err)
		}
		if p.Page != 1 || p.Pages() != 4 || len(p.ChildProperties) != 10 || p.ChildProperties[0].FullName != "$numbers[10]" {
			t.Errorf("page = %+v", p)
		}
	})
	t.Run("GetProperty truncated", func(t *testing.T) {
		e.Handle("property_get", reply("property_get_maxdata"))
		defer e.Handle("property_get", reply("property_get_object"))
		p, err := s.GetProperty("$long", PropertyOptions{Depth: 1, MaxData: 20})
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := p.DecodedValue(); v != "abcdefghijabcdefghij" || p.Size != 200 || !p.Truncated() {
			t.Errorf("value %q, size %d, truncated %v", v, p.Size, p.Truncated())
		}
	})
	t.Run("GetPropertyValue", func(t *testing.T) {
		v, err := s.GetPropertyValue("$long", PropertyOptions{Depth: 1})
		if err != nil || v != strings.Repeat("abcdefghij", 20) {
			t.Errorf("GetPropertyValue = %q, %v", v, err)
		}
	})
	t.Run("SetProperty", func(t *testing.T) {
		if err := s.SetProperty("$b", "10", PropertyOptions{}); err != nil {
			t.Error(err)
		}
	})
	t.Run("ContextNames", func(t *testing.T) {
		names, err := s.ContextNames(0)
		want := []ContextName{{0, "Locals"}, {1, "Superglobals"}, {2, "User defined constants"}}
		if err != nil || !reflect.DeepEqual(names, want) {
			t.Errorf("ContextNames = %+v, %v", names, err)
		}
	})
	t.Run("StackDepth", func(t *testing.T) {
		if d, err := s.StackDepth(); err != nil || d != 2 {
			t.Errorf("StackDepth = %d, %v; want 2", d, err)
		}
	})
	t.Run("GetStackFrame", func(t *testing.T) {
		e.Handle("stack_get", reply("stack_get_depth"))
		defer e.Handle("stack_get", reply("stack_get"))
		f, err := s.GetStackFrame(1)
		if err != nil || f.Level != 1 || f.Where != "{main}" || f.Lineno != lineCallAdd {
			t.Errorf("GetStackFrame = %+v, %v", f, err)
		}
	})
	t.Run("GetContextProperties", func(t *testing.T) {
		props, err := s.GetContextProperties(0, 0)
		if err != nil || len(props) != 3 || props[0].FullName != "$a" || props[2].Type != "uninitialized" {
			t.Errorf("GetContextProperties = %+v, %v", props, err)
		}
	})
	t.Run("EvalProperty", func(t *testing.T) {
		e.Handle("eval", reply("eval_array"))
		defer e.Handle("eval", reply("eval"))
		p, err := s.EvalProperty("[$a, $b, 'k' => 'v']", 0)
		if err != nil || p.Type != "array" || p.NumChildren != 3 || len(p.ChildProperties) != 3 {
			t.Fatalf("EvalProperty = %+v, %v", p, err)
		}
		if v, _ := p.ChildProperties[2].DecodedValue(); p.ChildProperties[2].Name != "k" || v != "v" {
			t.Errorf("child k = %+v", p.ChildProperties[2])
		}
		if v, err := s.Eval("[$a, $b, 'k' => 'v']"); err != nil || v != "array[3]" {
			t.Errorf("Eval = %q, %v; want array[3]", v, err)
		}
	})
	t.Run("TypeMap", func(t *testing.T) {
		types, err := s.TypeMap()
		if err != nil || len(types) != 8 {
			t.Fatalf("TypeMap = %+v, %v", types, err)
		}
		if want := (TypeMapEntry{Name: "int", Type: "int", SchemaType: "xsd:decimal"}); types[1] != want {
			t.Errorf("types[1] = %+v, want %+v", types[1], want)
		}
		if want := (TypeMapEntry{Name: "array", Type: "hash"}); types[5] != want {
			t.Errorf("types[5] = %+v, want %+v", types[5], want)
		}
	})
}

func TestEngineErrorCarriesCode(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{"property_get": reply("error_property_not_found")})
	_, err := s.GetProperty("$nope", PropertyOptions{})
	var engineErr *EngineError
	if !errors.As(err, &engineErr) || engineErr.Code != 300 || engineErr.Command != "property_get" {
		t.Fatalf("err = %#v, want *EngineError with code 300", err)
	}
	if err.Error() != "error 300: can not get property" {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestSetPropertyFailure(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"property_set": func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{strings.Replace(e.withTransaction(e.fixture("property_set"), cmd), `success="1"`, `success="0"`, 1)}
		},
	})
	if err := s.SetProperty("$b", "1", PropertyOptions{}); err == nil {
		t.Error("SetProperty succeeded although the engine reported failure")
	}
}

package dbgp

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Tests in this file drive Client against a real PHP with PHP Debugger or
// Xdebug and are skipped when neither is installed (or with -short).

// startEngineSession returns a client connected to the debuggee, stopped at
// the start of the script.
func startEngineSession(t *testing.T) *Client {
	t.Helper()
	requireDebugEngine(t)
	c := newTestClient(t)
	startDebuggee(t, c.Port())
	if err := c.WaitForConnection(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEngineSession(t *testing.T) {
	c := startEngineSession(t)
	script := debuggeePath(t)

	init := c.Init()
	if init.Language != "PHP" || init.Protocol != "1.0" || init.FileURI != MakeFileURI(script) {
		t.Fatalf("init = %+v", init)
	}
	if st, err := c.Status(); err != nil || st != StatusStarting {
		t.Fatalf("Status = %q, %v; want starting", st, err)
	}
	if err := c.FeatureSet("max_children", "10"); err != nil {
		t.Fatal(err)
	}
	if v, err := c.FeatureGet("max_children"); err != nil || v != "10" {
		t.Fatalf("FeatureGet(max_children) = %q, %v", v, err)
	}

	id, err := c.SetBreakpoint(script, lineAddBody)
	if err != nil || id == 0 {
		t.Fatalf("SetBreakpoint = %d, %v", id, err)
	}
	bps, err := c.ListBreakpoints()
	if err != nil || len(bps) != 1 || bps[0].ID != id || bps[0].Lineno != lineAddBody {
		t.Fatalf("ListBreakpoints = %+v, %v", bps, err)
	}

	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Status(); err != nil || st != StatusBreak {
		t.Fatalf("Status after run = %q, %v; want break", st, err)
	}
	stack, err := c.GetStack()
	if err != nil || len(stack) != 2 || stack[0].Where != "add" || stack[0].Lineno != lineAddBody || stack[1].Lineno != lineCallAdd {
		t.Fatalf("GetStack = %+v, %v", stack, err)
	}
	if got := FormatFileURI(stack[0].Filename); got != script {
		t.Errorf("frame file = %q, want %q", got, script)
	}
	vars, err := c.GetContext(0, 0)
	if err != nil || len(vars) != 3 || vars[0].Name != "$a" || vars[0].Value != "2" || vars[1].Value != "3" {
		t.Fatalf("GetContext = %+v, %v", vars, err)
	}
	src, err := c.GetSource(script, lineAddBody, lineAddReturn)
	if err != nil || !strings.Contains(src, "$sum = $a + $b;") || !strings.Contains(src, "return $sum;") {
		t.Fatalf("GetSource = %q, %v", src, err)
	}

	if err := c.StepOver(); err != nil {
		t.Fatal(err)
	}
	if stack, _ := c.GetStack(); len(stack) == 0 || stack[0].Lineno != lineAddReturn {
		t.Errorf("after step_over: stack = %+v, want line %d", stack, lineAddReturn)
	}
	if err := c.StepOut(); err != nil {
		t.Fatal(err)
	}
	if stack, _ := c.GetStack(); len(stack) != 1 || stack[0].Where != "{main}" {
		t.Errorf("after step_out: stack = %+v, want {main}", stack)
	}
	if err := c.StepInto(); err != nil {
		t.Fatal(err)
	}

	if err := c.RemoveBreakpoint(id); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveBreakpoint(id); err == nil || !strings.Contains(err.Error(), "205") {
		t.Errorf("second RemoveBreakpoint err = %v, want error 205", err)
	}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Status(); err != nil || st != StatusStopping {
		t.Errorf("Status at end = %q, %v; want stopping", st, err)
	}
	if err := c.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestEngineBreakpointCallback(t *testing.T) {
	c := startEngineSession(t)
	hits := make(chan string, 1)
	c.OnBreakpoint(func(file string, line int, stack []StackFrame, vars []Variable) {
		hits <- fmt.Sprintf("%s:%d %d %d", file, line, len(stack), len(vars))
	})
	if _, err := c.SetBreakpoint(debuggeePath(t), lineAddBody); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-hits:
		if want := fmt.Sprintf("%s:%d 2 3", debuggeePath(t), lineAddBody); got != want {
			t.Errorf("hit = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnBreakpoint not called")
	}
}

func TestEngineConditionalBreakpoint(t *testing.T) {
	knownBug(t, "txn-after-data", "-i is appended after --; the engine rejects the command")
	c := startEngineSession(t)
	if _, err := c.SetConditionalBreakpoint(debuggeePath(t), lineLoopBody, "$i == 3"); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	vars, err := c.GetContext(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vars {
		if v.Name == "$i" && v.Value != "3" {
			t.Errorf("$i = %s, want 3", v.Value)
		}
	}
}

func TestEngineEval(t *testing.T) {
	knownBug(t, "txn-after-data", "-i is appended after --; the engine rejects the command")
	c := startEngineSession(t)
	if _, err := c.SetBreakpoint(debuggeePath(t), lineAddBody); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Eval("$a + $b"); err != nil || v != "5" {
		t.Errorf("Eval = %q, %v; want 5", v, err)
	}
}

func TestEngineReportsVersion(t *testing.T) {
	knownBug(t, "init-engine-version", "engine version is an attribute, parsed as a child element")
	c := startEngineSession(t)
	if c.Init().EngineVersion == "" {
		t.Error("EngineVersion is empty")
	}
}

func TestEngineDetach(t *testing.T) {
	c := startEngineSession(t)
	if err := c.Detach(); err != nil {
		t.Fatal(err)
	}
}

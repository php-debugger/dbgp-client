package dbgp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests in this file drive a Server against a real PHP with PHP Debugger or
// Xdebug and are skipped when neither is installed (or with -short).

// stepWait is how long a test lets a continuation run before failing.
const stepWait = 10 * time.Second

// startEngineSession starts a server and the debuggee and returns the
// session, stopped at the start of the script.
func startEngineSession(t *testing.T, cfg Config) (*Server, *Session) {
	t.Helper()
	requireDebugEngine(t)
	srv := newTestServer(t, cfg)
	startDebuggee(t, srv.Port())
	return srv, waitSession(t, srv)
}

// mustContinue continues sess and fails the test unless it stops.
func mustContinue(t *testing.T, sess *Session, command string) State {
	t.Helper()
	st, err := sess.Continue(context.Background(), command, stepWait)
	if err != nil {
		t.Fatalf("Continue(%s): %v", command, err)
	}
	if st.Status == StatusRunning {
		t.Fatalf("Continue(%s): still running after %v", command, stepWait)
	}
	return st
}

func TestEngineSession(t *testing.T) {
	_, sess := startEngineSession(t, Config{Features: map[string]string{"max_children": "10"}})
	script := debuggeePath(t)

	init := sess.Init()
	if init.Language != "PHP" || init.Protocol != "1.0" || init.FileURI != MakeFileURI(script) {
		t.Fatalf("init = %+v", init)
	}
	if err := sess.SetupError(); err != nil {
		t.Fatalf("SetupError: %v", err)
	}
	if m := sess.EngineMapping(); !m.Detected || m.Enabled || m.RemoteScript != script {
		t.Errorf("EngineMapping() = %+v, want detected, off, %s", m, script)
	}
	if st := sess.State(); st.Status != StatusStarting {
		t.Fatalf("State = %+v, want starting", st)
	}
	if v, err := sess.FeatureGet("max_children"); err != nil || v != "10" {
		t.Fatalf("FeatureGet(max_children) = %q, %v", v, err)
	}

	id, err := sess.SetBreakpoint(script, lineAddBody)
	if err != nil || id == 0 {
		t.Fatalf("SetBreakpoint = %d, %v", id, err)
	}
	bps, err := sess.ListBreakpoints()
	if err != nil || len(bps) != 1 || bps[0].ID != id || bps[0].Lineno != lineAddBody {
		t.Fatalf("ListBreakpoints = %+v, %v", bps, err)
	}

	st := mustContinue(t, sess, ContinueRun)
	if st.Status != StatusBreak || st.File != script || st.Line != lineAddBody {
		t.Fatalf("after run: %+v, want break at %s:%d", st, script, lineAddBody)
	}
	stack, err := sess.GetStack()
	if err != nil || len(stack) != 2 || stack[0].Where != "add" || stack[1].Lineno != lineCallAdd {
		t.Fatalf("GetStack = %+v, %v", stack, err)
	}
	vars, err := sess.GetContext(0, 0)
	if err != nil || len(vars) != 3 || vars[0].Name != "$a" || vars[0].Value != "2" || vars[1].Value != "3" {
		t.Fatalf("GetContext = %+v, %v", vars, err)
	}
	if v, err := sess.Eval("$a + $b"); err != nil || v != "5" {
		t.Errorf("Eval = %q, %v; want 5", v, err)
	}
	src, err := sess.GetSource(script, lineAddBody, lineAddReturn)
	if err != nil || !strings.Contains(src, "$sum = $a + $b;") || !strings.Contains(src, "return $sum;") {
		t.Fatalf("GetSource = %q, %v", src, err)
	}

	if st := mustContinue(t, sess, ContinueStepOver); st.Line != lineAddReturn {
		t.Errorf("after step_over: %+v, want line %d", st, lineAddReturn)
	}
	if st := mustContinue(t, sess, ContinueStepOut); st.Line != lineCallAdd+1 {
		t.Errorf("after step_out: %+v, want line %d", st, lineCallAdd+1)
	}
	mustContinue(t, sess, ContinueStepInto)

	if err := sess.RemoveBreakpoint(id); err != nil {
		t.Fatal(err)
	}
	if err := sess.RemoveBreakpoint(id); err == nil || !strings.Contains(err.Error(), "205") {
		t.Errorf("second RemoveBreakpoint err = %v, want error 205", err)
	}
	if st := mustContinue(t, sess, ContinueRun); st.Status != StatusStopping {
		t.Errorf("at end: %+v, want stopping", st)
	}
	if err := sess.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed after stop")
	}
	if st := sess.State(); !st.Closed {
		t.Errorf("State after stop = %+v, want closed", st)
	}
}

// Breakpoints added to the server before PHP starts apply to its session.
func TestEnginePendingBreakpoint(t *testing.T) {
	requireDebugEngine(t)
	srv := newTestServer(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{File: debuggeePath(t), Line: lineAddBody}); err != nil {
		t.Fatal(err)
	}
	startDebuggee(t, srv.Port())
	sess := waitSession(t, srv)
	if st := mustContinue(t, sess, ContinueRun); st.Status != StatusBreak || st.Line != lineAddBody {
		t.Errorf("after run: %+v, want break at line %d", st, lineAddBody)
	}
}

func TestEngineOutputAndNotifications(t *testing.T) {
	_, sess := startEngineSession(t, Config{})
	if st := mustContinue(t, sess, ContinueRun); st.Status != StatusStopping {
		t.Fatalf("after run: %+v, want stopping", st)
	}
	if out, _, _ := sess.Output(0); out != "x=5\ndone\n" {
		t.Errorf("Output = %q, want %q", out, "x=5\ndone\n")
	}
	notes, _ := sess.Notifications(0)
	var warning *Message
	for _, n := range notes {
		if n.Name == "error" {
			warning = n.Message
		}
	}
	if warning == nil || warning.Type != "Warning" || warning.Lineno != lineWarning ||
		warning.Filename != debuggeePath(t) || !strings.Contains(warning.Text, "$undefinedVariable") {
		t.Errorf("warning notification = %+v; notifications = %+v", warning, notes)
	}
}

func TestEngineExceptionBreakpoint(t *testing.T) {
	srv, sess := startEngineSession(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{Type: BreakpointException, Exception: "RuntimeException"}); err != nil {
		t.Fatal(err)
	}
	st := mustContinue(t, sess, ContinueRun)
	if st.Status != StatusBreak || st.Line != lineThrow || st.Exception != "RuntimeException" || st.Message != "boom" {
		t.Errorf("after run: %+v, want RuntimeException boom at line %d", st, lineThrow)
	}
}

func TestEngineConditionalBreakpoint(t *testing.T) {
	srv, sess := startEngineSession(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{Type: BreakpointConditional, File: debuggeePath(t), Line: lineLoopBody, Condition: "$i == 3"}); err != nil {
		t.Fatal(err)
	}
	if st := mustContinue(t, sess, ContinueRun); st.Line != lineLoopBody {
		t.Fatalf("after run: %+v, want line %d", st, lineLoopBody)
	}
	if v, err := sess.Eval("$i"); err != nil || v != "3" {
		t.Errorf("$i = %q, %v; want 3", v, err)
	}
}

func TestEngineReportsVersion(t *testing.T) {
	_, sess := startEngineSession(t, Config{})
	if e := sess.Init().Engine; e.Name == "" || e.Version == "" {
		t.Errorf("Engine = %+v, want name and version", e)
	}
}

func TestEngineDetach(t *testing.T) {
	_, sess := startEngineSession(t, Config{})
	if err := sess.Detach(); err != nil {
		t.Fatal(err)
	}
}

// Two scripts debugged at once get independent sessions.
func TestEngineConcurrentSessions(t *testing.T) {
	requireDebugEngine(t)
	srv := newTestServer(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{File: debuggeePath(t), Line: lineAddBody}); err != nil {
		t.Fatal(err)
	}
	startDebuggee(t, srv.Port())
	startDebuggee(t, srv.Port())
	first, second := waitSession(t, srv), waitSession(t, srv)
	if first.ID() == second.ID() {
		t.Fatalf("both sessions have id %d", first.ID())
	}

	if st := mustContinue(t, first, ContinueRun); st.Line != lineAddBody {
		t.Fatalf("first: %+v", st)
	}
	if st := second.State(); st.Status != StatusStarting {
		t.Errorf("second moved when first was continued: %+v", st)
	}
	if st := mustContinue(t, second, ContinueRun); st.Line != lineAddBody {
		t.Fatalf("second: %+v", st)
	}
	if st := mustContinue(t, first, ContinueRun); st.Status != StatusStopping {
		t.Errorf("first at end: %+v", st)
	}
	if got := len(srv.Sessions()); got != 2 {
		t.Errorf("Sessions() has %d sessions, want 2", got)
	}
}

// Sessions for another IDE key are detached and their script runs on.
func TestEngineIDEKeyFilter(t *testing.T) {
	requireDebugEngine(t)
	srv := newTestServer(t, Config{IDEKey: "agent"})

	other := startDebuggee(t, srv.Port(), "xdebug.idekey=someone-else")
	select {
	case <-other.exited:
	case <-time.After(stepWait):
		t.Fatal("script for another IDE key did not run to completion")
	}
	if !strings.Contains(other.out.String(), "done") {
		t.Errorf("script output = %q, want it to have run", other.out.String())
	}
	if got := len(srv.Sessions()); got != 0 {
		t.Errorf("Sessions() has %d sessions, want 0", got)
	}

	startDebuggee(t, srv.Port(), "xdebug.idekey=agent")
	if sess := waitSession(t, srv); sess.Init().IDEKey != "agent" {
		t.Errorf("idekey = %q", sess.Init().IDEKey)
	}
}

func TestEngineInspection(t *testing.T) {
	_, sess := startEngineSession(t, Config{Features: map[string]string{"max_children": "10", "max_data": "16"}})
	if _, err := sess.SetBreakpoint(debuggeePath(t), lineAddBody); err != nil {
		t.Fatal(err)
	}
	mustContinue(t, sess, ContinueRun)
	main := PropertyOptions{Depth: 1}

	if d, err := sess.StackDepth(); err != nil || d != 2 {
		t.Errorf("StackDepth = %d, %v; want 2", d, err)
	}
	if f, err := sess.GetStackFrame(1); err != nil || f.Where != "{main}" || f.Lineno != lineCallAdd {
		t.Errorf("GetStackFrame(1) = %+v, %v", f, err)
	}
	names, err := sess.ContextNames(0)
	if err != nil || len(names) < 2 || names[0].Name != "Locals" || names[1].Name != "Superglobals" {
		t.Errorf("ContextNames = %+v, %v", names, err)
	}
	if types, err := sess.TypeMap(); err != nil || len(types) == 0 {
		t.Errorf("TypeMap = %+v, %v", types, err)
	}

	// Paging through an array of 40 with max_children 10.
	var all []string
	for page := 0; ; page++ {
		opts := main
		opts.Page = page
		p, err := sess.GetProperty("$numbers", opts)
		if err != nil {
			t.Fatalf("GetProperty($numbers, page %d): %v", page, err)
		}
		for _, c := range p.ChildProperties {
			v, _ := c.DecodedValue()
			all = append(all, v)
		}
		if page+1 >= p.Pages() {
			break
		}
	}
	if len(all) != 40 || all[0] != "1" || all[39] != "40" {
		t.Errorf("paged $numbers = %v", all)
	}

	// Names that need quoting.
	p, err := sess.GetProperty(`$user["name"]`, main)
	if v, _ := p.DecodedValue(); err != nil || v != "Alice" {
		t.Errorf(`$user["name"] = %+v, %v`, p, err)
	}

	// Long values: truncated to max_data by property_get, whole by property_value.
	p, err = sess.GetProperty("$long", main)
	if err != nil || !p.Truncated() || p.Size != 200 {
		t.Errorf("$long = %+v, %v; want truncated", p, err)
	}
	if v, err := sess.GetPropertyValue("$long", main); err != nil || len(v) != 200 {
		t.Errorf("GetPropertyValue($long) = %d bytes, %v; want 200", len(v), err)
	}

	// property_set evaluates its value as PHP.
	if err := sess.SetProperty("$b", "7 * 2", PropertyOptions{}); err != nil {
		t.Fatal(err)
	}
	if v, err := sess.Eval("$b"); err != nil || v != "14" {
		t.Errorf("$b after SetProperty = %q, %v; want 14", v, err)
	}

	if p, err := sess.EvalProperty("['k' => $a]", 0); err != nil || len(p.ChildProperties) != 1 || p.ChildProperties[0].Name != "k" {
		t.Errorf("EvalProperty = %+v, %v", p, err)
	}

	var engineErr *EngineError
	if _, err := sess.GetProperty("$nope", PropertyOptions{}); !errors.As(err, &engineErr) || engineErr.Code != 300 {
		t.Errorf("GetProperty($nope) err = %v, want engine error 300", err)
	}
}

// The engine sees the debuggee's real directory; the test works only with
// an imaginary local checkout, as when debugging a container or a server.
func TestEnginePathMapping(t *testing.T) {
	requireDebugEngine(t)
	remoteDir := filepath.Dir(debuggeePath(t))
	localDir := filepath.FromSlash("/home/agent/my project")
	srv := newTestServer(t, Config{PathMap: []PathMapping{{Local: localDir, Remote: remoteDir}}})
	script := filepath.Join(localDir, "basic.php")

	if _, err := srv.AddBreakpoint(Breakpoint{File: script, Line: lineAddBody}); err != nil {
		t.Fatal(err)
	}
	startDebuggee(t, srv.Port())
	sess := waitSession(t, srv)
	if sess.Script() != script {
		t.Errorf("Script() = %q, want %q", sess.Script(), script)
	}

	st := mustContinue(t, sess, ContinueRun)
	if st.File != script || st.Line != lineAddBody {
		t.Fatalf("stopped at %s:%d, want %s:%d", st.File, st.Line, script, lineAddBody)
	}
	if stack, err := sess.GetStack(); err != nil || stack[0].Filename != script || stack[1].Filename != script {
		t.Errorf("GetStack = %+v, %v", stack, err)
	}
	if src, err := sess.GetSource(script, lineAddBody, lineAddBody); err != nil || !strings.Contains(src, "$sum = $a + $b;") {
		t.Errorf("GetSource = %q, %v", src, err)
	}

	if st := mustContinue(t, sess, ContinueRun); st.Status != StatusStopping {
		t.Fatalf("at end: %+v", st)
	}
	notes, _ := sess.Notifications(0)
	for _, n := range notes {
		if n.Name == "error" && n.Message.Filename != script {
			t.Errorf("warning reported in %q, want %q", n.Message.Filename, script)
		}
	}
}

// The engine maps paths itself, from map files on the server; the client
// detects it and passes local paths through.
func TestEngineOwnPathMapping(t *testing.T) {
	requireDebugEngine(t)
	src, err := os.ReadFile(debuggeePath(t))
	if err != nil {
		t.Fatal(err)
	}
	// The map's remote prefix must be the path PHP sees, without symlinks.
	remoteDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	remoteScript := filepath.Join(remoteDir, "basic.php")
	localDir := "/home/agent/project"
	script := localDir + "/basic.php"
	files := map[string]string{
		remoteScript: string(src),
		filepath.Join(remoteDir, ".xdebug", "agent.map"): "remote_prefix: " + filepath.ToSlash(remoteDir) +
			"\nlocal_prefix: " + localDir + "\n/basic.php = /basic.php\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	srv := newTestServer(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{File: script, Line: lineAddBody}); err != nil {
		t.Fatal(err)
	}
	startDebuggeeScript(t, srv.Port(), remoteScript, "xdebug.path_mapping=1")
	sess := waitSession(t, srv)

	if m := sess.EngineMapping(); !m.Detected || !m.Enabled || m.RemoteScript != remoteScript {
		t.Errorf("EngineMapping() = %+v, want enabled with remote script %s", m, remoteScript)
	}
	if sess.Script() != script {
		t.Errorf("Script() = %q, want %q", sess.Script(), script)
	}
	if w := sess.SetupWarnings(); len(w) != 0 {
		t.Errorf("SetupWarnings() = %q", w)
	}
	if st := mustContinue(t, sess, ContinueRun); st.File != script || st.Line != lineAddBody {
		t.Fatalf("stopped at %s:%d, want %s:%d", st.File, st.Line, script, lineAddBody)
	}
	stack, err := sess.GetStack()
	if err != nil || stack[0].Filename != script || stack[0].Facet != "mapped" {
		t.Errorf("GetStack = %+v, %v; want mapped frames in %s", stack, err, script)
	}
}

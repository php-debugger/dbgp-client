package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	dbgp "github.com/php-debugger/dbgp-client"
)

// newTestREPL returns a REPL on a free port and its output.
func newTestREPL(t *testing.T) (*repl, *dbgp.Server, func() string) {
	t.Helper()
	srv, err := dbgp.Listen(dbgp.Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	var out bytes.Buffer
	r := newREPL(srv, &out)
	output := func() string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return out.String()
	}
	return r, srv, output
}

func TestParsePathMap(t *testing.T) {
	abs, _ := filepath.Abs("rel")
	got, err := parsePathMap([]string{"/home/me/app=/var/www", "/a=C:/b", "rel=/srv"})
	want := []dbgp.PathMapping{{Local: "/home/me/app", Remote: "/var/www"}, {Local: "/a", Remote: "C:/b"}, {Local: abs, Remote: "/srv"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("parsePathMap = %+v, %v", got, err)
	}
	for _, bad := range []string{"/a", "=/b", "/a="} {
		if _, err := parsePathMap([]string{bad}); err == nil {
			t.Errorf("parsePathMap(%q) succeeded", bad)
		}
	}
}

func TestParseBreakpoint(t *testing.T) {
	r, _, _ := newTestREPL(t)
	abs, _ := filepath.Abs("a.php")
	tests := []struct {
		args string
		want dbgp.Breakpoint
	}{
		{"/app/a.php:12", dbgp.Breakpoint{Type: "line", File: "/app/a.php", Line: 12}},
		{"a.php:3", dbgp.Breakpoint{Type: "line", File: abs, Line: 3}},
		{"/app/a.php:5 if $i > 2", dbgp.Breakpoint{Type: "conditional", File: "/app/a.php", Line: 5, Condition: "$i > 2"}},
		{"call App\\run", dbgp.Breakpoint{Type: "call", Function: "App\\run"}},
		{"return add", dbgp.Breakpoint{Type: "return", Function: "add"}},
		{"exception RuntimeException", dbgp.Breakpoint{Type: "exception", Exception: "RuntimeException"}},
	}
	for _, tt := range tests {
		got, err := r.parseBreakpoint(tt.args)
		if err != nil || got != tt.want {
			t.Errorf("parseBreakpoint(%q) = %+v, %v; want %+v", tt.args, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "call", "exception", "a.php", "a.php:x", "a.php:3,7", ":3", "a.php:0",
		"/a.php:3 when $x", "/a.php:3 if ", "24"} {
		if got, err := r.parseBreakpoint(bad); err == nil {
			t.Errorf("parseBreakpoint(%q) = %+v, want error", bad, got)
		}
	}
}

func TestParseLocation(t *testing.T) {
	if file, line, err := parseLocation("12"); err != nil || file != "" || line != 12 {
		t.Errorf(`parseLocation("12") = %q, %d, %v`, file, line, err)
	}
	if file, line, err := parseLocation("file:///app/a.php:4"); err != nil || file != "file:///app/a.php" || line != 4 {
		t.Errorf("parseLocation(uri) = %q, %d, %v", file, line, err)
	}
}

func TestParsePropertyArgs(t *testing.T) {
	tests := []struct {
		args    string
		allowed []string
		opts    dbgp.PropertyOptions
		name    string
		ok      bool
	}{
		{"$a", []string{"-p", "-d"}, dbgp.PropertyOptions{}, "$a", true},
		{"-p 2 -d 1 $user[\"first name\"]", []string{"-p", "-d"}, dbgp.PropertyOptions{Page: 2, Depth: 1}, `$user["first name"]`, true},
		{"-d 1 $a", []string{"-d"}, dbgp.PropertyOptions{Depth: 1}, "$a", true},
		{"-p 1 $a", []string{"-d"}, dbgp.PropertyOptions{}, "-p 1 $a", true},
		{"-p x $a", []string{"-p"}, dbgp.PropertyOptions{}, "", false},
		{"-d -1 $a", []string{"-d"}, dbgp.PropertyOptions{}, "", false},
		{"-d 1", []string{"-d"}, dbgp.PropertyOptions{Depth: 1}, "", false},
	}
	for _, tt := range tests {
		opts, name, ok := parsePropertyArgs(tt.args, tt.allowed...)
		if ok != tt.ok || (ok && (opts != tt.opts || name != tt.name)) {
			t.Errorf("parsePropertyArgs(%q) = %+v, %q, %v", tt.args, opts, name, ok)
		}
	}
}

func TestCommandsWithoutSession(t *testing.T) {
	r, _, output := newTestREPL(t)
	// Output and notifications belong to a session, but do not wait for one.
	for _, line := range []string{"output", "notes"} {
		r.execLine(line)
	}
	if got := strings.Count(output(), "Error: no session yet"); got != 2 {
		t.Errorf("got %d no-session errors, want 2:\n%s", got, output())
	}
	// Bad arguments are reported at once, without waiting for a session.
	for _, line := range []string{"vars x", "vars 1 2 3", "print -p x $a", "print", "value", "eval", "set nope", "source x"} {
		done := make(chan struct{})
		go func() { r.execLine(line); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%q waited for a session instead of reporting its usage", line)
		}
	}
	if got := strings.Count(output(), "Error: usage: "); got != 8 {
		t.Errorf("got %d usage errors, want 8:\n%s", got, output())
	}
	// Usage spread over several help lines is reported on one line.
	r.execLine("break")
	if !strings.Contains(output(), "Error: usage: break [FILE:]LINE [if COND] | break call|return FUNC | break exception CLASS\n") {
		t.Errorf("break usage error not on one line:\n%s", output())
	}
	r.execLine("frobnicate")
	r.execLine("sessions")
	r.execLine("breaks")
	if out := output(); !strings.Contains(out, `Unknown command "frobnicate"`) ||
		!strings.Contains(out, "No sessions.") || !strings.Contains(out, "No breakpoints.") {
		t.Errorf("output:\n%s", out)
	}
	for _, quit := range []string{"quit", "q", "exit"} {
		if !r.execLine(quit) {
			t.Errorf("%s did not end the loop", quit)
		}
	}
	if r.execLine("") {
		t.Error("an empty line ended the loop")
	}
}

func TestHelpListsEveryCommandOnce(t *testing.T) {
	r, _, output := newTestREPL(t)
	r.execLine("help")
	out := output()
	for name := range commands {
		if _, alias := aliases[name]; alias {
			continue
		}
		for _, form := range strings.Split(commands[name].usage, "\n") {
			if !strings.Contains(out, "\n  "+form) {
				t.Errorf("help does not list %q", form)
			}
		}
	}
	if strings.Count(out, "break exception CLASS") != 1 {
		t.Error("help lists break more than once")
	}
	for _, line := range []string{
		"  break [FILE:]LINE [if COND]          stop at a line",
		"  break call|return FUNC               stop when the function or method FUNC is called, or when it returns",
		"  break exception CLASS                stop when an exception of CLASS, or a subclass, is thrown",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("help lacks %q", line)
		}
	}
	// Commands are in alphabetical order, quit included.
	var listed []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(line) > 2 && line[:2] == "  " && len(f) > 0 {
			listed = append(listed, f[0])
		}
	}
	listed = slices.Compact(listed) // break's extra forms
	if !sort.StringsAreSorted(listed) || listed[len(listed)-1] != "vars" || !slices.Contains(listed, "quit") {
		t.Errorf("help order = %q", listed)
	}
	if !strings.Contains(out, "Aliases: b break, bt stack, exit quit, l source, n next, o out, p print, q quit, r run, s step.") {
		t.Errorf("aliases line missing or wrong:\n%s", out)
	}
}

// Commands that need a session wait for PHP to connect, until Ctrl-C.
func TestCommandsWaitForSessionUntilInterrupted(t *testing.T) {
	for _, line := range []string{"run", "step", "stack", "vars", "print $a", "eval 1", "set $a = 1", "source", "stop", "detach"} {
		r, _, output := newTestREPL(t)
		interrupts := make(chan os.Signal, 1)
		go r.cancelOnInterrupt(interrupts)
		go func() {
			time.Sleep(100 * time.Millisecond)
			interrupts <- os.Interrupt
		}()
		start := time.Now()
		r.execLine(line)
		out := output()
		if time.Since(start) < 100*time.Millisecond || time.Since(start) > 5*time.Second ||
			!strings.Contains(out, "Waiting for PHP to connect (Ctrl-C to cancel)...") || !strings.Contains(out, "Interrupted.") {
			t.Errorf("%s: after %v:\n%s", line, time.Since(start), out)
		}
	}
}

// requireEngine skips unless php has PHP Debugger or Xdebug, like the
// library's integration tests.
func requireEngine(t *testing.T) {
	t.Helper()
	skip := t.Skip
	if os.Getenv("DBGP_REQUIRE_ENGINE") != "" {
		skip = t.Fatal
	}
	if testing.Short() {
		skip("skipping debug engine test in -short mode")
	}
	loaded := `exit(extension_loaded("php_debugger") || extension_loaded("xdebug") ? 0 : 1);`
	// Debugging off: with it on, php would try to connect to whatever
	// listens on the default port and wait for it.
	if err := exec.Command("php", "-dxdebug.mode=off", "-r", loaded).Run(); err != nil {
		skip("php has neither PHP Debugger nor Xdebug loaded")
	}
}

// A scripted debugging session against a real engine.
func TestREPLSession(t *testing.T) {
	requireEngine(t)
	script, err := filepath.Abs("../../testdata/php/basic.php")
	if err != nil {
		t.Fatal(err)
	}
	r, srv, output := newTestREPL(t)
	go r.announceSessions()

	php := exec.Command("php", "-dxdebug.mode=debug", "-dxdebug.start_with_request=yes",
		fmt.Sprintf("-dxdebug.client_port=%d", srv.Port()), "-ddisplay_errors=0", script)
	if err := php.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = php.Process.Kill(); _ = php.Wait() })

	commands := []string{
		"break " + script + ":24",
		"run",
		"stack",
		"vars",
		"eval $a + $b",
		"set $b = 7 * 2",
		"next",
		"print -d 1 $obj",
		"print -d 1 $numbers",
		"value -d 1 $long",
		"print $nope",
		"print -p 5 -d 1 $numbers",
		"vars 1",
		"contexts",
		"source 30",
		"l " + script + ":10",
		"breaks",
		"delete 1",
		"breaks",
		"use 1",
		"step",
		"bt",
		"run",
		"output",
		"output",
		"notes",
		"stop",
		"sessions",
	}
	r.run(newPlainReader(r, strings.NewReader(strings.Join(commands, "\n"))))

	out := output()
	for _, want := range []string{
		"[session 1 connected: " + script + " (selected)]",
		"Breakpoint 1: " + script + ":24",
		"Stopped at " + script + ":24",
		"=>   24      $sum = $a + $b;",
		"#0 add() at " + script + ":24",
		"$a                             int      = 2",
		"$sum                           unset\n",
		"5 (int)",
		"$b = 14",
		"Stopped at " + script + ":25",
		"  $obj->name                   string   = \"svc\"",
		"(page 1 of 2; print -p 1 -d 1 $numbers for the next)",
		strings.Repeat("abcdefghij", 20),
		"Error: error 300: can not get property",
		"Error: $numbers has 2 pages",
		"$long                          string   = \"abcdefghij",
		"$numbers                       array    = array[40]",
		"$obj                           object   = Service",
		"(print NAME shows what an array or object contains)",
		"0  Locals\n1  Superglobals",
		"     30  $long = str_repeat(",
		"=>   25      return $sum;",
		"     10  class Service",
		"1  " + script + ":24",
		"Deleted breakpoint 1.",
		"No breakpoints.",
		"Stopped at " + script + ":37",
		"#0 {main}() at " + script + ":37",
		"(no new output)",
		"Script finished",
		"PHP Warning: Undefined variable $undefinedVariable at " + script + ":42",
		"x=16\ndone\n",
		"Stopped session 1.",
		"* 1  ended",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out, "uninitialized") {
		t.Error("output shows the DBGp type uninitialized instead of unset")
	}
	// vars lists variables only, not what arrays and objects contain; the
	// one array element in the output comes from print.
	if n := strings.Count(out, "$numbers[0]"); n != 1 {
		t.Errorf("$numbers[0] appears %d times, want once (from print only)", n)
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		p        dbgp.Property
		want     string
		compound bool
	}{
		{dbgp.Property{Type: "array", NumChildren: 40, ChildProperties: make([]dbgp.Property, 32)}, "array[40]", true},
		{dbgp.Property{Type: "array"}, "array[0]", true},
		{dbgp.Property{Type: "object", ClassName: "App\\Service", NumChildren: 3}, "App\\Service", true},
		{dbgp.Property{Type: "object"}, "object", true},
		{dbgp.Property{Type: "int", Value: "42"}, "42", false},
		{dbgp.Property{Type: "string", Encoding: "base64", Value: "c3Zj"}, `"svc"`, false},
		{dbgp.Property{Type: "null"}, "null", false},
	}
	for _, tt := range tests {
		if got, compound := summarize(tt.p); got != tt.want || compound != tt.compound {
			t.Errorf("summarize(%+v) = %q, %v; want %q, %v", tt.p, got, compound, tt.want, tt.compound)
		}
	}
}

func TestREPLBreakpointKindsAndDetach(t *testing.T) {
	requireEngine(t)
	script, err := filepath.Abs("../../testdata/php/basic.php")
	if err != nil {
		t.Fatal(err)
	}
	r, srv, output := newTestREPL(t)
	go r.announceSessions()
	php := exec.Command("php", "-dxdebug.mode=debug", "-dxdebug.start_with_request=yes",
		fmt.Sprintf("-dxdebug.client_port=%d", srv.Port()), "-ddisplay_errors=0", script)
	if err := php.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = php.Process.Kill(); _ = php.Wait() })

	r.run(newPlainReader(r, strings.NewReader(strings.Join([]string{
		"break call add",
		"break exception RuntimeException",
		"run",
		"out",
		"break 39 if $i == 3",
		"run",
		"eval $i",
		"run",
		"detach",
		"sessions",
	}, "\n"))))

	out := output()
	for _, want := range []string{
		"Breakpoint 1: call of add",
		"Breakpoint 2: exception RuntimeException",
		"Stopped at " + script + ":24",
		"Stopped at " + script + ":37",
		"Breakpoint 3: " + script + ":39 if $i == 3",
		"Stopped at " + script + ":39",
		"3 (int)",
		"Exception RuntimeException: boom",
		"Stopped at " + script + ":44",
		"Detached session 1; the script runs on.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

// The built command: flags, startup and quitting.
func TestBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "dbgp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, _ := exec.Command(bin, "-h").CombinedOutput()
	if !strings.Contains(string(out), "Usage: dbgp [options]") || !strings.Contains(string(out), "-map LOCAL=REMOTE") {
		t.Errorf("-h output:\n%s", out)
	}

	cmd := exec.Command(bin, "-map", "nonsense")
	if out, err := cmd.CombinedOutput(); cmd.ProcessState.ExitCode() != 2 || !strings.Contains(string(out), "LOCAL=REMOTE") {
		t.Errorf("-map nonsense: %v\n%s", err, out)
	}

	srv, err := dbgp.Listen(dbgp.Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	busy := srv.Addr()
	cmd = exec.Command(bin, "-addr", busy)
	if out, err := cmd.CombinedOutput(); cmd.ProcessState.ExitCode() != 1 {
		t.Errorf("busy address: %v\n%s", err, out)
	}
	_ = srv.Close()

	cmd = exec.Command(bin, "-addr", "127.0.0.1:0", "-break", "/app/a.php:3", "-break", "/app/b.php:5")
	cmd.Stdin = strings.NewReader("breaks\nquit\n")
	out, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Listening on 127.0.0.1:") ||
		!strings.Contains(string(out), "1  /app/a.php:3\n") || !strings.Contains(string(out), "2  /app/b.php:5\n") {
		t.Errorf("session: %v\n%s", err, out)
	}
}

// The engine reports stopped just before it closes the connection.
func TestEnded(t *testing.T) {
	for st, want := range map[dbgp.State]bool{
		{Status: dbgp.StatusBreak}:                 false,
		{Status: dbgp.StatusStopping}:              false,
		{Status: dbgp.StatusStopped}:               true,
		{Status: dbgp.StatusStopped, Closed: true}: true,
		{Status: dbgp.StatusBreak, Closed: true}:   true,
	} {
		if got := ended(st); got != want {
			t.Errorf("ended(%+v) = %v, want %v", st, got, want)
		}
	}
}

func TestFormatVariableUnset(t *testing.T) {
	if got := formatVariable(dbgp.Variable{Name: "$sum", Type: "uninitialized", Value: "null"}); got != "$sum"+strings.Repeat(" ", 27)+"unset" {
		t.Errorf("unset variable = %q", got)
	}
	if got := formatVariable(dbgp.Variable{Name: "$x", Type: "uninitialized", Level: 1}); got != "  $x"+strings.Repeat(" ", 27)+"unset" {
		t.Errorf("nested unset variable = %q", got)
	}
	if got, want := formatVariable(dbgp.Variable{Name: "$n", Type: "null", Value: "null"}), dbgp.FormatVariable(dbgp.Variable{Name: "$n", Type: "null", Value: "null"}); got != want {
		t.Errorf("null variable = %q, want %q", got, want)
	}
}

func TestListenAndUnlisten(t *testing.T) {
	r, srv, output := newTestREPL(t)
	addr := srv.Addr()
	refused := func() bool {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	}

	for _, line := range []string{"unlisten", "unlisten", "sessions"} {
		r.execLine(line)
	}
	if srv.Listening() || !refused() {
		t.Fatal("still accepting connections after unlisten")
	}
	// Commands that need a session fail at once rather than wait.
	done := make(chan struct{})
	go func() { r.execLine("stack"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stack waited for a session while not listening")
	}

	r.execLine("listen")
	r.execLine("listen")
	r.execLine("sessions")
	if !srv.Listening() || refused() || srv.Addr() != addr {
		t.Errorf("after listen: listening %v, refused %v, addr %s (was %s)", srv.Listening(), refused(), srv.Addr(), addr)
	}

	out := output()
	for _, want := range []string{
		"Stopped listening: new PHP connections are refused. Connected sessions carry on.\nNot listening.\n",
		"Not listening: PHP connections are refused (use listen).\nNo sessions.\n",
		"Error: no session, and not listening for PHP connections (use listen)\n",
		"Listening on " + addr + ".\nAlready listening on " + addr + ".\nListening on " + addr + ".\nNo sessions.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

func TestMapAndMaps(t *testing.T) {
	r, srv, output := newTestREPL(t)
	r.execLine("maps")
	r.execLine("map /home/me/app=/var/www/html")
	r.execLine("map testdata/php = /srv/php")
	r.execLine("map /home/me/app=/app")
	r.execLine("map nonsense")
	r.execLine("maps")

	abs, _ := filepath.Abs("testdata/php")
	want := []dbgp.PathMapping{{Local: abs, Remote: "/srv/php"}, {Local: "/home/me/app", Remote: "/app"}}
	if got := srv.PathMap(); !reflect.DeepEqual(got, want) {
		t.Errorf("PathMap() = %+v, want %+v", got, want)
	}
	out := output()
	for _, line := range []string{
		"No path mappings.\n",
		"Mapped /home/me/app => /var/www/html\n",
		"Mapped " + abs + " => /srv/php\n",
		"Error: usage: map LOCAL=REMOTE\n",
		abs + " => /srv/php\n/home/me/app => /app\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q", line)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

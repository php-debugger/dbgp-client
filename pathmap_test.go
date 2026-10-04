package dbgp

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustPathMap(t *testing.T, mappings ...PathMapping) PathMap {
	t.Helper()
	m, err := newPathMap(mappings)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPathMapTranslation(t *testing.T) {
	m := mustPathMap(t,
		PathMapping{Local: "/home/me/project", Remote: "/var/www/html"},
		PathMapping{Local: "/home/me/vendor-src/", Remote: "/var/www/html/vendor/"},
		PathMapping{Local: "/home/me/win", Remote: `C:\inetpub\wwwroot`},
		PathMapping{Local: "/home/me/share", Remote: "//fileserver/php"},
	)
	tests := []struct{ local, remote string }{
		{"/home/me/project/index.php", "/var/www/html/index.php"},
		{"/home/me/project", "/var/www/html"},
		{"/home/me/project/src/a b.php", "/var/www/html/src/a b.php"},
		// The longest matching prefix wins, in both directions.
		{"/home/me/vendor-src/lib/x.php", "/var/www/html/vendor/lib/x.php"},
		// Windows server: backslashes and drive letters.
		{"/home/me/win/default.php", "C:/inetpub/wwwroot/default.php"},
		// UNC share.
		{"/home/me/share/app.php", "//fileserver/php/app.php"},
		// Unmapped paths pass through.
		{"/opt/other/file.php", "/opt/other/file.php"},
	}
	for _, tt := range tests {
		local := filepath.FromSlash(tt.local)
		if got := m.ToRemote(local); got != tt.remote {
			t.Errorf("ToRemote(%q) = %q, want %q", local, got, tt.remote)
		}
		if got := m.ToLocal(tt.remote); got != local {
			t.Errorf("ToLocal(%q) = %q, want %q", tt.remote, got, local)
		}
	}

	// Mappings match whole path components only.
	if got := m.ToRemote(filepath.FromSlash("/home/me/project2/a.php")); got != filepath.FromSlash("/home/me/project2/a.php") {
		t.Errorf("ToRemote matched a partial component: %q", got)
	}
	if got := m.ToLocal("/var/www/html2/a.php"); got != filepath.FromSlash("/var/www/html2/a.php") {
		t.Errorf("ToLocal matched a partial component: %q", got)
	}
	// Remote paths from a Windows server may use backslashes.
	if got := m.ToLocal(`C:\inetpub\wwwroot\default.php`); got != filepath.FromSlash("/home/me/win/default.php") {
		t.Errorf("ToLocal(backslashes) = %q", got)
	}
}

func TestPathMapRoots(t *testing.T) {
	m := mustPathMap(t, PathMapping{Local: "/srv/mirror", Remote: "/"})
	if got := m.ToLocal("/etc/app.php"); got != filepath.FromSlash("/srv/mirror/etc/app.php") {
		t.Errorf("ToLocal = %q", got)
	}
	if got := m.ToRemote(filepath.FromSlash("/srv/mirror/etc/app.php")); got != "/etc/app.php" {
		t.Errorf("ToRemote = %q", got)
	}
	if got := m.ToRemote(filepath.FromSlash("/srv/mirror")); got != "/" {
		t.Errorf("ToRemote(root) = %q", got)
	}

	d := mustPathMap(t, PathMapping{Local: "/srv/d", Remote: `D:\`})
	if got := d.ToRemote(filepath.FromSlash("/srv/d/x.php")); got != "D:/x.php" {
		t.Errorf("ToRemote(drive root) = %q", got)
	}
	if got := d.ToLocal("D:/x.php"); got != filepath.FromSlash("/srv/d/x.php") {
		t.Errorf("ToLocal(drive root) = %q", got)
	}
}

func TestPathMapURIs(t *testing.T) {
	m := mustPathMap(t,
		PathMapping{Local: "/home/me/my project", Remote: "/var/www/html"},
		PathMapping{Local: "/home/me/win", Remote: "C:/inetpub"},
	)
	tests := []struct{ local, uri string }{
		{"/home/me/my project/a b.php", "file:///var/www/html/a%20b.php"},
		{"/home/me/win/x.php", "file:///C:/inetpub/x.php"},
		{"/unmapped/dir/x.php", "file:///unmapped/dir/x.php"},
	}
	for _, tt := range tests {
		local := filepath.FromSlash(tt.local)
		if got := m.engineURI(local); got != tt.uri {
			t.Errorf("engineURI(%q) = %q, want %q", local, got, tt.uri)
		}
		if got := m.localPath(tt.uri); got != local {
			t.Errorf("localPath(%q) = %q, want %q", tt.uri, got, local)
		}
	}
	if got := m.localPath("dbgp://1"); got != "dbgp://1" {
		t.Errorf("localPath(dbgp://1) = %q, want it unchanged", got)
	}
	if got := m.engineURI("file:///already/remote.php"); got != "file:///already/remote.php" {
		t.Errorf("engineURI(uri) = %q, want it unchanged", got)
	}
	var none PathMap
	if got := none.localPath("file:///app/a.php"); got != filepath.FromSlash("/app/a.php") {
		t.Errorf("empty map localPath = %q", got)
	}
}

func TestPathMapRejectsIncompleteMappings(t *testing.T) {
	for _, pm := range []PathMapping{{Local: "/a"}, {Remote: "/b"}} {
		if _, err := Listen(Config{Addr: "127.0.0.1:0", PathMap: []PathMapping{pm}}); err == nil {
			t.Errorf("Listen accepted mapping %+v", pm)
		}
	}
}

// Every path a session sends or returns goes through the map. The recorded
// fixtures use /app/basic.php, the engine-side path here.
func TestSessionUsesPathMap(t *testing.T) {
	const local = "/home/me/my project"
	srv := newTestServer(t, Config{PathMap: []PathMapping{{Local: local, Remote: "/app"}}})
	script := filepath.FromSlash(local + "/basic.php")
	if _, err := srv.AddBreakpoint(Breakpoint{File: script, Line: lineAddBody}); err != nil {
		t.Fatal(err)
	}
	e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	sess := waitSession(t, srv)

	if got := e.LastCommand("breakpoint_set").Args["f"]; got != fixtureScriptURI {
		t.Errorf("breakpoint sent for %q, want %q", got, fixtureScriptURI)
	}
	if got := sess.Script(); got != script {
		t.Errorf("Script() = %q, want %q", got, script)
	}
	st, err := sess.Continue(context.Background(), ContinueRun, time.Second)
	if err != nil || st.File != script {
		t.Errorf("stopped at %q, %v; want %q", st.File, err, script)
	}
	stack, err := sess.GetStack()
	if err != nil || stack[0].Filename != script || stack[1].Filename != script {
		t.Errorf("GetStack = %+v, %v", stack, err)
	}
	e.Handle("stack_get", reply("stack_get_depth"))
	if f, err := sess.GetStackFrame(1); err != nil || f.Filename != script {
		t.Errorf("GetStackFrame = %+v, %v", f, err)
	}
	bps, err := sess.ListBreakpoints()
	if err != nil || bps[0].Filename != script || bps[2].Filename != "" {
		t.Errorf("ListBreakpoints = %+v, %v", bps, err)
	}
	if _, err := sess.GetSource(script, 1, 2); err != nil {
		t.Fatal(err)
	}
	if got := e.LastCommand("source").Args["f"]; got != fixtureScriptURI {
		t.Errorf("source requested for %q, want %q", got, fixtureScriptURI)
	}

	e.Send(e.fixture("notify_error"))
	e.Send(e.fixture("notify_breakpoint_resolved"))
	waitFor(t, func() bool { _, next := sess.Notifications(0); return next == 2 })
	notes, _ := sess.Notifications(0)
	if notes[0].Message.Filename != script || notes[1].Breakpoint.Filename != script {
		t.Errorf("notification paths = %q, %q", notes[0].Message.Filename, notes[1].Breakpoint.Filename)
	}

	// Paths outside every mapping pass through unchanged.
	if _, err := sess.SetBreakpoint("/elsewhere/x.php", 3); err != nil {
		t.Fatal(err)
	}
	if got := e.LastCommand("breakpoint_set").Args["f"]; !strings.HasSuffix(got, "/elsewhere/x.php") {
		t.Errorf("unmapped breakpoint sent for %q", got)
	}
}

// evalString is an eval response with a string result, shaped like the
// recorded ones. size is the full length the engine reports.
func evalString(cmd fakeCommand, value string, size int) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="iso-8859-1"?>`+"\n"+
		`<response xmlns="urn:debugger_protocol_v1" xmlns:xdebug="https://xdebug.org/dbgp/xdebug" command="eval" transaction_id="%s">`+
		`<property type="string" size="%d" encoding="base64"><![CDATA[%s]]></property></response>`,
		cmd.Args["i"], size, base64.StdEncoding.EncodeToString([]byte(value)))
}

// probeReply answers the engine-mapping probe with reply, and other evals
// with the recorded eval response.
func probeReply(reply func(e *fakeEngine, cmd fakeCommand) []string) map[string]fakeHandler {
	h := standardHandlers()
	h["eval"] = func(e *fakeEngine, cmd fakeCommand) []string {
		if cmd.Data == engineMappingProbe {
			return reply(e, cmd)
		}
		return []string{e.withTransaction(e.fixture("eval"), cmd)}
	}
	return h
}

func TestEngineMappingDetection(t *testing.T) {
	answer := func(json string) func(e *fakeEngine, cmd fakeCommand) []string {
		return func(e *fakeEngine, cmd fakeCommand) []string { return []string{evalString(cmd, json, len(json))} }
	}
	tests := []struct {
		name  string
		reply func(e *fakeEngine, cmd fakeCommand) []string
		want  EngineMapping
	}{
		{"xdebug setting on", answer(`["1","0","/srv/app/basic.php"]`),
			EngineMapping{Detected: true, Enabled: true, RemoteScript: "/srv/app/basic.php"}},
		{"php_debugger setting on", answer(`["0","On","/srv/app/basic.php"]`),
			EngineMapping{Detected: true, Enabled: true, RemoteScript: "/srv/app/basic.php"}},
		{"off", answer(`["0","0","/srv/app/basic.php"]`),
			EngineMapping{Detected: true, RemoteScript: "/srv/app/basic.php"}},
		{"engine without the setting", answer(`[false,false,"/srv/app/basic.php"]`),
			EngineMapping{Detected: true, RemoteScript: "/srv/app/basic.php"}},
		{"not json", answer(`oops`), EngineMapping{}},
		{"truncated", func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{evalString(cmd, `["1","0","/sr`, 40)}
		}, EngineMapping{}},
		{"eval error", func(e *fakeEngine, cmd fakeCommand) []string {
			return []string{e.withTransaction(e.fixture("eval_error"), cmd)}
		}, EngineMapping{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, Config{})
			dialFakeEngine(t, srv, fixture(t, "init"), probeReply(tt.reply))
			sess := waitSession(t, srv)
			if got := sess.EngineMapping(); got != tt.want {
				t.Errorf("EngineMapping() = %+v, want %+v", got, tt.want)
			}
			if w := sess.SetupWarnings(); len(w) != 0 {
				t.Errorf("SetupWarnings() = %q, want none", w)
			}
		})
	}
}

func TestEngineMappingWithPathMapWarns(t *testing.T) {
	on := func(e *fakeEngine, cmd fakeCommand) []string {
		v := `["1","1","/srv/app/basic.php"]`
		return []string{evalString(cmd, v, len(v))}
	}
	srv := newTestServer(t, Config{PathMap: []PathMapping{{Local: "/home/me/app", Remote: "/srv/app"}}})
	dialFakeEngine(t, srv, fixture(t, "init"), probeReply(on))
	sess := waitSession(t, srv)
	if w := sess.SetupWarnings(); len(w) != 1 || !strings.Contains(w[0], "Config.PathMap") {
		t.Errorf("SetupWarnings() = %q, want a double-mapping warning", w)
	}
}

func TestStackFrameFacet(t *testing.T) {
	s, _ := startFakeSession(t, map[string]fakeHandler{
		"stack_get": func(e *fakeEngine, cmd fakeCommand) []string {
			packet := e.withTransaction(e.fixture("stack_get"), cmd)
			packet = strings.Replace(packet, `level="0"`, `level="0" xdebug:facet="mapped"`, 1)
			packet = strings.Replace(packet, `level="1"`, `level="1" xdebug:facet="skipped"`, 1)
			return []string{packet}
		},
	})
	stack, err := s.GetStack()
	if err != nil || stack[0].Facet != "mapped" || stack[1].Facet != "skipped" {
		t.Errorf("GetStack = %+v, %v; want facets mapped, skipped", stack, err)
	}
}

// Adding a mapping applies to a connected session at once: paths from the
// engine are translated, and breakpoints are set again with their new path.
func TestAddPathMapping(t *testing.T) {
	srv := newTestServer(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{File: "/home/me/app/basic.php", Line: lineAddBody}); err != nil {
		t.Fatal(err)
	}
	e := dialFakeEngine(t, srv, fixture(t, "init"), standardHandlers())
	sess := waitSession(t, srv)
	if got := e.LastCommand("breakpoint_set").Args["f"]; got != "file:///home/me/app/basic.php" {
		t.Fatalf("breakpoint before mapping sent for %q", got)
	}
	if sess.Script() != "/app/basic.php" {
		t.Fatalf("Script() before mapping = %q", sess.Script())
	}
	e.ClearReceived()

	if err := srv.AddPathMapping(PathMapping{Local: "/home/me/app", Remote: "/app"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"breakpoint_remove -d 42420001", "breakpoint_set -t line -f " + fixtureScriptURI + " -n 24"}
	if got := breakpointCommands(e); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("after mapping, engine got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if sess.Script() != filepath.FromSlash("/home/me/app/basic.php") {
		t.Errorf("Script() after mapping = %q", sess.Script())
	}
	st, err := sess.Continue(context.Background(), ContinueRun, time.Second)
	if err != nil || st.File != filepath.FromSlash("/home/me/app/basic.php") {
		t.Errorf("stopped at %q, %v", st.File, err)
	}

	// A mapping for the same local directory replaces the old one.
	if err := srv.AddPathMapping(PathMapping{Local: "/home/me/app/", Remote: "/srv/app"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.PathMap(); len(got) != 1 || got[0].Remote != "/srv/app" {
		t.Errorf("PathMap() = %+v, want one mapping to /srv/app", got)
	}
	for _, bad := range []PathMapping{{Local: "/a"}, {Remote: "/b"}} {
		if err := srv.AddPathMapping(bad); err == nil {
			t.Errorf("AddPathMapping(%+v) succeeded", bad)
		}
	}
}

// A running session sets its breakpoints again on its next Continue.
func TestAddPathMappingWhileRunning(t *testing.T) {
	release := make(chan struct{})
	handlers := standardHandlers()
	handlers["run"] = func(e *fakeEngine, cmd fakeCommand) []string {
		<-release
		return []string{e.withTransaction(e.fixture("run_break"), cmd)}
	}
	srv := newTestServer(t, Config{})
	if _, err := srv.AddBreakpoint(Breakpoint{File: "/home/me/app/basic.php", Line: lineAddBody}); err != nil {
		t.Fatal(err)
	}
	e := dialFakeEngine(t, srv, fixture(t, "init"), handlers)
	sess := waitSession(t, srv)
	if st, _ := sess.Continue(context.Background(), ContinueRun, 0); st.Status != StatusRunning {
		t.Fatalf("State = %+v, want running", st)
	}
	e.ClearReceived()

	if err := srv.AddPathMapping(PathMapping{Local: "/home/me/app", Remote: "/app"}); err != nil {
		t.Fatal(err)
	}
	if got := breakpointCommands(e); len(got) != 0 {
		t.Fatalf("breakpoint commands sent to a running session: %q", got)
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
	if want := "run breakpoint_remove breakpoint_set run"; strings.Join(names, " ") != want {
		t.Errorf("commands = %q, want %q", strings.Join(names, " "), want)
	}
}

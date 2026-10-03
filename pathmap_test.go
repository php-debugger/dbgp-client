package dbgp

import (
	"context"
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

package dbgp

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// fixtureDir holds DBGp packets recorded from a real debug engine session
// (PHP Debugger). The fake engine replays them, so tests see byte-for-byte
// engine output.
const fixtureDir = "testdata/dbgp"

// fixtureScriptURI replaces the debuggee's absolute URI in recorded fixtures.
const fixtureScriptURI = "file:///app/basic.php"

// fixtureAppID replaces the debuggee's process id (appid, breakpoint ids).
const fixtureAppID = "4242"

// TestCaptureFixtures regenerates testdata/dbgp from the engine php has loaded.
// Run with: DBGP_CAPTURE=1 go test -run TestCaptureFixtures
func TestCaptureFixtures(t *testing.T) {
	if os.Getenv("DBGP_CAPTURE") == "" {
		t.Skip("set DBGP_CAPTURE=1 to re-record engine fixtures")
	}
	requireDebugEngine(t)
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newRawSession(t)
	scriptURI := MakeFileURI(debuggeePath(t))
	appID := attr(s.init, "appid")
	if appID == "" {
		t.Fatalf("init packet has no appid: %s", s.init)
	}

	save := func(name string, packet []byte) {
		t.Helper()
		packet = bytes.ReplaceAll(packet, []byte(scriptURI), []byte(fixtureScriptURI))
		packet = bytes.ReplaceAll(packet, []byte(appID), []byte(fixtureAppID))
		if err := os.WriteFile(filepath.Join(fixtureDir, name+".xml"), packet, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// saveOther stores the stream/notify packets that preceded a response.
	saveOther := func(other [][]byte) {
		t.Helper()
		for _, p := range other {
			switch {
			case bytes.Contains(p, []byte(`<stream`)):
				save("stream_"+attr(p, "type"), p)
			case bytes.Contains(p, []byte(`<notify`)):
				save("notify_"+attr(p, "name"), p)
			default:
				t.Fatalf("unexpected packet: %s", p)
			}
		}
	}
	cmd := func(fixture, name, args string, data ...string) []byte {
		t.Helper()
		resp, other := s.command(name, args, data...)
		saveOther(other)
		if fixture != "" {
			save(fixture, resp)
		}
		return resp
	}

	save("init", s.init)

	// Session setup and introspection.
	cmd("status_starting", "status", "")
	cmd("feature_get", "feature_get", "-n max_depth")
	cmd("feature_get_unsupported", "feature_get", "-n no_such_feature")
	cmd("feature_set", "feature_set", "-n resolved_breakpoints -v 1")
	cmd("", "feature_set", "-n notify_ok -v 1")
	cmd("", "feature_set", "-n max_children -v 10")
	cmd("typemap_get", "typemap_get", "")
	cmd("stdout", "stdout", "-c 1")

	// Protocol errors.
	s.sendRaw("11\x00") // what a length-prefixed command looks like to Xdebug
	save("error_invalid_options", s.read())
	cmd("error_breakpoint_not_found", "breakpoint_remove", "-d 999999")

	// Breakpoints.
	lineBP := cmd("breakpoint_set_line", "breakpoint_set", "-t line -f "+scriptURI+" -n "+strconv.Itoa(lineAddBody))
	condBP := cmd("breakpoint_set_conditional", "breakpoint_set",
		"-t conditional -f "+scriptURI+" -n "+strconv.Itoa(lineLoopBody), "$i == 3")
	callBP := cmd("breakpoint_set_call", "breakpoint_set", "-t call -m add")
	cmd("breakpoint_set_exception", "breakpoint_set", "-t exception -x Exception")
	cmd("breakpoint_get", "breakpoint_get", "-d "+attr(condBP, "id"))
	cmd("breakpoint_update", "breakpoint_update", "-d "+attr(callBP, "id")+" -s disabled")
	cmd("breakpoint_list", "breakpoint_list", "")

	// Run to the line breakpoint inside add().
	cmd("run_break", "run", "")
	cmd("status_break", "status", "")
	cmd("stack_depth", "stack_depth", "")
	cmd("stack_get", "stack_get", "")
	cmd("stack_get_depth", "stack_get", "-d 1")
	cmd("context_names", "context_names", "")
	cmd("context_get_locals", "context_get", "-d 0 -c 0")
	cmd("context_get_main", "context_get", "-d 1 -c 0")
	cmd("property_get_page", "property_get", "-d 1 -n $numbers -p 1")
	cmd("property_get_object", "property_get", "-d 1 -n $obj")
	cmd("property_value", "property_value", "-d 1 -n $long")
	cmd("property_set", "property_set", "-d 0 -n $b", "10")
	cmd("eval", "eval", "", "$a + $b")
	cmd("eval_error", "eval", "", "$a +")
	cmd("source", "source", "-f "+scriptURI+" -b 22 -e 26")

	// Stepping.
	cmd("step_over", "step_over", "")
	cmd("step_out", "step_out", "")
	cmd("step_into", "step_into", "")

	// Run to the conditional breakpoint, then to the end.
	cmd("run_break_conditional", "run", "")
	// Removing the conditional breakpoint is not recorded: both engines echo
	// its expression from freed memory, so the reply differs on every run.
	cmd("breakpoint_remove", "breakpoint_remove", "-d "+attr(lineBP, "id"))
	cmd("", "breakpoint_remove", "-d "+attr(condBP, "id"))
	cmd("run_break_exception", "run", "")
	cmd("run_stopping", "run", "")
	cmd("stop", "stop", "")

	// A second session for detach.
	d := newRawSession(t)
	resp, _ := d.command("detach", "")
	save("detach", bytes.ReplaceAll(resp, []byte(attr(d.init, "appid")), []byte(fixtureAppID)))

	// A third session for an unimplemented command: the engine answers with
	// error 4 and then resumes the script, which runs to completion.
	u := newRawSession(t)
	resp, _ = u.command("no_such_command", "")
	save("error_unimplemented", resp)
	resp, other := u.command("status", "")
	for i, p := range other {
		save("error_unimplemented_after_"+strconv.Itoa(i+1), p)
	}
	save("error_unimplemented_status", resp)
}

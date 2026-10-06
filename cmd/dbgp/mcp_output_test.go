package main

import (
	"strings"
	"testing"
)

func TestMCPOutputToolsWithoutSession(t *testing.T) {
	cs, _ := newTestMCP(t)
	for _, tool := range []string{"source", "output", "warnings"} {
		if msg := callToolError(t, cs, tool, nil); !strings.Contains(msg, "no session") {
			t.Errorf("%s: %q", tool, msg)
		}
	}
}

func TestMCPSource(t *testing.T) {
	cs, _, script := stoppedInAdd(t)

	var out sourceOutput
	callTool(t, cs, "source", nil, &out)
	if out.File != script || len(out.Lines) != 11 || out.Lines[0].Line != 19 || out.Lines[10].Line != 29 {
		t.Fatalf("source = %s, lines %d to %d", out.File, out.Lines[0].Line, out.Lines[len(out.Lines)-1].Line)
	}
	for _, l := range out.Lines {
		if l.Current != (l.Line == 24) {
			t.Errorf("line %d current = %v", l.Line, l.Current)
		}
	}
	// Indentation is kept.
	if out.Lines[5].Text != "    $sum = $a + $b;" {
		t.Errorf("line 24 = %q", out.Lines[5].Text)
	}

	callTool(t, cs, "source", map[string]any{"line": 36, "around": 1}, &out)
	if len(out.Lines) != 3 || out.Lines[1].Text != "$x = add(2, 3);" || out.Lines[1].Current {
		t.Errorf("source around line 36 = %+v", out.Lines)
	}
	callTool(t, cs, "source", map[string]any{"file": "../../testdata/php/basic.php", "line": 1, "around": 2}, &out)
	if out.File != script || len(out.Lines) != 3 || out.Lines[0].Line != 1 || out.Lines[0].Text != "<?php" {
		t.Errorf("source at the start, relative path = %s %+v", out.File, out.Lines)
	}

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"around": 101}, "around must be between 1 and 100"},
		{map[string]any{"around": -1}, "around must be between 1 and 100"},
		{map[string]any{"file": "/no/such/file.php"}, "PHP cannot read /no/such/file.php"},
	} {
		if msg := callToolError(t, cs, "source", tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("source(%v): %q, want %q", tc.args, msg, tc.want)
		}
	}
}

func TestMCPOutputAndWarnings(t *testing.T) {
	cs, _, script := stoppedInAdd(t)

	var printed outputOutput
	callTool(t, cs, "output", nil, &printed)
	if printed.Output != "" {
		t.Errorf("output before the script printed anything = %q", printed.Output)
	}
	var st stateOutput
	callTool(t, cs, "continue", nil, &st)
	if st.Status != "stopping" {
		t.Fatalf("continue = %+v, want stopping", st)
	}

	callTool(t, cs, "output", nil, &printed)
	if printed.Output != "x=5\ndone\n" || printed.Session != 1 || printed.Truncated {
		t.Errorf("output = %+v, want x=5 and done", printed)
	}
	callTool(t, cs, "output", nil, &printed)
	if printed.Output != "" {
		t.Errorf("second output = %q, want nothing new", printed.Output)
	}

	var raised warningsOutput
	callTool(t, cs, "warnings", nil, &raised)
	want := warningInfo{Type: "Warning", Message: "Undefined variable $undefinedVariable", File: script, Line: 42}
	if len(raised.Warnings) != 1 || raised.Warnings[0] != want {
		t.Errorf("warnings = %+v, want %+v", raised.Warnings, want)
	}
	callTool(t, cs, "warnings", nil, &raised)
	if raised.Warnings == nil || len(raised.Warnings) != 0 {
		t.Errorf("second warnings = %+v, want an empty list", raised.Warnings)
	}

	// Both still work once the session has ended, by default.
	callTool(t, cs, "stop", nil, &st)
	callTool(t, cs, "output", map[string]any{"all": true}, &printed)
	if printed.Output != "x=5\ndone\n" {
		t.Errorf("output all after stop = %q", printed.Output)
	}
	callTool(t, cs, "warnings", map[string]any{"all": true}, &raised)
	if len(raised.Warnings) != 1 {
		t.Errorf("warnings all after stop = %+v", raised.Warnings)
	}
}

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	dbgp "github.com/php-debugger/dbgp-client"
)

// stoppedInAdd starts the debuggee through MCP and runs it to the
// breakpoint in add(), at line 24.
func stoppedInAdd(t *testing.T) (*mcp.ClientSession, *dbgp.Server, string) {
	t.Helper()
	requireEngine(t)
	cs, srv := newTestMCP(t)
	script, err := filepath.Abs("../../testdata/php/basic.php")
	if err != nil {
		t.Fatal(err)
	}
	callTool(t, cs, "add_breakpoint", map[string]any{"file": script, "line": 24}, &addBreakpointOutput{})
	callTool(t, cs, "listen", nil, &listenOutput{})
	startPHP(t, srv.Port(), script)
	var waited waitForSessionOutput
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 10}, &waited)
	if !waited.Connected {
		t.Fatal("PHP did not connect")
	}
	var st stateOutput
	callTool(t, cs, "continue", nil, &st)
	if st.Status != "break" || st.Line != 24 {
		t.Fatalf("continue = %+v, want break at line 24", st)
	}
	return cs, srv, script
}

// byName indexes variables by name.
func byName(vars []variableInfo) map[string]variableInfo {
	m := map[string]variableInfo{}
	for _, v := range vars {
		m[v.Name] = v
	}
	return m
}

func sizeOf(n int) *int { return &n }

// same compares variables, including the size they point to.
func same(a, b variableInfo) bool {
	if (a.Size == nil) != (b.Size == nil) || (a.Size != nil && *a.Size != *b.Size) {
		return false
	}
	a.Size, b.Size = nil, nil
	return a == b
}

func TestMCPInspectWithoutSession(t *testing.T) {
	cs, _ := newTestMCP(t)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"stack", nil},
		{"variables", nil},
		{"variable", map[string]any{"name": "$a"}},
		{"variable_value", map[string]any{"name": "$a"}},
	} {
		if msg := callToolError(t, cs, tc.tool, tc.args); !strings.Contains(msg, "no session") {
			t.Errorf("%s: %q", tc.tool, msg)
		}
	}
}

func TestMCPStack(t *testing.T) {
	cs, _, script := stoppedInAdd(t)
	var out stackOutput
	callTool(t, cs, "stack", nil, &out)
	want := []frameInfo{
		{Depth: 0, Function: "add", File: script, Line: 24},
		{Depth: 1, Function: "{main}", File: script, Line: 36},
	}
	if out.Session != 1 || len(out.Frames) != 2 || out.Frames[0] != want[0] || out.Frames[1] != want[1] {
		t.Errorf("stack = %+v, want %+v", out, want)
	}
}

func TestMCPVariables(t *testing.T) {
	cs, _, _ := stoppedInAdd(t)

	var local variablesOutput
	callTool(t, cs, "variables", nil, &local)
	want := []variableInfo{{Name: "$a", Type: "int", Value: "2"}, {Name: "$b", Type: "int", Value: "3"}, {Name: "$sum", Type: "unset"}}
	if len(local.Variables) != len(want) {
		t.Fatalf("frame 0 variables = %+v", local.Variables)
	}
	for i := range want {
		if !same(local.Variables[i], want[i]) {
			t.Errorf("frame 0 variable %d = %+v, want %+v", i, local.Variables[i], want[i])
		}
	}

	var main variablesOutput
	callTool(t, cs, "variables", map[string]any{"depth": 1}, &main)
	vars := byName(main.Variables)
	for _, w := range []variableInfo{
		{Name: "$numbers", Type: "array", Size: sizeOf(40)},
		{Name: "$user", Type: "array", Size: sizeOf(3)},
		{Name: "$obj", Type: "object", Class: "Service", Size: sizeOf(3)},
		{Name: "$greeting", Type: "string", Value: "Héllo, wörld"},
		{Name: "$float", Type: "float", Value: "1.5"},
		{Name: "$nullVal", Type: "null"},
	} {
		if !same(vars[w.Name], w) {
			t.Errorf("frame 1 %s = %+v, want %+v", w.Name, vars[w.Name], w)
		}
	}
	for name := range vars {
		if strings.ContainsAny(name, "[-") {
			t.Errorf("variables lists the element or property %s", name)
		}
	}

	var globals variablesOutput
	callTool(t, cs, "variables", map[string]any{"context": 1}, &globals)
	if g := byName(globals.Variables)["$_SERVER"]; g.Type != "array" || g.Size == nil || *g.Size == 0 || globals.Context != 1 {
		t.Errorf("superglobals: $_SERVER = %+v, context %d", g, globals.Context)
	}
}

func TestMCPVariable(t *testing.T) {
	cs, _, _ := stoppedInAdd(t)
	var out variableOutput
	callTool(t, cs, "variable", map[string]any{"name": "$numbers", "depth": 1}, &out)
	if out.Variable.Size == nil || *out.Variable.Size != 40 || out.Pages != 2 || len(out.Children) != 32 ||
		!same(out.Children[0], variableInfo{Name: "$numbers[0]", Type: "int", Value: "1"}) {
		t.Errorf("$numbers page 0 = %+v, %d children, pages %d", out.Variable, len(out.Children), out.Pages)
	}
	callTool(t, cs, "variable", map[string]any{"name": "$numbers", "depth": 1, "page": 1}, &out)
	if out.Page != 1 || len(out.Children) != 8 || out.Children[7].Value != "40" {
		t.Errorf("$numbers page 1 = %d children, page %d", len(out.Children), out.Page)
	}
	if msg := callToolError(t, cs, "variable", map[string]any{"name": "$numbers", "depth": 1, "page": 2}); !strings.Contains(msg, "has 2 pages") {
		t.Errorf("page past the end: %q", msg)
	}

	callTool(t, cs, "variable", map[string]any{"name": "$obj", "depth": 1}, &out)
	props := byName(out.Children)
	for _, w := range []variableInfo{
		{Name: "$obj->id", Type: "int", Value: "42", Facet: "public"},
		{Name: "$obj->cache", Type: "object", Class: "Cache", Size: sizeOf(1), Facet: "protected"},
		{Name: "$obj->name", Type: "string", Value: "svc", Facet: "private"},
	} {
		if !same(props[w.Name], w) {
			t.Errorf("%s = %+v, want %+v", w.Name, props[w.Name], w)
		}
	}
	callTool(t, cs, "variable", map[string]any{"name": "$obj->cache", "depth": 1}, &out)
	if items := byName(out.Children)["$obj->cache->items"]; items.Type != "array" || items.Size == nil || *items.Size != 3 {
		t.Errorf("$obj->cache->items = %+v", items)
	}
	callTool(t, cs, "variable", map[string]any{"name": `$user["active"]`, "depth": 1}, &out)
	if !same(out.Variable, variableInfo{Name: `$user["active"]`, Type: "bool", Value: "true"}) || len(out.Children) != 0 {
		t.Errorf(`$user["active"] = %+v, children %v`, out.Variable, out.Children)
	}
	if msg := callToolError(t, cs, "variable", map[string]any{"name": "$nope", "depth": 1}); !strings.Contains(msg, "$nope does not exist in frame 1") {
		t.Errorf("unknown variable: %q", msg)
	}
}

func TestMCPVariableValue(t *testing.T) {
	cs, srv, _ := stoppedInAdd(t)
	// A small max_data, so the engine truncates $long (200 characters).
	if err := srv.Session(1).FeatureSet("max_data", "16"); err != nil {
		t.Fatal(err)
	}
	var main variablesOutput
	callTool(t, cs, "variables", map[string]any{"depth": 1}, &main)
	if long := byName(main.Variables)["$long"]; !long.Truncated || len(long.Value) != 16 {
		t.Errorf("$long = %+v, want 16 characters, truncated", long)
	}
	var out variableValueOutput
	callTool(t, cs, "variable_value", map[string]any{"name": "$long", "depth": 1}, &out)
	if out.Value != strings.Repeat("abcdefghij", 20) {
		t.Errorf("variable_value = %d characters, want 200", len(out.Value))
	}
}

func TestMCPInspectRunningSession(t *testing.T) {
	requireEngine(t)
	cs, srv := newTestMCP(t)
	callTool(t, cs, "listen", nil, &listenOutput{})
	startPHP(t, srv.Port(), "-r", `usleep(1500000);`)
	callTool(t, cs, "wait_for_session", map[string]any{"wait": 10}, &waitForSessionOutput{})
	var st stateOutput
	callTool(t, cs, "continue", map[string]any{"wait": 0.2}, &st)
	if st.Status != "running" {
		t.Fatalf("continue = %+v, want running", st)
	}
	for _, tool := range []string{"stack", "variables"} {
		if msg := callToolError(t, cs, tool, nil); !strings.Contains(msg, "still running: call wait_for_stop first") {
			t.Errorf("%s while running: %q", tool, msg)
		}
	}
}

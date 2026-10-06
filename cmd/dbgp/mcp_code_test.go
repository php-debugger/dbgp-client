package main

import (
	"context"
	"strings"
	"testing"
)

func TestMCPCodeToolsCanBeLeftOut(t *testing.T) {
	cs, _ := newTestMCPOn(t, "127.0.0.1:0", false)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "eval" || tool.Name == "set_variable" {
			t.Errorf("%s offered although code tools are off", tool.Name)
		}
	}
	if len(res.Tools) != 21 {
		t.Errorf("got %d tools, want the other 21", len(res.Tools))
	}
	// Agents are told the tools were turned off, not that they are missing.
	if instructions := cs.InitializeResult().Instructions; !strings.Contains(instructions, "-no-eval") {
		t.Errorf("instructions do not explain -no-eval: %s", instructions)
	}
	on, _ := newTestMCPOn(t, "127.0.0.1:0", true)
	if strings.Contains(on.InitializeResult().Instructions, "-no-eval") {
		t.Error("instructions mention -no-eval although code tools are on")
	}
}

func TestMCPCodeToolsWithoutSession(t *testing.T) {
	cs, _ := newTestMCP(t)
	for tool, args := range map[string]map[string]any{
		"eval":         {"code": "1"},
		"set_variable": {"name": "$a", "value": "1"},
	} {
		if msg := callToolError(t, cs, tool, args); !strings.Contains(msg, "no session") {
			t.Errorf("%s: %q", tool, msg)
		}
	}
}

func TestMCPEval(t *testing.T) {
	cs, _, _ := stoppedInAdd(t)

	var out evalOutput
	callTool(t, cs, "eval", map[string]any{"code": "$a + $b"}, &out)
	if out.Result == nil || !same(*out.Result, variableInfo{Name: "$a + $b", Type: "int", Value: "5"}) || len(out.Children) != 0 {
		t.Errorf("eval $a + $b = %+v", out)
	}

	callTool(t, cs, "eval", map[string]any{"code": "[$a, 'k' => $b]"}, &out)
	if out.Result == nil || out.Result.Type != "array" || *out.Result.Size != 2 || len(out.Children) != 2 ||
		!same(out.Children[0], variableInfo{Name: "0", Type: "int", Value: "2"}) ||
		!same(out.Children[1], variableInfo{Name: "k", Type: "int", Value: "3"}) {
		t.Errorf("eval array = %+v, children %+v", out.Result, out.Children)
	}

	callTool(t, cs, "eval", map[string]any{"code": "range(1, 40)", "page": 1}, &out)
	if out.Pages != 2 || out.Page != 1 || len(out.Children) != 8 || out.Children[7].Value != "40" {
		t.Errorf("eval page 1 = %d children, page %d of %d", len(out.Children), out.Page, out.Pages)
	}
	callTool(t, cs, "eval", map[string]any{"code": "null"}, &out)
	if out.Result == nil || out.Result.Type != "null" {
		t.Errorf("eval null = %+v", out.Result)
	}

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"code": "$a +"}, "PHP could not evaluate it"},
		{map[string]any{"code": "range(1, 40)", "page": 2}, "the result has 2 pages"},
		{map[string]any{"code": ""}, "code is required"},
	} {
		if msg := callToolError(t, cs, "eval", tc.args); !strings.Contains(msg, tc.want) {
			t.Errorf("eval(%v): %q, want %q", tc.args, msg, tc.want)
		}
	}
}

func TestMCPSetVariable(t *testing.T) {
	cs, _, _ := stoppedInAdd(t)

	var out setVariableOutput
	callTool(t, cs, "set_variable", map[string]any{"name": "$b", "value": "$a * 10"}, &out)
	if !same(out.Variable, variableInfo{Name: "$b", Type: "int", Value: "20"}) {
		t.Errorf("set $b = %+v", out.Variable)
	}
	// The script carries on with the new value: $sum = $a + $b.
	callTool(t, cs, "continue", map[string]any{"command": "step_over"}, &stateOutput{})
	var sum variableOutput
	callTool(t, cs, "variable", map[string]any{"name": "$sum"}, &sum)
	if sum.Variable.Value != "22" {
		t.Errorf("$sum after setting $b = %+v, want 22", sum.Variable)
	}

	callTool(t, cs, "set_variable", map[string]any{"name": "$name", "value": "'Mars'", "depth": 1}, &out)
	if !same(out.Variable, variableInfo{Name: "$name", Type: "string", Value: "Mars"}) {
		t.Errorf("set $name in frame 1 = %+v", out.Variable)
	}

	if msg := callToolError(t, cs, "set_variable", map[string]any{"name": "$b", "value": "$a +"}); !strings.Contains(msg, "PHP could not set $b to $a +: check that the value is a valid PHP expression") {
		t.Errorf("set_variable with a syntax error: %q", msg)
	}
	if msg := callToolError(t, cs, "set_variable", map[string]any{"name": "$b"}); !strings.Contains(msg, "required") {
		t.Errorf("set_variable without a value: %q", msg)
	}
}

// eval has no stack frame option: the description must say so.
func TestMCPEvalDescriptionExplainsFrames(t *testing.T) {
	cs, _ := newTestMCP(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "eval" {
			continue
		}
		for _, want := range []string{"innermost frame", "no way to evaluate in a caller's frame", "with depth"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("eval description lacks %q", want)
			}
		}
	}
}
